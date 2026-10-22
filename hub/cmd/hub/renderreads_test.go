package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// The marks of a SQL commit: a view that read a marked table, a view without a table set, a view
// in flight, and every view for a schema change. A view whose tables the commit did not change
// is not marked.
func TestMarkSQL(t *testing.T) {
	h := depsHub()
	h.vs.mu.Lock()
	a := h.vs.stateLocked("p", "a")
	a.tables = map[string]bool{"a": true}
	b := h.vs.stateLocked("p", "b")
	b.tables = map[string]bool{"b": true, "c": true}
	broad := h.vs.stateLocked("p", "broad")
	flight := h.vs.stateLocked("p", "flight")
	flight.tables, flight.inFlight = map[string]bool{"z": true}, true
	other := h.vs.stateLocked("q", "other")
	other.tables = map[string]bool{"a": true}
	h.vs.mu.Unlock()
	reset := func() { a.dirty, b.dirty, broad.dirty, flight.dirty, other.dirty = 0, 0, 0, 0, 0 }
	marked := func(st *viewState) bool { return st.dirty&causeCommit != 0 }

	h.markSQL("p", []string{"a"})
	if !marked(a) || marked(b) || !marked(broad) || !marked(flight) || marked(other) {
		t.Errorf("commit to a: a %v, b %v, broad %v, flight %v, other plugin %v", a.dirty, b.dirty, broad.dirty, flight.dirty, other.dirty)
	}
	reset()
	h.markSQL("p", []string{"x", "c"})
	if marked(a) || !marked(b) || !marked(broad) {
		t.Errorf("commit to x and c: a %v, b %v, broad %v", a.dirty, b.dirty, broad.dirty)
	}
	reset()
	h.markSQL("p", []string{"*"})
	if !marked(a) || !marked(b) || !marked(broad) || marked(other) {
		t.Errorf("a schema change: a %v, b %v, broad %v, other plugin %v", a.dirty, b.dirty, broad.dirty, other.dirty)
	}
	if h.vs.loose["p"]&causeCommit == 0 || h.vs.all["p"] != 0 {
		t.Errorf("a view without state gets the cause through loose, and no cause is for every view: loose %v, all %v", h.vs.loose["p"], h.vs.all["p"])
	}
}

// The reads of an open render, and the untracked rule.
func TestRenderReadRules(t *testing.T) {
	h := depsHub()
	open := func() (string, *openRender) {
		id := h.openRenderID("p", "c1", "v", 0)
		return id, h.vs.renders[id]
	}
	query := func(client, id string) { h.renderCall("p", client, "hub.sql.query", map[string]any{"render": id}) }

	// A tokened read adds its tables; the schema stays tracked.
	id, r := open()
	query("c1", id)
	h.renderRead("p", "c1", id, map[string]bool{"t": true, "sqlite_master": true}, true)
	if r.untracked || !r.tables["t"] || !r.tables["sqlite_master"] {
		t.Errorf("a tokened read: %+v", r)
	}
	h.closeRenderID(id)

	for name, step := range map[string]func(id string){
		"untokened query":  func(string) { h.renderCall("p", "c1", "hub.sql.query", map[string]any{}) },
		"unknown ID":       func(string) { query("c1", "r-unknown") },
		"another instance": func(id string) { query("c2", id) },
		"hub.store.get":    func(string) { h.renderCall("p", "c1", "hub.store.get", map[string]any{"keys": []any{"k"}}) },
		"hub.agents":       func(string) { h.renderCall("p", "c1", "hub.agents", map[string]any{}) },
		"a failed query":   func(id string) { h.renderRead("p", "c1", id, nil, false) },
		"sqlite_sequence": func(id string) {
			h.renderRead("p", "c1", id, map[string]bool{"t": true, "sqlite_sequence": true}, true)
		},
		"closed before reading": func(string) { h.renderRead("p", "c1", "r-closed", map[string]bool{"t": true}, true) },
	} {
		id, r := open()
		step(id)
		if !r.untracked {
			t.Errorf("%s: the render stays tracked", name)
		}
		h.closeRenderID(id)
	}

	// Another plugin's ID counts as unknown: the caller's renders become untracked, the other
	// plugin's render does not.
	id, r = open()
	qid := h.openRenderID("q", "cq", "w", 0)
	h.renderCall("p", "c1", "hub.sql.query", map[string]any{"render": qid})
	if !r.untracked || h.vs.renders[qid].untracked {
		t.Errorf("a foreign ID: caller %v, owner %v", r.untracked, h.vs.renders[qid].untracked)
	}
	h.closeRenderID(id)
	h.closeRenderID(qid)

	// A call with no open render changes nothing; a closed ID used during the next render makes
	// that render untracked.
	h.renderCall("p", "c1", "hub.store.get", map[string]any{})
	old, _ := open()
	h.closeRenderID(old)
	_, r = open()
	query("c1", old)
	if !r.untracked {
		t.Error("a closed ID during the next render: tracked")
	}
}

// renderHub has two open, attached views v1 and v2 of plugin p (instance c1), with snapshots
// that equal the test tree, so a render sends no view.changed.
func renderHub(t *testing.T) *Hub {
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
	h.vs.snaps = map[string]*snapshot{}
	for _, v := range []string{"v1", "v2"} {
		h.vs.snaps[v] = &snapshot{tree: []byte(`{"back":null,"nodes":[],"title":"T"}`)}
	}
	h.plugins = map[string]*Plugin{"p": {ClientID: "c1"}}
	return h
}

// renderStep is what the test plugin does in one render of a view: the reads (each a set of
// tables; nil: an untokened read), the refresh value of the reply, and an error.
type renderStep struct {
	reads    []map[string]bool
	failRead bool // after the reads, a tokened query that fails before it runs
	refresh  any  // nil: no field
	err      error
	during   func() // runs after the reads, before the reply
}

// renderWith renders the marked views of p once; steps gives each view's render.
func renderWith(t *testing.T, h *Hub, steps map[string]renderStep) {
	t.Helper()
	viewRenderHook = func(h *Hub, ctx context.Context, plugin string, args map[string]any, out any) error {
		id, view := args["render"].(string), args["view_id"].(string)
		s := steps[view]
		for _, reads := range s.reads {
			if reads == nil {
				h.renderCall(plugin, "c1", "hub.sql.query", map[string]any{})
				continue
			}
			h.renderCall(plugin, "c1", "hub.sql.query", map[string]any{"render": id})
			h.renderRead(plugin, "c1", id, reads, true)
		}
		if s.failRead {
			h.renderCall(plugin, "c1", "hub.sql.query", map[string]any{"render": id})
			h.renderRead(plugin, "c1", id, nil, false)
		}
		if s.during != nil {
			s.during()
		}
		if s.err != nil {
			return s.err
		}
		reply := map[string]any{"tree": map[string]any{"title": "T", "back": nil, "nodes": []any{}}}
		if s.refresh != nil {
			reply["refresh"] = s.refresh
		}
		b, _ := json.Marshal(reply)
		return json.Unmarshal(b, out)
	}
	t.Cleanup(func() { viewRenderHook = nil })
	h.renderViews(context.Background(), "p")
}

func tracked(h *Hub, view string) bool {
	h.vs.mu.Lock()
	defer h.vs.mu.Unlock()
	st := h.vs.views["p"][view]
	return st != nil && st.tables != nil
}

func markAll(h *Hub) {
	h.markView("p", "v1", causeTest)
	h.markView("p", "v2", causeTest)
}

// The default: a render installs its tables when its reply has no refresh field (or null), it
// succeeded, its reads stayed tracked and it read at least one table. "all" is the opt-out
// (TestRenderRefreshRefused: any other value is an error). One view's setting does not change
// another view, and a later "all" render clears the set.
func TestRenderDefault(t *testing.T) {
	ta := map[string]bool{"a": true}
	null := json.RawMessage("null")
	for name, c := range map[string]struct {
		step renderStep
		want bool
	}{
		"no refresh":  {renderStep{reads: []map[string]bool{ta}}, true},
		"null":        {renderStep{reads: []map[string]bool{ta}, refresh: null}, true},
		"all":         {renderStep{reads: []map[string]bool{ta}, refresh: "all"}, false},
		"empty set":   {renderStep{}, false},
		"untracked":   {renderStep{reads: []map[string]bool{ta, nil}}, false},
		"failed read": {renderStep{reads: []map[string]bool{ta}, failRead: true}, false},
		"failed":      {renderStep{reads: []map[string]bool{ta}, err: &rex.RexError{Code: "boom", Message: "test"}}, false},
		"transient":   {renderStep{reads: []map[string]bool{ta}, err: &rex.RexError{Code: "timeout", Message: "test"}}, false},
	} {
		h := renderHub(t)
		if c.step.err != nil { // the Error tree is the snapshot: no view.changed
			h.vs.snaps["v1"].tree = []byte(`{"back":null,"nodes":[{"id":"$error","retry_action":null,"text":"The plugin could not render this view.","type":"error"}],"title":"Error"}`)
		}
		markAll(h)
		renderWith(t, h, map[string]renderStep{"v1": c.step, "v2": {reads: []map[string]bool{ta}, refresh: "all"}})
		if got := tracked(h, "v1"); got != c.want {
			t.Errorf("%s: v1 tracked %v", name, got)
		}
		if tracked(h, "v2") {
			t.Errorf("%s: v2 sent all, but has a table set", name)
		}
	}

	// A tracked view, then an "all" render: the filter is gone.
	h := renderHub(t)
	markAll(h)
	renderWith(t, h, map[string]renderStep{"v1": {reads: []map[string]bool{ta}}})
	h.markSQL("p", []string{"b"})
	if st := h.vs.views["p"]["v1"]; st.dirty != 0 {
		t.Fatalf("a tracked view that read a: a commit to b marks it: %v", st.dirty)
	}
	h.markSQL("p", []string{"a"})
	if h.vs.views["p"]["v1"].dirty&causeCommit == 0 {
		t.Fatal("a tracked view that read a: a commit to a does not mark it")
	}
	renderWith(t, h, map[string]renderStep{"v1": {reads: []map[string]bool{ta}, refresh: "all"}})
	if tracked(h, "v1") {
		t.Fatal("an all render keeps the old filter")
	}
	h.markSQL("p", []string{"b"})
	if h.vs.views["p"]["v1"].dirty&causeCommit == 0 {
		t.Fatal("after an all render, a commit to b does not mark the view")
	}

}

// A refresh value other than "all" (or no field) is a declaration error, like malformed deps:
// the tree is shown, the view has no table set (so every SQL commit renders it again), and
// view_deps_invalid comes once for each new error text. The value is checked before the
// untracked and empty-read cases. The next valid render clears the error and tracks again.
func TestRenderRefreshRefused(t *testing.T) {
	ta := map[string]bool{"a": true}
	h := renderHub(t)
	events := func() int {
		b, _ := os.ReadFile(h.ev.f.Name())
		return strings.Count(string(b), `"view_deps_invalid"`)
	}
	depsErr := func() string {
		h.vs.mu.Lock()
		defer h.vs.mu.Unlock()
		return h.vs.views["p"]["v1"].depsErr
	}
	markAll(h)
	renderWith(t, h, map[string]renderStep{"v1": {reads: []map[string]bool{ta}}})
	if !tracked(h, "v1") {
		t.Fatal("a valid render is not tracked")
	}
	for i, c := range []struct {
		step   renderStep
		want   string
		events int
	}{
		{renderStep{reads: []map[string]bool{ta}, refresh: "reads"}, `refresh "reads": only "all" or no field is valid`, 1},
		{renderStep{reads: []map[string]bool{ta}, refresh: "reads"}, `refresh "reads": only "all" or no field is valid`, 1},        // the same text: no second event
		{renderStep{refresh: 5}, `refresh 5: only "all" or no field is valid`, 2},                                                  // no table read
		{renderStep{reads: []map[string]bool{ta, nil}, refresh: "writes"}, `refresh "writes": only "all" or no field is valid`, 3}, // untracked
	} {
		markAll(h)
		renderWith(t, h, map[string]renderStep{"v1": c.step})
		if tracked(h, "v1") {
			t.Errorf("case %d: a refused refresh value left a table set", i)
		}
		if got := depsErr(); got != c.want {
			t.Errorf("case %d: error %q, want %q", i, got, c.want)
		}
		if n := events(); n != c.events {
			t.Errorf("case %d: %d view_deps_invalid events, want %d", i, n, c.events)
		}
	}
	h.markSQL("p", []string{"b"})
	if h.vs.views["p"]["v1"].dirty&causeCommit == 0 {
		t.Error("a view with a refused refresh value is not marked by an unrelated commit")
	}
	renderWith(t, h, map[string]renderStep{"v1": {reads: []map[string]bool{ta}}})
	if !tracked(h, "v1") || depsErr() != "" {
		t.Errorf("after a valid render: tracked %v, error %q", tracked(h, "v1"), depsErr())
	}
}

// Read tracking filters only the SQL-commit trigger: a tracked view still renders on a
// deps.changes name that it declared, and on an invalidate.
func TestRenderTrackedOtherTriggers(t *testing.T) {
	ta := map[string]bool{"a": true}
	h := renderHub(t)
	markAll(h)
	renderWith(t, h, map[string]renderStep{"v1": {reads: []map[string]bool{ta}}})
	if !tracked(h, "v1") {
		t.Fatal("v1 read a without refresh: no table set")
	}
	h.vs.mu.Lock()
	st := h.vs.views["p"]["v1"]
	st.deps = &viewDeps{prefixes: []string{}, changes: []string{"workers"}}
	h.vs.mu.Unlock()
	dirty := func() renderCause {
		h.vs.mu.Lock()
		defer h.vs.mu.Unlock()
		d := st.dirty
		st.dirty = 0
		return d
	}
	dirty()
	h.markSQL("p", []string{"b"})
	if d := dirty(); d != 0 {
		t.Fatalf("a commit to b marks the tracked view: %v", d)
	}
	h.markChanged("p", "workers")
	if d := dirty(); d&causeChanged == 0 {
		t.Fatalf("changed workers does not mark the tracked view: %v", d)
	}
	h.markView("p", "v1", causeInvalidate)
	if d := dirty(); d&causeInvalidate == 0 {
		t.Fatalf("an invalidate does not mark the tracked view: %v", d)
	}
}

// A commit while a render is open marks the view, also when its table is not in the set that
// the render then installs: the view renders again after it. A new registration during the
// render installs nothing.
func TestRenderTrackedRaces(t *testing.T) {
	ta := map[string]bool{"a": true}
	h := renderHub(t)
	markAll(h)
	renderWith(t, h, map[string]renderStep{"v1": {reads: []map[string]bool{ta}}})
	if !tracked(h, "v1") {
		t.Fatal("v1 read a: no table set")
	}
	// The second render has the set {a} before it starts; the commit to b lands during it.
	h.markView("p", "v1", causeTest)
	renderWith(t, h, map[string]renderStep{
		"v1": {reads: []map[string]bool{ta}, during: func() { h.markSQL("p", []string{"b"}) }},
	})
	h.vs.mu.Lock()
	st := h.vs.views["p"]["v1"]
	dirty := st.dirty
	h.vs.mu.Unlock()
	if dirty&causeCommit == 0 {
		t.Fatal("a commit to b during the render of v1 is lost")
	}
	if !tracked(h, "v1") {
		t.Fatal("v1 read a: no table set")
	}

	h = renderHub(t)
	markAll(h)
	renderWith(t, h, map[string]renderStep{
		"v1": {reads: []map[string]bool{ta}, during: func() { h.clearDeclarations("p") }},
	})
	if tracked(h, "v1") {
		t.Fatal("a render from before a new registration installed its tables")
	}
	h.vs.mu.Lock()
	h.vs.stateLocked("p", "v2").tables = ta
	h.vs.mu.Unlock()
	h.clearDeclarations("p")
	if tracked(h, "v2") {
		t.Fatal("a new registration keeps a table set")
	}
	h.forgetView("v2")
	if _, ok := h.vs.views["p"]["v2"]; ok {
		t.Fatal("a closed view keeps its state")
	}
}

// hub.sql.query reports the tables of a query with a render ID: through a SQL view, a join, a
// count(*) and a subquery; also for a conflict (the statement ran fully). A failed statement
// reports ok false. A query without render reports nothing.
func TestSQLRenderReads(t *testing.T) {
	l := newSQLLab(t)
	type report struct {
		render string
		reads  string
		ok     bool
	}
	var got []report
	l.s.rendered = func(plugin, client, render string, reads map[string]bool, ok bool) {
		var names []string
		for n := range reads {
			names = append(names, n)
		}
		sort.Strings(names)
		got = append(got, report{render, strings.Join(names, ","), ok})
	}
	for _, q := range []string{"CREATE TABLE a (x)", "CREATE TABLE b (y)", "CREATE VIEW va AS SELECT x FROM a"} {
		l.must("hub.sql.exec", `{"sql":"`+q+`"}`)
	}
	l.must("hub.sql.query", `{"sql":"SELECT x FROM va", "render":"r-1"}`)
	l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM a JOIN b ON 1", "render":"r-1"}`)
	l.must("hub.sql.query", `{"sql":"SELECT EXISTS (SELECT 1 FROM b)", "render":"r-1"}`)
	l.fails("conflict", "hub.sql.query", `{"sql":"SELECT x FROM a", "expect_rows":1, "render":"r-1"}`)
	l.fails("sql_error", "hub.sql.query", `{"sql":"SELECT nope FROM a", "render":"r-1"}`)
	l.must("hub.sql.query", `{"sql":"SELECT x FROM a"}`)
	// Errors before the statement runs (codex-29 review of 891aae8): each one reports a failed read.
	l.fails("invalid_input", "hub.sql.query", `{"sql":"SELECT x FROM b", "expect_rows":-1, "render":"r-1"}`)
	l.fails("invalid_input", "hub.sql.query", `{"render":"r-1"}`)
	l.fails("invalid_input", "hub.sql.query", `{"sql":"SELECT x FROM b", "params":5, "render":"r-1"}`)
	l.fails("refused", "hub.sql.query", `{"sql":"SELECT x FROM b", "tx":"x", "render":"r-1"}`)
	if _, err := l.call("c-old", "hub.sql.query", `{"sql":"SELECT x FROM b", "render":"r-1"}`); err == nil {
		t.Fatal("a stale instance read")
	}
	want := []report{{"r-1", "a,va", true}, {"r-1", "a,b", true}, {"r-1", "b", true}, {"r-1", "a", true}, {"r-1", "", false},
		{"r-1", "", false}, {"r-1", "", false}, {"r-1", "", false}, {"r-1", "", false}, {"r-1", "", false}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("reports:\n got %v\nwant %v", got, want)
	}
}
