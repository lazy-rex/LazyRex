package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// hub.cwd.lock asks Rex for every terminal pane's live cwd, in every session: a shell that ran cd
// into the path refuses the lock, though no hub record names it. Lab only (rexLab: HARNESS_LAB=1).
func TestCwdLockLivePanesLab(t *testing.T) {
	l := rexLab(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, l.socket)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range r.Events {
		}
	}()
	h, _ := workerTestHub(t)
	h.db.Exec(`DELETE FROM spawns`)
	h.rex, h.cfg.Server = r, l.server
	h.ui = &uiState{sess: map[string]string{}, kick: make(chan struct{}, 1)}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	tree := filepath.Join(root, "repo", "feature")
	os.MkdirAll(filepath.Join(tree, "sub"), 0o755)
	// A session that the hub does not own, with a shell that starts outside the tree and then runs
	// cd into it (the hub has no record of this pane).
	var s struct {
		ID string `json:"session_id"`
	}
	if err := r.Call(ctx, "session.create", map[string]any{"label": "cwd-live-test"}, &s); err != nil {
		t.Fatal(err)
	}
	defer r.Call(context.Background(), "session.destroy", map[string]any{"session_id": s.ID}, nil)
	r.Call(ctx, "session.attach", map[string]any{"session_id": s.ID, "event_scope": "session"}, nil)
	shell := map[string]any{"flavor": "com.superlogical.terminal.shell", "label": "user shell",
		"options": map[string]any{"cwd": root, "login": false, "command": []string{"/bin/sh", "-c", "cd " + filepath.Join(tree, "sub") + " && exec sleep 60"}}}
	var w struct {
		Blocks []string `json:"block_ids"`
	}
	if err := r.Call(ctx, "session.new_window", map[string]any{"session_id": s.ID, "focus": false, "layout": map[string]any{"block": shell}}, &w); err != nil {
		t.Fatal(err)
	}
	var panes []any
	for i := 0; i < 50; i++ { // the shell needs a moment to cd
		if panes, err = h.liveCwds(ctx, tree); err != nil {
			t.Fatal(err)
		}
		if len(panes) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(panes) != 1 || panes[0].(map[string]any)["block_id"] != w.Blocks[0] {
		t.Fatalf("the shell that ran cd into the tree must be found: %v", panes)
	}
	out, err := h.cwdLock(ctx, "worktrees", map[string]any{"path": tree, "key": "rm-1"})
	if err != nil || out.(map[string]any)["locked"] != false {
		t.Fatalf("a live pane in the tree must refuse the lock: %v %v", out, err)
	}
	if p, _ := h.liveCwds(ctx, filepath.Join(root, "repo", "other")); len(p) != 0 {
		t.Fatalf("a pane in another tree does not count: %v", p)
	}
	r.Call(ctx, "block.close", map[string]any{"session_id": s.ID, "block_id": w.Blocks[0]}, nil)
	out, err = h.cwdLock(ctx, "worktrees", map[string]any{"path": tree, "key": "rm-1"})
	if err != nil || out.(map[string]any)["locked"] != true {
		t.Fatalf("with the pane closed the lock succeeds: %v %v", out, err)
	}
}
