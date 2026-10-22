package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sqlLab is a sqlService with one plugin "p" whose current client is "c1".
type sqlLab struct {
	t       *testing.T
	s       *sqlService
	mu      sync.Mutex
	client  string
	commits [][]string
}

func newSQLLab(t *testing.T) *sqlLab {
	l := &sqlLab{t: t, client: "c1"}
	l.s = newSQLService(t.TempDir(), func(plugin, client string) bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return plugin == "p" && client == l.client
	})
	l.s.committed = func(plugin string, marks []string) {
		l.mu.Lock()
		l.commits = append(l.commits, marks)
		l.mu.Unlock()
	}
	t.Cleanup(l.s.close)
	return l
}

// call runs an action as client with args given as JSON, and returns the reply as decoded JSON.
func (l *sqlLab) call(client, action, args string) (map[string]any, error) {
	var a map[string]any
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		l.t.Fatalf("%s: bad test args %s", action, args)
	}
	out, err := l.s.dispatch(context.Background(), action, sqlCall{plugin: "p", client: client, args: a})
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(out)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m, nil
}

func (l *sqlLab) must(action, args string) map[string]any {
	l.t.Helper()
	m, err := l.call(l.client, action, args)
	if err != nil {
		l.t.Fatalf("%s %s: %v", action, args, err)
	}
	return m
}

func (l *sqlLab) fails(code, action, args string) string {
	l.t.Helper()
	_, err := l.call(l.client, action, args)
	var se *storeError
	if !errors.As(err, &se) || se.code != code {
		l.t.Fatalf("%s %s: want %s, got %v", action, args, code, err)
	}
	return se.msg
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSQLExecQueryAndValues(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (a, b, c)"}`)
	r := l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (?, ?, ?)", "params":[1, {"null":true}, 3]}`)
	if r["changes"] != 1.0 || r["id"] != 1.0 {
		t.Fatalf("insert: %v", r)
	}
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (:a, :b, :c)", "params":{"a":{"int":"9007199254740993"}, "b":{"blob":"AP8="}, "c":{"null":true}}}`)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (?, ?, ?)", "params":["x", true, 2.5]}`)
	q := l.must("hub.sql.query", `{"sql":"SELECT a, b, c FROM t ORDER BY rowid", "params":{}}`)
	want := `{"columns":["a","b","c"],"complete":true,"rows":[[1,{"null":true},3],[{"int":"9007199254740993"},{"blob":"AP8="},{"null":true}],["x",1,2.5]]}`
	if got := js(q); got != want {
		t.Fatalf("query:\n got %s\nwant %s", got, want)
	}
	// A JSON integer above 2^53 was rounded by Lua already.
	l.fails("invalid_input", "hub.sql.exec", `{"sql":"INSERT INTO t VALUES (?, 0, 0)", "params":[9007199254740994]}`)
	// RETURNING rows come with the exec reply.
	r = l.must("hub.sql.exec", `{"sql":"UPDATE t SET c = 7 WHERE a = 'x' RETURNING a, c"}`)
	if js(r) != `{"changes":1,"columns":["a","c"],"id":3,"rows":[["x",7]]}` {
		t.Fatalf("returning: %s", js(r))
	}
	// A SELECT after a write reports no changes of its own.
	if r := l.must("hub.sql.exec", `{"sql":"SELECT 1"}`); r["changes"] != 0.0 {
		t.Fatalf("select changes: %v", r)
	}
}

func TestSQLRefusals(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (x)"}`)
	l.fails("invalid_input", "hub.sql.exec", `{"sql":"INSERT INTO t VALUES (1); INSERT INTO t VALUES (2)"}`)
	l.fails("invalid_input", "hub.sql.query", `{"sql":"SELECT 1; DELETE FROM t"}`)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (';') ; -- a comment\n /* and another */"}`)
	l.must("hub.sql.exec", `{"sql":"CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; SELECT 2; END;"}`)
	l.fails("refused", "hub.sql.query", `{"sql":"DELETE FROM t"}`)
	l.fails("refused", "hub.sql.query", `{"sql":"PRAGMA user_version = 3"}`)
	l.fails("refused", "hub.sql.exec", `{"sql":"SELECT * FROM lazyrex_ops"}`)
	l.fails("refused", "hub.sql.exec", `{"sql":"BEGIN"}`)
	l.fails("refused", "hub.sql.exec", `{"sql":"ATTACH 'x.db' AS x"}`)
	l.fails("refused", "hub.sql.exec", `{"sql":"PRAGMA journal_mode = delete"}`)
	msg := l.fails("refused", "hub.sql.exec", `{"sql":"CREATE TABLE ev (at DATETIME, n INTEGER)"}`)
	if !strings.Contains(msg, "ev.at DATETIME") {
		t.Fatalf("the refusal names the column: %s", msg)
	}
	l.fails("refused", "hub.sql.exec", `{"sql":"ALTER TABLE t ADD COLUMN d timestamp"}`)
	if q := l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM sqlite_schema WHERE name = 'ev'"}`); js(q["rows"]) != "[[0]]" {
		t.Fatalf("a refused DDL left its table: %v", q)
	}
	l.must("hub.sql.exec", `{"sql":"PRAGMA user_version = 3"}`)
	if q := l.must("hub.sql.query", `{"sql":"PRAGMA user_version"}`); js(q["rows"]) != "[[3]]" {
		t.Fatalf("user_version: %v", q)
	}
	l.fails("sql_error", "hub.sql.exec", `{"sql":"INSERT INTO nope VALUES (1)"}`)
}

func TestSQLTransactionIsolationAndCommit(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (x)"}`)
	l.commits = nil
	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (1)", "tx":"`+tx+`"}`)
	// The transaction sees its own write; a read outside it does not.
	if q := l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM t", "tx":"`+tx+`"}`); js(q["rows"]) != "[[1]]" {
		t.Fatalf("own write: %v", q)
	}
	if q := l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`); js(q["rows"]) != "[[0]]" {
		t.Fatalf("uncommitted write visible: %v", q)
	}
	if len(l.commits) != 0 {
		t.Fatalf("marks before the commit: %v", l.commits)
	}
	l.must("hub.sql.commit", `{"tx":"`+tx+`"}`)
	if q := l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`); js(q["rows"]) != "[[1]]" {
		t.Fatalf("after the commit: %v", q)
	}
	if js(l.commits) != `[["t"]]` {
		t.Fatalf("commit marks: %v", l.commits)
	}
	l.fails("tx_expired", "hub.sql.exec", `{"sql":"INSERT INTO t VALUES (2)", "tx":"`+tx+`"}`)

	// A rollback undoes the work and is idempotent.
	tx = l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (3)", "tx":"`+tx+`"}`)
	l.must("hub.sql.rollback", `{"tx":"`+tx+`"}`)
	l.must("hub.sql.rollback", `{"tx":"`+tx+`"}`)
	if q := l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`); js(q["rows"]) != "[[1]]" {
		t.Fatalf("after the rollback: %v", q)
	}
	// A failed statement in a transaction leaves no effect, and the transaction continues.
	tx = l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE u (id INTEGER PRIMARY KEY)", "tx":"`+tx+`"}`)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO u VALUES (1)", "tx":"`+tx+`"}`)
	l.fails("sql_error", "hub.sql.exec", `{"sql":"INSERT INTO u VALUES (1)", "tx":"`+tx+`"}`)
	l.fails("conflict", "hub.sql.exec", `{"sql":"UPDATE u SET id = 2 WHERE id = 9", "expect_changes":1, "tx":"`+tx+`"}`)
	l.must("hub.sql.commit", `{"tx":"`+tx+`"}`)
	if q := l.must("hub.sql.query", `{"sql":"SELECT id FROM u"}`); js(q["rows"]) != "[[1]]" {
		t.Fatalf("after a failed statement: %v", q)
	}
}

// A call without the token waits for the open transaction; it never runs inside it.
func TestSQLCallWaitsForOpenTransaction(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (x)"}`)
	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	done := make(chan error, 1)
	go func() {
		_, err := l.call("c1", "hub.sql.exec", `{"sql":"INSERT INTO t VALUES (2)"}`)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (1)", "tx":"`+tx+`"}`)
	l.must("hub.sql.rollback", `{"tx":"`+tx+`"}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if q := l.must("hub.sql.query", `{"sql":"SELECT x FROM t"}`); js(q["rows"]) != "[[2]]" {
		t.Fatalf("the waiting call ran inside the transaction: %v", q)
	}
}

func TestSQLTransactionLimits(t *testing.T) {
	l := newSQLLab(t)
	l.s.idle, l.s.total, l.s.queue = 150*time.Millisecond, 400*time.Millisecond, 300*time.Millisecond
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (x)"}`)
	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (1)", "tx":"`+tx+`"}`)
	time.Sleep(300 * time.Millisecond)
	l.fails("tx_expired", "hub.sql.commit", `{"tx":"`+tx+`"}`)

	// Any hub call of the instance keeps it open; the total limit still ends it.
	tx = l.must("hub.sql.begin", `{}`)["tx"].(string)
	for i := 0; i < 3; i++ {
		time.Sleep(100 * time.Millisecond)
		l.s.touch("p", "c1")
	}
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (2)", "tx":"`+tx+`"}`)
	time.Sleep(200 * time.Millisecond)
	l.fails("tx_expired", "hub.sql.commit", `{"tx":"`+tx+`"}`)
	if q := l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`); js(q["rows"]) != "[[0]]" {
		t.Fatalf("an expired transaction committed: %v", q)
	}
	// A second begin while one is open waits, then fails with busy.
	l.s.idle, l.s.total = time.Hour, time.Hour
	tx = l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.fails("busy", "hub.sql.begin", `{}`)
	l.must("hub.sql.rollback", `{"tx":"`+tx+`"}`)
	l.must("hub.sql.begin", `{}`)
}

func TestSQLStaleInstance(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (x)"}`)
	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (1)", "tx":"`+tx+`"}`)
	l.mu.Lock()
	l.client = "c2" // a new instance registered
	l.mu.Unlock()
	if _, err := l.call("c1", "hub.sql.commit", `{"tx":"`+tx+`"}`); err == nil || !strings.Contains(err.Error(), "stale_instance") {
		t.Fatalf("the old instance committed: %v", err)
	}
	// The new instance does not wait for the old transaction: it rolls back.
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (2)"}`)
	if q := l.must("hub.sql.query", `{"sql":"SELECT x FROM t"}`); js(q["rows"]) != "[[2]]" {
		t.Fatalf("after the new instance: %v", q)
	}
	// The fence rolls back an open transaction at once.
	tx = l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (3)", "tx":"`+tx+`"}`)
	l.s.fence("p")
	l.fails("stale_instance", "hub.sql.commit", `{"tx":"`+tx+`"}`)
	if q := l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`); js(q["rows"]) != "[[1]]" {
		t.Fatalf("after the fence: %v", q)
	}
}

func TestSQLSavedReplies(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (x)"}`)
	if r := l.must("hub.sql.replay", `{"key":"k1", "fingerprint":"f1"}`); r["found"] != false {
		t.Fatalf("replay before: %v", r)
	}
	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (1)", "tx":"`+tx+`"}`)
	if r := l.must("hub.sql.commit", `{"tx":"`+tx+`", "key":"k1", "fingerprint":"f1", "reply":{"id":1}}`); js(r) != "{}" {
		t.Fatalf("commit: %v", r)
	}
	if r := l.must("hub.sql.replay", `{"key":"k1", "fingerprint":"f1"}`); js(r) != `{"found":true,"reply":{"id":1}}` {
		t.Fatalf("replay: %s", js(r))
	}
	l.fails("operation_conflict", "hub.sql.replay", `{"key":"k1", "fingerprint":"f2"}`)
	// A second commit with the same key gets the first reply, and its own work is undone.
	tx = l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (2)", "tx":"`+tx+`"}`)
	if r := l.must("hub.sql.commit", `{"tx":"`+tx+`", "key":"k1", "fingerprint":"f1", "reply":{"id":2}}`); js(r) != `{"replayed":true,"reply":{"id":1}}` {
		t.Fatalf("second commit: %s", js(r))
	}
	if q := l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`); js(q["rows"]) != "[[1]]" {
		t.Fatalf("the replayed commit wrote: %v", q)
	}
}

func TestSQLBatchAllOrNothing(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (id INTEGER PRIMARY KEY)"}`)
	msg := l.fails("sql_error", "hub.sql.batch", `{"statements":[{"sql":"INSERT INTO t VALUES (1)"}, {"sql":"INSERT INTO t VALUES (1)"}]}`)
	if !strings.Contains(msg, "statements[2]") {
		t.Fatalf("the error names the statement: %s", msg)
	}
	if q := l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`); js(q["rows"]) != "[[0]]" {
		t.Fatalf("a failed batch wrote: %v", q)
	}
	r := l.must("hub.sql.batch", `{"statements":[{"sql":"INSERT INTO t VALUES (?)", "params":[1]}, {"sql":"INSERT INTO t VALUES (2) RETURNING id"}]}`)
	if js(r) != `{"results":[{"changes":1,"id":1},{"changes":1,"columns":["id"],"id":2,"rows":[[2]]}]}` {
		t.Fatalf("batch: %s", js(r))
	}
}

func TestSQLQueryPaging(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (s TEXT)"}`)
	l.must("hub.sql.exec", `{"sql":"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 3) INSERT INTO t SELECT printf('%.400000c', 'x') FROM n"}`)
	q := l.must("hub.sql.query", `{"sql":"SELECT s FROM t"}`)
	if q["complete"] != false || len(q["rows"].([]any)) != 2 {
		t.Fatalf("paging: complete %v, %d rows", q["complete"], len(q["rows"].([]any)))
	}
	l.fails("conflict", "hub.sql.query", `{"sql":"SELECT s FROM t", "expect_rows":3}`)
	l.fails("too_large", "hub.sql.exec", `{"sql":"UPDATE t SET s = s RETURNING s"}`)
}

// The RETURNING budget counts the complete encoded reply: the column names, the rows, and the exec
// and batch envelopes. A reply over it fails inside the statement's savepoint, before the commit;
// before, a long column alias let the statement commit and the reply guard refuse its reply.
func TestSQLReturningReplyBudget(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (x TEXT)"}`)
	count := func() string { return js(l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`)["rows"]) }
	big := `INSERT INTO t VALUES (printf('%.*c', 1035000, 'x')) RETURNING x AS "` + strings.Repeat("a", 16000) + `"`
	l.fails("too_large", "hub.sql.exec", js(map[string]any{"sql": big}))
	if c := count(); c != "[[0]]" {
		t.Fatalf("an oversized RETURNING reply committed its row: count %s", c)
	}
	// In a transaction the statement fails alone; the transaction continues.
	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.fails("too_large", "hub.sql.exec", js(map[string]any{"sql": big, "tx": tx}))
	l.must("hub.sql.exec", js(map[string]any{"sql": "INSERT INTO t VALUES ('y')", "tx": tx}))
	l.must("hub.sql.commit", js(map[string]any{"tx": tx}))
	if c := count(); c != "[[1]]" {
		t.Fatalf("the transaction after a refused statement: count %s", c)
	}
	// A batch counts each result and its wrapper; it fails all or nothing.
	l.fails("too_large", "hub.sql.batch", js(map[string]any{"statements": []any{
		map[string]any{"sql": "INSERT INTO t VALUES ('z')"}, map[string]any{"sql": big}}}))
	if c := count(); c != "[[1]]" {
		t.Fatalf("an oversized batch reply committed: count %s", c)
	}
	// A reply that the budget admits passes the reply guard at the server's 1 MiB message limit.
	for _, q := range []string{
		`INSERT INTO t VALUES (printf('%.*c', 1035000, 'x')) RETURNING x`,
		`INSERT INTO t VALUES (printf('%.*c', 1015000, 'x')) RETURNING x AS "` + strings.Repeat("a", 16000) + `"`,
	} {
		out := l.must("hub.sql.exec", js(map[string]any{"sql": q}))
		if _, ok := guardReply("hub.sql.exec", map[string]any{"call_id": "c1", "result": out}, (1<<20)-replyMargin); !ok {
			t.Errorf("an admitted RETURNING reply exceeds the reply guard: %.80s", q)
		}
	}
}

// A query's row budget counts the column names too: with a long alias, the page ends earlier
// (complete false) and its reply passes the reply guard. Before, the rows filled the budget and
// the reply guard refused the reply. hub.sql.test reads the same way.
func TestSQLQueryBudgetCountsColumns(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (id INTEGER PRIMARY KEY, s TEXT)"}`)
	l.must("hub.sql.exec", `{"sql":"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 3) INSERT INTO t SELECT i, printf('%.345000c', 'x') FROM n"}`)
	alias := `"` + strings.Repeat("a", 16000) + `"`
	page := func(after int) map[string]any {
		q := l.must("hub.sql.query", js(map[string]any{"sql": "SELECT id, s AS " + alias + " FROM t WHERE id > ? ORDER BY id", "params": []any{after}}))
		if _, ok := guardReply("hub.sql.query", map[string]any{"call_id": "c1", "result": q}, (1<<20)-replyMargin); !ok {
			t.Fatalf("a query page after id %d exceeds the reply guard", after)
		}
		return q
	}
	first := page(0)
	if first["complete"] != false || len(first["rows"].([]any)) != 2 {
		t.Fatalf("first page: complete %v, %d rows", first["complete"], len(first["rows"].([]any)))
	}
	if rest := page(2); rest["complete"] != true || len(rest["rows"].([]any)) != 1 {
		t.Fatalf("second page: complete %v, %d rows", rest["complete"], len(rest["rows"].([]any)))
	}
	out, err := l.s.test(context.Background(), "p", map[string]any{"sql": "SELECT id, s AS " + alias + " FROM t ORDER BY id"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := guardReply("hub.sql.test", map[string]any{"call_id": "c1", "result": out}, (1<<20)-replyMargin); !ok {
		t.Error("a hub.sql.test read exceeds the reply guard")
	}
}

// Notices of a commit go through the outbox: a rollback has none; a failed apply keeps the rows,
// and the retry applies them; the outbox is empty after an apply.
func TestSQLNoticesOutbox(t *testing.T) {
	l := newSQLLab(t)
	var mu sync.Mutex
	var applied [][]outboxRow
	failing := true
	l.s.admitNotices = func(plugin string, notes []noticeOp) error {
		for _, n := range notes {
			if n.agent == "ghost" {
				return refuse("invalid_input", "unknown agent")
			}
		}
		return nil
	}
	l.s.applyNotices = func(plugin string, rows []outboxRow) error {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			failing = false
			return errors.New("hub.db is busy")
		}
		applied = append(applied, rows)
		return nil
	}
	outbox := func() int {
		p, _ := l.s.open(context.Background(), "p")
		p.lock <- struct{}{}
		defer p.release()
		n, _ := p.w.hubInt(context.Background(), "SELECT count(*) FROM lazyrex_notice_outbox")
		return int(n)
	}
	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.fails("invalid_input", "hub.sql.commit", `{"tx":"`+tx+`", "notices":[{"agent":"a", "pending":true}, {"agent":"a", "pending":false}]}`)
	l.fails("invalid_input", "hub.sql.commit", `{"tx":"`+tx+`", "notices":[{"agent":"ghost", "pending":true}]}`)
	l.must("hub.sql.rollback", `{"tx":"`+tx+`"}`)
	if outbox() != 0 {
		t.Fatal("a refused commit wrote notices")
	}
	// The first apply fails: the commit stands, and the rows wait for the retry.
	tx = l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.commit", `{"tx":"`+tx+`", "notices":[{"agent":"a", "pending":true}, {"agent":"b", "pending":false}]}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(applied)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the retry did not apply the notices")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := fmt.Sprint(applied[0]); got != "[{1 a true} {2 b false}]" {
		t.Fatalf("applied %s", got)
	}
	for time.Now().Before(deadline) && outbox() != 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if outbox() != 0 {
		t.Fatal("the outbox kept applied rows")
	}
	// A later commit gets a new seq: AUTOINCREMENT never gives a seq again.
	tx = l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.commit", `{"tx":"`+tx+`", "notices":[{"agent":"a", "pending":false}]}`)
	if got := fmt.Sprint(applied[1]); got != "[{3 a false}]" {
		t.Fatalf("applied %s", got)
	}
}

// A retry that applies the outbox wakes the notice loop. The commit's own wake comes after its
// failed apply, when no notice exists yet; without a wake after the retry, the applied notice waits.
func TestSQLNoticesRetryWakes(t *testing.T) {
	l := newSQLLab(t)
	var calls atomic.Int32
	applied := make(chan struct{}, 1)
	wakes := make(chan struct{}, 4)
	l.s.admitNotices = func(string, []noticeOp) error { return nil }
	l.s.applyNotices = func(string, []outboxRow) error {
		if calls.Add(1) == 1 {
			return errors.New("hub.db is busy")
		}
		applied <- struct{}{}
		return nil
	}
	l.s.notified = func() { wakes <- struct{}{} }
	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.commit", `{"tx":"`+tx+`", "notices":[{"agent":"a", "pending":true}]}`)
	<-wakes // the commit's wake, after its failed apply
	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("the retry did not apply the notices")
	}
	select {
	case <-wakes:
	case <-time.After(2 * time.Second):
		t.Fatal("the retry applied the notices and did not wake the notice loop")
	}
}

// applySQLNotices skips a row with a receipt: a crash after the apply and before the outbox
// delete applies nothing twice. A plugin whose notices moved cannot send store notices.
func TestSQLNoticesReceipts(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	rows := []outboxRow{{1, "a", true}, {2, "b", true}}
	if err := h.db.applySQLNotices(ctx, "p", rows); err != nil {
		t.Fatal(err)
	}
	rev := func(agent string) int64 {
		var r int64
		h.db.QueryRow(`SELECT intent_rev FROM notices WHERE ns = 'p' AND agent = ?`, agent).Scan(&r)
		return r
	}
	ra, rb := rev("a"), rev("b")
	if ra == 0 || rb == 0 {
		t.Fatal("no notices")
	}
	if err := h.db.applySQLNotices(ctx, "p", append(rows, outboxRow{3, "a", true})); err != nil {
		t.Fatal(err)
	}
	if rev("b") != rb || rev("a") == ra {
		t.Fatalf("receipts: b %d -> %d (want the same), a %d -> %d (want a new one)", rb, rev("b"), ra, rev("a"))
	}
	h.db.dropNoticeReceipts(ctx, "p", 3)
	var n int
	h.db.QueryRow(`SELECT count(*) FROM notice_receipts WHERE plugin = 'p'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d receipts left", n)
	}
	if moved, _ := h.db.noticesMoved(ctx, "p"); moved {
		t.Fatal("moved before a move")
	}
	h.db.moveNotices(ctx, "p")
	if moved, _ := h.db.noticesMoved(ctx, "p"); !moved {
		t.Fatal("not moved")
	}
	if !hasNotify(map[string]any{"ops": []any{map[string]any{"key": "k"}, map[string]any{"notify": map[string]any{"agent": "a", "pending": true}}}}) {
		t.Fatal("hasNotify")
	}
}
