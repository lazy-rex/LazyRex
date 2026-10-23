package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lab tests of the plugin manager's ops (pluginops.go). Lab only: HARNESS_LAB=1.

// opsAgent opens a test agent pane (lazyrex-hub testagent) that reports the identity "claude:<id>",
// and returns its helper client. The hub sees the pane's real ancestry for its calls.
func (s *startLab) opsAgent(id string) string {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var sess struct {
		SessionID string `json:"session_id"`
	}
	shell := func(cmd []string) map[string]any {
		return map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"command": cmd}}}
	}
	if err := s.l.r.Call(ctx, "session.create", map[string]any{"label": "tests", "initial_windows": []any{
		map[string]any{"window_label": "runner", "layout": shell([]string{"/bin/sleep", "100000"})}}}, &sess); err != nil {
		s.t.Fatal(err)
	}
	if err := s.l.r.Call(ctx, "session.attach", map[string]any{"session_id": sess.SessionID, "event_scope": "session"}, nil); err != nil {
		s.t.Fatal(err)
	}
	if err := s.l.r.Call(ctx, "session.new_window", map[string]any{"session_id": sess.SessionID, "window_label": id,
		"layout": shell([]string{s.bin, "testagent", "-id", id})}, nil); err != nil {
		s.t.Fatal(err)
	}
	var client string
	s.wait("agent "+id+" starts", func() bool {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var err error
		client, err = s.c.resolveClient(c, "lazyrex-testagent:"+id)
		return err == nil
	})
	// The report runs in a new process in the pane, as an agent hook does.
	if _, err := s.c.perform(ctx, client, "agent.report", map[string]any{"args": map[string]any{
		"kind": "claude", "conversation_id": id, "source": "startup"}}); err != nil {
		s.t.Fatalf("agent %s: report: %v", id, err)
	}
	return client
}

// agentDo calls a hub action through the agent face from the agent pane.
func (s *startLab) agentDo(agent, action string, args any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := s.c.perform(ctx, agent, "agent.call", map[string]any{"action": action, "args": args})
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out, err
}

// opDone runs an op from the agent and waits until it ends; it returns the final op.
func (s *startLab) opDone(agent, key string, args map[string]any) map[string]any {
	s.t.Helper()
	args["key"] = key
	op, err := s.agentDo(agent, "hub.plugins.op", args)
	if err != nil {
		s.t.Fatalf("op %s: %v", key, err)
	}
	id, _ := op["op_id"].(string)
	s.wait("op "+key+" ends", func() bool {
		op, err = s.agentDo(agent, "hub.plugins.ops", map[string]any{"op_id": id})
		return err == nil && opFinal[op["phase"].(string)]
	})
	s.t.Logf("op %s: phase %v, result %v, error %v, origin %v", key, op["phase"], op["result"], op["error"], op["origin"])
	return op
}

// An agent runs plugin ops through the agent face against a real hub: a remove with a dependent
// is refused and stops nothing; settings change only the entry; stop, start and remove act. A
// caller outside every pane is refused.
func TestPluginOpsLab(t *testing.T) {
	s := newStartLab(t)
	s.writeSource("m", `{"name":"m","main":"main.lua"}`, "m")
	s.writeSource("d", `{"name":"d","main":"main.lua","depends":["m"]}`, "d")
	s.writeManagedIDs("m=m", "d=d")
	s.start()
	s.registered("m")
	s.registered("d")
	alice := s.opsAgent("alice")
	m0 := s.status("m")
	mStart := startTime(m0.ChildPID)
	managed := filepath.Join(s.l.dir, "hub", "plugins.json")
	file := func() string { b, _ := os.ReadFile(managed); return string(b) }
	before := file()

	// A remove with a dependent: conflict, nothing stops, the file stays.
	op := s.opDone(alice, "rm-m", map[string]any{"op": "remove", "entry": "managed:m"})
	if op["phase"] != "conflict" || !strings.Contains(op["error"].(string), "managed:d") {
		t.Errorf("remove m: %v", op)
	}
	s.same("m", m0, mStart)
	if file() != before {
		t.Errorf("the file changed: %s", file())
	}

	// Settings: the entry changes, the running instance stays (the args apply at the next start).
	op = s.opDone(alice, "set-m", map[string]any{"op": "settings", "entry": "managed:m", "input": map[string]any{"args": map[string]any{"k": "v"}}})
	if op["phase"] != "done" {
		t.Errorf("settings m: %v", op)
	}
	list, err := parseManaged(managed)
	if err != nil || managedByID(list, "m").Args["k"] != "v" || managedByID(list, "d") == nil {
		t.Errorf("after settings: %s %v", file(), err)
	}
	s.same("m", m0, mStart)

	// Stop and start of d; then remove d (stop, unconfigure, reload).
	if op = s.opDone(alice, "stop-d", map[string]any{"op": "stop", "entry": "managed:d"}); op["phase"] != "done" || s.status("d").Registered {
		t.Errorf("stop d: %v %+v", op, s.status("d"))
	}
	if op = s.opDone(alice, "start-d", map[string]any{"op": "start", "entry": "managed:d"}); op["phase"] != "done" {
		t.Errorf("start d: %v", op)
	}
	s.registered("d")
	if op = s.opDone(alice, "rm-d", map[string]any{"op": "remove", "entry": "managed:d"}); op["phase"] != "done" {
		t.Errorf("remove d: %v", op)
	}
	if s.status("d").Registered || s.managedEntry("d") != nil {
		t.Errorf("after remove d: %+v %v", s.status("d"), s.managedEntry("d"))
	}
	if list, _ := parseManaged(managed); managedByID(list, "d") != nil || managedByID(list, "m").Args["k"] != "v" {
		t.Errorf("after remove d: %s", file())
	}
	s.same("m", m0, mStart)

	// The face passes an agent's update with input {to: "@"} to admission (B9 removed for update):
	// a local entry is refused there, not user_only.
	if _, err := s.agentDo(alice, "hub.plugins.op", map[string]any{"key": "up-m", "op": "update", "entry": "managed:m", "input": map[string]any{"to": "@"}}); err == nil || !strings.Contains(err.Error(), "update needs a GitHub source") {
		t.Errorf("an agent update of a local entry: %v", err)
	}

	// The same key with another request; a caller outside every pane.
	if _, err := s.agentDo(alice, "hub.plugins.op", map[string]any{"key": "rm-d", "op": "stop", "entry": "managed:m"}); err == nil || !strings.Contains(err.Error(), `operation_conflict: key "rm-d" has a different operation`) ||
		!strings.Contains(err.Error(), "code=operation_conflict") {
		t.Errorf("a reused key: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	face, err := s.c.resolveClient(ctx, agentLabel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.c.perform(ctx, face, "hub.plugins.op", map[string]any{"key": "x", "op": "stop", "entry": "managed:m"}); err == nil || !strings.Contains(err.Error(), "not a reported conversation") {
		t.Errorf("a caller outside every pane: %v", err)
	}
	if _, err := s.call("hub.plugins.op", map[string]any{"key": "x", "op": "stop", "entry": "managed:m"}); err == nil {
		t.Error("hub.plugins.op on the hub client passed")
	}
}

// liveBlocks counts the live blocks labeled lazyrex-plugin:<name> in every session of the lab: a
// block whose terminal.process has a child. A block that only keeps its log after an exit does not
// count.
func (s *startLab) liveBlocks(name string) int {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var list struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := s.l.r.Call(ctx, "session.list", map[string]any{}, &list); err != nil {
		s.t.Fatal(err)
	}
	n := 0
	for _, se := range list.Sessions {
		s.l.r.Call(ctx, "session.attach", map[string]any{"session_id": se.SessionID, "event_scope": "session"}, nil) // list_blocks needs it
		var bl struct {
			Blocks []rexBlock `json:"blocks"`
		}
		if err := s.l.r.Call(ctx, "session.list_blocks", map[string]any{"session_id": se.SessionID}, &bl); err != nil {
			s.t.Fatalf("session %s: %v", se.SessionID, err)
		}
		for _, b := range bl.Blocks {
			if b.Label != "lazyrex-plugin:"+name {
				continue
			}
			var proc struct {
				Child *struct {
					PID int `json:"pid"`
				} `json:"child"`
			}
			if err := s.l.r.BlockCall(ctx, "com.superlogical.terminal.process", se.SessionID, b.BlockID, nil, &proc); err != nil {
				s.t.Fatalf("block %s: %v", b.BlockID, err)
			}
			if proc.Child != nil {
				n++
			}
		}
	}
	return n
}

// An unproven attempt blocks no start, and a start keeps one live instance: an op on n that ended
// unknown, its start attempt with an "unproven:" outcome (its later observation ended) and its
// spawn intent abandoned are written while the hub is stopped. After the restart, the agent's
// start of managed:n runs, the lab has exactly one live block of n, and the seeded attempt stays
// as written. The add path is not tested here (no add on the agent face, and no lab add until C6);
// the add guard with an unproven attempt is TestPluginOpAddRowGuard. The bound itself is
// TestPluginOpLaterObservationBound.
func TestUnprovenAttemptStartLab(t *testing.T) {
	s := newStartLab(t)
	s.writeSource("n", `{"name":"n","main":"main.lua"}`, "n")
	s.writeManagedIDs("n=n")
	s.start()
	s.registered("n")
	s.must("hub.admin.stop", map[string]any{"plugin": "n"})
	s.wait("no live block of n", func() bool { return s.liveBlocks("n") == 0 })
	s.stop()
	outcome := unprovenPrefix + "error: the start's outcome is unknown: lost"
	db, err := openDB(filepath.Join(s.l.dir, "hub", "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range [][]any{
		{`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated) VALUES ('o-old', 'seed', 'seed', 'f', '{}', 'start', 'managed:n', 'n', '', '', '', 'unknown', 1, 1)`},
		{`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, bound_at, sent_at, intent_id, outcome) VALUES ('a-old', 'o-old', 'start', 1, 't', 't', 'n-0-old', ?)`, outcome},
		{`INSERT INTO spawn_intents (instance_id, plugin, status) VALUES ('n-0-old', 'n', 'abandoned')`},
	} {
		if _, err := db.Exec(q[0].(string), q[1:]...); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	s.start()
	alice := s.opsAgent("alice")
	if op := s.opDone(alice, "start-n", map[string]any{"op": "start", "entry": "managed:n"}); op["phase"] != "done" {
		t.Fatalf("start n: %v", op)
	}
	s.registered("n")
	if n := s.liveBlocks("n"); n != 1 {
		t.Fatalf("%d live blocks of n; want 1", n)
	}
	db, err = openDB(filepath.Join(s.l.dir, "hub", "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var gotOutcome, proof string
	if err := db.QueryRow(`SELECT outcome, proof FROM plugin_op_attempts WHERE attempt = 'a-old'`).Scan(&gotOutcome, &proof); err != nil {
		t.Fatal(err)
	}
	if gotOutcome != outcome || proof != "" {
		t.Errorf("the seeded attempt changed: outcome %q, proof %q", gotOutcome, proof)
	}
}
