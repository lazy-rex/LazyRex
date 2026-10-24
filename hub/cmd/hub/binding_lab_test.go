package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// Bindings through real Rex (docs/hub.md, "Bindings"). A current row from before the binding
// column gets a binding at the hub start; a later start keeps it while the process runs; a stale
// binding or revision is refused with not_current over the wire; a gone process ends the binding.
func TestBindingGuardsLab(t *testing.T) {
	s := newStartLab(t)
	agent := exec.Command("/bin/sleep", "300")
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Process.Kill(); agent.Wait() })

	s.start()
	s.stop()
	db, err := openDB(filepath.Join(s.state(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	pid := agent.Process.Pid
	_, err = db.Exec(`INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current) VALUES ('block:bind-lab', '', ?, ?, 'claude', 'claude:bind');
		INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES ('claude:bind', 'claude', 'bind', 'current', 'block:bind-lab', '', 'Bind', 'x', 'x');
		INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated, revision) VALUES ('claude:old', 'claude', 'old', 'detached', 'block:gone', '', 'Old', 'x', 'x', 7)`, pid, startTime(pid))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	s.start()
	b := s.agentField("claude:bind", "binding")
	if b == nil {
		t.Fatalf("no binding after the start: %v", s.must("hub.agents", map[string]any{"all": true}))
	}
	if r := s.agentField("claude:old", "revision"); r != float64(7) {
		t.Fatalf("the detached revision: %v", r)
	}
	n, _ := parseBinding(b)
	s.wantCode("a stale binding", "not_current", "hub.admin.restart", map[string]any{"key": "lab-r1", "agent": "claude:bind", "binding": bindingName(n + 1)})
	s.wantCode("a malformed binding", "invalid_input", "hub.admin.restart", map[string]any{"key": "lab-r2", "agent": "claude:bind", "binding": "x"})
	s.wantCode("a stale revision", "not_current", "hub.admin.resume", map[string]any{"key": "lab-r3", "agent": "claude:old", "revision": 6})

	// The same process across a hub restart: the same binding.
	s.stop()
	s.start()
	if b2 := s.agentField("claude:bind", "binding"); b2 != b {
		t.Fatalf("the restart changed the binding: %v -> %v", b, b2)
	}

	// The process exits while the hub is down: the start ends the generation, and the binding is refused.
	s.stop()
	agent.Process.Kill()
	agent.Wait()
	s.start()
	if b3 := s.agentField("claude:bind", "binding"); b3 != nil {
		t.Fatalf("a gone process keeps binding %v", b3)
	}
	s.wantCode("the binding of a gone process", "not_current", "hub.admin.restart", map[string]any{"key": "lab-r4", "agent": "claude:bind", "binding": b})
}

func (s *startLab) agentField(id, field string) any {
	s.t.Helper()
	for _, x := range s.must("hub.agents", map[string]any{"all": true})["agents"].([]any) {
		if e := x.(map[string]any); e["id"] == id {
			return e[field]
		}
	}
	s.t.Fatalf("%s is not listed", id)
	return nil
}

func (s *startLab) wantCode(what, code, action string, args map[string]any) {
	s.t.Helper()
	_, err := s.call(action, args)
	var re *rex.RexError
	if !errors.As(err, &re) || re.Code != code {
		s.t.Fatalf("%s: %v, want %s", what, err, code)
	}
	s.t.Logf("%s: %v", what, err)
}
