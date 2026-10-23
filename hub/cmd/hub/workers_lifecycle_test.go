package main

import (
	"context"
	"encoding/json"
	"os"
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

// workerGone reports a stored close also when its read of the row afterward fails.
func TestWorkerGoneReportsAStoredCloseWhenTheReadFails(t *testing.T) {
	h, s := workerTestHub(t)
	if _, err := h.db.Exec(`UPDATE spawns SET status = 'open' WHERE spawn_id = 'spawn-test'`); err != nil {
		t.Fatal(err)
	}
	s.Status = "open"
	// The row vanishes after the close, so the read in workerGone finds nothing.
	if _, err := h.db.Exec(`CREATE TRIGGER vanish AFTER UPDATE ON spawns BEGIN DELETE FROM spawns WHERE spawn_id = NEW.spawn_id; END`); err != nil {
		t.Fatal(err)
	}
	wait := changedNames(t, h)
	if err := h.workerGone(context.Background(), s, "pane gone"); err == nil {
		t.Fatal("the read of a vanished row did not fail")
	}
	wait("workers")
}

// A worker row without a recorded session is ignored: no Rex call (this hub has no Rex) and no
// write. Every admission records the session; the live store has no row without one.
func TestWorkerBlocksIgnoresARowWithoutASession(t *testing.T) {
	h, s := workerTestHub(t)
	ctx := context.Background()
	if err := h.workersSchemaStart(ctx); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"sending", "open"} {
		if _, err := h.db.Exec(`UPDATE spawns SET status = ?, session_id = '', error = '' WHERE spawn_id = 'spawn-test'`, status); err != nil {
			t.Fatal(err)
		}
		s.Status, s.Session = status, ""
		blocks, err := h.workerBlocks(ctx, s)
		if blocks != nil || err != nil {
			t.Fatalf("%s: workerBlocks = %v, %v; want nothing", status, blocks, err)
		}
		row, err := h.spawnByID(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != status || row.Error != "" || row.Session != "" {
			t.Fatalf("%s: the row changed: status %q, error %q, session %q", status, row.Status, row.Error, row.Session)
		}
	}
}

// An adoption (a join or an identity report from the pane) can open a spawn while its create
// request is still in flight. The create answer then names the same block: the spawn stays open,
// sendSpawn returns it, and spawn_opened is not written twice. Another block is an error.
func TestSendSpawnAfterAdoption(t *testing.T) {
	for _, tc := range []struct{ name, adopted string }{{"same block", "block:b1"}, {"other block", "block:b2"}} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := workerTestHub(t)
			ctx := context.Background()
			if _, err := h.db.Exec(`UPDATE spawns SET status = 'prepared', session_id = 'session:test'`); err != nil {
				t.Fatal(err)
			}
			s.Status, s.Session = "prepared", "session:test"
			real := spawnCreate
			t.Cleanup(func() { spawnCreate = real })
			spawnCreate = func(ctx context.Context, h *Hub, method string, req map[string]any, out any) error {
				other, err := h.spawnByID(ctx, s.ID)
				if err != nil {
					return err
				}
				if err := h.openSpawn(ctx, other, "window:w1", tc.adopted); err != nil {
					return err
				}
				return json.Unmarshal([]byte(`{"window_id":"window:w1","block_ids":["block:b1"]}`), out)
			}
			err := h.sendSpawn(ctx, s, workerPlacement{}, []string{"/bin/true"})
			stored, rerr := h.spawnByID(ctx, s.ID)
			if rerr != nil || stored.Status != "open" || stored.Block != tc.adopted {
				t.Fatalf("stored: %+v %v", stored, rerr)
			}
			if tc.adopted != "block:b1" {
				if err == nil || !strings.Contains(err.Error(), "no longer sending") {
					t.Fatalf("another block: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("send after adoption: %v", err)
			}
			if r := s.result(); r["status"] != "open" || r["block_id"] != "block:b1" || r["window_id"] != "window:w1" {
				t.Fatalf("result: %v", r)
			}
			log, err := os.ReadFile(h.ev.f.Name())
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(log), `"event":"spawn_opened"`); n != 1 {
				t.Fatalf("spawn_opened written %d times:\n%s", n, log)
			}
		})
	}
}
