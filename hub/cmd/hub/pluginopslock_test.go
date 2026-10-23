package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// The admission stores the lock base of the add's key: "absent" for a new key, else that key's
// pin.
func TestPluginOpLockBaseStored(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	if got := resolvedAdd(t, h, "b1", "github:acme/notes@v1.0.0").LockBase; got != `{"github:acme/notes@v1.0.0":"absent"}` {
		t.Fatalf("a new key: %s", got)
	}
	h2 := fetchTestHub(t, r)
	le := otherLockEntry()
	writeTestLock(t, h2, &pluginLock{Plugins: map[string]*lockEntry{"github:acme/notes@v1.0.0": le}})
	want, _ := json.Marshal(map[string]string{"github:acme/notes@v1.0.0": lockBase(&pluginLock{Plugins: map[string]*lockEntry{"k": le}}, "k")})
	if got := resolvedAdd(t, h2, "b2", "github:acme/notes@v1.0.0").LockBase; got != string(want) {
		t.Fatalf("a present key: %s, want %s", got, want)
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
// unchanged.
func TestPluginOpLockBase(t *testing.T) {
	t.Run("changed", func(t *testing.T) {
		r, _ := fetchTestRepo(t)
		h := fetchTestHub(t, r)
		op := fetchedAdd(t, h, "x", "github:acme/notes@v1.0.0")
		writeTestLock(t, h, &pluginLock{Plugins: map[string]*lockEntry{"github:acme/notes@v1.0.0": otherLockEntry()}})
		before, _ := os.ReadFile(opLockFile(h))
		if err := h.opLock(context.Background(), op); err != nil || op.Phase != "conflict" || !strings.Contains(op.Error, "the lock changed during the add") {
			t.Fatalf("op %+v %v", op, err)
		}
		if after, _ := os.ReadFile(opLockFile(h)); string(after) != string(before) || len(lockProofs(t, h, op.ID)) != 0 {
			t.Fatal("the lock was written")
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

// The op's pin is already in the lock (another writer put exactly that pin there): the graph check
// still runs before the op goes on. Here the added plugin depends on a plugin that no entry gives,
// so the op is conflict. (codex-27, TestReviewerExistingPinChecksGraph.)
func TestPluginOpLockWrittenChecksGraph(t *testing.T) {
	r := newTestRepo(t)
	r.commit(map[string]string{"lazyrex.json": manifestJSON("notes", "tasks"), "main.lua": "return 1\n"})
	r.git("tag", "v1.0.0")
	h := fetchTestHub(t, r)
	op := fetchedAdd(t, h, "wg", "github:acme/notes@v1.0.0")
	var pin lockEntry
	json.Unmarshal([]byte(op.Pin), &pin)
	writeTestLock(t, h, &pluginLock{Plugins: map[string]*lockEntry{"github:acme/notes@v1.0.0": &pin}})
	if err := h.opLock(context.Background(), op); err != nil || op.Phase != "conflict" || !strings.Contains(op.Error, "the new lock refuses managed:notes") {
		t.Fatalf("op %+v %v", op, err)
	}
	if !strings.Contains(op.Error, "the lock has the operation's pin") || len(lockProofs(t, h, op.ID)) != 0 {
		t.Fatalf("the text or a write: %q", op.Error)
	}
}

// A CLI write of the add's key between admission and the resolve (T2): the lock base is the
// admission snapshot, so the lock write is conflict, and T2 stays. (The hub lead's ruling
// lead-c3dev 1; codex-27's CAS case.)
func TestPluginOpLockBaseAtAdmission(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	v, err := userAdmit(h, "t2", opRequest{Op: "add", Input: rawJSON(map[string]any{"source": "github:acme/notes@v1.0.0"})})
	if err != nil {
		t.Fatal(err)
	}
	t2 := otherLockEntry()
	writeTestLock(t, h, &pluginLock{Plugins: map[string]*lockEntry{"github:acme/notes@v1.0.0": t2}})
	op := opsOf(t, h, v["op_id"].(string))
	op.Phase = "resolving"
	if err := h.opResolve(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if err := h.opFetch(context.Background(), op); err != nil || op.Phase != "locking" {
		t.Fatalf("fetch: %+v %v", op, err)
	}
	if err := h.opLock(context.Background(), op); err != nil || op.Phase != "conflict" || !strings.Contains(op.Error, "the lock changed during the add") {
		t.Fatalf("op %+v %v", op, err)
	}
	if got := readTestLock(t, h).Plugins["github:acme/notes@v1.0.0"]; got == nil || *got != *t2 {
		t.Fatalf("T2 was overwritten: %+v", got)
	}
}

// An update to a tag snapshots both spellings of the new key at admission. The lock write uses the
// snapshot of the key that the resolve selected: a CLI write of that key after admission is
// conflict. Both resolution choices: @<tag>, and @refs/tags/<tag> when a branch has the same name.
func TestPluginOpLockBaseBothSpellings(t *testing.T) {
	for _, branch := range []bool{false, true} {
		t.Run(fmt.Sprintf("branch=%v", branch), func(t *testing.T) {
			r, _ := fetchTestRepo(t)
			r.commit(map[string]string{"main.lua": "return 2\n"})
			r.git("tag", "v1.1.0")
			key := "github:acme/notes@v1.1.0"
			if branch {
				r.git("branch", "v1.1.0")
				key = "github:acme/notes@refs/tags/v1.1.0"
			}
			h := fetchTestHub(t, r)
			writeManaged(t, h.cfg.State, `{"plugins":[{"id":"notes","source":"github:acme/notes@v1.0.0"}]}`)
			opsLoadEntries(t, h)
			v, err := userAdmit(h, "u", opRequest{Op: "update", Entry: "managed:notes", Input: rawJSON(map[string]any{"to": "v1.1.0"})})
			if err != nil {
				t.Fatal(err)
			}
			op := opsOf(t, h, v["op_id"].(string))
			var snap map[string]string
			if err := json.Unmarshal([]byte(op.LockBase), &snap); err != nil || len(snap) != 3 || snap["github:acme/notes@v1.0.0"] != "absent" || snap["github:acme/notes@v1.1.0"] != "absent" || snap["github:acme/notes@refs/tags/v1.1.0"] != "absent" {
				t.Fatalf("the admission snapshot %q %v", op.LockBase, err)
			}
			cli := otherLockEntry()
			writeTestLock(t, h, &pluginLock{Plugins: map[string]*lockEntry{key: cli}})
			op.Phase = "resolving"
			if err := h.opResolve(context.Background(), op); err != nil {
				t.Fatal(err)
			}
			if got := storedTarget(t, op).Source; got != key {
				t.Fatalf("the resolve chose %s, want %s", got, key)
			}
			if err := h.opFetch(context.Background(), op); err != nil || op.Phase != "locking" {
				t.Fatalf("fetch: %+v %v", op, err)
			}
			if err := h.opLock(context.Background(), op); err != nil || op.Phase != "conflict" || !strings.Contains(op.Error, "the lock changed during the update") {
				t.Fatalf("op %+v %v", op, err)
			}
			if got := readTestLock(t, h).Plugins[key]; got == nil || *got != *cli {
				t.Fatalf("the CLI write was overwritten: %+v", got)
			}
		})
	}
}

// The admission snapshot is stored with the op: an idempotent replay of the request (the same key)
// keeps the first snapshot, also when the lock changed since.
func TestPluginOpLockBaseReplay(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	req := opRequest{Op: "add", Input: rawJSON(map[string]any{"source": "github:acme/notes@v1.0.0"})}
	v, err := userAdmit(h, "rp", req)
	if err != nil {
		t.Fatal(err)
	}
	first := opsOf(t, h, v["op_id"].(string)).LockBase
	writeTestLock(t, h, &pluginLock{Plugins: map[string]*lockEntry{"github:acme/notes@v1.0.0": otherLockEntry()}})
	v2, err := userAdmit(h, "rp", req)
	if err != nil || v2["op_id"] != v["op_id"] {
		t.Fatalf("the replay: %v %v", v2, err)
	}
	if got := opsOf(t, h, v["op_id"].(string)).LockBase; got != first || !strings.Contains(first, `"absent"`) {
		t.Fatalf("the snapshot %q, first %q", got, first)
	}
}

// The lock already has the op's pin with another writer's cache directory (R2b, lead-c3final): the
// op verifies that retained directory before it goes on.
//   - valid (a verified copy of the entry): written, no write;
//   - missing, or a changed file (a digest mismatch): the op writes its own verified entry (the same
//     pin), and the lock then names a directory that verifies;
//   - a crash after the repair rename (of a missing directory): the recovery sees its own write and
//     writes no second time.
func TestPluginOpLockRetainedDir(t *testing.T) {
	for _, c := range []string{"valid", "missing", "changed", "crash after repair"} {
		t.Run(c, func(t *testing.T) {
			r, _ := fetchTestRepo(t)
			h := fetchTestHub(t, r)
			op := fetchedAdd(t, h, "rd", "github:acme/notes@v1.0.0")
			var pin lockEntry
			json.Unmarshal([]byte(op.Pin), &pin)
			cli := pin
			cli.Dir = "github/acme/notes/" + pin.Commit + ".c1c1000000000000"
			own := filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(pin.Dir))
			other := filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(cli.Dir))
			if c == "valid" || c == "changed" {
				if out, err := exec.Command("cp", "-Rp", own, other).CombinedOutput(); err != nil {
					t.Fatalf("copy: %v %s", err, out)
				}
			}
			if c == "changed" {
				os.Chmod(other, 0o755)
				os.Chmod(filepath.Join(other, "main.lua"), 0o644)
				if err := os.WriteFile(filepath.Join(other, "main.lua"), []byte("return 3\n"), 0o444); err != nil {
					t.Fatal(err)
				}
			}
			writeTestLock(t, h, &pluginLock{Plugins: map[string]*lockEntry{"github:acme/notes@v1.0.0": &cli}})
			if c == "crash after repair" {
				h.pops.fault = func(p string) bool { return p == "plugin-op-after-lock-rename" }
				if err := h.opLock(context.Background(), op); !errors.Is(err, errOpCrash) {
					t.Fatalf("the crash: %v", err)
				}
				h.pops.fault = nil
				op = opsOf(t, h, op.ID)
			}
			if err := h.opLock(context.Background(), op); err != nil || op.Phase != "writing" {
				t.Fatalf("op %+v %v", op, err)
			}
			got := readTestLock(t, h).Plugins["github:acme/notes@v1.0.0"]
			attempts := len(lockProofs(t, h, op.ID))
			e := &configEntry{Source: "github:acme/notes@v1.0.0"}
			e.checkSource("")
			if _, _, err := lockedDir(h.cfg.path, h.cfg.State, readTestLock(t, h), e); err != nil {
				t.Fatalf("the lock entry does not verify: %v", err)
			}
			switch c {
			case "valid":
				if got.Dir != cli.Dir || attempts != 0 {
					t.Fatalf("a write: %+v, %d attempts", got, attempts)
				}
			default:
				if got.Dir != pin.Dir || attempts != 1 {
					t.Fatalf("no repair: %+v, %d attempts", got, attempts)
				}
			}
		})
	}
}

// The read-back after a failed write:managed rename reads at most managedFileLimit+1 bytes (the
// lead, lead-c3final): here the managed path is a symlink to an endless file, and the read-back
// returns, not_applied.
func TestPluginOpManagedReadBackLimit(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	if err := os.Symlink("/dev/zero", path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	done := make(chan error, 1)
	go func() {
		done <- h.renameAttempt(context.Background(), &pluginOp{ID: "limit"}, "write:managed", path, []byte("{}\n"), "limit")
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the rename in a read-only directory did not fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the read-back did not return in 10 s")
	}
	var proof string
	h.db.QueryRow(`SELECT proof FROM plugin_op_attempts WHERE op_id = ?`, "limit").Scan(&proof)
	if !strings.HasPrefix(proof, "not_applied") {
		t.Fatalf("proof %q", proof)
	}
}

// A crash before the rename, and then the lock does not read (here, a directory): the op ends
// conflict, and it first settles the open attempt (no sent_at: not_applied), so no attempt of an
// ended op stays open.
func TestPluginOpLockUnreadableSettles(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	op := fetchedAdd(t, h, "c", "github:acme/notes@v1.0.0")
	h.pops.fault = func(p string) bool { return p == "plugin-op-before-lock-rename" }
	if err := h.opLock(context.Background(), op); !errors.Is(err, errOpCrash) {
		t.Fatalf("the crash: %v", err)
	}
	h.pops.fault = nil
	os.Remove(opLockFile(h))
	if err := os.Mkdir(opLockFile(h), 0o700); err != nil {
		t.Fatal(err)
	}
	op = opsOf(t, h, op.ID)
	if err := h.opLock(context.Background(), op); err != nil || op.Phase != "conflict" {
		t.Fatalf("the replay: %+v %v", op, err)
	}
	var outcome, proof string
	if err := h.db.QueryRow(`SELECT outcome, proof FROM plugin_op_attempts WHERE op_id = ? AND call = 'write:lock'`, op.ID).Scan(&outcome, &proof); err != nil || outcome == "" || !strings.HasPrefix(proof, "not_applied") {
		t.Fatalf("the attempt: %q %q %v", outcome, proof, err)
	}
}
