package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// These tests call the worker entry points against an explicitly selected lab server.
func TestWorkerSessionLab(t *testing.T) {
	socket := os.Getenv("WORKER_LAB_SOCKET")
	if socket == "" {
		t.Skip("set WORKER_LAB_SOCKET to an isolated Rex lab socket")
	}
	if !strings.HasPrefix(socket, "/tmp/rexlab-worker-") || !strings.HasSuffix(socket, "/rex.sock") {
		t.Fatal("refuse a non-lab socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range r.Events {
		}
	}()
	session := func(t *testing.T) string {
		var out struct {
			ID string `json:"session_id"`
		}
		if err := r.Call(ctx, "session.create", map[string]any{"label": "worker-recovery-test"}, &out); err != nil {
			t.Fatal(err)
		}
		if err := r.Call(ctx, "session.attach", map[string]any{"session_id": out.ID, "event_scope": "session"}, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Call(context.Background(), "session.destroy", map[string]any{"session_id": out.ID}, nil) })
		return out.ID
	}
	setup := func(t *testing.T) (*Hub, perform, map[string]any, *spawnRow) {
		h, _ := workerTestHub(t)
		if _, err := h.db.Exec(`DELETE FROM spawns; DELETE FROM bindings`); err != nil {
			t.Fatal(err)
		}
		h.rex = r
		h.ui = &uiState{sess: map[string]string{session(t): uiSessionKey}, kick: make(chan struct{}, 1)}
		h.selfClosed = map[string]bool{}
		h.vs = newViewSvc()
		h.vs.workers["test"] = make(chan struct{}, 1)
		h.plugins = map[string]*Plugin{"test": {Name: "test", ClientID: "client:00000000-0000-0000-0000-000000000001"}}
		h.cfg.Profiles = t.TempDir() + "/profiles.json"
		if err := os.WriteFile(h.cfg.Profiles, []byte(`{"profiles":[{"id":"probe","name":"Probe","kind":"accessory","command":["/bin/sleep","120"]}]}`), 0600); err != nil {
			t.Fatal(err)
		}
		var p perform
		p.Requester.ClientID = "client:00000000-0000-0000-0000-000000000001"
		args := map[string]any{"for": "user", "profile": "probe", "key": "one"}
		out, err := h.spawnWorker(ctx, p, args)
		if err != nil {
			t.Fatal(err)
		}
		s, err := h.spawnByID(ctx, out.(map[string]any)["spawn_id"].(string))
		if err != nil || s.Status != "open" || s.Session != h.workSession() {
			t.Fatalf("spawn: %+v %v", s, err)
		}
		return h, p, args, s
	}
	t.Run("LostSessionAndRetry", func(t *testing.T) {
		h, p, args, s := setup(t)
		if err := r.Call(ctx, "session.destroy", map[string]any{"session_id": s.Session}, nil); err != nil {
			t.Fatal(err)
		}
		replacement := session(t)
		h.ui.sess = map[string]string{replacement: uiSessionKey}
		// No close event reaches this hub. Recovery must use the persisted session.
		if err := h.resolveOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := h.spawnWorker(ctx, p, args)
		if err != nil || got.(map[string]any)["status"] != "closed" {
			t.Fatalf("retry: %v %v", got, err)
		}
		blocks, err := h.blocksIn(ctx, replacement)
		if err != nil || len(blocks) != 0 {
			t.Fatalf("replacement worker started: %v %v", blocks, err)
		}
	})
	t.Run("LivePreviousSessionStop", func(t *testing.T) {
		h, p, _, s := setup(t)
		h.ui.sess = map[string]string{session(t): uiSessionKey}
		if err := r.Call(ctx, "session.detach", map[string]any{"session_id": s.Session}, nil); err != nil {
			t.Fatal(err)
		}
		if err := h.resolveOnce(ctx); err != nil {
			t.Fatal(err)
		}
		current, err := h.spawnByID(ctx, s.ID)
		if err != nil || current.Status != "open" || current.Session != s.Session {
			t.Fatalf("lost ownership: %+v %v", current, err)
		}
		if _, err := h.stop(ctx, p, map[string]any{"for": "user", "spawn_id": s.ID}); err != nil {
			t.Fatal(err)
		}
		blocks, err := h.blocksIn(ctx, s.Session)
		if err != nil || len(blocks) != 0 {
			t.Fatalf("old worker still present: %v %v", blocks, err)
		}
	})
	t.Run("PendingPreviousSession", func(t *testing.T) {
		h, _, _, s := setup(t)
		if _, err := h.db.Exec(`UPDATE spawns SET status='sending',block_id='',window_id='',created='2026-10-16T00:00:00Z'`); err != nil {
			t.Fatal(err)
		}
		h.ui.sess = map[string]string{session(t): uiSessionKey}
		if err := r.Call(ctx, "session.detach", map[string]any{"session_id": s.Session}, nil); err != nil {
			t.Fatal(err)
		}
		if err := h.resolveOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := h.spawnByID(ctx, s.ID)
		if err != nil || got.Status != "sending" || got.Session != s.Session || !strings.Contains(got.Error, "1 candidate") {
			t.Fatalf("pending previous session: %+v %v", got, err)
		}
		blocks, err := h.blocksIn(ctx, s.Session)
		if err != nil || len(blocks) != 1 {
			t.Fatalf("pending process changed: %v %v", blocks, err)
		}
	})

	t.Run("PendingLost", func(t *testing.T) {
		h, _, _, s := setup(t)
		if _, err := h.db.Exec(`UPDATE spawns SET status='sending',block_id='',window_id=''`); err != nil {
			t.Fatal(err)
		}
		if err := r.Call(ctx, "session.destroy", map[string]any{"session_id": s.Session}, nil); err != nil {
			t.Fatal(err)
		}
		h.ui.sess = map[string]string{session(t): uiSessionKey}
		if err := r.Call(ctx, "session.detach", map[string]any{"session_id": s.Session}, nil); err != nil {
			t.Fatal(err)
		}
		if err := h.resolveOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := h.spawnByID(ctx, s.ID)
		if err != nil || got.Status != "failed" {
			t.Fatalf("lost pending: %+v %v", got, err)
		}
	})
	// A row without a session (no hub writes one) is ignored: resolveOnce writes nothing, and a
	// join from the spawn's own pane does not adopt it.
	t.Run("NoSessionIgnored", func(t *testing.T) {
		h, _, _, s := setup(t)
		if _, err := h.db.Exec(`UPDATE spawns SET status='sending',session_id='',block_id='',window_id='',error=''`); err != nil {
			t.Fatal(err)
		}
		if err := h.resolveOnce(ctx); err != nil {
			t.Fatal(err)
		}
		pending, err := h.spawnByID(ctx, s.ID)
		if err != nil || pending.Status != "sending" || pending.Session != "" || pending.Error != "" {
			t.Fatalf("resolveOnce changed a row without a session: %+v %v", pending, err)
		}
		state, err := h.adoptByJoin(ctx, pending, s.Block)
		if err != nil || state != "sending" {
			t.Fatalf("a join adopted a row without a session: %s %v", state, err)
		}
		got, err := h.spawnByID(ctx, s.ID)
		if err != nil || got.Status != "sending" || got.Session != "" || got.Block != "" {
			t.Fatalf("the row changed: %+v %v", got, err)
		}
	})
}

func TestWorkerSessionSchemaMigration(t *testing.T) {
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(`ALTER TABLE spawns DROP COLUMN session_id`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := h.workersSchemaStart(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	s, err := h.spawnByID(context.Background(), "spawn-test")
	if err != nil || s.Session != "" || s.Status != "sending" || s.Agent != "agent-1" {
		t.Fatalf("migration changed identity: %+v %v", s, err)
	}
}
