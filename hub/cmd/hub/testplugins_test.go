package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plugDir writes dir/sub/lazyrex.json with body and dir/sub/<main>.
func plugDir(t *testing.T, dir, sub, body, main string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, sub), 0o700)
	os.WriteFile(filepath.Join(dir, sub, "lazyrex.json"), []byte(body), 0o600)
	os.WriteFile(filepath.Join(dir, sub, main), nil, 0o600)
}

// test.plugins replaces requires (docs/plugin-sources.md, section 9): local directories with their
// own manifests, the hub's manifest rules, and harness errors for the home plugin and duplicates.
func TestTestPlugins(t *testing.T) {
	bad := map[string]string{
		`{"name":"p","main":"p.lua","requires":[{"name":"d","path":"p.lua"}]}`:                `unknown field "requires"`,
		`{"name":"p","main":"../p.lua"}`:                                                      "must be a file inside the plugin directory",
		`{"name":"p","main":"p.lua","test":{"plugins":[{"source":"/abs"}]}}`:                  "must be a directory relative to the manifest",
		`{"name":"p","main":"p.lua","test":{"plugins":[{"source":"~/x"}]}}`:                   "must be a directory relative to the manifest",
		`{"name":"p","main":"p.lua","test":{"plugins":[{"source":"github:o/r@main"}]}}`:       "must be a directory relative to the manifest",
		`{"name":"p","main":"p.lua","test":{"plugins":[{"source":"none"}]}}`:                  "test.plugins 1: no lazyrex.json",
		`{"name":"p","main":"p.lua","test":{"plugins":[{"source":"home"}]}}`:                  "the harness adds the home plugin lazyrex itself",
		`{"name":"p","main":"p.lua","test":{"plugins":[{"source":"same"}]}}`:                  "test.plugins 1: the name p is used twice",
		`{"name":"p","main":"p.lua","test":{"plugins":[{"source":"dep"},{"source":"dep2"}]}}`: "test.plugins 2: the name dep is used twice",
		`{"name":"p","main":"p.lua","test":{"plugins":[{"source":"badmain"}]}}`:               "test.plugins 1: plugin badmain: main ../x.lua must be a file inside",
		`{"name":"p","main":"p.lua","test":{"args":{"other":{"k":"v"}}}}`:                     "is not the plugin, lazyrex or a test.plugins entry",
	}
	for body, want := range bad {
		dir := t.TempDir()
		plugDir(t, dir, "home", `{"name":"lazyrex","main":"l.lua"}`, "l.lua")
		plugDir(t, dir, "same", `{"name":"p","main":"s.lua"}`, "s.lua")
		plugDir(t, dir, "dep", `{"name":"dep","main":"d.lua"}`, "d.lua")
		plugDir(t, dir, "dep2", `{"name":"dep","main":"d.lua"}`, "d.lua")
		plugDir(t, dir, "badmain", `{"name":"badmain","main":"../x.lua"}`, "b.lua")
		if _, err := manifestIn2(t, dir, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", body, err, want)
		}
	}

	// A dependency's events and args key come from its own manifest name.
	dir := t.TempDir()
	plugDir(t, dir, "deps/q", `{"name":"q","main":"q.lua","events":"all","depends":["p"]}`, "q.lua")
	m, err := manifestIn2(t, dir, `{"name":"p","main":"p.lua","test":{"plugins":[{"source":"deps/q"}],"args":{"q":{"k":"v"}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.deps) != 1 || m.deps[0].m.Name != "q" || m.deps[0].m.Events != "all" || m.deps[0].main != filepath.Join(dir, "deps", "q", "q.lua") {
		t.Errorf("deps: %+v", m.deps)
	}
}

// The harness applies the hub's graph rules to the plugins of a lab: a missing dependency and a
// cycle are harness errors.
func TestTestPluginGraph(t *testing.T) {
	dir := t.TempDir()
	plugDir(t, dir, "home", `{"name":"lazyrex","main":"l.lua"}`, "l.lua")
	plugDir(t, dir, "q", `{"name":"q","main":"q.lua","depends":["p"]}`, "q.lua")
	plugDir(t, dir, "r", `{"name":"r","main":"r.lua","depends":["missing"]}`, "r.lua")
	home := &testDep{dir: filepath.Join(dir, "home")}
	var err error
	if home.m, home.main, err = readPluginManifest(home.dir); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`{"name":"p","main":"p.lua","depends":["lazyrex","q"],"test":{"plugins":[{"source":"q"}]}}`: "dependency cycle",
		`{"name":"p","main":"p.lua","test":{"plugins":[{"source":"r"}]}}`:                           "plugin r: dependency missing is not configured",
		`{"name":"p","main":"p.lua","depends":["lazyrex"],"test":{"plugins":[{"source":"q"}]}}`:     "",
	}
	for body, want := range cases {
		m, err := manifestIn2(t, dir, body)
		if err != nil {
			t.Fatal(err)
		}
		err = m.checkPluginGraph(home)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: got %v, want %q", body, err, want)
		}
	}
	// The plugin under test is lazyrex: no separate home plugin, so a dependency on it is met.
	m, err := manifestIn2(t, dir, `{"name":"lazyrex","main":"p.lua","test":{"plugins":[{"source":"q2"}]}}`)
	if err == nil || !strings.Contains(err.Error(), "no lazyrex.json") {
		t.Fatalf("q2 is missing: %v", err)
	}
	plugDir(t, dir, "q2", `{"name":"q2","main":"q.lua","depends":["lazyrex"]}`, "q.lua")
	if m, err = manifestIn2(t, dir, `{"name":"lazyrex","main":"p.lua","test":{"plugins":[{"source":"q2"}]}}`); err != nil {
		t.Fatal(err)
	}
	if err := m.checkPluginGraph(nil); err != nil {
		t.Error(err)
	}
}

// The home plugin of this checkout loads with its test.plugins.
func TestHomePluginManifest(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	home, err := homePlugin(root)
	if err != nil {
		t.Fatal(err)
	}
	if home.main != filepath.Join(root, "plugins", "lazyrex", "lazyrex.lua") {
		t.Errorf("main %s", home.main)
	}
	m, err := loadManifest(home.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.checkPluginGraph(nil); err != nil {
		t.Error(err)
	}
	var names []string
	for _, d := range m.deps {
		names = append(names, d.m.Name)
	}
	if strings.Join(names, ",") != "messaging,launcher,harnessfix" {
		t.Errorf("test.plugins: %v", names)
	}
}

// manifestIn2 writes dir/lazyrex.json with body and dir/p.lua, then loads it.
func manifestIn2(t *testing.T, dir, body string) (*manifest, error) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "p.lua"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "lazyrex.json"), []byte(body), 0o600)
	return loadManifest(dir)
}
