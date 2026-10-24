package main

// The plugin manager in a command environment: the install op and the update op install and
// update the managed GitHub entries of a cmdEnv, as a running hub does.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// opsHub gives the ops test hub of the config cfg: its state, its lock and its managed file are
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
	if _, _, err := lockedDir(cfg, env.state, env.lock(), githubEntry(t, srcM)); err != nil {
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
			return lockMismatch(cfg, srcM, le, githubEntry(t, srcM).githubRef(), "ref").Error()
		}},
		{"untrusted", "cannot be trusted: commit is not 40 lowercase hex;", func(le *lockEntry) { le.Commit = strings.ToUpper(le.Commit) }, func(cfg string, le *lockEntry) string {
			return lockMismatch(cfg, srcM, le, githubEntry(t, srcM).githubRef(), "commit").Error()
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
		if want := untrustedCommit(cfg, srcLatest, why).Error(); c.entries[0].err == nil || c.entries[0].err.Error() != want {
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
