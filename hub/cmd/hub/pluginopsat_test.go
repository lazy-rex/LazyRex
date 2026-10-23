package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// atTestHub is a hub whose lock has github:acme/notes@v1.0.0 at the repository's commit, with a
// cache entry that verifies (an add op's fetch and lock write), and the managed entry notes at
// source src. update admits an update of notes to v1.0.0 and runs its resolving phase.
func atTestHub(t *testing.T, src string) (h *Hub, r *testRepo, commit string, update func(key string) *pluginOp) {
	t.Helper()
	r, commit = fetchTestRepo(t)
	h = fetchTestHub(t, r)
	add := fetchedAdd(t, h, "at-add", "github:acme/notes@v1.0.0")
	if err := h.opLock(context.Background(), add); err != nil || add.Phase != "writing" {
		t.Fatalf("the lock write: %+v %v", add, err)
	}
	if err := os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"notes","source":"`+src+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opsLoadEntries(t, h)
	update = func(key string) *pluginOp {
		t.Helper()
		op, err := resolveOp(t, h, key, opRequest{Op: "update", Entry: "managed:notes", Input: rawJSON(map[string]any{"to": "v1.0.0"})})
		if err != nil || op.Phase != "fetching" {
			t.Fatalf("resolve: %+v %v", op, err)
		}
		return op
	}
	return h, r, commit, update
}

// lockedNotes is the lock entry of github:acme/notes@v1.0.0.
func lockedNotes(t *testing.T, h *Hub) *lockEntry {
	t.Helper()
	le := readTestLock(t, h).Plugins["github:acme/notes@v1.0.0"]
	if le == nil {
		t.Fatal("no lock entry for github:acme/notes@v1.0.0")
	}
	return le
}

// "already at" (C4 plan r2, item 1): the update ends done and writes nothing only when the managed
// source is the target, the lock entry is the target with a directory that verifies, and the
// record is at the target: no row; desired stopped with no instance bound; or desired running with
// the current entry's selected run at the pin, also while it waits or crashes. Every other record
// goes on (here: the fetch skip, so the op is in locking with the lock's entry as its pin).
func TestPluginOpUpdateAlreadyAt(t *testing.T) {
	h, _, commit, update := atTestHub(t, "github:acme/notes@v1.0.0")
	ctx := context.Background()
	le := lockedNotes(t, h)
	_, list, err := readManaged(h.cfg.managedPath)
	if err != nil {
		t.Fatal(err)
	}
	identity := entryIdentity(managedByID(list, "notes"))
	selected := func(entry, commit, digest string) string {
		b, _ := json.Marshal(selectedRun{Entry: entry, Run: pluginRun{Name: "notes", Commit: commit, Digest: digest}})
		return string(b)
	}
	lockBytes, _ := os.ReadFile(opLockFile(h))
	managedBytes, _ := os.ReadFile(h.cfg.managedPath)
	want := fmt.Sprintf("already at github:acme/notes@v1.0.0 %s", commit)
	for i, c := range []struct {
		name                     string
		desired, block, selected string
		pid                      int
		at                       bool
	}{
		{name: "no row", at: true},
		{name: "stopped, no instance", desired: "stopped", at: true},
		{name: "stopped, a block bound", desired: "stopped", block: "b1"},
		{name: "stopped, a child", desired: "stopped", pid: 42},
		{name: "running, no selected run", desired: "running"},
		{name: "running (waiting or crashing), selected at another commit", desired: "running", selected: selected(identity, "c0ffee", le.Digest)},
		{name: "running, selected at another digest", desired: "running", selected: selected(identity, le.Commit, "sha256:other")},
		{name: "running, selected at the pin by another entry", desired: "running", selected: selected(`["managed:old","github:acme/notes@main"]`, le.Commit, le.Digest)},
		{name: "running (waiting or crashing), selected at the pin", desired: "running", selected: selected(identity, le.Commit, le.Digest), at: true},
	} {
		if _, err := h.db.Exec(`DELETE FROM plugins WHERE name = 'notes'`); err != nil {
			t.Fatal(err)
		}
		if c.desired != "" {
			if _, err := h.db.Exec(`INSERT INTO plugins (name, desired, block_id, child_pid, selected_run) VALUES ('notes', ?, ?, ?, ?)`, c.desired, c.block, c.pid, c.selected); err != nil {
				t.Fatal(err)
			}
		}
		op := update(fmt.Sprintf("at-%d", i))
		if err := h.opFetch(ctx, op); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		switch {
		case c.at && (op.Phase != "done" || op.Result != want || op.Pin != ""):
			t.Errorf("%s: %+v, want done %q with no pin", c.name, op, want)
		case !c.at && (op.Phase != "locking" || op.Pin == ""):
			t.Errorf("%s: %+v, want locking with a pin (not already at)", c.name, op)
		}
	}
	if b, _ := os.ReadFile(opLockFile(h)); string(b) != string(lockBytes) {
		t.Error("an already-at check wrote the lock")
	}
	if b, _ := os.ReadFile(h.cfg.managedPath); string(b) != string(managedBytes) {
		t.Error("an already-at check wrote the managed file")
	}
}

// "already at" needs the locked cache entry to verify (codex-27, C4 case 1): with its directory
// gone, the op is not done; it fetches, and goes on to locking.
func TestPluginOpUpdateAlreadyAtMissingDir(t *testing.T) {
	h, _, _, update := atTestHub(t, "github:acme/notes@v1.0.0")
	le := lockedNotes(t, h)
	removeTree(filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(le.Dir)))
	op := update("at-gone")
	if err := h.opFetch(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	var pin lockEntry
	json.Unmarshal([]byte(op.Pin), &pin)
	if op.Phase != "locking" || pin.Commit != le.Commit || pin.Dir == le.Dir {
		t.Fatalf("%+v: want locking with a new fetched entry (not %s)", op, le.Dir)
	}
}

// The fetch skip (step 4 plan r2 B): the lock entry of the target's key is the target and its
// directory verifies, so it becomes the op's pin, and no fetch runs (the remote is gone). With
// another commit in the lock, the op fetches, and the fetch fails at the missing remote.
func TestPluginOpFetchSkip(t *testing.T) {
	h, _, commit, update := atTestHub(t, "github:acme/notes@main")
	le := lockedNotes(t, h)
	op := update("skip-1")
	h.pops.cmd = pluginCmdOpts{remote: func(owner, repo string) string { return "file:///nonexistent/remote" }}
	if err := h.opFetch(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(le)
	if op.Phase != "locking" || op.Pin != string(want) || op.Result != "the lock has github:acme/notes@v1.0.0 at "+commit+"; no fetch" {
		t.Fatalf("%+v, want locking with the lock's entry %s", op, want)
	}
	if stored := opsOf(t, h, op.ID); stored.Pin != op.Pin {
		t.Fatalf("the stored pin %q", stored.Pin)
	}

	h2, r2, _, update2 := atTestHub(t, "github:acme/notes@main")
	l := readTestLock(t, h2)
	old := *l.Plugins["github:acme/notes@v1.0.0"]
	old.Commit = r2.commit(map[string]string{"main.lua": "return 2\n"}) // not the tag's commit
	l.Plugins["github:acme/notes@v1.0.0"] = &old
	writeTestLock(t, h2, l)
	op2 := update2("skip-2")
	h2.pops.cmd = pluginCmdOpts{remote: func(owner, repo string) string { return "file:///nonexistent/remote" }}
	if err := h2.opFetch(context.Background(), op2); err != nil {
		t.Fatal(err)
	}
	if op2.Phase != "failed" || op2.Pin != "" {
		t.Fatalf("another commit in the lock: %+v, want a fetch that fails", op2)
	}
}

// An agent's refresh ("@", B9 removed for update) runs like any update: an entry at a tag that is
// already at its pin ends done in fetching, "already at", and writes nothing.
func TestPluginOpAgentRefreshAlreadyAt(t *testing.T) {
	h, _, commit, _ := atTestHub(t, "github:acme/notes@v1.0.0")
	ctx := context.Background()
	lockBytes, _ := os.ReadFile(opLockFile(h))
	v, err := opsAdmit(t, h, "a1", "refresh", opRequest{Op: "update", Entry: "managed:notes", Input: rawJSON(map[string]any{"to": "@"})})
	if err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, v["op_id"].(string))
	op.Phase = "resolving"
	if err := h.opResolve(ctx, op); err != nil || op.Phase != "fetching" {
		t.Fatalf("the resolve: %+v %v", op, err)
	}
	if err := h.opFetch(ctx, op); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("already at github:acme/notes@v1.0.0 %s", commit); op.Phase != "done" || op.Result != want {
		t.Fatalf("the refresh: %+v, want done %q", op, want)
	}
	if b, _ := os.ReadFile(opLockFile(h)); string(b) != string(lockBytes) {
		t.Error("the refresh wrote the lock")
	}
}
