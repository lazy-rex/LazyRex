package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePlugin makes dir/lazyrex.json with body and dir/p.lua.
func writePlugin(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "p.lua"), []byte("-- plugin\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "lazyrex.json"), []byte(body), 0o644)
}

func TestReadPluginManifest(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, `{"name":"p","main":"p.lua","events":"all","notice":"Hi.","depends":["profiles"],"test":{"agents":[]}}`)
	m, main, err := readPluginManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "p" || m.Events != "all" || m.Notice != "Hi." || len(m.Depends) != 1 || main != filepath.Join(dir, "p.lua") {
		t.Errorf("got %+v, %s", m, main)
	}

	bad := map[string]string{
		`{"main":"p.lua"}`:                                "name",
		`{"name":"P","main":"p.lua"}`:                     "name",
		`{"name":"p"}`:                                    "main is required",
		`{"name":"p","main":"p.lua","events":"some"}`:     "events",
		`{"name":"p","main":"p.lua","notice":"a\nb"}`:     "control characters",
		`{"name":"p","main":"p.lua","depends":["Bad"]}`:   "not a plugin name",
		`{"name":"p","main":"p.lua","depends":["a","a"]}`: "appears twice",
		`{"name":"p","main":"p.lua","path":"x"}`:          "unknown field",
		`{"name":"p","main":"missing.lua"}`:               "does not exist",
		`{"name":"p","main":"../p.lua"}`:                  "inside the plugin directory",
		`{"name":"p","main":"/etc/hosts"}`:                "inside the plugin directory",
		`{"name":"p","main":"./p.lua"}`:                   "inside the plugin directory",
		`{"name":"p","main":"lib"}`:                       "inside the plugin directory",
	}
	for body, want := range bad {
		d := t.TempDir()
		writePlugin(t, d, body)
		os.Mkdir(filepath.Join(d, "lib"), 0o755)
		if _, _, err := readPluginManifest(d); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", body, err, want)
		}
	}
	if _, _, err := readPluginManifest(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no lazyrex.json") {
		t.Errorf("missing manifest: %v", err)
	}
}

func TestPluginMainSymlinks(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	writePlugin(t, src, `{"name":"p","main":"link.lua"}`)

	// A symlink main is refused, also when it points inside the source.
	os.Symlink("p.lua", filepath.Join(src, "link.lua"))
	if _, _, err := readPluginManifest(src); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlink main inside: %v", err)
	}

	// The source directory itself may be a symlink (a release clone path).
	os.WriteFile(filepath.Join(src, "lazyrex.json"), []byte(`{"name":"p","main":"p.lua"}`), 0o644)
	alias := filepath.Join(root, "alias")
	os.Symlink(src, alias)
	if _, main, err := readPluginManifest(alias); err != nil || main != filepath.Join(alias, "p.lua") {
		t.Errorf("symlinked source dir: %s %v", main, err)
	}

	// A symlinked parent directory that resolves inside the source is accepted.
	os.MkdirAll(filepath.Join(src, "real"), 0o755)
	os.WriteFile(filepath.Join(src, "real", "m.lua"), []byte(""), 0o644)
	os.Symlink("real", filepath.Join(src, "lib"))
	if _, err := pluginMain(src, "lib/m.lua"); err != nil {
		t.Errorf("parent inside: %v", err)
	}

	// A symlinked parent directory that resolves outside is refused.
	outside := filepath.Join(root, "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "m.lua"), []byte(""), 0o644)
	os.Symlink(outside, filepath.Join(src, "ext"))
	if _, err := pluginMain(src, "ext/m.lua"); err == nil || !strings.Contains(err.Error(), "inside the plugin directory") {
		t.Errorf("parent outside: %v", err)
	}
}

func entries(spec ...[]string) []*sourceEntry {
	var out []*sourceEntry
	for i, s := range spec {
		out = append(out, &sourceEntry{Index: i + 1, Source: "src/" + s[0] + "/" + string(rune('a'+i)), Name: s[0], Depends: s[1:]})
	}
	return out
}

func errs(es []*sourceEntry) map[int]string {
	out := map[int]string{}
	for _, e := range es {
		if e.Err != nil {
			out[e.Index] = e.Err.Error()
		}
	}
	return out
}

func TestCheckGraph(t *testing.T) {
	type tc struct {
		name string
		es   []*sourceEntry
		want map[int]string // index -> substring of the error; absent: no error
	}
	refused := entries([]string{"profiles"}, []string{"worktrees", "profiles"}, []string{"top", "worktrees"})
	refused[0].Err = graphErr("plugin profiles: the cache has no entry")
	unnamed := entries([]string{"worktrees", "profiles"})
	unnamed = append(unnamed, &sourceEntry{Index: 2, Source: "../Profiles", Err: graphErr("no lazyrex.json in /x")})
	cases := []tc{
		{"no dependencies", entries([]string{"a"}, []string{"b"}), map[int]string{}},
		{"satisfied", entries([]string{"a", "b"}, []string{"b"}), map[int]string{}},
		{"missing", entries([]string{"worktrees", "profiles"}), map[int]string{1: "plugin worktrees: dependency profiles is not configured"}},
		{"refused, transitive", refused, map[int]string{
			1: "the cache has no entry",
			2: "plugin worktrees: dependency profiles is refused: plugin profiles: the cache has no entry",
			3: "plugin top: dependency worktrees is refused: plugin worktrees: dependency profiles is refused"}},
		{"unnamed entry satisfies nothing", unnamed, map[int]string{1: "dependency profiles is not configured", 2: "no lazyrex.json"}},
		{"cycle", entries([]string{"a", "b"}, []string{"b", "a"}, []string{"c", "a"}), map[int]string{
			1: "plugin a: dependency cycle a -> b -> a",
			2: "plugin b: dependency cycle a -> b -> a",
			3: "plugin c: dependency a is refused: plugin a: dependency cycle"}},
		{"self", entries([]string{"a", "a"}), map[int]string{1: "plugin a: dependency cycle a -> a"}},
		{"duplicate names", entries([]string{"m"}, []string{"x"}, []string{"m"}, []string{"d", "m"}), map[int]string{
			1: "entries 1 and 3 have the same name m",
			3: "entries 1 and 3 have the same name m",
			4: "plugin d: dependency m is refused: entries 1 and 3 have the same name m"}},
	}
	for _, c := range cases {
		checkGraph(c.es)
		got := errs(c.es)
		for i, want := range c.want {
			if !strings.Contains(got[i], want) {
				t.Errorf("%s: entry %d: got %q, want %q", c.name, i, got[i], want)
			}
		}
		for i, g := range got {
			if _, ok := c.want[i]; !ok {
				t.Errorf("%s: entry %d: unexpected %q", c.name, i, g)
			}
		}
	}
}

type graphErr string

func (e graphErr) Error() string { return string(e) }

func TestStartOrder(t *testing.T) {
	es := entries([]string{"top", "mid", "base"}, []string{"other"}, []string{"mid", "base"}, []string{"base"}, []string{"bad", "missing"})
	checkGraph(es)
	var names []string
	for _, e := range startOrder(es) {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, " "); got != "base mid top other" {
		t.Errorf("order: %s", got)
	}
}

func TestDuplicateSources(t *testing.T) {
	es := []*sourceEntry{{Index: 1, Source: "../A"}, {Index: 2, Source: "github:o/r@main"}, {Index: 3, Source: "github:o/r@main"}}
	if err := duplicateSources(es); err == nil || err.Error() != "entries 2 and 3 have the same source github:o/r@main" {
		t.Errorf("got %v", err)
	}
	if err := duplicateSources(es[:2]); err != nil {
		t.Error(err)
	}
}
