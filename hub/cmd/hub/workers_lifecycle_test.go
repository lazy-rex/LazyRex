package main

import (
	"context"
	"strings"
	"testing"
)

func workerTestHub(t *testing.T) (*Hub, *spawnRow) {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(dir + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(workersSchema); err != nil {
		t.Fatal(err)
	}
	ev, err := openEvents(dir + "/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ev.f.Close() })
	s := &spawnRow{ID: "spawn-test", Agent: "agent-1", Status: "sending"}
	_, err = db.Exec(`INSERT INTO spawns (spawn_id,caller,key,request,profile,title,agent_id,parent,status,created)
 VALUES ('spawn-test','user','key','{}','agent','Agent 1','agent-1','user','sending','2026-10-16T23:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	return &Hub{db: db, ev: ev, agents: map[string]*Agent{"agent-1": {ID: "agent-1", Profile: "agent"}}}, s
}

// A failed binding write must not leave a spawn open without its agent binding.
func TestOpenSpawnCommitsBindingAtomically(t *testing.T) {
	h, s := workerTestHub(t)
	ctx := context.Background()
	_, err := h.db.Exec(`CREATE TRIGGER reject_binding BEFORE INSERT ON bindings BEGIN SELECT RAISE(FAIL,'test binding failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.openSpawn(ctx, s, "window:1", "block:1"); err == nil {
		t.Fatal("binding failure was ignored")
	}
	var state, block string
	if err := h.db.QueryRow(`SELECT status,block_id FROM spawns WHERE spawn_id='spawn-test'`).Scan(&state, &block); err != nil {
		t.Fatal(err)
	}
	if state != "sending" || block != "" || h.agents["agent-1"].BlockID != "" {
		t.Fatalf("partial binding: state=%s block=%s", state, block)
	}
	if _, err := h.db.Exec(`DROP TRIGGER reject_binding`); err != nil {
		t.Fatal(err)
	}
	if err := h.openSpawn(ctx, s, "window:1", "block:1"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT block_id FROM bindings WHERE agent='agent-1'`).Scan(&block); err != nil {
		t.Fatal(err)
	}
	if block != "block:1" || h.agents["agent-1"].BlockID != block {
		t.Fatal("missing committed binding")
	}
	if err := h.openSpawn(ctx, s, "window:2", "block:2"); err == nil {
		t.Fatal("an open spawn accepted a new block")
	}
	if err := h.db.QueryRow(`SELECT block_id FROM bindings WHERE agent='agent-1'`).Scan(&block); err != nil {
		t.Fatal(err)
	}
	if block != "block:1" {
		t.Fatal("retry changed identity")
	}
}

// Retired dynamic IDs cannot enroll another pane, including after a hub restart.
func TestRetiredDynamicAgentRefusesJoin(t *testing.T) {
	for _, state := range []string{"failed", "closed"} {
		t.Run(state, func(t *testing.T) {
			h, _ := workerTestHub(t)
			if _, err := h.db.Exec(`UPDATE spawns SET status=?`, state); err != nil {
				t.Fatal(err)
			}
			// A fresh Agent has no ephemeral launch state or block binding.
			_, err := h.join(context.Background(), perform{}, map[string]any{"agent": "agent-1"})
			if err == nil || !strings.Contains(err.Error(), "retired") {
				t.Fatalf("join: %v", err)
			}
		})
	}
}

// An admitted spawn is listed before Rex answers: hub.workers shows it starting, and hub.agents
// lists its agent. hub.changed carries both names at the admission, not only at the outcome.
func TestSpawnAdmissionIsAChange(t *testing.T) {
	h, _ := workerTestHub(t)
	wait := changedNames(t, h)
	h.spawnAdmitted(&spawnRow{ID: "spawn-2", Agent: "agent-2"}, Profile{ID: "agent"}, "Agent 2")
	wait("workers", "agents")
}
