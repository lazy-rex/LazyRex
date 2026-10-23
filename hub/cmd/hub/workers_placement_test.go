package main

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

func TestWorkerPlacementValidation(t *testing.T) {
	for _, raw := range []any{nil, "other", map[string]any{"session": false}, map[string]any{"session": ""}, map[string]any{"beside": "b", "direction": "diagonal"}, map[string]any{"beside": "b", "ratio": 0.0}, map[string]any{"beside": "b", "ratio": 1.0}, map[string]any{"beside": "b", "ratio": math.NaN()}, map[string]any{"direction": "right"}, map[string]any{"ratio": 0.5}, map[string]any{"detached": "true"}, map[string]any{"detached": true, "beside": "b"}, map[string]any{"unknown": true}} {
		if _, err := parseWorkerPlacement(raw); err == nil {
			t.Fatalf("accepted %#v", raw)
		}
	}
}

func TestWorkerPlacementLab(t *testing.T) {
	l := rexLab(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) // first, so it runs after every cleanup that uses r
	go func() {
		for range r.Events {
		}
	}()
	session := func(t *testing.T) string {
		var out struct {
			ID string `json:"session_id"`
		}
		if err := r.Call(ctx, "session.create", map[string]any{"label": "worker-placement-test"}, &out); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := r.Call(cleanup, "session.attach", map[string]any{"session_id": out.ID, "event_scope": "session"}, nil)
			if sessionGone(err) {
				return
			}
			if err != nil {
				t.Errorf("cleanup attach: %v", err)
				return
			}
			if err := r.Call(cleanup, "session.destroy", map[string]any{"session_id": out.ID}, nil); err != nil && !sessionGone(err) {
				t.Errorf("cleanup destroy: %v", err)
			}
		})
		return out.ID
	}
	setup := func(t *testing.T) (*Hub, perform) {
		h, _ := workerTestHub(t)
		if _, err := h.db.Exec(`DELETE FROM spawns`); err != nil {
			t.Fatal(err)
		}
		h.rex, h.cfg.Server = r, l.server
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
		return h, p
	}
	spawn := func(t *testing.T, h *Hub, p perform, key string, placement map[string]any) *spawnRow {
		args := map[string]any{"for": "user", "profile": "probe", "key": key}
		if placement != nil {
			args["placement"] = placement
		}
		out, err := h.spawnWorker(ctx, p, args)
		if err != nil {
			t.Fatal(err)
		}
		s, err := h.spawnByID(ctx, out.(map[string]any)["spawn_id"].(string))
		if err != nil || s.Status != "open" {
			t.Fatalf("spawn: %+v %v", s, err)
		}
		return s
	}
	for _, direction := range []string{"right", "left", "above", "below"} {
		t.Run("Split/"+direction, func(t *testing.T) {
			h, p := setup(t)
			target := session(t) // This session is not attached until placement admission.
			anchor := spawn(t, h, p, "anchor", map[string]any{"session": target})
			s := spawn(t, h, p, "split", map[string]any{"session": target, "beside": anchor.Block, "direction": direction, "ratio": 0.3})
			blocks, err := h.blocksIn(ctx, target)
			if err != nil || len(blocks) != 2 || blocks[s.Block].WindowID != anchor.Window || s.Window != anchor.Window || s.Session != target {
				t.Fatalf("split: %+v %v %+v", blocks, err, s)
			}
			var view any
			if err := r.Call(ctx, "session.view", map[string]any{"session_id": target}, &view); err != nil {
				t.Fatal(err)
			}
			ratio := 0.7
			axis := "horizontal"
			if direction == "left" || direction == "above" {
				ratio = 0.3
			}
			if direction == "above" || direction == "below" {
				axis = "vertical"
			}
			if !workerSplitMatches(view, axis, ratio) {
				t.Fatalf("wrong split geometry: %#v", view)
			}
			again := spawn(t, h, p, "split", map[string]any{"session": target, "beside": anchor.Block, "direction": direction, "ratio": 0.3})
			if again.ID != s.ID || again.Block != s.Block {
				t.Fatal("retry changed identity")
			}
			_, err = h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "probe", "key": "split", "placement": map[string]any{"session": target, "detached": true}})
			if err == nil {
				t.Fatal("retry changed placement")
			}
			if _, err = h.stop(ctx, p, map[string]any{"for": "user", "spawn_id": s.ID}); err != nil {
				t.Fatal(err)
			}
			blocks, err = h.blocksIn(ctx, target)
			if err != nil || len(blocks) != 1 || blocks[anchor.Block].BlockID != anchor.Block {
				t.Fatalf("stop changed anchor: %v %v", blocks, err)
			}
		})
	}
	t.Run("Detached", func(t *testing.T) {
		h, p := setup(t)
		target := session(t)
		s := spawn(t, h, p, "detached", map[string]any{"session": target, "detached": true})
		blocks, err := h.blocksIn(ctx, target)
		if err != nil || len(blocks) != 1 || blocks[s.Block].Placement != "detached" || s.Window != "" || s.Session != target {
			t.Fatalf("detached: %+v %v %+v", blocks, err, s)
		}
		if _, err = h.navigateWorker(ctx, map[string]any{"spawn_id": s.ID}); err == nil {
			t.Fatal("detached worker pretended to navigate")
		}
		if _, err = h.stop(ctx, p, map[string]any{"for": "user", "spawn_id": s.ID}); err != nil {
			t.Fatal(err)
		}
		blocks, err = h.blocksIn(ctx, target)
		if err != nil || len(blocks) != 0 {
			t.Fatalf("detached stop: %+v %v", blocks, err)
		}
	})
	t.Run("DefaultAndRefusals", func(t *testing.T) {
		h, p := setup(t)
		anchor := spawn(t, h, p, "default", nil)
		if anchor.Session != h.workSession() || anchor.Window == "" {
			t.Fatalf("default: %+v", anchor)
		}
		split := spawn(t, h, p, "default-split", map[string]any{"beside": anchor.Block})
		if split.Window != anchor.Window {
			t.Fatal("default split created a tab")
		}
		var view any
		if err := r.Call(ctx, "session.view", map[string]any{"session_id": h.workSession()}, &view); err != nil {
			t.Fatal(err)
		}
		if !workerSplitMatches(view, "horizontal", 0.5) {
			t.Fatalf("default split: %+v", view)
		}
		foreign := session(t)
		for _, placement := range []map[string]any{{"session": foreign, "beside": anchor.Block}, {"session": "session:00000000-0000-0000-0000-000000000001"}} {
			if _, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "probe", "key": "refused", "placement": placement}); err == nil {
				t.Fatalf("accepted %v", placement)
			}
		}
		detached := spawn(t, h, p, "detached-anchor", map[string]any{"detached": true})
		if _, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "probe", "key": "refused", "placement": map[string]any{"beside": detached.Block}}); err == nil {
			t.Fatal("split accepted a detached anchor")
		}
		var n int
		if err := h.db.QueryRow(`SELECT COUNT(*) FROM spawns WHERE key='refused'`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("refusal admitted intent: %d %v", n, err)
		}
	})
}

func workerSplitMatches(v any, axis string, ratio float64) bool {
	switch x := v.(type) {
	case map[string]any:
		if n, ok := x["ratio"].(float64); ok && math.Abs(n-ratio) < 0.00001 && x["direction"] == axis {
			return true
		}
		for _, child := range x {
			if workerSplitMatches(child, axis, ratio) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if workerSplitMatches(child, axis, ratio) {
				return true
			}
		}
	}
	return false
}
