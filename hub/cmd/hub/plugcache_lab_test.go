package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cacheFixture is startFixture with its name from the sibling module name.lua: the plugin reads a
// file beside its main in the cache entry. rex do puts the script directory first in package.path.
var cacheFixture = strings.Replace(fmt.Sprintf(startFixture, "gh"), `local name = "gh"`, `local name = require('name')`, 1)

// A hub runs a GitHub plugin from its read-only cache entry (section 12). An update and a reload
// give a candidate with the new commit; the instance continues on its run commit. A stop and a
// start run the new commit.
func TestPluginCacheStartLab(t *testing.T) {
	if !strings.Contains(cacheFixture, "require('name')") {
		t.Fatal("the fixture does not read its sibling module")
	}
	s := newStartLab(t)
	gitTestEnv(t)
	r := newTestRepo(t)
	first := r.commit(map[string]string{"lazyrex.json": `{"name":"gh","main":"main.lua"}`, "main.lua": cacheFixture, "name.lua": "return 'gh'\n"})
	const src = "github:lab/gh@main"
	s.setConfig("plugins", []any{map[string]any{"source": src}})
	opts := pluginCmdOpts{remote: func(owner, repo string) string { return r.url() }}
	if err := pluginSync(context.Background(), s.cfg, opts); err != nil {
		t.Fatal(err)
	}
	cacheDir := func() string {
		t.Helper()
		var c Config
		b, _ := os.ReadFile(s.cfg)
		json.Unmarshal(b, &c)
		lock, err := readLock(lockPath(s.cfg, ""))
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(c.State, "plugins", filepath.FromSlash(lock.Plugins[src].Dir))
	}
	firstDir := cacheDir()
	if fi, err := os.Stat(firstDir); err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("the cache entry is not read-only: %v", err)
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

	s.start()
	old, _ := s.startRun("gh")
	if e := entry(); e["commit"] != first || e["dir"] != firstDir || e["candidate"] != nil || e["state"] != "running" {
		t.Fatalf("the first run: %v", e)
	}

	second := r.commit(map[string]string{"name.lua": "return 'gh' -- " + "second\n"})
	if err := pluginUpdate(context.Background(), s.cfg, nil, opts); err != nil {
		t.Fatal(err)
	}
	secondDir := cacheDir()
	if e := entry(); e["candidate"] != nil {
		t.Fatalf("hub.plugins resolved without a trigger: %v", e)
	}
	s.must("hub.plugins.reload", nil)
	e := entry()
	cand, _ := e["candidate"].(map[string]any)
	if e["commit"] != first || e["dir"] != firstDir || cand["commit"] != second || cand["dir"] != secondDir || len(cand) != 2 {
		t.Fatalf("after the update and the reload: %v", e)
	}
	if st := s.status("gh"); st.InstanceID != old.InstanceID || st.ChildPID != old.ChildPID || !st.Registered {
		t.Fatalf("the reload changed the instance: %+v, old %+v", st, old)
	}

	s.must("hub.admin.stop", map[string]any{"plugin": "gh"})
	s.must("hub.admin.start", map[string]any{"plugin": "gh"})
	now, _ := s.startRun("gh")
	if now.InstanceID == old.InstanceID {
		t.Fatal("the start did not spawn a new instance")
	}
	if e := entry(); e["commit"] != second || e["dir"] != secondDir || e["candidate"] != nil {
		t.Fatalf("after the start: %v", e)
	}
}
