package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func userAdmit(h *Hub, key string, req opRequest) (map[string]any, error) {
	v, err := h.admitOp(context.Background(), opOrigin{"user", "view-1"}, "user", key, req)
	if err != nil {
		return nil, err
	}
	return v.(map[string]any), nil
}

func rawJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// add admission (step 4 C1): a GitHub source only; the default id (the last path element,
// lowercased) or an explicit id, stored with the op; no entry with that id, or with the same
// repository and subdir at any ref (the lock key, without case for owner and repository).
func TestPluginOpAddAdmission(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, `{"plugins":[{"id":"tools","source":"github:acme/tools@latest"}]}`)
	opsLoadEntries(t, h)
	add := func(in map[string]any) (map[string]any, error) {
		return userAdmit(h, "add-"+in["source"].(string)+"-"+strings.Repeat("x", len(in)), opRequest{Op: "add", Input: rawJSON(in)})
	}
	for _, c := range []struct {
		in   map[string]any
		code string
		text string
	}{
		{map[string]any{"source": "/abs/dir"}, "", "needs a GitHub source"},
		{map[string]any{"source": "github:acme/Foo.Bar@v1.0.0"}, "", `the default id "foo.bar"`},
		{map[string]any{"source": "github:acme/notes@v1", "id": "Bad"}, "", "must match"},
		{map[string]any{"source": "github:acme/notes@v1", "id": "tools"}, "conflict", "managed:tools exists"},
		{map[string]any{"source": "github:acme/tools@v2.0.0", "id": "t2"}, "conflict", "the same repository as managed:tools"},
		{map[string]any{"source": "github:ACME/Tools@main", "id": "t3"}, "conflict", "the same repository"},
		{map[string]any{"source": "github:acme/notes@v1", "extra": true}, "", "unknown field"},
	} {
		_, err := add(c.in)
		if err == nil || (c.code != "" && !isRefusal(err, c.code)) || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%v: %v", c.in, err)
		}
	}
	// The default id, and a subdir of the same repository (another lock key).
	v, err := add(map[string]any{"source": "github:acme/notes@latest"})
	if err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, v["op_id"].(string))
	if op.Entry != "managed:notes" || op.Base != "absent" || op.Name != "" || op.Phase != "queued" ||
		op.Input != `{"source":"github:acme/notes@latest","id":"notes","args":{},"watch_roots":[]}` ||
		op.EntryIdentity != `["managed:notes","github:acme/notes@latest"]` {
		t.Fatalf("the add op: %+v", op)
	}
	if _, err := add(map[string]any{"source": "github:acme/tools/sub/Deep@v1"}); err != nil {
		t.Fatalf("a subdir of the same repository: %v", err)
	}
	if op := opsOf(t, h, mustLastOp(t, h)); op.Entry != "managed:deep" {
		t.Fatalf("the default id of a subdir: %+v", op)
	}
	// An agent add and an agent update are user_only (temporary, until the request flow).
	for _, req := range []opRequest{{Op: "add", Input: rawJSON(map[string]any{"source": "github:acme/x@v1"})},
		{Op: "update", Entry: "managed:tools", Input: rawJSON(map[string]any{"to": "latest"})}} {
		if _, err := opsAdmit(t, h, "a1", "agent-"+req.Op, req); !isRefusal(err, "user_only") {
			t.Errorf("agent %s: %v", req.Op, err)
		}
	}
}

func mustLastOp(t *testing.T, h *Hub) string {
	t.Helper()
	var id string
	if err := h.db.QueryRow(`SELECT op_id FROM plugin_ops ORDER BY created DESC, rowid DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// update admission: managed GitHub entries only; to is "latest" or a tag name, never a branch ref
// or a commit; the base is the entry at admission.
func TestPluginOpUpdateAdmission(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	local := opsLocal(t, t.TempDir(), "loc")
	writeManaged(t, dir, strings.Replace(local, `]}`, `,{"id":"tools","source":"github:acme/tools@v1.0.0"}]}`, 1))
	opsLoadEntries(t, h)
	update := func(entry, to string) error {
		_, err := userAdmit(h, "up-"+entry+"-"+to, opRequest{Op: "update", Entry: entry, Input: rawJSON(map[string]any{"to": to})})
		return err
	}
	if err := update("managed:loc", "latest"); err == nil || !strings.Contains(err.Error(), "needs a GitHub source") {
		t.Errorf("a local entry: %v", err)
	}
	for _, to := range []string{"", "refs/heads/main", "refs/tags/v1", strings.Repeat("a", 40), "v1 x", "v1@x"} {
		if err := update("managed:tools", to); err == nil || !strings.Contains(err.Error(), "latest") {
			t.Errorf("to %q: %v", to, err)
		}
	}
	for _, to := range []string{"latest", "v1.2.0", "release/2"} {
		if err := update("managed:tools", to); err != nil {
			t.Errorf("to %q: %v", to, err)
		}
	}
	op := opsOf(t, h, mustLastOp(t, h))
	if op.Kind != "update" || op.Name != "tools" || op.Input != `{"to":"release/2"}` ||
		op.Base != `{"id":"tools","source":"github:acme/tools@v1.0.0","args":{},"watch_roots":[]}` {
		t.Fatalf("the update op: %+v", op)
	}
}

// restart admission: any entry (managed or config), also from an agent; the plugin must run, and
// its candidate must be its run; the home plugin is refused; the target is the run and the
// instance at admission.
func TestPluginOpRestartAdmission(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	e := opsEntry(t, h, "managed:x")
	pl := &Plugin{Name: "x", entry: e}
	h.plugins["x"] = pl
	restart := func(key string) error {
		_, err := opsAdmit(t, h, "a1", key, opRequest{Op: "restart", Entry: "managed:x"})
		return err
	}
	if err := restart("r0"); !isRefusal(err, "refused") || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("not running: %v", err)
	}
	run := pluginRun{Name: "x", Source: e.Source}
	pl.ClientID, pl.ChildPID, pl.ChildStart, pl.InstanceID, pl.run = "client:x", os.Getpid(), startTime(os.Getpid()), "x-1-aaaaaaaa", run
	other := run
	other.Source = "/elsewhere"
	h.entriesMu.Lock()
	e.cand = &other
	h.entriesMu.Unlock()
	if err := restart("r1"); !isRefusal(err, "candidate_changed") || !strings.Contains(err.Error(), "use update") {
		t.Fatalf("another candidate: %v", err)
	}
	h.entriesMu.Lock()
	e.cand = &run
	h.entriesMu.Unlock()
	if err := restart("r2"); err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, mustLastOp(t, h))
	var x opRestartTarget
	json.Unmarshal([]byte(op.Target), &x)
	if op.Kind != "restart" || x.Instance != "x-1-aaaaaaaa" || x.Source != e.Source || op.Origin.Kind != "agent" {
		t.Fatalf("the restart op: %+v %+v", op, x)
	}
	h.cfg.Home = "x"
	if err := restart("r3"); !isRefusal(err, "refused") || !strings.Contains(err.Error(), "home plugin") {
		t.Fatalf("the home plugin: %v", err)
	}
}

// The pin column: a lab store from before it gets it at the manager's start.
func TestPluginOpsPinColumn(t *testing.T) {
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(strings.Replace(pluginOpsSchema, "\n  pin TEXT NOT NULL DEFAULT '', UNIQUE (scope, key));", "\n  UNIQUE (scope, key));", 1)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := h.pluginOpsSchemaUp(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`SELECT pin FROM plugin_ops`); err != nil {
		t.Fatal(err)
	}
	if err := h.pluginOpsSchemaUp(ctx); err != nil {
		t.Fatalf("a second schema run: %v", err)
	}
}
