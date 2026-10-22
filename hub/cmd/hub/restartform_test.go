package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

func restartTestToken() restartToken {
	return restartToken{Identity: "codex:a", Session: "session:s", Block: "block:b", Spawn: "spawn-1", PID: 7, Start: "1.000001",
		Path: "/opt/codex/1", Invoked: "/opt/bin/codex", Program: "/opt/bin/codex"}
}

// The token compares the pane and the process generation apart; missing evidence refuses.
func TestRestartTokenChanged(t *testing.T) {
	shown := restartTestToken()
	for _, c := range []struct {
		what string
		edit func(*restartToken)
		want string
	}{
		{"the same", func(*restartToken) {}, ""},
		{"another pane, the same pid", func(n *restartToken) { n.Block = "block:c" }, "moved to another pane"},
		{"another session", func(n *restartToken) { n.Session = "session:t" }, "moved to another pane"},
		{"another spawn", func(n *restartToken) { n.Spawn = "spawn-2" }, "another spawn"},
		{"a reused pid", func(n *restartToken) { n.Start = "2.000001" }, "process changed"},
		{"an exec in the same pid", func(n *restartToken) { n.Path = "/bin/sleep" }, "executable changed"},
		{"another exec path", func(n *restartToken) { n.Invoked = "" }, "executable changed"},
		{"refused now", func(n *restartToken) { n.Refused = "the agent is detached, not current" }, "detached"},
	} {
		now := shown
		c.edit(&now)
		if got := shown.changed(now); c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.what, got, c.want)
		}
	}
	for _, edit := range []func(*restartToken){
		func(n *restartToken) { n.Spawn = "" }, func(n *restartToken) { n.PID = 0 }, func(n *restartToken) { n.Start = "" },
		func(n *restartToken) { n.Path = "" }, func(n *restartToken) { n.Program = "" },
	} {
		tok := restartTestToken()
		edit(&tok)
		if tok.incomplete() == "" {
			t.Errorf("incomplete evidence passes: %+v", tok)
		}
	}
	if restartTestToken().incomplete() != "" {
		t.Error("complete evidence refused")
	}
}

// restartFormHub is a hub with the view tables, and one open form view "v" with the form f, bound
// to the renderer client "c1" (attachment "a1"). The owner's worker is a channel that nobody reads:
// a test runs runOps itself.
func restartFormHub(t *testing.T, f *restartForm) *Hub {
	t.Helper()
	h := resumeTestHub(t)
	ctx := context.Background()
	if _, err := h.db.Exec(uiSchema); err != nil {
		t.Fatal(err)
	}
	if err := formsStart(ctx, h.db); err != nil {
		t.Fatal(err)
	}
	if err := h.viewsStart(ctx); err != nil {
		t.Fatal(err)
	}
	h.vs.workers[restartFormOwner] = make(chan struct{}, 1)
	b, _ := json.Marshal(formContext{Restart: f})
	if _, err := h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, context, client_id, attachment) VALUES ('v', ?, 'form', 'restart', 'open', ?, 'c1', 'a1')`,
		restartFormOwner, string(b)); err != nil {
		t.Fatal(err)
	}
	return h
}

// storedForm is the form in the view row now.
func storedForm(t *testing.T, h *Hub) *restartForm {
	t.Helper()
	var s string
	if err := h.db.QueryRow(`SELECT context FROM views WHERE view_id = 'v'`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	var fc formContext
	if err := json.Unmarshal([]byte(s), &fc); err != nil || fc.Restart == nil {
		t.Fatalf("stored context %s: %v", s, err)
	}
	return fc.Restart
}

// formAction runs one view.action through restartFormCall, as the worker does.
func formAction(h *Hub, opKey, action string) error {
	return h.restartFormCall(context.Background(), "view.action", map[string]any{"view_id": "v", "operation_key": opKey, "action": action}, nil)
}

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if re, definite := definiteFailure(err); !definite || re.Code != code {
		t.Fatalf("%s: %v, want %s", what, err, code)
	}
}

// A submit names the revision that the user saw; another revision is refused before anything runs.
func TestRestartFormStaleRevision(t *testing.T) {
	h := restartFormHub(t, &restartForm{Kind: "codex", Revision: 2, Key: "k2", Targets: []restartToken{restartTestToken()}})
	for _, action := range []string{"restart@1", "restart@x", "restart@", "restart", "refresh@1", "refresh"} {
		wantCode(t, action, formAction(h, "op-"+action, action), "stale")
	}
	if err := formAction(h, "op-nope", "nope"); err == nil {
		t.Error("an unknown action passed")
	}
	var res map[string]any
	if err := h.restartFormCall(context.Background(), "view.action", map[string]any{"view_id": "v", "operation_key": "c", "action": "close"}, &res); err != nil || res["close_form"] != true {
		t.Errorf("close: %v %v", res, err)
	}
}

// The reviewer's probe (codex-24, b15b265): a context copied at the admission is not the form. The
// call reads the view row, so a context in the arguments changes nothing.
func TestRestartFormReadsTheStoredContext(t *testing.T) {
	h := restartFormHub(t, &restartForm{Kind: "codex", Revision: 1, Key: "k1"})
	if err := formAction(h, "r1", "refresh@1"); err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(formContext{Restart: &restartForm{Kind: "codex", Revision: 1, Key: "k1"}})
	err := h.restartFormCall(context.Background(), "view.action", map[string]any{"view_id": "v", "operation_key": "x1", "context": json.RawMessage(old), "action": "restart@1"}, nil)
	wantCode(t, "a queued restart of revision 1", err, "stale")
	if f := storedForm(t, h); f.Revision != 2 || f.Key == "k1" || f.RefreshOp != "r1" {
		t.Fatalf("after the refresh: %+v", f)
	}
}

// Two operations queued while revision 1 is shown, run by the owner's worker in order (viewAdmit,
// then runOps): Refresh takes revision 2, and the restart of revision 1 is stale. A retry of the
// Refresh key after the context changed is the same operation, not a conflict. A crash between the
// context write and the operation's record runs the Refresh again: no new snapshot, no new key.
func TestRestartFormQueuedOperations(t *testing.T) {
	h := restartFormHub(t, &restartForm{Kind: "codex", Revision: 1, Key: "k1"})
	ctx := context.Background()
	var p perform
	p.Requester.ClientID = "c1"
	admit := func(key, action string) any {
		t.Helper()
		res, err := h.viewAdmit(ctx, "action", p, map[string]any{"view_id": "v", "attachment": "a1", "operation_key": key, "node_id": "n", "action": action, "input": map[string]any{}})
		if err != nil {
			t.Fatalf("admit %s %s: %v", key, action, err)
		}
		return res
	}
	state := func(key string) map[string]any {
		t.Helper()
		s, err := h.opState(ctx, restartFormOwner, key)
		if err != nil {
			t.Fatal(err)
		}
		return s.(map[string]any)
	}
	admit("r1", "refresh@1")
	admit("x1", "restart@1")
	h.runOps(ctx, restartFormOwner)
	if s := state("r1"); s["status"] != "succeeded" {
		t.Fatalf("refresh: %v", s)
	}
	if s := state("x1"); s["status"] != "failed" || !strings.Contains(string(s["error"].(json.RawMessage)), `"stale"`) {
		t.Fatalf("the queued restart of revision 1: %v", s)
	}
	f := storedForm(t, h)
	if f.Revision != 2 || f.RefreshOp != "r1" {
		t.Fatalf("after the refresh: %+v", f)
	}
	// The renderer lost the reply and sends the same key again.
	if s := admit("r1", "refresh@1").(map[string]any); s["status"] != "succeeded" {
		t.Fatalf("a retry of the refresh key: %v", s)
	}
	// A crash before the record: the operation is pending again and runs again.
	h.db.Exec(`UPDATE view_ops SET status = 'pending', result = '' WHERE op_key = 'r1'`)
	h.runOps(ctx, restartFormOwner)
	if s := state("r1"); s["status"] != "succeeded" {
		t.Fatalf("the refresh after a crash: %v", s)
	}
	if again := storedForm(t, h); again.Revision != 2 || again.Key != f.Key {
		t.Fatalf("the retry took a new snapshot: %+v, was %+v", again, f)
	}
	// Another Refresh of the old revision is stale.
	admit("r2", "refresh@1")
	h.runOps(ctx, restartFormOwner)
	if s := state("r2"); s["status"] != "failed" {
		t.Fatalf("a second refresh of revision 1: %v", s)
	}
}

// After the admission the form is frozen. A retry of the restart replays the operation without
// reading the targets again (the restart changed them), Refresh is refused also when it was queued
// for an older revision, and the render shows the outcome.
func TestRestartFormFrozenAfterAdmission(t *testing.T) {
	f := &restartForm{Kind: "codex", Revision: 2, Key: "restart-form-k", Targets: []restartToken{restartTestToken()}}
	h := restartFormHub(t, f)
	ctx := context.Background()
	fp, _ := json.Marshal(map[string]any{"action": "restart", "kind": "codex", "form_revision": 2})
	h.db.Exec(`INSERT INTO agent_ops VALUES ('op-1', 'admin', ?, 'restart', ?, 't0')`, f.Key, string(fp))
	h.db.Exec(`INSERT INTO agent_op_targets (op_id, identity, seq, phase, detail, updated, stop_result) VALUES ('op-1', 'codex:a', 0, 'resumed', '', 't1', 'interrupted')`)
	// No Rex in this test: a target read would fail. The replay must not read one.
	var res map[string]any
	if err := h.restartFormCall(ctx, "view.action", map[string]any{"view_id": "v", "operation_key": "x2", "action": "restart@2"}, &res); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if op := res["operation"].(map[string]any); op["op_id"] != "op-1" {
		t.Fatalf("replay: %v", res)
	}
	for _, action := range []string{"refresh@2", "refresh@1"} {
		wantCode(t, action+" after the admission", formAction(h, "r-"+action, action), "invalid_state")
	}
	if got := storedForm(t, h); got.Key != f.Key || got.Revision != 2 {
		t.Fatalf("a refresh replaced the admitted form: %+v", got)
	}
	tree, err := h.restartFormRender(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(tree)
	for _, want := range []string{`"Done."`, `"resumed · interrupted"`, `"name":"close"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %s in %s", want, b)
		}
	}
	if strings.Contains(string(b), "refresh") || strings.Contains(string(b), "restart@") {
		t.Errorf("a frozen form offers a new restart: %s", b)
	}
}

// restartFormCall answers like a plugin: a refusal is a definite error with its code. A view
// without a stored form is invalid_state.
func TestRestartFormCallErrors(t *testing.T) {
	h := restartFormHub(t, &restartForm{Kind: "codex", Revision: 3, Key: "k"})
	wantCode(t, "stale", formAction(h, "o", "restart@2"), "stale")
	err := h.restartFormCall(context.Background(), "view.render", map[string]any{"view_id": "missing"}, nil)
	var re *rex.RexError
	if !errors.As(err, &re) || re.Code != "invalid_state" {
		t.Fatalf("no stored snapshot: %v", err)
	}
}

// No plugin can have the form's owner: the manifest name rule has no colon, and register refuses it.
func TestRestartFormOwnerIsReserved(t *testing.T) {
	if pluginName.MatchString(restartFormOwner) {
		t.Fatal("a manifest could name the restart form's owner")
	}
	h, _ := workerTestHub(t)
	if _, err := h.register(perform{}, map[string]any{"plugin": restartFormOwner, "instance_id": "i"}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("register: %v", err)
	}
}

func TestPaletteHasTheRestartEntries(t *testing.T) {
	lua := paletteLua(nil)
	for _, want := range []string{`name = "lazyrexhub_restart_codex"`, `run = lazyrex_restart_form("codex")`, `name = "lazyrexhub_restart_claude"`,
		`action = "hub.admin.restart.form"`, `args = { kind = kind, ctx = ctx }`} {
		if !strings.Contains(lua, want) {
			t.Errorf("no %s in the palette file", want)
		}
	}
}
