package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Focused tests of the lab keeper (labkeeper.go). The test binary is the keeper (TestMain), with a
// grace of 300 ms. Each test checks the keeper's group with a real ps listing.

// keptShell starts a shell script under a keeper with its output on a pipe, as runFile starts the
// runner. The test's cleanup retires the keeper if the test did not.
func keptShell(t *testing.T, script string) (*kept, *os.File) {
	t.Helper()
	stdout, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	k, err := startKept(os.Args[0], "runner", []string{"/bin/sh", "-c", script}, append(os.Environ(), labKeeperGraceEnv+"=300"), "", out, nil)
	out.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.kill() })
	return k, stdout
}

// rawKeeper is a keeper that the test drives through its two pipes.
type rawKeeper struct {
	c    *exec.Cmd
	ctl  *os.File
	repF *os.File
	rep  *bufio.Reader
	done chan error
}

func startRawKeeper(t *testing.T, argv []string, env ...string) *rawKeeper {
	t.Helper()
	cr, cw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rr, rw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(os.Args[0], argv...)
	c.Env = append(append(os.Environ(), labKeeperEnv+"=1", labKeeperGraceEnv+"=300"), env...)
	c.ExtraFiles = []*os.File{cr, rw}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	cr.Close()
	rw.Close()
	k := &rawKeeper{c: c, ctl: cw, repF: rr, rep: bufio.NewReader(rr), done: make(chan error, 1)}
	go func() { k.done <- c.Wait() }()
	t.Cleanup(func() {
		cw.Close()
		rr.Close()
		select {
		case <-k.done:
		case <-time.After(5 * time.Second):
			t.Errorf("keeper %d did not end", c.Process.Pid)
		}
	})
	return k
}

func (k *rawKeeper) send(t *testing.T, line string) {
	t.Helper()
	if _, err := k.ctl.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func (k *rawKeeper) expect(t *testing.T, prefix string) string {
	t.Helper()
	k.repF.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := k.rep.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, prefix) {
		t.Fatalf("report: %q, %v; want %q", line, err, prefix)
	}
	return strings.TrimSuffix(line, "\n")
}

// wait returns the keeper's exit error within 5 s.
func (k *rawKeeper) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-k.done:
		k.done <- err // for the cleanup
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper did not end in 5 s")
		return nil
	}
}

// groupLeft lists the live processes of group pgid (real ps), waiting up to 2 s for it to empty.
func groupLeft(t *testing.T, pgid int) []int {
	t.Helper()
	var left []int
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, err := exec.Command("ps", "-eo", "pid=,pgid=,stat=").Output()
		if err != nil {
			t.Fatal(err)
		}
		left = left[:0]
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) == 3 && f[1] == strconv.Itoa(pgid) && !strings.HasPrefix(f[2], "Z") {
				pid, _ := strconv.Atoi(f[0])
				left = append(left, pid)
			}
		}
		if len(left) == 0 || time.Now().After(end) {
			return left
		}
	}
}

func killedBySIGKILL(err error) bool {
	e, ok := err.(*exec.ExitError)
	if !ok {
		return false
	}
	ws, ok := e.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// EOF before "start", or another first line: exit 125, and the command never ran.
func TestKeeperNoStart(t *testing.T) {
	for _, first := range []string{"", "go"} {
		marker := filepath.Join(t.TempDir(), "ran")
		k := startRawKeeper(t, []string{"/bin/sh", "-c", "touch " + marker})
		if first != "" {
			k.send(t, first)
		}
		k.ctl.Close()
		if err := k.wait(t); exitCode(err) != keeperNoStart {
			t.Errorf("first line %q: keeper status %v, want exit %d", first, err, keeperNoStart)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("first line %q: the command ran", first)
		}
	}
}

// A normal stop with the real ps listing: "exit 7", then "stop" gives "stopped" and status 0.
func TestKeeperNormalStop(t *testing.T) {
	k := startRawKeeper(t, []string{"/bin/sh", "-c", "exit 7"})
	k.send(t, "start")
	k.expect(t, "child ")
	k.expect(t, "exit 7")
	k.send(t, "stop")
	k.expect(t, "stopped")
	if err := k.wait(t); err != nil {
		t.Errorf("keeper status %v, want 0", err)
	}
	if left := groupLeft(t, k.c.Process.Pid); len(left) > 0 {
		t.Errorf("group members left: %v", left)
	}
}

// The command exits 3 while its child, which ignores SIGTERM, runs on. The keeper reports "exit 3"
// and stays. "stop" gives "escalate" and SIGKILL: the keeper's status is killed, and the child is gone.
func TestKeeperEscalates(t *testing.T) {
	k := startRawKeeper(t, []string{"/bin/sh", "-c", "(trap '' TERM; exec sleep 30) & exit 3"})
	k.send(t, "start")
	k.expect(t, "child ")
	k.expect(t, "exit 3")
	select {
	case err := <-k.done:
		t.Fatalf("the keeper ended after the command's exit: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	k.send(t, "stop")
	if line := k.expect(t, "escalate members remain"); !strings.Contains(line, "[") {
		t.Errorf("escalate line: %q", line)
	}
	if err := k.wait(t); !killedBySIGKILL(err) {
		t.Errorf("keeper status %v, want SIGKILL", err)
	}
	if left := groupLeft(t, k.c.Process.Pid); len(left) > 0 {
		t.Errorf("group members left: %v", left)
	}
}

// A failed listing never means "empty": "escalate listing failed", then SIGKILL.
func TestKeeperListingFails(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	k := startRawKeeper(t, []string{"/bin/sleep", "30"}, "PATH="+bin)
	k.send(t, "start")
	k.expect(t, "child ")
	k.send(t, "stop")
	// The command's exit line can come first: the SIGTERM reaches it before the listing.
	if line := k.expect(t, ""); !strings.HasPrefix(line, "escalate listing failed") {
		if line != "exit signal terminated" {
			t.Fatalf("report: %q", line)
		}
		k.expect(t, "escalate listing failed")
	}
	if err := k.wait(t); !killedBySIGKILL(err) {
		t.Errorf("keeper status %v, want SIGKILL", err)
	}
	if left := groupLeft(t, k.c.Process.Pid); len(left) > 0 {
		t.Errorf("group members left: %v", left)
	}
}

// The command's SIGTERM has the default disposition: a command that sends SIGTERM to itself dies.
func TestKeeperCommandSIGTERMDefault(t *testing.T) {
	k := startRawKeeper(t, []string{"/bin/sh", "-c", "kill -TERM $$; sleep 5"})
	k.send(t, "start")
	k.expect(t, "child ")
	k.expect(t, "exit signal terminated")
	k.send(t, "stop")
	k.expect(t, "stopped")
	k.wait(t)
}

// The command gets neither the keeper variables (the hub would start as a second keeper) nor the
// keeper's pipes (fd 3 and 4). The probe of an fd is a subshell: dash exits at a failed redirection
// of a special builtin.
func TestKeeperCommandEnvAndFiles(t *testing.T) {
	dir := t.TempDir()
	k := startRawKeeper(t, []string{"/bin/sh", "-c", `env > "$0/env"; for fd in 3 4; do if (true >&$fd) 2>/dev/null; then echo "fd $fd"; fi; done > "$0/fds"`, dir})
	k.send(t, "start")
	k.expect(t, "child ")
	k.expect(t, "exit 0")
	k.send(t, "stop")
	k.expect(t, "stopped")
	k.wait(t)
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	fds, err := os.ReadFile(filepath.Join(dir, "fds"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), labKeeperEnv+"=") || strings.Contains(string(env), labKeeperGraceEnv+"=") || !strings.Contains(string(env), "PATH=") {
		t.Errorf("the command's environment:\n%s", env)
	}
	if len(fds) > 0 {
		t.Errorf("the command has the keeper's pipes: %s", fds)
	}
}

// The test process dies during the report: after "start", both its control writer and its report
// reader close before it reads the report. The keeper's report writes fail, and it still stops its
// group and exits 0.
func TestKeeperParentDeathDuringReport(t *testing.T) {
	k := startRawKeeper(t, []string{"/bin/sleep", "30"})
	k.send(t, "start")
	k.ctl.Close()
	k.repF.Close()
	if err := k.wait(t); err != nil {
		t.Errorf("keeper status %v, want 0", err)
	}
	if left := groupLeft(t, k.c.Process.Pid); len(left) > 0 {
		t.Errorf("group members left: %v", left)
	}
}

// The report reader closes after the "child" line, and the control pipe ends without "stop" (the
// test process was killed): the keeper stops the group, and the failed writes change nothing.
func TestKeeperClosedReader(t *testing.T) {
	k := startRawKeeper(t, []string{"/bin/sleep", "30"})
	k.send(t, "start")
	k.expect(t, "child ")
	k.repF.Close()
	k.ctl.Close()
	if err := k.wait(t); err != nil {
		t.Errorf("keeper status %v, want 0", err)
	}
	if left := groupLeft(t, k.c.Process.Pid); len(left) > 0 {
		t.Errorf("group members left: %v", left)
	}
}

// The parent side: startKept gives the command's pid and status; stop is clean.
func TestKeptStartStop(t *testing.T) {
	k, stdout := keptShell(t, "echo out; exit 4")
	defer stdout.Close()
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	<-k.done
	if line != "out\n" || k.status == nil || k.status.Error() != "exit status 4" || k.pid <= 0 || k.start == "" {
		t.Fatalf("output %q, status %v, pid %d, start %q", line, k.status, k.pid, k.start)
	}
	if p := k.stop(); len(p) > 0 {
		t.Fatalf("stop: %v", p)
	}
}

// A keeper that dies from outside (a SIGKILL that is not its own) leaves the command's status
// unknown. The parent never takes the keeper's status as the command's: stop names it.
func TestKeptNoReport(t *testing.T) {
	k, stdout := keptShell(t, "exec sleep 2")
	defer stdout.Close()
	syscall.Kill(k.w.pid, syscall.SIGKILL) // the keeper only; its group is not signaled
	<-k.done
	if k.status != errKeeperNoReport {
		t.Fatalf("status %v", k.status)
	}
	p := strings.Join(k.stop(), "\n")
	if !strings.Contains(p, "status is unknown") || !strings.Contains(p, "without a clean stop") {
		t.Fatalf("stop: %s", p)
	}
}

// An unknown report line is logged and ignored.
func TestKeptUnknownLine(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	k := &kept{what: "hub", done: make(chan struct{}), ended: make(chan struct{}), logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }}
	go k.read(bufio.NewReader(r), r)
	w.WriteString("later 1\nexit 0\nstopped\n")
	w.Close()
	<-k.ended
	if k.status != nil || !k.reported || k.end != "stopped" || len(logged) != 1 || !strings.Contains(logged[0], `"later 1"`) {
		t.Fatalf("status %v, end %q, log %v", k.status, k.end, logged)
	}
}

func TestKeeperGraceBounds(t *testing.T) {
	for v, want := range map[string]time.Duration{"": labKeeperGrace, "x": labKeeperGrace, "1": 50 * time.Millisecond, "300": 300 * time.Millisecond, "99999": 10 * time.Second} {
		if got := keeperGrace(v); got != want {
			t.Errorf("keeperGrace(%q) = %v, want %v", v, got, want)
		}
	}
}
