package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A failed file's lab is removed without -keep, and the output line says so, because the printed
// log paths are gone (the vanished-lab reports, 2026-10-22). A passed file prints nothing; -keep
// removes nothing. A removal that fails is a problem (endLab) for a failed and for a passed file,
// and then no "lab removed" line.
func TestRemoveRunLab(t *testing.T) {
	root := t.TempDir()
	mk := func(name string) string {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(d, "state"), 0o700); err != nil {
			t.Fatal(err)
		}
		return d
	}
	end := func(dir string, keep, failed bool) (string, []string) {
		kept, probs := endLab(dir, keep, "f.lua", nil)
		return removedNote(dir, kept, len(probs) == 0, failed), probs
	}
	failed := mk("lrt-1")
	if note, _ := end(failed, false, true); note != "lab removed: "+failed+" (run with -keep to keep it)" {
		t.Errorf("a failed file: %q", note)
	}
	if _, err := os.Lstat(failed); err == nil {
		t.Error("the failed file's lab stays without -keep")
	}
	passed := mk("lrt-2")
	if note, _ := end(passed, false, false); note != "" {
		t.Errorf("a passed file: %q", note)
	}
	if _, err := os.Lstat(passed); err == nil {
		t.Error("the passed file's lab stays")
	}
	kept := mk("lrt-3")
	if note, _ := end(kept, true, true); note != "" {
		t.Errorf("-keep: %q", note)
	}
	if _, err := os.Lstat(kept); err != nil {
		t.Error("-keep removed the lab")
	}
	// A read-only parent: removeLab makes the lab's own tree writable, not its parent.
	for i, failedFile := range []bool{true, false} {
		parent := filepath.Join(root, fmt.Sprintf("ro-%d", i))
		locked := filepath.Join(parent, "lrt-4")
		if err := os.MkdirAll(filepath.Join(locked, "state"), 0o700); err != nil {
			t.Fatal(err)
		}
		os.Chmod(parent, 0o500)
		t.Cleanup(func() { os.Chmod(parent, 0o700) })
		note, probs := end(locked, false, failedFile)
		if note != "" || len(probs) == 0 {
			t.Errorf("a failed removal (failed file %v): note %q, problems %v", failedFile, note, probs)
		}
	}
}
