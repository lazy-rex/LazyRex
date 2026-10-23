package main

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// hubCall makes a hub call of the plugin instance client through dispatch, with the render ID id
// ("": no render argument) and the other arguments of extra.
func hubCall(h *Hub, client, action, id string, extra ...map[string]any) error {
	args := map[string]any{}
	for _, e := range extra {
		for k, v := range e {
			args[k] = v
		}
	}
	if id != "" {
		args["render"] = id
	}
	b, _ := json.Marshal(args)
	p := perform{Action: action, Args: b}
	p.Requester.ClientID = client
	_, err := h.dispatch(context.Background(), p)
	return err
}

// viewSets returns the table set and the change names of a view.
func viewSets(h *Hub, view string) (tables, changes map[string]bool) {
	h.vs.mu.Lock()
	defer h.vs.mu.Unlock()
	st := h.vs.views["p"][view]
	if st == nil {
		return nil, nil
	}
	return st.tables, st.changes
}

// marked reports whether the view has the cause, and clears its causes.
func marked(h *Hub, view string, cause renderCause) bool {
	h.vs.mu.Lock()
	defer h.vs.mu.Unlock()
	st := h.vs.views["p"][view]
	m := st.dirty&cause != 0
	st.dirty = 0
	return m
}

// A covered hub read with an open render ID of the calling instance keeps the render tracked and
// adds its change names. An error, no ID, an unknown ID, an ID of an older instance and an ID of
// another plugin each untrack the caller's render; the other plugin's render stays tracked.
func TestCoveredReadRules(t *testing.T) {
	h := renderHub(t)
	id := h.openRenderID("p", "c1", "v1", 0)
	r := h.vs.renders[id]
	if err := hubCall(h, "c1", "hub.agents", id); err != nil {
		t.Fatal(err)
	}
	if r.untracked || !r.changes["agents"] || len(r.changes) != 1 {
		t.Errorf("hub.agents with the render ID: %+v", r)
	}
	h.closeRenderID(id)

	closed := h.openRenderID("p", "c1", "v2", 0)
	h.closeRenderID(closed)
	for name, call := range map[string]func(id string){
		"an error":            func(id string) { hubCall(h, "c1", "hub.agents.resolve", id) }, // no "to"
		"no ID":               func(string) { hubCall(h, "c1", "hub.agents", "") },
		"unknown ID":          func(string) { hubCall(h, "c1", "hub.agents", "r-unknown") },
		"an older instance":   func(string) { hubCall(h, "c1", "hub.agents", h.openRenderID("p", "c0", "v2", 0)) },
		"a closed ID":         func(string) { hubCall(h, "c1", "hub.agents", closed) },
		"a non-covered read":  func(id string) { hubCall(h, "c1", "hub.plugins", id) },
		"a non-string render": func(string) { h.renderCall("p", "c1", "hub.agents", map[string]any{"render": 1}) },
	} {
		id := h.openRenderID("p", "c1", "v1", 0)
		r := h.vs.renders[id]
		call(id)
		if !r.untracked {
			t.Errorf("%s: the render stays tracked", name)
		}
		h.closeRenderID(id)
	}

	id = h.openRenderID("p", "c1", "v1", 0)
	qid := h.openRenderID("q", "cq", "w", 0)
	hubCall(h, "c1", "hub.agents", qid)
	if !h.vs.renders[id].untracked || h.vs.renders[qid].untracked {
		t.Errorf("an ID of another plugin: caller %v, owner %v", h.vs.renders[id].untracked, h.vs.renders[qid].untracked)
	}
}

// A render that read only covered hub reads gets an empty table set and its change names: a
// commit to a table does not mark it, a schema change does, and changed() marks it for a name
// that it read, also when its deps.changes is empty.
func TestCoveredReadOnlyRender(t *testing.T) {
	h := renderHub(t)
	markAll(h)
	renderWith(t, h, map[string]renderStep{"v1": {hub: []string{"hub.agents"}, deps: map[string]any{"changes": []any{}}}})
	tables, changes := viewSets(h, "v1")
	if tables == nil || len(tables) != 0 || !changes["agents"] || len(changes) != 1 {
		t.Fatalf("v1: tables %v, changes %v", tables, changes)
	}
	marked(h, "v1", causeTest|causeCommit|causeChanged)
	h.markSQL("p", []string{"t"})
	if marked(h, "v1", causeCommit) {
		t.Error("a commit to t marked a view that read no table")
	}
	h.markSQL("p", []string{"*"})
	if !marked(h, "v1", causeCommit) {
		t.Error("a schema change did not mark the view")
	}
	h.markChanged("p", "workers")
	if marked(h, "v1", causeChanged) {
		t.Error("changed(workers) marked a view that read agents and declared no change")
	}
	h.markChanged("p", "agents")
	if !marked(h, "v1", causeChanged) {
		t.Error("changed(agents) did not mark a view that read hub.agents")
	}
}

// The lifecycle of change names follows the table set: a change during the render marks the view
// again; a new registration during the render installs nothing, and one after it drops the names;
// a later failed or untracked render clears both sets; SQL and covered reads keep both sets.
func TestCoveredReadLifecycle(t *testing.T) {
	ta := map[string]bool{"a": true}
	narrow := map[string]any{"changes": []any{}}

	h := renderHub(t)
	markAll(h)
	renderWith(t, h, map[string]renderStep{
		"v1": {hub: []string{"hub.agents"}, deps: narrow, during: func() { h.markChanged("p", "agents") }},
	})
	if !marked(h, "v1", causeChanged) {
		t.Error("changed(agents) during the render of v1 is lost")
	}

	h = renderHub(t)
	markAll(h)
	renderWith(t, h, map[string]renderStep{
		"v1": {hub: []string{"hub.agents"}, during: func() { h.clearDeclarations("p") }},
		"v2": {hub: []string{"hub.agents"}},
	})
	if tables, changes := viewSets(h, "v1"); tables != nil || changes != nil {
		t.Errorf("a render from before a new registration installed tables %v, changes %v", tables, changes)
	}
	if _, changes := viewSets(h, "v2"); !changes["agents"] {
		t.Fatalf("v2 read hub.agents: changes %v", changes)
	}
	h.clearDeclarations("p")
	if tables, changes := viewSets(h, "v2"); tables != nil || changes != nil {
		t.Errorf("a new registration keeps tables %v, changes %v", tables, changes)
	}

	for name, later := range map[string]renderStep{
		"failed":    {hub: []string{"hub.agents"}, err: &rex.RexError{Code: "boom", Message: "test"}},
		"untracked": {hub: []string{"hub.agents"}, reads: []map[string]bool{nil}},
		"all":       {hub: []string{"hub.agents"}, refresh: "all"},
	} {
		h := renderHub(t)
		markAll(h)
		renderWith(t, h, map[string]renderStep{"v1": {hub: []string{"hub.agents"}, deps: narrow}})
		if later.err != nil { // the error tree equals the snapshot: no view.changed
			h.vs.snaps["v1"].tree = []byte(`{"back":null,"nodes":[{"id":"$error","retry_action":null,"text":"The plugin could not render this view.","type":"error"}],"title":"Error"}`)
		}
		markAll(h)
		renderWith(t, h, map[string]renderStep{"v1": later})
		if tables, changes := viewSets(h, "v1"); tables != nil || changes != nil {
			t.Errorf("a later %s render keeps tables %v, changes %v", name, tables, changes)
		}
		marked(h, "v1", ^renderCause(0))
		h.markSQL("p", []string{"t"})
		if !marked(h, "v1", causeCommit) {
			t.Errorf("after a %s render, a commit does not mark the view", name)
		}
	}

	h = renderHub(t)
	markAll(h)
	renderWith(t, h, map[string]renderStep{"v1": {reads: []map[string]bool{ta}, hub: []string{"hub.agents"}}})
	if tables, changes := viewSets(h, "v1"); !tables["a"] || len(tables) != 1 || !changes["agents"] || len(changes) != 1 {
		t.Errorf("mixed reads: tables %v, changes %v", tables, changes)
	}
}

// A hub.workers read in which a Rex read of a pane failed untracks the render (workersRead); a
// good read adds "workers".
func TestCoveredWorkersDegraded(t *testing.T) {
	for _, c := range []struct {
		procErr string
		tracked bool
	}{{"", true}, {"internal", false}} {
		h := renderHub(t)
		if _, err := h.db.Exec(workersSchema); err != nil {
			t.Fatal(err)
		}
		mustExec(t, h, `INSERT INTO spawns (spawn_id, caller, key, request, profile, title, agent_id, parent, status, created, session_id, window_id, block_id)
			VALUES ('sp', 'user', 'k', '{}', 'agent', 'W', '', 'user', 'open', '2026-10-23T00:00:00Z', 's1', 'w', 'b')`)
		h.ui.sess = map[string]string{"s1": uiSessionKey} // no session.attach
		h.rex = replyRex(t, func(method string) (any, string) {
			switch method {
			case "session.list_blocks":
				return map[string]any{"blocks": []any{map[string]any{"block_id": "b", "window_id": "w"}}}, ""
			case "com.superlogical.terminal.process":
				if c.procErr != "" {
					return nil, c.procErr
				}
				return map[string]any{"child": map[string]any{"pid": 4242}}, ""
			}
			return map[string]any{}, ""
		})
		markAll(h)
		renderWith(t, h, map[string]renderStep{"v1": {hub: []string{"hub.workers"}}})
		_, changes := viewSets(h, "v1")
		if changes["workers"] != c.tracked {
			t.Errorf("process error %q: changes %v, want tracked %v", c.procErr, changes, c.tracked)
		}
	}
}

// The covered actions of the hub equal COVERED_READS in the SDK. A missing line is a failure: the
// SDK would then send no render ID with a covered read.
func TestCoveredReadsMatchSDK(t *testing.T) {
	src, err := os.ReadFile("../../../sdk/lua/lazyrex.lua")
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(l, "local COVERED_READS = ") {
			line = l
		}
	}
	if line == "" {
		t.Fatal(`sdk/lua/lazyrex.lua has no line that starts with "local COVERED_READS = "`)
	}
	var sdk, hub []string
	for _, m := range regexp.MustCompile(`\['([^']+)'\] = true`).FindAllStringSubmatch(line, -1) {
		sdk = append(sdk, m[1])
	}
	for a := range coveredReads {
		hub = append(hub, a)
	}
	slices.Sort(sdk)
	slices.Sort(hub)
	if !slices.Equal(sdk, hub) {
		t.Errorf("SDK COVERED_READS %v, hub coveredReads %v", sdk, hub)
	}
}

// Each covered action, called with success in a render, installs exactly its change names: a
// changed() of each name marks the view although deps.changes is empty, another name does not, and
// a commit to a table does not.
func TestCoveredReadEachAction(t *testing.T) {
	for _, c := range []struct {
		action string
		args   map[string]any
		names  []string
	}{
		{"hub.agents", nil, []string{"agents"}},
		{"hub.agents.resolve", map[string]any{"to": "a1"}, []string{"agents"}},
		{"hub.workers", nil, []string{"workers"}},
		{"hub.profiles", nil, []string{"profiles", "workers"}},
	} {
		h := renderHub(t)
		if _, err := h.db.Exec(workersSchema); err != nil {
			t.Fatal(err)
		}
		h.idents = map[string]*Agent{"a1": {ID: "a1"}}
		markAll(h)
		renderWith(t, h, map[string]renderStep{"v1": {hub: []string{c.action}, hubArgs: c.args, deps: map[string]any{"changes": []any{}}}})
		tables, changes := viewSets(h, "v1")
		var got []string
		for n := range changes {
			got = append(got, n)
		}
		slices.Sort(got)
		if tables == nil || len(tables) != 0 || !slices.Equal(got, c.names) {
			t.Errorf("%s: tables %v, changes %v, want names %v", c.action, tables, got, c.names)
			continue
		}
		marked(h, "v1", ^renderCause(0))
		h.markSQL("p", []string{"t"})
		if marked(h, "v1", causeCommit) {
			t.Errorf("%s: a commit to t marked the view", c.action)
		}
		for _, n := range changeNames {
			h.markChanged("p", n)
			if got, want := marked(h, "v1", causeChanged), slices.Contains(c.names, n); got != want {
				t.Errorf("%s: changed(%s) marked %v, want %v", c.action, n, got, want)
			}
		}
	}
}

// renderArgAccepted reports whether a declared action accepts {"render": "r-..."} as Rex checks
// it: an action without an args schema takes any argument; a strict schema (additionalProperties
// false) takes only a declared property, and a declared render must be a string.
func renderArgAccepted(a map[string]any) bool {
	args, ok := a["args"].(map[string]any)
	if !ok {
		return true
	}
	props, _ := args["properties"].(map[string]any)
	if p, ok := props["render"].(map[string]any); ok {
		return p["type"] == "string"
	}
	return args["additionalProperties"] != false
}

// The plugin face of each covered read and of hub.sql.query accepts the render ID that the SDK
// sends in a render (the Worktrees suite found hub.workers and hub.profiles refused by Rex). The
// agent face does not offer render, and the shared declarations stay without it.
func TestCoveredReadsAcceptRender(t *testing.T) {
	declared := map[string]map[string]any{}
	for _, a := range hubClientActions(false) {
		declared[a["name"].(string)] = a
	}
	names := []string{"hub.sql.query"}
	for a := range coveredReads {
		names = append(names, a)
	}
	for _, name := range names {
		a, ok := declared[name]
		if !ok {
			t.Errorf("%s: the plugin face does not declare it", name)
			continue
		}
		if !renderArgAccepted(a) {
			t.Errorf("%s: the plugin face refuses render: %v", name, a["args"])
		}
	}
	for _, a := range hubToolActions {
		props, _ := a["args"].(map[string]any)["properties"].(map[string]any)
		if _, ok := props["render"]; ok {
			t.Errorf("%s: the shared declaration has render", a["name"])
		}
		if !agentHubActions[a["name"].(string)] {
			continue
		}
		props, _ = agentFaceAction(a)["args"].(map[string]any)["properties"].(map[string]any)
		if _, ok := props["render"]; ok {
			t.Errorf("%s: the agent face offers render", a["name"])
		}
	}
	for _, a := range agentListActions {
		if renderArgAccepted(a) {
			t.Errorf("%s: the agent face accepts render", a["name"])
		}
	}
}
