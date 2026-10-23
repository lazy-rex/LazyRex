package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A profile's resume template: ${conversation_id} is required, other tokens are declared args, each
// token is a whole argument and not the program, and only an agent profile has one.
func TestProfileResumeTemplate(t *testing.T) {
	path := t.TempDir() + "/profiles.json"
	h := &Hub{}
	h.cfg.Profiles = path
	os.WriteFile(path, []byte(`{"profiles":[{"id":"codex","name":"Codex","kind":"agent",
		"command":["codex","--no-daemon","-m","${model}"],"args":[{"name":"model","default":"gpt"}],
		"resume":["codex","--no-daemon","-m","${model}","resume","${conversation_id}"]}]}`), 0o600)
	byID, _, err := h.loadProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p := byID["codex"]
	got := strings.Join(resumeArgv(&spawnRow{Agent: "codex-3"}, p, map[string]string{"model": "sol"}, "conv-1"), " ")
	// argv[0] goes through LookPath, as for the command.
	if !strings.HasSuffix(got, "codex --no-daemon -m sol resume conv-1") || !strings.Contains(got, " LAZYREX_AGENT=codex-3 ") {
		t.Errorf("argv: %s", got)
	}
	// The resume launch keeps the spawn's environment cleanup.
	if !strings.Contains(got, "-u CLAUDE_CODE_CHILD_SESSION") {
		t.Errorf("no env cleanup: %s", got)
	}
	// A conversation_id from the resolved args cannot replace the identity's.
	got = strings.Join(resumeArgv(&spawnRow{}, p, map[string]string{"model": "sol", "conversation_id": "other"}, "conv-1"), " ")
	if !strings.HasSuffix(got, "resume conv-1") {
		t.Errorf("conversation_id override: %s", got)
	}
	for _, bad := range []string{
		`"resume":["codex","resume"]`,                       // no ${conversation_id}
		`"resume":["${conversation_id}"]`,                   // the program
		`"resume":["codex","--resume=${conversation_id}"]`,  // not a whole argument
		`"resume":["codex","${task}","${conversation_id}"]`, // not a declared arg
		`"resume":[]`, // empty
	} {
		os.WriteFile(path, []byte(`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["x"],`+bad+`}]}`), 0o600)
		if _, _, err := h.loadProfiles(); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	os.WriteFile(path, []byte(`{"profiles":[{"id":"x","name":"x","kind":"accessory","command":["x"],"resume":["x","${conversation_id}"]}]}`), 0o600)
	if _, _, err := h.loadProfiles(); err == nil {
		t.Error("an accessory profile has a resume template")
	}
}

// resumeTestHub is a store-only hub with a resumable profile and one source spawn in block:old.
func resumeTestHub(t *testing.T) *Hub {
	t.Helper()
	h, src := workerTestHub(t)
	h.vs = newViewSvc() // an agent op kicks the restart forms
	ctx := context.Background()
	if err := h.workersSchemaStart(ctx); err != nil {
		t.Fatal(err)
	}
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.Profiles = filepath.Join(cwd, "profiles.json")
	os.WriteFile(h.cfg.Profiles, []byte(`{"profiles":[{"id":"agent","name":"Agent","kind":"agent","command":["/bin/echo"],"resume":["/bin/echo","${conversation_id}"]}]}`), 0o600)
	if _, err := h.db.Exec(`UPDATE spawns SET cwd = ?, session_id = 'session:test', effective_args = '{}', block_id = 'block:old', status = 'closed' WHERE spawn_id = ?`, cwd, src.ID); err != nil {
		t.Fatal(err)
	}
	return h
}

// A queued target launches only if its identity is still detached in the generation that was
// admitted. A manual resume in between refuses the target; the frozen list stays.
func TestResumeLaunchRechecksTheIdentity(t *testing.T) {
	h := resumeTestHub(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if _, err := h.db.Exec(`INSERT INTO identities VALUES (?, 'codex', ?, 'detached', 'block:old', '', '', 't0', 't0')`, "codex:"+id, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.adminResume(ctx, map[string]any{"key": "k", "kind": "codex"}); err != nil {
		t.Fatal(err)
	}
	// A manual resume makes a current before the driver runs.
	h.db.Exec(`UPDATE identities SET state = 'current', block_id = 'block:manual', updated = 't1' WHERE identity = 'codex:a'`)
	old := spawnCreate
	defer func() { spawnCreate = old }()
	var creates []string
	spawnCreate = func(_ context.Context, _ *Hub, _ string, req map[string]any, _ any) error {
		creates = append(creates, fmt.Sprint(req))
		return errors.New("injected: no answer")
	}
	if _, err := h.resumeStep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(creates) != 1 {
		t.Fatalf("created %d panes, want 1 (only codex:b)", len(creates))
	}
	r, err := h.adminResume(ctx, map[string]any{"key": "k", "kind": "codex"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, x := range r.(map[string]any)["targets"].([]any) {
		e := x.(map[string]any)
		got[e["agent"].(string)] = fmt.Sprint(e["phase"], " ", e["detail"])
	}
	if len(got) != 2 || !strings.HasPrefix(got["codex:a"], "refused ") || !strings.Contains(got["codex:a"], "changed after admission") ||
		!strings.HasPrefix(got["codex:b"], "unknown ") {
		t.Fatalf("targets: %v", got)
	}
}
