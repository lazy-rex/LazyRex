package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A renderer that exits before it attaches, against a real Rex lab (rexLab: HARNESS_LAB=1). The
// renderer script exits at once; its pane keeps open with no child. panelsLabHub drains Rex's
// events and the test feeds none, so each pass finds the exit before the hub sees child_exited.
// No UI worker runs: the test runs the passes.
func TestUnseenRendererExitLab(t *testing.T) {
	h, _, _, _ := panelsLabHub(t)
	ctx := context.Background()
	view := filepath.Join(t.TempDir(), "crash.sh")
	os.WriteFile(view, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	h.cfg.View = view
	mustExec(t, h, `INSERT INTO ui_tabs (plugin, tab, title, route, pos) VALUES ('p', 'main', 'Main', '/main', 0)`)

	// pass opens a view for the tab when none is open, waits until its renderer has exited, and
	// runs the pass that finds it gone. restart forgets the views of this process first, as a new
	// hub process on the same database has none.
	pass := func(restart bool) {
		t.Helper()
		if _, err := h.reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		var sess, block string
		h.db.QueryRow(`SELECT session_id, block_id FROM views WHERE plugin = 'p' AND status = 'open'`).Scan(&sess, &block)
		if block == "" {
			t.Fatalf("no open view after a pass")
		}
		waitUntilLab(t, "the renderer exit", func() bool {
			pid, err := h.childIn(ctx, sess, block)
			return err == nil && pid == 0
		})
		if restart {
			h.ui.mu.Lock()
			h.ui.own = map[string]bool{}
			h.ui.mu.Unlock()
		}
		if _, err := h.reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// The lead's cases 3 and 4: a dead renderer of an earlier hub process closes with no count, three
	// times, and the tab stays.
	for range 3 {
		pass(true)
	}
	if c, s := tabCrashes(h); c != 0 || s != 0 {
		t.Fatalf("after three restarts: crashes %d, suppressed %d; want 0 and 0", c, s)
	}

	// Case 1: an own view's renderer exit, found by a pass first, counts; three suppress the tab.
	pass(false)
	if c, _ := tabCrashes(h); c != 1 {
		t.Fatalf("an own exit found by a pass: crashes %d, want 1", c)
	}
	pass(false)
	pass(false)
	if c, s := tabCrashes(h); c != maxCrashes || s != 1 {
		t.Fatalf("three own exits: crashes %d, suppressed %d; want %d and 1", c, s, maxCrashes)
	}
	h.reconcile(ctx)
	var open int
	h.db.QueryRow(`SELECT count(*) FROM views WHERE plugin = 'p' AND status = 'open'`).Scan(&open)
	if open != 0 {
		t.Fatalf("a suppressed tab got %d new views", open)
	}
}
