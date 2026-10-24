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
