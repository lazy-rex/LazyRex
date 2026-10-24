package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestStartBefore(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"1.000001", "1.000002", true}, {"1.000002", "1.000001", false}, {"9.999999", "10.000000", true}, {"5.000000", "5.000000", false}} {
		if got := startBefore(c.a, c.b); got != c.want {
			t.Errorf("startBefore(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// Identity reports come from the agent hooks only: the MCP shim, the one sender of source "mcp",
// is gone. The hub refuses that source before it looks for the pane.
func TestIdentityReportRefusesSourceMCP(t *testing.T) {
	h, _ := workerTestHub(t)
	_, err := h.identityReport(context.Background(), perform{}, map[string]any{"kind": "codex", "conversation_id": "thread-1", "source": "mcp"})
	if err == nil || !strings.Contains(err.Error(), "invalid: source must be one of startup, resume, clear, compact, fork, prompt") {
		t.Fatalf("source mcp: %v", err)
	}
}

// A pane identity change is a worker change too: hub.workers shows each open agent worker's pane
// identity (workerIdentity). The process exit clears the identity, and hub.changed carries both
// names.
func TestIdentityChangeIsAWorkersChange(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	if err := h.workersSchemaStart(ctx); err != nil {
		t.Fatal(err)
	}
	h.db.Exec(`UPDATE spawns SET status = 'open', block_id = 'block:1', session_id = 'session:1' WHERE spawn_id = 'spawn-test'`)
	h.db.Exec(`INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current) VALUES ('block:1', 'session:1', 0, '', 'claude', 'claude:c1')`)
	h.db.Exec(`INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES ('claude:c1', 'claude', 'c1', 'current', 'block:1', '', '', 'x', 'x')`)
	h.idents = map[string]*Agent{}
	wait := changedNames(t, h)
	h.identityChildExited(ctx, "block:1")
	wait("agents", "workers")
}

// At hub start, a pane whose recorded process is gone loses its current identity (an exit while the
// hub was down); a pane whose recorded process still runs keeps it.
func TestIdentityRecoverEndsGoneProcesses(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	if err := h.workersSchemaStart(ctx); err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	h.db.Exec(`INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current) VALUES ('block:1', 'session:1', ?, 'not-its-start', 'claude', 'claude:gone')`, self)
	h.db.Exec(`INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current) VALUES ('block:2', 'session:1', ?, ?, 'claude', 'claude:live')`, self, startTime(self))
	h.db.Exec(`INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES ('claude:gone', 'claude', 'gone', 'current', 'block:1', '', '', 'x', 'x')`)
	h.db.Exec(`INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES ('claude:live', 'claude', 'live', 'current', 'block:2', '', '', 'x', 'x')`)
	// The live identity is already in memory (a kept binding): the sync reads no program status.
	h.idents = map[string]*Agent{"claude:live": {ID: "claude:live", BlockID: "block:2", ChildPID: self, delivered: map[string]int64{}}}
	if err := h.identityRecover(ctx); err != nil {
		t.Fatal(err)
	}
	for block, want := range map[string]string{"block:1": "", "block:2": "claude:live"} {
		if got, err := h.paneCurrent(ctx, block); err != nil || got != want {
			t.Fatalf("%s: current %q (%v), want %q", block, got, err, want)
		}
	}
	for id, want := range map[string]string{"claude:gone": "detached", "claude:live": "current"} {
		var st string
		if err := h.db.QueryRow(`SELECT state FROM identities WHERE identity = ?`, id).Scan(&st); err != nil || st != want {
			t.Fatalf("%s: state %q (%v), want %s", id, st, err, want)
		}
	}
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM identity_log WHERE identity = 'claude:gone' AND event = 'exited'`).Scan(&n)
	if n != 1 {
		t.Fatalf("exited events for claude:gone = %d, want 1", n)
	}
}

// The recovery ends an identity only on a verified end of its process: no such pid, another start
// time, or a zombie. A failed process read and a pane without a recorded start keep the identity
// (lead-c43rec, codex-24).
func TestIdentityRecoverReadOutcomes(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	if err := h.workersSchemaStart(ctx); err != nil {
		t.Fatal(err)
	}
	const start = "1.000001"
	reads := map[int]func() (procRecord, error){
		101: func() (procRecord, error) { return procRecord{}, errors.New("permission denied") },
		102: func() (procRecord, error) { return procRecord{}, errProcGone },
		103: func() (procRecord, error) { return procRecord{start: start, zombie: true}, nil },
		104: func() (procRecord, error) { return procRecord{start: "2.000002"}, nil },
		105: func() (procRecord, error) { return procRecord{start: start}, nil },
		106: func() (procRecord, error) { return procRecord{start: start}, nil },
	}
	keep := map[int]bool{101: true, 105: true, 106: true}
	h.idents = map[string]*Agent{}
	for pid := range reads {
		id, block, st := fmt.Sprintf("claude:c%d", pid), fmt.Sprintf("block:%d", pid), start
		if pid == 106 {
			st = "" // no recorded start
		}
		h.db.Exec(`INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current) VALUES (?, 'session:1', ?, ?, 'claude', ?)`, block, pid, st, id)
		h.db.Exec(`INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES (?, 'claude', ?, 'current', ?, '', '', 'x', 'x')`, id, fmt.Sprint(pid), block)
		// A kept identity is already in memory: the sync reads no program status.
		h.idents[id] = &Agent{ID: id, BlockID: block, ChildPID: pid, delivered: map[string]int64{}}
	}
	h.procRead = func(pid int) (procRecord, error) { return reads[pid]() }
	if err := h.identityRecover(ctx); err != nil {
		t.Fatal(err)
	}
	for pid := range reads {
		want := ""
		if keep[pid] {
			want = fmt.Sprintf("claude:c%d", pid)
		}
		if got, err := h.paneCurrent(ctx, fmt.Sprintf("block:%d", pid)); err != nil || got != want {
			t.Errorf("pid %d: current %q (%v), want %q", pid, got, err, want)
		}
	}
}

// identitySync frees h.mu for the status reads of new bindings. A program status event in that
// window writes the old record of a kept binding, and the installed identity keeps that write.
func TestIdentitySyncKeepsAStatusWrittenDuringTheSync(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	if err := h.workersSchemaStart(ctx); err != nil {
		t.Fatal(err)
	}
	h.db.Exec(`INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current) VALUES ('block:1', 'session:1', 0, '', 'claude', 'claude:c1')`)
	h.db.Exec(`INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES ('claude:c1', 'claude', 'c1', 'current', 'block:1', '', '', 'x', 'x')`)
	old := &Agent{ID: "claude:c1", BlockID: "block:1", State: "working", Record: statusRecord{State: "working"}, delivered: map[string]int64{}}
	h.idents = map[string]*Agent{"claude:c1": old}
	real := identitySyncGap
	t.Cleanup(func() { identitySyncGap = real })
	identitySyncGap = func() {
		h.mu.Lock()
		old.setRecord(statusRecord{State: "idle"}) // as blockEvent does for program_status_changed
		h.mu.Unlock()
	}
	if err := h.identitySync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.idents["claude:c1"].State; got != "idle" {
		t.Fatalf("state after the sync = %q, want the idle written during it", got)
	}
}
