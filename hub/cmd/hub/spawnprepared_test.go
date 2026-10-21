package main

import (
	"context"
	"strings"
	"testing"
)

// insertSpawn adds a spawn row in a state and generation, for the prepared-state tests.
func insertSpawn(t *testing.T, h *Hub, id, status string, gen int64) {
	t.Helper()
	if _, err := h.db.Exec(`INSERT INTO spawns (spawn_id,caller,key,request,profile,title,agent_id,parent,status,created,generation)
 VALUES (?,'user',?,'{}','probe','Probe','','user',?,'2026-10-21T23:00:00Z',?)`, id, "key-"+id, status, gen); err != nil {
		t.Fatal(err)
	}
}

func spawnState(t *testing.T, h *Hub, id string) (status, why string) {
	t.Helper()
	if err := h.db.QueryRow(`SELECT status, error FROM spawns WHERE spawn_id = ?`, id).Scan(&status, &why); err != nil {
		t.Fatal(err)
	}
	return status, why
}

// A hub that stopped after the admission and before the sending transition sent nothing: the next
// hub fails its prepared rows as not created. A prepared row of this generation is in flight, and
// a sending row may have reached Rex: both stay.
func TestSpawnPreparedRecovery(t *testing.T) {
	h, _ := workerTestHub(t)
	h.generation = 7
	insertSpawn(t, h, "old-prepared", "prepared", 6)
	insertSpawn(t, h, "legacy-prepared", "prepared", 0)
	insertSpawn(t, h, "own-prepared", "prepared", 7)
	insertSpawn(t, h, "old-sending", "sending", 6)
	if err := h.failUnsentSpawns(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old-prepared", "legacy-prepared"} {
		if st, why := spawnState(t, h, id); st != "failed" || !strings.HasPrefix(why, "not_created: ") {
			t.Fatalf("%s: %s %q", id, st, why)
		}
	}
	if st, _ := spawnState(t, h, "own-prepared"); st != "prepared" {
		t.Fatalf("own-prepared: %s", st)
	}
	if st, why := spawnState(t, h, "old-sending"); st != "sending" || why != "" {
		t.Fatalf("old-sending: %s %q", st, why)
	}
	// A failed spawn reads as failed; a prepared one as pending.
	s, err := h.spawnByID(context.Background(), "own-prepared")
	if err != nil || s.result()["status"] != "pending" {
		t.Fatalf("own-prepared result: %v %v", s, err)
	}
}

// The sending transition of a window batch is one transaction: when one row is no longer
// prepared, no row becomes sending, and the prepared rows fail as not created.
func TestMarkSendingBatch(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	insertSpawn(t, h, "a", "prepared", 0)
	insertSpawn(t, h, "b", "prepared", 0)
	a, b := &spawnRow{ID: "a", Status: "prepared"}, &spawnRow{ID: "b", Status: "prepared"}
	if err := h.markSending(ctx, a, b); err != nil || a.Status != "sending" || b.Status != "sending" {
		t.Fatalf("markSending: %v %s %s", err, a.Status, b.Status)
	}
	for _, id := range []string{"a", "b"} {
		if st, _ := spawnState(t, h, id); st != "sending" {
			t.Fatalf("%s: %s", id, st)
		}
	}

	insertSpawn(t, h, "c", "prepared", 0)
	insertSpawn(t, h, "d", "open", 0) // moved on: the batch must not send
	c, d := &spawnRow{ID: "c", Status: "prepared"}, &spawnRow{ID: "d", Status: "prepared"}
	err := h.markSending(ctx, c, d)
	if err == nil || !strings.Contains(err.Error(), "spawn d is no longer prepared") {
		t.Fatalf("markSending: %v", err)
	}
	if st, why := spawnState(t, h, "c"); st != "failed" || !strings.HasPrefix(why, "not_created: ") {
		t.Fatalf("c: %s %q", st, why)
	}
	if st, _ := spawnState(t, h, "d"); st != "open" {
		t.Fatalf("d: %s", st)
	}
}

// rejectSending makes the sending transition fail in the database, as a write error would.
func rejectSending(t *testing.T, h *Hub) func() {
	t.Helper()
	if _, err := h.db.Exec(`CREATE TRIGGER reject_sending BEFORE UPDATE OF status ON spawns WHEN NEW.status = 'sending'
 BEGIN SELECT RAISE(FAIL, 'test: sending refused'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := h.db.Exec(`DROP TRIGGER reject_sending`); err != nil {
			t.Fatal(err)
		}
	}
}

// A failed sending transition sends no create request: the spawn fails as not created, and a new
// key spawns normally.
func TestSpawnNotCreatedLab(t *testing.T) {
	r, ctx := labRex(t)
	h, p, _ := labSpawnHub(t, r, ctx)
	real := spawnCreate
	t.Cleanup(func() { spawnCreate = real })
	sent := 0
	spawnCreate = func(ctx context.Context, h *Hub, method string, req map[string]any, out any) error {
		sent++
		return real(ctx, h, method, req, out)
	}
	allow := rejectSending(t, h)
	out, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "probe", "key": "refused"})
	if err != nil || out.(map[string]any)["status"] != "failed" || sent != 0 {
		t.Fatalf("refused: %v %v, %d sent", out, err, sent)
	}
	if why, _ := out.(map[string]any)["error"].(string); !strings.HasPrefix(why, "not_created: ") {
		t.Fatalf("refused error: %q", why)
	}
	// A retry with the same key returns the failed spawn and sends nothing.
	allow()
	again, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "probe", "key": "refused"})
	if err != nil || again.(map[string]any)["status"] != "failed" || sent != 0 {
		t.Fatalf("retry: %v %v, %d sent", again, err, sent)
	}
	// A new key may retry: the hub proved that nothing was created.
	fresh, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "probe", "key": "fresh"})
	if err != nil || fresh.(map[string]any)["status"] != "open" || sent != 1 {
		t.Fatalf("fresh: %v %v, %d sent", fresh, err, sent)
	}
}
