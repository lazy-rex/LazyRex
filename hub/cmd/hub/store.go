package main

// Hub-owned storage (README section 3). One SQLite database. The hub is its only user.
// Namespaces come from the caller's registration, never from the request.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS notices (ns TEXT NOT NULL, agent TEXT NOT NULL, pending INTEGER NOT NULL, intent_rev INTEGER NOT NULL, PRIMARY KEY (ns, agent));
-- notice_deliveries: the last nudge the hub wrote for a notice row, and the conversation it reached
-- (block, child pid and start). A hub restart or a shim reconnect does not write it again.
CREATE TABLE IF NOT EXISTS notice_deliveries (ns TEXT NOT NULL, agent TEXT NOT NULL, intent_rev INTEGER NOT NULL, at INTEGER NOT NULL, block_id TEXT NOT NULL, child_pid INTEGER NOT NULL, child_start TEXT NOT NULL, repeated INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (ns, agent));
-- hub.sql notices (sqlnotice.go): a receipt for each applied outbox row.
CREATE TABLE IF NOT EXISTS notice_receipts (plugin TEXT NOT NULL, seq INTEGER NOT NULL, PRIMARY KEY (plugin, seq));
CREATE TABLE IF NOT EXISTS bindings (agent TEXT PRIMARY KEY, block_id TEXT NOT NULL, child_pid INTEGER NOT NULL, child_start TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS plugins (name TEXT PRIMARY KEY, desired TEXT NOT NULL, instance_id TEXT NOT NULL DEFAULT '', block_id TEXT NOT NULL DEFAULT '', child_pid INTEGER NOT NULL DEFAULT 0, child_start TEXT NOT NULL DEFAULT '');
-- status: prepared (request not sent), sending (outcome unknown until resolved), created, abandoned
CREATE TABLE IF NOT EXISTS spawn_intents (instance_id TEXT PRIMARY KEY, plugin TEXT NOT NULL, status TEXT NOT NULL, block_id TEXT NOT NULL DEFAULT '');
INSERT OR IGNORE INTO meta VALUES ('rev', 0), ('generation', 0);
`

type DB struct{ *sql.DB }

func openDB(path string) (*DB, error) {
	// WAL with synchronous=FULL: a committed batch survives a process crash (not tested for power loss).
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; transactions are short and never span an RPC
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// Columns added after the first release. "duplicate column" means the store has them already.
	for _, m := range []string{
		`ALTER TABLE plugins ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE spawn_intents ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bindings ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE plugins ADD COLUMN run TEXT NOT NULL DEFAULT ''`,    // Plugin.run as JSON; '' = no run
		`ALTER TABLE plugins ADD COLUMN source TEXT NOT NULL DEFAULT ''`, // the exact source of the entry that bound the name
		`ALTER TABLE notices ADD COLUMN read INTEGER NOT NULL DEFAULT 1`, // the agent read since the last revision
	} {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	if err := retireSchema(db); err != nil {
		return nil, err
	}
	if err := retireStore(db, filepath.Join(filepath.Dir(path), "sql"), log.Printf); err != nil {
		return nil, err
	}
	return &DB{db}, nil
}

// retiredStep is one step that removes a table or a column of hub.db.
type retiredStep struct {
	table, column string // what the step removes; column "" is the whole table
	// conditional: a "may drop" that can wait. It has no check or statement; retireStore runs it.
	conditional      bool
	check, statement string // the statement runs only while check counts more than 0
}

// retiredSchema is the list of every table and column that the hub removes from hub.db, and its one
// source of truth: the release rehearsal lets a table or column of OLD go only when the NEW hub
// names it here. A new drop of a hub table or column goes through this list, never a bare DROP
// elsewhere. A step runs at most once on a database, and a new database skips it. A step goes when
// no live hub.db needs it any more.
var retiredSchema = []retiredStep{
	// ui_tabs.rev_known marked the tabs closed before tab revisions existed. The live hub.db had no
	// such row, so the column goes.
	{table: "ui_tabs", column: "rev_known",
		check: `SELECT COUNT(*) FROM pragma_table_info('ui_tabs') WHERE name = 'rev_known'`, statement: `ALTER TABLE ui_tabs DROP COLUMN rev_known`},
	// The hub.store removal: sql_returns (hub.admin.sql_return) and sql_notice_plugins (the notice
	// move) have no reader. records and ops wait for the plugin databases (retireStore).
	{table: "sql_returns",
		check: `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sql_returns'`, statement: `DROP TABLE sql_returns`},
	{table: "sql_notice_plugins",
		check: `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sql_notice_plugins'`, statement: `DROP TABLE sql_notice_plugins`},
	{table: "records", conditional: true},
	{table: "ops", conditional: true},
}

// retiredExport is the output of "lazyrex-hub -retired-schema": retiredSchema as JSON. A change of
// its fields changes Version. The release rehearsal reads it from the NEW hub binary.
type retiredExport struct {
	Version int            `json:"version"`
	Retired []retiredEntry `json:"retired"`
}

type retiredEntry struct {
	Table       string `json:"table"`
	Column      string `json:"column"`
	Conditional bool   `json:"conditional"`
}

// retiredSchemaMain prints retiredSchema as retiredExport version 1, in its order. It opens no
// state and reads no config.
func retiredSchemaMain(w io.Writer) int {
	out := retiredExport{Version: 1, Retired: []retiredEntry{}}
	for _, s := range retiredSchema {
		out.Retired = append(out.Retired, retiredEntry{s.table, s.column, s.conditional})
	}
	b, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if _, err := fmt.Fprintf(w, "%s\n", b); err != nil {
		return 1
	}
	return 0
}

// retireSchema runs the steps of retiredSchema that are still due, except the conditional ones.
func retireSchema(db *sql.DB) error {
	for _, s := range retiredSchema {
		if s.conditional {
			continue
		}
		var n int
		if err := db.QueryRow(s.check).Scan(&n); err != nil {
			return fmt.Errorf("retire schema check %q: %w", s.check, err)
		}
		if n == 0 {
			continue
		}
		if _, err := db.Exec(s.statement); err != nil {
			return fmt.Errorf("retire schema %q: %w", s.statement, err)
		}
	}
	return nil
}

// retireStore runs the conditional steps of retiredSchema. It drops records and ops, the tables of
// the removed hub.store, in one transaction when each namespace with a row in either table has its
// hub.sql database (sqlDir/<namespace>.db). Else it keeps both tables, logs one line for each
// namespace without a database, and tries again at the next open.
// It is a backstop: a database file does not prove that the plugin imported its rows (D1 of the
// hub.store removal). A database without the tables (a new one, or one after the drop) skips it.
func retireStore(db *sql.DB, sqlDir string, logf func(string, ...any)) error {
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('records', 'ops')`).Scan(&tables); err != nil {
		return fmt.Errorf("retire store check: %w", err)
	}
	if tables == 0 {
		return nil
	}
	if tables != 2 {
		return fmt.Errorf("retire store: hub.db has only one of records and ops")
	}
	rows, err := db.Query(`SELECT ns FROM records UNION SELECT ns FROM ops ORDER BY ns`)
	if err != nil {
		return fmt.Errorf("retire store namespaces: %w", err)
	}
	var missing []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			rows.Close()
			return fmt.Errorf("retire store namespaces: %w", err)
		}
		st, err := os.Stat(filepath.Join(sqlDir, ns+".db"))
		if err != nil || !st.Mode().IsRegular() {
			missing = append(missing, ns)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("retire store namespaces: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("retire store namespaces: %w", err)
	}
	if len(missing) > 0 {
		for _, ns := range missing {
			logf("hub.store tables kept: namespace %q has rows and no %s", ns, filepath.Join(sqlDir, ns+".db"))
		}
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("retire store: %w", err)
	}
	defer tx.Rollback()
	for _, s := range []string{`DROP TABLE records`, `DROP TABLE ops`} {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("retire store %q: %w", s, err)
		}
	}
	return tx.Commit()
}

// nextRev returns a fresh revision from the database-wide counter. Revisions never reset.
func nextRev(tx *sql.Tx) (int64, error) {
	var rev int64
	err := tx.QueryRow(`UPDATE meta SET v = v + 1 WHERE k = 'rev' RETURNING v`).Scan(&rev)
	return rev, err
}

func (db *DB) newGeneration() (int64, error) {
	var g int64
	err := db.QueryRow(`UPDATE meta SET v = v + 1 WHERE k = 'generation' RETURNING v`).Scan(&g)
	return g, err
}

// storeError is a malformed request or unauthorized caller: it becomes a client.reply error.
type storeError struct{ msg, code string }

func (e *storeError) Error() string { return e.msg }

func bad(format string, args ...any) error { return &storeError{msg: fmt.Sprintf(format, args...)} }

// refuse is bad with a typed error code in the reply (instead of "refused").
func refuse(code, format string, args ...any) error {
	return &storeError{msg: code + ": " + fmt.Sprintf(format, args...), code: code}
}

// storeErrorCode is the error code of a hub error in a reply: its own code, or "refused" for an
// untyped bad(). Every reply path that answers a hub error uses it (answer, agentAnswer and the
// restart form), so a caller gets the same code on each face.
func storeErrorCode(se *storeError) string {
	if se.code == "" {
		return "refused"
	}
	return se.code
}

// asList accepts a JSON array, or {} from Rex Lua for an empty list.
func asList(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case map[string]any:
		return nil, len(t) == 0
	case nil:
		return nil, true
	}
	return nil, false
}

func revString(n int64) string { return strconv.FormatInt(n, 10) }

// noticeOp is one notice of a hub.sql commit: {agent, pending}, {agent, pending: true, once: true} (a new revision
// only if the row is not pending or the agent read since the last one), or {agent, read: true}
// (the agent read; pending does not change).
type noticeOp struct {
	agent               string
	pending, once, read bool
}

// parseNotice reads the fields of a notice of agent (hub.sql.commit notices).
func parseNotice(agent string, nm map[string]any) (noticeOp, error) {
	op := noticeOp{agent: agent}
	_, hasPending := nm["pending"]
	_, hasOnce := nm["once"]
	if r, hasRead := nm["read"]; hasRead {
		if r != true || hasPending || hasOnce {
			return op, bad("a read notice is {agent, read: true}, with no pending or once")
		}
		op.read = true
		return op, nil
	}
	pending, okP := nm["pending"].(bool)
	once, okO := nm["once"].(bool)
	if !okP || (hasOnce && (!okO || !pending)) {
		return op, bad("a notice needs a boolean pending; once is true or false, and only with pending: true")
	}
	op.pending, op.once = pending, once
	return op, nil
}

// applyNotice applies one notice in tx (for the hub.sql outbox rows: applySQLNotices). rev is the
// new intent revision, or 0 when the notice made none.
func applyNotice(ctx context.Context, tx *sql.Tx, ns string, n noticeOp) (rev int64, err error) {
	switch {
	case n.read:
		_, err := tx.ExecContext(ctx, `UPDATE notices SET read = 1 WHERE ns = ? AND agent = ?`, ns, n.agent)
		return 0, err
	case n.pending:
		if n.once {
			// One nudge until a read: a pending row that was not read since its revision stays.
			var pending, read int
			err := tx.QueryRowContext(ctx, `SELECT pending, read FROM notices WHERE ns = ? AND agent = ?`, ns, n.agent).Scan(&pending, &read)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			if err == nil && pending == 1 && read == 0 {
				return 0, nil
			}
		}
		// A plain pending:true advances the intent revision each time, so each send can cause a
		// new notice.
		rev, err := nextRev(tx)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notices (ns, agent, pending, intent_rev, read) VALUES (?, ?, 1, ?, 0) ON CONFLICT (ns, agent) DO UPDATE SET pending = 1, intent_rev = excluded.intent_rev, read = 0`, ns, n.agent, rev); err != nil {
			return 0, err
		}
		return rev, nil
	default:
		_, err := tx.ExecContext(ctx, `UPDATE notices SET pending = 0 WHERE ns = ? AND agent = ?`, ns, n.agent)
		return 0, err
	}
}

// noticeConv names the conversation that a nudge reached: the agent's pane and its child process.
type noticeConv struct {
	block string
	pid   int
	start string
}

// pendingNotice is a pending notice row with its last delivery (delivRev 0: none).
type pendingNotice struct {
	ns, agent string
	intentRev int64
	read      bool // the agent read since the revision
	delivRev  int64
	delivAt   int64 // Unix milliseconds of the first write of delivRev
	delivConv noticeConv
	repeated  bool
}

func (db *DB) pendingNotices(ctx context.Context) ([]pendingNotice, error) {
	rows, err := db.QueryContext(ctx, `SELECT n.ns, n.agent, n.intent_rev, n.read, COALESCE(d.intent_rev, 0), COALESCE(d.at, 0),
		COALESCE(d.block_id, ''), COALESCE(d.child_pid, 0), COALESCE(d.child_start, ''), COALESCE(d.repeated, 0)
		FROM notices n LEFT JOIN notice_deliveries d ON d.ns = n.ns AND d.agent = n.agent WHERE n.pending = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingNotice
	for rows.Next() {
		var p pendingNotice
		if err := rows.Scan(&p.ns, &p.agent, &p.intentRev, &p.read, &p.delivRev, &p.delivAt, &p.delivConv.block, &p.delivConv.pid, &p.delivConv.start, &p.repeated); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// recordNotice stores that the hub wrote the nudge of n to conversation c at time at. A repeat
// only marks the stored delivery: its time stays the first write's.
func (db *DB) recordNotice(ctx context.Context, n pendingNotice, c noticeConv, at time.Time, repeat bool) error {
	if repeat {
		_, err := db.ExecContext(ctx, `UPDATE notice_deliveries SET repeated = 1 WHERE ns = ? AND agent = ? AND intent_rev = ?`, n.ns, n.agent, n.intentRev)
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO notice_deliveries VALUES (?, ?, ?, ?, ?, ?, ?, 0) ON CONFLICT (ns, agent) DO UPDATE SET
		intent_rev = excluded.intent_rev, at = excluded.at, block_id = excluded.block_id, child_pid = excluded.child_pid, child_start = excluded.child_start, repeated = 0`,
		n.ns, n.agent, n.intentRev, at.UnixMilli(), c.block, c.pid, c.start)
	return err
}

// noticeDelivery is the hub run's memory of the last delivery of a notice row: the same fields as
// its notice_deliveries row, written at the same time. It stands when the store write failed.
type noticeDelivery struct {
	rev, at  int64
	conv     noticeConv
	repeated bool
}

// withDelivery lays the memory record d over the stored delivery of n: the memory is never older,
// and it is newer only when a store write failed (Codex review of a1205c1).
func (n pendingNotice) withDelivery(d noticeDelivery, ok bool) pendingNotice {
	if ok && d.rev >= n.delivRev {
		n.delivRev, n.delivAt, n.delivConv, n.repeated = d.rev, d.at, d.conv, d.repeated
	}
	return n
}

// noticeDue decides a pending notice for conversation c (with its delivery merged: withDelivery).
// mem is the revision that the hub wrote to the agent's current connection. due: write the nudge;
// repeat: it is the one repeat, after renudge with no read and no new revision. wake: when to look
// again (zero: no need). A read ends the nudge of its revision, also before the first write.
func noticeDue(n pendingNotice, c noticeConv, mem int64, now time.Time, renudge time.Duration) (due, repeat bool, wake time.Time) {
	if n.read {
		return false, false, time.Time{}
	}
	stored := n.delivRev >= n.intentRev && n.delivConv == c
	if n.intentRev > mem && !stored {
		return true, false, time.Time{}
	}
	if !stored || n.delivRev != n.intentRev || n.repeated {
		return false, false, time.Time{}
	}
	at := time.UnixMilli(n.delivAt).Add(renudge)
	if now.Before(at) {
		return false, false, at
	}
	return true, true, time.Time{}
}

// applySQLNotices applies hub.sql outbox rows of plugin (sqlnotice.go) in seq order, in one
// transaction with a receipt for each row. A row with a receipt was applied before (a crash after
// the apply and before the outbox delete): it is skipped.
func (db *DB) applySQLNotices(ctx context.Context, plugin string, rows []outboxRow) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM notice_receipts WHERE plugin = ? AND seq = ?`, plugin, r.seq).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := applyNotice(ctx, tx, plugin, r.noticeOp); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notice_receipts VALUES (?, ?)`, plugin, r.seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// dropNoticeReceipts deletes the receipts of plugin up to seq, after the outbox rows are gone.
func (db *DB) dropNoticeReceipts(ctx context.Context, plugin string, seq int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM notice_receipts WHERE plugin = ? AND seq <= ?`, plugin, seq)
	return err
}
