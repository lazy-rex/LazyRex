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

// A submit names the revision that the user saw; another revision is refused before anything runs.
func TestRestartFormStaleRevision(t *testing.T) {
	h := resumeTestHub(t)
	fc := formContext{Restart: &restartForm{Kind: "codex", Revision: 2, Key: "k2", Targets: []restartToken{restartTestToken()}}}
	for _, action := range []string{"restart@1", "restart@x", "restart@"} {
		_, err := h.restartFormAction(context.Background(), "view-1", fc, action)
		var se *storeError
		if !errors.As(err, &se) || se.code != "stale" {
			t.Errorf("%s: %v", action, err)
		}
	}
	if _, err := h.restartFormAction(context.Background(), "view-1", fc, "nope"); err == nil {
		t.Error("an unknown action passed")
	}
	if res, err := h.restartFormAction(context.Background(), "view-1", fc, "close"); err != nil || res.(map[string]any)["close_form"] != true {
		t.Errorf("close: %v %v", res, err)
	}
}

// After the admission the form is frozen. A retry of the restart replays the operation without
// reading the targets again (the restart changed them), Refresh is refused, and the render shows
// the outcome.
func TestRestartFormFrozenAfterAdmission(t *testing.T) {
	h := resumeTestHub(t)
	ctx := context.Background()
	f := &restartForm{Kind: "codex", Revision: 1, Key: "restart-form-k", Targets: []restartToken{restartTestToken()}}
	fp, _ := json.Marshal(map[string]any{"action": "restart", "kind": "codex", "form_revision": 1})
	h.db.Exec(`INSERT INTO agent_ops VALUES ('op-1', 'admin', ?, 'restart', ?, 't0')`, f.Key, string(fp))
	h.db.Exec(`INSERT INTO agent_op_targets (op_id, identity, seq, phase, detail, updated, stop_result) VALUES ('op-1', 'codex:a', 0, 'resumed', '', 't1', 'interrupted')`)
	fc := formContext{Restart: f}
	// No Rex in this test: a target read would fail. The replay must not read one.
	res, err := h.restartFormAction(ctx, "view-1", fc, "restart@1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if op := res.(map[string]any)["operation"].(map[string]any); op["op_id"] != "op-1" {
		t.Fatalf("replay: %v", res)
	}
	_, err = h.restartFormAction(ctx, "view-1", fc, "refresh")
	var se *storeError
	if !errors.As(err, &se) || se.code != "invalid_state" {
		t.Fatalf("refresh after the admission: %v", err)
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

// restartFormCall answers like a plugin: a refusal is a definite error with its code.
func TestRestartFormCallErrors(t *testing.T) {
	h := resumeTestHub(t)
	ctxJSON, _ := json.Marshal(formContext{Restart: &restartForm{Kind: "codex", Revision: 3, Key: "k"}})
	err := h.restartFormCall(context.Background(), "view.action", json.RawMessage(`{"view_id":"v","context":`+string(ctxJSON)+`,"action":"restart@2"}`), nil)
	if re, definite := definiteFailure(err); !definite || re.Code != "stale" {
		t.Fatalf("stale: %v", err)
	}
	err = h.restartFormCall(context.Background(), "view.render", map[string]any{"view_id": "v"}, nil)
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
