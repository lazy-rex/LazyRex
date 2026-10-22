package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A removal that fails is a problem of the file (endLab), for a passed file too (49893aa); the lab
// is then not reported as kept. -keep removes nothing. A failed file keeps its lab
// (TestEndLabFailedFile), so its removal cannot fail.
func TestEndLabRemovalFails(t *testing.T) {
	root := t.TempDir()
	// A read-only parent: removeLab makes the lab's own tree writable, not its parent.
	parent := filepath.Join(root, "ro")
	locked := filepath.Join(parent, "lrt-1")
	if err := os.MkdirAll(filepath.Join(locked, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(parent, 0o500)
	t.Cleanup(func() { os.Chmod(parent, 0o700) })
	if kept, probs := endLab(locked, false, false, "f.lua", nil); kept || len(probs) == 0 {
		t.Errorf("a failed removal of a passed file: kept %v, problems %v", kept, probs)
	}
	keep := filepath.Join(root, fmt.Sprintf("lrt-%d", 2))
	os.MkdirAll(filepath.Join(keep, "state"), 0o700)
	if kept, probs := endLab(keep, true, false, "f.lua", nil); !kept || len(probs) != 0 {
		t.Errorf("-keep: kept %v, problems %v", kept, probs)
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Error("-keep removed the lab")
	}
}
