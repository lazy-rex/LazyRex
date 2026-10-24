package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cacheFixture is startFixture with its name from the sibling module name.lua: the plugin reads a
// file beside its main in the cache entry. rex do puts the script directory first in package.path
// (observed on a lab server, Rex 0.0.1792158932-g88fd41).
var cacheFixture = strings.Replace(fmt.Sprintf(startFixture, "gh"), `local name = "gh"`, `local name = require('name')`, 1)

// A hub runs a GitHub plugin from its read-only cache entry (section 12), which the install at the
// hub start wrote. The manager's update of the running plugin writes a new cache entry and
// replaces the instance: the new instance runs the new commit from it. A stop and a start keep it.
func TestPluginCacheStartLab(t *testing.T) {
	if !strings.Contains(cacheFixture, "require('name')") {
		t.Fatal("the fixture does not read its sibling module")
	}
	gitTestEnv(t)
	r := newTestRepo(t)
	first := r.commit(map[string]string{"lazyrex.json": `{"name":"gh","main":"main.lua"}`, "main.lua": cacheFixture, "name.lua": "return 'gh'\n"})
	const src = "github:lab/gh@main"
	s := installLab(t, map[string]*testRepo{"gh": r})
	s.writeManagedFile(`{"plugins":[{"id":"gh","source":"` + src + `"}]}`)
	cacheDir := func() string {
		t.Helper()
		var c Config
		b, _ := os.ReadFile(s.cfg)
		json.Unmarshal(b, &c)
		lock, err := readLock(lockPath(s.cfg, ""))
		if err != nil {
			t.Fatal(err)
		}
		// The canonical state (absolute, symlinks resolved): the path that the hub uses and reports.
		state, err := canonicalState(c.State)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(state, "plugins", filepath.FromSlash(lock.Plugins[src].Dir))
	}
	entry := func() map[string]any {
		t.Helper()
		b, _ := json.Marshal(s.must("hub.plugins", nil)["entries"])
		var list []map[string]any
		json.Unmarshal(b, &list)
		for _, e := range list {
			if e["source"] == src {
				return e
			}
		}
		t.Fatalf("no entry for %s: %s", src, b)
		return nil
	}

	s.start() // the hub installs gh at its start
	old, _ := s.startRun("gh")
	firstDir := cacheDir()
	if fi, err := os.Stat(firstDir); err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("the cache entry is not read-only: %v", err)
	}
	if e := entry(); e["commit"] != first || e["dir"] != firstDir || e["candidate"] != nil || e["state"] != "running" {
		t.Fatalf("the first run: %v", e)
	}

	second := r.commit(map[string]string{"name.lua": "return 'gh' -- " + "second\n"})
	if op := s.opDone(s.opsAgent("alice"), "up-gh", map[string]any{"op": "update", "entry": "managed:gh", "input": map[string]any{"to": "@"}}); op["phase"] != "done" {
		t.Fatalf("update: %v", op)
	}
	secondDir := cacheDir()
	if secondDir == firstDir {
		t.Fatal("the update wrote no new cache entry")
	}
	var mid startStatus
	s.wait("the update replaces gh", func() bool { mid = s.status("gh"); return mid.Registered && mid.InstanceID != old.InstanceID })
	if e := entry(); e["commit"] != second || e["dir"] != secondDir || e["candidate"] != nil {
		t.Fatalf("after the update: %v", e)
	}

	s.must("hub.admin.stop", map[string]any{"plugin": "gh"})
	s.must("hub.admin.start", map[string]any{"plugin": "gh"})
	s.wait("the start spawns a new instance", func() bool { st := s.status("gh"); return st.Registered && st.InstanceID != mid.InstanceID })
	if e := entry(); e["commit"] != second || e["dir"] != secondDir || e["candidate"] != nil {
		t.Fatalf("after the start: %v", e)
	}
}
