package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fetchedAdd admits an add of src and runs its resolving and fetching phases: the op is in
// locking, with its target and its pin.
func fetchedAdd(t *testing.T, h *Hub, key, src string) *pluginOp {
	t.Helper()
	op := resolvedAdd(t, h, key, src)
	if err := h.opFetch(context.Background(), op); err != nil || op.Phase != "locking" {
		t.Fatalf("fetch: %+v %v", op, err)
	}
	return op
}

func opLockFile(h *Hub) string { return lockPath(h.cfg.path, h.cfg.Lock) }

// readTestLock reads the hub's lock file.
func readTestLock(t *testing.T, h *Hub) *pluginLock {
	t.Helper()
	l, err := readLock(opLockFile(h))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// writeTestLock writes l as the hub's lock file, as a CLI command does.
func writeTestLock(t *testing.T, h *Hub, l *pluginLock) {
	t.Helper()
	if err := writeLock(opLockFile(h), l); err != nil {
		t.Fatal(err)
	}
}

// otherLockEntry is a lock entry of another source, which no entry names.
func otherLockEntry() *lockEntry {
	return &lockEntry{lockPin: lockPin{Name: "other", Owner: "acme", Repo: "other", Ref: "v9", RefKind: "tag", Commit: strings.Repeat("a", 40), Digest: "sha256:x"}, Dir: "github/acme/other/x"}
}

func lockProofs(t *testing.T, h *Hub, id string) []string {
	t.Helper()
	rows, err := h.db.Query(`SELECT proof FROM plugin_op_attempts WHERE op_id = ? AND call = 'write:lock' ORDER BY rowid`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		out = append(out, p)
	}
	return out
}

func storedTarget(t *testing.T, op *pluginOp) opResolved {
	t.Helper()
	var r opResolved
	if err := json.Unmarshal([]byte(op.Target), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The resolve stores the lock base of the target's key with the target: "absent" for a new key,
// else that key's pin.
func TestPluginOpLockBaseStored(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	op := resolvedAdd(t, h, "b1", "github:acme/notes@v1.0.0")
	if got := storedTarget(t, op).LockBase; got != "absent" {
		t.Fatalf("a new key: base %q", got)
	}
	h2 := fetchTestHub(t, r)
	le := otherLockEntry()
	writeTestLock(t, h2, &pluginLock{Plugins: map[string]*lockEntry{"github:acme/notes@v1.0.0": le}})
	op2 := resolvedAdd(t, h2, "b2", "github:acme/notes@v1.0.0")
	want, _ := json.Marshal(le.lockPin)
	if got := storedTarget(t, op2).LockBase; got != string(want) {
		t.Fatalf("a present key: base %s, want %s", got, want)
	}
}

// The lock write: the op's pin is in the lock, every other entry is unchanged, the attempt is
// applied, and no peers file is written. A replay writes nothing more.
func TestPluginOpLock(t *testing.T) {
	r, commit := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	other := otherLockEntry()
	writeTestLock(t, h, &pluginLock{Plugins: map[string]*lockEntry{"github:acme/other@v9": other}})
	op := fetchedAdd(t, h, "l1", "github:acme/notes@v1.0.0")
	if err := h.opLock(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if op.Phase != "writing" {
		t.Fatalf("after the lock write: %+v", op)
	}
	l := readTestLock(t, h)
	var pin lockEntry
	json.Unmarshal([]byte(op.Pin), &pin)
	got := l.Plugins["github:acme/notes@v1.0.0"]
	if got == nil || *got != pin || got.Commit != commit {
		t.Fatalf("the key's entry %+v, want %+v", got, pin)
	}
	if o := l.Plugins["github:acme/other@v9"]; o == nil || *o != *other || len(l.Plugins) != 2 {
		t.Fatalf("the other entries: %+v", l.Plugins)
	}
	if p := lockProofs(t, h, op.ID); len(p) != 1 || p[0] != "applied: the rename returned" {
		t.Fatalf("attempts %v", p)
	}
	if _, err := os.Stat(opLockFile(h) + ".peers.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a peers file: %v", err)
	}
	before, _ := os.ReadFile(opLockFile(h))
	op.Phase = "locking"
	if err := h.opLock(context.Background(), op); err != nil || op.Phase != "writing" {
		t.Fatalf("the replay: %+v %v", op, err)
	}
	if after, _ := os.ReadFile(opLockFile(h)); string(after) != string(before) || len(lockProofs(t, h, op.ID)) != 1 {
		t.Fatal("the replay wrote the lock again")
	}
}

// A crash before the rename (no sent_at) and after it (before the phase record). The next run
// settles the attempt and does not write old bytes: before the rename it writes once; after the
// rename the file is the output. With another writer's change of another key after the rename,
// the attempt is unresolved, the key's pin is the op's pin, and that change stays.
func TestPluginOpLockCrashRecovery(t *testing.T) {
	for _, c := range []struct {
		point string
		edit  bool
		want  []string // proof prefixes; "" is unresolved
	}{
		{"plugin-op-before-lock-rename", false, []string{"not_applied", "applied: the rename returned"}},
		{"plugin-op-after-lock-rename", false, []string{"applied: the file is the intended output"}},
		{"plugin-op-after-lock-rename", true, []string{""}},
	} {
		t.Run(fmt.Sprintf("%s edit=%v", c.point, c.edit), func(t *testing.T) {
			r, _ := fetchTestRepo(t)
			h := fetchTestHub(t, r)
			op := fetchedAdd(t, h, "c", "github:acme/notes@v1.0.0")
			h.pops.fault = func(p string) bool { return p == c.point }
			if err := h.opLock(context.Background(), op); !errors.Is(err, errOpCrash) {
				t.Fatalf("the crash: %v", err)
			}
			h.pops.fault = nil
			if c.edit {
				l := readTestLock(t, h)
				l.Plugins["github:acme/other@v9"] = otherLockEntry()
				writeTestLock(t, h, l)
			}
			op = opsOf(t, h, op.ID)
			if op.Phase != "locking" {
				t.Fatalf("after the crash: %+v", op)
			}
			if err := h.opLock(context.Background(), op); err != nil || op.Phase != "writing" {
				t.Fatalf("the recovery: %+v %v", op, err)
			}
			l := readTestLock(t, h)
			if l.Plugins["github:acme/notes@v1.0.0"] == nil || c.edit && l.Plugins["github:acme/other@v9"] == nil {
				t.Fatalf("the lock %+v", l.Plugins)
			}
			got := lockProofs(t, h, op.ID)
			if len(got) != len(c.want) {
				t.Fatalf("proofs %q, want %q", got, c.want)
			}
			for i, w := range c.want {
				if w == "" && got[i] != "" || !strings.HasPrefix(got[i], w) {
					t.Fatalf("proofs %q, want %q", got, c.want)
				}
			}
		})
	}
}

// Another writer changed the key's entry after the target was stored: conflict, and the lock is
// unchanged. The same pin from another writer (here, with its own cache directory) counts as
// written, with no write.
func TestPluginOpLockBase(t *testing.T) {
	t.Run("changed", func(t *testing.T) {
		r, _ := fetchTestRepo(t)
		h := fetchTestHub(t, r)
		op := fetchedAdd(t, h, "x", "github:acme/notes@v1.0.0")
		writeTestLock(t, h, &pluginLock{Plugins: map[string]*lockEntry{"github:acme/notes@v1.0.0": otherLockEntry()}})
		before, _ := os.ReadFile(opLockFile(h))
		if err := h.opLock(context.Background(), op); err != nil || op.Phase != "conflict" || !strings.Contains(op.Error, "changed since the operation resolved it") {
			t.Fatalf("op %+v %v", op, err)
		}
		if after, _ := os.ReadFile(opLockFile(h)); string(after) != string(before) || len(lockProofs(t, h, op.ID)) != 0 {
			t.Fatal("the lock was written")
		}
	})
	t.Run("same pin", func(t *testing.T) {
		r, _ := fetchTestRepo(t)
		h := fetchTestHub(t, r)
		op := fetchedAdd(t, h, "y", "github:acme/notes@v1.0.0")
		var pin lockEntry
		json.Unmarshal([]byte(op.Pin), &pin)
		cli := pin
		cli.Dir = "github/acme/notes/cli-dir"
		writeTestLock(t, h, &pluginLock{Plugins: map[string]*lockEntry{"github:acme/notes@v1.0.0": &cli}})
		if err := h.opLock(context.Background(), op); err != nil || op.Phase != "writing" {
			t.Fatalf("op %+v %v", op, err)
		}
		if got := readTestLock(t, h).Plugins["github:acme/notes@v1.0.0"]; got.Dir != cli.Dir || len(lockProofs(t, h, op.ID)) != 0 {
			t.Fatalf("the lock was written: %+v", got)
		}
	})
}

// The op's cache entry does not verify at locking (removed after the fetch): the op fetches again,
// and the lock is not written.
func TestPluginOpLockRefetch(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	op := fetchedAdd(t, h, "f", "github:acme/notes@v1.0.0")
	var pin lockEntry
	json.Unmarshal([]byte(op.Pin), &pin)
	removeTree(filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(pin.Dir)))
	if err := h.opLock(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if stored := opsOf(t, h, op.ID); stored.Phase != "fetching" || stored.Pin != "" || !strings.Contains(stored.Result, "fetching again") {
		t.Fatalf("op %+v", stored)
	}
	if _, err := os.Stat(opLockFile(h)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock was written: %v", err)
	}
}

// A refusal that only the new lock and entries give is conflict, and nothing is written: here the
// added plugin depends on a plugin that no entry gives.
func TestPluginOpLockGraph(t *testing.T) {
	r := newTestRepo(t)
	r.commit(map[string]string{"lazyrex.json": manifestJSON("notes", "tasks"), "main.lua": "return 1\n"})
	r.git("tag", "v1.0.0")
	h := fetchTestHub(t, r)
	op := fetchedAdd(t, h, "g", "github:acme/notes@v1.0.0")
	if err := h.opLock(context.Background(), op); err != nil || op.Phase != "conflict" || !strings.Contains(op.Error, "the new lock refuses managed:notes") {
		t.Fatalf("op %+v %v", op, err)
	}
	if _, err := os.Stat(opLockFile(h)); !errors.Is(err, os.ErrNotExist) || len(lockProofs(t, h, op.ID)) != 0 {
		t.Fatalf("the lock was written: %v", err)
	}
}

// A plugin command holds the flocks: the op waits in locking and writes nothing.
func TestPluginOpLockBusy(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	op := fetchedAdd(t, h, "w", "github:acme/notes@v1.0.0")
	release, err := lockCommands(h.cfg.State, opLockFile(h))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var wait *opWait
	if err := h.opLock(context.Background(), op); !errors.As(err, &wait) || op.Phase != "locking" {
		t.Fatalf("op %+v %v", op, err)
	}
	if _, err := os.Stat(opLockFile(h)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock was written: %v", err)
	}
}

// A failed rename reads the file back to compare its hash, in bounded memory: here a sparse 64 MiB
// file in a read-only directory, so the rename fails. (codex-27, TestReviewerFailedManagedRenameReadBound.)
func TestPluginOpRenameReadBackBounded(t *testing.T) {
	for _, call := range []string{"write:managed", "write:lock"} {
		t.Run(call, func(t *testing.T) {
			h, _, _ := opsTestHub(t, "")
			dir := t.TempDir()
			path := filepath.Join(dir, "file.json")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(64 << 20); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(dir, 0o700)
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			err = h.renameAttempt(context.Background(), &pluginOp{ID: "bound-" + call}, call, path, []byte("{}\n"), "bound")
			runtime.ReadMemStats(&after)
			if err == nil {
				t.Fatal("the rename in a read-only directory did not fail")
			}
			if n := after.TotalAlloc - before.TotalAlloc; n > 8<<20 {
				t.Fatalf("the read-back allocated %d bytes", n)
			}
			var proof string
			h.db.QueryRow(`SELECT proof FROM plugin_op_attempts WHERE op_id = ?`, "bound-"+call).Scan(&proof)
			if !strings.HasPrefix(proof, "not_applied") {
				t.Fatalf("proof %q", proof)
			}
		})
	}
}
