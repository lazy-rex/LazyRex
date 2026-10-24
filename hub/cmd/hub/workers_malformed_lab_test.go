package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// labRex starts a lab Rex server (rexLab: LAZYREX_LAB=1) and dials it. It returns the connection,
// a 45 s context, and the server address for the Hub's cfg.Server.
func labRex(t *testing.T) (*rex.Rex, context.Context, string) {
	l := rexLab(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	r, err := rex.Dial(ctx, l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	go func() {
		for range r.Events {
		}
	}()
	return r, ctx, l.server
}

// labSpawnHub is a hub on the lab server with one work session and the accessory profile "probe".
func labSpawnHub(t *testing.T, r *rex.Rex, ctx context.Context, server string) (*Hub, perform, string) {
	var sess struct {
		ID string `json:"session_id"`
	}
	if err := r.Call(ctx, "session.create", map[string]any{"label": "spawn-lab-test"}, &sess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Call(context.Background(), "session.destroy", map[string]any{"session_id": sess.ID}, nil) })
	h, _ := workerTestHub(t)
	h.rex, h.cfg.Server = r, server
	h.ui = &uiState{sess: map[string]string{sess.ID: uiSessionKey}, kick: make(chan struct{}, 1)}
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
	return h, p, sess.ID
}

// A success answer without exactly one block is not a failure: Rex said yes, so a pane may exist.
func TestSpawnMalformedSuccessLab(t *testing.T) {
	r, ctx, server := labRex(t)
	h, p, _ := labSpawnHub(t, r, ctx, server)
	real := spawnCreate
	t.Cleanup(func() { spawnCreate = real })
	garble := func(ctx context.Context, h *Hub, method string, req map[string]any, out any) error {
		if err := real(ctx, h, method, req, out); err != nil { // Rex creates the pane
			return err
		}
		out.(*struct {
			WindowID string   `json:"window_id"`
			BlockIDs []string `json:"block_ids"`
			BlockID  string   `json:"block_id"`
		}).BlockIDs = nil // and the answer is garbled
		return nil
	}
	// Rex answers with an empty block ID, and creates nothing: also unknown, never failed.
	spawnCreate = func(ctx context.Context, h *Hub, method string, req map[string]any, out any) error {
		out.(*struct {
			WindowID string   `json:"window_id"`
			BlockIDs []string `json:"block_ids"`
			BlockID  string   `json:"block_id"`
		}).BlockIDs = []string{""}
		return nil
	}
	if out, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "probe", "key": "empty-id"}); err != nil || out.(map[string]any)["status"] != "pending" {
		t.Fatalf("empty block ID: %v %v", out, err)
	}
	spawnCreate = garble
	out, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "probe", "key": "garbled"})
	if err != nil || out.(map[string]any)["status"] != "pending" {
		t.Fatalf("garbled success: %v %v", out, err)
	}
	s, _ := h.spawnByID(ctx, out.(map[string]any)["spawn_id"].(string))
	if s.Status != "sending" || s.Error != "" {
		t.Fatalf("row: %+v", s)
	}
	// The pane exists, under the spawn's label.
	blocks, err := h.workerBlocks(ctx, s)
	if err != nil || len(spawnCandidates(blocks, s.ID)) != 1 {
		t.Fatalf("candidates: %v %v", blocks, err)
	}
	// A retry with the same key returns the same pending spawn and sends nothing.
	sent := 0
	spawnCreate = func(context.Context, *Hub, string, map[string]any, any) error { sent++; return nil }
	again, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "probe", "key": "garbled"})
	if err != nil || again.(map[string]any)["spawn_id"] != s.ID || sent != 0 {
		t.Fatalf("retry: %v %v sent %d", again, err, sent)
	}
}
