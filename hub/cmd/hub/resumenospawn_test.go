package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resumeHandHub is resumeTestHub with the setup profiles "codex" and "claude" (the profile ID is the
// kind), an agent started by hand (codex:hand, no spawn record) and a Rex that answers every call.
func resumeHandHub(t *testing.T) (*Hub, string) {
	t.Helper()
	h := resumeTestHub(t)
	os.WriteFile(h.cfg.Profiles, []byte(`{"profiles":[
		{"id":"agent","name":"Agent","kind":"agent","command":["/bin/echo"],"resume":["/bin/echo","${conversation_id}"]},
		{"id":"codex","name":"Codex","kind":"agent","command":["/bin/echo","${task}"],"args":[{"name":"task"}],"resume":["/bin/echo","resume","${conversation_id}"]}]}`), 0o600)
	h.rex = limitRex(t, 1<<20, func(string, json.RawMessage) (any, string) { return map[string]any{}, "" })
	if _, err := h.db.Exec(`INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES ('codex:hand', 'codex', 'conv-hand', 'detached', 'block:hand', '', 'Hand', 't0', 't0')`); err != nil {
		t.Fatal(err)
	}
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return h, cwd
}

func resumeTargetsOf(t *testing.T, r any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, x := range r.(map[string]any)["targets"].([]any) {
		e := x.(map[string]any)
		out[e["agent"].(string)] = e
	}
	return out
}

// placement, cwd and for are arguments of one agent's resume (lead-resume): the batch forms, release
// and restart refuse them, a malformed value is refused, and each one is part of the request.
func TestResumePlacementCwdForArguments(t *testing.T) {
	h, cwd := resumeHandHub(t)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"placement with kind", map[string]any{"key": "a1", "kind": "codex", "placement": map[string]any{"session": "session:test"}}, "need agent"},
		{"cwd with kind", map[string]any{"key": "a2", "kind": "codex", "cwd": cwd}, "need agent"},
		{"for with session_id", map[string]any{"key": "a3", "session_id": "session:test", "for": "user"}, "need agent"},
		{"for with release", map[string]any{"key": "a4", "release": "codex:hand", "for": "user"}, "need agent"},
		{"relative cwd", map[string]any{"key": "a5", "agent": "codex:hand", "cwd": "rel/dir"}, "absolute directory"},
		{"for not user", map[string]any{"key": "a6", "agent": "codex:hand", "for": "agent"}, `for must be "user"`},
		{"bad placement", map[string]any{"key": "a7", "agent": "codex:hand", "placement": map[string]any{"ratio": 0.3}}, "requires beside"},
	} {
		if _, err := h.adminResume(ctx, c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := h.adminRestart(ctx, map[string]any{"key": "r1", "agent": "codex:hand", "placement": map[string]any{"session": "session:test"}}); err == nil ||
		!strings.Contains(err.Error(), "not an argument of hub.admin.restart") {
		t.Errorf("restart with placement: %v", err)
	}
	if n := countRows(t, h, `SELECT COUNT(*) FROM agent_ops`); n != 0 {
		t.Fatalf("%d operations stored by refused calls", n)
	}
	// The same key with another cwd is another request.
	if _, err := h.adminResume(ctx, map[string]any{"key": "k", "agent": "codex:hand", "cwd": cwd}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(cwd, "other")
	os.Mkdir(other, 0o700)
	if _, err := h.adminResume(ctx, map[string]any{"key": "k", "agent": "codex:hand", "cwd": other}); err == nil || !strings.Contains(err.Error(), "operation_conflict") {
		t.Fatalf("same key, other cwd: %v", err)
	}
}

func countRows(t *testing.T, h *Hub, q string) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An agent started by hand (no spawn record) resumes from the setup profile of its kind, in the
// given cwd: without cwd it is refused, a kind without a setup profile is refused, and the reply
// names the cwd. The admission moves the revision on.
func TestResumeWithoutSpawnRecord(t *testing.T) {
	h, cwd := resumeHandHub(t)
	ctx := context.Background()
	h.db.Exec(`INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES ('gemini:x', 'gemini', 'x', 'detached', 'block:g', '', '', 't0', 't0')`)
	r, err := h.adminResume(ctx, map[string]any{"key": "nocwd", "agent": "codex:hand"})
	if err != nil {
		t.Fatal(err)
	}
	if e := resumeTargetsOf(t, r)["codex:hand"]; e["phase"] != "refused" || e["detail"] != "no spawn record and no cwd" || e["cwd"] != nil {
		t.Fatalf("without cwd: %v", e)
	}
	r, err = h.adminResume(ctx, map[string]any{"key": "gemini", "agent": "gemini:x", "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	if e := resumeTargetsOf(t, r)["gemini:x"]; e["phase"] != "refused" || e["detail"] != "no resume profile for kind gemini" {
		t.Fatalf("no setup profile: %v", e)
	}
	r, err = h.adminResume(ctx, map[string]any{"key": "missing", "agent": "codex:hand", "cwd": filepath.Join(cwd, "gone")})
	if err != nil {
		t.Fatal(err)
	}
	if e := resumeTargetsOf(t, r)["codex:hand"]; e["phase"] != "refused" || !strings.HasPrefix(fmt.Sprint(e["detail"]), "cwd: ") {
		t.Fatalf("a missing cwd: %v", e)
	}
	var before int64
	h.db.QueryRow(`SELECT revision FROM identities WHERE identity = 'codex:hand'`).Scan(&before)
	r, err = h.adminResume(ctx, map[string]any{"key": "ok", "agent": "codex:hand", "cwd": cwd, "revision": float64(before)})
	if err != nil {
		t.Fatal(err)
	}
	if e := resumeTargetsOf(t, r)["codex:hand"]; e["phase"] != "admitted" || e["cwd"] != cwd {
		t.Fatalf("with cwd: %v", e)
	}
	var after int64
	h.db.QueryRow(`SELECT revision FROM identities WHERE identity = 'codex:hand'`).Scan(&after)
	if after != before+1 {
		t.Fatalf("revision %d -> %d", before, after)
	}
}

// The launch of a resume without a spawn record: the kind's setup profile's resume template, in the
// request's cwd and placement. Its spawn row is the identity's spawn record afterwards: a second
// resume takes the normal path, uses that spawn's cwd and names it, and accepts another cwd unused.
func TestResumeWithoutSpawnRecordLaunch(t *testing.T) {
	h, cwd := resumeHandHub(t)
	ctx := context.Background()
	old := spawnCreate
	defer func() { spawnCreate = old }()
	var method string
	var req map[string]any
	spawnCreate = func(_ context.Context, _ *Hub, m string, r map[string]any, _ any) error {
		method, req = m, r
		return errors.New("injected: no answer")
	}
	place := map[string]any{"session": "session:rebuilt"}
	if _, err := h.adminResume(ctx, map[string]any{"key": "k1", "agent": "codex:hand", "cwd": cwd, "placement": place, "for": "user"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.resumeStep(ctx); err != nil {
		t.Fatal(err)
	}
	if method != "session.new_window" || req["session_id"] != "session:rebuilt" {
		t.Fatalf("create %s %v", method, req)
	}
	opts := req["layout"].(map[string]any)["block"].(map[string]any)["options"].(map[string]any)
	cmd := fmt.Sprint(opts["command"])
	if opts["cwd"] != cwd || !strings.HasSuffix(cmd, "/bin/echo resume conv-hand]") || !strings.Contains(cmd, "-u CLAUDE_CODE_CHILD_SESSION") {
		t.Fatalf("options %v", opts)
	}
	var spawnID, profile, resumeOf, title string
	if err := h.db.QueryRow(`SELECT spawn_id, profile, resume_of, title FROM spawns WHERE resume_of = 'codex:hand'`).Scan(&spawnID, &profile, &resumeOf, &title); err != nil {
		t.Fatal(err)
	}
	if profile != "codex" || title != "Hand" {
		t.Fatalf("spawn row: profile %q title %q", profile, title)
	}

	// The pane opened and the agent rebound there, then its pane closed again: detached, with a
	// spawn record now.
	h.db.Exec(`UPDATE spawns SET block_id = 'block:new', status = 'closed' WHERE spawn_id = ?`, spawnID)
	h.db.Exec(`UPDATE agent_op_targets SET phase = 'rebound' WHERE identity = 'codex:hand'`)
	h.db.Exec(`UPDATE identities SET block_id = 'block:new', updated = 't2' WHERE identity = 'codex:hand'`)
	other := filepath.Join(cwd, "snapshot-cwd")
	os.Mkdir(other, 0o700)
	r, err := h.adminResume(ctx, map[string]any{"key": "k2", "agent": "codex:hand", "cwd": other})
	if err != nil {
		t.Fatal(err)
	}
	if e := resumeTargetsOf(t, r)["codex:hand"]; e["phase"] != "admitted" || e["cwd"] != cwd {
		t.Fatalf("second resume: %v; want admitted in the spawn's cwd %s", e, cwd)
	}
	if _, err := h.resumeStep(ctx); err != nil {
		t.Fatal(err)
	}
	opts = req["layout"].(map[string]any)["block"].(map[string]any)["options"].(map[string]any)
	if opts["cwd"] != cwd {
		t.Fatalf("second launch cwd %v, want the spawn's %s", opts["cwd"], cwd)
	}
}

// for is only for a registered plugin instance: another caller that passes the operator guard (an
// operator outside every pane) is refused, and nothing is stored.
func TestResumeForUserNeedsAPlugin(t *testing.T) {
	h, cwd := resumeHandHub(t)
	raw, _ := json.Marshal(map[string]any{"key": "k", "agent": "codex:hand", "cwd": cwd, "for": "user"})
	p := perform{Action: "hub.admin.resume", Args: raw}
	p.Requester.ClientID = "client:not-a-plugin"
	p.Requester.Principal.PID = os.Getpid() // an operator outside every pane: the guard passes
	if _, err := h.dispatch(context.Background(), p); err == nil || !strings.Contains(err.Error(), `only a registered plugin may act for "user"`) {
		t.Fatalf("for from an operator: %v", err)
	}
	if n := countRows(t, h, `SELECT COUNT(*) FROM agent_ops`); n != 0 {
		t.Fatalf("%d operations stored", n)
	}
}
