package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sizedManifest writes dir/lazyrex.json of exactly size bytes (a padded notice) and dir/p.lua.
func sizedManifest(t *testing.T, size int) string {
	dir := t.TempDir()
	head, tail := `{"name":"p","main":"p.lua","notice":"`, `"}`
	body := head + strings.Repeat(" ", size-len(head)-len(tail)) + tail
	if len(body) != size {
		t.Fatalf("manifest is %d bytes, want %d", len(body), size)
	}
	os.WriteFile(filepath.Join(dir, "lazyrex.json"), []byte(body), 0o644)
	os.WriteFile(filepath.Join(dir, "p.lua"), nil, 0o644)
	return dir
}

// Decision "the hub refuses plugin manifests over 64 KiB by bounding the read": both loaders (the
// hub's and the harness's) accept 65536 bytes and refuse 65537.
func TestManifestBound(t *testing.T) {
	loaders := map[string]func(string) error{
		"hub":     func(d string) error { _, _, err := readPluginManifest(d); return err },
		"harness": func(d string) error { _, err := loadManifest(d); return err },
	}
	for name, load := range loaders {
		if err := load(sizedManifest(t, maxManifestBytes)); err != nil {
			t.Errorf("%s: %d bytes: %v", name, maxManifestBytes, err)
		}
		dir := sizedManifest(t, maxManifestBytes+1)
		want := fmt.Sprintf("%s is %d bytes, over the 65536-byte limit", filepath.Join(dir, "lazyrex.json"), maxManifestBytes+1)
		if err := load(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %d bytes: got %v, want %q", name, maxManifestBytes+1, err, want)
		}
	}
}
