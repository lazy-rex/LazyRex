package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noLock is the lock snapshot of a resolution without GitHub sources: an empty lock.
func noLock(cfgPath string) lockSnapshot {
	return lockSnapshot{cfgPath: cfgPath, lock: &pluginLock{Version: lockVersion, Plugins: map[string]*lockEntry{}}}
}

// load loads the config at p, with its entries.
func (env *cmdEnv) load(p string) Config {
	env.t.Helper()
	c, err := loadConfig(p, false)
	if err != nil {
		env.t.Fatal(err)
	}
	return c
}

// resolve resolves the entries of c with a fresh lock snapshot, as resolveLocked does.
func resolve(c *Config) []*configEntry {
	return resolveEntries(readLockSnapshot(c, c.entries), c.entries, nil)
}

// A synced GitHub entry resolves from its cache entry: the run dir is the read-only entry (or
// <entry>/<subdir>), the commit is the locked commit, and the graph rules apply to GitHub and local
// entries alike (section 7).
func TestResolveGithub(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging", "base")
	env.homeRepo()
	loc := env.local("base", "base")
	env.local("user", "user", "messaging")
	cfg := env.config(srcM, srcHome, loc, "user")
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	c := env.load(cfg)
	order := resolve(&c)
	for _, e := range c.entries {
		if e.err != nil || e.cand == nil {
			t.Fatalf("%s: %v", e.Source, e.err)
		}
	}
	var names []string
	for _, e := range order {
		names = append(names, e.name)
	}
	if got := strings.Join(names, ","); got != "lazyrex,base,messaging,user" && got != "base,lazyrex,messaging,user" && got != "base,messaging,lazyrex,user" && got != "base,messaging,user,lazyrex" {
		t.Errorf("order %s: dependencies first", got)
	}
	m := c.entries[0].cand
	if want := env.entryDir(srcM); m.Dir != want || m.Commit != r.git("rev-parse", "HEAD") || m.Path != filepath.Join(want, "main.lua") {
		t.Errorf("messaging run %+v, want dir %s", m, want)
	}
	if fi, err := os.Stat(m.Dir); err != nil || fi.Mode().Perm() != 0o555 {
		t.Errorf("the run dir is not the read-only cache entry: %v %v", fi.Mode(), err)
	}
	h := c.entries[1].cand
	if want := filepath.Join(env.entryDir(srcHome), "plugins", "lazyrex"); h.Dir != want || h.Commit != env.lock().Plugins[srcHome].Commit {
		t.Errorf("home run %+v, want dir %s", h, want)
	}
	if l := c.entries[2].cand; l.Commit != "" || l.Dir != filepath.Join(env.dir, "base") {
		t.Errorf("local run %+v", l)
	}
}

// Each lockedDir error is the entry error unchanged, and refuses the entries that depend on it.
// The independent local entries pass.
func TestResolveLockedDirErrors(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	loc := env.local("loc", "loc")
	env.local("dep", "dep", "messaging")
	cfg := env.config(srcM, loc, "dep")
	check := func(what, want string) {
		t.Helper()
		c := env.load(cfg)
		resolve(&c)
		if c.entries[0].err == nil || c.entries[0].err.Error() != want || c.entries[0].cand != nil {
			t.Errorf("%s: %v\nwant %s", what, c.entries[0].err, want)
		}
		if c.entries[1].err != nil || c.entries[1].cand == nil {
			t.Errorf("%s: the local entry: %v", what, c.entries[1].err)
		}
		if c.entries[2].err == nil || !strings.HasPrefix(c.entries[2].err.Error(), "plugin dep: dependency messaging is ") {
			t.Errorf("%s: the dependent entry: %v", what, c.entries[2].err)
		}
	}
	check("not installed", "plugin "+srcM+" is not installed: run lazyrex-hub plugin sync -config "+cfg)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	le := env.lock().Plugins[srcM]
	entry := env.entryDir(srcM)
	writeInto(entry, "main.lua", "changed")
	check("changed", "plugin messaging: the cache entry for commit "+le.Commit+" changed (digest mismatch): run lazyrex-hub plugin sync -config "+cfg)
	removeTree(entry)
	check("missing", "plugin messaging: the cache has no entry for commit "+le.Commit+": run lazyrex-hub plugin sync -config "+cfg)
	l := env.lock()
	l.Plugins[srcM].Ref = "other"
	env.writeLockRaw(l)
	check("agreement", lockMismatch(cfg, srcM, l.Plugins[srcM], githubEntry(t, srcM).githubRef(), "ref").Error())
}

// A lock file that does not parse, or cannot be read, refuses every GitHub entry with
// "plugin <source>: <the readLock text>". The local entries resolve as usual.
func TestResolveLockFileErrors(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.homeRepo()
	loc := env.local("loc", "loc")
	cfg := env.config(srcM, loc, srcHome)
	lockFile := filepath.Join(env.dir, "hub.lock.json")
	check := func(what, text string) {
		t.Helper()
		c := env.load(cfg)
		resolve(&c)
		for _, i := range []int{0, 2} {
			e := c.entries[i]
			if want := "plugin " + e.Source + ": " + text; e.err == nil || e.err.Error() != want {
				t.Errorf("%s: %v\nwant %s", what, e.err, want)
			}
		}
		if c.entries[1].err != nil || c.entries[1].cand == nil {
			t.Errorf("%s: the local entry: %v", what, c.entries[1].err)
		}
	}
	os.WriteFile(lockFile, []byte(`{"version":1,"plugins":{`), 0o600)
	check("malformed JSON", "the lock file "+lockFile+" is not valid: unexpected EOF; repair the file or restore it from a backup")
	os.WriteFile(lockFile, []byte(`{"version":2,"plugins":{}}`), 0o600)
	check("another version", "the lock file "+lockFile+" is not valid: version 2 is not supported (this hub reads version 1); repair the file or restore it from a backup")
	os.Remove(lockFile)
	if err := os.Mkdir(lockFile, 0o700); err != nil {
		t.Fatal(err)
	}
	check("a directory", "cannot read the lock file "+lockFile+": is a directory")
	os.Remove(lockFile)
	if os.Geteuid() != 0 {
		os.WriteFile(lockFile, []byte(`{"version":1,"plugins":{}}`), 0o000)
		check("no permission", "cannot read the lock file "+lockFile+": permission denied")
	}
}

// A config without GitHub entries resolves without the lock file: a broken lock changes nothing.
func TestResolveNoGithubReadsNoLock(t *testing.T) {
	env := newCmdEnv(t)
	cfg := env.config(env.local("loc", "loc"))
	os.WriteFile(filepath.Join(env.dir, "hub.lock.json"), []byte("{"), 0o600)
	c := env.load(cfg)
	if snap := readLockSnapshot(&c, c.entries); snap.err != nil {
		t.Fatalf("the snapshot read the lock: %v", snap.err)
	}
	resolve(&c)
	if c.entries[0].err != nil {
		t.Fatal(c.entries[0].err)
	}
}

// partBHub is a hub on the config at cfg, with a store, as hub start makes it (without Rex).
func partBHub(t *testing.T, c Config, db *DB) *Hub {
	t.Helper()
	return &Hub{cfg: c, entries: c.entries, plugins: map[string]*Plugin{}, db: db}
}

// Each production resolution (resolveLocked) reads the lock again; a run keeps its commit across a
// save and a load, until a spawn applies a candidate; hub.plugins reports the run commit and the
// candidate fields that differ.
func TestResolveLockedFreshAndRetention(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	old, oldDir := r.git("rev-parse", "HEAD"), env.entryDir(srcM)
	db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := partBHub(t, env.load(cfg), db)
	h.entriesMu.Lock()
	h.resolveLocked()
	h.entriesMu.Unlock()
	e, pl := h.entries[0], h.plugins["messaging"]
	if e.cand == nil || e.cand.Commit != old || pl == nil {
		t.Fatalf("first resolution: %v", e.err)
	}
	pl.run = *e.cand // as spawn does
	if err := h.savePlugin(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	if got := entryItem(t, h); got["commit"] != old || got["dir"] != oldDir || got["candidate"] != nil {
		t.Errorf("after the spawn: %v", got)
	}

	next := r.commit(map[string]string{"main.lua": "return 2\n"})
	if err := env.update(cfg); err != nil {
		t.Fatal(err)
	}
	newDir := env.entryDir(srcM)
	h.entriesMu.Lock()
	h.resolveLocked()
	h.entriesMu.Unlock()
	if e.cand == nil || e.cand.Commit != next || e.cand.Dir != newDir {
		t.Fatalf("the second resolution did not read the new lock: %+v %v", e.cand, e.err)
	}
	if pl.run.Commit != old {
		t.Errorf("a resolution changed the run commit to %s", pl.run.Commit)
	}
	got := entryItem(t, h)
	if c, _ := got["candidate"].(map[string]any); got["commit"] != old || c["commit"] != next || c["dir"] != newDir || len(c) != 2 {
		t.Errorf("after the update: %v", got)
	}

	// A new hub on the same store: the loaded run keeps the old commit, and the candidate is new.
	h2 := partBHub(t, env.load(cfg), db)
	h2.entriesMu.Lock()
	h2.resolveLocked()
	h2.entriesMu.Unlock()
	pl2 := h2.plugins["messaging"]
	if pl2.run.Commit != old || pl2.run.Dir != oldDir || h2.entries[0].cand.Commit != next {
		t.Errorf("after a load: run %+v, candidate %+v", pl2.run, h2.entries[0].cand)
	}
}

// entryItem gives the first entries item of hub.plugins.
func entryItem(t *testing.T, h *Hub) map[string]any {
	t.Helper()
	out, err := h.pluginsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var got struct{ Entries []map[string]any }
	json.Unmarshal(b, &got)
	return got.Entries[0]
}

// The candidate object (section 7): each differing field independently; nothing when neither
// differs or when there is no candidate.
func TestCandidateChange(t *testing.T) {
	run := &pluginRun{Commit: "c1", Dir: "/d1"}
	for _, tc := range []struct {
		cand *pluginRun
		want string
	}{
		{nil, "null"},
		{&pluginRun{Commit: "c1", Dir: "/d1"}, "null"},
		{&pluginRun{Commit: "c2", Dir: "/d1"}, `{"commit":"c2"}`},
		{&pluginRun{Commit: "c1", Dir: "/d2"}, `{"dir":"/d2"}`},
		{&pluginRun{Commit: "c2", Dir: "/d2"}, `{"commit":"c2","dir":"/d2"}`},
	} {
		if b, _ := json.Marshal(candidateChange(tc.cand, run)); string(b) != tc.want {
			t.Errorf("%+v: %s, want %s", tc.cand, b, tc.want)
		}
	}
	// A local source: the run and the candidate have no commit, so only dir can differ.
	if b, _ := json.Marshal(candidateChange(&pluginRun{Dir: "/d2"}, &pluginRun{Dir: "/d1"})); string(b) != `{"dir":"/d2"}` {
		t.Errorf("local: %s", b)
	}
}

// hub.plugins: a refused entry has only {"error"}; an entry without a run, or with a candidate equal
// to its run, has no candidate; a kept record reports its run commit.
func TestPluginsListCommit(t *testing.T) {
	h, _ := workerTestHub(t)
	cand := &pluginRun{Name: "b", Commit: "c2", Dir: "/d2"}
	es := []*configEntry{
		{index: 1, Source: "s1", name: "a", seen: "a", err: errorString("plugin a: refused")},
		{index: 2, Source: "s2", name: "b", seen: "b", cand: cand},
		{index: 3, Source: "s3", name: "c", seen: "c", cand: &pluginRun{Name: "c", Commit: "c3", Dir: "/d3"}},
	}
	h.entries = es
	h.plugins = map[string]*Plugin{
		"a": {Name: "a", entry: es[0], Desired: "running", run: pluginRun{Name: "a", Commit: "c1", Dir: "/d1"}},
		"b": {Name: "b", entry: es[1], Desired: "stopped"},
		"c": {Name: "c", entry: es[2], Desired: "stopped", run: pluginRun{Name: "c", Commit: "c3", Dir: "/d3"}},
		"k": {Name: "k", source: "sk", Desired: "running", run: pluginRun{Name: "k", Commit: "ck", Dir: "/dk"}},
	}
	out, err := h.pluginsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var got struct{ Entries, Kept []map[string]any }
	json.Unmarshal(b, &got)
	want := []string{
		`{"candidate":{"error":"plugin a: refused"},"commit":"c1","desired":"running","dir":"/d1","entry":1,"instance_id":"","instance_pending":false,"name":"a","registered":false,"source":"s1","state":"running"}`,
		`{"desired":"stopped","entry":2,"instance_id":"","instance_pending":false,"name":"b","registered":false,"source":"s2","state":"stopped"}`,
		`{"commit":"c3","desired":"stopped","dir":"/d3","entry":3,"instance_id":"","instance_pending":false,"name":"c","registered":false,"source":"s3","state":"stopped"}`,
	}
	for i, e := range got.Entries {
		if b, _ := json.Marshal(e); string(b) != want[i] {
			t.Errorf("entry %d: %s\nwant %s", i+1, b, want[i])
		}
	}
	if b, _ := json.Marshal(got.Kept); string(b) != `[{"commit":"ck","dir":"/dk","error":"","name":"k","source":"sk","state":"running"}]` {
		t.Errorf("kept %s", b)
	}
}

// A change between a GitHub and a local source (hub.plugins.reload of a managed entry) reports
// the commit, with null for a local candidate. The top-level commit is the run's (section 7).
func TestPluginsListKindChange(t *testing.T) {
	h, _ := workerTestHub(t)
	es := []*configEntry{
		{index: 1, Source: "/local/a", name: "a", seen: "a", cand: &pluginRun{Name: "a", Dir: "/local/a"}},
		{index: 2, Source: "/local/b", name: "b", seen: "b", cand: &pluginRun{Name: "b", Commit: "c2", Dir: "/cache/b"}},
	}
	h.entries = es
	h.plugins = map[string]*Plugin{
		"a": {Name: "a", entry: es[0], Desired: "stopped", run: pluginRun{Name: "a", Commit: "c1", Dir: "/cache/a"}},
		"b": {Name: "b", entry: es[1], Desired: "stopped", run: pluginRun{Name: "b", Dir: "/local/b"}},
	}
	out, err := h.pluginsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var got struct{ Entries []map[string]any }
	json.Unmarshal(b, &got)
	want := []string{
		`{"candidate":{"commit":null,"dir":"/local/a"},"commit":"c1","desired":"stopped","dir":"/cache/a","entry":1,"instance_id":"","instance_pending":false,"name":"a","registered":false,"source":"/local/a","state":"stopped"}`,
		`{"candidate":{"commit":"c2","dir":"/cache/b"},"desired":"stopped","dir":"/local/b","entry":2,"instance_id":"","instance_pending":false,"name":"b","registered":false,"source":"/local/b","state":"stopped"}`,
	}
	for i, e := range got.Entries {
		if b, _ := json.Marshal(e); string(b) != want[i] {
			t.Errorf("entry %d: %s\nwant %s", i+1, b, want[i])
		}
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
