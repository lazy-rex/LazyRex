package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// writeHubJSON writes the hub's config file, as the CLI reads it (readPluginCmdConfig), with the
// config plugin entries plugins (a JSON array).
func writeHubJSON(t *testing.T, h *Hub, plugins string) {
	t.Helper()
	b, _ := json.Marshal(h.cfg.State)
	if err := os.WriteFile(h.cfg.path, []byte(`{"state":`+string(b)+`,"plugins":`+plugins+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lockOld is the stored lock_old record of the op.
func lockOld(t *testing.T, h *Hub, id string) map[string]any {
	t.Helper()
	var s string
	if err := h.db.QueryRow(`SELECT lock_old FROM plugin_ops WHERE op_id = ?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal([]byte(s), &m)
	return m
}

// statusOK runs plugin status on the hub's config.
func statusOK(t *testing.T, h *Hub) bool {
	t.Helper()
	ok, err := runPluginStatus(h.cfg.path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

const (
	unpinLatest = "github:acme/notes@latest"
	unpinTarget = "github:acme/notes@v1.0.0"
)

// unpinUpdate is case B (step 4 r2 B): v1.0.0 and v1.1.0 tag one commit, the entry notes is
// @latest at v1.1.0, and its record runs that pin. It returns the update of notes to v1.0.0,
// admitted and resolved. afterReload runs in the op's reload, after the hub loaded the files.
func unpinUpdate(t *testing.T, afterReload func(h *Hub)) (*Hub, *opsCalls, *pluginOp) {
	t.Helper()
	ctx := context.Background()
	r := newTestRepo(t)
	r.commit(map[string]string{"lazyrex.json": manifestJSON("notes"), "main.lua": "return 1\n"})
	r.git("tag", "v1.0.0")
	r.git("tag", "v1.1.0")
	h, rec := fetchTestHubCalls(t, r)
	writeHubJSON(t, h, `[]`)
	seed := lockedAdd(t, h, "seed", unpinLatest)
	var p lockEntry
	json.Unmarshal([]byte(seed.Pin), &p)
	os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"notes","source":"`+unpinLatest+`"}]}`), 0o600)
	opsLoadEntries(t, h)
	pl := &Plugin{Name: "notes", entry: opsEntry(t, h, "managed:notes")}
	h.plugins["notes"] = pl
	if err := h.loadPluginRow(ctx, pl); err != nil {
		t.Fatal(err)
	}
	pl.Desired, pl.InstanceID, pl.BlockID, pl.ClientID = "running", "notes-1-aaaaaaaa", "block:1", "client:notes"
	pl.ChildPID, pl.ChildStart = os.Getpid(), startTime(os.Getpid())
	pl.run = pluginRun{Name: "notes", Source: unpinLatest, Commit: p.Commit, Digest: p.Digest}
	reload := opReload
	opReload = func(ctx context.Context, h *Hub) (any, error) {
		res, err := reload(ctx, h)
		e := opsEntry(t, h, "managed:notes")
		h.mu.Lock()
		pl.entry = e
		h.mu.Unlock()
		if afterReload != nil {
			afterReload(h)
		}
		return res, err
	}
	op, err := resolveOp(t, h, "unpin", opRequest{Op: "update", Entry: "managed:notes", Input: rawJSON(map[string]any{"to": "v1.0.0"})})
	if err != nil || op.Phase != "fetching" {
		t.Fatalf("resolve: %+v %v", op, err)
	}
	return h, rec, op
}

// unpinning (C5f plan r4): after the reload, the update removes the lock key of the source that it
// stopped naming, and plugin status is ok. Before C5f the @latest key stayed, and status was not
// ok (an orphan).
func TestPluginOpUnpinUpdate(t *testing.T) {
	h, _, op := unpinUpdate(t, nil)
	if err := h.runOp(context.Background(), op); err != nil || op.Phase != "done" {
		t.Fatalf("the update: %+v %v", op, err)
	}
	lock := readTestLock(t, h)
	if lock.Plugins[unpinLatest] != nil || lock.Plugins[unpinTarget] == nil {
		t.Fatalf("the lock: %v", lock.Plugins)
	}
	if !statusOK(t, h) {
		t.Fatal("plugin status is not ok after the update")
	}
	if rec := lockOld(t, h, op.ID); rec["key"] != unpinLatest || rec["outcome"] != "removed" {
		t.Fatalf("lock_old: %v", rec)
	}
	if strings.Contains(op.Result, "was kept") {
		t.Fatalf("a note for a removed key: %q", op.Result)
	}
}

// The old key is kept, and the op goes on, when evidence says that it is in use or changed (C5f
// plan r4 items 3 to 6). The end text and lock_old give the reason, and the key is still in the
// lock.
func TestPluginOpUnpinKept(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		after        func(t *testing.T, h *Hub)
	}{
		{"a config entry on disk", "names it", func(t *testing.T, h *Hub) {
			writeHubJSON(t, h, `[{"source":"`+unpinLatest+`"}]`)
		}},
		{"a loaded entry only", "names it", func(t *testing.T, h *Hub) {
			e := &configEntry{Source: unpinLatest}
			h.entriesMu.Lock()
			h.entries = append([]*configEntry{e}, h.entries...)
			h.entriesMu.Unlock()
		}},
		{"another managed entry", "managed:other names it", func(t *testing.T, h *Hub) {
			os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"notes","source":"`+unpinTarget+`"},{"id":"other","source":"`+unpinLatest+`"}]}`), 0o600)
		}},
		{"a CLI pin change after admission", "it changed after the operation was admitted", func(t *testing.T, h *Hub) {
			l := readTestLock(t, h)
			l.Plugins[unpinLatest] = otherLockEntry()
			writeTestLock(t, h, l)
		}},
		{"the managed file changed after writing", "the managed file does not have the operation's update of managed:notes", func(t *testing.T, h *Hub) {
			os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"notes","source":"github:acme/notes@v9"}]}`), 0o600)
		}},
		{"the managed file does not parse", "does not read", func(t *testing.T, h *Hub) {
			os.WriteFile(h.cfg.managedPath, []byte(`{`), 0o600)
		}},
		{"the config file does not parse", "does not read", func(t *testing.T, h *Hub) {
			os.WriteFile(h.cfg.path, []byte(`{`), 0o600)
		}},
		{"the lock does not parse", "does not read", func(t *testing.T, h *Hub) {
			os.WriteFile(lockPath(h.cfg.path, h.cfg.Lock), []byte(`{`), 0o600)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, op := unpinUpdate(t, func(h *Hub) { c.after(t, h) })
			if err := h.runOp(context.Background(), op); err != nil {
				t.Fatal(err)
			}
			rec := lockOld(t, h, op.ID)
			if rec["key"] != unpinLatest || rec["outcome"] != "kept" || !strings.Contains(rec["reason"].(string), c.reason) {
				t.Fatalf("lock_old: %v, want kept with %q", rec, c.reason)
			}
			text := op.Result
			if op.Phase != "done" {
				text = op.Error
			}
			if !strings.Contains(text, "the old lock entry "+unpinLatest+" was kept: ") {
				t.Fatalf("the end text of %+v has no note", op)
			}
			if c.name == "the lock does not parse" {
				return
			}
			if readTestLock(t, h).Plugins[unpinLatest] == nil {
				t.Fatal("the kept key is gone")
			}
		})
	}
}

// A reload that fails ends the update before unpinning (C5f plan r4 item 8): the final phase, its
// error and lock_old kept are stored in one statement; the key stays in the lock.
func TestPluginOpUnpinFailedReload(t *testing.T) {
	h, _, op := unpinUpdate(t, nil)
	reload := opReload
	opReload = func(ctx context.Context, h *Hub) (any, error) {
		reload(ctx, h)
		return nil, errors.New("the reload failed in this test")
	}
	if err := h.runOp(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	var phase, errText, rec string
	if err := h.db.QueryRow(`SELECT phase, error, lock_old FROM plugin_ops WHERE op_id = ?`, op.ID).Scan(&phase, &errText, &rec); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal([]byte(rec), &m)
	if opFinal[phase] && phase != "done" && m["outcome"] == "kept" && m["reason"] == "the operation ended before unpinning" &&
		strings.Contains(errText, "the old lock entry "+unpinLatest+" was kept: the operation ended before unpinning") {
		if readTestLock(t, h).Plugins[unpinLatest] == nil {
			t.Fatal("the key is gone")
		}
		return
	}
	t.Fatalf("phase %s, error %q, lock_old %s", phase, errText, rec)
}

// Crashes in unpinning (C5f plan r4 item 9): before the rename, after the rename, and after the
// phase store. Each replay ends with the key gone, one effective write, and the record removed.
func TestPluginOpUnpinCrash(t *testing.T) {
	for _, fault := range []string{"plugin-op-before-lock-old-rename", "plugin-op-after-lock-old-rename"} {
		t.Run(fault, func(t *testing.T) {
			h, _, op := unpinUpdate(t, nil)
			h.pops.fault = func(p string) bool { return p == fault }
			if err := h.runOp(context.Background(), op); !errors.Is(err, errOpCrash) {
				t.Fatalf("the crash: %v", err)
			}
			h.pops.fault = nil
			op = opsOf(t, h, op.ID)
			if op.Phase != "unpinning" {
				t.Fatalf("the phase after the crash: %s", op.Phase)
			}
			if err := h.runOp(context.Background(), op); err != nil || op.Phase != "done" {
				t.Fatalf("the replay: %+v %v", op, err)
			}
			if readTestLock(t, h).Plugins[unpinLatest] != nil || lockOld(t, h, op.ID)["outcome"] != "removed" {
				t.Fatalf("the key or the record: %v", lockOld(t, h, op.ID))
			}
			var written int
			h.db.QueryRow(`SELECT COUNT(*) FROM plugin_op_attempts WHERE op_id = ? AND call = 'write:lock-old' AND proof LIKE 'applied%'`, op.ID).Scan(&written)
			if written != 1 {
				t.Fatalf("%d effective writes", written)
			}
		})
	}
}

// A crash after the rename, and then another writer changes the lock (the old key stays absent).
// The replay makes no second write, and the record follows the proof of the op's attempt:
//   - settled: a replay settled the attempt (applied) and stopped before the phase store; then the
//     other writer. The op's applied attempt removed the key: removed.
//   - unsettled: the other writer changed the lock before the replay, so the replay cannot prove the
//     rename (the attempt is unknown): absent.
func TestPluginOpUnpinCrashOtherWriter(t *testing.T) {
	for _, settled := range []bool{true, false} {
		t.Run(map[bool]string{true: "settled", false: "unsettled"}[settled], func(t *testing.T) {
			ctx := context.Background()
			h, _, op := unpinUpdate(t, nil)
			h.pops.fault = func(p string) bool { return p == "plugin-op-after-lock-old-rename" }
			if err := h.runOp(ctx, op); !errors.Is(err, errOpCrash) {
				t.Fatalf("the crash: %v", err)
			}
			h.pops.fault = nil
			op = opsOf(t, h, op.ID)
			if settled {
				b, _ := os.ReadFile(lockPath(h.cfg.path, h.cfg.Lock))
				if err := h.settleFileAttempt(ctx, op, "write:lock-old", b); err != nil {
					t.Fatal(err)
				}
			}
			lock := readTestLock(t, h)
			lock.Plugins["github:acme/other@v1.0.0"] = otherLockEntry()
			writeTestLock(t, h, lock)
			if err := h.runOp(ctx, op); err != nil || op.Phase != "done" {
				t.Fatalf("the replay: %+v %v", op, err)
			}
			want := map[bool]string{true: "removed", false: "absent"}[settled]
			var n int
			h.db.QueryRow(`SELECT COUNT(*) FROM plugin_op_attempts WHERE op_id = ? AND call = 'write:lock-old'`, op.ID).Scan(&n)
			if got := readTestLock(t, h); got.Plugins[unpinLatest] != nil || got.Plugins["github:acme/other@v1.0.0"] == nil || n != 1 || lockOld(t, h, op.ID)["outcome"] != want {
				t.Fatalf("the lock %v, %d attempts, the record %v, want %s", got.Plugins, n, lockOld(t, h, op.ID), want)
			}
		})
	}
}

// A remove of a GitHub entry removes its key after the reload; a remove of a local entry makes no
// unpinning attempt. An update that keeps its source string makes none either.
func TestPluginOpUnpinRemoveAndNone(t *testing.T) {
	ctx := context.Background()
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	writeHubJSON(t, h, `[]`)
	const src = "github:acme/notes@v1.0.0"
	lockedAdd(t, h, "seed", src)
	os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"notes","source":"`+src+`"}]}`), 0o600)
	opsLoadEntries(t, h)
	id := opsMust(t, h, "rm", opRequest{Op: "remove", Entry: "managed:notes"})
	op := opsOf(t, h, id)
	if err := h.runOp(ctx, op); err != nil || op.Phase != "done" {
		t.Fatalf("the remove: %+v %v", op, err)
	}
	if readTestLock(t, h).Plugins[src] != nil || !statusOK(t, h) {
		t.Fatalf("after the remove: lock %v", readTestLock(t, h).Plugins)
	}

	h2, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "y"))
	opsLoadEntries(t, h2)
	id = opsMust(t, h2, "rm-local", opRequest{Op: "remove", Entry: "managed:y"})
	op = opsOf(t, h2, id)
	if err := h2.runOp(ctx, op); err != nil || op.Phase != "done" {
		t.Fatalf("the local remove: %+v %v", op, err)
	}
	var n int
	h2.db.QueryRow(`SELECT COUNT(*) FROM plugin_op_attempts WHERE op_id = ? AND call = 'write:lock-old'`, id).Scan(&n)
	if n != 0 || lockOld(t, h2, id) != nil {
		t.Fatalf("a local remove unpinned: %d attempts, %v", n, lockOld(t, h2, id))
	}
}

// A crash after the phase store of unpinning (here, in the replace that follows): the replay goes
// on from replacing. It does not decide again, and it makes no second write:lock-old attempt.
func TestPluginOpUnpinCrashAfterStore(t *testing.T) {
	h, _, op := unpinUpdate(t, nil)
	h.pops.fault = func(p string) bool { return p == "plugin-op-after-replace" }
	if err := h.runOp(context.Background(), op); !errors.Is(err, errOpCrash) {
		t.Fatalf("the crash: %v", err)
	}
	h.pops.fault = nil
	op = opsOf(t, h, op.ID)
	if op.Phase != "replacing" || lockOld(t, h, op.ID)["outcome"] != "removed" {
		t.Fatalf("after the crash: %+v, %v", op, lockOld(t, h, op.ID))
	}
	wait := registerWait
	t.Cleanup(func() { registerWait = wait })
	registerWait = 0
	if err := h.runOp(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM plugin_op_attempts WHERE op_id = ? AND call = 'write:lock-old'`, op.ID).Scan(&n)
	if n != 1 || lockOld(t, h, op.ID)["outcome"] != "removed" {
		t.Fatalf("%d unpinning attempts, %v", n, lockOld(t, h, op.ID))
	}
}
