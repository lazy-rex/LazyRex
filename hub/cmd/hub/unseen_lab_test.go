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

	// pass waits until the renderer of the tab's open view has exited (a pass opens one when none is
	// open), then runs the pass that finds it gone. That pass opens the next view. restart runs the
	// startup initializer (uiStart) between the exit and the pass, as a new hub process on the same
	// database: the view is then of an earlier process.
	openViews := func() (n int, sess, block string) {
		h.db.QueryRow(`SELECT count(*), max(session_id), max(block_id) FROM views WHERE plugin = 'p' AND status = 'open'`).Scan(&n, &sess, &block)
		return
	}
	pass := func(restart bool) {
		t.Helper()
		n, sess, block := openViews()
		if n == 0 {
			if _, err := h.reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			n, sess, block = openViews()
		}
		if n != 1 {
			t.Fatalf("%d open views before the pass, want 1", n)
		}
		waitUntilLab(t, "the renderer exit", func() bool {
			pid, err := h.childIn(ctx, sess, block)
			return err == nil && pid == 0
		})
		if restart {
			if err := h.uiStart(ctx); err != nil {
				t.Fatal(err)
			}
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

	// Case 1: an own view's renderer exit, found by a pass first, counts; three suppress the tab. The
	// pass that suppresses it opens no new view.
	pass(false)
	if c, _ := tabCrashes(h); c != 1 {
		t.Fatalf("an own exit found by a pass: crashes %d, want 1", c)
	}
	pass(false)
	pass(false)
	if c, s := tabCrashes(h); c != maxCrashes || s != 1 {
		t.Fatalf("three own exits: crashes %d, suppressed %d; want %d and 1", c, s, maxCrashes)
	}
	if n, _, _ := openViews(); n != 0 {
		t.Fatalf("the pass that suppressed the tab opened %d views", n)
	}
	h.reconcile(ctx)
	if n, _, _ := openViews(); n != 0 {
		t.Fatalf("a suppressed tab got %d new views", n)
	}
}
