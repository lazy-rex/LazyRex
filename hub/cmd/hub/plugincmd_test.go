package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hub source names no removed plugin command (the hub lead's ruling B): the plugin manager
// installs and updates, and only "lazyrex-hub plugin new" is left. The check reads every non-test
// Go file of the package, the strings and the comments. A bare "plugin sync" is the peer plugin sync
// of peersync.go, so only "lazyrex-hub plugin sync" is a removed command.
func TestRemovedPluginCommandsNotNamed(t *testing.T) {
	removed := []string{"lazyrex-hub plugin sync", "plugin update", "plugin pull", "plugin export", "plugin status"}
	files, _ := filepath.Glob("*.go")
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			for _, r := range removed {
				if strings.Contains(line, r) {
					t.Errorf("%s:%d names the removed command %q: %.80s", name, i+1, r, strings.TrimSpace(line))
				}
			}
		}
	}
}

// pluginCmdMain has only the new command: a removed command is a usage error (exit 2).
func TestPluginCmdOnlyNew(t *testing.T) {
	for _, cmd := range []string{"sync", "update", "pull", "export", "status"} {
		var out, errOut strings.Builder
		if code := pluginCmdMain([]string{cmd}, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "usage: lazyrex-hub plugin new <name> [-dir <path>]") {
			t.Errorf("plugin %s: exit %d, stdout %q, stderr %q", cmd, code, out.String(), errOut.String())
		}
	}
}

// A GitHub entry of hub.json is refused at the load (the hub lead, lead-c2c3), also when the lock
// and the cache have it. The graph of the plugin manager's ops sees the refusal as the hub's
// resolution does: a managed entry that depends on it is refused.
func TestConfigGithubRefused(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.managed("messaging=" + srcM)
	if err := env.install(env.config()); err != nil {
		t.Fatal(err)
	}
	y := filepath.Join(env.dir, env.local("plugins/y", "y", "messaging"))
	managedFile := env.managed("y=" + y)
	cfg := env.config(srcM)
	want := "plugin " + srcM + " in hub.json: a GitHub plugin goes in the managed file: add it in LazyRex: Plugins…"
	c := env.load(cfg)
	resolve(&c)
	if e := c.entries[0]; e.err == nil || e.err.Error() != want || e.cand != nil {
		t.Errorf("the config entry: %v", e.err)
	}
	env.h = nil
	h := env.opsHub(cfg)
	h.entriesMu.Lock()
	h.entries = env.load(cfg).entries // the config entries of cfg: opsHub keeps its own
	h.entriesMu.Unlock()
	list, err := parseManaged(managedFile)
	if err != nil {
		t.Fatal(err)
	}
	refusals := h.opGraph(opLockFile(h), list, env.lock())
	if len(refusals) != 2 || refusals["managed:y"] == nil || !strings.Contains(refusals["managed:y"].Error(), "messaging") {
		t.Fatalf("the graph refusals: %v", refusals)
	}
	for k, err := range refusals {
		if k != "managed:y" && err.Error() != want {
			t.Errorf("%s: %v, want %q", k, err, want)
		}
	}
}
