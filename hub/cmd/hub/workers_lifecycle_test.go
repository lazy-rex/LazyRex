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
	s := &spawnRow{ID: "spawn-test", Agent: "spawn:spawn-test", Status: "sending"}
	_, err = db.Exec(`INSERT INTO spawns (spawn_id,caller,key,request,profile,title,agent_id,parent,status,created)
 VALUES ('spawn-test','user','key','{}','agent','Agent 1','spawn:spawn-test','user','sending','2026-10-16T23:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	return &Hub{db: db, ev: ev, idents: map[string]*Agent{}, spawnAddrs: map[string]*spawnAddr{}}, s
}

// An open spawn keeps its pane: a second open with another block is refused, and the same block is
// the same pane (an adoption opened it first).
func TestOpenSpawnKeepsItsPane(t *testing.T) {
	h, s := workerTestHub(t)
	ctx := context.Background()
	if err := h.openSpawn(ctx, s, "window:1", "block:1"); err != nil {
		t.Fatal(err)
	}
	if err := h.openSpawn(ctx, &spawnRow{ID: s.ID, Agent: s.Agent, Status: "sending"}, "window:1", "block:1"); err != nil {
		t.Fatalf("the same block again: %v", err)
	}
	if err := h.openSpawn(ctx, &spawnRow{ID: s.ID, Agent: s.Agent, Status: "sending"}, "window:2", "block:2"); err == nil {
		t.Fatal("an open spawn accepted a new block")
	}
	var block string
	if err := h.db.QueryRow(`SELECT block_id FROM spawns WHERE spawn_id = 'spawn-test'`).Scan(&block); err != nil || block != "block:1" {
		t.Fatalf("block %q (%v)", block, err)
	}
}

// An admitted spawn is listed before Rex answers: hub.workers shows it starting, and hub.agents
// lists its address. hub.changed carries both names at the admission, not only at the outcome.
func TestSpawnAdmissionIsAChange(t *testing.T) {
	h, _ := workerTestHub(t)
	wait := changedNames(t, h)
	h.spawnAdmitted(&spawnRow{ID: "spawn-2", Agent: "spawn:spawn-2"}, Profile{ID: "agent"})
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
