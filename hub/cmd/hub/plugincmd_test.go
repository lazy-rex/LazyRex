package main

// Tests of the plugin commands. GitHub sources are local repositories through file:// URLs (the
// internal remote option). They do not prove GitHub behavior.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cmdEnv is a config directory, a state directory and the GitHub repositories of one test.
type cmdEnv struct {
	t        *testing.T
	dir      string // the config directory
	state    string
	repos    map[string]*testRepo // "owner/repo" to the repository
	opts     pluginCmdOpts
	cfgState string // the state field of the config, when it differs from state (a relative state)
}

func newCmdEnv(t *testing.T) *cmdEnv {
	t.Helper()
	gitTestEnv(t)
	env := &cmdEnv{t: t, dir: t.TempDir(), state: cacheTemp(t), repos: map[string]*testRepo{}}
	env.opts.remote = func(owner, repo string) string {
		if r := env.repos[owner+"/"+repo]; r != nil {
			return r.url()
		}
		return "file:///nonexistent/" + owner + "/" + repo
	}
	return env
}

// repo makes the repository owner/repo with a manifest for name (and depends) on main, and gives
// it.
func (env *cmdEnv) repo(owner, repo, name string, depends ...string) *testRepo {
	env.t.Helper()
	r := newTestRepo(env.t)
	env.repos[owner+"/"+repo] = r
	r.commit(map[string]string{"lazyrex.json": manifestJSON(name, depends...), "main.lua": "return 1\n"})
	return r
}

func manifestJSON(name string, depends ...string) string {
	m := map[string]any{"name": name, "main": "main.lua"}
	if len(depends) > 0 {
		m["depends"] = depends
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// config writes hub.json with these sources and gives its path.
func (env *cmdEnv) config(sources ...string) string {
	env.t.Helper()
	var plugins []map[string]any
	for _, s := range sources {
		plugins = append(plugins, map[string]any{"source": s})
	}
	st := env.state
	if env.cfgState != "" {
		st = env.cfgState
	}
	b, _ := json.Marshal(map[string]any{"server": "unix:///tmp/lrt-x/rex.sock", "state": st,
		"agents": []map[string]string{{"id": "user", "name": "User"}}, "plugins": plugins})
	p := filepath.Join(env.dir, "hub.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		env.t.Fatal(err)
	}
	return p
}

// local makes a local plugin directory sub with a manifest for name.
func (env *cmdEnv) local(sub, name string, depends ...string) string {
	plugDir(env.t, env.dir, sub, manifestJSON(name, depends...), "main.lua")
	return sub
}

func (env *cmdEnv) sync(cfg string) error {
	return pluginSync(context.Background(), cfg, env.opts)
}

func (env *cmdEnv) update(cfg string, args ...string) error {
	return pluginUpdate(context.Background(), cfg, args, env.opts)
}

func (env *cmdEnv) lockBytes() []byte {
	b, err := os.ReadFile(filepath.Join(env.dir, "hub.lock.json"))
	if err != nil && !os.IsNotExist(err) {
		env.t.Fatal(err)
	}
	return b
}

func (env *cmdEnv) lock() *pluginLock {
	env.t.Helper()
	l, err := readLock(filepath.Join(env.dir, "hub.lock.json"))
	if err != nil {
		env.t.Fatal(err)
	}
	return l
}

// writeLockRaw replaces the lock file with a JSON value (for broken and partial locks).
func (env *cmdEnv) writeLockRaw(v any) {
	env.t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(env.dir, "hub.lock.json"), b, 0o600); err != nil {
		env.t.Fatal(err)
	}
}

// treeSnapshot gives every path, mode and content under dir, to show that a directory did not change.
func treeSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		fi, _ := d.Info()
		c := ""
		if d.Type().IsRegular() {
			x, _ := os.ReadFile(p)
			c = string(x)
		}
		fmt.Fprintf(&b, "%s %o %q\n", p, fi.Mode(), c)
		return nil
	})
	return b.String()
}

func (env *cmdEnv) entryDir(src string) string {
	return filepath.Join(env.state, "plugins", filepath.FromSlash(env.lock().Plugins[src].Dir))
}

const srcM = "github:lazy-rex/Messaging@main"

// sync installs a new GitHub source: every field, and an entry that lockedDir accepts. A second
// sync changes nothing.
func TestSyncInstalls(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	le := env.lock().Plugins[srcM]
	head := r.git("rev-parse", "HEAD")
	if le == nil || le.Name != "messaging" || le.Owner != "lazy-rex" || le.Repo != "Messaging" || le.Ref != "main" ||
		le.RefKind != "branch" || le.Commit != head || !lockDigest.MatchString(le.Digest) {
		t.Fatalf("lock entry %+v", le)
	}
	entries, _ := parseEntries(env.dir, []json.RawMessage{json.RawMessage(`{"source":"` + srcM + `"}`)})
	dir, commit, err := lockedDir(cfg, env.state, env.lock(), entries[0])
	if err != nil || commit != head || !strings.HasSuffix(dir, filepath.FromSlash(le.Dir)) {
		t.Fatalf("lockedDir: %s %s %v", dir, commit, err)
	}
	before := env.lockBytes()
	r.commit(map[string]string{"main.lua": "moved"})
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, env.lockBytes()) {
		t.Fatal("a second sync changed the lock (sync never moves a locked commit)")
	}
	left, _ := os.ReadDir(filepath.Join(env.state, "plugins", ".tmp"))
	if len(left) != 0 {
		t.Fatalf("temporary directories left: %v", left)
	}
}

// A config without GitHub sources makes no lock file.
func TestSyncNoGithub(t *testing.T) {
	env := newCmdEnv(t)
	cfg := env.config(env.local("plugins/p", "p"))
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	if env.lockBytes() != nil {
		t.Fatal("a lock file was made")
	}
}

// sync repairs a missing and a changed cache entry at the locked commit, with a new dir. The
// changed entry is left as it is.
func TestSyncRepairsCache(t *testing.T) {
	for _, mode := range []string{"missing", "changed"} {
		env := newCmdEnv(t)
		r := env.repo("lazy-rex", "Messaging", "messaging")
		cfg := env.config(srcM)
		if err := env.sync(cfg); err != nil {
			t.Fatal(err)
		}
		old := env.lock().Plugins[srcM]
		oldDir := env.entryDir(srcM)
		if mode == "missing" {
			removeTree(oldDir)
		} else if err := writeInto(oldDir, "main.lua", "changed"); err != nil {
			t.Fatal(err)
		}
		snap := ""
		if mode == "changed" {
			snap = treeSnapshot(t, oldDir)
		}
		r.commit(map[string]string{"main.lua": "the branch moved"})
		if err := env.sync(cfg); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		le := env.lock().Plugins[srcM]
		if le.Commit != old.Commit || le.Dir == old.Dir || le.Digest != old.Digest {
			t.Fatalf("%s: old %+v new %+v", mode, old, le)
		}
		if mode == "changed" && treeSnapshot(t, oldDir) != snap {
			t.Fatalf("the changed entry was written")
		}
	}
}

// sync rebuilds an empty, missing or null derived field from the trusted commit. A missing commit
// is untrusted: sync refuses with the update text and keeps the lock; update repairs the entry.
func TestSyncDerivedFields(t *testing.T) {
	for _, field := range []string{"name", "owner", "repo", "ref", "ref_kind", "commit", "digest", "dir"} {
		for _, mode := range []string{"empty", "missing", "null"} {
			env := newCmdEnv(t)
			r := env.repo("lazy-rex", "Messaging", "messaging")
			cfg := env.config(srcM)
			if err := env.sync(cfg); err != nil {
				t.Fatal(err)
			}
			good := env.lock().Plugins[srcM]
			var fields map[string]any
			raw, _ := json.Marshal(good)
			json.Unmarshal(raw, &fields)
			switch mode {
			case "empty":
				fields[field] = ""
			case "missing":
				delete(fields, field)
			default:
				fields[field] = nil
			}
			env.writeLockRaw(map[string]any{"version": 1, "plugins": map[string]any{srcM: fields}})
			before := env.lockBytes()
			r.commit(map[string]string{"main.lua": "the branch moved"})
			err := env.sync(cfg)
			if field == "commit" {
				want := "plugin sync: " + srcM + ": the locked commit for " + srcM + " cannot be trusted: commit is missing; run lazyrex-hub plugin update -config " + cfg + " " + srcM + "; the lock is unchanged"
				if err == nil || err.Error() != want {
					t.Errorf("commit %s: %v", mode, err)
				}
				if !bytes.Equal(before, env.lockBytes()) {
					t.Errorf("commit %s: the lock changed", mode)
				}
				if err := env.update(cfg); err != nil || env.lock().Plugins[srcM].Commit != r.git("rev-parse", "HEAD") {
					t.Errorf("commit %s: update: %v", mode, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s %s: %v", field, mode, err)
				continue
			}
			le := env.lock().Plugins[srcM]
			if le.lockPin != good.lockPin {
				t.Errorf("%s %s: repaired %+v, want %+v", field, mode, le.lockPin, good.lockPin)
			}
		}
	}
}

// An untrusted commit (not a commit of the ref, or not hex) is refused by sync with the update
// text; an unavailable trusted commit is refused with its text. The lock stays unchanged.
func TestSyncRefusesCommit(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	good := env.lock().Plugins[srcM]
	unknown := strings.Repeat("ab", 20)
	cases := map[string]struct {
		commit, want string
	}{
		"not hex":     {"xyz", "cannot be trusted: commit is not 40 lowercase hex; run lazyrex-hub plugin update -config " + cfg + " " + srcM},
		"unavailable": {unknown, srcM + ": commit " + unknown + " is not available: "},
	}
	for name, c := range cases {
		e := *good
		e.Commit = c.commit
		env.writeLockRaw(&pluginLock{Version: 1, Plugins: map[string]*lockEntry{srcM: &e}})
		before := env.lockBytes()
		err := env.sync(cfg)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasSuffix(err.Error(), "; the lock is unchanged") {
			t.Errorf("%s: %v", name, err)
		}
		if !bytes.Equal(before, env.lockBytes()) {
			t.Errorf("%s: the lock changed", name)
		}
	}
}

// A failure at the second of two plugins writes nothing, and no referenced dir changes.
func TestSyncAllOrNothing(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	before, snap := env.lockBytes(), treeSnapshot(t, env.entryDir(srcM))
	bad := env.repo("lazy-rex", "Profiles", "profiles")
	os.Symlink("main.lua", filepath.Join(bad.dir, "link.lua"))
	bad.commit(nil)
	env.repo("lazy-rex", "Templates", "templates")
	cfg = env.config(srcM, "github:lazy-rex/Templates@main", "github:lazy-rex/Profiles@main")
	err := env.sync(cfg)
	if err == nil || err.Error() != "plugin sync: github:lazy-rex/Profiles@main: plugin trees with symlinks are not supported: link.lua; the lock is unchanged" {
		t.Fatalf("got %v", err)
	}
	if !bytes.Equal(before, env.lockBytes()) || treeSnapshot(t, filepath.Join(env.state, "plugins", filepath.FromSlash(env.lock().Plugins[srcM].Dir))) != snap {
		t.Fatal("the failed sync changed the lock or a referenced dir")
	}
}

// A failure after an earlier entry's fetch prints no result line: the lock does not have the
// earlier change. The earlier fetch did run (its cache entry is published, unreferenced).
func TestSyncFailurePrintsNoLines(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	before, entries := env.lockBytes(), len(cacheEntries(env.state))
	env.repo("lazy-rex", "Templates", "templates")
	bad := env.repo("lazy-rex", "Profiles", "profiles")
	os.Symlink("main.lua", filepath.Join(bad.dir, "link.lua"))
	bad.commit(nil)
	cfg = env.config(srcM, "github:lazy-rex/Templates@main", "github:lazy-rex/Profiles@main")
	var out bytes.Buffer
	err := runPluginCmd(context.Background(), "sync", cfg, nil, env.opts, &out)
	if err == nil || !strings.HasPrefix(err.Error(), "plugin sync: github:lazy-rex/Profiles@main: ") {
		t.Fatalf("got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("the failed sync printed %q", out.String())
	}
	if !bytes.Equal(before, env.lockBytes()) {
		t.Fatal("the failed sync changed the lock")
	}
	if got := len(cacheEntries(env.state)); got != entries+1 {
		t.Fatalf("cache entries: %d before, %d after; the Templates fetch did not run first", entries, got)
	}
}

// sync drops a lock entry whose source is not in the config.
func TestSyncDrops(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.repo("lazy-rex", "Profiles", "profiles")
	if err := env.sync(env.config(srcM, "github:lazy-rex/Profiles@main")); err != nil {
		t.Fatal(err)
	}
	if err := env.sync(env.config(srcM)); err != nil {
		t.Fatal(err)
	}
	if l := env.lock(); len(l.Plugins) != 1 || l.Plugins[srcM] == nil {
		t.Fatalf("%+v", l.Plugins)
	}
}

// A lock file that is not valid is refused and kept, by sync and by update.
func TestCommandsKeepInvalidLock(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	cfg := env.config(srcM)
	for _, body := range []string{`{"version":1,"plugins":null}`, `{"version":3,"plugins":{}}`, `{`, `{"version":1,"plugins":{"` + srcM + `":null}}`} {
		os.WriteFile(filepath.Join(env.dir, "hub.lock.json"), []byte(body), 0o600)
		for name, run := range map[string]func() error{"sync": func() error { return env.sync(cfg) }, "update": func() error { return env.update(cfg) }} {
			err := run()
			if err == nil || !strings.Contains(err.Error(), "is not valid") || !strings.Contains(err.Error(), "repair the file or restore it from a backup") {
				t.Errorf("%s %s: %v", name, body, err)
			}
			if string(env.lockBytes()) != body {
				t.Errorf("%s %s: the lock changed", name, body)
			}
		}
	}
}

// update moves a branch to its new commit. A ref that moves during the fetch is refused and the
// lock stays unchanged.
func TestUpdate(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	c2 := r.commit(map[string]string{"main.lua": "2"})
	if err := env.update(cfg); err != nil || env.lock().Plugins[srcM].Commit != c2 {
		t.Fatalf("update: %v %+v", err, env.lock().Plugins[srcM])
	}
	before := env.lockBytes()
	if err := env.update(cfg); err != nil || !bytes.Equal(before, env.lockBytes()) {
		t.Fatalf("an update without a move changed the lock: %v", err)
	}
	r.commit(map[string]string{"main.lua": "3"})
	env.opts.afterResolve = func(string) { r.commit(map[string]string{"main.lua": "4"}) }
	err := env.update(cfg)
	if err == nil || err.Error() != "plugin update: "+srcM+": ref refs/heads/main moved during the fetch; run the command again; the lock is unchanged" {
		t.Fatalf("moved: %v", err)
	}
	if !bytes.Equal(before, env.lockBytes()) {
		t.Fatal("the lock changed")
	}
}

// The update selector (the approved rule): by exact source or plugin name; managed: matches only
// managed ids (none yet); exactly one GitHub entry. An unselected entry keeps its pin.
func TestUpdateSelect(t *testing.T) {
	env := newCmdEnv(t)
	a := env.repo("lazy-rex", "Messaging", "messaging")
	b := env.repo("lazy-rex", "Profiles", "profiles")
	srcP := "github:lazy-rex/Profiles@main"
	env.local("plugins/dup", "messaging2")
	cfg := env.config(srcM, srcP, "plugins/dup")
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	pinP, _ := json.Marshal(env.lock().Plugins[srcP])
	newA := a.commit(map[string]string{"main.lua": "2"})
	b.commit(map[string]string{"main.lua": "2"})
	for _, args := range [][]string{{srcM}, {"messaging"}, {"messaging", srcM, "messaging"}} {
		if err := env.update(cfg, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if env.lock().Plugins[srcM].Commit != newA {
			t.Fatalf("%v: A did not move", args)
		}
		if got, _ := json.Marshal(env.lock().Plugins[srcP]); !bytes.Equal(got, pinP) {
			t.Fatalf("%v: the unselected entry changed: %s", args, got)
		}
	}
	before := env.lockBytes()
	bad := map[string]string{
		"nothing":     "plugin update: no entry has the name, source or identity nothing",
		"managed:x":   "plugin update: no entry has the name, source or identity managed:x",
		"plugins/dup": "plugin update: plugins/dup is a local source; update changes GitHub sources only",
		"messaging2":  "plugin update: messaging2 is a local source; update changes GitHub sources only",
	}
	for arg, want := range bad {
		if err := env.update(cfg, arg); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", arg, err, want)
		}
	}
	if !bytes.Equal(before, env.lockBytes()) {
		t.Fatal("a refused update changed the lock")
	}
}

// Two entries with one name: the name selects both, so it is refused. A GitHub entry that is not
// installed has no known name; its source selects it. A local source literally named
// "managed:..." is selected by its plugin name, never by "managed:".
func TestUpdateSelectIdentities(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.local("plugins/m", "messaging")
	env.local("managed:x", "lit")
	cfg := env.config(srcM, "plugins/m", "managed:x")
	// The lock is empty: entry 1 is not installed, so only the local entry 2 has the name.
	if err := env.update(cfg, "messaging"); err == nil || err.Error() != "plugin update: messaging is a local source; update changes GitHub sources only" {
		t.Fatalf("not installed, by name: %v", err)
	}
	// By source, the GitHub entry is selected; the graph then refuses the duplicate name.
	err := env.update(cfg, srcM)
	if err == nil || !strings.Contains(err.Error(), "entries 1 and 2 have the same name messaging") {
		t.Fatalf("by source: %v", err)
	}
	// With the lock entry (from a sync of a config without the duplicate), the name selects two.
	if err := env.sync(env.config(srcM)); err != nil {
		t.Fatal(err)
	}
	cfg = env.config(srcM, "plugins/m", "managed:x") // config rewrites hub.json
	if err := env.update(cfg, "messaging"); err == nil || err.Error() != "plugin update: messaging selects entry 1, entry 2; write managed:<id> or the exact source" {
		t.Fatalf("two by name: %v", err)
	}
	if err := env.update(cfg, "managed:x"); err == nil || err.Error() != "plugin update: no entry has the name, source or identity managed:x" {
		t.Fatalf("managed:x: %v", err)
	}
	if err := env.update(cfg, "lit"); err == nil || err.Error() != "plugin update: lit is a local source; update changes GitHub sources only" {
		t.Fatalf("lit: %v", err)
	}
}

// The graph check covers the complete new graph: a lock that refuses any entry at the next start
// is not written.
func TestCommandGraph(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "b")
	cfg := env.config(env.local("plugins/a", "a", "b"), srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	before := env.lockBytes()
	// GitHub B renames itself from b to c: local A loses its dependency.
	r.commit(map[string]string{"lazyrex.json": manifestJSON("c")})
	err := env.update(cfg)
	if err == nil || err.Error() != "plugin update: plugin a: dependency b is not configured; the lock is unchanged" {
		t.Fatalf("rename: %v", err)
	}
	if !bytes.Equal(before, env.lockBytes()) {
		t.Fatal("the lock changed")
	}
	// A cycle between a local and a GitHub plugin.
	r.commit(map[string]string{"lazyrex.json": manifestJSON("b", "a")})
	if err := env.update(cfg); err == nil || !strings.Contains(err.Error(), "dependency cycle a -> b -> a") {
		t.Fatalf("cycle: %v", err)
	}
	// A GitHub plugin with a missing dependency, at first install.
	env2 := newCmdEnv(t)
	env2.repo("lazy-rex", "Worktrees", "worktrees", "profiles")
	err = env2.sync(env2.config("github:lazy-rex/Worktrees@main"))
	if err == nil || err.Error() != "plugin sync: plugin worktrees: dependency profiles is not configured; the lock is unchanged" {
		t.Fatalf("missing: %v", err)
	}
	if env2.lockBytes() != nil {
		t.Fatal("a lock was written")
	}
}

// An invalid ref is refused before any fetch.
func TestCommandInvalidRef(t *testing.T) {
	env := newCmdEnv(t)
	err := env.sync(env.config("github:lazy-rex/Messaging@a..b"))
	if err == nil || err.Error() != "plugin sync: github:lazy-rex/Messaging@a..b: a..b is not a valid ref; the lock is unchanged" {
		t.Fatalf("got %v", err)
	}
}

// status reports each entry, orphaned lock entries and unreferenced cache entries, and fails
// when an entry is not ok.
func TestStatus(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.repo("lazy-rex", "Profiles", "profiles")
	env.repo("lazy-rex", "Templates", "templates")
	srcP, srcT := "github:lazy-rex/Profiles@main", "github:lazy-rex/Templates@main"
	if err := env.sync(env.config(srcM, srcP, srcT)); err != nil {
		t.Fatal(err)
	}
	status := func(cfg string) (int, string) {
		var out, errb bytes.Buffer
		code := pluginCmdMain([]string{"status", "-config", cfg}, &out, &errb)
		return code, out.String() + errb.String()
	}
	cfg := env.config(srcM, srcP, srcT)
	if code, out := status(cfg); code != 0 || strings.Count(out, "cache ok") != 3 {
		t.Fatalf("all ok: %d\n%s", code, out)
	}
	lock := env.lock()
	removeTree(env.entryDir(srcP))
	writeInto(env.entryDir(srcT), "main.lua", "changed")
	// An unreferenced entry: a second publish of Messaging that no lock names.
	publishTree(t, env.state, testCommit, testTree)
	delete(lock.Plugins, srcM)
	code, out := status(env.config(srcP, srcT, "github:lazy-rex/Tunnels@main"))
	for _, want := range []string{
		srcP + "\tref main\tcommit " + lock.Plugins[srcP].Commit + "\tcache missing",
		srcT + "\tref main\tcommit " + lock.Plugins[srcT].Commit + "\tcache changed",
		"github:lazy-rex/Tunnels@main\tref main\tcommit -\tcache not installed",
		srcM + "\torphaned (not in the config; plugin sync drops it)",
		"github/lazy-rex/Messaging/" + testCommit + ".",
		"unreferenced (no recorded digest; a running instance or another config can use it)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if code != 1 {
		t.Errorf("exit %d", code)
	}
}

// shortWriter writes one byte less than asked and returns no error.
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return max(len(p)-1, 0), nil }

// An output write failure after the lock write is an error that says that the lock is written:
// a failed write and a short write, for sync (an install), update and a dropped-only sync. The
// lock is written in each case.
func TestCmdOutputFailure(t *testing.T) {
	writers := map[string]io.Writer{"failed": failWriter{}, "short": shortWriter{}}
	for wname, w := range writers {
		for _, c := range []string{"install", "update", "dropped"} {
			env := newCmdEnv(t)
			r := env.repo("lazy-rex", "Messaging", "messaging")
			env.repo("lazy-rex", "Profiles", "profiles")
			cfg, cmd, args := env.config(srcM), "sync", []string(nil)
			switch c {
			case "update":
				if err := env.sync(cfg); err != nil {
					t.Fatal(err)
				}
				r.commit(map[string]string{"main.lua": "moved"})
				cmd, args = "update", []string{srcM}
			case "dropped":
				if err := env.sync(env.config(srcM, "github:lazy-rex/Profiles@main")); err != nil {
					t.Fatal(err)
				}
				cfg = env.config(srcM) // the same path: Profiles leaves the config
			}
			before := env.lockBytes()
			err := runPluginCmd(context.Background(), cmd, cfg, args, env.opts, w)
			want := "plugin " + cmd + ": the lock is written, but the result lines are not: "
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("%s %s: got %v", wname, c, err)
			}
			if wname == "short" && !errors.Is(err, io.ErrShortWrite) {
				t.Errorf("%s %s: %v is not io.ErrShortWrite", wname, c, err)
			}
			if bytes.Equal(before, env.lockBytes()) {
				t.Errorf("%s %s: the lock is not written", wname, c)
			}
		}
	}
}

// A status output failure is an error, and it wins over the not-ok result.
func TestStatusOutputFailure(t *testing.T) {
	env := newCmdEnv(t)
	cfg := env.config("github:lazy-rex/Tunnels@main") // not installed: not ok
	for wname, w := range map[string]io.Writer{"failed": failWriter{}, "short": shortWriter{}} {
		ok, err := runPluginStatus(cfg, w)
		if ok || err == nil || !strings.HasPrefix(err.Error(), "plugin status: the result lines are not written: ") {
			t.Errorf("%s: ok %v, err %v", wname, ok, err)
		}
		var errb bytes.Buffer
		if code := pluginCmdMain([]string{"status", "-config", cfg}, w, &errb); code != 1 || !strings.Contains(errb.String(), "the result lines are not written") {
			t.Errorf("%s: exit %d, stderr %q", wname, code, errb.String())
		}
	}
}

func TestPluginCmdUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"prune"}, {"sync"}, {"sync", "-config", "x", "extra"}, {"status", "-x"}} {
		var out, errb bytes.Buffer
		if code := pluginCmdMain(args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage") {
			t.Errorf("%v: exit %d %q", args, code, errb.String())
		}
	}
	var out, errb bytes.Buffer
	if code := pluginCmdMain([]string{"sync", "-config", filepath.Join(t.TempDir(), "none.json")}, &out, &errb); code != 1 {
		t.Errorf("missing config: exit %d", code)
	}
}

// The flock helper process: it takes both flocks of $LAZYREX_TEST_FLOCK ("state|lockfile"),
// writes "locked" and waits until its stdin closes.
func TestFlockHelper(t *testing.T) {
	spec := os.Getenv("LAZYREX_TEST_FLOCK")
	if spec == "" {
		t.Skip("helper process only")
	}
	state, lock, _ := strings.Cut(spec, "|")
	release, err := lockCommands(state, lock)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	defer release()
	fmt.Println("locked")
	b := make([]byte, 1)
	os.Stdin.Read(b)
}

// holdFlocks starts a helper process that holds the flocks of state and lock, and gives its pid.
func holdFlocks(t *testing.T, state, lock string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFlockHelper$")
	cmd.Env = append(os.Environ(), "LAZYREX_TEST_FLOCK="+state+"|"+lock)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	buf := make([]byte, 64)
	n, _ := stdout.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "locked") {
		t.Fatalf("helper: %q", buf[:n])
	}
	return cmd.Process.Pid
}

// Two commands on one state (any lock file) or on one lock file (any state): the second fails at
// once with the holder's pid. A failed second flock releases the first.
func TestFlocks(t *testing.T) {
	dir := t.TempDir()
	s1, s2 := filepath.Join(dir, "s1"), filepath.Join(dir, "s2")
	l1, l2, l3 := filepath.Join(dir, "a.lock.json"), filepath.Join(dir, "b.lock.json"), filepath.Join(dir, "c.lock.json")
	pid := holdFlocks(t, s1, l1)
	want := fmt.Sprintf("another plugin command runs (pid %d)", pid)
	if _, err := lockCommands(s1, l2); err == nil || err.Error() != want {
		t.Errorf("same state, other lock: %v", err)
	}
	if _, err := lockCommands(s2, l1); err == nil || err.Error() != want {
		t.Errorf("other state, same lock: %v", err)
	}
	// The failure on l1 released the flock of s2.
	release, err := lockCommands(s2, l3)
	if err != nil {
		t.Fatalf("s2 after a failed second flock: %v", err)
	}
	release()
	// An empty pid file (a holder that has not written it yet) gives pid unknown.
	f, _ := os.OpenFile(filepath.Join(s1, "plugins", ".lock"), os.O_RDWR, 0)
	f.Truncate(0)
	f.Close()
	if _, err := lockCommands(s1, l2); err == nil || err.Error() != "another plugin command runs (pid unknown)" {
		t.Errorf("empty pid: %v", err)
	}
	// The commands report it with their prefix.
	cfg := filepath.Join(dir, "hub.json")
	os.WriteFile(cfg, []byte(`{"state":"`+s2+`","lock":"a.lock.json","plugins":[]}`), 0o600)
	if err := pluginSync(context.Background(), cfg, pluginCmdOpts{}); err == nil || err.Error() != "plugin sync: "+want {
		t.Errorf("sync: %v", err)
	}
}

// A ref that the remote does not have gives the agreed text, with the source prefix.
func TestCommandRefNotFound(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	err := env.sync(env.config("github:lazy-rex/Messaging@nope"))
	if err == nil || err.Error() != "plugin sync: github:lazy-rex/Messaging@nope: ref nope is not found; the lock is unchanged" {
		t.Fatalf("got %v", err)
	}
}

// A relative state is relative to the working directory of the command, as for the hub. Every
// command works with it: install, repair, update and status.
func TestRelativeState(t *testing.T) {
	env := newCmdEnv(t)
	env.cfgState = "state"
	env.state = filepath.Join(env.dir, "state")
	t.Cleanup(func() { removeTree(env.state) })
	t.Chdir(env.dir)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	removeTree(env.entryDir(srcM))
	if err := env.sync(cfg); err != nil {
		t.Fatalf("repair: %v", err)
	}
	c2 := r.commit(map[string]string{"main.lua": "2"})
	if err := env.update(cfg); err != nil || env.lock().Plugins[srcM].Commit != c2 {
		t.Fatalf("update: %v", err)
	}
	var out, errb bytes.Buffer
	if code := pluginCmdMain([]string{"status", "-config", "hub.json"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "cache ok") {
		t.Fatalf("status: %d %s %s", code, out.String(), errb.String())
	}
	if _, err := os.Stat(filepath.Join(env.dir, "state", "plugins", "github")); err != nil {
		t.Fatalf("the cache is not under the relative state: %v", err)
	}
}

// A repair of ref_kind takes the kind from a full ref form (refs/heads/..., refs/tags/...), with no
// ls-remote: the ref may be gone while the pinned commit is still available by ID. Only a bare name
// needs ls-remote.
func TestSyncRepairKindFromRefForm(t *testing.T) {
	for _, c := range []struct{ ref, kind, del string }{
		{"refs/heads/main", "branch", "branch -D main"},
		{"refs/tags/v1", "tag", "tag -d v1"},
	} {
		env := newCmdEnv(t)
		r := env.repo("lazy-rex", "Messaging", "messaging")
		r.git("tag", "v1")
		src := "github:lazy-rex/Messaging@" + c.ref
		cfg := env.config(src)
		if err := env.sync(cfg); err != nil {
			t.Fatal(err)
		}
		good := env.lock().Plugins[src]
		e := *good
		e.RefKind = ""
		env.writeLockRaw(&pluginLock{Version: 1, Plugins: map[string]*lockEntry{src: &e}})
		r.git("checkout", "-q", "-b", "saved")
		r.git(strings.Fields(c.del)...)
		if err := env.sync(cfg); err != nil {
			t.Fatalf("%s: %v", c.ref, err)
		}
		if got := env.lock().Plugins[src]; got.RefKind != c.kind || got.Commit != good.Commit {
			t.Fatalf("%s: %+v", c.ref, got)
		}
	}
}
