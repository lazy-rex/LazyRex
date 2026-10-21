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

// The restart wrapper with the real hub binary and its own lab server (docs/hub.md, "Restart
// wrapper"). Lab only: SHUTDOWN_LAB=1.
//  1. A second wrapper on the same state starts no hub and exits 1; the Rex client list is unchanged.
//  2. The lab server stops: the hub exits 1 and the wrapper goes to backoff. A new lab server in the
//     same directory: the restarted hub reaches ready.
//  3. hub.admin.shutdown: the hub exits 0, the wrapper exits 0.
//  4. A new wrapper; the config then names another state, and the lab server stops: the restarted
//     child exits 3 and the wrapper exits 3, with nothing made in the new state.
//  5. The palette launch: the wrapper in a labeled pane; its lines in the pane, the hub's output in
//     <state>/hub.log only.
func TestRestartWrapperLab(t *testing.T) {
	if os.Getenv("SHUTDOWN_LAB") != "1" {
		t.Skip("set SHUTDOWN_LAB=1 to run the restart lab test (it starts its own lab server)")
	}
	rexBin, err := exec.LookPath("rex")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	lab, err := os.MkdirTemp("/tmp", "rexlab-restart-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	socket := "unix://" + lab + "/rex.sock"
	server := startLabServer(t, root, lab)
	state := filepath.Join(tmp, "state")
	cfg := filepath.Join(tmp, "hub.json")
	writeCfg := func(state string) {
		b, _ := json.Marshal(map[string]any{"server": socket, "state": state,
			"agents": []map[string]any{{"id": "user", "name": "User", "status_record": ""}}})
		if err := os.WriteFile(cfg, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCfg(state)
	rexIn := func(args ...string) (string, error) {
		out, err := exec.Command(rexBin, append([]string{"-S", socket, "--autostart=false", "--timeout", "5s"}, args...)...).CombinedOutput()
		return string(out), err
	}

	wlog := filepath.Join(tmp, "wrapper.log")
	wrapper, exit := startRestartWrapper(t, bin, cfg, wlog)
	hubLog := filepath.Join(state, "hub.log") // the hub's output; the wrapper's lines go to its pane (wlog) and here
	waitHubStarted(t, hubLog)
	waitLogLine(t, wlog, " ready\n")

	// 1. A second wrapper: no hub, exit 1, the same Rex clients.
	before, _ := rexIn("ls")
	out, err := exec.Command(bin, "-config", cfg, "-restart").CombinedOutput()
	if code := exitCodeOf(err); code != 1 || !strings.Contains(string(out), "another restart wrapper owns") {
		t.Fatalf("second wrapper: exit %d\n%s", code, out)
	}
	if strings.Contains(string(out), "started hub") {
		t.Errorf("the second wrapper started a hub:\n%s", out)
	}
	if after, _ := rexIn("ls"); after != before {
		t.Errorf("rex ls changed after a refused wrapper:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// 2. The Rex connection closes: exit 1, backoff; a new lab server: the restarted hub is ready.
	server.Process.Signal(syscall.SIGTERM)
	waitLogLine(t, wlog, "restart in")
	server = startLabServer(t, root, lab)
	for deadline := time.Now().Add(30 * time.Second); strings.Count(readFile(wlog), " ready\n") < 2; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no second ready:\n%s", readFile(wlog))
		}
	}
	client := lastHubStarted(t, hubLog)

	// 3. hub.admin.shutdown: the hub exits 0 and the wrapper exits 0.
	if out, err := rexIn("-C", client, "do", "hub.admin.shutdown"); err != nil {
		t.Fatalf("shutdown: %v\n%s", err, out)
	}
	if code := waitHubExit(t, wrapper, exit); code != 0 {
		t.Fatalf("wrapper exit %d, want 0\n%s", code, readFile(wlog))
	}

	// 4. A changed config state: the new wrapper owns the old state; its restarted child is refused.
	wlog2 := filepath.Join(tmp, "wrapper2.log")
	wrapper, exit = startRestartWrapper(t, bin, cfg, wlog2)
	waitLogLine(t, wlog2, " ready\n")
	newState := filepath.Join(tmp, "state2")
	os.Mkdir(newState, 0o700)
	writeCfg(newState)
	server.Process.Signal(syscall.SIGTERM)
	server = startLabServer(t, root, lab)
	if code := waitHubExit(t, wrapper, exit); code != exitStateMismatch {
		t.Fatalf("wrapper exit %d after a changed state, want 3\n%s", code, readFile(wlog2))
	}
	for _, f := range []string{"hub.lock", "hub.db", "events.jsonl", "restart.lock"} {
		if _, err := os.Stat(filepath.Join(newState, f)); !os.IsNotExist(err) {
			t.Errorf("%s exists in the new state", f)
		}
	}

	// 5. The launch that the palette entry uses: the wrapper in a labeled pane. The pane shows the
	// wrapper's lines; the hub's output is in <state>/hub.log only; <state>/dev/hub.log (dev.sh) is
	// never written.
	writeCfg(state)
	if out, err := rexIn("new", "--shell=none", "launch", "--", "/bin/sleep", "300"); err != nil {
		t.Fatalf("new session: %v\n%s", err, out)
	}
	started := strings.Count(readFile(hubLog), "\"event\":\"hub_started\"")
	if out, err := rexIn("run", "-s", "launch", "--shell=none", "--focus=false", "--keep-open", "--label", "lazyrex-hub-wrapper",
		"--", bin, "-config", cfg, "-restart"); err != nil {
		t.Fatalf("rex run: %v\n%s", err, out)
	}
	pane := func() string { out, _ := rexIn("capture", "-b", "lazyrex-hub-wrapper"); return out }
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(pane(), " ready"); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no ready in the pane:\n%s", pane())
		}
	}
	if n := strings.Count(readFile(hubLog), "\"event\":\"hub_started\""); n != started+1 {
		t.Errorf("hub_started in hub.log: %d, want %d", n, started+1)
	}
	if p := pane(); strings.Contains(p, "hub_started") || !strings.Contains(p, "restart: started hub") {
		t.Errorf("pane:\n%s", p)
	}
	if _, err := os.Stat(filepath.Join(state, "dev", "hub.log")); !os.IsNotExist(err) {
		t.Errorf("dev/hub.log exists: %v", err)
	}
	if out, err := rexIn("-C", lastHubStarted(t, hubLog), "do", "hub.admin.shutdown"); err != nil {
		t.Fatalf("shutdown: %v\n%s", err, out)
	}
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(pane(), "exited: status 0"); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the wrapper did not stop:\n%s", pane())
		}
	}
}

func startRestartWrapper(t *testing.T, bin, cfg, logPath string) (*exec.Cmd, chan int) {
	t.Helper()
	logf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-config", cfg, "-restart")
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exit := make(chan int, 1)
	go func() {
		cmd.Wait()
		logf.Close()
		exit <- cmd.ProcessState.ExitCode()
	}()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Signal(syscall.SIGTERM) // the wrapper stops its own hub
			select {
			case <-exit:
			case <-time.After(15 * time.Second):
				cmd.Process.Kill()
			}
		}
	})
	return cmd, exit
}

func waitLogLine(t *testing.T, path, s string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(readFile(path), s) {
			return
		}
	}
	t.Fatalf("no %q in %s:\n%s", s, path, readFile(path))
}

func lastHubStarted(t *testing.T, path string) string {
	t.Helper()
	m := hubStartedLine.FindAllStringSubmatch(readFile(path), -1)
	if len(m) == 0 {
		t.Fatalf("no hub_started in %s", path)
	}
	return m[len(m)-1][1]
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	return -1
}
