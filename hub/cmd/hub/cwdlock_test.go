package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hub.cwd.lock: refused while a pending or open worker runs in the path or has an unknown cwd;
// owned by one operation key; a symlink into the tree resolves to the locked path.
func TestCwdLock(t *testing.T) {
	ctx := context.Background()
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(`DELETE FROM spawns`); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	tree := filepath.Join(root, "repo", "feature")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(tree, link); err != nil {
		t.Fatal(err)
	}
	row := func(id, cwd, status string) {
		if _, err := h.db.Exec(`INSERT INTO spawns (spawn_id,caller,key,request,profile,title,parent,status,created,cwd)
			VALUES (?,'user',?,'{}','p','P','user',?,'2026-10-16T23:00:00Z',?)`, id, id, status, cwd); err != nil {
			t.Fatal(err)
		}
	}
	lock := func(owner, key, path string) map[string]any {
		t.Helper()
		resolved, key, err := lockPathArg(map[string]any{"path": path, "key": key})
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		out, err := h.lockPath(ctx, owner, resolved, key, nil)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		return out.(map[string]any)
	}

	row("w-in", filepath.Join(tree, "sub"), "open")
	if got := lock("worktrees", "rm-1", link); got["locked"] != false {
		t.Fatalf("an open worker inside the tree (reached through a symlink) must refuse the lock: %v", got)
	}
	h.db.Exec(`UPDATE spawns SET status = 'closed' WHERE spawn_id = 'w-in'`)
	row("w-legacy", "", "sending")
	if out, _ := h.lockPath(ctx, "worktrees", tree, "rm-0", []any{map[string]any{"block_id": "b"}}); out.(map[string]any)["locked"] != false {
		t.Fatal("a live pane in the path must refuse the lock")
	}
	if got := lock("worktrees", "rm-1", tree); got["locked"] != false {
		t.Fatalf("a pending worker with an unknown cwd must refuse the lock: %v", got)
	}
	h.db.Exec(`UPDATE spawns SET status = 'failed' WHERE spawn_id = 'w-legacy'`)
	row("w-other", filepath.Join(root, "repo"), "open") // the parent directory is not under the tree

	if got := lock("worktrees", "rm-1", link); got["locked"] != true || got["path"] != tree {
		t.Fatalf("lock through the symlink must store the resolved path: %v", got)
	}
	if got := lock("worktrees", "rm-1", tree); got["locked"] != true {
		t.Fatalf("a retry of the same operation must succeed: %v", got)
	}
	if got := lock("worktrees", "rm-2", tree); !strings.Contains(got["error"].(string), "conflict") {
		t.Fatalf("another operation key must conflict: %v", got)
	}
	// Overlap: a parent or a descendant of a held lock conflicts, also for another plugin.
	for _, c := range []struct{ owner, key, path string }{
		{"worktrees", "rm-3", filepath.Join(root, "repo")},
		{"other", "x", filepath.Join(tree, "sub")},
		{"worktrees", "rm-1", filepath.Join(tree, "sub")}, // same operation, different path
	} {
		if got := lock(c.owner, c.key, c.path); got["error"] == nil || !strings.Contains(got["error"].(string), "overlaps") {
			t.Fatalf("lock %v overlaps %s and must conflict: %v", c, tree, got)
		}
	}
	if err := os.MkdirAll(tree+"-other", 0o755); err != nil {
		t.Fatal(err)
	}
	if got := lock("worktrees", "rm-4", tree+"-other"); got["locked"] != true {
		t.Fatalf("a sibling with the same prefix does not overlap: %v", got)
	}
	if _, err := h.cwdUnlock(ctx, "worktrees", false, map[string]any{"path": tree + "-other", "key": "rm-4"}); err != nil {
		t.Fatal(err)
	}
	// Admission: a cwd under the lock, also through the symlink, is covered.
	for _, dir := range []string{tree, filepath.Join(tree, "sub")} {
		if p, _, err := lockedPath(ctx, h.db, dir); err != nil || p != tree {
			t.Fatalf("lockedPath(%s) = %q, %v", dir, p, err)
		}
	}
	if p, _, _ := lockedPath(ctx, h.db, filepath.Join(root, "repo")); p != "" {
		t.Fatal("the parent of a locked path is not locked")
	}
	if p, _, _ := lockedPath(ctx, h.db, tree+"-other"); p != "" {
		t.Fatal("a sibling with the same prefix is not locked")
	}
	if _, err := h.cwdUnlock(ctx, "worktrees", false, map[string]any{"path": tree, "key": "rm-2"}); err == nil {
		t.Fatal("another key must not unlock an operation in progress")
	}
	os.RemoveAll(tree) // the worktree is removed; unlock must still match the stored path
	if _, err := h.cwdUnlock(ctx, "worktrees", false, map[string]any{"path": tree, "key": "rm-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cwdUnlock(ctx, "worktrees", false, map[string]any{"path": tree, "key": "rm-1"}); err != nil {
		t.Fatal("unlock must be idempotent:", err)
	}
	if p, _, _ := lockedPath(ctx, h.db, tree); p != "" {
		t.Fatal("the lock is gone after unlock")
	}
}
