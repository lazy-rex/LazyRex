package main

// These tests use local repositories through file:// URLs. They prove the git commands and the
// checks of this file. They do not prove GitHub behavior (for example, a fetch by ID of a commit
// that is not a branch tip).

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitTestEnv isolates git from the user's and the system's git config.
func gitTestEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("the plugin command tests need git")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
}

// testRepo is a work repository that a test fetches from with a file:// URL.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	gitTestEnv(t)
	r := &testRepo{t, t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *testRepo) url() string { return "file://" + r.dir }

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes the files (a path that ends in "*" is executable), commits them, and gives the
// commit ID.
func (r *testRepo) commit(files map[string]string) string {
	r.t.Helper()
	for p, c := range files {
		path, exec := strings.CutSuffix(p, "*")
		full := filepath.Join(r.dir, filepath.FromSlash(path))
		os.MkdirAll(filepath.Dir(full), 0o755)
		mode := os.FileMode(0o644)
		if exec {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(c), mode); err != nil {
			r.t.Fatal(err)
		}
		os.Chmod(full, mode)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "c")
	return r.git("rev-parse", "HEAD")
}

// fetchTest fetches commit of r with the fetch path of the commands, into a new state directory.
func fetchTest(t *testing.T, r *testRepo, fetchRef, commit string, byID bool) (string, *fetchedTree, error) {
	t.Helper()
	state := cacheTemp(t)
	tmp := filepath.Join(state, "plugins", ".tmp", "x")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	ft, err := fetchCommit(context.Background(), state, tmp, r.url(), "lazy-rex", "Messaging", fetchRef, commit, byID)
	return state, ft, err
}

// published lists the published entries of lazy-rex/Messaging in state (stages included).
func published(t *testing.T, state string) []string {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(state, "plugins", "github", "lazy-rex", "Messaging"))
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestCheckRef(t *testing.T) {
	gitTestEnv(t)
	ctx, dir := context.Background(), t.TempDir()
	for _, ok := range []string{"main", "v1.2", "team/x", "refs/heads/main", "refs/tags/v1", testCommit, "abc123"} {
		if err := checkRef(ctx, dir, ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"a..b", "x~1", "x^", "x:y", "a b", "refs/remotes/o/x", "refs/heads/", "x.lock", "-x/..", strings.ToUpper(testCommit) + "/.."} {
		if err := checkRef(ctx, dir, bad); err == nil || err.Error() != bad+" is not a valid ref" {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// ls-remote resolution: exact names only, branch and tag refused, annotated tags peeled.
func TestResolveRef(t *testing.T) {
	r := newTestRepo(t)
	c1 := r.commit(map[string]string{"a": "1"})
	r.git("branch", "team/foo")
	r.git("tag", "light")
	r.git("tag", "-a", "-m", "annotated", "ann")
	c2 := r.commit(map[string]string{"a": "2"})
	r.git("branch", "both")
	r.git("tag", "both")
	ctx, dir := context.Background(), t.TempDir()
	cases := []struct {
		ref, commit, kind, fetch, err string
	}{
		{ref: "main", commit: c2, kind: "branch", fetch: "refs/heads/main"},
		{ref: "refs/heads/main", commit: c2, kind: "branch", fetch: "refs/heads/main"},
		{ref: "light", commit: c1, kind: "tag", fetch: "refs/tags/light"},
		{ref: "ann", commit: c1, kind: "tag", fetch: "refs/tags/ann"},
		{ref: "refs/tags/ann", commit: c1, kind: "tag", fetch: "refs/tags/ann"},
		{ref: "foo", err: "ref foo is not found"},                       // only refs/heads/team/foo exists: a suffix match
		{ref: "refs/tags/main", err: "ref refs/tags/main is not found"}, // a branch, not a tag
		{ref: "both", err: "ref both is both a branch and a tag: write refs/heads/both or refs/tags/both"},
		{ref: "refs/heads/both", commit: c2, kind: "branch", fetch: "refs/heads/both"},
		{ref: "refs/tags/both", commit: c2, kind: "tag", fetch: "refs/tags/both"},
		{ref: c1, commit: c1, kind: "commit", fetch: c1},
	}
	for _, c := range cases {
		got, err := resolveRef(ctx, dir, r.url(), c.ref)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("%s: %v, want %q", c.ref, err, c.err)
			}
			continue
		}
		if err != nil || got != (resolved{c.commit, c.kind, c.fetch}) {
			t.Errorf("%s: %+v %v", c.ref, got, err)
		}
	}
}

// A fetch publishes the commit's tree: modes from the tree, the digest of the content, and the
// manifest. An executable helper stays executable (0555) and the disk digest equals it.
func TestFetchPublishes(t *testing.T) {
	r := newTestRepo(t)
	c := r.commit(map[string]string{
		"lazyrex.json": `{"name":"messaging","main":"m.lua"}`, "m.lua": "return 1\n",
		"lib/x.lua": "return 2\n", "bin/helper*": "#!/bin/sh\n",
	})
	state, ft, err := fetchTest(t, r, "refs/heads/main", c, false)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(state, "plugins", filepath.FromSlash(ft.Dir))
	if ft.Manifest.Name != "messaging" || !strings.HasPrefix(ft.Dir, "github/lazy-rex/Messaging/"+c+".") {
		t.Fatalf("%+v", ft)
	}
	want := treeDigest(map[string]string{"lazyrex.json": `{"name":"messaging","main":"m.lua"}`, "m.lua": "return 1\n",
		"lib/x.lua": "return 2\n", "bin/helper*": "#!/bin/sh\n"})
	if d, err := dirDigest(root); err != nil || d != want || ft.Digest != want {
		t.Fatalf("digest: fetch %s disk %s %v, want %s", ft.Digest, d, err, want)
	}
	if fi, _ := os.Stat(filepath.Join(root, "bin", "helper")); fi.Mode().Perm() != 0o555 {
		t.Fatalf("helper mode %o", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(root); fi.Mode().Perm() != 0o555 {
		t.Fatalf("root mode %o", fi.Mode().Perm())
	}
	// A second fetch of the same commit gives the same digest and another dir.
	_, ft2, err := fetchTest(t, r, c, c, true)
	if err != nil || ft2.Digest != ft.Digest || ft2.Dir == ft.Dir {
		t.Fatalf("second fetch: %+v %v", ft2, err)
	}
}

// An annotated tag is peeled through the real fetch path: FETCH_HEAD is the tag object, and
// FETCH_HEAD^{commit} is the commit.
func TestFetchAnnotatedTag(t *testing.T) {
	r := newTestRepo(t)
	c := r.commit(map[string]string{"lazyrex.json": `{"name":"messaging","main":"m.lua"}`, "m.lua": ""})
	r.git("tag", "-a", "-m", "v1", "v1")
	res, err := resolveRef(context.Background(), t.TempDir(), r.url(), "v1")
	if err != nil || res.Commit != c {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	if _, _, err := fetchTest(t, r, res.FetchRef, res.Commit, false); err != nil {
		t.Fatal(err)
	}
}

// A ref that moves between the resolve and the fetch is refused, and nothing is published.
func TestFetchMoved(t *testing.T) {
	r := newTestRepo(t)
	old := r.commit(map[string]string{"lazyrex.json": `{"name":"messaging","main":"m.lua"}`, "m.lua": ""})
	r.commit(map[string]string{"m.lua": "moved"})
	state, _, err := fetchTest(t, r, "refs/heads/main", old, false)
	if err == nil || err.Error() != "ref refs/heads/main moved during the fetch; run the command again" {
		t.Fatalf("got %v", err)
	}
	if p := published(t, state); len(p) != 0 {
		t.Fatalf("published: %v", p)
	}
}

// Commit IDs: a commit that is not a branch tip, a blob ID, and an unknown ID.
func TestFetchByID(t *testing.T) {
	r := newTestRepo(t)
	c1 := r.commit(map[string]string{"lazyrex.json": `{"name":"messaging","main":"m.lua"}`, "m.lua": "1"})
	r.commit(map[string]string{"m.lua": "2"})
	if _, ft, err := fetchTest(t, r, c1, c1, true); err != nil || ft == nil {
		t.Fatalf("a commit that is not a tip: %v", err)
	}
	blob := r.git("rev-parse", "HEAD:m.lua")
	if _, _, err := fetchTest(t, r, blob, blob, true); err == nil || err.Error() != blob+" is a blob, not a commit" {
		t.Errorf("blob: %v", err)
	}
	unknown := strings.Repeat("ab", 20)
	_, _, err := fetchTest(t, r, unknown, unknown, true)
	if _, ok := err.(*errUnavailable); !ok || !strings.HasPrefix(err.Error(), "commit "+unknown+" is not available: ") {
		t.Errorf("unknown: %v", err)
	}
}

// Trees with a symlink, a submodule or a missing main are refused before publication.
func TestFetchRefusedTrees(t *testing.T) {
	manifest := map[string]string{"lazyrex.json": `{"name":"messaging","main":"m.lua"}`, "m.lua": ""}
	cases := map[string]struct {
		edit func(r *testRepo)
		want string
	}{
		"symlink": {func(r *testRepo) {
			os.Symlink("m.lua", filepath.Join(r.dir, "link.lua"))
		}, "plugin trees with symlinks are not supported: link.lua"},
		"submodule": {func(r *testRepo) {
			os.MkdirAll(filepath.Join(r.dir, "vendor", "sub"), 0o755) // an unpopulated submodule: add -A keeps the gitlink
			r.git("update-index", "--add", "--cacheinfo", "160000,"+testCommit+",vendor/sub")
		}, "submodules are not supported: vendor/sub"},
		"missing main": {func(r *testRepo) {
			os.Remove(filepath.Join(r.dir, "m.lua"))
		}, "plugin messaging: main m.lua does not exist in the tree of "},
		"no manifest": {func(r *testRepo) {
			os.Remove(filepath.Join(r.dir, "lazyrex.json"))
		}, "no lazyrex.json in"},
		"export-ignore": {func(r *testRepo) {
			os.WriteFile(filepath.Join(r.dir, ".gitattributes"), []byte("secret.txt export-ignore\n"), 0o644)
			os.WriteFile(filepath.Join(r.dir, "secret.txt"), []byte("s"), 0o644)
		}, "secret.txt: the file is not in the archive"},
		"export-subst": {func(r *testRepo) {
			os.WriteFile(filepath.Join(r.dir, ".gitattributes"), []byte("v.txt export-subst\n"), 0o644)
			os.WriteFile(filepath.Join(r.dir, "v.txt"), []byte("$Format:%H$\n"), 0o644)
		}, "v.txt: the content differs from the blob"},
	}
	for name, c := range cases {
		r := newTestRepo(t)
		r.commit(manifest)
		c.edit(r)
		r.git("add", "-A")
		r.git("commit", "-q", "-m", name)
		commit := r.git("rev-parse", "HEAD")
		state, _, err := fetchTest(t, r, commit, commit, true)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if p := published(t, state); len(p) != 0 {
			t.Errorf("%s: left %v", name, p)
		}
	}
}

// extractTar refuses an archive that differs from the tree. git does not make such archives, so
// the test writes them.
func TestExtractTarMismatch(t *testing.T) {
	oid := func(c string) string {
		h := sha1.New()
		fmt.Fprintf(h, "blob %d\x00%s", len(c), c)
		return hex.EncodeToString(h.Sum(nil))
	}
	tree := map[string]treeFile{"a.lua": {OID: oid("a")}, "d/b.lua": {OID: oid("b")}}
	type ent struct {
		name, body string
		typ        byte
	}
	good := []ent{{"pax_global_header", "", tar.TypeXGlobalHeader}, {"d/", "", tar.TypeDir}, {"a.lua", "a", tar.TypeReg}, {"d/b.lua", "b", tar.TypeReg}}
	cases := map[string]struct {
		ents []ent
		want string
	}{
		"good":           {good, ""},
		"duplicate":      {append(append([]ent{}, good...), ent{"a.lua", "a", tar.TypeReg}), "a.lua: the file appears twice"},
		"unexpected":     {append(append([]ent{}, good...), ent{"c.lua", "c", tar.TypeReg}), "c.lua: the file is not in the tree"},
		"omitted":        {good[:3], "d/b.lua: the file is not in the archive"},
		"absolute":       {append(append([]ent{}, good...), ent{"/etc/x", "", tar.TypeReg}), "the path is not allowed"},
		"dotdot":         {append(append([]ent{}, good...), ent{"d/../a.lua", "a", tar.TypeReg}), "the path is not allowed"},
		"dot":            {append(append([]ent{}, good...), ent{"./a.lua", "a", tar.TypeReg}), "the path is not allowed"},
		"double slash":   {append(append([]ent{}, good...), ent{"d//b.lua", "b", tar.TypeReg}), "the path is not allowed"},
		"other dir":      {append(append([]ent{}, good...), ent{"e/", "", tar.TypeDir}), "e/: an unexpected directory"},
		"symlink":        {append(append([]ent{}, good...), ent{"l", "", tar.TypeSymlink}), `l: entry type '2' is not allowed`},
		"second global":  {append(append([]ent{}, good...), ent{"g", "", tar.TypeXGlobalHeader}), "a second global header"},
		"content differ": {[]ent{good[0], good[1], {"a.lua", "A", tar.TypeReg}, good[3]}, "a.lua: the content differs from the blob"},
	}
	for name, c := range cases {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, e := range c.ents {
			h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
			if e.typ == tar.TypeXGlobalHeader {
				h = &tar.Header{Typeflag: e.typ, PAXRecords: map[string]string{"comment": testCommit}}
			}
			if e.typ == tar.TypeSymlink {
				h.Linkname, h.Size = "a.lua", 0
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(name, err)
			}
			tw.Write([]byte(e.body))
		}
		tw.Close()
		dir := t.TempDir()
		_, err := extractTar(&buf, testCommit, tree, dir)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}
