package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lockHolderEnv selects the lock holder role of the test binary (TestLockHolderHelper): the value
// is the path of the file to lock.
const lockHolderEnv = "LAZYREX_TEST_LOCK_HOLDER"

// TestLockHolderHelper is a helper process, not a test: it takes an exclusive flock of the file
// (waiting while another holder has it), writes "locked", and holds the lock until its stdin ends.
func TestLockHolderHelper(t *testing.T) {
	path := os.Getenv(lockHolderEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.Exit(3)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		os.Exit(4)
	}
	os.Stdout.WriteString("locked\n")
	io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// heldLock is a helper process that holds an exclusive flock of a file.
type heldLock struct {
	cmd     *exec.Cmd
	release io.WriteCloser
	ended   bool
}

// holdLock starts a lock holder of path and returns after it has the lock: the acquisition is
// synchronized, not timed. The test's cleanup releases and reaps it, also when the test never
// releases it.
func holdLock(t *testing.T, path string) *heldLock {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHolderHelper$")
	cmd.Env = append(os.Environ(), lockHolderEnv+"="+path)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &heldLock{cmd: cmd, release: in}
	t.Cleanup(h.end)
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("the lock holder did not report the lock: %q, %v", line, err)
	}
	return h
}

// end releases the lock and reaps the holder. A second call does nothing.
func (h *heldLock) end() {
	if h.ended {
		return
	}
	h.ended = true
	h.release.Close()
	h.cmd.Wait()
}

// shortRestartWait sets a short bound and poll for one test.
func shortRestartWait(t *testing.T, bound time.Duration) {
	t.Helper()
	b, p := restartWaitBound, restartWaitPoll
	restartWaitBound, restartWaitPoll = bound, 10*time.Millisecond
	t.Cleanup(func() { restartWaitBound, restartWaitPoll = b, p })
}

// probeFailures counts the failed checks of waitOldHub, and runs then (if set) once at the first.
func probeFailures(t *testing.T, then func()) *int {
	t.Helper()
	n := 0
	restartProbeFailed = func() {
		n++
		if n == 1 && then != nil {
			then()
		}
	}
	t.Cleanup(func() { restartProbeFailed = nil })
	return &n
}

// zombie starts a child that exits at once and is not reaped while the test runs: a process in
// state Z (Linux) or SZOMB (darwin). It returns the child's pid and start time.
func zombie(t *testing.T) (int, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Wait() })
	pid := cmd.Process.Pid
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		r, err := procRead(pid)
		if err == nil && r.zombie {
			return pid, r.start
		}
		if time.Now().After(end) {
			t.Fatalf("pid %d is not a zombie: %+v, %v", pid, r, err)
		}
	}
}

// goneProcess returns a pid that no process has (procRead answers errProcGone).
func goneProcess(t *testing.T) int {
	t.Helper()
	if _, err := procRead(0x7ffffff0); !errors.Is(err, errProcGone) {
		t.Skipf("pid 0x7ffffff0 is not gone: %v", err)
	}
	return 0x7ffffff0
}

func tempLock(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hub.lock")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The flock decides: a held lock keeps the restart waiting even when the old hub is gone, and a
// release lets it go on.
func TestWaitOldHubWaitsForTheLock(t *testing.T) {
	shortRestartWait(t, 10*time.Second)
	p := tempLock(t)
	h := holdLock(t, p)
	n := probeFailures(t, h.end)
	if err := waitOldHub(context.Background(), goneProcess(t), "1.000000", p); err != nil {
		t.Fatal(err)
	}
	if *n == 0 {
		t.Error("no failed check while the lock was held")
	}
}

// The observed case (Linux lab host, hub 6fbd787): the old hub's leader in state Z while hub.lock
// is still held. Z is never enough alone.
func TestWaitOldHubZombieWithTheLockHeldWaits(t *testing.T) {
	shortRestartWait(t, 10*time.Second)
	p := tempLock(t)
	pid, start := zombie(t)
	h := holdLock(t, p)
	n := probeFailures(t, h.end)
	if err := waitOldHub(context.Background(), pid, start, p); err != nil {
		t.Fatal(err)
	}
	if *n == 0 {
		t.Error("a zombie with the lock held did not wait")
	}
}

func TestWaitOldHubZombieWithAFreeLockGoesOn(t *testing.T) {
	shortRestartWait(t, 10*time.Second)
	pid, start := zombie(t)
	n := probeFailures(t, nil)
	if err := waitOldHub(context.Background(), pid, start, tempLock(t)); err != nil || *n != 0 {
		t.Fatalf("%v, %d failed checks", err, *n)
	}
}

// A pid that now belongs to another process (another start time) is gone.
func TestWaitOldHubReusedPIDIsGone(t *testing.T) {
	shortRestartWait(t, 10*time.Second)
	n := probeFailures(t, nil)
	if err := waitOldHub(context.Background(), os.Getpid(), "1.000000", tempLock(t)); err != nil || *n != 0 {
		t.Fatalf("%v, %d failed checks", err, *n)
	}
}

// Each failed part has its own text at the bound.
func TestWaitOldHubBoundTexts(t *testing.T) {
	self := os.Getpid()
	r, err := procRead(self)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, want string
		pid        int
		held       bool
		readErr    error
	}{
		{"running, free lock", "the old hub has not exited (pid", self, false, nil},
		{"running, held lock", "is still running", self, true, nil},
		{"gone, held lock", "is gone", 0x7ffffff0, true, nil},
		{"read error", "cannot read the old hub", self, false, errors.New("permission denied")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shortRestartWait(t, 200*time.Millisecond)
			p := tempLock(t)
			if c.held {
				holdLock(t, p)
			}
			if c.readErr != nil {
				oldHubRead = func(int) (procRecord, error) { return procRecord{}, c.readErr }
				t.Cleanup(func() { oldHubRead = procRead })
			}
			began := time.Now()
			err := waitOldHub(context.Background(), c.pid, r.start, p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if d := time.Since(began); d < 200*time.Millisecond {
				t.Errorf("returned after %v, before the bound", d)
			}
		})
	}
}

func TestWaitOldHubCancel(t *testing.T) {
	shortRestartWait(t, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	probeFailures(t, cancel)
	if err := waitOldHub(ctx, os.Getpid(), startTime(os.Getpid()), tempLock(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// The probe releases its lock: a second flock succeeds after it, and a missing file is free.
func TestLockFreeReleases(t *testing.T) {
	p := tempLock(t)
	if free, err := lockFree(p); !free || err != nil {
		t.Fatalf("first probe: %v, %v", free, err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("a flock after the probe: %v", err)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if free, err := lockFree(filepath.Join(t.TempDir(), "none")); !free || err != nil {
		t.Fatalf("missing file: %v, %v", free, err)
	}
	holdLock(t, p)
	if free, err := lockFree(p); free || err != nil {
		t.Fatalf("held: %v, %v", free, err)
	}
}
