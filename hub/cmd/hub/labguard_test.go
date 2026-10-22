package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// guardGroupProc starts a stand-in lab process: a shell in its own process group with a child in
// the same group, as the server and its helpers are.
func guardGroupProc(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() })
	return cmd
}

func groupLine(pid int, start string) string { return fmt.Sprintf("group %d %s\n", pid, start) }

func runGuard(t *testing.T, lines ...string) (done chan int, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan int, 1)
	go func() { done <- labGuard(r, time.Second) }()
	for _, l := range lines {
		w.WriteString(l)
	}
	return done, w
}

func waitGuard(t *testing.T, done chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("the guard did not end")
		return 0
	}
}

func groupGone(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatalf("pid %d runs after the guard's stop", cmd.Process.Pid)
	}
	if err := syscall.Kill(-cmd.Process.Pid, 0); err == nil {
		t.Errorf("a process of group %d survived", cmd.Process.Pid)
	}
}

// At EOF (the test process ended) the guard stops every group that it was told about: the server
// and the runner, each with its child. Before EOF it signals nothing.
func TestLabGuardStopsGroupsAtEOF(t *testing.T) {
	srv, runner := guardGroupProc(t), guardGroupProc(t)
	done, w := runGuard(t, groupLine(srv.Process.Pid, startTime(srv.Process.Pid)), groupLine(runner.Process.Pid, startTime(runner.Process.Pid)))
	time.Sleep(300 * time.Millisecond)
	if !alive(srv.Process.Pid, startTime(srv.Process.Pid)) || !alive(runner.Process.Pid, startTime(runner.Process.Pid)) {
		t.Fatal("the guard signaled before EOF")
	}
	w.Close()
	if code := waitGuard(t, done); code != 0 {
		t.Errorf("exit %d", code)
	}
	groupGone(t, srv)
	groupGone(t, runner)
}

// A normal lab stop: the groups ended before EOF, so the guard ends at once and signals nothing.
// A group whose leader has another start time (a reused pid) is not signaled.
func TestLabGuardSignalsOnlyItsGroups(t *testing.T) {
	gone := guardGroupProc(t)
	pid, start := gone.Process.Pid, startTime(gone.Process.Pid)
	syscall.Kill(-pid, syscall.SIGKILL)
	gone.Wait()
	other := guardGroupProc(t)
	done, w := runGuard(t, groupLine(pid, start), groupLine(other.Process.Pid, "1.000000"), "noise\n")
	t0 := time.Now()
	w.Close()
	waitGuard(t, done)
	if time.Since(t0) > 500*time.Millisecond {
		t.Errorf("the guard waited %v with no live group", time.Since(t0))
	}
	if !alive(other.Process.Pid, startTime(other.Process.Pid)) {
		t.Error("the guard signaled a group with another start time")
	}
}

// After "release" (the lab's own stop) the guard ends at once and signals no group, also a live one:
// lab.stop reports a survivor and does not signal it.
func TestLabGuardRelease(t *testing.T) {
	g := guardGroupProc(t)
	done, w := runGuard(t, groupLine(g.Process.Pid, startTime(g.Process.Pid)), "release\n")
	if code := waitGuard(t, done); code != 0 {
		t.Errorf("exit %d", code)
	}
	w.Close()
	time.Sleep(300 * time.Millisecond)
	if !alive(g.Process.Pid, startTime(g.Process.Pid)) {
		t.Error("the guard signaled a group after release")
	}
}

// A guard started without a pipe on stdin (by hand, with a terminal or a file) exits at once.
func TestLabGuardWithoutPipe(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if code := labGuard(f, time.Second); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}
