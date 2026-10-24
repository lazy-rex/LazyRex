package main

// Subdir sources (docs/plugin-sources.md, sections 1, 3, 4 and 5): github:<owner>/<repo>/<subdir>@<ref>.
// The cache entry is the whole commit; the plugin directory is <entry>/<subdir>. GitHub sources are
// local repositories through file:// URLs; the tests do not prove GitHub behavior.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubdirSourceParse(t *testing.T) {
	good := map[string]githubRef{
		"github:lazy-rex/LazyRex@main":                 {"lazy-rex", "LazyRex", "", "main"},
		"github:lazy-rex/LazyRex/plugins@main":         {"lazy-rex", "LazyRex", "plugins", "main"},
		"github:lazy-rex/LazyRex/plugins/lazyrex@main": {"lazy-rex", "LazyRex", "plugins/lazyrex", "main"},
		"github:o/r/a.b/_c-d/.e@v1":                    {"o", "r", "a.b/_c-d/.e", "v1"},
		"github:o/r/p@rel@1#x":                         {"o", "r", "p", "rel@1#x"},
		"github:o/r@a@b":                               {"o", "r", "", "a@b"},
	}
	for src, want := range good {
		if got := githubEntry(t, src).githubRef(); got != want {
			t.Errorf("%s: %+v, want %+v", src, got, want)
		}
	}
	subdir := func(d string) string {
		return "subdir " + d + " must be a relative path of names (A-Z, a-z, 0-9, ., _, -)"
	}
	bad := map[string]string{
		"github:o/r/@main":       subdir(""),
		"github:o/r//p@main":     subdir("/p"),
		"github:o/r/p/@main":     subdir("p/"),
		"github:o/r/p//q@main":   subdir("p//q"),
		"github:o/r/./p@main":    subdir("./p"),
		"github:o/r/p/..@main":   subdir("p/.."),
		"github:o/r/a b@main":    subdir("a b"),
		"github:o/r/plügin@main": subdir("plügin"),
		"github:o/r/p@":          "write github:<owner>/<repo>@<ref>",
		"github:o/r/p":           "write github:<owner>/<repo>@<ref>",
		"github:o/../p@main":     "write github:<owner>/<repo>@<ref>",
	}
	for src, want := range bad {
		_, err := parseEntries(t.TempDir(), []json.RawMessage{json.RawMessage(`{"source":` + jsonString(src) + `}`)})
		if err == nil || err.Error() != "plugin entry 1: source "+src+": "+want {
			t.Errorf("%s: %v, want %q", src, err, want)
		}
	}
}

const srcHome = "github:lazy-rex/LazyRex/plugins/lazyrex@main"

// homeRepo is a repository with the home plugin in plugins/lazyrex and other files beside it.
func (env *cmdEnv) homeRepo() *testRepo {
	env.t.Helper()
	r := newTestRepo(env.t)
	env.repos["lazy-rex/LazyRex"] = r
	r.commit(map[string]string{
		"README.md":                        "the hub\n",
		"plugins/lazyrex/lazyrex.json":     manifestJSON("lazyrex"),
		"plugins/lazyrex/main.lua":         "return 1\n",
		"plugins/other/lazyrex.json":       manifestJSON("other"),
		"plugins/other/main.lua":           "return 2\n",
		"lib/shared.lua":                   "return 3\n",
		"plugins/lazyrex/tests/stub/x.lua": "",
		"plugins/lazyrex/bin/helper.sh*":   "#!/bin/sh\n",
	})
	return r
}

// The install of a subdir source: the lock has the subdir and a whole-commit dir; lockedDir gives
// <entry>/<subdir>; the digest covers the whole entry.
func TestSubdirSync(t *testing.T) {
	env := newCmdEnv(t)
	r := env.homeRepo()
	env.managed("lazyrex=" + srcHome)
	cfg := env.config()
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	le := env.lock().Plugins[srcHome]
	head := r.git("rev-parse", "HEAD")
	if le.Name != "lazyrex" || le.Subdir != "plugins/lazyrex" || le.Commit != head || !strings.HasPrefix(le.Dir, "github/lazy-rex/LazyRex/"+head+".") {
		t.Fatalf("%+v", le)
	}
	entry := filepath.Join(env.state, "plugins", filepath.FromSlash(le.Dir))
	for _, p := range []string{"README.md", "lib/shared.lua", "plugins/other/main.lua"} {
		if _, err := os.Stat(filepath.Join(entry, filepath.FromSlash(p))); err != nil {
			t.Errorf("the entry lacks %s: %v", p, err)
		}
	}
	e := githubEntry(t, srcHome)
	dir, commit, err := lockedDir(env.state, env.lock(), e)
	if err != nil || commit != head {
		t.Fatalf("lockedDir: %v", err)
	}
	if want, _ := filepath.Abs(filepath.Join(entry, "plugins", "lazyrex")); dir != want {
		t.Fatalf("dir %s, want %s", dir, want)
	}
	// A change outside the subdir changes the entry.
	writeInto(entry, "lib/shared.lua", "changed")
	if _, _, err := lockedDir(env.state, env.lock(), e); err == nil || !strings.Contains(err.Error(), "changed (digest mismatch)") {
		t.Fatalf("a change outside the subdir: %v", err)
	}
}

// One repository at two subdirs: two keys and two cache entries.
func TestSubdirTwoKeys(t *testing.T) {
	env := newCmdEnv(t)
	env.homeRepo()
	other := "github:lazy-rex/LazyRex/plugins/other@main"
	env.managed("lazyrex="+srcHome, "other="+other)
	if err := env.install(env.config()); err != nil {
		t.Fatal(err)
	}
	l := env.lock()
	a, b := l.Plugins[srcHome], l.Plugins[other]
	if a == nil || b == nil || a.Dir == b.Dir || a.Commit != b.Commit || a.Digest != b.Digest || a.Name != "lazyrex" || b.Name != "other" {
		t.Fatalf("%+v %+v", a, b)
	}
}

// A subdir that is missing, a file, or a directory without lazyrex.json is not a plugin directory.
// A main outside the subdir is refused, although the file is in the commit.
func TestSubdirRefused(t *testing.T) {
	cases := map[string]struct {
		subdir string
		files  map[string]string
		want   string
	}{
		"missing":     {"plugins/none", nil, "plugins/none is not a plugin directory at commit "},
		"a file":      {"README.md", nil, "README.md is not a plugin directory at commit "},
		"no manifest": {"lib", nil, "lib is not a plugin directory at commit "},
		"main outside": {"plugins/up", map[string]string{"plugins/up/lazyrex.json": `{"name":"up","main":"../../lib/shared.lua"}`},
			"plugin up: main ../../lib/shared.lua must be a file inside the plugin directory"},
	}
	for name, c := range cases {
		env := newCmdEnv(t)
		r := env.homeRepo()
		if c.files != nil {
			r.commit(c.files)
		}
		src := "github:lazy-rex/LazyRex/" + c.subdir + "@main"
		env.managed("x=" + src)
		err := env.install(env.config())
		if err == nil || !strings.HasPrefix(err.Error(), "install managed:x: failed: ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if strings.HasSuffix(name, "manifest") || name == "missing" || name == "a file" {
			head := r.git("rev-parse", "HEAD")
			if want := "install managed:x: failed: fetch " + src + ": " + c.subdir + " is not a plugin directory at commit " + head + "; the lock and the managed file are unchanged"; err == nil || err.Error() != want {
				t.Errorf("%s: %v, want %q", name, err, want)
			}
		}
		if env.lockBytes() != nil {
			t.Errorf("%s: a lock was written", name)
		}
	}
}

// The subdir in the lock must equal the source's: a lock entry for another subdir does not
// match. For a source without a subdir, a lock entry without the field agrees.
func TestSubdirAgreement(t *testing.T) {
	env := newCmdEnv(t)
	env.homeRepo()
	env.managed("lazyrex=" + srcHome)
	cfg := env.config()
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	l := env.lock()
	l.Plugins[srcHome].Subdir = "plugins/other"
	_, _, err := lockedDir(env.state, l, githubEntry(t, srcHome))
	if err == nil || err.Error() != "the lock entry for "+srcHome+" does not match: subdir; update the plugin in LazyRex: Plugins…" {
		t.Fatalf("got %v", err)
	}
	r := githubEntry(t, "github:lazy-rex/Messaging@main").githubRef()
	if f := goodLockEntry().agree(r); f != "" {
		t.Fatalf("a root source: %q", f)
	}
}

// A ref with @ and # goes through resolve and fetch: the first @ of the source ends the path.
func TestSubdirRefWithAt(t *testing.T) {
	env := newCmdEnv(t)
	r := env.homeRepo()
	r.git("branch", "rel@1#x")
	src := "github:lazy-rex/LazyRex/plugins/lazyrex@rel@1#x"
	env.managed("lazyrex=" + src)
	if err := env.install(env.config()); err != nil {
		t.Fatal(err)
	}
	if le := env.lock().Plugins[src]; le.Ref != "rel@1#x" || le.RefKind != "branch" || le.Subdir != "plugins/lazyrex" {
		t.Fatalf("%+v", le)
	}
}

// A plugin directory that cannot be read gives the cannot-read text, not a changed entry.
func TestSubdirCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory")
	}
	env := newCmdEnv(t)
	env.homeRepo()
	env.managed("lazyrex=" + srcHome)
	cfg := env.config()
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	entry := env.entryDir(srcHome)
	plug := filepath.Join(entry, "plugins")
	os.Chmod(plug, 0o000)
	t.Cleanup(func() { os.Chmod(plug, 0o555) })
	_, _, err := lockedDir(env.state, env.lock(), githubEntry(t, srcHome))
	abs, _ := filepath.Abs(entry)
	if err == nil || !strings.HasPrefix(err.Error(), "plugin lazyrex: cannot read the cache entry "+abs+": ") || strings.Contains(err.Error(), "sync") {
		t.Fatalf("got %v", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("permission denied")) {
		t.Fatalf("got %v", err)
	}
}
