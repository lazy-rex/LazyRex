package main

// P1 (the P1 plan r6): an automatic start (respawnDue, pluginExited, restartAfterSessionLoss,
// wakeWaiting) runs the selected run, the run that the last explicit start selected. It reads no
// pin: a lock change takes effect at the next explicit start. GitHub sources here are hand-made
// cache entries and locks; the tests do not prove GitHub behavior.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var (
	p1A = strings.Repeat("a", 40)
	p1B = strings.Repeat("b", 40)
)

// p1Manifest is a plugin tree at the root of an entry: the manifest of name, with depends.
func p1Manifest(name string, depends ...string) map[string]string {
	m := map[string]any{"name": name, "main": "main.lua"}
	if depends != nil {
		m["depends"] = depends
	}
	b, _ := json.Marshal(m)
	return map[string]string{"lazyrex.json": string(b), "main.lua": "return 1\n"}
}

// p1Source is the source of the GitHub plugin name in these tests.
func p1Source(name string) string { return "github:lazy-rex/" + name + "@main" }

// p1Publish publishes tree for github:lazy-rex/<repo> at commit, and gives its lock entry, with
// the manifest name name and the subdir subdir.
func p1Publish(t *testing.T, state, repo, subdir, name, commit string, tree map[string]string) *lockEntry {
	t.Helper()
	s, err := newStage(state, "lazy-rex", repo, commit)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range tree {
		full := filepath.Join(s.Dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rel, err := s.publish()
	if err != nil {
		t.Fatal(err)
	}
	d, err := dirDigest(filepath.Join(state, "plugins", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return &lockEntry{lockPin: lockPin{Name: name, Owner: "lazy-rex", Repo: repo, Subdir: subdir, Ref: "main", RefKind: "branch", Commit: commit, Digest: d}, Dir: rel}
}

// p1Hub is a hub without Rex, with a work session. Each github key is a GitHub plugin at commit
// p1A (a root plugin: its tree); each local key is a local plugin directory with that manifest.
// The plugins field lists the GitHub plugins first, in name order. A spawn sends nothing: the
// test arms a refused create for each spawn (p1Arm), and the run that the spawn saved stays in
// pl.run.
func p1Hub(t *testing.T, github map[string]map[string]string, local map[string]string) (*Hub, *fakeClock) {
	t.Helper()
	dir := cacheTemp(t)
	lock := &pluginLock{Version: 1, Plugins: map[string]*lockEntry{}}
	var entries []string
	names := make([]string, 0, len(github))
	for n := range github {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		lock.Plugins[p1Source(n)] = p1Publish(t, dir, n, "", n, p1A, github[n])
		entries = append(entries, `{"source":"`+p1Source(n)+`"}`)
	}
	for n, m := range local {
		writePlugin(t, filepath.Join(dir, n), m)
		entries = append(entries, `{"source":"`+filepath.Join(dir, n)+`"}`)
	}
	b, err := encodeLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub.lock.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := writeConfig(t, dir, "["+strings.Join(entries, ",")+"]")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := appModeHub(t)
	h.cfg, h.entries, h.plugins, h.selfClosed = c, c.entries, map[string]*Plugin{}, map[string]bool{}
	h.faults = &faults{}
	h.test = &testState{}
	h.ui.sess["session:home"] = uiSessionKey
	h.vs = &viewSvc{dirty: map[string]bool{}, workers: map[string]chan struct{}{}}
	call := changedCall
	t.Cleanup(func() { changedCall = call })
	t.Cleanup(func() { waitIdle(t, h) }) // no delivery outlives the fake
	t.Cleanup(func() {
		// Runs first: a later h.changed (a goroutine of a start) finds no client to deliver to.
		h.mu.Lock()
		for _, pl := range h.plugins {
			pl.ClientID = ""
		}
		h.mu.Unlock()
	})
	changedCall = func(context.Context, *Hub, string, string) error { return nil }
	h.entriesMu.Lock()
	h.resolveLocked()
	h.entriesMu.Unlock()
	clock := &fakeClock{}
	clock.install(h)
	return h, clock
}

// p1Pin publishes tree for the GitHub plugin name at commit, and makes the lock name it (a CLI
// update).
func p1Pin(t *testing.T, h *Hub, name, commit string, tree map[string]string) {
	t.Helper()
	l := readTestLock(t, h)
	l.Plugins[p1Source(name)] = p1Publish(t, h.cfg.State, name, "", name, commit, tree)
	writeTestLock(t, h, l)
}

// p1Arm makes the next block create fail as a Rex refusal: a known spawn failure after the run
// is saved.
func p1Arm(h *Hub) {
	h.test.mu.Lock()
	h.test.faults = append(h.test.faults, &testFault{Point: "plugin-create", Mode: "refuse", Nth: 1})
	h.test.mu.Unlock()
}

// p1Start is an explicit start of pl (hub.admin.start's start path) with one armed create.
func p1Start(t *testing.T, h *Hub, pl *Plugin) error {
	t.Helper()
	p1Arm(h)
	pl.lifecycle.Lock()
	defer pl.lifecycle.Unlock()
	return h.startPlugin(context.Background(), pl)
}

// p1Spawned checks that err is the refused create of a spawn, and that the spawn's run has commit.
func p1Spawned(t *testing.T, h *Hub, pl *Plugin, err error, commit string) {
	t.Helper()
	var se *spawnError
	if !errors.As(err, &se) || se.stage != "create" {
		t.Fatalf("no spawn: %v", err)
	}
	h.mu.Lock()
	got := pl.run.Commit
	h.mu.Unlock()
	if got != commit {
		t.Fatalf("the spawn ran commit %s, want %s", got, commit)
	}
}

// p1Register makes pl registered (a live client), as hub.register does.
func p1Register(h *Hub, pl *Plugin) {
	h.mu.Lock()
	pl.ClientID, pl.ChildPID, pl.ChildStart = "client:"+pl.Name, os.Getpid(), startTime(os.Getpid())
	h.mu.Unlock()
}

// p1Lose is a session loss of pl's instance, and the restart after it (an automatic start). It
// returns the restart's start error, through the log of restartAfterSessionLoss.
func p1Lose(t *testing.T, h *Hub, pl *Plugin) string {
	t.Helper()
	p1Arm(h)
	h.dropInstance(context.Background(), pl, "session:lost", "session closed")
	l := captureLog(t)
	h.restartAfterSessionLoss(pl, "session:lost")
	return l.String()
}

func p1State(h *Hub, pl *Plugin) (state, refusal string, waiting []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return pl.reportedState(), pl.refusal, slices.Clone(pl.waiting)
}

// U1: the selected run's cache entry is gone at a crash respawn: a coded refusal, state refused,
// and no further respawn.
func TestSelectedRunGone(t *testing.T) {
	h, clock := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p")}, nil)
	pl := h.plugins["p"]
	p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
	running(t, h, pl, "p-1")
	exited(h, pl, context.Background(), `{"exit_code":1}`)
	removeTree(filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(readTestLock(t, h).Plugins[p1Source("p")].Dir)))
	dropForTest(h, pl)
	tm, n := clock.last(t)
	tm.f()
	state, refusal, _ := p1State(h, pl)
	if want := "plugin p: run " + p1A + " is no longer available; start it again"; state != "refused" || refusal != want {
		t.Fatalf("state %s, refusal %q; want refused, %q", state, refusal, want)
	}
	if clock.count() != n {
		t.Fatalf("a respawn after the refusal: %d timers", clock.count())
	}
	// plugin_respawn is the attempt (as in TestRespawnRefusedStartKeepsTimer): the refused attempt
	// gives one, and no event comes after the refusal.
	if got := len(eventsNamed(t, h.ev.f.Name(), "plugin_respawn")); got != 1 {
		t.Fatalf("%d plugin_respawn events, want 1 (the refused attempt)", got)
	}
}

// U2: a waiting explicit start at A; the lock moves to B; the dependency registers: the wake
// spawns A.
func TestSelectedRunWake(t *testing.T) {
	h, _ := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p", "d")}, map[string]string{"d": `{"name":"d","main":"p.lua"}`})
	pl := h.plugins["p"]
	if err := p1Start(t, h, pl); err != nil {
		t.Fatal(err)
	}
	if state, _, waiting := p1State(h, pl); state != "waiting" || !slices.Equal(waiting, []string{"d"}) {
		t.Fatalf("state %s, waiting %v", state, waiting)
	}
	p1Pin(t, h, "p", p1B, p1Manifest("p", "d"))
	p1Register(h, h.plugins["d"])
	p1Arm(h)
	h.wakeWaiting(context.Background())
	h.mu.Lock()
	got := pl.run.Commit
	h.mu.Unlock()
	if got != p1A {
		t.Fatalf("the wake ran commit %q, want %s", got, p1A)
	}
}

// U3: a session loss with pin B in the lock: the restart spawns A.
func TestSelectedRunSessionLoss(t *testing.T) {
	h, _ := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p")}, nil)
	pl := h.plugins["p"]
	p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
	running(t, h, pl, "p-1")
	p1Pin(t, h, "p", p1B, p1Manifest("p"))
	p1Lose(t, h, pl)
	h.mu.Lock()
	got := pl.run.Commit
	h.mu.Unlock()
	if got != p1A {
		t.Fatalf("the restart ran commit %q, want %s", got, p1A)
	}
}

// U4: a known spawn failure at a crash respawn, with pin B in the lock: the backoff retry spawns
// A again.
func TestSelectedRunRetry(t *testing.T) {
	h, clock := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p")}, nil)
	pl := h.plugins["p"]
	p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
	running(t, h, pl, "p-1")
	exited(h, pl, context.Background(), `{"exit_code":1}`)
	p1Pin(t, h, "p", p1B, p1Manifest("p"))
	dropForTest(h, pl)
	for i := range 2 {
		tm, _ := clock.last(t)
		p1Arm(h)
		tm.f()
		h.mu.Lock()
		got := pl.run.Commit
		h.mu.Unlock()
		if got != p1A {
			t.Fatalf("respawn %d ran commit %q, want %s", i+1, got, p1A)
		}
	}
}

// U5: the cycle among the selected runs. a's selected run (A) depends on b. a's new pin drops
// that edge, and b's new pin adds b -> a; an explicit start of b selects b's new run. At a
// session loss both refuse with the cycle, and neither waits.
func TestSelectedRunCycle(t *testing.T) {
	h, _ := p1Hub(t, map[string]map[string]string{"a": p1Manifest("a", "b"), "b": p1Manifest("b")}, nil)
	a, b := h.plugins["a"], h.plugins["b"]
	p1Spawned(t, h, b, p1Start(t, h, b), p1A)
	p1Register(h, b)
	p1Spawned(t, h, a, p1Start(t, h, a), p1A)
	p1Register(h, a)
	p1Pin(t, h, "a", p1B, p1Manifest("a"))
	p1Pin(t, h, "b", p1B, p1Manifest("b", "a"))
	h.mu.Lock()
	b.ClientID = ""
	h.mu.Unlock()
	p1Spawned(t, h, b, p1Start(t, h, b), p1B)
	h.mu.Lock()
	a.ClientID = ""
	h.mu.Unlock()
	for _, c := range []struct {
		pl    *Plugin
		cycle string
	}{{a, "a -> b -> a"}, {b, "b -> a -> b"}} {
		p1Lose(t, h, c.pl)
		state, refusal, _ := p1State(h, c.pl)
		if want := "plugin " + c.pl.Name + ": dependency cycle among the selected runs (" + c.cycle + "); start it explicitly"; state != "refused" || refusal != want {
			t.Errorf("%s: state %s, refusal %q; want refused, %q", c.pl.Name, state, refusal, want)
		}
	}
}

// U6: the dependency states of a selected run at an automatic start.
func TestSelectedRunDependencies(t *testing.T) {
	for _, c := range []struct {
		name           string
		dep            func(h *Hub, d *Plugin)
		state, refusal string
		waiting        []string
		spawned        bool
	}{
		{"missing", func(h *Hub, d *Plugin) { delete(h.plugins, "d") }, "refused", "plugin p: dependency d of run " + p1A + " is not available", nil, false},
		{"not registered", func(h *Hub, d *Plugin) { d.ClientID = "" }, "waiting", "", []string{"d"}, false},
		{"refused", func(h *Hub, d *Plugin) { d.ClientID, d.state = "", "refused" }, "waiting", "waiting for d (refused)", []string{"d"}, false},
		{"stopped", func(h *Hub, d *Plugin) { d.ClientID, d.Desired = "", "stopped" }, "waiting", "waiting for d (stopped)", []string{"d"}, false},
		{"registered at another pin", func(h *Hub, d *Plugin) { d.run.Commit = p1B }, "running", "", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p", "d")}, map[string]string{"d": `{"name":"d","main":"p.lua"}`})
			pl, d := h.plugins["p"], h.plugins["d"]
			p1Register(h, d)
			p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
			h.mu.Lock()
			c.dep(h, d)
			h.mu.Unlock()
			p1Lose(t, h, pl)
			state, refusal, waiting := p1State(h, pl)
			if state != c.state || refusal != c.refusal || !slices.Equal(waiting, c.waiting) {
				t.Fatalf("state %s, refusal %q, waiting %v; want %s, %q, %v", state, refusal, waiting, c.state, c.refusal, c.waiting)
			}
			h.mu.Lock()
			got := pl.run.Commit
			h.mu.Unlock()
			if c.spawned != (got == p1A) {
				t.Fatalf("the spawn: commit %q", got)
			}
		})
	}
}

// U7: the current pin's manifest does not read: the restart spawns the selected run.
func TestSelectedRunBadPin(t *testing.T) {
	h, _ := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p")}, nil)
	pl := h.plugins["p"]
	p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
	p1Pin(t, h, "p", p1B, map[string]string{"lazyrex.json": "{", "main.lua": ""})
	p1Lose(t, h, pl)
	h.mu.Lock()
	got := pl.run.Commit
	h.mu.Unlock()
	if got != p1A {
		t.Fatalf("the restart ran commit %q, want %s", got, p1A)
	}
}

// U8: a subdir plugin: the run check covers the whole entry. An unchanged entry restarts from
// <entry>/<subdir>; a changed sibling file refuses.
func TestSelectedRunSubdir(t *testing.T) {
	for _, changed := range []bool{false, true} {
		h, _ := p1Hub(t, nil, nil)
		src := "github:lazy-rex/home/p@main"
		tree := p1Manifest("p")
		tree = map[string]string{"p/lazyrex.json": tree["lazyrex.json"], "p/main.lua": tree["main.lua"], "lib/x.lua": "return 2\n"}
		le := p1Publish(t, h.cfg.State, "home", "p", "p", p1A, tree)
		writeTestLock(t, h, &pluginLock{Version: 1, Plugins: map[string]*lockEntry{src: le}})
		c, err := writeConfig(t, h.cfg.State, `[{"source":"`+src+`"}]`)
		if err != nil {
			t.Fatal(err)
		}
		h.cfg, h.entries = c, c.entries
		h.entriesMu.Lock()
		h.resolveLocked()
		h.entriesMu.Unlock()
		pl := h.plugins["p"]
		p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
		root := filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(le.Dir))
		if changed {
			os.Chmod(filepath.Join(root, "lib"), 0o755)
			os.Chmod(filepath.Join(root, "lib", "x.lua"), 0o644)
			if err := os.WriteFile(filepath.Join(root, "lib", "x.lua"), []byte("return 3\n"), 0o444); err != nil {
				t.Fatal(err)
			}
		}
		p1Lose(t, h, pl)
		state, refusal, _ := p1State(h, pl)
		h.mu.Lock()
		dir := pl.run.Dir
		h.mu.Unlock()
		if changed {
			if want := "plugin p: run " + p1A + " is no longer available; start it again"; state != "refused" || refusal != want {
				t.Fatalf("changed: state %s, refusal %q", state, refusal)
			}
		} else if dir != filepath.Join(root, "p") {
			t.Fatalf("unchanged: the restart ran from %q", dir)
		}
	}
}

// U9: hub start clears every selected run before recovery. H selected A; OLD kept the column;
// H starts, and recovery refuses the current pin B: a later automatic start refuses, and A never
// runs.
func TestSelectedRunStartupClear(t *testing.T) {
	h, _ := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p")}, nil)
	pl := h.plugins["p"]
	p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
	var sel string
	if err := h.db.QueryRow(`SELECT selected_run FROM plugins WHERE name = 'p'`).Scan(&sel); err != nil || sel == "" {
		t.Fatalf("no selected run: %q %v", sel, err)
	}
	if err := h.loadKept(context.Background()); err != nil { // the hub start
		t.Fatal(err)
	}
	p1Pin(t, h, "p", p1B, map[string]string{"lazyrex.json": "{", "main.lua": ""})
	pl.lifecycle.Lock()
	err := h.startPlugin(context.Background(), pl) // recovery
	pl.lifecycle.Unlock()
	if codeOf(err) != "refused" {
		t.Fatalf("recovery: %v", err)
	}
	var before int
	h.db.QueryRow(`SELECT count(*) FROM spawn_intents`).Scan(&before)
	p1Lose(t, h, pl)
	state, refusal, _ := p1State(h, pl)
	var after int
	h.db.QueryRow(`SELECT count(*) FROM spawn_intents`).Scan(&after)
	if want := "plugin p: no run is recorded; start it again"; state != "refused" || refusal != want || after != before {
		t.Fatalf("state %s, refusal %q, spawns %d -> %d", state, refusal, before, after)
	}
}

// U11: a run is a copy: a change of the entry's Args after the start does not reach it.
func TestSelectedRunCopy(t *testing.T) {
	h, _ := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p")}, nil)
	pl := h.plugins["p"]
	h.entriesMu.Lock()
	pl.entry.Args = map[string]string{"k": "1"}
	h.resolveLocked()
	h.entriesMu.Unlock()
	p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
	h.entriesMu.Lock()
	pl.entry.Args["k"] = "2"
	h.entriesMu.Unlock()
	h.mu.Lock()
	got := pl.run.Args["k"]
	h.mu.Unlock()
	if got != "1" {
		t.Fatalf("the run's argument k is %q, want 1", got)
	}
}

// U12: a selected run without an entry directory (a run from before P1) refuses once; the next
// explicit start stores the entry.
func TestSelectedRunNoEntry(t *testing.T) {
	h, _ := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p")}, nil)
	pl := h.plugins["p"]
	p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
	h.mu.Lock()
	var old map[string]any
	b, _ := json.Marshal(pl.run)
	json.Unmarshal(b, &old)
	delete(old, "entry")
	sel, _ := json.Marshal(map[string]any{"entry": entryIdentity(pl.entry), "run": old})
	h.mu.Unlock()
	if _, err := h.db.Exec(`UPDATE plugins SET selected_run = ? WHERE name = 'p'`, string(sel)); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.readPluginRow(context.Background(), pl) // the record reads its row again, as a start does
	h.mu.Unlock()
	p1Lose(t, h, pl)
	if state, refusal, _ := p1State(h, pl); state != "refused" || refusal != "plugin p: run "+p1A+" is no longer available; start it again" {
		t.Fatalf("state %s, refusal %q", state, refusal)
	}
	p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
	var run string
	h.db.QueryRow(`SELECT run FROM plugins WHERE name = 'p'`).Scan(&run)
	if want := `"entry":"` + readTestLock(t, h).Plugins[p1Source("p")].Dir + `"`; !strings.Contains(run, want) {
		t.Fatalf("the run %s has no %s", run, want)
	}
}

// p1Local is a hub with the local plugin p (its source is <state>/p) and its record.
func p1Local(t *testing.T) (*Hub, *Plugin, string) {
	t.Helper()
	h, _ := p1Hub(t, nil, map[string]string{"p": `{"name":"p","main":"p.lua"}`})
	return h, h.plugins["p"], filepath.Join(h.cfg.State, "p")
}

// p1Run is pl's run.
func p1Run(h *Hub, pl *Plugin) pluginRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return pl.run
}

// p1Entries lists the published local snapshots.
func p1Entries(t *testing.T, h *Hub) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(h.cfg.State, "plugins", "local", "[0-9a-f]*"))
	for i := range m {
		m[i] = "local/" + filepath.Base(m[i])
	}
	return m
}

// p1Edit writes a file of the local source.
func p1Edit(t *testing.T, src, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// U10: an explicit start of a local source runs a published snapshot of the source.
func TestLocalSnapshot(t *testing.T) {
	t.Run("published", func(t *testing.T) {
		h, pl, src := p1Local(t)
		os.MkdirAll(filepath.Join(src, ".git"), 0o755)
		p1Edit(t, filepath.Join(src, ".git"), "config", "x")
		p1Spawned(t, h, pl, p1Start(t, h, pl), "")
		run := p1Run(h, pl)
		dir := filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(run.Entry))
		d, err := dirDigest(dir)
		if !strings.HasPrefix(run.Entry, "local/"+strings.TrimPrefix(run.Digest, "sha256:")+".") || err != nil || d != run.Digest ||
			run.Dir != dir || run.Path != filepath.Join(dir, "p.lua") {
			t.Fatalf("the run %+v (digest %s %v)", run, d, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the snapshot has .git: %v", err)
		}
		if fi, err := os.Stat(run.Path); err != nil || fi.Mode().Perm() != 0o444 {
			t.Fatalf("the snapshot file: %v %v", fi.Mode(), err)
		}
	})
	t.Run("reuse", func(t *testing.T) {
		h, pl, _ := p1Local(t)
		p1Start(t, h, pl)
		first := p1Run(h, pl).Entry
		p1Start(t, h, pl)
		if got := p1Run(h, pl).Entry; got != first || len(p1Entries(t, h)) != 1 {
			t.Fatalf("%s, then %s; entries %v", first, got, p1Entries(t, h))
		}
	})
	t.Run("an entry that does not verify", func(t *testing.T) {
		h, pl, _ := p1Local(t)
		p1Start(t, h, pl)
		first := p1Run(h, pl)
		os.Chmod(first.Dir, 0o755)
		os.Chmod(first.Path, 0o644)
		p1Edit(t, first.Dir, "p.lua", "-- changed\n")
		p1Start(t, h, pl)
		got := p1Run(h, pl)
		if b, _ := os.ReadFile(first.Path); got.Entry == first.Entry || got.Digest != first.Digest || string(b) != "-- changed\n" {
			t.Fatalf("%s, then %s; the bad entry has %q", first.Entry, got.Entry, b)
		}
	})
	for _, c := range []struct {
		name, want string
		setup      func(t *testing.T, src string)
	}{
		{"symlink", "plugin p: link is not a regular file; a local source is copied, and the copy has no symlinks", func(t *testing.T, src string) {
			if err := os.Symlink("p.lua", filepath.Join(src, "link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"bound", "plugin p: the source SRC is over the copy bound (1 files, 67108864 bytes)", func(t *testing.T, src string) {
			n := localSnapshotFiles
			localSnapshotFiles = 1
			t.Cleanup(func() { localSnapshotFiles = n })
		}},
		{"the source changed", "plugin p: the source changed during the start; start it again", func(t *testing.T, src string) {
			localSnapshotHook = func() { p1Edit(t, src, "lazyrex.json", `{"name":"p","main":"p.lua","depends":["x"]}`) }
			t.Cleanup(func() { localSnapshotHook = nil })
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, pl, src := p1Local(t)
			c.setup(t, src)
			err := p1Start(t, h, pl)
			want := strings.ReplaceAll(c.want, "SRC", src)
			state, refusal, _ := p1State(h, pl)
			if codeOf(err) != "refused" || state != "refused" || refusal != want {
				t.Fatalf("%v; state %s, refusal %q, want %q", err, state, refusal, want)
			}
			if m, _ := filepath.Glob(filepath.Join(h.cfg.State, "plugins", "local", ".stage-*")); len(m) != 0 {
				t.Fatalf("a stage is left: %v", m)
			}
		})
	}
}

// U13: a start that fails after the publication stores no selected run: the record keeps its
// earlier one, the lease is released, and no row names the new entry.
func TestLocalSnapshotLateFailure(t *testing.T) {
	for _, c := range []string{"the source changed", "the save fails"} {
		t.Run(c, func(t *testing.T) {
			h, pl, src := p1Local(t)
			p1Spawned(t, h, pl, p1Start(t, h, pl), "")
			var before string
			h.db.QueryRow(`SELECT selected_run FROM plugins WHERE name = 'p'`).Scan(&before)
			first := p1Run(h, pl).Entry
			if c == "the source changed" {
				localSnapshotHook = func() { p1Edit(t, src, "lazyrex.json", `{"name":"p","main":"p.lua","depends":["x"]}`) }
				t.Cleanup(func() { localSnapshotHook = nil })
			} else {
				p1Edit(t, src, "p.lua", "-- new\n")
				if _, err := h.db.Exec(`CREATE TRIGGER refuse_run BEFORE UPDATE OF run ON plugins WHEN NEW.run != OLD.run AND NEW.run != ''
					BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err := p1Start(t, h, pl)
			var se *spawnError
			if c == "the save fails" && (!errors.As(err, &se) || se.stage != "save") || c != "the save fails" && codeOf(err) != "refused" {
				t.Fatalf("the start: %v", err)
			}
			var after, run string
			h.db.QueryRow(`SELECT selected_run, run FROM plugins WHERE name = 'p'`).Scan(&after, &run)
			if after != before {
				t.Fatalf("the selected run changed:\n%s\n%s", before, after)
			}
			if !pruneTry(t, h.cfg.State) {
				t.Fatal("the start lease is held")
			}
			var fresh []string
			for _, e := range p1Entries(t, h) {
				if e != first {
					fresh = append(fresh, e)
				}
			}
			if len(fresh) != 1 || strings.Contains(after+run, fresh[0]) {
				t.Fatalf("the new entries %v; the row: %s %s", fresh, after, run)
			}
		})
	}
}

// hub.plugins: commit is the running commit, and pinned_commit is the current lock's commit (""
// for a local entry). After a CLI update to B, a crash keeps A running: commit A, pinned_commit B.
func TestSelectedRunPinnedCommit(t *testing.T) {
	h, _ := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p")}, map[string]string{"q": `{"name":"q","main":"p.lua"}`})
	pl := h.plugins["p"]
	p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
	p1Pin(t, h, "p", p1B, p1Manifest("p"))
	p1Lose(t, h, pl)
	out, err := h.pluginsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var got struct{ Entries []map[string]any }
	json.Unmarshal(b, &got)
	if len(got.Entries) != 2 || got.Entries[0]["commit"] != p1A || got.Entries[0]["pinned_commit"] != p1B {
		t.Fatalf("the GitHub entry: %v", got.Entries)
	}
	if v, ok := got.Entries[1]["pinned_commit"]; !ok || v != "" {
		t.Fatalf("the local entry: %v", got.Entries[1])
	}
}

// A lock that does not read gives pinned_commit "" in hub.plugins: the listing never reads a
// missing lock (codex-27, plugins-p1-source-findings-1).
func TestPinnedCommitBadLock(t *testing.T) {
	h, _ := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p")}, nil)
	if err := os.WriteFile(filepath.Join(h.cfg.State, "hub.lock.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var item map[string]any
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("hub.plugins panicked: %v", r)
			}
		}()
		item = entryItem(t, h)
	}()
	if v, ok := item["pinned_commit"]; !ok || v != "" {
		t.Fatalf("pinned_commit %v (present %v)", v, ok)
	}
}

// An explicit start that waits and cannot save its selection keeps the earlier selected run, in
// memory too: a later save of the record does not store the failed selection (codex-27,
// plugins-p1-source-findings-1).
func TestSelectedRunWaitSaveFails(t *testing.T) {
	h, _ := p1Hub(t, map[string]map[string]string{"p": p1Manifest("p", "d")}, map[string]string{"d": `{"name":"d","main":"p.lua"}`})
	pl, d := h.plugins["p"], h.plugins["d"]
	p1Register(h, d)
	p1Spawned(t, h, pl, p1Start(t, h, pl), p1A)
	var before string
	h.db.QueryRow(`SELECT selected_run FROM plugins WHERE name = 'p'`).Scan(&before)
	h.mu.Lock()
	d.ClientID = ""
	h.mu.Unlock()
	p1Pin(t, h, "p", p1B, p1Manifest("p", "d"))
	if _, err := h.db.Exec(`CREATE TRIGGER refuse_sel BEFORE UPDATE OF selected_run ON plugins WHEN NEW.selected_run != OLD.selected_run
		BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := p1Start(t, h, pl); err == nil || !strings.Contains(err.Error(), "the selected run is not saved") {
		t.Fatalf("the waiting start: %v", err)
	}
	if _, err := h.db.Exec(`DROP TRIGGER refuse_sel`); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.savePlugin(context.Background(), pl) // any later save of the record
	h.mu.Unlock()
	var after string
	h.db.QueryRow(`SELECT selected_run FROM plugins WHERE name = 'p'`).Scan(&after)
	if after != before {
		t.Fatalf("the failed selection was stored:\n%s\n%s", before, after)
	}
}

// The byte bound counts the bytes that the copy wrote: a file that grows after its stat still
// refuses the start, and no stage is left (codex-27, plugins-p1-deviations-source-review).
func TestLocalSnapshotGrowingFile(t *testing.T) {
	h, pl, src := p1Local(t)
	n, hook := localSnapshotBytes, localCopyHook
	localSnapshotBytes = 100
	localCopyHook = func(p string) {
		if filepath.Base(p) == "p.lua" {
			p1Edit(t, src, "p.lua", strings.Repeat("x", 200))
		}
	}
	t.Cleanup(func() { localSnapshotBytes, localCopyHook = n, hook })
	err := p1Start(t, h, pl)
	state, refusal, _ := p1State(h, pl)
	if want := "plugin p: the source " + src + " is over the copy bound (10000 files, 100 bytes)"; codeOf(err) != "refused" || refusal != want {
		t.Fatalf("%v; state %s, refusal %q", err, state, refusal)
	}
	if m, _ := filepath.Glob(filepath.Join(h.cfg.State, "plugins", "local", "*")); len(m) != 0 {
		t.Fatalf("left: %v", m)
	}
}

// A live retained local instance and a start with a source-only expect_run (a local expect_run has
// no commit and no digest): the run, with its snapshot digest, is the expected run, so the start
// keeps the instance and spawns nothing (codex-27, plugins-p1-deviations-source-review).
func TestLocalSnapshotExpectRunKeeps(t *testing.T) {
	h, pl, src := p1Local(t)
	p1Spawned(t, h, pl, p1Start(t, h, pl), "")
	h.mu.Lock()
	pl.state, pl.ClientID, pl.InstanceID, pl.BlockID = "retained", "client:p", "p-1", "block:p-1"
	pl.ChildPID, pl.ChildStart = os.Getpid(), startTime(os.Getpid())
	h.savePlugin(context.Background(), pl)
	run := pl.run
	h.mu.Unlock()
	var before int
	h.db.QueryRow(`SELECT count(*) FROM spawn_intents`).Scan(&before)
	pl.lifecycle.Lock()
	err := h.startPluginExpect(context.Background(), pl, true, &expectRun{Source: src})
	pl.lifecycle.Unlock()
	var after int
	h.db.QueryRow(`SELECT count(*) FROM spawn_intents`).Scan(&after)
	state, _, _ := p1State(h, pl)
	if err != nil || after != before || state != "running" || p1Run(h, pl).Digest != run.Digest {
		t.Fatalf("%v; spawns %d -> %d, state %s, run %+v", err, before, after, state, p1Run(h, pl))
	}
}
