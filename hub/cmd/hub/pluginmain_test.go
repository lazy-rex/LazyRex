package main

import (
	"path/filepath"
	"testing"
)

// The hub accepts the config field lock (section 1); lockPath resolves it against the config.
func TestConfigLockField(t *testing.T) {
	dir := t.TempDir()
	c, err := writeConfig(t, dir, `[],"lock":"locks/hub.lock.json"`)
	if err != nil || c.Lock != "locks/hub.lock.json" || lockPath(c.path, c.Lock) != filepath.Join(dir, "locks", "hub.lock.json") {
		t.Fatalf("got %q %v", c.Lock, err)
	}
	c, err = writeConfig(t, dir, `[]`)
	if err != nil || c.Lock != "" || lockPath(c.path, c.Lock) != filepath.Join(dir, "hub.lock.json") {
		t.Fatalf("default: %q %v", c.Lock, err)
	}
}
