package main

// Hub-owned storage (README section 3). One SQLite database. The hub is its only user.
// Namespaces come from the caller's registration, never from the request.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS records (ns TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, rev INTEGER NOT NULL, PRIMARY KEY (ns, key));
CREATE TABLE IF NOT EXISTS ops (ns TEXT NOT NULL, key TEXT NOT NULL, fingerprint TEXT NOT NULL, result TEXT NOT NULL, PRIMARY KEY (ns, key));
CREATE TABLE IF NOT EXISTS notices (ns TEXT NOT NULL, agent TEXT NOT NULL, pending INTEGER NOT NULL, intent_rev INTEGER NOT NULL, PRIMARY KEY (ns, agent));
-- notice_deliveries: the last nudge the hub wrote for a notice row, and the conversation it reached
-- (block, child pid and start). A hub restart or a shim reconnect does not write it again.
CREATE TABLE IF NOT EXISTS notice_deliveries (ns TEXT NOT NULL, agent TEXT NOT NULL, intent_rev INTEGER NOT NULL, at INTEGER NOT NULL, block_id TEXT NOT NULL, child_pid INTEGER NOT NULL, child_start TEXT NOT NULL, repeated INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (ns, agent));
-- hub.sql notices (sqlnotice.go): a receipt for each applied outbox row, and the plugins whose notices moved to hub.sql.
CREATE TABLE IF NOT EXISTS notice_receipts (plugin TEXT NOT NULL, seq INTEGER NOT NULL, PRIMARY KEY (plugin, seq));
CREATE TABLE IF NOT EXISTS sql_returns (plugin TEXT PRIMARY KEY, stamp TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sql_notice_plugins (plugin TEXT PRIMARY KEY);
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
		`ALTER TABLE plugins ADD COLUMN entry_identity TEXT NOT NULL DEFAULT ''`, // entryIdentity of the entry that the record was last saved with; '' = unknown
		`ALTER TABLE plugins ADD COLUMN entry_stamp TEXT NOT NULL DEFAULT ''`,    // entryStamp of the row when entry_identity was written; '' = the identity is not valid
		`ALTER TABLE plugins ADD COLUMN selected_run TEXT NOT NULL DEFAULT ''`,   // Plugin.selected as JSON; '' = none (selectedrun.go)
	} {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	return &DB{db}, nil
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

// checkValue accepts JSON objects, arrays, strings, finite numbers and booleans. No null.
func checkValue(v any) error {
	switch t := v.(type) {
	case nil:
		return bad("null is not supported")
	case string, bool:
		return nil
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return bad("numbers must be finite")
		}
		return nil
	case []any:
		for _, e := range t {
			if err := checkValue(e); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for _, e := range t {
			if err := checkValue(e); err != nil {
				return err
			}
		}
		return nil
	}
	return bad("unsupported value type %T", v)
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

func parseRev(v any) (int64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, bad("revisions are decimal strings")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != s {
		return 0, bad("bad revision %q", s)
	}
	return n, nil
}

func revString(n int64) string { return strconv.FormatInt(n, 10) }

// storeScan handles hub.store.scan {prefix?, after?, limit?}: the caller's records in key order,
// one snapshot per page. Only the caller's namespace. next is the last key when more records exist.
// A page ends at limit records or at scanPageBytes of encoded records, whichever comes first.
func (db *DB) storeScan(ctx context.Context, ns string, args map[string]any) (any, error) {
	prefix, _ := args["prefix"].(string)
	after, _ := args["after"].(string)
	limit := 500
	if v, ok := args["limit"].(float64); ok {
		if v < 1 || v > 500 || v != float64(int(v)) {
			return nil, bad("limit is an integer from 1 to 500")
		}
		limit = int(v)
	}
	// Keys are compared as SQLite text (byte order). The prefix range ends before prefix+0xFF.
	rows, err := db.QueryContext(ctx, `SELECT key, value, rev FROM records WHERE ns = ? AND key > ? AND substr(key, 1, ?) = ?
		ORDER BY key LIMIT ?`, ns, after, len(prefix), prefix, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := map[string]any{}
	var keys []string
	size, full := 0, false
	for rows.Next() {
		var key, value string
		var rev int64
		if err := rows.Scan(&key, &value, &rev); err != nil {
			return nil, err
		}
		if len(keys) == limit {
			keys = append(keys, key) // only to show that more records exist
			break
		}
		record := map[string]any{"value": json.RawMessage(value), "rev": revString(rev)}
		n := scanRecordBytes(key, record)
		if len(keys) > 0 && size+n > scanPageBytes {
			full = true // this record starts the next page
			break
		}
		size += n
		keys = append(keys, key)
		records[key] = record
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]any{"records": records}
	if full || len(keys) > limit {
		out["next"] = keys[len(records)-1]
	}
	return out, nil
}

// storeOpsScan handles hub.store.ops_scan {after?, limit?}: the caller's saved operation records
// {key, fingerprint, result} in key order, one snapshot per page, with the page rules of
// storeScan. A plugin that moves to hub.sql uses it once, to import its saved replies.
func (db *DB) storeOpsScan(ctx context.Context, ns string, args map[string]any) (any, error) {
	for k := range args {
		if k != "after" && k != "limit" {
			return nil, bad("hub.store.ops_scan takes after and limit, not %s", k)
		}
	}
	after, _ := args["after"].(string)
	limit := 500
	if v, ok := args["limit"].(float64); ok {
		if v < 1 || v > 500 || v != float64(int(v)) {
			return nil, bad("limit is an integer from 1 to 500")
		}
		limit = int(v)
	}
	rows, err := db.QueryContext(ctx, `SELECT key, fingerprint, result FROM ops WHERE ns = ? AND key > ? ORDER BY key LIMIT ?`, ns, after, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ops := []any{}
	size, more := 0, false
	for rows.Next() {
		var key, fp, result string
		if err := rows.Scan(&key, &fp, &result); err != nil {
			return nil, err
		}
		if len(ops) == limit {
			more = true
			break
		}
		op := map[string]any{"key": key, "fingerprint": fp, "result": json.RawMessage(result)}
		b, _ := json.Marshal(op)
		if len(ops) > 0 && size+len(b) > scanPageBytes {
			more = true // this record starts the next page
			break
		}
		size += len(b)
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]any{"ops": ops}
	if more {
		out["next"] = ops[len(ops)-1].(map[string]any)["key"]
	}
	return out, nil
}

// scanPageBytes bounds the encoded records of one hub.store.scan page, so that a page of large
// records stays far below the reply limit (Rex's max_message_bytes, about 1 MiB). A page can
// therefore hold fewer than limit records; callers follow next. A page always holds at least one
// record, so a scan never stops at a large record. The hub has no record size limit of its own: a
// record comes from one hub.store.batch, and a Lua plugin sends at most 64 KiB in one request
// (rex-protocol.md). The reply encoder writes '<', '>' and '&' as 6 bytes, so such a record
// encodes to at most about 384 KiB. A client with a larger message limit (a Go client gets 1 MiB)
// can write a record that no reply can carry; nothing guards that case.
const scanPageBytes = 512 << 10

// scanRecordBytes is the encoded size of one record in a scan result: its key, its value and rev,
// and the separators. It uses the encoder that writes the reply, so escapes are counted.
func scanRecordBytes(key string, record map[string]any) int {
	k, _ := json.Marshal(key)
	v, _ := json.Marshal(record)
	return len(k) + len(v) + 2 // ':' and ','
}

func (db *DB) storeGet(ctx context.Context, ns string, args map[string]any) (any, error) {
	keys, ok := asList(args["keys"])
	if !ok {
		return nil, bad("keys must be a list")
	}
	tx, err := db.BeginTx(ctx, nil) // one snapshot for every key
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	records := map[string]any{}
	for _, k := range keys {
		key, ok := k.(string)
		if !ok || key == "" {
			return nil, bad("keys are nonempty strings")
		}
		var value string
		var rev int64
		err := tx.QueryRowContext(ctx, `SELECT value, rev FROM records WHERE ns = ? AND key = ?`, ns, key).Scan(&value, &rev)
		if errors.Is(err, sql.ErrNoRows) {
			records[key] = map[string]any{"rev": "0"}
			continue
		}
		if err != nil {
			return nil, err
		}
		records[key] = map[string]any{"value": json.RawMessage(value), "rev": revString(rev)}
	}
	return map[string]any{"records": records}, nil
}

func (db *DB) storeOp(ctx context.Context, ns string, args map[string]any) (any, error) {
	key, _ := args["key"].(string)
	if key == "" {
		return nil, bad("key is a nonempty string")
	}
	var fp, result string
	err := db.QueryRowContext(ctx, `SELECT fingerprint, result FROM ops WHERE ns = ? AND key = ?`, ns, key).Scan(&fp, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{"found": false}, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"found": true, "fingerprint": fp, "result": json.RawMessage(result)}, nil
}

type recordOp struct {
	key    string
	value  string // JSON; "" for delete
	delete bool
	ifRev  *int64
}

// noticeOp is one notify op: {agent, pending}, {agent, pending: true, once: true} (a new revision
// only if the row is not pending or the agent read since the last one), or {agent, read: true}
// (the agent read; pending does not change).
type noticeOp struct {
	agent               string
	pending, once, read bool
}

// parseNotice reads the fields of a notify op of agent. hub.sql.commit notices use it too.
func parseNotice(agent string, nm map[string]any) (noticeOp, error) {
	op := noticeOp{agent: agent}
	_, hasPending := nm["pending"]
	_, hasOnce := nm["once"]
	if r, hasRead := nm["read"]; hasRead {
		if r != true || hasPending || hasOnce {
			return op, bad("a read is {notify: {agent, read: true}}, with no pending or once")
		}
		op.read = true
		return op, nil
	}
	pending, okP := nm["pending"].(bool)
	once, okO := nm["once"].(bool)
	if !okP || (hasOnce && (!okO || !pending)) {
		return op, bad("notify needs a boolean pending; once is true or false, and only with pending: true")
	}
	op.pending, op.once = pending, once
	return op, nil
}

// applyNotice applies one notify op in tx. rev is the new intent revision, or 0 when the op made
// none. hub.sql outbox rows use it too (applySQLNotices).
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

type batchOutcome struct {
	result    any
	committed bool
	opKey     string
	revs      map[string]string
	notices   []noticeOp
	intentRev map[string]int64 // agent -> new intent revision, for pending notices
}

// storeBatch validates the whole batch, then applies it in one transaction (README section 3).
// noticed reports whether ns has a notice in its manifest: without one, a notify op fails with code
// no_notice and nothing in the batch is written (docs/plugin-sources.md, section 10).
func (db *DB) storeBatch(ctx context.Context, ns string, args map[string]any, roster map[string]bool, noticed bool) (*batchOutcome, error) {
	rawOps, ok := asList(args["ops"])
	if !ok {
		return nil, bad("ops must be a list")
	}
	var recs []recordOp
	var notes []noticeOp
	seenKey, seenAgent := map[string]bool{}, map[string]bool{}
	for _, r := range rawOps {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, bad("each op is an object")
		}
		if n, isNotice := m["notify"]; isNotice {
			if !noticed {
				return nil, refuse("no_notice", "plugin %s has no notice in its manifest", ns)
			}
			nm, ok := n.(map[string]any)
			if !ok || len(m) != 1 {
				return nil, bad("a notify op is {notify: {agent, pending}}, {notify: {agent, pending: true, once: true}} or {notify: {agent, read: true}}")
			}
			agent, _ := nm["agent"].(string)
			if !roster[agent] {
				return nil, bad("notify needs a roster agent")
			}
			op, err := parseNotice(agent, nm)
			if err != nil {
				return nil, err
			}
			if seenAgent[agent] {
				return nil, bad("agent %q appears twice", agent)
			}
			seenAgent[agent] = true
			notes = append(notes, op)
			continue
		}
		key, _ := m["key"].(string)
		if key == "" {
			return nil, bad("a record op needs a nonempty key")
		}
		if seenKey[key] {
			return nil, bad("key %q appears twice", key)
		}
		seenKey[key] = true
		op := recordOp{key: key}
		del, hasDel := m["delete"]
		val, hasVal := m["value"]
		switch {
		case hasDel && hasVal, !hasDel && !hasVal:
			return nil, bad("op %q needs exactly one of value and delete", key)
		case hasDel:
			if del != true {
				return nil, bad("delete must be true")
			}
			op.delete = true
		default:
			if err := checkValue(val); err != nil {
				return nil, err
			}
			b, _ := json.Marshal(val)
			op.value = string(b)
		}
		if iv, ok := m["if_rev"]; ok {
			n, err := parseRev(iv)
			if err != nil {
				return nil, err
			}
			op.ifRev = &n
		}
		for k := range m {
			if k != "key" && k != "value" && k != "delete" && k != "if_rev" {
				return nil, bad("unknown field %q in op", k)
			}
		}
		recs = append(recs, op)
	}
	var opKey, opFP, opResult string
	// op.result_revs {field: record key}: the committed revision of each record goes into
	// the saved result under field, in the same transaction. A replay then returns the
	// revisions that the first execution produced.
	var resultRevs map[string]string
	if o, ok := args["op"]; ok {
		om, ok := o.(map[string]any)
		if !ok {
			return nil, bad("op is {key, fingerprint, result}")
		}
		opKey, _ = om["key"].(string)
		opFP, _ = om["fingerprint"].(string)
		if opKey == "" || opFP == "" {
			return nil, bad("op key and fingerprint are nonempty strings")
		}
		if err := checkValue(om["result"]); err != nil {
			return nil, err
		}
		b, _ := json.Marshal(om["result"])
		opResult = string(b)
		if rr, ok := om["result_revs"].(map[string]any); ok && len(rr) > 0 {
			if _, isMap := om["result"].(map[string]any); !isMap {
				return nil, bad("op result_revs needs an object result")
			}
			resultRevs = map[string]string{}
			for field, k := range rr {
				key, _ := k.(string)
				found := false
				for _, r := range recs {
					found = found || r.key == key
				}
				if !found {
					return nil, bad("op result_revs %q names a key that this batch does not write", field)
				}
				resultRevs[field] = key
			}
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := &batchOutcome{opKey: opKey}
	// A saved op is checked before any revision condition, so a retry succeeds with stale revisions.
	if opKey != "" {
		var fp, result string
		err := tx.QueryRowContext(ctx, `SELECT fingerprint, result FROM ops WHERE ns = ? AND key = ?`, ns, opKey).Scan(&fp, &result)
		switch {
		case err == nil && fp == opFP:
			out.result = map[string]any{"op_exists": map[string]any{"fingerprint": fp, "result": json.RawMessage(result)}}
			return out, nil
		case err == nil:
			out.result = map[string]any{"conflict": map[string]any{"kind": "operation", "key": opKey}}
			return out, nil
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
	}
	for _, r := range recs {
		if r.ifRev == nil {
			continue
		}
		var cur int64
		err := tx.QueryRowContext(ctx, `SELECT rev FROM records WHERE ns = ? AND key = ?`, ns, r.key).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			cur = 0
		} else if err != nil {
			return nil, err
		}
		if cur != *r.ifRev {
			out.result = map[string]any{"conflict": map[string]any{"kind": "revision", "key": r.key, "rev": revString(cur)}}
			return out, nil
		}
	}
	revs := map[string]string{}
	for _, r := range recs {
		if r.delete {
			if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE ns = ? AND key = ?`, ns, r.key); err != nil {
				return nil, err
			}
			revs[r.key] = "0"
			continue
		}
		rev, err := nextRev(tx)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records VALUES (?, ?, ?, ?) ON CONFLICT (ns, key) DO UPDATE SET value = excluded.value, rev = excluded.rev`, ns, r.key, r.value, rev); err != nil {
			return nil, err
		}
		revs[r.key] = revString(rev)
	}
	out.intentRev = map[string]int64{}
	for _, n := range notes {
		rev, err := applyNotice(ctx, tx, ns, n)
		if err != nil {
			return nil, err
		}
		if rev != 0 {
			out.intentRev[n.agent] = rev
		}
	}
	if opKey != "" {
		if len(resultRevs) > 0 {
			var m map[string]any
			json.Unmarshal([]byte(opResult), &m)
			for field, key := range resultRevs {
				m[field] = revs[key]
			}
			b, _ := json.Marshal(m)
			opResult = string(b)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ops VALUES (?, ?, ?, ?)`, ns, opKey, opFP, opResult); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out.committed, out.revs, out.notices = true, revs, notes
	out.result = map[string]any{"revs": revs}
	return out, nil
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
// the apply and before the outbox delete): it is skipped. The rules are those of a notify op of
// hub.store.batch.
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

// moveNotices records that plugin sends its notices through hub.sql: from now on its
// hub.store.batch notify ops are refused (notices_moved). The caller holds the store fence
// exclusively (admitSQLNotices), so the two paths cannot reorder.
func (db *DB) moveNotices(ctx context.Context, plugin string) error {
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO sql_notice_plugins VALUES (?)`, plugin)
	return err
}

func (db *DB) noticesMoved(ctx context.Context, plugin string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM sql_notice_plugins WHERE plugin = ?`, plugin).Scan(&n)
	return n > 0, err
}
