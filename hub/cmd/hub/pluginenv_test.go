package main

// The plugin manager in a command environment: a config directory, a state directory and the
// GitHub repositories of one test. The install op and the update op install and update the
// managed GitHub entries of a cmdEnv, as a running hub does. GitHub sources are local
// repositories through file:// URLs (the internal remote option); the tests do not prove GitHub
// behavior.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	h        *Hub   // the ops test hub of opsHub
	n        int    // the count of op keys
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
	b, _ := json.Marshal(map[string]any{"server": "unix:///tmp/lrt-x/rex.sock", "state": st, "plugins": plugins})
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

const srcLatest = "github:lazy-rex/Messaging@latest"

// the environment's, and its git remotes are env.repos. A second call gives the same hub.
func (env *cmdEnv) opsHub(cfg string) *Hub {
	env.t.Helper()
	if env.h == nil {
		noInstallStart(env.t)
		env.h, _, _ = opsTestHub(env.t, "")
	}
	c := env.load(cfg)
	h := env.h
	h.cfg.State, h.cfg.path, h.cfg.managedPath, h.cfg.Lock = c.State, c.path, c.managedPath, c.Lock
	h.pops.cmd = env.opts
	opsLoadEntries(env.t, h)
	// Each managed entry has a stopped record, as the hub's load gives it: an update ends at
	// "its next start runs" the pin.
	h.mu.Lock()
	for _, e := range h.entries {
		if h.plugins[e.name] == nil {
			h.plugins[e.name] = &Plugin{Name: e.name, entry: e, Desired: "stopped"}
		}
	}
	h.mu.Unlock()
	return h
}

// install runs the hub's install scan on cfg and each install op that it admits to its end
// (runInstalls). The error joins the error of each op that did not end done.
func (env *cmdEnv) install(cfg string) error {
	env.t.Helper()
	ops := runInstalls(env.t, env.opsHub(cfg), "start")
	return opsFailed(ops)
}

// updateOp runs an update op of each managed entry id to its own ref ("@"), as the Plugins view
// does, to its end. The error joins the error of each op that did not end done.
func (env *cmdEnv) updateOp(cfg string, ids ...string) error {
	env.t.Helper()
	h := env.opsHub(cfg)
	ops := map[string]*pluginOp{}
	for _, id := range ids {
		env.n++
		v, err := userAdmit(h, fmt.Sprintf("update-%s-%d", id, env.n), opRequest{Op: "update", Entry: "managed:" + id, Input: rawJSON(map[string]any{"to": "@"})})
		if err != nil {
			return err
		}
		op := opsOf(env.t, h, v["op_id"].(string))
		if err := h.runOp(context.Background(), op); err != nil {
			return err
		}
		ops["managed:"+id] = op
	}
	return opsFailed(ops)
}

// opsFailed joins the error of each op that did not end done, by entry key.
func opsFailed(ops map[string]*pluginOp) error {
	var keys []string
	for k := range ops {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var errs []error
	for _, k := range keys {
		if op := ops[k]; op.Phase != "done" {
			errs = append(errs, fmt.Errorf("%s %s: %s: %s", op.Kind, k, op.Phase, op.Error))
		}
	}
	return errors.Join(errs...)
}

// The install op and the update op on a command environment: the install writes the lock entry
// and a cache entry that lockedDir accepts; a second install scan admits nothing; the update of a
// moved branch writes the new commit.
func TestCmdEnvInstallAndUpdate(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	env.managed("messaging=" + srcM)
	cfg := env.config()
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	le := env.lock().Plugins[srcM]
	if le == nil || le.Commit != r.git("rev-parse", "HEAD") {
		t.Fatalf("the lock entry: %+v", le)
	}
	if _, _, err := lockedDir(env.state, env.lock(), githubEntry(t, srcM)); err != nil {
		t.Fatal(err)
	}
	if ops := runInstalls(t, env.opsHub(cfg), "reload"); len(ops) != 0 {
		t.Fatalf("a second scan: %+v", ops)
	}
	next := r.commit(map[string]string{"main.lua": "return 2\n"})
	if err := env.updateOp(cfg, "messaging"); err != nil {
		t.Fatal(err)
	}
	if got := env.lock().Plugins[srcM].Commit; got != next {
		t.Fatalf("after the update: %s, want %s", got, next)
	}
}

// A lock entry that does not agree with its source refuses the entry, and the install scan admits
// no op for it. The manager's update writes a new lock entry: the entry resolves again. This
// holds for a field that does not match and for a commit that cannot be trusted.
func TestUpdateClearsLockMismatch(t *testing.T) {
	for _, c := range []struct {
		name, part string // part: a part of the refusal text
		break_     func(le *lockEntry)
		want       func(cfg string, le *lockEntry) string
	}{
		{"ref", "does not match: ref;", func(le *lockEntry) { le.Ref = "other" }, func(cfg string, le *lockEntry) string {
			return lockMismatch(srcM, le, githubEntry(t, srcM).githubRef(), "ref").Error()
		}},
		{"untrusted", "cannot be trusted: commit is not 40 lowercase hex;", func(le *lockEntry) { le.Commit = strings.ToUpper(le.Commit) }, func(cfg string, le *lockEntry) string {
			return lockMismatch(srcM, le, githubEntry(t, srcM).githubRef(), "commit").Error()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newCmdEnv(t)
			r := env.repo("lazy-rex", "Messaging", "messaging")
			env.managed("messaging=" + srcM)
			cfg := env.config()
			if err := env.install(cfg); err != nil {
				t.Fatal(err)
			}
			l := env.lock()
			c.break_(l.Plugins[srcM])
			env.writeLockRaw(l)
			conf := env.load(cfg)
			resolve(&conf)
			if want := c.want(cfg, l.Plugins[srcM]); conf.entries[0].err == nil || conf.entries[0].err.Error() != want || !strings.Contains(want, c.part) {
				t.Fatalf("the refusal: %v\nwant %s", conf.entries[0].err, want)
			}
			if ops := runInstalls(t, env.opsHub(cfg), "reload"); len(ops) != 0 {
				t.Fatalf("an install of a lock entry that does not agree: %+v", ops)
			}
			if err := env.updateOp(cfg, "messaging"); err != nil {
				t.Fatal(err)
			}
			conf = env.load(cfg)
			resolve(&conf)
			if e := conf.entries[0]; e.err != nil || e.cand == nil || e.cand.Commit != r.git("rev-parse", "HEAD") {
				t.Fatalf("after the update: %+v %v", e.cand, e.err)
			}
		})
	}
}

// The install of a latest source selects the highest release, not the branch tip, and records its
// tag (lock version 2). A later scan keeps that selection, also after a newer release. A cache
// repair fetches the locked commit and keeps the locked tag, also when the tag is gone from the
// repository.
func TestLatestInstall(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	r.git("tag", "v0.9.0")
	c1 := r.commit(map[string]string{"main.lua": "1"})
	r.git("tag", "-a", "-m", "release", "v1.0.0")
	r.commit(map[string]string{"main.lua": "tip"})
	r.git("tag", "v1.1.0-rc.1")
	env.managed("messaging=" + srcLatest)
	cfg := env.config()
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	le := env.lock().Plugins[srcLatest]
	if le.Ref != "latest" || le.RefKind != "latest" || le.Tag != "v1.0.0" || le.Commit != c1 || le.Name != "messaging" {
		t.Fatalf("lock entry %+v", le)
	}
	if !bytes.HasPrefix(env.lockBytes(), []byte("{\n  \"version\": 2,")) {
		t.Fatalf("lock:\n%s", env.lockBytes())
	}
	c := env.load(cfg)
	if resolve(&c); c.entries[0].err != nil || c.entries[0].cand.Commit != c1 {
		t.Fatalf("the hub: %v", c.entries[0].err)
	}

	before := env.lockBytes()
	r.git("tag", "v2.0.0")
	if ops := runInstalls(t, env.opsHub(cfg), "reload"); len(ops) != 0 || !bytes.Equal(before, env.lockBytes()) {
		t.Fatalf("a scan after a newer release: %+v", ops)
	}

	// The repair needs no tag: every release tag is gone, and the release commit is no branch tip.
	removeTree(env.entryDir(srcLatest))
	for _, tag := range []string{"v0.9.0", "v1.0.0", "v2.0.0", "v1.1.0-rc.1"} {
		r.git("tag", "-d", tag)
	}
	if err := env.install(cfg); err != nil {
		t.Fatalf("repair: %v", err)
	}
	got := env.lock().Plugins[srcLatest]
	if got.Tag != "v1.0.0" || got.Commit != c1 || got.RefKind != "latest" || got.Digest != le.Digest || got.Dir == le.Dir {
		t.Fatalf("repaired %+v, was %+v", got, le)
	}
}

// A latest lock entry without a valid tag is not trusted: the hub refuses the entry with the
// untrusted text, the install scan takes no tag from the repository, and the manager's update
// writes the release tag.
func TestLatestInstallRefusesMissingTag(t *testing.T) {
	for _, tag := range []string{"", "v1.0"} {
		env := newCmdEnv(t)
		r := env.repo("lazy-rex", "Messaging", "messaging")
		r.git("tag", "v1.0.0")
		env.managed("messaging=" + srcLatest)
		cfg := env.config()
		if err := env.install(cfg); err != nil {
			t.Fatal(err)
		}
		l := env.lock()
		l.Plugins[srcLatest].Tag = tag
		env.writeLockRaw(l)
		before := env.lockBytes()
		why := "tag is missing"
		if tag != "" {
			why = "tag is not a version tag vX.Y.Z"
		}
		c := env.load(cfg)
		resolve(&c)
		if want := untrustedCommit(srcLatest, why).Error(); c.entries[0].err == nil || c.entries[0].err.Error() != want {
			t.Fatalf("%q: got  %v\nwant %s", tag, c.entries[0].err, want)
		}
		if ops := runInstalls(t, env.opsHub(cfg), "reload"); len(ops) != 0 || !bytes.Equal(before, env.lockBytes()) {
			t.Fatalf("%q: an install: %+v", tag, ops)
		}
		if err := env.updateOp(cfg, "messaging"); err != nil || env.lock().Plugins[srcLatest].Tag != "v1.0.0" {
			t.Fatalf("%q: update: %v %+v", tag, err, env.lock().Plugins[srcLatest])
		}
	}
}

// A repository without a release tag fails the install of @latest; it never falls back to a
// branch, and no lock file is made.
func TestLatestInstallNoRelease(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	r.git("tag", "v1.0.0-rc.1")
	r.git("branch", "latest")
	env.managed("messaging=" + srcLatest)
	err := env.install(env.config())
	want := "install managed:messaging: failed: resolve managed:messaging: the repository has no version tag vX.Y.Z that points to a commit; nothing was written"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v", err)
	}
	if env.lockBytes() != nil {
		t.Fatal("a lock file was made")
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
}

// A relative state is relative to the working directory at the load (lead-relstate): the load
// makes it absolute, and the install op fetches into it. Before, the fetch child failed in a
// relative directory.
func TestRelativeStateInstall(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	env.cfgState = "state"
	env.state = filepath.Join(env.dir, "state")
	t.Cleanup(func() { removeTree(env.state) })
	t.Chdir(env.dir)
	env.managed("messaging=" + srcM)
	cfg := env.config()
	c := env.load(cfg)
	wd, _ := os.Getwd()
	if c.State != filepath.Join(wd, "state") || c.managedPath != filepath.Join(wd, "state", "plugins.json") {
		t.Fatalf("state %s, managed file %s; want them under %s", c.State, c.managedPath, wd)
	}
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	if le := env.lock().Plugins[srcM]; le == nil || le.Commit != r.git("rev-parse", "HEAD") {
		t.Fatalf("the lock entry: %+v", le)
	}
	if _, _, err := lockedDir(env.state, env.lock(), githubEntry(t, srcM)); err != nil {
		t.Fatal(err)
	}
}
