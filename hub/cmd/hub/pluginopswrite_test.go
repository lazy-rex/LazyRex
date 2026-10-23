package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lockedAdd admits an add of src and runs it to the writing phase: the lock has its pin.
func lockedAdd(t *testing.T, h *Hub, key, src string) *pluginOp {
	t.Helper()
	op := fetchedAdd(t, h, key, src)
	if err := h.opLock(context.Background(), op); err != nil || op.Phase != "writing" {
		t.Fatalf("the lock write: %+v %v", op, err)
	}
	return op
}

// managedText is the managed file as text ("" when it is missing).
func managedText(t *testing.T, h *Hub) string {
	t.Helper()
	b, err := os.ReadFile(h.cfg.managedPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// managedSource is the source of the managed entry id, or "".
func managedSource(t *testing.T, h *Hub, id string) string {
	t.Helper()
	_, list, err := readManaged(h.cfg.managedPath)
	if err != nil {
		t.Fatal(err)
	}
	if e := managedByID(list, id); e != nil {
		return e.Source
	}
	return ""
}

// The managed write of an add (the entry comes last) and of an update (the new source in place).
// A replay writes nothing more.
func TestPluginOpWriteAddUpdate(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	keep := t.TempDir()
	os.WriteFile(h.cfg.managedPath, []byte(opsLocal(t, keep, "first")), 0o600)
	opsLoadEntries(t, h)
	op := lockedAdd(t, h, "w-add", "github:acme/notes@v1.0.0")
	ctx := context.Background()
	if err := h.opWrite(ctx, op); err != nil || op.Phase != "reloading" {
		t.Fatalf("the add's write: %+v %v", op, err)
	}
	_, list, _ := readManaged(h.cfg.managedPath)
	if len(list) != 2 || list[0].managed != "first" || canonicalEntry(list[1]) != `{"id":"notes","source":"github:acme/notes@v1.0.0","args":{},"watch_roots":[]}` {
		t.Fatalf("the file after the add: %s", managedText(t, h))
	}
	before := managedText(t, h)
	op.Phase = "writing"
	if err := h.opWrite(ctx, op); err != nil || op.Phase != "reloading" || managedText(t, h) != before {
		t.Fatalf("the replay: %+v %v", op, err)
	}

	h2, _, _, update := atTestHub(t, "github:acme/notes@main")
	up := update("w-up")
	if err := h2.opFetch(ctx, up); err != nil || up.Phase != "locking" {
		t.Fatalf("fetch: %+v %v", up, err)
	}
	if err := h2.opLock(ctx, up); err != nil || up.Phase != "writing" {
		t.Fatalf("lock: %+v %v", up, err)
	}
	if err := h2.opWrite(ctx, up); err != nil || up.Phase != "reloading" || managedSource(t, h2, "notes") != "github:acme/notes@v1.0.0" {
		t.Fatalf("the update's write: %+v %v; file %s", up, err, managedText(t, h2))
	}
}

// The pin check at writing (C4 plan r2, 2a): another writer changes the key's pin after the lock
// write. The op ends conflict, and the managed file is unchanged.
func TestPluginOpWritePinChanged(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	op := lockedAdd(t, h, "w-pin", "github:acme/notes@v1.0.0")
	l := readTestLock(t, h)
	l.Plugins["github:acme/notes@v1.0.0"] = otherLockEntry()
	writeTestLock(t, h, l)
	if err := h.opWrite(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if op.Phase != "conflict" || !strings.Contains(op.Error, "the lock changed since the add wrote its pin") || !strings.HasSuffix(op.Error, "; nothing more was written") || managedText(t, h) != "" {
		t.Fatalf("%+v; file %q", op, managedText(t, h))
	}
}

// The directory check at writing (C4 plan r2, 2b; codex-27's case 4): another writer keeps the pin
// and names a cache directory that does not verify. The op goes back to locking, which repairs the
// lock entry; then the write runs.
func TestPluginOpWriteDirInvalid(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	op := lockedAdd(t, h, "w-dir", "github:acme/notes@v1.0.0")
	l := readTestLock(t, h)
	good := *l.Plugins["github:acme/notes@v1.0.0"]
	bad := good
	bad.Dir = "github/acme/notes/" + good.Commit + ".0000000000000000"
	l.Plugins["github:acme/notes@v1.0.0"] = &bad
	writeTestLock(t, h, l)
	ctx := context.Background()
	if err := h.opWrite(ctx, op); err != nil {
		t.Fatal(err)
	}
	if op.Phase != "locking" || !strings.Contains(op.Result, "does not verify") || managedText(t, h) != "" {
		t.Fatalf("an invalid directory: %+v; file %q", op, managedText(t, h))
	}
	if err := h.opLock(ctx, op); err != nil || op.Phase != "writing" || readTestLock(t, h).Plugins["github:acme/notes@v1.0.0"].Dir != good.Dir {
		t.Fatalf("the repair: %+v %v", op, err)
	}
	if err := h.opWrite(ctx, op); err != nil || op.Phase != "reloading" || managedSource(t, h, "notes") != "github:acme/notes@v1.0.0" {
		t.Fatalf("the write after the repair: %+v %v", op, err)
	}
}

// The graph check at writing (C4 plan r2, 2c, with codex-27's correction A): before and after,
// with the current file and lock. Another writer adds an entry that gives the op's name, or
// removes a dependency of the op's entry, after the lock write: conflict, no write.
func TestPluginOpWriteGraph(t *testing.T) {
	ctx := context.Background()
	t.Run("another entry gives the name", func(t *testing.T) {
		r, _ := fetchTestRepo(t)
		h := fetchTestHub(t, r)
		op := lockedAdd(t, h, "w-name", "github:acme/notes@v1.0.0")
		dir := t.TempDir()
		plugDir(t, dir, "other", `{"name":"notes","main":"m.lua"}`, "m.lua")
		file := `{"plugins":[{"id":"other","source":"` + filepath.Join(dir, "other") + `"}]}`
		os.WriteFile(h.cfg.managedPath, []byte(file), 0o600)
		if err := h.opWrite(ctx, op); err != nil {
			t.Fatal(err)
		}
		if op.Phase != "conflict" || !strings.Contains(op.Error, "the add of managed:notes refuses") || managedText(t, h) != file {
			t.Fatalf("%+v", op)
		}
	})
	t.Run("a dependency removed", func(t *testing.T) {
		r := newTestRepo(t)
		r.commit(map[string]string{"lazyrex.json": manifestJSON("notes", "dep"), "main.lua": "return 1\n"})
		r.git("tag", "v1.0.0")
		h := fetchTestHub(t, r)
		dir := t.TempDir()
		os.WriteFile(h.cfg.managedPath, []byte(opsLocal(t, dir, "dep")), 0o600)
		opsLoadEntries(t, h)
		op := lockedAdd(t, h, "w-dep", "github:acme/notes@v1.0.0")
		os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[]}`), 0o600)
		if err := h.opWrite(ctx, op); err != nil {
			t.Fatal(err)
		}
		if op.Phase != "conflict" || !strings.Contains(op.Error, "refuses") || managedText(t, h) != `{"plugins":[]}` {
			t.Fatalf("%+v; file %s", op, managedText(t, h))
		}
	})
	// codex-27's example: an update changes the manifest name a to b. After its lock write, another
	// writer adds c, which depends on a. The update passes for itself, but it would refuse c.
	t.Run("a valid entry would become refused", func(t *testing.T) {
		r := newTestRepo(t)
		r.commit(map[string]string{"lazyrex.json": manifestJSON("a"), "main.lua": "return 1\n"})
		r.git("tag", "v1")
		r.commit(map[string]string{"lazyrex.json": manifestJSON("b")})
		r.git("tag", "v2")
		h := fetchTestHub(t, r)
		lockedAdd(t, h, "w-seed", "github:acme/notes@v1") // the lock entry of @v1, as a CLI sync gives it
		os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"a","source":"github:acme/notes@v1"}]}`), 0o600)
		opsLoadEntries(t, h)
		up, err := resolveOp(t, h, "w-rename", opRequest{Op: "update", Entry: "managed:a", Input: rawJSON(map[string]any{"to": "v2"})})
		if err != nil || up.Phase != "fetching" {
			t.Fatalf("resolve: %+v %v", up, err)
		}
		if err := h.opFetch(ctx, up); err != nil || up.Phase != "locking" {
			t.Fatalf("fetch: %+v %v", up, err)
		}
		if err := h.opLock(ctx, up); err != nil || up.Phase != "writing" {
			t.Fatalf("lock: %+v %v", up, err)
		}
		dir := t.TempDir()
		plugDir(t, dir, "c", manifestJSON("c", "a"), "main.lua")
		file := `{"plugins":[{"id":"a","source":"github:acme/notes@v1"},{"id":"c","source":"` + filepath.Join(dir, "c") + `"}]}`
		os.WriteFile(h.cfg.managedPath, []byte(file), 0o600)
		if err := h.opWrite(ctx, up); err != nil {
			t.Fatal(err)
		}
		if up.Phase != "conflict" || !strings.Contains(up.Error, "the update of managed:a refuses") || managedText(t, h) != file {
			t.Fatalf("%+v; file %s", up, managedText(t, h))
		}
	})
}
