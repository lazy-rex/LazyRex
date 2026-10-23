package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
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
