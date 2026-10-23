package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// sqlReturnHub has a plugin p (client c1, notice set) with the SQL service wired as in start.
// applied counts the outbox rows that reached hub.db.
type sqlReturnHub struct {
	*Hub
	pl      *Plugin
	mu      sync.Mutex
	applied int
}

func newSQLReturnHub(t *testing.T) *sqlReturnHub {
	h, _ := workerTestHub(t)
	h.cfg.State = t.TempDir()
	h.vs = newViewSvc()
	h.faults = &faults{}
	h.idents["a"] = &Agent{ID: "a", delivered: map[string]int64{}}
	pl := &Plugin{Name: "p", ClientID: "c1", Desired: "running", run: pluginRun{Notice: "n"}}
	h.plugins = map[string]*Plugin{"p": pl}
	r := &sqlReturnHub{Hub: h, pl: pl}
	r.restartSQL()
	t.Cleanup(func() { r.sql.close() })
	return r
}

// restartSQL makes a new SQL service, as a hub start does after a crash, and runs its start drain.
func (r *sqlReturnHub) restartSQL() {
	if r.sql != nil {
		r.sql.close()
	}
	r.sql = newSQLService(r.cfg.State, r.currentClient)
	r.sql.admitNotices = func(plugin string, _ []noticeOp) error { return r.db.moveNotices(context.Background(), plugin) }
	r.sql.applyNotices = func(plugin string, rows []outboxRow) error {
		r.mu.Lock()
		r.applied += len(rows)
		r.mu.Unlock()
		return r.db.applySQLNotices(context.Background(), plugin, rows)
	}
	r.sql.dropReceipts = func(plugin string, seq int64) error {
		return r.db.dropNoticeReceipts(context.Background(), plugin, seq)
	}
	r.sql.returning = r.db.sqlReturning
	r.sql.drainAll(context.Background())
}

func (r *sqlReturnHub) sqlCall(t *testing.T, action, args string) (map[string]any, error) {
	var a map[string]any
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		t.Fatalf("bad test args %s", args)
	}
	out, err := r.sql.dispatch(context.Background(), action, sqlCall{plugin: "p", client: "c1", args: a})
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(out)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m, nil
}

func (r *sqlReturnHub) mustSQL(t *testing.T, action, args string) map[string]any {
	t.Helper()
	m, err := r.sqlCall(t, action, args)
	if err != nil {
		t.Fatalf("%s %s: %v", action, args, err)
	}
	return m
}

// cutover makes the plugin's SQL state: a table with a row, a committed notice (the notices
// moved), and one outbox row that has not reached hub.db yet.
func (r *sqlReturnHub) cutover(t *testing.T) {
	r.mustSQL(t, "hub.sql.exec", `{"sql":"CREATE TABLE IF NOT EXISTS t (a)"}`)
	r.mustSQL(t, "hub.sql.exec", `{"sql":"INSERT INTO t VALUES ('done')"}`)
	tx := r.mustSQL(t, "hub.sql.begin", `{}`)["tx"].(string)
	r.mustSQL(t, "hub.sql.commit", `{"tx":"`+tx+`", "notices":[{"agent":"a", "pending":true}]}`)
	p, _ := r.sql.open(context.Background(), "p")
	p.lock <- struct{}{}
	err := p.w.hub(context.Background(), `INSERT INTO lazyrex_notice_outbox (agent, pending) VALUES ('a', 0)`)
	p.release()
	if err != nil {
		t.Fatal(err)
	}
}

func (r *sqlReturnHub) stop(t *testing.T) {
	r.mu.Lock()
	r.pl.ClientID, r.pl.Desired = "", "stopped"
	r.mu.Unlock()
	if _, err := r.db.Exec(`INSERT OR REPLACE INTO plugins (name, desired) VALUES ('p', 'stopped')`); err != nil {
		t.Fatal(err)
	}
}

func (r *sqlReturnHub) sqlReturn() (map[string]any, error) {
	out, err := r.adminSQLReturn(context.Background(), map[string]any{"plugin": "p"})
	m, _ := out.(map[string]any)
	return m, err
}

// hub.admin.sql_return refuses a running plugin. A crash after each step leaves the marker: a
// restarted hub does not drain the outbox or open the file, and the call again completes. After
// the return, store notices work, the outbox rows reached hub.db once, and a later forward starts
// with a new, empty file.
func TestSQLReturn(t *testing.T) {
	for _, crash := range []string{"marked", "drained", "closed", "moved", ""} {
		t.Run("crash after "+crash, func(t *testing.T) {
			r := newSQLReturnHub(t)
			r.cutover(t)
			if _, err := r.sqlReturn(); codeOf(err) != "refused" {
				t.Fatalf("a running plugin: %v", err)
			}
			r.stop(t)
			if crash != "" {
				sqlReturnHook = func(step string) error {
					if step == crash {
						return errors.New("crash")
					}
					return nil
				}
				_, err := r.sqlReturn()
				sqlReturnHook = nil
				if err == nil || err.Error() != "crash" {
					t.Fatalf("the crash: %v", err)
				}
				r.restartSQL()
				if _, err := r.sqlCall(t, "hub.sql.query", `{"sql":"SELECT a FROM t"}`); codeOf(err) != "unavailable" {
					t.Fatalf("a call during the return: %v", err)
				}
			}
			out, err := r.sqlReturn()
			if err != nil {
				t.Fatalf("the return: %v", err)
			}
			dest, _ := out["returned"].(string)
			if !strings.HasPrefix(dest, filepath.Join(r.cfg.State, "sql-returned", "p-")) {
				t.Fatalf("returned %q", dest)
			}
			if _, err := os.Stat(filepath.Join(dest, "p.db")); err != nil {
				t.Fatalf("the moved file: %v", err)
			}
			left, _ := filepath.Glob(filepath.Join(r.cfg.State, "sql", "p.db*"))
			if len(left) != 0 {
				t.Fatalf("files left in the SQL directory: %v", left)
			}
			if r.applied != 2 {
				t.Fatalf("%d outbox rows reached hub.db, want 2", r.applied)
			}
			if moved, _ := r.db.noticesMoved(context.Background(), "p"); moved {
				t.Fatal("the notices are still moved")
			}
			if r.db.sqlReturning("p") {
				t.Fatal("the marker stays")
			}
			// A second call: nothing to move, nothing fails.
			if out, err := r.sqlReturn(); err != nil || out["returned"] != "" {
				t.Fatalf("a second return: %v %v", out, err)
			}
			// The store version: notify ops work again.
			r.mu.Lock()
			r.pl.ClientID, r.pl.Desired, r.pl.run = "c1", "running", pluginRun{Notice: "n"}
			r.mu.Unlock()
			p := perform{Action: "hub.store.batch", Args: json.RawMessage(`{"ops":[{"key":"k","value":1},{"notify":{"agent":"a","pending":true}}]}`)}
			p.Requester.ClientID = "c1"
			if _, err := r.dispatch(context.Background(), p); err != nil {
				t.Fatalf("a store notice after the return: %v", err)
			}
			// A later forward: a new file without the old table, and the notices move again.
			if _, err := r.sqlCall(t, "hub.sql.query", `{"sql":"SELECT a FROM t"}`); err == nil {
				t.Fatal("the forward file has the old table")
			}
			r.cutover(t)
			if moved, _ := r.db.noticesMoved(context.Background(), "p"); !moved {
				t.Fatal("the forward did not move the notices")
			}
		})
	}
}
