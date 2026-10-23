package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// uiLabHub is a hub on a real Rex lab with the UI worker's state and no plugin. Lab only (rexLab:
// HARNESS_LAB=1). The "renderer" is a shell loop that keeps its arguments (the view ID), so the
// hub's process proofs work.
func uiLabHub(t *testing.T) *Hub {
	t.Helper()
	l := rexLab(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) // first, so it runs after every cleanup that uses r
	h, _ := workerTestHub(t)
	go func() {
		for range r.Events { // the test runs the passes itself; no event handler
		}
	}()
	h.rex, h.cfg.Server = r, l.server
	view := filepath.Join(t.TempDir(), "view.sh")
	os.WriteFile(view, []byte("#!/bin/sh\nwhile :; do sleep 600; done\n"), 0o755)
	h.cfg.View = view
	h.selfClosed = map[string]bool{}
	h.generation++
	if err := h.uiStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.viewsStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var sess string
		h.db.QueryRow(`SELECT session_id FROM ui_sessions WHERE plugin = ?`, uiSessionKey).Scan(&sess)
		if sess != "" {
			r.Call(context.Background(), "session.destroy", map[string]any{"session_id": sess}, nil)
		}
	})
	return h
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timeout: %s", what)
}

// declareThreadTab declares one messaging tab, thread/a.
func declareThreadTab(t *testing.T, h *Hub) {
	t.Helper()
	tabs := []any{map[string]any{"key": "thread/a", "title": "A", "route": "/thread/a"}}
	if _, err := h.uiTabs(context.Background(), "messaging", map[string]any{"tabs": tabs}); err != nil {
		t.Fatal(err)
	}
}

// passes runs n reconcile passes.
func passes(t *testing.T, h *Hub, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := h.reconcile(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
}

// onlyView is the one open messaging view.
func onlyView(t *testing.T, h *Hub) *view {
	t.Helper()
	views, err := h.loadViews(context.Background(), "messaging")
	if err != nil {
		t.Fatal(err)
	}
	var open []*view
	for _, v := range views {
		if v.Status == "open" {
			open = append(open, v)
		}
	}
	if len(open) != 1 {
		t.Fatalf("open views: %d", len(open))
	}
	return open[0]
}
