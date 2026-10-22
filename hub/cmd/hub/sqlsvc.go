package main

// The hub.sql actions (docs/sql.md): handler transactions, saved replies and the retirement fence.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sqlTxIdle   = 5 * time.Second  // no hub call of the instance for this long rolls the transaction back
	sqlTxTotal  = 10 * time.Second // a transaction is open at most this long
	sqlQueueMax = 5 * time.Second  // a call that waited this long for the writer fails with busy
	sqlOpsAge   = 7 * 24 * time.Hour
	sqlOpsMax   = 10000
	sqlExpired  = 256 // the expired tokens that the hub remembers for each plugin
)

// sqlService keeps the open plugin files.
type sqlService struct {
	dir       string
	current   func(plugin, client string) bool    // is client the plugin's registered instance?
	committed func(plugin string, marks []string) // after a commit that changed tables ("*": the schema)
	idle      time.Duration
	total     time.Duration
	queue     time.Duration

	mu  sync.Mutex
	dbs map[string]*pluginSQL
}

func newSQLService(state string, current func(plugin, client string) bool) *sqlService {
	return &sqlService{dir: filepath.Join(state, "sql"), current: current, committed: func(string, []string) {}, idle: sqlTxIdle, total: sqlTxTotal,
		queue: sqlQueueMax, dbs: map[string]*pluginSQL{}}
}

// pluginSQL is one plugin file. lock is the plugin's SQL lock: it serializes the use of the writer.
// Lock order: a plugin's lifecycle lock, then lock, then mu. No SQL lock is taken under h.mu.
type pluginSQL struct {
	name string
	lock chan struct{}
	w    *sqlConn
	r    *sqlConn
	rmu  sync.Mutex // serializes the reader

	mu      sync.Mutex // guards tx and expired
	tx      *sqlTx
	expired map[string]string // token -> the code that later calls get
	order   []string
}

// sqlTx is an open handler transaction.
type sqlTx struct {
	token  string
	client string
	begun  time.Time
	last   time.Time
	timer  *time.Timer
	done   chan struct{} // closed when the transaction ends
	marks  map[string]bool
}

func (s *sqlService) open(ctx context.Context, name string) (*pluginSQL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.dbs[name]; p != nil {
		return p, nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, name+".db")
	w, err := openSQLConn(ctx, path, false)
	if err != nil {
		return nil, err
	}
	for _, q := range strings.Split(sqlHubSchema, ";\n") {
		if err := w.hub(ctx, q); err != nil {
			w.close()
			return nil, err
		}
	}
	r, err := openSQLConn(ctx, path, true)
	if err != nil {
		w.close()
		return nil, err
	}
	p := &pluginSQL{name: name, lock: make(chan struct{}, 1), w: w, r: r, expired: map[string]string{}}
	s.dbs[name] = p
	return p, nil
}

// acquire takes the plugin's SQL lock; a wait longer than the queue limit fails with busy.
func (s *sqlService) acquire(p *pluginSQL) error {
	t := time.NewTimer(s.queue)
	defer t.Stop()
	select {
	case p.lock <- struct{}{}:
		return nil
	case <-t.C:
		return refuse("busy", "the plugin's database was busy for %s", s.queue)
	}
}

func (p *pluginSQL) release() { <-p.lock }

// end closes the open transaction with a rollback or after a commit. The caller holds the lock.
// code is what later calls with the token get ("" after a commit: the token is unknown).
func (p *pluginSQL) end(ctx context.Context, code string) {
	p.mu.Lock()
	tx := p.tx
	p.tx = nil
	if tx != nil && code != "" {
		p.expired[tx.token] = code
		p.order = append(p.order, tx.token)
		if len(p.order) > sqlExpired {
			delete(p.expired, p.order[0])
			p.order = p.order[1:]
		}
	}
	p.mu.Unlock()
	if tx == nil {
		return
	}
	tx.timer.Stop()
	if code != "" {
		p.w.hub(context.WithoutCancel(ctx), "ROLLBACK")
	}
	close(tx.done)
}

// txFor finds the open transaction of token for client. The caller holds the lock.
func (p *pluginSQL) txFor(token, client string) (*sqlTx, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tx != nil && p.tx.token == token {
		if p.tx.client != client {
			return nil, refuse("stale_instance", "the transaction belongs to another instance")
		}
		p.tx.last = time.Now()
		return p.tx, nil
	}
	if code, ok := p.expired[token]; ok {
		return nil, refuse(code, "the transaction ended: %s", code)
	}
	return nil, refuse("tx_expired", "no open transaction with this token")
}

// touch records a hub call of client: an open transaction of that instance stays open.
func (s *sqlService) touch(name, client string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	p := s.dbs[name]
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.tx != nil && p.tx.client == client {
		p.tx.last = time.Now()
	}
	p.mu.Unlock()
}

// expire is the timer of an open transaction: it rolls back after the idle or the total limit.
func (s *sqlService) expire(p *pluginSQL, tx *sqlTx) {
	p.mu.Lock()
	if p.tx != tx {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	due := tx.last.Add(s.idle)
	if end := tx.begun.Add(s.total); end.Before(due) {
		due = end
	}
	if now.Before(due) {
		tx.timer.Reset(due.Sub(now))
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.lock <- struct{}{} // a running statement ends within its 5 s
	defer p.release()
	if p.tx == tx {
		p.end(context.Background(), "tx_expired")
	}
}

// fence ends the transactions of a plugin's old instance: an open transaction rolls back, and a
// running statement ends first (docs/sql.md section 9). The hub calls it after it marks the
// instance retired, and when a new instance registers.
func (s *sqlService) fence(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	p := s.dbs[name]
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.lock <- struct{}{}
	p.end(context.Background(), "stale_instance")
	p.release()
}

// close rolls back each open transaction and closes the files (hub shutdown).
func (s *sqlService) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, p := range s.dbs {
		p.lock <- struct{}{}
		p.end(context.Background(), "unavailable")
		p.w.close()
		p.r.close()
		delete(s.dbs, name)
	}
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sqlCall is one hub.sql call of a plugin instance.
type sqlCall struct {
	plugin string
	client string
	args   map[string]any
}

// dispatch runs one hub.sql action.
func (s *sqlService) dispatch(ctx context.Context, action string, c sqlCall) (any, error) {
	if s == nil {
		return nil, refuse("unavailable", "the hub has no SQL service")
	}
	p, err := s.open(ctx, c.plugin)
	if err != nil {
		return nil, refuse("unavailable", "open the plugin database: %v", err)
	}
	switch action {
	case "hub.sql.begin":
		return s.begin(ctx, p, c)
	case "hub.sql.exec":
		return s.exec(ctx, p, c)
	case "hub.sql.query":
		return s.query(ctx, p, c)
	case "hub.sql.batch":
		return s.batch(ctx, p, c)
	case "hub.sql.commit":
		return s.commit(ctx, p, c)
	case "hub.sql.rollback":
		return s.rollback(ctx, p, c)
	case "hub.sql.replay":
		return s.replay(ctx, p, c)
	}
	return nil, bad("unknown action %s", action)
}

// withWriter takes the SQL lock and checks that the caller is the current instance. Without a
// token, a call waits while another call's transaction is open.
func (s *sqlService) withWriter(p *pluginSQL, c sqlCall, token string, fn func(tx *sqlTx) (any, error)) (any, error) {
	deadline := time.Now().Add(s.queue)
	for {
		if err := s.acquire(p); err != nil {
			return nil, err
		}
		if !s.current(c.plugin, c.client) {
			p.release()
			return nil, refuse("stale_instance", "the caller is not the plugin's registered instance")
		}
		if token != "" {
			tx, err := p.txFor(token, c.client)
			if err != nil {
				p.release()
				return nil, err
			}
			defer p.release()
			return fn(tx)
		}
		p.mu.Lock()
		open := p.tx
		p.mu.Unlock()
		if open != nil && !s.current(c.plugin, open.client) {
			p.end(context.Background(), "stale_instance") // the instance of that transaction is gone
			open = nil
		}
		if open == nil {
			defer p.release()
			return fn(nil)
		}
		p.release()
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, refuse("busy", "another call's transaction stayed open for %s", s.queue)
		}
		t := time.NewTimer(wait)
		select {
		case <-open.done:
		case <-t.C:
		}
		t.Stop()
	}
}

func tokenArg(args map[string]any) (string, error) {
	v, ok := args["tx"]
	if !ok || v == nil {
		return "", nil
	}
	t, ok := v.(string)
	if !ok || t == "" {
		return "", refuse("invalid_input", "tx is the token string of hub.sql.begin")
	}
	return t, nil
}

func (s *sqlService) begin(ctx context.Context, p *pluginSQL, c sqlCall) (any, error) {
	if len(c.args) > 0 {
		return nil, refuse("invalid_input", "hub.sql.begin takes no arguments")
	}
	return s.withWriter(p, c, "", func(*sqlTx) (any, error) {
		if err := p.w.hub(ctx, "BEGIN DEFERRED"); err != nil {
			return nil, refuse("sql_error", "begin: %v", err)
		}
		now := time.Now()
		tx := &sqlTx{token: newToken(), client: c.client, begun: now, last: now, done: make(chan struct{}), marks: map[string]bool{}}
		p.mu.Lock()
		p.tx = tx
		tx.timer = time.AfterFunc(s.idle, func() { s.expire(p, tx) })
		p.mu.Unlock()
		return map[string]any{"tx": tx.token}, nil
	})
}

// stmtArgs reads sql and params.
func stmtArgs(args map[string]any) (string, []any, error) {
	q, ok := args["sql"].(string)
	if !ok {
		return "", nil, refuse("invalid_input", "sql is a string")
	}
	params, err := sqlParams(args["params"])
	return q, params, err
}

func expectArg(args map[string]any, name string) (int64, bool, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f != float64(int64(f)) {
		return 0, false, refuse("invalid_input", "%s is a count", name)
	}
	return int64(f), true, nil
}

// execReply is the reply of one exec statement.
func execReply(r *stmtResult) map[string]any {
	out := map[string]any{"changes": r.changes, "id": r.id}
	if len(r.columns) > 0 {
		out["columns"], out["rows"] = r.columns, rowsOrEmpty(r.rows)
	}
	return out
}

func rowsOrEmpty(rows [][]any) [][]any {
	if rows == nil {
		return [][]any{}
	}
	return rows
}

// stmt runs one exec statement of a call inside the open SQLite transaction, under a savepoint: a
// failed statement leaves no effect, and the transaction continues.
func (s *sqlService) stmt(ctx context.Context, p *pluginSQL, args map[string]any, budget int) (*stmtResult, error) {
	q, params, err := stmtArgs(args)
	if err != nil {
		return nil, err
	}
	want, check, err := expectArg(args, "expect_changes")
	if err != nil {
		return nil, err
	}
	if err := p.w.hub(ctx, "SAVEPOINT lazyrex_stmt"); err != nil {
		return nil, refuse("sql_error", "savepoint: %v", err)
	}
	r, err := p.w.run(ctx, q, params, false, budget)
	if err == nil && check {
		if r.ddl || (len(r.writes) == 0 && r.changed == 0) {
			err = refuse("invalid_input", "expect_changes applies to INSERT, UPDATE and DELETE only")
		} else if r.changes != want {
			err = refuse("conflict", "the statement changed %d rows, expected %d", r.changes, want)
		}
	}
	if err != nil {
		p.w.hub(context.WithoutCancel(ctx), "ROLLBACK TO lazyrex_stmt")
		p.w.hub(context.WithoutCancel(ctx), "RELEASE lazyrex_stmt")
		return nil, err
	}
	if err := p.w.hub(ctx, "RELEASE lazyrex_stmt"); err != nil {
		return nil, refuse("sql_error", "release: %v", err)
	}
	return r, nil
}

// alone runs fn in a transaction of its own on the writer: all or nothing.
func (s *sqlService) alone(ctx context.Context, p *pluginSQL, fn func() (any, []string, error)) (any, []string, error) {
	if err := p.w.hub(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, nil, refuse("sql_error", "begin: %v", err)
	}
	out, marks, err := fn()
	if err != nil {
		p.w.hub(context.WithoutCancel(ctx), "ROLLBACK")
		return nil, nil, err
	}
	if err := p.w.hub(ctx, "COMMIT"); err != nil {
		p.w.hub(context.WithoutCancel(ctx), "ROLLBACK")
		return nil, nil, refuse("sql_error", "commit: %v", err)
	}
	return out, marks, nil
}

func (s *sqlService) exec(ctx context.Context, p *pluginSQL, c sqlCall) (any, error) {
	token, err := tokenArg(c.args)
	if err != nil {
		return nil, err
	}
	return s.withWriter(p, c, token, func(tx *sqlTx) (any, error) {
		if tx != nil {
			r, err := s.stmt(ctx, p, c.args, sqlReplyBudget)
			if err != nil {
				return nil, err
			}
			for _, m := range r.marks() {
				tx.marks[m] = true
			}
			return execReply(r), nil
		}
		out, marks, err := s.alone(ctx, p, func() (any, []string, error) {
			r, err := s.stmt(ctx, p, c.args, sqlReplyBudget)
			if err != nil {
				return nil, nil, err
			}
			return execReply(r), r.marks(), nil
		})
		if err == nil && len(marks) > 0 {
			s.committed(c.plugin, marks)
		}
		return out, err
	})
}

func (s *sqlService) query(ctx context.Context, p *pluginSQL, c sqlCall) (any, error) {
	token, err := tokenArg(c.args)
	if err != nil {
		return nil, err
	}
	q, params, err := stmtArgs(c.args)
	if err != nil {
		return nil, err
	}
	want, check, err := expectArg(c.args, "expect_rows")
	if err != nil {
		return nil, err
	}
	reply := func(r *stmtResult) (any, error) {
		if check && (!r.complete || int64(len(r.rows)) != want) {
			return nil, refuse("conflict", "the query returned %d rows, expected %d", len(r.rows), want)
		}
		return map[string]any{"columns": r.columns, "rows": rowsOrEmpty(r.rows), "complete": r.complete}, nil
	}
	if token != "" {
		return s.withWriter(p, c, token, func(*sqlTx) (any, error) {
			r, err := p.w.run(ctx, q, params, true, sqlReplyBudget)
			if err != nil {
				return nil, err
			}
			return reply(r)
		})
	}
	if !s.current(c.plugin, c.client) {
		return nil, refuse("stale_instance", "the caller is not the plugin's registered instance")
	}
	p.rmu.Lock()
	defer p.rmu.Unlock()
	r, err := p.r.run(ctx, q, params, true, sqlReplyBudget)
	if err != nil {
		return nil, err
	}
	return reply(r)
}

func (s *sqlService) batch(ctx context.Context, p *pluginSQL, c sqlCall) (any, error) {
	token, err := tokenArg(c.args)
	if err != nil {
		return nil, err
	}
	list, ok := c.args["statements"].([]any)
	if !ok || len(list) == 0 {
		return nil, refuse("invalid_input", "statements is a non-empty array of {sql, params?, expect_changes?}")
	}
	var stmts []map[string]any
	for i, x := range list {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, refuse("invalid_input", "statements[%d] is an object", i+1)
		}
		stmts = append(stmts, m)
	}
	run := func() (any, []string, error) {
		results := make([]any, 0, len(stmts))
		var marks []string
		budget := sqlReplyBudget
		for i, m := range stmts {
			r, err := s.stmt(ctx, p, m, budget)
			if err != nil {
				var se *storeError
				if errors.As(err, &se) {
					return nil, nil, &storeError{msg: se.msg + " (statements[" + strconv.Itoa(i+1) + "])", code: se.code}
				}
				return nil, nil, err
			}
			budget -= r.size + 32
			results = append(results, execReply(r))
			marks = append(marks, r.marks()...)
		}
		return map[string]any{"results": results}, marks, nil
	}
	return s.withWriter(p, c, token, func(tx *sqlTx) (any, error) {
		if tx != nil {
			// All or nothing inside the transaction too.
			if err := p.w.hub(ctx, "SAVEPOINT lazyrex_batch"); err != nil {
				return nil, refuse("sql_error", "savepoint: %v", err)
			}
			out, marks, err := run()
			if err != nil {
				p.w.hub(context.WithoutCancel(ctx), "ROLLBACK TO lazyrex_batch")
				p.w.hub(context.WithoutCancel(ctx), "RELEASE lazyrex_batch")
				return nil, err
			}
			p.w.hub(ctx, "RELEASE lazyrex_batch")
			for _, m := range marks {
				tx.marks[m] = true
			}
			return out, nil
		}
		out, marks, err := s.alone(ctx, p, run)
		if err == nil && len(marks) > 0 {
			s.committed(c.plugin, marks)
		}
		return out, err
	})
}

// savedReply reads a saved reply of key. The caller holds the lock or reads on the reader.
func savedReply(ctx context.Context, k *sqlConn, key, fingerprint string) (map[string]any, error) {
	k.trusted = true
	defer func() { k.trusted = false }()
	var fp, reply string
	err := k.c.QueryRowContext(ctx, `SELECT fingerprint, reply FROM lazyrex_ops WHERE key = ?`, key).Scan(&fp, &reply)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, refuse("sql_error", "read the saved reply: %v", err)
	}
	if fp != fingerprint {
		return nil, refuse("operation_conflict", "the key %s was used with another fingerprint", key)
	}
	var v any
	json.Unmarshal([]byte(reply), &v)
	return map[string]any{"reply": v}, nil
}

func keyArgs(args map[string]any) (key, fingerprint string, err error) {
	key, _ = args["key"].(string)
	fingerprint, _ = args["fingerprint"].(string)
	if _, ok := args["key"]; ok && key == "" {
		return "", "", refuse("invalid_input", "key is a non-empty string")
	}
	if key != "" && fingerprint == "" {
		return "", "", refuse("invalid_input", "a key needs a fingerprint")
	}
	return key, fingerprint, nil
}

func (s *sqlService) commit(ctx context.Context, p *pluginSQL, c sqlCall) (any, error) {
	token, err := tokenArg(c.args)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, refuse("invalid_input", "tx is required")
	}
	key, fingerprint, err := keyArgs(c.args)
	if err != nil {
		return nil, err
	}
	if n, ok := c.args["notices"].([]any); ok && len(n) > 0 {
		return nil, refuse("invalid_input", "notices in hub.sql.commit are not implemented yet")
	}
	var reply []byte
	if key != "" {
		if reply, err = json.Marshal(c.args["reply"]); err != nil {
			return nil, refuse("invalid_input", "reply: %v", err)
		}
	}
	return s.withWriter(p, c, token, func(tx *sqlTx) (any, error) {
		if key != "" {
			saved, err := savedReply(ctx, p.w, key, fingerprint)
			if err != nil {
				p.end(ctx, "tx_expired")
				return nil, err
			}
			if saved != nil {
				p.end(ctx, "tx_expired") // the first answer stands: this handler's work is undone
				saved["replayed"] = true
				return saved, nil
			}
			now := time.Now()
			err = p.w.hub(ctx, `INSERT INTO lazyrex_ops (key, fingerprint, reply, at) VALUES (?, ?, ?, ?)`, key, fingerprint, string(reply), now.UnixMilli())
			if err == nil {
				err = p.w.hub(ctx, `DELETE FROM lazyrex_ops WHERE at < ? OR rowid IN
					(SELECT rowid FROM lazyrex_ops ORDER BY at DESC, rowid DESC LIMIT -1 OFFSET ?)`, now.Add(-sqlOpsAge).UnixMilli(), sqlOpsMax)
			}
			if err != nil {
				p.end(ctx, "tx_expired")
				return nil, refuse("sql_error", "save the reply: %v", err)
			}
		}
		if err := p.w.hub(ctx, "COMMIT"); err != nil {
			p.end(ctx, "tx_expired")
			return nil, refuse("sql_error", "commit: %v; nothing was committed", err)
		}
		marks := make([]string, 0, len(tx.marks))
		for m := range tx.marks {
			marks = append(marks, m)
		}
		p.end(ctx, "")
		if len(marks) > 0 {
			s.committed(c.plugin, marks)
		}
		return map[string]any{}, nil
	})
}

func (s *sqlService) rollback(ctx context.Context, p *pluginSQL, c sqlCall) (any, error) {
	token, err := tokenArg(c.args)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, refuse("invalid_input", "tx is required")
	}
	if err := s.acquire(p); err != nil {
		return nil, err
	}
	defer p.release()
	if !s.current(c.plugin, c.client) {
		return nil, refuse("stale_instance", "the caller is not the plugin's registered instance")
	}
	p.mu.Lock()
	mine := p.tx != nil && p.tx.token == token && p.tx.client == c.client
	p.mu.Unlock()
	if mine {
		p.end(ctx, "tx_expired")
	}
	// Idempotent: an unknown or ended transaction is rolled back already.
	return map[string]any{}, nil
}

func (s *sqlService) replay(ctx context.Context, p *pluginSQL, c sqlCall) (any, error) {
	key, fingerprint, err := keyArgs(c.args)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, refuse("invalid_input", "key is required")
	}
	if !s.current(c.plugin, c.client) {
		return nil, refuse("stale_instance", "the caller is not the plugin's registered instance")
	}
	p.rmu.Lock()
	defer p.rmu.Unlock()
	saved, err := savedReply(ctx, p.r, key, fingerprint)
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return map[string]any{"found": false}, nil
	}
	saved["found"] = true
	return saved, nil
}
