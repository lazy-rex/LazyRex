package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	upSource = "github:acme/y@v2"
	upCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	upOld    = "cccccccccccccccccccccccccccccccccccccccc"
	upDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// updateOpHub is a hub whose managed entry y has the source after an update (upSource, as the
// writing phase left it), a record of y, and an update op of y in its replacing phase with the pin
// upCommit. setup sets the record's state before the op runs.
func updateOpHub(t *testing.T, setup func(h *Hub, pl *Plugin)) (*Hub, *opsCalls, *Plugin, *pluginOp) {
	t.Helper()
	h, c, dir := opsTestHub(t, "")
	writeManaged(t, dir, `{"plugins":[{"id":"y","source":"`+upSource+`"},{"id":"z","source":"github:acme/z@v1"}]}`)
	opsLoadEntries(t, h)
	pl := &Plugin{Name: "y", entry: opsEntry(t, h, "managed:y")}
	h.plugins["y"] = pl
	if err := h.loadPluginRow(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	pl.Desired = "stopped"
	if setup != nil {
		setup(h, pl)
	}
	base, _ := json.Marshal(managedFileEntry{ID: "y", Source: "github:acme/y@v1"})
	target, _ := json.Marshal(opResolved{Source: upSource, RefKind: "tag", Commit: upCommit})
	pin, _ := json.Marshal(lockEntry{Name: "y", Commit: upCommit, Digest: upDigest})
	if _, err := h.db.Exec(`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated, base, pin, entry_identity)
		VALUES ('u1', 's', 'k', 'f', '{"kind":"user","id":"view-1"}', 'update', 'managed:y', 'y', '', '{"to":"v2"}', ?, 'replacing', 1, 1, ?, ?, '["managed:y","github:acme/y@v1"]')`,
		string(target), string(base), string(pin)); err != nil {
		t.Fatal(err)
	}
	return h, c, pl, opsOf(t, h, "u1")
}

// live makes pl run instance y-1-aaaaaaaa at commit.
func live(pl *Plugin, commit string) {
	pl.Desired, pl.InstanceID, pl.BlockID, pl.ClientID = "running", "y-1-aaaaaaaa", "block:1", "client:y"
	pl.ChildPID, pl.ChildStart = os.Getpid(), startTime(os.Getpid())
	pl.run = pluginRun{Name: "y", Source: upSource, Commit: commit, Digest: upDigest}
}

// selected makes pl's run the selected run of entry e.
func selected(pl *Plugin, e *configEntry) {
	pl.selected = &selectedRun{Entry: entryIdentity(e), Run: pl.run}
}

// notLive leaves pl desired as it is, with no instance that runs.
func notLive(pl *Plugin) {
	pl.BlockID, pl.ClientID, pl.ChildPID, pl.ChildStart = "", "", 0, ""
}

// callNames are the admin actions of the recorder, in order.
func callNames(c *opsCalls) []string {
	var out []string
	for _, s := range c.list() {
		if a, _, ok := strings.Cut(s, " "); ok && strings.HasPrefix(a, "hub.admin.") {
			out = append(out, strings.TrimPrefix(a, "hub.admin."))
		}
	}
	return out
}

// The replacing phase of an update (C5 plan r1 item 5, r2 A, B, C and E): one read of the record
// picks one call, or no call, for its state; every result with no call first checks that the record
// is still bound to the intended entry. A replace that refuses no_instance before its own effect
// (not_applied) goes to fallback, which picks only a start or no call. A stopped replace after
// sent_at has no proof: unknown, and no start.
func TestPluginOpUpdateReplacing(t *testing.T) {
	other := func(h *Hub, pl *Plugin) { pl.entry = opsEntry(t, h, "managed:z") }
	for _, c := range []struct {
		name   string
		setup  func(h *Hub, pl *Plugin)
		reply  func(h *Hub, pl *Plugin, action string) error
		calls  string
		phase  string
		result string // a part of the result or the error text
	}{
		{name: "live at the pin", setup: func(h *Hub, pl *Plugin) { live(pl, upCommit); selected(pl, pl.entry) }, phase: "done", result: "y runs " + upSource},
		// codex-27's selected-run gap: the run is the pin, but the next automatic start would not run
		// it, so the update replaces, and the replace selects the pin under the intended entry.
		{name: "live at the pin, no selected run", setup: func(h *Hub, pl *Plugin) { live(pl, upCommit) }, calls: "replace", phase: "done"},
		{name: "live at the pin, selected under an old entry", setup: func(h *Hub, pl *Plugin) {
			live(pl, upCommit)
			selected(pl, opsEntry(t, h, "managed:z"))
		}, calls: "replace", phase: "done"},
		{name: "live at the pin, another run selected", setup: func(h *Hub, pl *Plugin) {
			live(pl, upCommit)
			selected(pl, pl.entry)
			pl.selected.Run.Commit = upOld
		}, calls: "replace", phase: "done"},
		{name: "live at another run", setup: func(h *Hub, pl *Plugin) { live(pl, upOld) }, calls: "replace", phase: "done"},
		{name: "waiting", setup: func(h *Hub, pl *Plugin) { pl.Desired = "running" }, calls: "start", phase: "done"},
		{name: "retained", setup: func(h *Hub, pl *Plugin) { live(pl, upOld); notLive(pl) }, calls: "start", phase: "done"},
		{name: "stopped", phase: "done", result: "y is stopped; its next start runs " + upSource},
		{name: "pending", setup: func(h *Hub, pl *Plugin) {
			pl.Desired = "running"
			if err := h.insertIntent(context.Background(), pl, "y-2-bbbbbbbb", "session:home"); err != nil {
				t.Fatal(err)
			}
		}, reply: func(h *Hub, pl *Plugin, action string) error {
			return refuse("unresolved_intent", "plugin y has the open intent y-2-bbbbbbbb")
		}, calls: "start", phase: "failed", result: "unresolved_intent"},
		{name: "same pin, another entry", setup: func(h *Hub, pl *Plugin) { live(pl, upCommit); other(h, pl) }, phase: "conflict", result: "nothing was called"},
		{name: "stopped, another entry", setup: other, phase: "conflict", result: "nothing was called"},
		{name: "fallback start", setup: func(h *Hub, pl *Plugin) { live(pl, upOld) }, reply: func(h *Hub, pl *Plugin, action string) error {
			if action == "hub.admin.replace" {
				notLive(pl)
				return refuse("no_instance", "plugin y has no instance; use hub.admin.start")
			}
			return nil
		}, calls: "replace start", phase: "done"},
		{name: "fallback stopped", setup: func(h *Hub, pl *Plugin) { live(pl, upOld) }, reply: func(h *Hub, pl *Plugin, action string) error {
			notLive(pl)
			pl.Desired = "stopped"
			return refuse("no_instance", "plugin y has no instance; use hub.admin.start")
		}, calls: "replace", phase: "done", result: "y is stopped"},
		{name: "fallback, another entry", setup: func(h *Hub, pl *Plugin) { live(pl, upOld) }, reply: func(h *Hub, pl *Plugin, action string) error {
			notLive(pl)
			other(h, pl)
			return refuse("no_instance", "plugin y has no instance; use hub.admin.start")
		}, calls: "replace", phase: "conflict", result: "nothing was called"},
		{name: "fallback finds a live run", setup: func(h *Hub, pl *Plugin) { live(pl, upOld) }, reply: func(h *Hub, pl *Plugin, action string) error {
			return refuse("no_instance", "plugin y has no instance; use hub.admin.start")
		}, calls: "replace", phase: "done", result: "y runs " + upSource + " at " + upOld[:12]},
		{name: "stopped after sent_at", setup: func(h *Hub, pl *Plugin) { live(pl, upOld) }, reply: func(h *Hub, pl *Plugin, action string) error {
			// The handler's state after its retire saw the block closed (replace.go).
			notLive(pl)
			pl.Desired = "stopped"
			return refuse("stopped", "plugin y: the block of instance y-1-aaaaaaaa was closed; the plugin is stopped")
		}, calls: "replace", phase: "unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, rec, pl, op := updateOpHub(t, c.setup)
			if c.reply != nil {
				rec.reply = func(action string, _ map[string]any) (any, error) {
					if err := c.reply(h, pl, action); err != nil {
						return nil, err
					}
					return map[string]any{"ok": true}, nil
				}
			}
			if err := h.runOp(context.Background(), op); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(callNames(rec), " "); got != c.calls {
				t.Errorf("calls %q, want %q", got, c.calls)
			}
			if op.Phase != c.phase || !strings.Contains(op.Result+op.Error, c.result) {
				t.Errorf("op %+v, want %s with %q", op, c.phase, c.result)
			}
			if stored := opsOf(t, h, op.ID); stored.Phase != op.Phase {
				t.Errorf("stored phase %s, op %s", stored.Phase, op.Phase)
			}
		})
	}
}

// The calls of replacing send the intended entry (the new source) and the pin: a replace names the
// owned instance, and a start names the plugin.
func TestPluginOpUpdateReplacingArgs(t *testing.T) {
	h, rec, _, op := updateOpHub(t, func(h *Hub, pl *Plugin) { live(pl, upOld) })
	if err := h.runOp(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	want := `hub.admin.replace {"expect_entry":"[\"managed:y\",\"` + upSource + `\"]","expect_instance":"y-1-aaaaaaaa","expect_run":{"commit":"` + upCommit + `","digest":"` + upDigest + `","source":"` + upSource + `"},"plugin":"y"}`
	if got := rec.list(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

// A crash right after the transaction that stores the replace's no_instance outcome and phase
// fallback (C5 plan r2 B): a new run finds phase fallback, makes one start, and never a second
// replace. A crash after the start's attempt row: the replay observes it and calls nothing.
func TestPluginOpFallbackCrash(t *testing.T) {
	ctx := context.Background()
	h, rec, pl, op := updateOpHub(t, func(h *Hub, pl *Plugin) { live(pl, upOld) })
	rec.reply = func(action string, _ map[string]any) (any, error) {
		if action == "hub.admin.replace" {
			notLive(pl)
			return nil, refuse("no_instance", "plugin y has no instance; use hub.admin.start")
		}
		return map[string]any{"ok": true}, nil
	}
	h.pops.fault = func(p string) bool { return p == "plugin-op-after-finish" }
	if err := h.runOp(ctx, op); !errors.Is(err, errOpCrash) {
		t.Fatalf("the crash: %v", err)
	}
	h.pops.fault = nil
	op = opsOf(t, h, op.ID)
	if op.Phase != "fallback" {
		t.Fatalf("the stored phase: %+v", op)
	}
	h.pops.fault = func(p string) bool { return p == "plugin-op-after-start" }
	if err := h.runOp(ctx, op); !errors.Is(err, errOpCrash) {
		t.Fatalf("the crash after the start: %v", err)
	}
	h.pops.fault = nil
	wait := registerWait
	t.Cleanup(func() { registerWait = wait })
	registerWait = 0
	op = opsOf(t, h, op.ID)
	if err := h.runOp(ctx, op); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(callNames(rec), " "); got != "replace start" {
		t.Fatalf("calls %q, want one replace and one start", got)
	}
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM plugin_op_attempts WHERE op_id = ?`, op.ID).Scan(&n)
	if n != 2 || op.Phase == "fallback" || op.Phase == "replacing" {
		t.Fatalf("%d attempts, op %+v", n, op)
	}
}

// The replay rule of replacing (C5 plan r2 B): a replace attempt with a stored outcome while the
// phase is still replacing (a lost phase write; a written fixture) is never done from that
// outcome and never a fallback: with no applied proof it ends unknown, and nothing is called.
func TestPluginOpReplacingReplay(t *testing.T) {
	h, rec, _, op := updateOpHub(t, func(h *Hub, pl *Plugin) { live(pl, upOld) })
	if _, err := h.db.Exec(`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, bound_at, sent_at, outcome) VALUES ('r1', 'u1', 'replace', 1, 't', 't', 'error: no_instance: plugin y has no instance')`); err != nil {
		t.Fatal(err)
	}
	if err := h.runOp(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if op.Phase != "unknown" || len(rec.list()) != 0 {
		t.Fatalf("op %+v, calls %v", op, rec.list())
	}
}

// restart in runOp (C5 plan r1 item 5): replacing replaces the stored instance; a refusal ends the
// op, with no fallback.
func TestPluginOpRestartReplacing(t *testing.T) {
	h, rec, _, op := replaceOpHub(t)
	if err := h.runOp(context.Background(), op); err != nil || op.Phase != "done" || strings.Join(callNames(rec), " ") != "replace" {
		t.Fatalf("op %+v %v, calls %v", op, err, rec.list())
	}
	h, rec, _, op = replaceOpHub(t)
	rec.reply = func(string, map[string]any) (any, error) {
		return nil, refuse("no_instance", "plugin x has no instance; use hub.admin.start")
	}
	if err := h.runOp(context.Background(), op); err != nil || op.Phase != "failed" || strings.Join(callNames(rec), " ") != "replace" {
		t.Fatalf("a refused restart: op %+v %v, calls %v", op, err, rec.list())
	}
}

// Step 4 plan r2 B, case A: a CLI update pins v2 while v1 runs, then an update to latest
// resolves v2. The fetch is skipped (the lock has the target), the lock and the managed file are
// not written (no write attempt), the reload runs, and a replace makes v2 run.
func TestPluginOpUpdateCaseA(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	r.commit(map[string]string{"lazyrex.json": manifestJSON("notes"), "main.lua": "return 1\n"})
	r.git("tag", "v1.0.0")
	h, rec := fetchTestHubCalls(t, r)
	const src = "github:acme/notes@latest"
	v1 := lockedAdd(t, h, "seed-v1", src) // the lock entry at v1, as a CLI sync gives it
	var p1 lockEntry
	json.Unmarshal([]byte(v1.Pin), &p1)
	r.commit(map[string]string{"main.lua": "return 2\n"})
	r.git("tag", "v2.0.0")
	lockedAdd(t, h, "seed-v2", src) // the CLI update: the lock entry of @latest is v2 now
	lockBytes, _ := os.ReadFile(lockPath(h.cfg.path, h.cfg.Lock))
	file := `{"plugins":[{"id":"notes","source":"` + src + `"}]}`
	os.WriteFile(h.cfg.managedPath, []byte(file), 0o600)
	opsLoadEntries(t, h)
	pl := &Plugin{Name: "notes", entry: opsEntry(t, h, "managed:notes")}
	h.plugins["notes"] = pl
	if err := h.loadPluginRow(ctx, pl); err != nil {
		t.Fatal(err)
	}
	pl.Desired, pl.InstanceID, pl.BlockID, pl.ClientID = "running", "notes-1-aaaaaaaa", "block:1", "client:notes"
	pl.ChildPID, pl.ChildStart = os.Getpid(), startTime(os.Getpid())
	pl.run = pluginRun{Name: "notes", Source: src, Commit: p1.Commit, Digest: p1.Digest}
	op, err := resolveOp(t, h, "case-a", opRequest{Op: "update", Entry: "managed:notes", Input: rawJSON(map[string]any{"to": "latest"})})
	if err != nil || op.Phase != "fetching" {
		t.Fatalf("resolve: %+v %v", op, err)
	}
	if err := h.runOp(ctx, op); err != nil || op.Phase != "done" {
		t.Fatalf("the update: %+v %v; calls %v", op, err, rec.list())
	}
	var pin lockEntry
	json.Unmarshal([]byte(op.Pin), &pin)
	if pin.Commit == p1.Commit || strings.Join(callNames(rec), " ") != "replace" || !slices.Contains(rec.list(), "reload") {
		t.Fatalf("pin %s (v1 %s), calls %v", pin.Commit, p1.Commit, rec.list())
	}
	var writes int
	h.db.QueryRow(`SELECT COUNT(*) FROM plugin_op_attempts WHERE op_id = ? AND call LIKE 'write:%'`, op.ID).Scan(&writes)
	after, _ := os.ReadFile(lockPath(h.cfg.path, h.cfg.Lock))
	if writes != 0 || string(after) != string(lockBytes) || managedText(t, h) != file {
		t.Fatalf("%d write attempts; the lock or the file changed", writes)
	}
	for _, s := range rec.list() {
		if strings.HasPrefix(s, "hub.admin.replace ") && !strings.Contains(s, `"commit":"`+pin.Commit+`"`) {
			t.Fatalf("the replace does not name v2: %s", s)
		}
	}
}

// Step 4 plan r2 B, case B, with the hub lead's ruling (lead-caseb): v1.0.0 and v1.1.0 name one
// commit, and the entry is @latest at v1.1.0. The update to v1.0.0 writes the source @v1.0.0 and
// that key's pin with its tag metadata; the @latest pin stays. A run belongs to its entry, so the
// instance of @latest is replaced: the new run is @v1.0.0 with the same commit and digest, and the
// end text says so.
func TestPluginOpUpdateCaseB(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	r.commit(map[string]string{"lazyrex.json": manifestJSON("notes"), "main.lua": "return 1\n"})
	r.git("tag", "v1.0.0")
	r.git("tag", "v1.1.0")
	h, rec := fetchTestHubCalls(t, r)
	const latest, target = "github:acme/notes@latest", "github:acme/notes@v1.0.0"
	seed := lockedAdd(t, h, "seed", latest)
	var p lockEntry
	json.Unmarshal([]byte(seed.Pin), &p)
	if p.RefKind != "latest" || p.Tag != "v1.1.0" {
		t.Fatalf("the @latest pin: %+v", p)
	}
	os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"notes","source":"`+latest+`"}]}`), 0o600)
	opsLoadEntries(t, h)
	pl := &Plugin{Name: "notes", entry: opsEntry(t, h, "managed:notes")}
	h.plugins["notes"] = pl
	if err := h.loadPluginRow(ctx, pl); err != nil {
		t.Fatal(err)
	}
	pl.Desired, pl.InstanceID, pl.BlockID, pl.ClientID = "running", "notes-1-aaaaaaaa", "block:1", "client:notes"
	pl.ChildPID, pl.ChildStart = os.Getpid(), startTime(os.Getpid())
	pl.run = pluginRun{Name: "notes", Source: latest, Commit: p.Commit, Digest: p.Digest}
	reload := opReload
	opReload = func(ctx context.Context, h *Hub) (any, error) {
		res, err := reload(ctx, h)
		e := opsEntry(t, h, "managed:notes") // the reload binds the record to the new entry
		h.mu.Lock()
		pl.entry = e
		h.mu.Unlock()
		return res, err
	}
	var replaced map[string]any
	rec.reply = func(action string, args map[string]any) (any, error) {
		if action == "hub.admin.replace" {
			replaced, _ = args["expect_run"].(map[string]any)
		}
		return map[string]any{"ok": true}, nil
	}
	op, err := resolveOp(t, h, "case-b", opRequest{Op: "update", Entry: "managed:notes", Input: rawJSON(map[string]any{"to": "v1.0.0"})})
	if err != nil || op.Phase != "fetching" {
		t.Fatalf("resolve: %+v %v", op, err)
	}
	if err := h.runOp(ctx, op); err != nil || op.Phase != "done" {
		t.Fatalf("the update: %+v %v; calls %v", op, err, rec.list())
	}
	want := map[string]any{"source": target, "commit": p.Commit, "digest": p.Digest}
	if strings.Join(callNames(rec), " ") != "replace" || !reflect.DeepEqual(replaced, want) {
		t.Fatalf("calls %v; the replace's run %v, want %v", rec.list(), replaced, want)
	}
	if text := "replaced: the same commit " + p.Commit + " now runs as " + target; op.Result != text {
		t.Fatalf("the end text %q, want %q", op.Result, text)
	}
	lock := readTestLock(t, h)
	if le := lock.Plugins[target]; le == nil || le.Commit != p.Commit || le.RefKind != "tag" || le.Ref != "v1.0.0" || lock.Plugins[latest] == nil {
		t.Fatalf("the lock: %+v", lock.Plugins)
	}
	if managedSource(t, h, "notes") != target {
		t.Fatalf("the managed source: %s", managedText(t, h))
	}
}
