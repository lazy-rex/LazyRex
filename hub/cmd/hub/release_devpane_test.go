package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseDevPaneCommand: the dev pane runs /bin/sh -c for every spec (coordinator,
// h-freeze-and-shell; a host without zsh failed every lab). The test runs the real command with a
// stand-in dev.sh that reports its environment and arguments into the dev log.
func TestReleaseDevPaneCommand(t *testing.T) {
	for _, real := range []bool{false, true} {
		dir := t.TempDir()
		devsh := filepath.Join(dir, "dev sh's dir", "dev.sh")
		os.MkdirAll(filepath.Dir(devsh), 0o700)
		os.WriteFile(devsh, []byte("#!/bin/sh\necho \"args=$*\"\necho \"path=$PATH\"\necho \"role=${LAZYREX_FETCH_ROLE-unset}\"\necho \"real=${DEV_REAL-unset}\"\n"), 0o700)
		gobin := filepath.Join(dir, "go bin")
		log := filepath.Join(dir, "dev.log")
		r := &releaser{spec: &relSpec{Real: real, DevLog: log}, cfg: &pluginCmdConfig{path: filepath.Join(dir, "hub's config.json")},
			env: []string{"PATH=/usr/bin:/bin", "LAZYREX_FETCH_ROLE=fetch"}}
		argv := r.devPaneCommand(gobin, devsh)
		if len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" {
			t.Fatalf("argv %q", argv)
		}
		c := exec.Command(argv[0], argv[1:]...)
		c.Env = append(os.Environ(), "LAZYREX_FETCH_ROLE=fetch") // as if the server's env had it
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("the command: %v\n%s", err, out)
		}
		got := string(mustReadFile(log))
		wantReal := "real=unset"
		if real {
			wantReal = "real=1"
		}
		for _, want := range []string{"args=" + r.cfg.path + "\n", "path=" + gobin + ":/usr/bin:/bin\n", "role=unset\n", wantReal + "\n"} {
			if !strings.Contains(got, want) {
				t.Errorf("real %v: the dev log has no %q:\n%s", real, want, got)
			}
		}
	}
}
