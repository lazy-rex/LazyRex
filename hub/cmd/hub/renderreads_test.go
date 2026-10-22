package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// Each view.render call gets a new render ID in args.render. The ID is open while the call runs,
// names the plugin, its instance and the view, and is closed when the call returns, also after a
// failure. No ID is used twice.
func TestRenderIDLifetime(t *testing.T) {
	h := testUIHub(t)
	for _, m := range []string{pairSchema, `ALTER TABLE views ADD COLUMN context TEXT NOT NULL DEFAULT ''`, `ALTER TABLE views ADD COLUMN layout TEXT NOT NULL DEFAULT ''`, `ALTER TABLE ui_tabs ADD COLUMN status TEXT NOT NULL DEFAULT ''`, `ALTER TABLE ui_tabs ADD COLUMN layout TEXT NOT NULL DEFAULT ''`} {
		if _, err := h.db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			t.Fatal(err)
		}
	}
	if _, err := h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, session_id, block_id, client_id, attachment) VALUES
		('v1', 'p', 'home', '/a', 'open', 's1', 'b1', 'renderer', 'att-1'),
		('v2', 'p', 'home', '/b', 'open', 's1', 'b2', 'renderer', 'att-2')`); err != nil {
		t.Fatal(err)
	}
	h.vs = depsHub().vs
	h.plugins = map[string]*Plugin{"p": {ClientID: "c1"}}

	seen := map[string]string{} // render ID -> view
	viewRenderHook = func(h *Hub, ctx context.Context, plugin string, args map[string]any, out any) error {
		id, _ := args["render"].(string)
		h.vs.mu.Lock()
		r := h.vs.renders[id]
		h.vs.mu.Unlock()
		view, _ := args["view_id"].(string)
		if r == nil || r.plugin != "p" || r.client != "c1" || r.viewID != view {
			t.Errorf("render %q of %s: open record %+v", id, view, r)
		}
		seen[id] = view
		return &rex.RexError{Code: "timeout", Message: "test: no answer"} // the ID closes on a failure too
	}
	defer func() { viewRenderHook = nil }()

	for pass := 0; pass < 2; pass++ {
		h.markView("p", "v1", causeTest)
		h.markView("p", "v2", causeTest)
		h.renderViews(context.Background(), "p")
	}
	if len(seen) != 4 {
		t.Fatalf("4 renders need 4 different IDs: %v", seen)
	}
	for id := range seen {
		if !strings.HasPrefix(id, "r-") || len(id) != 34 {
			t.Errorf("render ID %q: want r- and 128 bits in hex", id)
		}
	}
	if len(h.vs.renders) != 0 {
		t.Errorf("open renders after the calls returned: %v", h.vs.renders)
	}
}

// A render ID is a read-only token. Every hub.sql action except hub.sql.query refuses it, and so
// does a query in a transaction: one code and one text. The refusal comes before the database
// opens, and an open transaction stays usable. A null render is no render.
func TestSQLRenderArg(t *testing.T) {
	l := newSQLLab(t)
	file := filepath.Join(l.s.dir, "p.db")
	for _, c := range []struct{ action, args string }{
		{"hub.sql.exec", `{"sql":"CREATE TABLE t (x)", "render":"r-1"}`},
		{"hub.sql.exec", `{"sql":"CREATE TABLE t (x)", "render":5}`}, // presence decides, not the type
		{"hub.sql.begin", `{"render":"r-1"}`},
		{"hub.sql.batch", `{"statements":[{"sql":"SELECT 1"}], "render":"r-1"}`},
		{"hub.sql.commit", `{"tx":"x", "render":"r-1"}`},
		{"hub.sql.rollback", `{"tx":"x", "render":"r-1"}`},
		{"hub.sql.replay", `{"key":"k", "render":"r-1"}`},
		{"hub.sql.query", `{"sql":"SELECT 1", "tx":"x", "render":"r-1"}`},
	} {
		if msg := l.fails("refused", c.action, c.args); msg != "refused: a render cannot write" {
			t.Errorf("%s %s: %q", c.action, c.args, msg)
		}
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("a refused call opened the database: %v", err)
	}
	l.fails("invalid_input", "hub.sql.query", `{"sql":"SELECT 1", "render":5}`)
	l.fails("invalid_input", "hub.sql.query", `{"sql":"SELECT 1", "render":""}`)
	if q := l.must("hub.sql.query", `{"sql":"SELECT 1", "render":"r-unknown"}`); js(q["rows"]) != "[[1]]" {
		t.Fatalf("a query with a render ID: %v", q)
	}
	l.must("hub.sql.query", `{"sql":"SELECT 1", "render":null}`)

	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (x)"}`)
	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (1)", "tx":"`+tx+`"}`)
	l.fails("refused", "hub.sql.exec", `{"sql":"INSERT INTO t VALUES (2)", "tx":"`+tx+`", "render":"r-1"}`)
	l.fails("refused", "hub.sql.commit", `{"tx":"`+tx+`", "render":"r-1"}`)
	l.must("hub.sql.commit", `{"tx":"`+tx+`"}`)
	if q := l.must("hub.sql.query", `{"sql":"SELECT x FROM t"}`); js(q["rows"]) != "[[1]]" {
		t.Fatalf("the transaction after the refusals: %v", q)
	}
}
