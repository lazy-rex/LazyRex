package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// lazyrex-hub restart with the real binary, a real wrapper and its own lab server (docs/hub.md,
// "Restart"). Lab only: SHUTDOWN_LAB=1. The wrapper starts through a link, as a release does:
//  1. -expect-exec 1 at exec 0: exit 1, nothing sent; the same hub runs.
//  2. The link moves to another copy; restart -expect-exec 0: exit 0. The hub exited 4, the same
//     wrapper process execed itself, and a new hub process runs the new copy at exec 1.
//  3. The same command again: exit 1 before any send (the hub runs exec 1).
//  4. The link names a file that cannot run; restart -expect-exec 1: the exec fails, the old
//     version runs again at exec 1 in a new hub process: exit 1 with the exec error.
//  5. lazyrex-hub stop: stopped, and the wrapper exits 0.
func TestRestartCommandLab(t *testing.T) {
	if os.Getenv("SHUTDOWN_LAB") != "1" {
		t.Skip("set SHUTDOWN_LAB=1 to run the restart command lab test (it starts its own lab server)")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	v1, v2 := filepath.Join(tmp, "v1", "lazyrex-hub"), filepath.Join(tmp, "v2", "lazyrex-hub")
	for _, p := range []string{v1, v2} {
		os.MkdirAll(filepath.Dir(p), 0o700)
		if out, err := exec.Command("go", "build", "-o", p, ".").CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
	}
	v2real, err := filepath.EvalSymlinks(v2) // ps shows the resolved path (/var is a link on macOS)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "current")
	os.Symlink(v1, link)
	lab, err := os.MkdirTemp("/tmp", "rexlab-restartcmd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	startLabServer(t, root, lab)
	state := filepath.Join(tmp, "state")
	cfg := filepath.Join(tmp, "hub.json")
	b, _ := json.Marshal(map[string]any{"server": "unix://" + lab + "/rex.sock", "state": state,
		"agents": []map[string]any{{"id": "user", "name": "User", "status_record": ""}}})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	wlog := filepath.Join(tmp, "wrapper.log")
	wrapper, exit := startRestartWrapper(t, link, cfg, wlog)
	waitLogLine(t, wlog, " ready\n")
	ctl := func(args ...string) (int, string) {
		out, err := exec.Command(v1, append(args, "-config", cfg)...).CombinedOutput()
		return exitCodeOf(err), string(out)
	}
	hubPID := func() int {
		_, out := ctl("status")
		m := regexp.MustCompile(`^running: hub pid (\d+)`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("status:\n%s", out)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	exe := func(pid int) string {
		out, _ := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		return strings.TrimSpace(string(out))
	}

	// 1. Another exec count: nothing sent.
	old := hubPID()
	if code, out := ctl("restart", "-expect-exec", "1"); code != 1 || !strings.Contains(out, "runs exec 0, not 1") || !strings.Contains(out, "nothing was sent") {
		t.Fatalf("-expect-exec 1 at exec 0: exit %d\n%s", code, out)
	}
	if hubPID() != old {
		t.Fatal("the hub changed after a check that sent nothing")
	}

	// 2. A relaunch onto the new copy.
	os.Remove(link)
	os.Symlink(v2, link)
	code, out := ctl("restart", "-expect-exec", "0")
	want := fmt.Sprintf("restart wrapper pid %d", wrapper.Process.Pid)
	if code != 0 || !strings.Contains(out, "restarted: hub pid") || !strings.Contains(out, "exec 1") || !strings.Contains(out, want) {
		t.Fatalf("restart: exit %d\n%s\n--- wrapper:\n%s", code, out, readFile(wlog))
	}
	pid := hubPID()
	if pid == old || !strings.HasPrefix(exe(pid), v2real) {
		t.Errorf("after the restart: hub pid %d (old %d) runs %q, want a new process of %s", pid, old, exe(pid), v2real)
	}

	// 3. The same command again: refused before the send.
	if code, out := ctl("restart", "-expect-exec", "0"); code != 1 || !strings.Contains(out, "runs exec 1, not 0") || !strings.Contains(out, "nothing was sent") {
		t.Fatalf("a second restart with exec 0: exit %d\n%s", code, out)
	}
	if hubPID() != pid {
		t.Fatal("the hub changed after a check that sent nothing")
	}

	// 4. A failed exec: the old version (the resolved path, v2) runs again at exec 1.
	bad := filepath.Join(tmp, "bad")
	os.WriteFile(bad, []byte("not a program\n"), 0o600)
	os.Remove(link)
	os.Symlink(bad, link)
	code, out = ctl("restart", "-expect-exec", "1")
	if code != 1 || !strings.Contains(out, "the exec of the new version failed") || !strings.Contains(out, "the old version runs again") ||
		!strings.Contains(out, "exec 1") || !strings.Contains(out, want) {
		t.Fatalf("a failed exec: exit %d\n%s\n--- wrapper:\n%s", code, out, readFile(wlog))
	}
	if again := hubPID(); again == pid || !strings.HasPrefix(exe(again), v2real) {
		t.Errorf("after the failed exec: hub pid %d (before %d) runs %q, want a new process of %s", again, pid, exe(again), v2real)
	}

	// 5. stop ends the hub and the wrapper.
	if code, out := ctl("stop"); code != 0 || !strings.Contains(out, "stopped") {
		t.Fatalf("stop: exit %d\n%s", code, out)
	}
	if code := waitHubExit(t, wrapper, exit); code != 0 {
		t.Fatalf("wrapper exit %d, want 0\n%s", code, readFile(wlog))
	}
}
