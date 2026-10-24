package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Bindings (binding.go, lead-bind3): the report rules, the hub start, and the three guarded
// effects. The lab (binding_lab_test.go) drives a restart guard through real Rex.

func paneBinding(t *testing.T, h *Hub, block string) int64 {
	t.Helper()
	var b int64
	if err := h.db.QueryRow(`SELECT binding FROM identity_panes WHERE block_id = ?`, block).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func identityRevision(t *testing.T, h *Hub, id string) int64 {
	t.Helper()
	var r int64
	if err := h.db.QueryRow(`SELECT revision FROM identities WHERE identity = ?`, id).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func listedAgent(t *testing.T, h *Hub, id string) map[string]any {
	t.Helper()
	for _, x := range h.agentsList(map[string]any{"all": true}).(map[string]any)["agents"].([]any) {
		if e := x.(map[string]any); e["id"] == id {
			return e
		}
	}
	t.Fatalf("%s is not listed", id)
	return nil
}

// A repeat report of the same generation keeps the binding; /clear makes a new one and
// supersedes the old identity (its revision moves on); the end of the generation clears it.
// hub.agents lists the binding of a current entry and the revision of a detached one.
func TestBindingReportGenerations(t *testing.T) {
	h, _ := spawnAddrHub(t)
	ctx := context.Background()
	reportAs(t, h, "c1", "startup")
	b1 := paneBinding(t, h, "b")
	if b1 == 0 {
		t.Fatal("no binding after the first report")
	}
	reportAs(t, h, "c1", "prompt")
	if b := paneBinding(t, h, "b"); b != b1 {
		t.Fatalf("a repeat report changed the binding: %d -> %d", b1, b)
	}
	if e := listedAgent(t, h, "claude:c1"); e["binding"] != bindingName(b1) {
		t.Fatalf("hub.agents: %v", e)
	}
	if err := bindingCurrent(ctx, h.db, "claude:c1", b1); err != nil {
		t.Fatal(err)
	}
	rev := identityRevision(t, h, "claude:c1")
	// A row from before the binding column: the next repeat report gives it one, and hub.agents
	// lists it.
	mustExec(t, h, `UPDATE identity_panes SET binding = 0 WHERE block_id = 'b'`)
	reportAs(t, h, "c1", "prompt")
	if b := paneBinding(t, h, "b"); b == 0 || b == b1 || listedAgent(t, h, "claude:c1")["binding"] != bindingName(b) {
		t.Fatalf("a repeat report on a row without a binding: %d, %v", b, listedAgent(t, h, "claude:c1"))
	}
	b1 = paneBinding(t, h, "b")

	reportAs(t, h, "c2", "clear")
	b2 := paneBinding(t, h, "b")
	if b2 == 0 || b2 == b1 {
		t.Fatalf("/clear: binding %d (was %d)", b2, b1)
	}
	wantRefused(t, "the old binding after /clear", bindingCurrent(ctx, h.db, "claude:c1", b1), "not_current")
	wantRefused(t, "the old binding for the new identity", bindingCurrent(ctx, h.db, "claude:c2", b1), "not_current")
	if r := identityRevision(t, h, "claude:c1"); r != rev+1 {
		t.Fatalf("superseded revision %d, want %d", r, rev+1)
	}

	h.identMu.Lock()
	h.identityEndLocked(ctx, "b", "claude:c2", "test")
	h.identMu.Unlock()
	if b := paneBinding(t, h, "b"); b != 0 {
		t.Fatalf("the ended generation keeps binding %d", b)
	}
	wantRefused(t, "after the end", bindingCurrent(ctx, h.db, "claude:c2", b2), "not_current")
	e := listedAgent(t, h, "claude:c2")
	if e["state"] != "detached" || e["binding"] != nil || e["revision"] != identityRevision(t, h, "claude:c2") {
		t.Fatalf("hub.agents after the end: %v", e)
	}
	// A new generation never gets an old number back.
	reportAs(t, h, "c3", "startup")
	if b := paneBinding(t, h, "b"); b <= b2 {
		t.Fatalf("binding %d after %d", b, b2)
	}
}

// A hub start keeps a binding while its process runs (the row survives), gives one to a current
// row from before the column, and gives none to a row whose process is gone. A changed process
// (the pane child, or the CLI under a live shell) makes the binding not current.
func TestBindingAcrossHubStart(t *testing.T) {
	h, _ := spawnAddrHub(t)
	ctx := context.Background()
	reportAs(t, h, "c1", "startup")
	b1 := paneBinding(t, h, "b")
	if err := h.bindingsStart(ctx); err != nil {
		t.Fatal(err)
	}
	if b := paneBinding(t, h, "b"); b != b1 {
		t.Fatalf("the start changed a live binding: %d -> %d", b1, b)
	}

	// A row from before the binding column: binding 0, the process still runs, no new report.
	mustExec(t, h, `UPDATE identity_panes SET binding = 0 WHERE block_id = 'b'`)
	if err := h.bindingsStart(ctx); err != nil {
		t.Fatal(err)
	}
	b2 := paneBinding(t, h, "b")
	if b2 == 0 || b2 == b1 {
		t.Fatalf("the start gave binding %d (old %d)", b2, b1)
	}
	if err := h.identitySync(ctx); err != nil {
		t.Fatal(err)
	}
	if e := listedAgent(t, h, "claude:c1"); e["binding"] != bindingName(b2) {
		t.Fatalf("hub.agents after the start: %v", e)
	}
	if err := bindingCurrent(ctx, h.db, "claude:c1", b2); err != nil {
		t.Fatal(err)
	}

	// The CLI under a live shell exited (its pid is gone): not current, before any loop ran.
	mustExec(t, h, `UPDATE identity_panes SET cli_pid = 2147483000, cli_start = '1.000001' WHERE block_id = 'b'`)
	wantRefused(t, "a gone CLI", bindingCurrent(ctx, h.db, "claude:c1", b2), "not_current")
	// The pane process changed while the hub was down: not current, and the start gives no binding.
	mustExec(t, h, `UPDATE identity_panes SET cli_pid = 0, cli_start = '', child_start = '1.000001', binding = 0 WHERE block_id = 'b'`)
	if err := h.bindingsStart(ctx); err != nil {
		t.Fatal(err)
	}
	if b := paneBinding(t, h, "b"); b != 0 {
		t.Fatalf("a gone process got binding %d", b)
	}
}

// The guard arguments: restart takes binding, resume takes revision, only with agent; malformed
// values are invalid_input.
func TestBindingGuardArguments(t *testing.T) {
	h := resumeTestHub(t)
	ctx := context.Background()
	for _, c := range []struct {
		action string
		args   map[string]any
	}{
		{"restart", map[string]any{"agent": "codex:a", "revision": float64(1)}},
		{"resume", map[string]any{"agent": "codex:a", "binding": "b-1"}},
		{"restart", map[string]any{"kind": "codex", "binding": "b-1"}},
		{"resume", map[string]any{"kind": "codex", "revision": float64(1)}},
		{"restart", map[string]any{"agent": "codex:a", "binding": "1"}},
		{"restart", map[string]any{"agent": "codex:a", "binding": "b-"}},
		{"restart", map[string]any{"agent": "codex:a", "binding": "b-0"}},
		{"restart", map[string]any{"agent": "codex:a", "binding": float64(1)}},
		{"resume", map[string]any{"agent": "codex:a", "revision": float64(-1)}},
		{"resume", map[string]any{"agent": "codex:a", "revision": 1.5}},
		{"resume", map[string]any{"agent": "codex:a", "revision": "1"}},
	} {
		c.args["key"] = "k"
		_, err := h.agentOp(ctx, c.action, c.args, nil)
		wantRefused(t, fmt.Sprint(c.action, " ", c.args), err, "invalid_input")
	}
}

// Resume: the revision is compared in the admission transaction. A stale revision stores
// nothing; a matching one admits and moves the revision on. A retry of the key replays the
// operation before any fresh check, and the same key with another revision is a conflict.
func TestResumeRevisionGuard(t *testing.T) {
	h := resumeTestHub(t)
	ctx := context.Background()
	mustExec(t, h, `INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated, revision)
		VALUES ('codex:a', 'codex', 'a', 'detached', 'block:old', '', '', 't0', 't0', 3)`)
	if e := listedAgentAfterSync(t, h, "codex:a"); e["revision"] != int64(3) {
		t.Fatalf("hub.agents: %v", e)
	}
	_, err := h.agentOp(ctx, "resume", map[string]any{"key": "k", "agent": "codex:a", "revision": float64(2)}, nil)
	wantRefused(t, "a stale revision", err, "not_current")
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM agent_ops WHERE key = 'k'`).Scan(&n)
	if n != 0 {
		t.Fatal("a refused guard stored an operation")
	}
	out, err := h.agentOp(ctx, "resume", map[string]any{"key": "k", "agent": "codex:a", "revision": float64(3)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ph := out.(map[string]any)["targets"].([]any)[0].(map[string]any)["phase"]; ph == "refused" {
		t.Fatalf("the matching revision was refused: %v", out)
	}
	if r := identityRevision(t, h, "codex:a"); r != 4 {
		t.Fatalf("revision %d after the admission, want 4", r)
	}
	if _, err := h.agentOp(ctx, "resume", map[string]any{"key": "k", "agent": "codex:a", "revision": float64(3)}, nil); err != nil {
		t.Fatalf("the replay of the key checked the revision again: %v", err)
	}
	_, err = h.agentOp(ctx, "resume", map[string]any{"key": "k", "agent": "codex:a", "revision": float64(4)}, nil)
	wantRefused(t, "the key with another revision", err, "operation_conflict")
}

func listedAgentAfterSync(t *testing.T, h *Hub, id string) map[string]any {
	t.Helper()
	if err := h.identitySync(context.Background()); err != nil {
		t.Fatal(err)
	}
	return listedAgent(t, h, id)
}

// Restart: a stale binding is refused at admission (nothing stored); a current one is part of the
// stored request. The stop half checks the binding before each key write and signal.
func TestRestartBindingGuard(t *testing.T) {
	h, _ := spawnAddrHub(t)
	h.vs = newViewSvc()
	ctx := context.Background()
	reportAs(t, h, "c1", "startup")
	b1 := paneBinding(t, h, "b")
	_, err := h.agentOp(ctx, "restart", map[string]any{"key": "k", "agent": "claude:c1", "binding": bindingName(b1 + 100)}, nil)
	wantRefused(t, "another binding", err, "not_current")
	if _, err := h.agentOp(ctx, "restart", map[string]any{"key": "k", "agent": "claude:c1", "binding": bindingName(b1)}, nil); err != nil {
		t.Fatalf("the current binding: %v", err)
	}
	_, err = h.agentOp(ctx, "restart", map[string]any{"key": "k", "agent": "claude:c1"}, nil)
	wantRefused(t, "the key without the binding", err, "operation_conflict")

	// The stop half: the binding changed (a new CLI process in the pane) since the admission.
	pid := os.Getpid()
	tgt := &opTarget{identity: "claude:c1", stopBlock: "b", childPID: pid, childStart: startTime(pid), binding: b1}
	mustExec(t, h, `UPDATE identity_panes SET binding = binding + 1000 WHERE block_id = 'b'`)
	if _, why, err := h.restartLook(ctx, tgt); err != nil || !strings.HasPrefix(why, "not_current") {
		t.Fatalf("the stop with a stale binding: %q %v", why, err)
	}
	// /clear: the old identity is no longer the pane's, so its binding is not current.
	reportAs(t, h, "c2", "clear")
	_, err = h.agentOp(ctx, "restart", map[string]any{"key": "k2", "agent": "claude:c1", "binding": bindingName(b1)}, nil)
	wantRefused(t, "after /clear", err, "not_current")
}

// hub.prompt.answer: the binding is checked before the first byte and again before Enter; a stale
// one writes nothing more. It is part of the answer's fingerprint.
func TestPromptAnswerBinding(t *testing.T) {
	h, a, pl, f := answerHub(t)
	ctx := context.Background()
	if err := h.workersSchemaStart(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, h, `INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current, reporter_start, binding)
		VALUES (?, ?, ?, ?, 'claude', 'alice', '', 5)`, a.BlockID, a.Session, a.ChildPID, a.ChildStart)
	ans := func(key string, input map[string]any, binding string) (map[string]any, error) {
		out, err := h.promptAnswer(ctx, pl, map[string]any{"prompt_id": openPrompt(h, "alice").ID, "key": key, "input": input, "binding": binding})
		if err != nil {
			return nil, err
		}
		return out.(map[string]any), nil
	}
	out, err := ans("k1", map[string]any{"keys": "1"}, "b-4")
	if phaseOf(t, out, err) != "refused" || reasonOf(out) != "not_current" || len(f.writes()) != 0 {
		t.Fatalf("a stale binding: %+v, writes %q", out, f.writes())
	}
	if out, err = ans("k2", map[string]any{"keys": "1"}, "b-5"); phaseOf(t, out, err) != "done" {
		t.Fatalf("the current binding: %+v", out)
	}
	if _, err = ans("k2", map[string]any{"keys": "1"}, "b-6"); err == nil || !strings.Contains(err.Error(), "another prompt_id or input") {
		t.Fatalf("the key with another binding: %v", err)
	}
	// The pane gets a new generation after the paste: no Enter.
	f.answer = func(n int, data string) (int, error) {
		if strings.HasPrefix(data, "\x1b[200~") {
			mustExec(t, h, `UPDATE identity_panes SET binding = 6 WHERE current = 'alice'`)
		}
		return len(data), nil
	}
	out, err = ans("k3", map[string]any{"text": "blue", "submit": true}, "b-5")
	r, _ := out["result"].(map[string]any)
	if phaseOf(t, out, err) != "partial" || r["enter"] != "not_sent" || r["reason"] != "not_current" {
		t.Fatalf("a new generation before Enter: %+v", out)
	}
	if w := f.writes(); w[len(w)-1] == "\r" {
		t.Fatalf("Enter was written: %q", w)
	}
	if _, err := h.promptAnswer(ctx, pl, map[string]any{"prompt_id": "x", "key": "k4", "input": map[string]any{"keys": "1"}, "binding": "5"}); err == nil || !strings.Contains(err.Error(), "invalid_input") {
		t.Fatalf("a malformed binding: %v", err)
	}
}

func wantRefused(t *testing.T, what string, err error, code string) {
	t.Helper()
	if codeOf(err) != code {
		t.Fatalf("%s: %v, want %s", what, err, code)
	}
}
