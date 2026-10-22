package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The op core reads the managed file with the same bound as parseManaged (managedFileLimit): an
// oversized file is refused after limit+1 bytes, not read in full (codex-27). The file here is
// sparse: 64 MiB on disk costs nothing, but a full read allocates 64 MiB.
func TestReadManagedBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, err = readManaged(path)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("an oversized file: %v", err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 8<<20 {
		t.Fatalf("the read allocated %d bytes for a file over the %d-byte limit", n, managedFileLimit)
	}
}
