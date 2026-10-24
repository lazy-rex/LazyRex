package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Stop and exit status of the hub process (docs/hub.md, "Stop and exit status"), with the real
// binary. The test starts its own lab server (lab/server.sh in a new lab directory) and stops only
// that server and the hubs it started. Lab only: LAZYREX_LAB=1.
//   - An argument to hub.admin.shutdown is refused; the hub keeps running.
//   - hub.admin.shutdown replies {"stopping": true}, then the hub exits 0.
//   - SIGTERM: exit 0.
//   - The Rex connection closes (the lab server stops): exit 1.
func TestHubShutdownExitStatusLab(t *testing.T) {
	needLab(t)
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
	// A short path: a Unix socket path has a length limit.
	lab, err := os.MkdirTemp("/tmp", "rexlab-shutdown-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	socket := "unix://" + lab + "/rex.sock"
	server := startLabServer(t, root, lab)

	cfg := filepath.Join(tmp, "hub.json")
	b, _ := json.Marshal(map[string]any{"server": socket, "state": filepath.Join(tmp, "state"),
		"agents": []map[string]any{{"id": "user", "name": "User", "status_record": ""}}})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rexIn := func(args ...string) (string, error) {
		out, err := exec.Command(rexBin, append([]string{"-S", socket, "--autostart=false", "--timeout", "5s"}, args...)...).CombinedOutput()
		return string(out), err
	}

	// 1. An argument is refused; then hub.admin.shutdown replies before the hub exits 0.
	hub, exit := startShutdownHub(t, bin, cfg, filepath.Join(tmp, "hub1.log"))
	client := waitHubStarted(t, filepath.Join(tmp, "hub1.log"))
	if n := strings.Count(readLog(tmp, "hub1.log"), " sdk: "); n != 1 {
		t.Errorf("sdk lines in hub1.log: %d, want 1\n%s", n, readLog(tmp, "hub1.log"))
	}
	if out, err := rexIn("-C", client, "do", "hub.admin.shutdown", "--args", `{"now":true}`); err == nil || !strings.Contains(out, "takes no arguments") {
		t.Fatalf("argument: %v\n%s", err, out)
	}
	if hubExited(exit, 300*time.Millisecond) {
		t.Fatal("the hub exited after a refused shutdown")
	}
	out, err := rexIn("-C", client, "do", "hub.admin.shutdown")
	if err != nil || !strings.Contains(out, `"stopping"`) || !strings.Contains(out, "true") {
		t.Fatalf("shutdown reply: %v\n%s", err, out)
	}
	if code := waitHubExit(t, hub, exit); code != 0 {
		t.Errorf("exit status after hub.admin.shutdown: %d, want 0\n%s", code, readLog(tmp, "hub1.log"))
	}

	// 2. SIGTERM: exit 0.
	hub, exit = startShutdownHub(t, bin, cfg, filepath.Join(tmp, "hub2.log"))
	waitHubStarted(t, filepath.Join(tmp, "hub2.log"))
	hub.Process.Signal(syscall.SIGTERM)
	if code := waitHubExit(t, hub, exit); code != 0 {
		t.Errorf("exit status after SIGTERM: %d, want 0\n%s", code, readLog(tmp, "hub2.log"))
	}

	// 3. The Rex connection closes: exit 1.
	hub, exit = startShutdownHub(t, bin, cfg, filepath.Join(tmp, "hub3.log"))
	waitHubStarted(t, filepath.Join(tmp, "hub3.log"))
	server.Process.Signal(syscall.SIGTERM)
	if code := waitHubExit(t, hub, exit); code != 1 {
		t.Errorf("exit status after the Rex connection closed: %d, want 1\n%s", code, readLog(tmp, "hub3.log"))
	}
	if log, _ := os.ReadFile(filepath.Join(tmp, "hub3.log")); !strings.Contains(string(log), "rex connection closed") {
		t.Errorf("hub log has no \"rex connection closed\":\n%s", log)
	}
}

// A signal while the hub starts is also a deliberate stop: exit 0. The test pauses its own lab
// server (SIGSTOP), so the hub waits on Rex (the real-server check, then the connection), then
// sends SIGTERM to the hub. Lab only: LAZYREX_LAB=1.
func TestHubSignalDuringStartLab(t *testing.T) { signalDuringStart(t) }

// The same with -restart: the wrapper stops before its first child, exit 0.
func TestHubRestartSignalDuringStartLab(t *testing.T) { signalDuringStart(t, "-restart") }

func signalDuringStart(t *testing.T, args ...string) {
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
	lab, err := os.MkdirTemp("/tmp", "rexlab-shutdown-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	server := startLabServer(t, root, lab)
	cfg := filepath.Join(tmp, "hub.json")
	b, _ := json.Marshal(map[string]any{"server": "unix://" + lab + "/rex.sock", "state": filepath.Join(tmp, "state"),
		"agents": []map[string]any{{"id": "user", "name": "User", "status_record": ""}}})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}

	server.Process.Signal(syscall.SIGSTOP)
	hub, exit := startShutdownHub(t, bin, cfg, filepath.Join(tmp, "hub.log"), args...)
	if hubExited(exit, time.Second) {
		t.Fatalf("the hub exited before the signal:\n%s", readLog(tmp, "hub.log"))
	}
	if strings.Contains(readLog(tmp, "hub.log"), "hub_started") {
		t.Fatal("the hub started although the lab server is paused")
	}
	hub.Process.Signal(syscall.SIGTERM)
	if code := waitHubExit(t, hub, exit); code != 0 {
		t.Errorf("exit status after SIGTERM during the start: %d, want 0\n%s", code, readLog(tmp, "hub.log"))
	}
}

// startLabServer starts lab/server.sh in lab (its own new directory) and waits for its socket. The
// script execs rex, so the started process is the server; cleanup stops only that process.
func startLabServer(t *testing.T, root, lab string) *exec.Cmd {
	t.Helper()
	logf, err := os.Create(filepath.Join(lab, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "lab", "server.sh"), lab)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); logf.Close(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGCONT) // a test may have paused it
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	})
	socket := "unix://" + lab + "/rex.sock"
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if exec.Command("rex", "-S", socket, "--autostart=false", "--timeout", "1s", "ls").Run() == nil {
			return cmd
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(lab, "server.log"))
			t.Fatalf("the lab server did not start:\n%s", b)
		}
	}
}

// startShutdownHub starts the hub binary; exit receives its exit status once.
func startShutdownHub(t *testing.T, bin, cfg, logPath string, args ...string) (*exec.Cmd, chan int) {
	t.Helper()
	logf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, append([]string{"-config", cfg}, args...)...)
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
			cmd.Process.Kill()
		}
	})
	return cmd, exit
}

func hubExited(exit chan int, d time.Duration) bool {
	select {
	case code := <-exit:
		exit <- code
		return true
	case <-time.After(d):
		return false
	}
}

func waitHubExit(t *testing.T, cmd *exec.Cmd, exit chan int) int {
	t.Helper()
	select {
	case code := <-exit:
		return code
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the hub did not exit within 10 s")
		return -1
	}
}

// waitHubStarted waits for the hub_started event in this hub's own log and returns its client ID.
// A client list is not enough: Rex can still list the previous hub's client as connected.
func waitHubStarted(t *testing.T, logPath string) string {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		b, _ := os.ReadFile(logPath)
		if m := hubStartedLine.FindSubmatch(b); m != nil {
			return string(m[1])
		}
	}
	b, _ := os.ReadFile(logPath)
	t.Fatalf("no hub_started event in %s:\n%s", logPath, b)
	return ""
}

var hubStartedLine = regexp.MustCompile(`"client_id":"([^"]+)","event":"hub_started"`)

func readLog(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return string(b)
}
