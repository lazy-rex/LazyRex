package main

// Managed entries in the load and the resolution of a command environment (docs/plugin-sources.md,
// section 13). GitHub sources are local repositories through file:// URLs; the tests do not prove
// GitHub behavior.

import (
	"encoding/json"
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

const srcP = "github:lazy-rex/Profiles@main"

// The managed path of the hub's load: the default is <state>/plugins.json, with the hub's meaning of
// a relative state (the working directory, made absolute at the load); managed_plugins is relative
// to the config directory. The working directory and the config directory differ, so the two
// bases cannot agree by accident.
func TestManagedPaths(t *testing.T) {
	env := newCmdEnv(t)
	cfgDir, run := filepath.Join(env.dir, "cfg"), filepath.Join(env.dir, "run")
	os.MkdirAll(cfgDir, 0o700)
	os.MkdirAll(run, 0o700)
	t.Chdir(run)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	items, _ := json.Marshal(map[string]any{"plugins": []map[string]any{{"id": "prof", "source": srcP}}})
	cfg := filepath.Join(cfgDir, "hub.json")
	for name, c := range map[string]struct{ field, file, path string }{
		"default": {"", filepath.Join(run, "state", "plugins.json"), filepath.Join(wd, "state", "plugins.json")},
		"field":   {"m.json", filepath.Join(cfgDir, "m.json"), filepath.Join(cfgDir, "m.json")},
	} {
		os.MkdirAll(filepath.Dir(c.file), 0o700)
		os.WriteFile(c.file, items, 0o600)
		conf := map[string]any{"server": "unix:///tmp/lrt-x/rex.sock", "state": "state", "plugins": []any{}}
		if c.field != "" {
			conf["managed_plugins"] = c.field
		}
		b, _ := json.Marshal(conf)
		os.WriteFile(cfg, b, 0o600)
		l := env.load(cfg)
		if l.managedPath != c.path || l.managedErr != nil || len(l.entries) != 1 || l.entries[0].Source != srcP {
			t.Fatalf("%s: %s %v %d", name, l.managedPath, l.managedErr, len(l.entries))
		}
		os.Remove(c.file)
	}
}

// The graph covers config and managed entries; texts name a managed entry by its identity. The
// install of a managed entry does not check the graph of the other entries: a cycle refuses the
// entries at resolution.
func TestManagedGraph(t *testing.T) {
	env := newCmdEnv(t)
	env.local("plugins/a", "n")
	b := filepath.Join(env.dir, env.local("plugins/b", "n"))
	env.managed("x=" + b)
	c := env.load(env.config("plugins/a"))
	resolve(&c)
	if err := c.entries[0].err; err == nil || err.Error() != "entries 1 and managed:x have the same name n" {
		t.Fatalf("duplicate: %v", err)
	}
	// A managed plugin that depends on a config plugin, and the reverse.
	env.repo("lazy-rex", "Profiles", "profiles", "c")
	env.local("plugins/c", "c", "profiles")
	env.managed("prof=" + srcP)
	cfg := env.config("plugins/c")
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	c = env.load(cfg)
	resolve(&c)
	if err := c.entries[0].err; err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("cycle: %v", err)
	}
	env.local("plugins/c", "c")
	c = env.load(cfg)
	resolve(&c)
	for _, e := range c.entries {
		if e.err != nil {
			t.Fatalf("managed depends on config: %v", e.err)
		}
	}
	// A config plugin that depends on a managed plugin.
	env2 := newCmdEnv(t)
	env2.repo("lazy-rex", "Profiles", "profiles")
	env2.local("plugins/d", "d", "profiles")
	env2.managed("prof=" + srcP)
	cfg2 := env2.config("plugins/d")
	if err := env2.install(cfg2); err != nil {
		t.Fatal(err)
	}
	c = env2.load(cfg2)
	resolve(&c)
	for _, e := range c.entries {
		if e.err != nil {
			t.Fatalf("config depends on managed: %v", e.err)
		}
	}
}
