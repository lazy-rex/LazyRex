package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

// adoptRex is a fake Rex for the adoption by identity report: session.list_blocks answers the
// blocks of the session, and fail makes the list_blocks call with that number fail (1 = first).
type adoptRex struct {
	mu     sync.Mutex
	blocks []rexBlock
	fail   map[int]bool
	lists  int
}

func (a *adoptRex) answer(method string, _ json.RawMessage) (any, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if method != "session.list_blocks" {
		return map[string]any{}, ""
	}
	a.lists++
	if a.fail[a.lists] {
		return nil, "failed"
	}
	return map[string]any{"blocks": a.blocks}, ""
}

// adoptTestHub has the agent spawn spawn-test (agent-1) in the given state, in session:s1, and a
// fake Rex with these blocks.
func adoptTestHub(t *testing.T, status string, blocks ...rexBlock) (*Hub, *adoptRex) {
	t.Helper()
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(`UPDATE spawns SET status = ?, session_id = 'session:s1'`, status); err != nil {
		t.Fatal(err)
	}
	ar := &adoptRex{blocks: blocks, fail: map[int]bool{}}
	fr := newPerformRex(t, func(performCall) (any, string) { return nil, "" })
	fr.other = ar.answer
	h.rex = fr.dial(t)
	h.ui = &uiState{sess: map[string]string{"session:s1": uiSessionKey}, kick: make(chan struct{}, 1)}
	h.spawnKick = make(chan struct{}, 1)
	return h, ar
}

func labeled(block, spawn string) rexBlock {
	return rexBlock{BlockID: block, Label: "lazyrex-spawn:" + spawn}
}

func spawnStatus(t *testing.T, h *Hub, id string) (status, block string) {
	t.Helper()
	if err := h.db.QueryRow(`SELECT status, block_id FROM spawns WHERE spawn_id = ?`, id).Scan(&status, &block); err != nil {
		t.Fatal(err)
	}
	return status, block
}

func adoptEvents(t *testing.T, h *Hub) string {
	t.Helper()
	b, err := os.ReadFile(h.ev.f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func kicked(h *Hub) bool {
	select {
	case <-h.spawnKick:
		return true
	default:
		return false
	}
}

// A lost create answer leaves the spawn sending. A report from its labeled pane opens it, with the
// event via identity, and wakes resolveSpawns.
func TestAdoptReportedOpensTheSpawn(t *testing.T) {
	h, _ := adoptTestHub(t, "sending", labeled("block:b1", "spawn-test"))
	if got := h.adoptReported(context.Background(), "session:s1", "block:b1"); got != "agent-1" {
		t.Fatalf("agent = %q, want agent-1", got)
	}
	if st, b := spawnStatus(t, h, "spawn-test"); st != "open" || b != "block:b1" {
		t.Fatalf("spawn: %s %s", st, b)
	}
	if log := adoptEvents(t, h); !strings.Contains(log, `"event":"spawn_adopted"`) || !strings.Contains(log, `"via":"identity"`) {
		t.Fatalf("event log:\n%s", log)
	}
	if !kicked(h) {
		t.Fatal("no kickSpawns after the adoption")
	}
	if h.agents["agent-1"].BlockID != "block:b1" {
		t.Fatal("the agent binding was not written")
	}
}

// No adoption: two panes with the label, an unlabeled pane, an accessory spawn, a prepared spawn.
func TestAdoptReportedRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, status, report string
		blocks               []rexBlock
		accessory            bool
	}{
		{"two labeled panes", "sending", "block:b1", []rexBlock{labeled("block:b1", "spawn-test"), labeled("block:b2", "spawn-test")}, false},
		{"unlabeled pane", "sending", "block:b1", []rexBlock{{BlockID: "block:b1", Label: "work"}}, false},
		{"accessory spawn", "sending", "block:b1", []rexBlock{labeled("block:b1", "spawn-test")}, true},
		{"prepared spawn", "prepared", "block:b1", []rexBlock{labeled("block:b1", "spawn-test")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := adoptTestHub(t, tc.status, tc.blocks...)
			if tc.accessory {
				if _, err := h.db.Exec(`UPDATE spawns SET agent_id = ''`); err != nil {
					t.Fatal(err)
				}
			}
			if got := h.adoptReported(context.Background(), "session:s1", tc.report); got != "" {
				t.Fatalf("agent = %q, want none", got)
			}
			if st, b := spawnStatus(t, h, "spawn-test"); st != tc.status || b != "" {
				t.Fatalf("spawn: %s %s", st, b)
			}
			if strings.Contains(adoptEvents(t, h), "spawn_adopted") || kicked(h) {
				t.Fatal("an adoption event or kick")
			}
		})
	}
}

// A Rex error during the adoption is not the report's error: the spawn stays sending, and the next
// report from the pane adopts it.
func TestAdoptReportedRetriesAfterAnError(t *testing.T) {
	for _, call := range []int{1, 2} { // 1: the label read; 2: the candidate read in adoptSpawn
		h, ar := adoptTestHub(t, "sending", labeled("block:b1", "spawn-test"))
		ar.fail[call] = true
		if got := h.adoptReported(context.Background(), "session:s1", "block:b1"); got != "" {
			t.Fatalf("call %d failed: agent = %q, want none", call, got)
		}
		if st, _ := spawnStatus(t, h, "spawn-test"); st != "sending" {
			t.Fatalf("call %d failed: spawn %s", call, st)
		}
		if got := h.adoptReported(context.Background(), "session:s1", "block:b1"); got != "agent-1" {
			t.Fatalf("call %d failed: the next report: agent = %q", call, got)
		}
		if st, b := spawnStatus(t, h, "spawn-test"); st != "open" || b != "block:b1" {
			t.Fatalf("call %d failed: the next report: %s %s", call, st, b)
		}
	}
}

// The pane's label names the one spawn to check: another sending spawn stays as it is.
func TestAdoptReportedChecksOnlyTheLabeledSpawn(t *testing.T) {
	h, _ := adoptTestHub(t, "sending", labeled("block:b2", "spawn-b"))
	if _, err := h.db.Exec(`INSERT INTO spawns (spawn_id,caller,key,request,profile,title,agent_id,parent,status,created,session_id)
 VALUES ('spawn-b','user','key-b','{}','agent','Agent 2','agent-2','user','sending','2026-10-16T23:00:00Z','session:s1')`); err != nil {
		t.Fatal(err)
	}
	h.agents["agent-2"] = &Agent{ID: "agent-2", Profile: "agent"}
	if got := h.adoptReported(context.Background(), "session:s1", "block:b2"); got != "agent-2" {
		t.Fatalf("agent = %q, want agent-2", got)
	}
	if st, b := spawnStatus(t, h, "spawn-b"); st != "open" || b != "block:b2" {
		t.Fatalf("spawn-b: %s %s", st, b)
	}
	if st, b := spawnStatus(t, h, "spawn-test"); st != "sending" || b != "" {
		t.Fatalf("spawn-test: %s %s", st, b)
	}
}

// A spawn that another opener opened in this pane gives its agent, also before that opener wrote
// the in-memory binding. An open spawn in another block gives none. Neither adopts again.
func TestAdoptReportedOpenSpawn(t *testing.T) {
	h, _ := adoptTestHub(t, "open", labeled("block:b1", "spawn-test"))
	if _, err := h.db.Exec(`UPDATE spawns SET block_id = 'block:b1'`); err != nil {
		t.Fatal(err)
	}
	if got := h.adoptReported(context.Background(), "session:s1", "block:b1"); got != "agent-1" {
		t.Fatalf("agent = %q, want agent-1", got)
	}
	if _, err := h.db.Exec(`UPDATE spawns SET block_id = 'block:b9'`); err != nil {
		t.Fatal(err)
	}
	if got := h.adoptReported(context.Background(), "session:s1", "block:b1"); got != "" {
		t.Fatalf("another block: agent = %q", got)
	}
	if strings.Contains(adoptEvents(t, h), "spawn_adopted") || kicked(h) {
		t.Fatal("an open spawn was adopted again")
	}
}
