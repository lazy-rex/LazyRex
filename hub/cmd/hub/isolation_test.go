package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReservedEnvKey(t *testing.T) {
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "LAZYREX_AGENT", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "REX_SERVER", "REX_SESSION"} {
		if !reservedEnvKey(k) {
			t.Errorf("%s is not reserved", k)
		}
	}
	for _, k := range []string{"LANG", "GIT_AUTHOR_NAME", "XDGX"} {
		if reservedEnvKey(k) {
			t.Errorf("%s is reserved", k)
		}
	}
}

func TestExpandValue(t *testing.T) {
	vars := map[string]string{"LAB": "/lab", "PLUGIN": "/plugin"}
	if v, err := expandValue("${LAB}/a:${PLUGIN}", vars); err != nil || v != "/lab/a:/plugin" {
		t.Errorf("got %q %v", v, err)
	}
	for _, bad := range []string{"${HOME}/x", "${}", "${LAB", "a${PWD}"} {
		if _, err := expandValue(bad, vars); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestCheckPath(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	lab := filepath.Join(base, "lab")
	plugin := filepath.Join(base, "plugin")
	other := filepath.Join(base, "outside")
	for _, d := range []string{lab, plugin, other, filepath.Join(base, "lab-other")} {
		os.MkdirAll(d, 0o700)
	}
	os.Symlink(other, filepath.Join(plugin, "escape"))
	os.Symlink(filepath.Join(plugin, "deep"), filepath.Join(lab, "inside"))
	os.MkdirAll(filepath.Join(plugin, "deep", "x"), 0o700)
	os.Symlink(filepath.Join(base, "absent"), filepath.Join(lab, "dangling")) // an outside target that does not exist
	os.Symlink(filepath.Join(lab, "loop"), filepath.Join(lab, "loop"))
	locked := filepath.Join(lab, "locked")
	os.MkdirAll(filepath.Join(locked, "in"), 0o700)
	os.Chmod(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	roots := []string{lab, plugin}
	good := []string{lab, filepath.Join(lab, "new", "file"), filepath.Join(plugin, "deep"), filepath.Join(lab, "inside", "x"),
		filepath.Join(lab, "missing-final")}
	for _, p := range good {
		if err := checkPath(p, roots); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	bad := map[string]string{
		filepath.Join(plugin, "escape", "f"):    "outside",  // a symlink escape
		plugin + "/escape/../x":                 "..",       // a symlink followed by ..
		lab + "/../outside":                     "..",       // traversal
		filepath.Join(base, "lab-other", "f"):   "outside",  // a sibling with the same prefix
		"cache/../../outside":                   "absolute", // relative traversal
		"relative/file":                         "absolute",
		"~/x":                                   "~",
		"/":                                     "outside",
		filepath.Join(lab, "dangling", "data"):  "does not resolve", // a dangling symlink out of the lab
		filepath.Join(lab, "dangling"):          "does not resolve",
		filepath.Join(lab, "loop", "x"):         "does not resolve",
		filepath.Join(lab, "locked", "in", "f"): "does not resolve", // a permission error
	}
	for p, want := range bad {
		if err := checkPath(p, roots); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", p, err, want)
		}
	}
	// A tool from PATH is accepted outside the roots.
	bin := filepath.Join(base, "bin")
	os.MkdirAll(bin, 0o700)
	os.WriteFile(filepath.Join(bin, "tool"), []byte("#!/bin/sh\n"), 0o700)
	os.WriteFile(filepath.Join(bin, "data"), []byte("x"), 0o600)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := checkPath(filepath.Join(bin, "tool"), roots); err != nil {
		t.Errorf("a PATH tool: %v", err)
	}
	if err := checkPath(filepath.Join(bin, "data"), roots); err == nil {
		t.Error("a file that is not executable in a PATH dir was accepted")
	}
}

func TestPathLike(t *testing.T) {
	for v, want := range map[string]bool{"Lab User": false, "a/b": true, ".x": true, "~": true, "/x": true, "plain": false} {
		if pathLike(v) != want {
			t.Errorf("%q: got %v", v, !want)
		}
	}
}

func manifestIn(t *testing.T, body string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "dep"), 0o700)
	os.WriteFile(filepath.Join(dir, "dep", "lazyrex.json"), []byte(`{"name":"dep","main":"dep.lua"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "dep", "dep.lua"), []byte(""), 0o600)
	os.WriteFile(filepath.Join(dir, "p.lua"), []byte(""), 0o600)
	os.WriteFile(filepath.Join(dir, "profiles.json"), []byte(`{"profiles":[]}`), 0o600)
	os.WriteFile(filepath.Join(dir, "lazyrex.json"), []byte(body), 0o600)
	_, err := loadManifest(dir)
	return dir, err
}

func TestManifestIsolation(t *testing.T) {
	ok := `{"name":"p","main":"p.lua","test":{"plugins":[{"source":"dep"}],"env":{"A":"x","B":"${LAB}/b"},
		"args":{"p":{"root":"${PLUGIN}/r"},"dep":{"k":"v"}},"profiles":"profiles.json"}}`
	if _, err := manifestIn(t, ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		`{"name":"p","main":"p.lua","test":{"env":{"HOME":"/x"}}}`:                           "not allowed",
		`{"name":"p","main":"p.lua","test":{"env":{"REX_SERVER":"x"}}}`:                      "not allowed",
		`{"name":"p","main":"p.lua","test":{"env":{"XDG_DATA_HOME":"x"}}}`:                   "not allowed",
		`{"name":"p","main":"p.lua","test":{"env":{"A":"${HOME}/x"}}}`:                       "unknown placeholder",
		`{"name":"p","main":"p.lua","test":{"args":{"nope":{"k":"v"}}}}`:                     "not the plugin",
		`{"name":"p","main":"p.lua","test":{"args":{"p":{"instance_id":"v"}}}}`:              "not allowed",
		`{"name":"p","main":"p.lua","test":{"profiles":"missing.json"}}`:                     "not a file",
		`{"name":"p","main":"p.lua","test":{"profiles":"../profiles.json"}}`:                 "must stay",
		`{"name":"p","main":"p.lua","test":{"profiles":"/etc/hosts"}}`:                       "relative",
		`{"name":"p","main":"p.lua","test":{"server":"x"}}`:                                  "unknown field",
		`{"name":"p","main":"p.lua","test":{"files":{"tests/a.lua":{}}}}`:                    "base name",
		`{"name":"p","main":"p.lua","test":{"files":{"a.lua":{"args":{"nope":{"k":"v"}}}}}}`: "not the plugin",
		`{"name":"p","main":"p.lua","test":{"files":{"a.lua":{"args":{"p":{"main":"v"}}}}}}`: "not allowed",
	}
	for body, want := range bad {
		if _, err := manifestIn(t, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", body, err, want)
		}
	}
	// Paths are checked per lab.
	dir, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{"env":{"A":"/etc/x"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := loadManifest(dir)
	if _, err := m.labValues(dir, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("an env path outside the roots: %v", err)
	}
	m.Test.Env = map[string]string{"A": "cache/../../outside"}
	if _, err := m.labValues(dir, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("a relative traversal: %v", err)
	}
}

func TestTestProfileCheck(t *testing.T) {
	lab, _ := filepath.EvalSymlinks(t.TempDir())
	roots := []string{lab}
	p := Profile{ID: "w", Name: "W", Kind: "accessory", Command: []string{"/bin/sh"}}
	if err := testProfileCheck(p, roots); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		env  map[string]string
		cwd  string
		want string
	}{
		{map[string]string{"HOME": lab}, "", "reserved"},
		{map[string]string{"REX_SERVER": "x"}, "", "reserved"},
		{map[string]string{"D": "/"}, "", "outside"},
		{nil, "/", "outside"},
		{nil, lab + "-other", "outside"},
	} {
		q := p
		q.Env, q.Cwd = c.env, c.cwd
		if err := testProfileCheck(q, roots); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v %q: got %v", c.env, c.cwd, err)
		}
	}
}

// test.files args go over test.args, for one file's lab only.
func TestTestFileArgs(t *testing.T) {
	dir, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{"args":{"p":{"a":"1","b":"2"}},"files":{"g.lua":{"args":{"p":{"b":"3","c":"${LAB}"}}}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := loadManifest(dir)
	lab := t.TempDir()
	plain, err := m.labValues(dir, lab, nil)
	if err != nil {
		t.Fatal(err)
	}
	file, err := m.labValues(dir, lab, m.Test.Files["g.lua"].Args)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(plain.args["p"]); got != "map[a:1 b:2]" {
		t.Errorf("without the file: %s", got)
	}
	if got := fmt.Sprint(file.args["p"]); got != "map[a:1 b:3 c:"+lab+"]" {
		t.Errorf("with the file: %s", got)
	}
}

// test.files prompts replaces test.prompts for one file: a list, or false for no grant. A file
// without the entry keeps test.prompts.
func TestTestFilePrompts(t *testing.T) {
	dir, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{"prompts":["p"],"files":{"none.lua":{"prompts":false},"list.lua":{"prompts":["lazyrex"]},"args.lua":{"args":{"p":{"a":"1"}}}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	o := &testOpts{m: m}
	for file, want := range map[string]string{"none.lua": "[]", "list.lua": "[lazyrex]", "args.lua": "[p]", "other.lua": "[p]"} {
		o.filePrompts = m.Test.Files[file].Prompts
		if got := fmt.Sprint(o.promptGrant()); got != want {
			t.Errorf("%s: grant %s, want %s", file, got, want)
		}
	}
	for raw, want := range map[string]string{
		`{"x.lua":{"prompts":true}}`:      "a list of plugin names, or false",
		`{"x.lua":{"prompts":"p"}}`:       "a list of plugin names, or false",
		`{"x.lua":{"prompts":null}}`:      "a list of plugin names, or false",
		`{"x.lua":{"prompts":["ghost"]}}`: `"ghost" is not the plugin`,
	} {
		dir, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{"files":`+raw+`}}`)
		if err == nil {
			_, err = loadManifest(dir)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", raw, err, want)
		}
	}
}
