package main

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
)

// The identity reconcile fields of hub.workers (docs/hub.md, hub.workers): identity_id is the
// current identity of an agent spawn's pane, absent when it has none; identity_seen is true once
// the pane has had one, from the durable identity_log. The rows below are the states that
// identity.go writes; the lab (TestIdentityFieldsLab) drives the real reports.
func identityFieldsHub(t *testing.T, attach *atomic.Int32, attachFails bool) *Hub {
	t.Helper()
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(identitySchema); err != nil {
		t.Fatal(err)
	}
	h.ui = &uiState{kick: make(chan struct{}, 1), sess: map[string]string{"s": uiSessionKey}} // no session.attach for "s"
	mustExec(t, h, `UPDATE spawns SET status = 'open', session_id = 's', window_id = 'w', block_id = 'b'`)
	h.rex = replyRex(t, func(method string) (any, string) {
		switch method {
		case "session.attach":
			if attach != nil {
				attach.Add(1)
			}
			if attachFails {
				return nil, "internal"
			}
		case "session.list_blocks":
			return map[string]any{"blocks": []any{map[string]any{"block_id": "b", "window_id": "w"}}}, ""
		case "com.superlogical.terminal.process":
			return map[string]any{"child": map[string]any{"pid": 4242}}, ""
		case "com.superlogical.terminal.program_status":
			return map[string]any{"records": []any{}}, ""
		}
		return map[string]any{}, ""
	})
	return h
}

func readWorker(t *testing.T, h *Hub, spawnID string) map[string]any {
	t.Helper()
	got, _, err := h.workersRead(context.Background(), spawnID)
	if err != nil {
		t.Fatal(err)
	}
	ws := got.(map[string]any)["workers"].([]any)
	if len(ws) != 1 {
		t.Fatalf("workers = %v, want one", ws)
	}
	return ws[0].(map[string]any)
}

func wantFields(t *testing.T, name string, w map[string]any, identity, id any, seen any) {
	t.Helper()
	if w["identity"] != identity || w["identity_id"] != id || w["identity_seen"] != seen {
		t.Errorf("%s: identity %v, identity_id %v, identity_seen %v; want %v, %v, %v",
			name, w["identity"], w["identity_id"], w["identity_seen"], identity, id, seen)
	}
	if _, ok := w["identity_id"]; ok && id == nil {
		t.Errorf("%s: identity_id is present (%v); want absent", name, w["identity_id"])
	}
}

func TestWorkerIdentityFields(t *testing.T) {
	h := identityFieldsHub(t, nil, false)
	pane := func(current string) {
		t.Helper()
		mustExec(t, h, `INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current, reporter_start)
			VALUES ('b', 's', 1, '1.0', 'claude', ?, '') ON CONFLICT (block_id) DO UPDATE SET current = excluded.current`, current)
	}
	logRow := func(id, event string) {
		t.Helper()
		mustExec(t, h, `INSERT INTO identity_log (at, identity, block_id, event, detail) VALUES ('x', ?, 'b', ?, '{}')`, id, event)
	}

	wantFields(t, "before any report", readWorker(t, h, ""), "none", nil, false)
	// Rows of events that do not make an identity current do not count.
	logRow("claude:c0", "renamed")
	logRow("claude:c0", "exited")
	logRow("claude:c0", "aliased")
	wantFields(t, "only other events", readWorker(t, h, ""), "none", nil, false)

	pane("claude:c1")
	logRow("claude:c1", "started")
	wantFields(t, "the first report", readWorker(t, h, ""), "current", "claude:c1", true)
	// A /clear: one report transaction makes the new identity current; no absent step between.
	pane("claude:c2")
	logRow("claude:c2", "cleared")
	wantFields(t, "a /clear", readWorker(t, h, ""), "current", "claude:c2", true)
	// The process ends and the pane stays (identityChildExited), or the conversation resumes in
	// another pane (identityReport): current is "" here.
	pane("")
	wantFields(t, "the process ended", readWorker(t, h, ""), "none", nil, true)
	// A later report in the same pane.
	pane("claude:c2")
	logRow("claude:c2", "resumed")
	wantFields(t, "resumed here", readWorker(t, h, ""), "current", "claude:c2", true)

	// Before the upgrade: a pane whose identity_panes row is gone (closed and recorded) but whose
	// log has a started row. A new Hub on the same database reads the same answer.
	mustExec(t, h, `DELETE FROM identity_panes`)
	wantFields(t, "no pane row, a started log row", readWorker(t, h, ""), "none", nil, true)
	again := &Hub{db: h.db, ev: h.ev, agents: h.agents, ui: h.ui, rex: h.rex}
	wantFields(t, "a new hub on the same database", readWorker(t, again, ""), "none", nil, true)

	// The event list: each event that makes an identity current counts by itself.
	for _, ev := range []string{"started", "cleared", "forked", "resumed"} {
		mustExec(t, h, `DELETE FROM identity_log`)
		logRow("claude:c3", ev)
		wantFields(t, "only "+ev, readWorker(t, h, ""), "none", nil, true)
	}
}

// Every agent spawn has identity_seen, also one with no pane yet; other workers have neither field.
func TestWorkerIdentityFieldsOtherRows(t *testing.T) {
	h := identityFieldsHub(t, nil, false)
	mustExec(t, h, `INSERT INTO identity_log (at, identity, block_id, event, detail) VALUES ('x', 'claude:c1', 'b', 'started', '{}')`)
	for _, st := range []string{"prepared", "sending", "failed"} {
		mustExec(t, h, `UPDATE spawns SET status = ?, block_id = '', session_id = ''`, st)
		w := readWorker(t, h, "")
		if _, ok := w["identity"]; ok {
			t.Errorf("%s: identity = %v, want absent", st, w["identity"])
		}
		wantFields(t, st, w, nil, nil, false)
	}
	mustExec(t, h, `UPDATE spawns SET status = 'open', agent_id = '', session_id = 's', block_id = 'b'`)
	w := readWorker(t, h, "")
	for _, f := range []string{"identity", "identity_id", "identity_seen"} {
		if _, ok := w[f]; ok {
			t.Errorf("a spawn with no agent_id has %s = %v", f, w[f])
		}
	}
}

// A failed read fails the call: never an absent identity_id or a false identity_seen.
func TestWorkerIdentityFieldsReadFails(t *testing.T) {
	h := identityFieldsHub(t, nil, false)
	mustExec(t, h, `DROP TABLE identity_log`)
	if got, _, err := h.workersRead(context.Background(), ""); err == nil {
		t.Errorf("a failed history read gave %v", got)
	}
	h = identityFieldsHub(t, nil, false)
	mustExec(t, h, `DROP TABLE identity_panes`)
	if got, _, err := h.workersRead(context.Background(), ""); err == nil {
		t.Errorf("a failed current read gave %v", got)
	}
}

// hub.workers {spawn_id}: one worker in the same shape; not_found for a closed or an unknown spawn.
// The filter selects before recovery: another worker's broken session is not read.
func TestWorkersSpawnFilter(t *testing.T) {
	var attach atomic.Int32
	h := identityFieldsHub(t, &attach, true)
	mustExec(t, h, `INSERT INTO spawns (spawn_id, caller, key, request, profile, title, agent_id, parent, status, created, session_id, window_id, block_id)
		VALUES ('spawn-other', 'user', 'key2', '{}', 'agent', 'Agent 2', 'agent-2', 'user', 'open', '2026-10-16T23:00:01Z', 's2', 'w2', 'b2')`)
	// Control: without the filter, the other worker's session is attached, and its failure fails
	// the call.
	if _, _, err := h.workersRead(context.Background(), ""); err == nil || attach.Load() == 0 {
		t.Fatalf("the control read: err %v, %d attaches; want an error after an attach", err, attach.Load())
	}
	attach.Store(0)
	w := readWorker(t, h, "spawn-test")
	if w["spawn_id"] != "spawn-test" || attach.Load() != 0 {
		t.Errorf("the filtered read: %v after %d attaches; want spawn-test and none", w["spawn_id"], attach.Load())
	}
	for _, c := range []struct{ id, setup string }{
		{"spawn-none", ""},
		{"spawn-test", `UPDATE spawns SET status = 'closed' WHERE spawn_id = 'spawn-test'`},
	} {
		if c.setup != "" {
			mustExec(t, h, c.setup)
		}
		_, _, err := h.workersRead(context.Background(), c.id)
		var se *storeError
		if !errors.As(err, &se) || se.code != "not_found" {
			t.Errorf("%s: %v, want code not_found", c.id, err)
		}
	}
}

// hub.identify gives spawn_id in the pane of an open agent spawn, for both branches (a current
// identity and a legacy agent), and none in another pane.
func TestIdentifySpawnID(t *testing.T) {
	h := identityFieldsHub(t, nil, false)
	pid := os.Getpid()
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		idents map[string]*Agent
		agents map[string]*Agent
	}{
		{"a current identity", map[string]*Agent{"claude:c1": {ID: "claude:c1", IdState: "current", BlockID: "b", ChildPID: pid, ChildStart: startTime(pid)}}, map[string]*Agent{}},
		{"a legacy agent", map[string]*Agent{}, map[string]*Agent{"agent-1": {ID: "agent-1", BlockID: "b", ChildPID: pid, ChildStart: startTime(pid)}}},
	} {
		h.mu.Lock()
		h.idents, h.agents = c.idents, c.agents
		h.mu.Unlock()
		out, err := h.identify(ctx, pid)
		if err != nil || out["found"] != true || out["spawn_id"] != "spawn-test" {
			t.Errorf("%s, the worker's pane: %v, %v; want spawn_id spawn-test", c.name, out, err)
		}
		for _, other := range []string{
			`UPDATE spawns SET block_id = 'b-other'`,          // another pane
			`UPDATE spawns SET block_id = 'b', agent_id = ''`, // not an agent spawn
			`UPDATE spawns SET agent_id = 'agent-1', status = 'closed'`,
		} {
			mustExec(t, h, other)
			out, err := h.identify(ctx, pid)
			if _, ok := out["spawn_id"]; err != nil || out["found"] != true || ok {
				t.Errorf("%s, after %q: %v, %v; want found and no spawn_id", c.name, other, out, err)
			}
		}
		mustExec(t, h, `UPDATE spawns SET status = 'open', block_id = 'b', agent_id = 'agent-1'`)
	}
	mustExec(t, h, `DROP TABLE spawns`)
	if out, err := h.identify(ctx, pid); err == nil {
		t.Errorf("a failed spawns read gave %v", out)
	}
}
