package main

import (
	"context"
	"errors"
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

// The writing checks run also when the file needs no write (lead-c4e 2; codex-27's probe
// TestReviewerSameSourceWriteChecksPin): an update that keeps its source string (the entry is
// already the intended entry), and an add whose rename happened before a crash (the file is the
// stored output). Another writer changes the pin after the lock write: conflict, not reloading.
func TestPluginOpWriteNoChangeChecks(t *testing.T) {
	ctx := context.Background()
	repin := func(h *Hub) {
		l := readTestLock(t, h)
		l.Plugins["github:acme/notes@v1.0.0"] = otherLockEntry()
		writeTestLock(t, h, l)
	}
	t.Run("same source", func(t *testing.T) {
		h, _, _, update := atTestHub(t, "github:acme/notes@v1.0.0")
		if _, err := h.db.Exec(`INSERT INTO plugins (name, desired) VALUES ('notes', 'running')`); err != nil {
			t.Fatal(err)
		}
		op := update("w-same")
		if err := h.opFetch(ctx, op); err != nil || op.Phase != "locking" {
			t.Fatalf("fetch: %+v %v", op, err)
		}
		if err := h.opLock(ctx, op); err != nil || op.Phase != "writing" {
			t.Fatalf("lock: %+v %v", op, err)
		}
		file := managedText(t, h)
		repin(h)
		if err := h.opWrite(ctx, op); err != nil {
			t.Fatal(err)
		}
		if op.Phase != "conflict" || !strings.Contains(op.Error, "the lock changed since the update wrote its pin") || managedText(t, h) != file {
			t.Fatalf("%+v; file %q", op, managedText(t, h))
		}
	})
	t.Run("written before a crash", func(t *testing.T) {
		r, _ := fetchTestRepo(t)
		h := fetchTestHub(t, r)
		op := lockedAdd(t, h, "w-crash", "github:acme/notes@v1.0.0")
		h.pops.fault = func(p string) bool { return p == "plugin-op-after-rename" }
		if err := h.opWrite(ctx, op); !errors.Is(err, errOpCrash) {
			t.Fatalf("the crash after the rename: %v", err)
		}
		h.pops.fault = nil
		file := managedText(t, h)
		repin(h)
		op = opsOf(t, h, op.ID)
		if err := h.opWrite(ctx, op); err != nil {
			t.Fatal(err)
		}
		if op.Phase != "conflict" || !strings.Contains(op.Error, "the lock changed since the add wrote its pin") || managedText(t, h) != file {
			t.Fatalf("%+v; file %q", op, managedText(t, h))
		}
	})
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

// The intended entry must pass the graph (C4 plan r3 A; codex-27's graph gap): also when the
// current file already has it, so that before and after refuse it alike. Another writer puts the
// identical intended entry of an add in the file, with a second entry of the same manifest name;
// or an update keeps its source and another writer removes its dependency. Conflict, and the add
// writes no row.
func TestPluginOpWriteIntendedRefused(t *testing.T) {
	ctx := context.Background()
	t.Run("a written add with a second entry of its name", func(t *testing.T) {
		r, _ := fetchTestRepo(t)
		h := fetchTestHub(t, r)
		op := lockedAdd(t, h, "w-twin", "github:acme/notes@v1.0.0")
		dir := t.TempDir()
		plugDir(t, dir, "other", `{"name":"notes","main":"m.lua"}`, "m.lua")
		file := `{"plugins":[{"id":"notes","source":"github:acme/notes@v1.0.0"},{"id":"other","source":"` + filepath.Join(dir, "other") + `"}]}`
		os.WriteFile(h.cfg.managedPath, []byte(file), 0o600)
		if err := h.opWrite(ctx, op); err != nil {
			t.Fatal(err)
		}
		if op.Phase != "conflict" || !strings.Contains(op.Error, "the add of managed:notes refuses managed:notes") || managedText(t, h) != file || rowCols(t, h, "notes") != nil {
			t.Fatalf("%+v; file %s; row %+v", op, managedText(t, h), rowCols(t, h, "notes"))
		}
	})
	t.Run("a same-source update with its dependency removed", func(t *testing.T) {
		r := newTestRepo(t)
		r.commit(map[string]string{"lazyrex.json": manifestJSON("notes", "dep"), "main.lua": "return 1\n"})
		r.git("tag", "v1.0.0")
		h := fetchTestHub(t, r)
		dir := t.TempDir()
		dep := opsLocal(t, dir, "dep")
		os.WriteFile(h.cfg.managedPath, []byte(dep), 0o600)
		opsLoadEntries(t, h)
		lockedAdd(t, h, "w-seed", "github:acme/notes@v1.0.0") // the lock entry, as a CLI sync gives it
		both := strings.TrimSuffix(dep, "]}") + `,{"id":"notes","source":"github:acme/notes@v1.0.0"}]}`
		os.WriteFile(h.cfg.managedPath, []byte(both), 0o600)
		opsLoadEntries(t, h)
		// desired running with no selected run: not "already at", so the update goes on.
		if _, err := h.db.Exec(`INSERT INTO plugins (name, desired) VALUES ('notes', 'running')`); err != nil {
			t.Fatal(err)
		}
		up, err := resolveOp(t, h, "w-same-dep", opRequest{Op: "update", Entry: "managed:notes", Input: rawJSON(map[string]any{"to": "v1.0.0"})})
		if err != nil || up.Phase != "fetching" {
			t.Fatalf("resolve: %+v %v", up, err)
		}
		if err := h.opFetch(ctx, up); err != nil || up.Phase != "locking" {
			t.Fatalf("fetch: %+v %v", up, err)
		}
		if err := h.opLock(ctx, up); err != nil || up.Phase != "writing" {
			t.Fatalf("lock: %+v %v", up, err)
		}
		file := `{"plugins":[{"id":"notes","source":"github:acme/notes@v1.0.0"}]}`
		os.WriteFile(h.cfg.managedPath, []byte(file), 0o600)
		if err := h.opWrite(ctx, up); err != nil {
			t.Fatal(err)
		}
		if up.Phase != "conflict" || !strings.Contains(up.Error, "the update of managed:notes refuses managed:notes") || managedText(t, h) != file {
			t.Fatalf("%+v; file %s", up, managedText(t, h))
		}
	})
}
