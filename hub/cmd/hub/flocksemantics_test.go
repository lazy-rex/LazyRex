package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The flock(2) facts that plugin prune and the hub start rule depend on (prune.lock), between two
// processes, on macOS and Linux:
//   - a shared holder blocks LOCK_EX|LOCK_NB and allows another shared lock;
//   - an exclusive holder blocks a shared lock;
//   - the kernel drops the lock of a killed holder.
//
// The error of a blocked try is syscall.EWOULDBLOCK on both (35 on macOS, 11 on Linux).
func TestFlockSemantics(t *testing.T) {
	p := filepath.Join(t.TempDir(), "prune.lock")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	try := func(how int) error {
		t.Helper()
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	}
	blocked := func(err error) bool { return errors.Is(err, syscall.EWOULDBLOCK) }

	sh := holdFlock(t, p, "sh")
	if err := try(syscall.LOCK_EX); !blocked(err) {
		t.Errorf("a shared holder: LOCK_EX gave %v, want EWOULDBLOCK", err)
	}
	if err := try(syscall.LOCK_SH); err != nil {
		t.Errorf("a shared holder: LOCK_SH gave %v", err)
	}
	sh.Process.Kill()
	sh.Wait()
	if err := try(syscall.LOCK_EX); err != nil {
		t.Errorf("a killed shared holder: LOCK_EX gave %v", err)
	}

	ex := holdFlock(t, p, "ex")
	if err := try(syscall.LOCK_SH); !blocked(err) {
		t.Errorf("an exclusive holder: LOCK_SH gave %v, want EWOULDBLOCK", err)
	}
	ex.Process.Kill()
	ex.Wait()
}

// TestFlockHoldHelper holds a flock (LAZYREX_TEST_HOLD = <sh|ex>|<path>) until it is killed.
func TestFlockHoldHelper(t *testing.T) {
	spec := os.Getenv("LAZYREX_TEST_HOLD")
	if spec == "" {
		t.Skip("helper process only")
	}
	how, path, _ := strings.Cut(spec, "|")
	f, err := os.Open(path)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	op := syscall.LOCK_SH
	if how == "ex" {
		op = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), op); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Println("locked")
	select {}
}

// holdFlock starts a helper process that holds a shared ("sh") or exclusive ("ex") flock on path.
func holdFlock(t *testing.T, path, how string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFlockHoldHelper$")
	cmd.Env = append(os.Environ(), "LAZYREX_TEST_HOLD="+how+"|"+path)
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	buf := make([]byte, 64)
	n, _ := stdout.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "locked") {
		t.Fatalf("helper: %q", buf[:n])
	}
	return cmd
}
