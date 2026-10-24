package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lazyrex-hub stop and status with the real binary, a restart wrapper and its own lab server
// (docs/hub.md, "Stop and status"). Lab only: LAZYREX_LAB=1.
func TestStopStatusLab(t *testing.T) {
	needLab(t)
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	lab, err := os.MkdirTemp("/tmp", "rexlab-ctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	socket := "unix://" + lab + "/rex.sock"
	server := startLabServer(t, root, lab)
	config := func(name, state string) string {
		p := filepath.Join(tmp, name)
		b, _ := json.Marshal(map[string]any{"server": socket, "state": state,
			"agents": []map[string]any{{"id": "user", "name": "User", "status_record": ""}}})
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	state := filepath.Join(tmp, "state")
	cfg := config("hub.json", state)
	cfg2 := config("hub2.json", filepath.Join(tmp, "state2"))
	ctl := func(cmd, cfg string) (int, string) {
		t.Helper()
		out, err := exec.Command(bin, cmd, "-config", cfg).CombinedOutput()
		return exitCodeOf(err), string(out)
	}
	expect := func(what string, code int, out string, wantCode int, want ...string) {
		t.Helper()
		if code != wantCode {
			t.Errorf("%s: exit %d, want %d\n%s", what, code, wantCode, out)
		}
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: no %q in\n%s", what, w, out)
			}
		}
	}

	code, out := ctl("status", cfg)
	expect("status before a start", code, out, ctlNotRunning, "not running")
	code, out = ctl("stop", cfg)
	expect("stop before a start", code, out, 0, "not running")

	wlog := filepath.Join(tmp, "wrapper.log")
	wrapper, exit := startRestartWrapper(t, bin, cfg, wlog)
	waitLogLine(t, wlog, " ready\n")
	code, out = ctl("status", cfg)
	expect("status, running", code, out, ctlRunning, "running: hub pid ", "restart wrapper pid ")

	// A hub of another state on the same server: each config sees its own hub, and the other one.
	hub2, exit2 := startShutdownHub(t, bin, cfg2, filepath.Join(tmp, "hub2.log"))
	waitHubStarted(t, filepath.Join(tmp, "hub2.log"))
	code, out = ctl("status", cfg)
	expect("status with another hub", code, out, ctlRunning, "running: ", "another hub runs on this Rex server")
	code, out = ctl("stop", cfg2)
	expect("stop of the other config", code, out, 0, "stopped")
	if c := waitHubExit(t, hub2, exit2); c != 0 {
		t.Errorf("the other hub exited %d", c)
	}
	if hubExited(exit, 300*time.Millisecond) {
		t.Fatal("stopping the other config stopped this wrapper")
	}

	// Backoff: the lab server stops. status says backoff (exit 3); stop sends no signal (exit 3).
	server.Process.Signal(syscall.SIGTERM)
	waitLogLine(t, wlog, "restart in")
	code, out = ctl("status", cfg)
	expect("status in backoff", code, out, ctlWrapper, "backoff: the restart wrapper (pid ")
	if strings.HasPrefix(out, "not running") {
		t.Errorf("status in backoff says not running:\n%s", out)
	}
	code, out = ctl("stop", cfg)
	expect("stop in backoff", code, out, ctlWrapper, "backoff: ", "stop it in its pane")
	if hubExited(exit, 300*time.Millisecond) {
		t.Fatal("stop in backoff stopped the wrapper")
	}

	// A new lab server: the wrapper's next start is ready; stop stops hub and wrapper.
	server = startLabServer(t, root, lab)
	for deadline := time.Now().Add(40 * time.Second); strings.Count(readFile(wlog), " ready\n") < 2; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no second ready:\n%s", readFile(wlog))
		}
	}
	code, out = ctl("stop", cfg)
	expect("stop, running", code, out, 0, "stopped")
	if c := waitHubExit(t, wrapper, exit); c != 0 {
		t.Errorf("the wrapper exited %d after stop\n%s", c, readFile(wlog))
	}
	code, out = ctl("status", cfg)
	expect("status after stop", code, out, ctlNotRunning, "not running")
	_ = server
}
