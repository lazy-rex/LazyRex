package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// conflictSites names the test that asserts each file's key conflict (B3′: every hub keyed request
// answers the same key with another request with operation_conflict). TestKeyConflictSitesCovered
// fails when a file gains or loses a site, so a new site gets a test.
var conflictSites = map[string]string{
	"workers.go":      "TestKeyConflictCode",
	"spawnwindow.go":  "TestKeyConflictCode",
	"resume.go":       "TestKeyConflictCode",
	"sessions.go":     "TestKeyConflictCode",
	"viewsvc.go":      "TestKeyConflictCode",
	"watchsvc.go":     "TestKeyConflictCode",
	"peers.go":        "TestKeyConflictCode",
	"identity.go":     "TestKeyConflictCode", // callerKeyRow directly: a dispatch needs a real pane
	"grants.go":       "tests/harness/tests/grants.lua",
	"pluginops.go":    "pluginops_test.go, pluginops_lab_test.go",
	"sqlsvc.go":       "sqlsvc_test.go",
	"promptanswer.go": "promptanswer_test.go",
}

func TestKeyConflictSitesCovered(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	site := regexp.MustCompile(`refuse\("operation_conflict"`)
	found := map[string]bool{}
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(site.FindAll(b, -1)); n > 0 {
			found[name] = true
			if n != 1 {
				t.Errorf("%s has %d key conflict sites; the table counts one", name, n)
			}
			if conflictSites[name] == "" {
				t.Errorf("%s has a key conflict site that no test covers", name)
			}
		}
	}
	for name := range conflictSites {
		if !found[name] {
			t.Errorf("%s has no key conflict site now: remove it from the table", name)
		}
	}
}

// TestKeyConflictCode: each keyed request, with its key used for another request, fails with the
// code operation_conflict and the message "operation_conflict: <text>". Each row runs through
// dispatch, except the agent-set check (a dispatch needs a real pane).
func TestKeyConflictCode(t *testing.T) {
	h := restartFormHub(t, &restartForm{Kind: "codex", Revision: 1, Key: "k1"})
	ctx := context.Background()
	h.plugins = map[string]*Plugin{"p": {Name: "p", ClientID: "c1", entry: &configEntry{}}}
	dir := t.TempDir()
	seed := ensuredSchema + spawnWindowSchema + watchSchema + `
INSERT INTO spawn_windows (caller, key, request, session_id, spawn_ids, created) VALUES ('user', 'w1', '{}', 'session:test', '[]', 'x');
INSERT INTO agent_ops (op_id, caller, key, action, request, created) VALUES ('op1', 'admin', 'r1', 'resume', '{}', 'x');
INSERT INTO ui_ensured (plugin, key, name, state) VALUES ('p', 'e1', 'Work', 'prepared');
INSERT INTO watches (plugin, key, watch_id, path, resolved, glob, settle_ms, emit_existing, state) VALUES ('p', 'w1', 'wid1', '` + dir + `', '` + dir + `', '*', 1000, 0, 'active');`
	if _, err := h.db.Exec(seed); err != nil {
		t.Fatal(err)
	}
	if _, err := h.viewAdmit(ctx, "action", viewPerform(), map[string]any{"view_id": "v", "attachment": "a1", "operation_key": "o1", "node_id": "n", "action": "refresh@1"}); err != nil {
		t.Fatal(err)
	}

	call := func(client, action string, pid int, args map[string]any) error {
		raw, _ := json.Marshal(args)
		p := perform{Action: action, Args: raw}
		p.Requester.ClientID = client
		p.Requester.Principal.PID = pid
		_, err := h.dispatch(ctx, p)
		return err
	}
	for _, c := range []struct {
		name string
		err  error
		msg  string
	}{
		{"hub.spawn", call("c1", "hub.spawn", 0, map[string]any{"for": "user", "key": "key", "profile": "agent"}),
			`key "key" was used for a different request`},
		{"hub.spawn_window", call("c1", "hub.spawn_window", 0, map[string]any{"for": "user", "key": "w1", "layout": map[string]any{"leaf": map[string]any{"profile": "agent"}}}),
			`key "w1" was used for a different request`},
		{"hub.admin.resume", call("", "hub.admin.resume", os.Getpid(), map[string]any{"key": "r1", "kind": "codex"}),
			`key "r1" was used for a different request`},
		{"hub.session.ensure", call("c1", "hub.session.ensure", 0, map[string]any{"key": "e1", "name": "Other"}),
			`key "e1" was used with another name`},
		{"hub.view.action", call("c1", "hub.view.action", 0, map[string]any{"view_id": "v", "attachment": "a1", "operation_key": "o1", "node_id": "n", "action": "close"}),
			`key "o1" has different arguments`},
		{"hub.watch.ensure", call("c1", "hub.watch.ensure", 0, map[string]any{"key": "w1", "path": dir, "glob": "*.md"}),
			`key w1 has another spec; remove it first`},
		{"agent set", agentSetConflict(t, h), `key "dup" was used by claude:x and by spawn:spawn-test before they became one identity; use a new key`},
	} {
		wantConflict(t, c.name, c.err, c.msg)
	}

	a := peerHub(t, "a", "b", nil)
	b := peerHub(t, "b", "a", func(map[string]any) (any, string) { return map[string]any{}, "" })
	linkHubs(a, b, func() bool { return false })
	if _, err := peerSendAs(a, "c-a", "b", "k1", "one"); err != nil {
		t.Fatal(err)
	}
	_, err := peerSendAs(a, "c-a", "b", "k1", "two")
	wantConflict(t, "hub.peer.send", err, `key "k1" was sent to b with another body`)
}

func viewPerform() perform {
	var p perform
	p.Requester.ClientID = "c1"
	return p
}

// agentSetConflict: two IDs of one agent (claude:x and its spawn address) each used the key "dup"
// before the spawn bound. A later request of the agent cannot pick one of the two rows.
func agentSetConflict(t *testing.T, h *Hub) error {
	t.Helper()
	if _, err := h.db.Exec(`UPDATE spawns SET identity = 'claude:x' WHERE spawn_id = 'spawn-test';
INSERT INTO spawn_windows (caller, key, request, session_id, spawn_ids, created) VALUES ('claude:x', 'dup', '{}', 's', '[]', 'x'), ('spawn:spawn-test', 'dup', '{}', 's', '[]', 'x')`); err != nil {
		t.Fatal(err)
	}
	_, err := h.callerKeyRow(context.Background(), "spawn_windows", "request", "claude:x", "dup")
	return err
}

func wantConflict(t *testing.T, name string, err error, text string) {
	t.Helper()
	var se *storeError
	if !errors.As(err, &se) || storeErrorCode(se) != "operation_conflict" || se.Error() != "operation_conflict: "+text {
		t.Errorf("%s: %v (want operation_conflict: %s)", name, err, text)
	}
}
