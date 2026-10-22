package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// labTestRoot is a short directory under /tmp: a lab socket path must stay under the Unix limit.
func labTestRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "lrtclean-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeLab(root) })
	return root
}

// fakeLab makes root/name with a lab.json and the given age.
func fakeLab(t *testing.T, root, name string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{}`), 0o600)
	at := time.Now().Add(-age)
	os.Chtimes(filepath.Join(dir, "lab.json"), at, at)
	os.Chtimes(dir, at, at)
	return dir
}

// readOnlyCache adds a read-only plugin cache entry, as a lab's hub writes one.
func readOnlyCache(t *testing.T, dir string) {
	t.Helper()
	entry := filepath.Join(dir, "hub", "plugins", "github", "lab", "gh", "c0ffee.sha256-x")
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(entry, "main.lua"), []byte("x"), 0o400)
	os.Chmod(entry, 0o500)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// A lab with a read-only plugin cache entry: os.RemoveAll fails on it, removeLab removes it.
func TestRemoveLabReadOnlyCache(t *testing.T) {
	root := labTestRoot(t)
	plain := fakeLab(t, root, "lrt-1", 0)
	readOnlyCache(t, plain)
	if err := os.RemoveAll(plain); err == nil {
		t.Fatal("os.RemoveAll removed a read-only cache entry: the test does not show the defect")
	}
	dir := fakeLab(t, root, "lrt-2", 0)
	readOnlyCache(t, dir)
	if err := removeLab(dir); err != nil || exists(dir) {
		t.Errorf("removeLab: %v, exists %v", err, exists(dir))
	}
}

// The sweep removes only an old lab of this user without a KEEP marker and without a live server.
// It keeps a marked lab, a new lab, a live lab, other names, a file and a symbolic link, and it
// runs at most once per hour.
func TestSweepLabs(t *testing.T) {
	root := labTestRoot(t)
	old := fakeLab(t, root, "lrt-100", 30*time.Hour)
	readOnlyCache(t, old)
	os.Chtimes(old, time.Now().Add(-30*time.Hour), time.Now().Add(-30*time.Hour)) // the cache entry made the directory new
	kept := fakeLab(t, root, "lrt-101", 30*time.Hour)
	markLabKept(kept, "test", "evidence")
	os.Chtimes(kept, time.Now().Add(-30*time.Hour), time.Now().Add(-30*time.Hour))
	young := fakeLab(t, root, "lrt-102", 10*time.Minute)
	live := fakeLab(t, root, "lrt-103", 30*time.Hour)
	ln, err := net.Listen("unix", filepath.Join(live, "rex.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	at := time.Now().Add(-30 * time.Hour)
	os.Chtimes(live, at, at)
	other := fakeLab(t, root, "lrt-x1", 30*time.Hour)
	os.WriteFile(filepath.Join(root, "lrt-104"), nil, 0o600)
	target := fakeLab(t, root, "target", 30*time.Hour)
	os.Symlink(target, filepath.Join(root, "lrt-105"))

	var lines []string
	sweepLabs(root, time.Now(), func(s string) { lines = append(lines, s) })
	if exists(old) {
		t.Errorf("the old unmarked lab stays; log %v", lines)
	}
	for _, p := range []string{kept, young, live, other, filepath.Join(root, "lrt-104"), filepath.Join(root, "lrt-105"), target} {
		if !exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "removed "+old) {
		t.Errorf("log %v", lines)
	}
	// Within the hour the sweep does not run again.
	again := fakeLab(t, root, "lrt-106", 30*time.Hour)
	sweepLabs(root, time.Now(), func(string) {})
	if !exists(again) {
		t.Error("the sweep ran twice in one hour")
	}
	sweepLabs(root, time.Now().Add(30*time.Hour), func(string) {})
	if exists(again) {
		t.Error("the sweep did not run after an hour")
	}
}

// -clean removes a kept lab after its review, and refuses other paths and a live lab.
func TestCleanLab(t *testing.T) {
	root := labTestRoot(t)
	kept := fakeLab(t, root, "lrt-200", 0)
	markLabKept(kept, "test", "evidence")
	readOnlyCache(t, kept)
	if err := cleanLab(root, kept); err != nil || exists(kept) {
		t.Errorf("clean of a kept lab: %v", err)
	}
	other := fakeLab(t, root, "notalab", 0)
	if err := cleanLab(root, other); err == nil || !exists(other) {
		t.Errorf("clean of another name: %v", err)
	}
	nested := fakeLab(t, other, "lrt-201", 0)
	if err := cleanLab(root, nested); err == nil || !exists(nested) {
		t.Errorf("clean outside the root: %v", err)
	}
	live := fakeLab(t, root, "lrt-202", 0)
	ln, err := net.Listen("unix", filepath.Join(live, "rex.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := cleanLab(root, live); err == nil || !strings.Contains(err.Error(), "live server") || !exists(live) {
		t.Errorf("clean of a live lab: %v", err)
	}
}

// The KEEP marker says who kept the lab and why.
func TestMarkLabKept(t *testing.T) {
	dir := fakeLab(t, labTestRoot(t), "lrt-300", 0)
	if err := markLabKept(dir, "lazyrex-hub test -keep", "tests/calls.lua: 5 passed, 0 failed, 0 problems"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, labKeepFile))
	var k labKeep
	if err := json.Unmarshal(b, &k); err != nil || k.By != "lazyrex-hub test -keep" || !strings.Contains(k.Why, "calls.lua") || k.At == "" {
		t.Errorf("marker %s: %v", b, err)
	}
}

// The real /tmp is never swept by a test of this package: sweepLabs panics on it.
func init() { sweepForbidden = []string{"/tmp", os.TempDir()} }

func TestSweepRefusesRealTmp(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a sweep of /tmp in a test did not panic")
		}
	}()
	sweepLabs("/tmp", time.Now(), func(string) {})
}
