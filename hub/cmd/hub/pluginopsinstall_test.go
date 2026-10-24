package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installStarts receives one value for each start after a scan's installs.
type installStarts chan struct{}

// noInstallStart replaces the start after a scan's installs (a goroutine) with a send.
func noInstallStart(t *testing.T) installStarts {
	t.Helper()
	c := make(installStarts, 16)
	old := afterInstalls
	afterInstalls = func(*Hub, context.Context) { c <- struct{}{} }
	t.Cleanup(func() { afterInstalls = old })
	return c
}

// want checks that exactly n starts come.
func (c installStarts) want(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatalf("the start after the installs ran %d times, want %d", i, n)
		}
	}
	select {
	case <-c:
		t.Fatalf("the start after the installs ran more than %d times", n)
	case <-time.After(100 * time.Millisecond):
	}
}

// runInstalls runs a scan and each install op that it admitted to its end, then the manager's
// end-of-pass check. It returns the ops by entry key.
func runInstalls(t *testing.T, h *Hub, why string) map[string]*pluginOp {
	t.Helper()
	ctx := context.Background()
	h.pops.installs.mu.Lock()
	h.pops.installs.recovered = true // a running hub: its start recovery ended
	h.pops.installs.mu.Unlock()
	h.installScan(ctx, why)
	h.pops.installs.mu.Lock()
	ids := map[string]string{}
	for id, entry := range h.pops.installs.pending {
		ids[id] = entry
	}
	h.pops.installs.mu.Unlock()
	out := map[string]*pluginOp{}
	for id, entry := range ids {
		op := opsOf(t, h, id)
		if err := h.runOp(ctx, op); err != nil {
			t.Fatalf("install %s: %v", entry, err)
		}
		out[entry] = op
	}
	h.installsEnded(ctx)
	return out
}

// install admission: only the hub admits it, of a managed GitHub entry, with no input; its base
// is the managed entry, and its lock base the entry's source.
func TestPluginInstallAdmission(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	local := opsLocal(t, t.TempDir(), "loc")
	writeManaged(t, dir, strings.Replace(local, `]}`, `,{"id":"tools","source":"github:acme/tools@v1.0.0"}]}`, 1))
	opsLoadEntries(t, h)
	ctx := context.Background()
	admit := func(origin opOrigin, key string, req opRequest) error {
		_, err := h.admitOp(ctx, origin, origin.Kind, key, req)
		return err
	}
	for i, c := range []struct {
		origin opOrigin
		req    opRequest
		text   string
	}{
		{opOrigin{"agent", "a1"}, opRequest{Op: "install", Entry: "managed:tools"}, "only the hub admits an install"},
		{opOrigin{"user", "v1"}, opRequest{Op: "install", Entry: "managed:tools"}, "only the hub admits an install"},
		{opOrigin{"hub", "start"}, opRequest{Op: "update", Entry: "managed:tools", Input: rawJSON(map[string]any{"to": "@"})}, "only the hub admits an install"},
		{opOrigin{"hub", "start"}, opRequest{Op: "install", Entry: "managed:tools", Input: rawJSON(map[string]any{"to": "@"})}, "only settings, add and update have input"},
		{opOrigin{"hub", "start"}, opRequest{Op: "install", Entry: "managed:loc"}, "install needs a GitHub source"},
	} {
		if err := admit(c.origin, "bad-"+string(rune('a'+i)), c.req); err == nil || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%d: %v, want %q", i, err, c.text)
		}
	}
	if err := admit(opOrigin{"hub", "start"}, "ok", opRequest{Op: "install", Entry: "managed:tools"}); err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, mustLastOp(t, h))
	if op.Kind != "install" || op.Phase != "queued" || op.Target != "" || op.Origin != (opOrigin{"hub", "start"}) ||
		op.Base != `{"id":"tools","source":"github:acme/tools@v1.0.0","args":{},"watch_roots":[]}` ||
		op.LockBase != `{"github:acme/tools@v1.0.0":"absent"}` {
		t.Fatalf("the install op: %+v", op)
	}
}

// A managed entry with no lock entry: the scan installs it at the entry's ref (the "@" path), and
// a second scan finds it in place. The install starts nothing itself.
func TestPluginInstallNoLock(t *testing.T) {
	starts := noInstallStart(t)
	r, commit := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	if err := os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"notes","source":"github:acme/notes@v1.0.0"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opsLoadEntries(t, h)
	ops := runInstalls(t, h, "start")
	op := ops["managed:notes"]
	if len(ops) != 1 || op.Phase != "done" || op.Origin != (opOrigin{"hub", "start"}) {
		t.Fatalf("the install: %+v", ops)
	}
	le := readTestLock(t, h).Plugins["github:acme/notes@v1.0.0"]
	if le == nil || le.Commit != commit {
		t.Fatalf("the lock entry: %+v", le)
	}
	if _, _, err := lockedDir(h.cfg.State, readTestLock(t, h), &configEntry{Source: "github:acme/notes@v1.0.0", github: true, owner: "acme", repo: "notes", gitRef: "v1.0.0"}); err != nil {
		t.Fatalf("the cache entry: %v", err)
	}
	starts.want(t, 1)
	if got := runInstalls(t, h, "reload"); len(got) != 0 {
		t.Errorf("a second scan: %+v", got)
	}
}

// A lock entry with a cleared cache: the install fetches the LOCKED commit by its ID, also when
// the remote tag moved (lead-c2inst: a cleared cache never upgrades a plugin). An entry in place,
// an entry with an open op, and a lock entry that does not agree with the source get no op.
func TestPluginInstallLockedCommit(t *testing.T) {
	noInstallStart(t)
	h, r, commit, _ := atTestHub(t, "github:acme/notes@v1.0.0")
	// The fixture's add stopped at writing: end it, so the entry has no open op.
	if _, err := h.db.Exec(`UPDATE plugin_ops SET phase = 'done' WHERE kind = 'add'`); err != nil {
		t.Fatal(err)
	}
	if got := h.installRequests(context.Background()); len(got) != 0 {
		t.Fatalf("an entry in place: %+v", got)
	}
	le := lockedNotes(t, h)
	removeTree(filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(le.Dir)))
	r.commit(map[string]string{"lazyrex.json": manifestJSON("notes"), "main.lua": "return 2\n"})
	r.git("tag", "-f", "v1.0.0")
	// An open op on the entry: no install.
	if _, err := userAdmit(h, "set", opRequest{Op: "settings", Entry: "managed:notes", Input: rawJSON(map[string]any{"args": map[string]any{"k": "v"}})}); err != nil {
		t.Fatal(err)
	}
	if got := h.installRequests(context.Background()); len(got) != 0 {
		t.Fatalf("an entry with an open op: %+v", got)
	}
	if _, err := h.db.Exec(`UPDATE plugin_ops SET phase = 'failed' WHERE kind = 'settings'`); err != nil {
		t.Fatal(err)
	}
	ops := runInstalls(t, h, "start")
	op := ops["managed:notes"]
	var target opResolved
	json.Unmarshal([]byte(op.Target), &target)
	if op == nil || op.Phase != "done" || target.Commit != commit || !target.ByID || target.Digest != le.Digest {
		t.Fatalf("the install: %+v", op)
	}
	if got := lockedNotes(t, h); got.Commit != commit || got.Digest != le.Digest {
		t.Fatalf("the lock after the install: %+v", got)
	}
	// A lock entry that does not agree with the source: no op (the start error says what to do).
	lock := readTestLock(t, h)
	lock.Plugins["github:acme/notes@v1.0.0"].RefKind = "branch"
	writeTestLock(t, h, lock)
	removeTree(filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(le.Dir)))
	if got := h.installRequests(context.Background()); len(got) != 0 {
		t.Fatalf("a lock entry that does not agree: %+v", got)
	}
}

// The lock phase of an install (lead-c2i5): A, which depends on B, passes locking while B has no
// manifest; a refusal that only the new lock gives another entry (a name clash) is conflict, and
// the lock is unchanged.
func TestPluginInstallLockGraph(t *testing.T) {
	noInstallStart(t)
	repos := map[string]*testRepo{}
	for name, m := range map[string]string{"a": manifestJSON("a", "b"), "b": manifestJSON("b"), "c": manifestJSON("b")} {
		r := newTestRepo(t)
		r.commit(map[string]string{"lazyrex.json": m, "main.lua": "return 1\n"})
		r.git("tag", "v1")
		repos[name] = r
	}
	h, _ := fetchTestHubCalls(t, repos["a"])
	h.pops.cmd = pluginCmdOpts{remote: func(owner, repo string) string { return repos[repo].url() }}
	write := func(body string) {
		if err := os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[`+body+`]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		opsLoadEntries(t, h)
	}
	write(`{"id":"a","source":"github:acme/a@v1"},{"id":"b","source":"github:acme/b@v1"}`)
	// A alone first: B has no lock entry and no manifest.
	v, err := h.admitInstall(context.Background(), "start", opRequest{Op: "install", Entry: "managed:a"})
	if err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, v["op_id"].(string))
	if err := h.runOp(context.Background(), op); err != nil || op.Phase != "done" {
		t.Fatalf("the install of a before b: %+v %v", op, err)
	}
	if ops := runInstalls(t, h, "start"); ops["managed:b"] == nil || ops["managed:b"].Phase != "done" || len(ops) != 1 {
		t.Fatalf("the install of b: %+v", ops)
	}
	before, _ := os.ReadFile(opLockFile(h))
	write(`{"id":"a","source":"github:acme/a@v1"},{"id":"b","source":"github:acme/b@v1"},{"id":"c","source":"github:acme/c@v1"}`)
	ops := runInstalls(t, h, "reload")
	if c := ops["managed:c"]; c == nil || c.Phase != "conflict" || !strings.Contains(c.Error, "the new lock refuses managed:") || strings.Contains(c.Error, "refuses managed:c") {
		t.Fatalf("the install of c (manifest name b): %+v", ops["managed:c"])
	}
	if after, _ := os.ReadFile(opLockFile(h)); string(after) != string(before) {
		t.Error("the conflict wrote the lock")
	}
}

// A failed install: hub.plugins reports "install failed: <op error>" on the refused entry, and a
// done install clears it.
func TestPluginInstallFailedError(t *testing.T) {
	noInstallStart(t)
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	h.pops.cmd = pluginCmdOpts{remote: func(owner, repo string) string { return "file:///nonexistent/" + repo }}
	if err := os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"notes","source":"github:acme/notes@v1.0.0"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opsLoadEntries(t, h)
	// A fresh entry: no manifest named it, so it has no name and no record.
	h.entriesMu.Lock()
	for _, e := range h.entries {
		e.name = ""
	}
	h.entriesMu.Unlock()
	entryError := func() string {
		v, err := h.pluginsList(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range v.(map[string]any)["entries"].([]any) {
			if m := e.(map[string]any); m["managed"] == "notes" {
				s, _ := m["error"].(string)
				return s
			}
		}
		return "no entry"
	}
	ops := runInstalls(t, h, "start")
	if op := ops["managed:notes"]; op == nil || op.Phase != "failed" {
		t.Fatalf("the install: %+v", ops)
	}
	if got := entryError(); !strings.HasPrefix(got, "install failed: resolve managed:notes") {
		v, _ := h.pluginsList(context.Background())
		t.Fatalf("the entry error: %q in %+v", got, v)
	}
	h.pops.cmd = pluginCmdOpts{remote: func(owner, repo string) string { return r.url() }}
	if ops := runInstalls(t, h, "reload"); ops["managed:notes"] == nil || ops["managed:notes"].Phase != "done" {
		t.Fatalf("the second install: %+v", ops)
	}
	if got := entryError(); strings.Contains(got, "install failed") {
		t.Errorf("the entry error after a done install: %q", got)
	}
}

// An install that a hub stop interrupted (lead-c22b item 1, lead-c22c, lead-c22e): the new hub
// keeps the database and loses the pending map. pluginOpsStart makes the open install pending
// before the manager's first pass, so its end leads to the one normal start, which waits for the
// end of the start recovery (recoverPlugins): a reload scan before it opens nothing. With the
// manager, the install ends before the start recovery runs; with the scan first (the reviewer's
// order), the pass after the start recovery ends it.
func TestPluginInstallRecovered(t *testing.T) {
	for _, manager := range []bool{true, false} {
		t.Run(fmt.Sprintf("manager %v", manager), func(t *testing.T) {
			starts := noInstallStart(t)
			r, _ := fetchTestRepo(t)
			h := fetchTestHub(t, r)
			if err := os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"notes","source":"github:acme/notes@v1.0.0"}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			opsLoadEntries(t, h)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			h.installScan(ctx, "start")
			id := mustLastOp(t, h)
			h.pops.installs.mu.Lock()
			h.pops.installs.pending, h.pops.installs.recovered = nil, false // a new hub
			h.pops.installs.mu.Unlock()
			// The unit hub has no Rex: notes is desired stopped, so the start recovery spawns nothing.
			mustExec(t, h, `INSERT INTO plugins (name, desired) VALUES ('notes', 'stopped') ON CONFLICT (name) DO UPDATE SET desired = 'stopped'`)
			if !manager {
				if err := h.installsRecover(ctx); err != nil {
					t.Fatal(err)
				}
				h.recoverPlugins(ctx)
				h.opsRun(ctx)
			} else {
				if err := h.pluginOpsStart(ctx); err != nil {
					t.Fatal(err)
				}
				for deadline := time.Now().Add(10 * time.Second); opsOf(t, h, id).Phase != "done"; time.Sleep(20 * time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatalf("the resumed install: %+v", opsOf(t, h, id))
					}
				}
				h.installScan(ctx, "reload") // a reload before the start recovery
				starts.want(t, 0)
				h.recoverPlugins(ctx)
			}
			if op := opsOf(t, h, id); op.Phase != "done" {
				t.Fatalf("the resumed install: %s %s", op.Phase, op.Error)
			}
			starts.want(t, 1)
		})
	}
}

// An install that ends before its scan records it (lead-c22b item 2: a fast manager): the scan's
// kick after the record gives the pass that sees it ended, and the normal start runs once.
func TestPluginInstallEndsBeforeRecord(t *testing.T) {
	starts := noInstallStart(t)
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	if err := os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"notes","source":"github:acme/notes@v1.0.0"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opsLoadEntries(t, h)
	ctx := context.Background()
	h.pops.installs.recovered = true // a running hub
	h.pops.kick = make(chan struct{}, 1)
	old := installAdmitted
	installAdmitted = func(h *Hub, ctx context.Context) {
		<-h.pops.kick // the manager takes the kick of the admission, then runs its pass
		h.opsRun(ctx)
	}
	t.Cleanup(func() { installAdmitted = old })
	h.installScan(ctx, "start")
	if op := opsOf(t, h, mustLastOp(t, h)); op.Phase != "done" {
		t.Fatalf("the install: %s %s", op.Phase, op.Error)
	}
	starts.want(t, 0)
	select {
	case <-h.pops.kick:
	default:
		t.Fatal("the scan did not kick the manager after its record")
	}
	h.opsRun(ctx) // the pass of the kick
	starts.want(t, 1)
}
