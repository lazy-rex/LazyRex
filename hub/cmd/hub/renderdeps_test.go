package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// depsHub has worker channels but no workers: a mark wakes nothing.
func depsHub() *Hub {
	return &Hub{vs: &viewSvc{workers: map[string]chan struct{}{"p": make(chan struct{}, 1), "q": make(chan struct{}, 1)}}}
}

// The marks of commits and changes: a matching prefix, no prefixes, an explicit empty list, a view
// in flight; a change name in the list, missing, empty, and a name that no list can hold.
func TestRenderDepsMarks(t *testing.T) {
	h := depsHub()
	h.vs.mu.Lock()
	match := h.vs.stateLocked("p", "match")
	match.deps = &viewDeps{prefixes: []string{"thread/a"}, changes: []string{"workers"}}
	none := h.vs.stateLocked("p", "none")
	empty := h.vs.stateLocked("p", "empty")
	empty.deps = &viewDeps{prefixes: []string{}, changes: []string{}}
	flight := h.vs.stateLocked("p", "flight")
	flight.deps, flight.inFlight = &viewDeps{prefixes: []string{}, changes: []string{}}, true
	h.vs.mu.Unlock()

	h.markCommit("p", []string{"thread/a/1"})
	h.markCommit("q", []string{"x"}) // another plugin
	for name, c := range map[string]struct {
		st   *viewState
		want bool
	}{"match": {match, true}, "none": {none, true}, "empty": {empty, false}, "flight": {flight, true}} {
		if got := c.st.dirty&causeCommit != 0; got != c.want {
			t.Errorf("commit, %s: marked %v", name, got)
		}
	}
	match.dirty, none.dirty, empty.dirty, flight.dirty = 0, 0, 0, 0
	h.markCommit("p", []string{"thread/b"})
	if match.dirty != 0 || empty.dirty != 0 || none.dirty == 0 {
		t.Errorf("an unrelated commit: match %v, empty %v, none %v", match.dirty, empty.dirty, none.dirty)
	}
	match.dirty, none.dirty, empty.dirty, flight.dirty = 0, 0, 0, 0
	h.markChanged("p", "workers")
	if match.dirty == 0 || none.dirty == 0 || empty.dirty != 0 || flight.dirty == 0 {
		t.Errorf("changed workers: match %v, none %v, empty %v, flight %v", match.dirty, none.dirty, empty.dirty, flight.dirty)
	}
	match.dirty, none.dirty, empty.dirty, flight.dirty = 0, 0, 0, 0
	h.markChanged("p", "agents")
	if match.dirty != 0 || empty.dirty != 0 || none.dirty == 0 {
		t.Errorf("changed agents: match %v, empty %v, none %v", match.dirty, empty.dirty, none.dirty)
	}
	h.markChanged("p", "something-new")
	if h.vs.all["p"]&causeChanged == 0 {
		t.Error("a name that no list can hold must mark every view")
	}
	if h.vs.loose["p"]&causeCommit == 0 {
		t.Error("a view without state gets the commit cause")
	}
}

// renderFinished installs a declaration only into the view's own state of the same epoch. A
// registration during the render (a newer epoch) and a close (the state is gone) get nothing
// from the old result, and a close is not undone. Causes from the render time give another pass.
func TestRenderFinishedLifetime(t *testing.T) {
	h := depsHub()
	ev, err := openEvents(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer ev.f.Close()
	h.ev = ev
	d := &viewDeps{prefixes: []string{"a/"}}

	h.vs.mu.Lock()
	st := h.vs.stateLocked("p", "v")
	st.inFlight = true
	epoch := h.vs.epoch["p"]
	h.vs.mu.Unlock()
	h.renderFinished("p", "v", "/r", st, epoch, d, true, "")
	if st.deps != d || st.inFlight {
		t.Fatalf("a good result: %+v", st)
	}

	st.inFlight = true
	h.clearDeclarations("p")
	h.renderFinished("p", "v", "/r", st, epoch, &viewDeps{prefixes: []string{"old/"}}, true, "")
	if st.deps != nil {
		t.Errorf("an old instance's result after a registration installed %+v", st.deps)
	}

	h.vs.mu.Lock()
	epoch = h.vs.epoch["p"]
	st.inFlight = true
	h.vs.mu.Unlock()
	h.forgetView("v")
	h.renderFinished("p", "v", "/r", st, epoch, d, true, "")
	if _, ok := h.vs.views["p"]["v"]; ok {
		t.Error("a result after a close made the view's state again")
	}

	h.vs.mu.Lock()
	st = h.vs.stateLocked("p", "w")
	st.inFlight, st.deps = true, d
	h.vs.mu.Unlock()
	h.markCommit("p", []string{"z/1"}) // in flight: marked without the filter
	h.renderFinished("p", "w", "/r", st, epoch, nil, false, "")
	if st.deps != nil || st.dirty&causeCommit == 0 || !h.vs.dirty["p"] {
		t.Errorf("a failed render with a commit during it: %+v, pass %v", st, h.vs.dirty["p"])
	}
}

func TestParseDeps(t *testing.T) {
	for raw, want := range map[string]string{
		``: "none", `null`: "none",
		`{}`:                           "prefixes=nil changes=nil",
		`{"prefixes":{}}`:              "prefixes=[] changes=nil",
		`{"prefixes":[],"changes":{}}`: "prefixes=[] changes=[]",
		`{"prefixes":["a/",""],"changes":["plugins"]}`: "prefixes=[a/ ] changes=[plugins]",
	} {
		d, err := parseDeps(json.RawMessage(raw))
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		got := "none"
		if d != nil {
			got = fmtList("prefixes", d.prefixes) + " " + fmtList("changes", d.changes)
		}
		if got != want {
			t.Errorf("%s: %s, want %s", raw, got, want)
		}
	}
	big := make([]string, depsMaxPrefixes+1)
	bigRaw, _ := json.Marshal(map[string]any{"prefixes": big})
	longRaw, _ := json.Marshal(map[string]any{"prefixes": []string{string(make([]byte, depsMaxPrefixLen+1))}})
	for _, raw := range []string{`"x"`, `[]`, `{"prefixes":"a"}`, `{"prefixes":{"a":1}}`, `{"prefixes":[1]}`, `{"changes":["tabs"]}`, `{"other":[]}`, string(bigRaw), string(longRaw)} {
		if _, err := parseDeps(json.RawMessage(raw)); err == nil {
			t.Errorf("%.60s: no error", raw)
		}
	}
}

func fmtList(name string, l []string) string {
	if l == nil {
		return name + "=nil"
	}
	out := name + "=["
	for i, x := range l {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out + "]"
}
