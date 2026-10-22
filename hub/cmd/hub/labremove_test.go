package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A failed file's lab is removed without -keep, and the output line says so, because the printed
// log paths are gone (the vanished-lab reports, 2026-10-22). A passed file prints nothing; -keep
// removes nothing. A removal that fails is reported for a failed and for a passed file.
func TestRemoveRunLab(t *testing.T) {
	root := t.TempDir()
	mk := func(name string) string {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(d, "state"), 0o700); err != nil {
			t.Fatal(err)
		}
		return d
	}
	failed := mk("lrt-1")
	if note := removeRunLab(failed, false, true); note != "lab removed: "+failed+" (run with -keep to keep it)" {
		t.Errorf("a failed file: %q", note)
	}
	if _, err := os.Lstat(failed); err == nil {
		t.Error("the failed file's lab stays without -keep")
	}
	passed := mk("lrt-2")
	if note := removeRunLab(passed, false, false); note != "" {
		t.Errorf("a passed file: %q", note)
	}
	if _, err := os.Lstat(passed); err == nil {
		t.Error("the passed file's lab stays")
	}
	kept := mk("lrt-3")
	if note := removeRunLab(kept, true, true); note != "" {
		t.Errorf("-keep: %q", note)
	}
	if _, err := os.Lstat(kept); err != nil {
		t.Error("-keep removed the lab")
	}
	locked := mk("lrt-4")
	os.Chmod(locked, 0o500)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if note := removeRunLab(locked, false, true); !strings.HasPrefix(note, "lab not fully removed: "+locked) {
		t.Errorf("a failed removal of a failed file: %q", note)
	}
	lockedPass := mk("lrt-5")
	os.Chmod(lockedPass, 0o500)
	t.Cleanup(func() { os.Chmod(lockedPass, 0o700) })
	if note := removeRunLab(lockedPass, false, false); !strings.HasPrefix(note, "lab not fully removed: "+lockedPass) {
		t.Errorf("a failed removal of a passed file: %q", note)
	}
}
