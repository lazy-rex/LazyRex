package main

import (
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cacheTemp is a temporary state directory. Published entries are 0555, so the cleanup makes the
// tree writable before t.TempDir removes it.
func cacheTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { removeTree(dir) })
	return dir
}

// testTree is a plugin tree: path to content. A path that ends in "*" is an executable file.
var testTree = map[string]string{
	"lazyrex.json":       `{"name":"messaging","main":"messaging.lua"}`,
	"messaging.lua":      "return 1\n",
	"lib/helper.lua":     "return 2\n",
	"bin/run.sh*":        "#!/bin/sh\necho hi\n",
	"lib/deep/inner.txt": "",
}

// treeDigest is the digest of a tree, computed from its contents (as the fetch computes it from
// the archive).
func treeDigest(tree map[string]string) string {
	var files []digestFile
	for p, c := range tree {
		path, exec := strings.CutSuffix(p, "*")
		files = append(files, digestFile{Path: path, Exec: exec, Size: int64(len(c)), Sum: sha256.Sum256([]byte(c))})
	}
	return digestOf(files)
}

// publishTree stages tree for lazy-rex/Messaging at commit and publishes it. It returns the lock
// field dir.
func publishTree(t *testing.T, state, commit string, tree map[string]string) string {
	t.Helper()
	s, err := newStage(state, "lazy-rex", "Messaging", commit)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range tree {
		path, exec := strings.CutSuffix(p, "*")
		full := filepath.Join(s.Dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o600)
		if exec {
			mode = 0o700
		}
		if err := os.WriteFile(full, []byte(c), mode); err != nil {
			t.Fatal(err)
		}
	}
	rel, err := s.publish()
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// lockFor gives a lock and a config entry for github:lazy-rex/Messaging@main at a published dir.
func lockFor(commit, rel, digest string) (*pluginLock, *configEntry) {
	src := "github:lazy-rex/Messaging@main"
	le := &lockEntry{lockPin: lockPin{Name: "messaging", Owner: "lazy-rex", Repo: "Messaging", Ref: "main", RefKind: "branch",
		Commit: commit, Digest: digest}, Dir: rel}
	return &pluginLock{Version: lockVersion, Plugins: map[string]*lockEntry{src: le}}, &configEntry{Source: src, github: true, owner: "lazy-rex", repo: "Messaging", gitRef: "main"}
}

// A published entry has the published modes (section 4): files 0444, executable files 0555,
// directories 0555, the root too. No stage is left.
func TestPublishModes(t *testing.T) {
	state := cacheTemp(t)
	rel := publishTree(t, state, testCommit, testTree)
	root := filepath.Join(state, "plugins", filepath.FromSlash(rel))
	want := map[string]os.FileMode{
		".": 0o555, "lazyrex.json": 0o444, "messaging.lua": 0o444, "lib": 0o555, "lib/helper.lua": 0o444,
		"lib/deep": 0o555, "lib/deep/inner.txt": 0o444, "bin": 0o555, "bin/run.sh": 0o555,
	}
	got := map[string]os.FileMode{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		fi, _ := d.Info()
		r, _ := filepath.Rel(root, p)
		got[filepath.ToSlash(r)] = fi.Mode().Perm()
		return nil
	})
	if len(got) != len(want) {
		t.Fatalf("paths: %v", got)
	}
	for p, m := range want {
		if got[p] != m {
			t.Errorf("%s: mode %o, want %o", p, got[p], m)
		}
	}
	stages, _ := filepath.Glob(filepath.Join(state, "plugins", "github", "lazy-rex", "Messaging", ".stage-*"))
	if len(stages) != 0 {
		t.Fatalf("stages left: %v", stages)
	}
	if !strings.HasPrefix(rel, "github/lazy-rex/Messaging/"+testCommit+".") || !cacheToken.MatchString(rel[len(rel)-16:]) {
		t.Fatalf("dir %s", rel)
	}
}

// The disk digest of a published entry equals the digest of its contents; two publishes of one
// tree give one digest and two dirs.
func TestDigestAfterPublish(t *testing.T) {
	state := cacheTemp(t)
	rel1 := publishTree(t, state, testCommit, testTree)
	rel2 := publishTree(t, state, testCommit, testTree)
	if rel1 == rel2 {
		t.Fatal("two publishes gave one dir")
	}
	want := treeDigest(testTree)
	for _, rel := range []string{rel1, rel2} {
		d, err := dirDigest(filepath.Join(state, "plugins", filepath.FromSlash(rel)))
		if err != nil || d != want {
			t.Fatalf("%s: %s %v, want %s", rel, d, err, want)
		}
	}
	// The x flag and the path are in the digest.
	other := map[string]string{}
	for p, c := range testTree {
		other[strings.TrimSuffix(p, "*")] = c
	}
	if treeDigest(other) == want {
		t.Fatal("the execute flag does not change the digest")
	}
}

// A chmod failure during publish publishes nothing and removes the stage.
func TestPublishChmodFailure(t *testing.T) {
	state := cacheTemp(t)
	old := cacheChmod
	t.Cleanup(func() { cacheChmod = old })
	calls := 0
	cacheChmod = func(p string, m os.FileMode) error {
		calls++
		if calls == 3 {
			return errors.New("injected chmod failure")
		}
		return os.Chmod(p, m)
	}
	s, err := newStage(state, "lazy-rex", "Messaging", testCommit)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a/b/c.lua", "lazyrex.json"} {
		os.MkdirAll(filepath.Dir(filepath.Join(s.Dir, p)), 0o700)
		os.WriteFile(filepath.Join(s.Dir, p), []byte("x"), 0o600)
	}
	if _, err := s.publish(); err == nil || err.Error() != "injected chmod failure" {
		t.Fatalf("publish: %v", err)
	}
	left, _ := os.ReadDir(filepath.Join(state, "plugins", "github", "lazy-rex", "Messaging"))
	if len(left) != 0 {
		t.Fatalf("left after a failed publish: %v", left)
	}
}

// lockedDir accepts an agreeing lock entry and an unchanged cache entry, and gives the texts of
// sections 3 and 8 otherwise. It never writes.
func TestLockedDir(t *testing.T) {
	state := cacheTemp(t)
	digest := treeDigest(testTree)
	rel := publishTree(t, state, testCommit, testTree)
	install := "the hub installs it at its next start or reload"
	again := "the hub installs the locked commit again at its next start or reload"
	update := "update the plugin in LazyRex: Plugins…"

	lock, e := lockFor(testCommit, rel, digest)
	dir, commit, err := lockedDir(state, lock, e)
	want, _ := filepath.Abs(filepath.Join(state, "plugins", rel))
	if err != nil || dir != want || commit != testCommit {
		t.Fatalf("got %s %s %v", dir, commit, err)
	}

	// Not installed: no lock entry.
	other := &configEntry{Source: "github:lazy-rex/Profiles@main", github: true}
	if _, _, err := lockedDir(state, lock, other); err == nil ||
		err.Error() != "plugin github:lazy-rex/Profiles@main is not installed: "+install {
		t.Errorf("not installed: %v", err)
	}
	// An agreement mismatch.
	lock, e = lockFor(testCommit, rel, digest)
	lock.Plugins[e.Source].Owner = "x"
	if _, _, err := lockedDir(state, lock, e); err == nil ||
		err.Error() != "the lock entry for github:lazy-rex/Messaging@main does not match: owner; "+update {
		t.Errorf("owner: %v", err)
	}
	// A manifest name that differs from the lock name.
	lock, e = lockFor(testCommit, rel, digest)
	lock.Plugins[e.Source].Name = "profiles"
	if _, _, err := lockedDir(state, lock, e); err == nil ||
		err.Error() != "the lock entry for github:lazy-rex/Messaging@main does not match: name; "+update {
		t.Errorf("name: %v", err)
	}
	// A digest that differs.
	lock, e = lockFor(testCommit, rel, testDigest)
	changed := "plugin messaging: the cache entry for commit " + testCommit + " changed (digest mismatch): " + again
	if _, _, err := lockedDir(state, lock, e); err == nil || err.Error() != changed {
		t.Errorf("digest: %v", err)
	}
	// No cache entry.
	lock, e = lockFor(testCommit, "github/lazy-rex/Messaging/"+testCommit+".0000000000000000", digest)
	if _, _, err := lockedDir(state, lock, e); err == nil ||
		err.Error() != "plugin messaging: the cache has no entry for commit "+testCommit+": "+again {
		t.Errorf("no entry: %v", err)
	}
}

// Each change of a published entry gives the digest mismatch text.
func TestLockedDirChanged(t *testing.T) {
	digest := treeDigest(testTree)
	edits := map[string]func(root string) error{
		"content": func(root string) error {
			return writeInto(root, "messaging.lua", "return 3\n")
		},
		"added file": func(root string) error {
			return writeInto(root, "lib/new.lua", "")
		},
		"removed file": func(root string) error {
			os.Chmod(filepath.Join(root, "lib"), 0o755)
			return os.Remove(filepath.Join(root, "lib", "helper.lua"))
		},
		"chmod +x": func(root string) error {
			return os.Chmod(filepath.Join(root, "messaging.lua"), 0o555)
		},
		"symlink inside": func(root string) error {
			os.Chmod(filepath.Join(root, "lib"), 0o755)
			return os.Symlink("../messaging.lua", filepath.Join(root, "lib", "link.lua"))
		},
		"symlink replaces a file": func(root string) error {
			os.Chmod(filepath.Join(root, "lib"), 0o755)
			os.Remove(filepath.Join(root, "lib", "helper.lua"))
			return os.Symlink("../messaging.lua", filepath.Join(root, "lib", "helper.lua"))
		},
	}
	for name, edit := range edits {
		state := cacheTemp(t)
		rel := publishTree(t, state, testCommit, testTree)
		root := filepath.Join(state, "plugins", filepath.FromSlash(rel))
		if err := edit(root); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		lock, e := lockFor(testCommit, rel, digest)
		_, _, err := lockedDir(state, lock, e)
		if err == nil || !strings.Contains(err.Error(), "changed (digest mismatch)") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The entry itself must be a directory: a symlink to a copy with the right digest is refused.
func TestLockedDirEntrySymlink(t *testing.T) {
	state := cacheTemp(t)
	digest := treeDigest(testTree)
	rel := publishTree(t, state, testCommit, testTree)
	link := "github/lazy-rex/Messaging/" + testCommit + ".1111111111111111"
	repoDir := filepath.Join(state, "plugins", "github", "lazy-rex", "Messaging")
	if err := os.Symlink(filepath.Base(rel), filepath.Join(repoDir, testCommit+".1111111111111111")); err != nil {
		t.Fatal(err)
	}
	lock, e := lockFor(testCommit, link, digest)
	if _, _, err := lockedDir(state, lock, e); err == nil || !strings.Contains(err.Error(), "changed (digest mismatch)") {
		t.Fatalf("got %v", err)
	}
}

// writeInto writes a file into a published entry, after it makes the parent writable (the test
// changes the entry on purpose).
func writeInto(root, rel, content string) error {
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.Chmod(filepath.Dir(p), 0o755)
	os.Chmod(p, 0o644)
	return os.WriteFile(p, []byte(content), 0o644)
}
