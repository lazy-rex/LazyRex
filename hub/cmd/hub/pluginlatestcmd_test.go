package main

// Tests of @latest in the plugin commands and in the hub resolution. The repositories are local,
// through file:// URLs.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const srcLatest = "github:lazy-rex/Messaging@latest"

// run runs sync or update and gives its output lines.
func (env *cmdEnv) run(cmd, cfg string, args ...string) (string, error) {
	var out bytes.Buffer
	err := runPluginCmd(context.Background(), cmd, cfg, args, env.opts, &out)
	return out.String(), err
}

// The first sync of a latest source selects the highest release, not the branch tip. A later sync
// keeps that selection, also after a newer release. A cache repair fetches the locked commit and
// keeps the locked tag, also when the tag is gone from the repository.
func TestLatestSync(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	r.git("tag", "v0.9.0")
	c1 := r.commit(map[string]string{"main.lua": "1"})
	r.git("tag", "-a", "-m", "release", "v1.0.0")
	r.commit(map[string]string{"main.lua": "tip"})
	r.git("tag", "v1.1.0-rc.1")
	cfg := env.config(srcLatest)

	out, err := env.run("sync", cfg)
	if err != nil || out != "installed "+srcLatest+" "+c1+"\n" {
		t.Fatalf("first sync: %q %v", out, err)
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
	if out, err := env.run("sync", cfg); err != nil || out != "" || !bytes.Equal(before, env.lockBytes()) {
		t.Fatalf("a sync after a newer release: %q %v", out, err)
	}

	// The repair needs no tag: every release tag is gone, and the release commit is no branch tip.
	removeTree(env.entryDir(srcLatest))
	for _, tag := range []string{"v0.9.0", "v1.0.0", "v2.0.0", "v1.1.0-rc.1"} {
		r.git("tag", "-d", tag)
	}
	out, err = env.run("sync", cfg)
	if err != nil || out != "repaired "+srcLatest+" "+c1+"\n" {
		t.Fatalf("repair: %q %v", out, err)
	}
	got := env.lock().Plugins[srcLatest]
	if got.Tag != "v1.0.0" || got.Commit != c1 || got.RefKind != "latest" || got.Digest != le.Digest || got.Dir == le.Dir {
		t.Fatalf("repaired %+v, was %+v", got, le)
	}
}

// A latest entry without a valid tag is not trusted: sync refuses it with the update text and
// takes no tag from the repository.
func TestLatestSyncRefusesMissingTag(t *testing.T) {
	for _, tag := range []string{"", "v1.0"} {
		env := newCmdEnv(t)
		r := env.repo("lazy-rex", "Messaging", "messaging")
		r.git("tag", "v1.0.0")
		cfg := env.config(srcLatest)
		if err := env.sync(cfg); err != nil {
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
		_, err := env.run("sync", cfg)
		want := "plugin sync: " + srcLatest + ": the locked commit for " + srcLatest + " cannot be trusted: " + why +
			"; run lazyrex-hub plugin update -config " + cfg + " " + srcLatest + "; the lock is unchanged"
		if err == nil || err.Error() != want {
			t.Fatalf("%q: got  %v\nwant %s", tag, err, want)
		}
		if !bytes.Equal(before, env.lockBytes()) {
			t.Fatalf("%q: the lock changed", tag)
		}
		if err := env.update(cfg); err != nil || env.lock().Plugins[srcLatest].Tag != "v1.0.0" {
			t.Fatalf("%q: update: %v %+v", tag, err, env.lock().Plugins[srcLatest])
		}
	}
}

// A repository without a release tag refuses @latest; it never falls back to a branch.
func TestLatestSyncNoRelease(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	r.git("tag", "v1.0.0-rc.1")
	r.git("branch", "latest")
	cfg := env.config(srcLatest)
	for _, cmd := range []string{"sync", "update"} {
		_, err := env.run(cmd, cfg)
		want := "plugin " + cmd + ": " + srcLatest + ": the repository has no version tag vX.Y.Z that points to a commit; the lock is unchanged"
		if err == nil || err.Error() != want {
			t.Fatalf("%s: %v", cmd, err)
		}
		if env.lockBytes() != nil {
			t.Fatalf("%s: a lock file was made", cmd)
		}
	}
}

// The update rules (section 6): unchanged, updated, a moved tag and a lower tag refused, a tag that
// moves during the fetch refused, skipped tags named.
func TestLatestUpdate(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	c1 := r.git("rev-parse", "HEAD")
	r.git("tag", "v1.0.0")
	cfg := env.config(srcLatest)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	before := env.lockBytes()
	if out, err := env.run("update", cfg); err != nil || out != "" || !bytes.Equal(before, env.lockBytes()) {
		t.Fatalf("unchanged: %q %v", out, err)
	}

	c2 := r.commit(map[string]string{"main.lua": "2"})
	r.git("tag", "-a", "-m", "release", "v1.1.0")
	r.git("tag", "v2.0.0", r.git("rev-parse", "HEAD^{tree}"))
	out, err := env.run("update", cfg)
	want := "ignored " + srcLatest + ": refs/tags/v2.0.0 points to a tree, not a commit\nupdated " + srcLatest + " " + c2 + "\n"
	if err != nil || out != want {
		t.Fatalf("updated: %q %v", out, err)
	}
	if le := env.lock().Plugins[srcLatest]; le.Tag != "v1.1.0" || le.Commit != c2 {
		t.Fatalf("updated %+v", le)
	}
	r.git("tag", "-d", "v2.0.0")

	check := func(name, wantErr string) {
		t.Helper()
		before := env.lockBytes()
		out, err := env.run("update", cfg)
		if err == nil || err.Error() != "plugin update: "+srcLatest+": "+wantErr+"; the lock is unchanged" || out != "" {
			t.Fatalf("%s: %q %v", name, out, err)
		}
		if !bytes.Equal(before, env.lockBytes()) {
			t.Fatalf("%s: the lock changed", name)
		}
	}
	c3 := r.commit(map[string]string{"main.lua": "3"})
	r.git("tag", "-f", "v1.1.0")
	check("moved", "the tag v1.1.0 moved from "+c2+" to "+c3+"; a release tag must not move: check the repository")
	r.git("tag", "-d", "v1.1.0")
	check("lower", "the newest version tag is v1.0.0, lower than the locked v1.1.0")

	r.git("tag", "v1.2.0", c3)
	env.opts.afterResolve = func(string) { r.git("tag", "-f", "v1.2.0", c1) }
	check("moved during the fetch", "ref refs/tags/v1.2.0 moved during the fetch; run the command again")
	env.opts.afterResolve = nil
	// v1.2.0 now names c1: a higher tag at an older commit is an update.
	if out, err := env.run("update", cfg); err != nil || out != "updated "+srcLatest+" "+c1+"\n" {
		t.Fatalf("after the move: %q %v", out, err)
	}
}

// legacyLock rewrites the lock entry of srcM (synced) as a legacy @latest entry of kind kind.
func (env *cmdEnv) legacyLock(kind string) *lockEntry {
	env.t.Helper()
	l := env.lock()
	le := l.Plugins[srcM]
	le.Ref, le.RefKind = "latest", kind
	delete(l.Plugins, srcM)
	l.Plugins[srcLatest] = le
	env.writeLockRaw(l)
	return le
}

// A legacy entry (ref latest of kind branch or tag, from before @latest) is refused by sync, pull
// and the hub, with the conversion text. Only plugin update converts it, with its own line.
func TestLatestLegacy(t *testing.T) {
	for _, kind := range []string{"branch", "tag"} {
		env := newCmdEnv(t)
		r := env.repo("lazy-rex", "Messaging", "messaging")
		cfg := env.config(srcM)
		if err := env.sync(cfg); err != nil {
			t.Fatal(err)
		}
		le := env.legacyLock(kind)
		cfg = env.config(srcLatest)
		full := map[string]string{"branch": "refs/heads/latest", "tag": "refs/tags/latest"}[kind]
		text := "@latest now means the highest version tag vX.Y.Z. To convert this entry (it replaces the pinned " + kind +
			" commit " + le.Commit + "), run: lazyrex-hub plugin update -config " + cfg + " " + srcLatest +
			". To keep following the " + kind + " named latest, change the source to @" + full
		before := env.lockBytes()

		if _, err := env.run("sync", cfg); err == nil || err.Error() != "plugin sync: "+srcLatest+": "+text+"; the lock is unchanged" {
			t.Fatalf("%s sync: %v", kind, err)
		}
		doc := filepath.Join(t.TempDir(), "pins.json")
		b, _ := encodePinDoc(&pinDoc{Hub: pinHub, Entries: []pinEntry{}})
		os.WriteFile(doc, b, 0o600)
		if _, err := pluginPull(context.Background(), cfg, doc, nil, nil, env.opts, &bytes.Buffer{}); err == nil ||
			err.Error() != "plugin pull: "+srcLatest+": "+text+"; the lock is unchanged" {
			t.Fatalf("%s pull: %v", kind, err)
		}
		c := env.load(cfg)
		if resolve(&c); c.entries[0].err == nil || c.entries[0].err.Error() != "plugin "+srcLatest+": "+text {
			t.Fatalf("%s hub: %v", kind, c.entries[0].err)
		}
		if !bytes.Equal(before, env.lockBytes()) {
			t.Fatalf("%s: the lock changed", kind)
		}

		c1 := r.commit(map[string]string{"main.lua": "1"})
		r.git("tag", "v1.0.0")
		out, err := env.run("update", cfg)
		if want := "converted " + srcLatest + ": " + kind + " latest " + le.Commit + " -> v1.0.0 " + c1 + "\n"; err != nil || out != want {
			t.Fatalf("%s update: %q %v", kind, out, err)
		}
		if got := env.lock().Plugins[srcLatest]; got.RefKind != "latest" || got.Tag != "v1.0.0" || got.Commit != c1 {
			t.Fatalf("%s converted %+v", kind, got)
		}
	}
}

// The hub refuses only the legacy entry: the other plugins resolve (lead C).
func TestLatestLegacyRefusesOnlyItsPlugin(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.homeRepo()
	if err := env.sync(env.config(srcM, srcHome)); err != nil {
		t.Fatal(err)
	}
	env.legacyLock("branch")
	loc := env.local("plugins/p", "p")
	c := env.load(env.config(srcLatest, srcHome, loc))
	resolve(&c)
	if e := c.entries[0]; e.err == nil || !strings.Contains(e.err.Error(), "@latest now means the highest version tag") {
		t.Fatalf("legacy: %v", e.err)
	}
	for _, e := range c.entries[1:] {
		if e.err != nil || e.cand == nil {
			t.Fatalf("%s: %v", e.Source, e.err)
		}
	}
}

// A config that leaves @latest returns the lock to version 1, byte for byte the lock of the other
// entries alone.
func TestLatestLeavesVersion2(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	r.git("tag", "v1.0.0")
	env.homeRepo()
	if err := env.sync(env.config(srcHome)); err != nil {
		t.Fatal(err)
	}
	only := env.lockBytes()
	if err := env.sync(env.config(srcHome, srcLatest)); err != nil || !bytes.HasPrefix(env.lockBytes(), []byte("{\n  \"version\": 2,")) {
		t.Fatalf("with latest: %v\n%s", err, env.lockBytes())
	}
	if out, err := env.run("sync", env.config(srcHome)); err != nil || out != "dropped "+srcLatest+"\n" || !bytes.Equal(only, env.lockBytes()) {
		t.Fatalf("after latest: %q %v\n%s", out, err, env.lockBytes())
	}
}

// An update that keeps the lock still prints the ignored lines, and the lock bytes stay. An
// output failure on that path does not claim a written lock.
func TestLatestIgnoredOnUnchanged(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	r.git("tag", "v1.0.0")
	cfg := env.config(srcLatest)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	r.git("tag", "v2.0.0", r.git("rev-parse", "HEAD^{tree}"))
	before := env.lockBytes()
	out, err := env.run("update", cfg)
	if err != nil || out != "ignored "+srcLatest+": refs/tags/v2.0.0 points to a tree, not a commit\n" || !bytes.Equal(before, env.lockBytes()) {
		t.Fatalf("update: %q %v", out, err)
	}
	for name, w := range map[string]io.Writer{"failed": failWriter{}, "short": shortWriter{}} {
		err := runPluginCmd(context.Background(), "update", cfg, nil, env.opts, w)
		if want := "plugin update: the lock is unchanged, but the result lines are not written: "; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
		if !bytes.Equal(before, env.lockBytes()) {
			t.Errorf("%s: the lock changed", name)
		}
	}
}

// setTag replaces the tag of the latest entry in the lock of env (a hand edit; the commit stays).
func (env *cmdEnv) setTag(tag string) {
	env.t.Helper()
	l := env.lock()
	l.Plugins[srcLatest].Tag = tag
	if err := writeLock(filepath.Join(env.dir, "hub.lock.json"), l); err != nil {
		env.t.Fatal(err)
	}
}

// pullWarn runs plugin pull like env.pull, and also gives the warning lines.
func (env *cmdEnv) pullWarn(cfg, doc string, take, keep []string) (string, string, error) {
	env.t.Helper()
	var warn bytes.Buffer
	env.opts.warn = &warn
	defer func() { env.opts.warn = nil }()
	out, _, err := env.pull(cfg, doc, take, keep)
	return out, warn.String(), err
}

// The merge compares commit, digest and tag. The same commit with a higher tag is taken, and the
// lock and the base advance to the tag. A lower tag is taken too (the peer went back on purpose),
// with a warning on standard error only: the result lines keep their form. A conflict with tags
// resolves with -take and -keep.
func TestLatestPull(t *testing.T) {
	a, b := machines(t)
	r := a.repo("lazy-rex", "Messaging", "messaging")
	c1 := r.git("rev-parse", "HEAD")
	r.git("tag", "v1.0.0")
	cfgA, cfgB := a.config(srcLatest), b.config(srcLatest)
	if err := a.sync(cfgA); err != nil {
		t.Fatal(err)
	}
	line := func(outcome, local, peer string) string {
		return outcome + "\t" + srcLatest + "\tlocal " + local + "\tpeer " + peer + "\n"
	}
	if out, warn, err := b.pullWarn(cfgB, a.mustExport(cfgA), nil, nil); err != nil || out != line("taken", "-", c1) || warn != "" {
		t.Fatalf("b takes: %q %q %v", out, warn, err)
	}
	if out, _, err := a.pullWarn(cfgA, b.mustExport(cfgB), nil, nil); err != nil || out != line("unchanged", c1, c1) {
		t.Fatalf("a unchanged: %q %v", out, err)
	}

	// The same commit, a higher tag.
	r.git("tag", "v1.1.0")
	if err := b.update(cfgB); err != nil || b.lock().Plugins[srcLatest].Tag != "v1.1.0" {
		t.Fatalf("b update: %v", err)
	}
	if out, warn, err := a.pullWarn(cfgA, b.mustExport(cfgB), nil, nil); err != nil || out != line("taken", c1, c1) || warn != "" {
		t.Fatalf("higher tag: %q %q %v", out, warn, err)
	}
	if le := a.lock().Plugins[srcLatest]; le.Tag != "v1.1.0" || le.Commit != c1 {
		t.Fatalf("a after the higher tag: %+v", le)
	}
	peers, err := readPeers(filepath.Join(a.dir, "hub.lock.json"))
	if err != nil || len(peers.Peers) != 1 {
		t.Fatalf("base: %+v %v", peers, err)
	}
	for _, pins := range peers.Peers {
		if p := pins[srcLatest]; p.Tag != "v1.1.0" || p.Commit != c1 {
			t.Fatalf("base pin %+v", p)
		}
	}
	if pb, _ := os.ReadFile(filepath.Join(a.dir, "hub.lock.json.peers.json")); !bytes.HasPrefix(pb, []byte("{\n  \"version\": 2,")) {
		t.Fatalf("base file:\n%s", pb)
	}

	// The peer goes back to v1.0.0 on purpose.
	b.setTag("v1.0.0")
	out, warn, err := a.pullWarn(cfgA, b.mustExport(cfgB), nil, nil)
	if err != nil || out != line("taken", c1, c1) || warn != "plugin pull: warning: "+srcLatest+": lower version v1.0.0 than the local v1.1.0\n" {
		t.Fatalf("lower tag: %q %q %v", out, warn, err)
	}
	if le := a.lock().Plugins[srcLatest]; le.Tag != "v1.0.0" {
		t.Fatalf("a after the lower tag: %+v", le)
	}

	// Both sides changed: a conflict; -take and -keep resolve it.
	a.setTag("v1.1.0")
	c2 := r.commit(map[string]string{"main.lua": "2"})
	r.git("tag", "v1.2.0")
	if err := b.update(cfgB); err != nil {
		t.Fatal(err)
	}
	doc := b.mustExport(cfgB)
	before := a.lockBytes()
	if out, _, err := a.pullWarn(cfgA, doc, nil, nil); err != nil || out != line("conflict", c1, c2) || !bytes.Equal(before, a.lockBytes()) {
		t.Fatalf("conflict: %q %v", out, err)
	}
	if out, warn, err := a.pullWarn(cfgA, doc, nil, []string{srcLatest}); err != nil || out != line("resolved-keep", c1, c2) || warn != "" ||
		!bytes.Equal(before, a.lockBytes()) {
		t.Fatalf("-keep: %q %q %v", out, warn, err)
	}
	// After -keep the base is the peer pin: the peer moves again, and the local pin differs from
	// the base, so it is a conflict again.
	c3 := r.commit(map[string]string{"main.lua": "3"})
	r.git("tag", "v1.3.0")
	if err := b.update(cfgB); err != nil {
		t.Fatal(err)
	}
	doc = b.mustExport(cfgB)
	if out, warn, err := a.pullWarn(cfgA, doc, []string{srcLatest}, nil); err != nil || out != line("resolved-take", c1, c3) || warn != "" {
		t.Fatalf("-take: %q %q %v", out, warn, err)
	}
	if la, lb := a.lock().Plugins[srcLatest], b.lock().Plugins[srcLatest]; la.lockPin != lb.lockPin || la.Tag != "v1.3.0" {
		t.Fatalf("-take: %+v, want %+v", la.lockPin, lb.lockPin)
	}

	// A warning that cannot be written is an error, after the lock and the base are written.
	b.setTag("v1.1.0")
	a.opts.warn = failWriter{}
	_, _, err = a.pull(cfgA, b.mustExport(cfgB), nil, nil)
	a.opts.warn = nil
	if err == nil || !strings.HasPrefix(err.Error(), "plugin pull: the lock and the base are written, but the warnings are not: ") {
		t.Fatalf("warning output: %v", err)
	}
	if le := a.lock().Plugins[srcLatest]; le.Tag != "v1.1.0" || le.Commit != c3 {
		t.Fatalf("after the warning failure: %+v", le)
	}
}

// The tag alone is a difference: the same commit and digest with another tag, and no base, is a
// conflict, not unchanged.
func TestLatestMergeComparesTag(t *testing.T) {
	p := testLatestPin()
	lock := &pluginLock{Version: 2, Plugins: map[string]*lockEntry{p.Source: {lockPin: p.lockPin}}}
	other := p
	other.Tag = "v1.3.0"
	plan, err := mergePins([]string{p.Source}, lock, nil, &pinDoc{Hub: pinHub, Entries: []pinEntry{other}}, nil, nil)
	if err != nil || len(plan.results) != 1 || plan.results[0].Outcome != pullConflict {
		t.Fatalf("got %+v %v", plan, err)
	}
	plan, err = mergePins([]string{p.Source}, lock, nil, &pinDoc{Hub: pinHub, Entries: []pinEntry{p}}, nil, nil)
	if err != nil || plan.results[0].Outcome != pullUnchanged || plan.base[p.Source].Tag != "v1.2.0" {
		t.Fatalf("got %+v %v", plan, err)
	}
}
