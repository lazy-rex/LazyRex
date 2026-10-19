package main

import (
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
		filepath.Join(plugin, "escape", "f"):  "outside",  // a symlink escape
		plugin + "/escape/../x":               "..",       // a symlink followed by ..
		lab + "/../outside":                   "..",       // traversal
		filepath.Join(base, "lab-other", "f"): "outside",  // a sibling with the same prefix
		"cache/../../outside":                 "absolute", // relative traversal
		"relative/file":                       "absolute",
		"~/x":                                 "~",
		"/":                                   "outside",
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
	os.WriteFile(filepath.Join(dir, "p.lua"), []byte(""), 0o600)
	os.WriteFile(filepath.Join(dir, "profiles.json"), []byte(`{"profiles":[]}`), 0o600)
	os.WriteFile(filepath.Join(dir, "lazyrex.json"), []byte(body), 0o600)
	_, err := loadManifest(dir)
	return dir, err
}

func TestManifestIsolation(t *testing.T) {
	ok := `{"name":"p","main":"p.lua","requires":[{"name":"dep","path":"p.lua"}],"test":{"env":{"A":"x","B":"${LAB}/b"},
		"args":{"p":{"root":"${PLUGIN}/r"},"dep":{"k":"v"}},"profiles":"profiles.json"}}`
	if _, err := manifestIn(t, ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		`{"name":"p","main":"p.lua","test":{"env":{"HOME":"/x"}}}`:              "not allowed",
		`{"name":"p","main":"p.lua","test":{"env":{"REX_SERVER":"x"}}}`:         "not allowed",
		`{"name":"p","main":"p.lua","test":{"env":{"XDG_DATA_HOME":"x"}}}`:      "not allowed",
		`{"name":"p","main":"p.lua","test":{"env":{"A":"${HOME}/x"}}}`:          "unknown placeholder",
		`{"name":"p","main":"p.lua","test":{"args":{"nope":{"k":"v"}}}}`:        "not the plugin",
		`{"name":"p","main":"p.lua","test":{"args":{"p":{"instance_id":"v"}}}}`: "not allowed",
		`{"name":"p","main":"p.lua","test":{"profiles":"missing.json"}}`:        "not a file",
		`{"name":"p","main":"p.lua","test":{"profiles":"../profiles.json"}}`:    "must stay",
		`{"name":"p","main":"p.lua","test":{"profiles":"/etc/hosts"}}`:          "relative",
		`{"name":"p","main":"p.lua","test":{"server":"x"}}`:                     "unknown field",
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
	if _, err := m.labValues(dir, t.TempDir()); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("an env path outside the roots: %v", err)
	}
	m.Test.Env = map[string]string{"A": "cache/../../outside"}
	if _, err := m.labValues(dir, t.TempDir()); err == nil || !strings.Contains(err.Error(), "absolute") {
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
