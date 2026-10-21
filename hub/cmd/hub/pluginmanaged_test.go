package main

// Managed entries in the plugin commands (docs/plugin-sources.md, section 13). GitHub sources are
// local repositories through file:// URLs; the tests do not prove GitHub behavior.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// managed writes the managed file at its default path, <state>/plugins.json. Each item is
// "id=source".
func (env *cmdEnv) managed(items ...string) string {
	env.t.Helper()
	plugins := []map[string]any{}
	for _, it := range items {
		id, src, _ := strings.Cut(it, "=")
		plugins = append(plugins, map[string]any{"id": id, "source": src})
	}
	b, _ := json.Marshal(map[string]any{"plugins": plugins})
	p := filepath.Join(env.state, "plugins.json")
	os.MkdirAll(env.state, 0o700)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		env.t.Fatal(err)
	}
	return p
}

func (env *cmdEnv) status(cfg string) (int, string) {
	var out, errb bytes.Buffer
	code := pluginCmdMain([]string{"status", "-config", cfg}, &out, &errb)
	return code, out.String() + errb.String()
}

const srcP = "github:lazy-rex/Profiles@main"

// sync installs a managed GitHub entry beside a config entry; a second sync changes nothing.
// status shows the identity of each entry.
func TestManagedSync(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.repo("lazy-rex", "Profiles", "profiles")
	env.managed("prof=" + srcP)
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	if l := env.lock(); l.Plugins[srcM] == nil || l.Plugins[srcP] == nil || l.Plugins[srcP].Name != "profiles" {
		t.Fatalf("%+v", l.Plugins)
	}
	before := env.lockBytes()
	if err := env.sync(cfg); err != nil || !bytes.Equal(before, env.lockBytes()) {
		t.Fatalf("second sync: %v", err)
	}
	code, out := env.status(cfg)
	if code != 0 || !strings.Contains(out, srcM+"\tref main\tcommit ") || !strings.Contains(out, "\tcache ok\tentry 1\n") ||
		!strings.Contains(out, "\tcache ok\tmanaged:prof\n") {
		t.Fatalf("status %d:\n%s", code, out)
	}
}

// A managed entry removed from the file: status reports its lock entry as orphaned, and sync drops
// it.
func TestManagedDrop(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.repo("lazy-rex", "Profiles", "profiles")
	env.managed("prof=" + srcP)
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	env.managed()
	if code, out := env.status(cfg); code != 1 || !strings.Contains(out, srcP+"\torphaned") {
		t.Fatalf("status %d:\n%s", code, out)
	}
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	if l := env.lock(); l.Plugins[srcP] != nil || l.Plugins[srcM] == nil {
		t.Fatalf("%+v", l.Plugins)
	}
}

// A malformed managed file fails sync, update and status, and the lock bytes stay the same. A
// missing file is an empty list.
func TestManagedMalformed(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.repo("lazy-rex", "Profiles", "profiles")
	env.managed("prof=" + srcP)
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	before := env.lockBytes()
	p := filepath.Join(env.state, "plugins.json")
	big := `{"plugins":[]}` + strings.Repeat(" ", managedFileLimit)
	bodies := map[string]string{
		`{`:                "unexpected EOF",
		`{"plugins":null}`: "the file must be an object with a plugins array",
		`{"plugins":[{"id":"a","source":"` + srcP + `"},{"id":"a","source":"github:o/r@main"}]}`: "managed:a: the id is used twice",
		big: "the file is over the 1048576-byte limit",
	}
	for body, want := range bodies {
		os.WriteFile(p, []byte(body), 0o600)
		prefix := "managed plugin file " + p + ": "
		for name, run := range map[string]func() error{
			"sync":   func() error { return env.sync(cfg) },
			"update": func() error { return env.update(cfg) },
		} {
			err := run()
			if err == nil || !strings.HasPrefix(err.Error(), "plugin "+name+": "+prefix) || !strings.Contains(err.Error(), want) ||
				!strings.HasSuffix(err.Error(), "; the lock is unchanged") {
				t.Errorf("%s %.20s: %v", name, body, err)
			}
		}
		if code, out := env.status(cfg); code != 1 || !strings.Contains(out, "plugin status: "+prefix) || !strings.Contains(out, want) {
			t.Errorf("status %.20s: %d %s", body, code, out)
		}
		if !bytes.Equal(before, env.lockBytes()) {
			t.Errorf("%.20s: the lock changed", body)
		}
	}
	os.Remove(p)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	if l := env.lock(); l.Plugins[srcP] != nil {
		t.Fatal("a missing managed file kept the managed lock entry")
	}
}

// The managed path: the default is <state>/plugins.json, with the hub's meaning of a relative state
// (the working directory); managed_plugins is relative to the config directory. The working
// directory and the config directory differ, so the two bases cannot agree by accident.
func TestManagedPaths(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Profiles", "profiles")
	cfgDir, run := filepath.Join(env.dir, "cfg"), filepath.Join(env.dir, "run")
	os.MkdirAll(cfgDir, 0o700)
	os.MkdirAll(run, 0o700)
	t.Cleanup(func() { removeTree(filepath.Join(run, "state")) })
	t.Chdir(run)
	items, _ := json.Marshal(map[string]any{"plugins": []map[string]any{{"id": "prof", "source": srcP}}})
	cfg := filepath.Join(cfgDir, "hub.json")
	for name, c := range map[string]struct{ field, file string }{
		"default": {"", filepath.Join(run, "state", "plugins.json")},
		"field":   {"m.json", filepath.Join(cfgDir, "m.json")},
	} {
		os.MkdirAll(filepath.Dir(c.file), 0o700)
		os.WriteFile(c.file, items, 0o600)
		conf := map[string]any{"state": "state", "plugins": []any{}}
		if c.field != "" {
			conf["managed_plugins"] = c.field
		}
		b, _ := json.Marshal(conf)
		os.WriteFile(cfg, b, 0o600)
		if err := pluginSync(context.Background(), cfg, env.opts); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		l, err := readLock(filepath.Join(cfgDir, "hub.lock.json"))
		if err != nil || l.Plugins[srcP] == nil {
			t.Fatalf("%s: %v %+v", name, err, l)
		}
		if _, err := os.Stat(filepath.Join(run, "state", "plugins", "github", "lazy-rex", "Profiles")); err != nil {
			t.Fatalf("%s: the cache is not under the working directory: %v", name, err)
		}
		os.Remove(c.file)
		os.Remove(filepath.Join(cfgDir, "hub.lock.json"))
	}
}

// A managed entry whose source the config also names is refused at the hub, so it blocks sync and
// update, before any git call, and the lock stays the same. status reports it and fails. This
// holds for a GitHub and for a local source.
func TestManagedStatic(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	before := env.lockBytes()
	env.managed("dup=" + srcM)
	static := "managed:dup: the source " + srcM + " is also in the config"
	env.opts.remote = func(owner, repo string) string { t.Fatal("a git call"); return "" }
	for _, run := range []func() error{
		func() error { return env.sync(cfg) },
		func() error { return env.update(cfg) },
		func() error { return env.update(cfg, "managed:dup") },
	} {
		if err := run(); err == nil || !strings.HasSuffix(err.Error(), ": "+static+"; the lock is unchanged") {
			t.Errorf("got %v", err)
		}
	}
	if !bytes.Equal(before, env.lockBytes()) {
		t.Fatal("the lock changed")
	}
	if code, out := env.status(cfg); code != 1 || !strings.Contains(out, srcM+"\trefused: "+static+"\tmanaged:dup\n") {
		t.Fatalf("status %d:\n%s", code, out)
	}
	// A local source.
	local := filepath.Join(env.dir, env.local("plugins/p", "p"))
	env.managed("loc=" + local)
	cfg = env.config(srcM, "plugins/p")
	if err := env.sync(cfg); err == nil || !strings.Contains(err.Error(), "managed:loc: the source "+local+" is also in the config; the lock is unchanged") {
		t.Fatalf("local: %v", err)
	}
	if code, out := env.status(cfg); code != 1 || !strings.Contains(out, "refused: managed:loc: the source") {
		t.Fatalf("local status %d:\n%s", code, out)
	}
}

// The graph covers config and managed entries; texts name a managed entry by its identity.
func TestManagedGraph(t *testing.T) {
	env := newCmdEnv(t)
	env.local("plugins/a", "n")
	b := filepath.Join(env.dir, env.local("plugins/b", "n"))
	env.managed("x=" + b)
	err := env.sync(env.config("plugins/a"))
	if err == nil || err.Error() != "plugin sync: entries 1 and managed:x have the same name n; the lock is unchanged" {
		t.Fatalf("duplicate: %v", err)
	}
	// A managed plugin that depends on a config plugin, and the reverse.
	env.repo("lazy-rex", "Profiles", "profiles", "c")
	env.local("plugins/c", "c", "profiles")
	env.managed("prof=" + srcP)
	cfg := env.config("plugins/c")
	if err := env.sync(cfg); err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("cycle: %v", err)
	}
	env.local("plugins/c", "c")
	if err := env.sync(cfg); err != nil {
		t.Fatalf("managed depends on config: %v", err)
	}
	// A config plugin that depends on a managed plugin.
	env2 := newCmdEnv(t)
	env2.repo("lazy-rex", "Profiles", "profiles")
	env2.local("plugins/d", "d", "profiles")
	env2.managed("prof=" + srcP)
	if err := env2.sync(env2.config("plugins/d")); err != nil {
		t.Fatalf("config depends on managed: %v", err)
	}
}

// The selector with managed entries: managed:<id> selects only by id; a bare word selects by name
// or source; identities in the text.
func TestManagedSelector(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.repo("lazy-rex", "Profiles", "profiles")
	// The managed id "messaging" equals the plugin name of config entry 1.
	env.managed("messaging=" + srcP)
	cfg := env.config(srcM)
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	pins := func() (string, string) {
		l := env.lock()
		return l.Plugins[srcM].Commit, l.Plugins[srcP].Commit
	}
	_, p0 := pins()
	m1 := env.repos["lazy-rex/Messaging"].commit(map[string]string{"main.lua": "2"})
	p1 := env.repos["lazy-rex/Profiles"].commit(map[string]string{"main.lua": "2"})
	// The bare word selects the name: config entry 1.
	if err := env.update(cfg, "messaging"); err != nil {
		t.Fatal(err)
	}
	if m, p := pins(); m != m1 || p != p0 {
		t.Fatalf("bare word: %s %s", m, p)
	}
	// managed:messaging selects the managed entry.
	if err := env.update(cfg, "managed:messaging"); err != nil {
		t.Fatal(err)
	}
	if m, p := pins(); m != m1 || p != p1 {
		t.Fatalf("managed id: %s %s", m, p)
	}
	for arg, want := range map[string]string{
		"managed:nope": "plugin update: no entry has the name, source or identity managed:nope",
		"nope":         "plugin update: no entry has the name, source or identity nope",
	} {
		if err := env.update(cfg, arg); err == nil || err.Error() != want {
			t.Errorf("%s: %v", arg, err)
		}
	}
	// A bare managed id that is no name or source selects nothing.
	env2 := newCmdEnv(t)
	env2.repo("lazy-rex", "Profiles", "profiles")
	env2.managed("pid=" + srcP)
	cfg2 := env2.config()
	if err := env2.sync(cfg2); err != nil {
		t.Fatal(err)
	}
	if err := env2.update(cfg2, "pid"); err == nil || err.Error() != "plugin update: no entry has the name, source or identity pid" {
		t.Fatalf("bare id: %v", err)
	}
	// A managed local entry gives the local-source text.
	loc := filepath.Join(env2.dir, env2.local("plugins/l", "l"))
	env2.managed("pid="+srcP, "loc="+loc)
	if err := env2.update(cfg2, "managed:loc"); err == nil || err.Error() != "plugin update: managed:loc is a local source; update changes GitHub sources only" {
		t.Fatalf("local: %v", err)
	}
}

// One name in a config entry and a managed entry: the bare name selects two entries.
func TestManagedSelectorShared(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.repo("lazy-rex", "Messaging2", "messaging")
	src2 := "github:lazy-rex/Messaging2@main"
	// Install each alone, then join the two lock entries: the graph refuses the shared name, so no
	// single sync installs both.
	if err := env.sync(env.config(srcM)); err != nil {
		t.Fatal(err)
	}
	a := env.lock().Plugins[srcM]
	if err := env.sync(env.config(src2)); err != nil {
		t.Fatal(err)
	}
	b := env.lock().Plugins[src2]
	env.writeLockRaw(&pluginLock{Version: 1, Plugins: map[string]*lockEntry{srcM: a, src2: b}})
	env.managed("m2=" + src2)
	cfg := env.config(srcM)
	err := env.update(cfg, "messaging")
	if err == nil || err.Error() != "plugin update: messaging selects entry 1, managed:m2; write managed:<id> or the exact source" {
		t.Fatalf("got %v", err)
	}
}

// A managed subdirectory source installs, and lockedDir gives the plugin subdirectory.
func TestManagedSubdir(t *testing.T) {
	env := newCmdEnv(t)
	env.homeRepo()
	env.managed("home=" + srcHome)
	cfg := env.config()
	if err := env.sync(cfg); err != nil {
		t.Fatal(err)
	}
	le := env.lock().Plugins[srcHome]
	if le == nil || le.Subdir != "plugins/lazyrex" {
		t.Fatalf("%+v", le)
	}
	dir, _, err := lockedDir(cfg, env.state, env.lock(), githubEntry(t, srcHome))
	if err != nil || !strings.HasSuffix(dir, filepath.Join("plugins", "lazyrex")) {
		t.Fatalf("%s %v", dir, err)
	}
}

// The command reads the managed file only under both flocks: a held flock stops it before it
// reads a malformed managed file.
func TestManagedReadUnderFlock(t *testing.T) {
	env := newCmdEnv(t)
	cfg := env.config()
	os.WriteFile(filepath.Join(env.state, "plugins.json"), []byte(`{`), 0o600)
	pid := holdFlocks(t, env.state, filepath.Join(env.dir, "hub.lock.json"))
	want := fmt.Sprintf("plugin sync: another plugin command runs (pid %d)", pid)
	if err := env.sync(cfg); err == nil || err.Error() != want {
		t.Fatalf("got %v", err)
	}
	if code, out := env.status(cfg); code != 1 || !strings.Contains(out, fmt.Sprintf("another plugin command runs (pid %d)", pid)) {
		t.Fatalf("status %d: %s", code, out)
	}
}
