package main

// The wrapper's self-exec (docs/hub.md, "Restart wrapper", "Relaunch"). A child that exits 4 after
// its ready asked for a relaunch: the wrapper runs its invoked path again in the same process
// (syscall.Exec), so the wrapper code, the hub code and the binary change together while the pane
// and the wrapper pid stay.
//   - Exec count: each exec is one generation. A first start has exec 0. The old wrapper at exec n
//     writes restart.json phase "exec" with exec_target n+1, and passes n+1 in
//     LAZYREX_RESTART_EXEC, before it changes anything else.
//   - The lock: restart.lock is an fcntl lock. It belongs to the process and survives exec while
//     its fd stays open, but the process loses it when it closes ANY fd for that file. So the
//     wrapper clears FD_CLOEXEC on its one lock fd before the exec, passes the fd number in
//     LAZYREX_RESTART_LOCK_FD, and never opens restart.lock a second time.
//   - Adoption (adoptRestartLock): the new wrapper takes both variables as one state. It adopts only
//     when the fd is restart.lock, read-write, and restart.json names this process (pid and start
//     time) in phase "exec" with the same exec_target. Anything else is refused (exit 1).
//   - An exec that fails: the old process keeps the lock and exec n, reopens its log, records the
//     failure in last_exec, and starts the child again from its resolved path (the old version).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const exitRelaunch = 4 // the hub asks its wrapper for a relaunch (hub.admin.relaunch)

// lastExec is the outcome of the wrapper's last exec attempt. It stays in restart.json until the
// next exec attempt begins, so a reader that looks after the hub is ready again still sees it.
type lastExec struct {
	Target  int    `json:"target"`
	Outcome string `json:"outcome"` // ok or failed
	Error   string `json:"error,omitempty"`
	At      string `json:"at"`
}

// adoption is the inherited state of a self-exec: the lock fd and the exec target.
type adoption struct {
	fd     int
	target int
}

// takeAdoption reads and unsets LAZYREX_RESTART_LOCK_FD and LAZYREX_RESTART_EXEC. Both absent: nil,
// a fresh wrapper. One alone, or a value that is not a number, is an error.
func takeAdoption() (*adoption, error) {
	fdText, hasFD := os.LookupEnv("LAZYREX_RESTART_LOCK_FD")
	execText, hasExec := os.LookupEnv("LAZYREX_RESTART_EXEC")
	os.Unsetenv("LAZYREX_RESTART_LOCK_FD")
	os.Unsetenv("LAZYREX_RESTART_EXEC")
	if !hasFD && !hasExec {
		return nil, nil
	}
	if !hasFD || !hasExec {
		return nil, errors.New("LAZYREX_RESTART_LOCK_FD and LAZYREX_RESTART_EXEC come together; one alone is refused")
	}
	fd, err := strconv.Atoi(fdText)
	if err != nil || fd <= 2 {
		return nil, fmt.Errorf("LAZYREX_RESTART_LOCK_FD %q is not a descriptor above 2", fdText)
	}
	target, err := strconv.Atoi(execText)
	if err != nil || target < 1 {
		return nil, fmt.Errorf("LAZYREX_RESTART_EXEC %q is not an exec count above 0", execText)
	}
	return &adoption{fd: fd, target: target}, nil
}

// adoptRestartLock adopts the lock fd of a self-exec. A refusal never closes an fd that names
// restart.lock: that close would release the lock while this process runs.
func adoptRestartLock(state string, a *adoption) (*os.File, error) {
	path := filepath.Join(state, "restart.lock")
	if _, err := fcntl(a.fd, syscall.F_GETFD, 0); err != nil {
		return nil, fmt.Errorf("LAZYREX_RESTART_LOCK_FD %d is not open: %w", a.fd, err)
	}
	var fst, pst syscall.Stat_t
	if err := syscall.Fstat(a.fd, &fst); err != nil {
		return nil, fmt.Errorf("LAZYREX_RESTART_LOCK_FD %d: %w", a.fd, err)
	}
	if err := syscall.Stat(path, &pst); err != nil {
		return nil, fmt.Errorf("restart lock %s: %w", path, err)
	}
	if fst.Dev != pst.Dev || fst.Ino != pst.Ino {
		syscall.Close(a.fd) // another file: its close does not touch the lock
		return nil, fmt.Errorf("LAZYREX_RESTART_LOCK_FD %d is not %s", a.fd, path)
	}
	if fl, err := fcntl(a.fd, syscall.F_GETFL, 0); err != nil || fl&syscall.O_ACCMODE != syscall.O_RDWR {
		return nil, fmt.Errorf("LAZYREX_RESTART_LOCK_FD %d is not open for reading and writing", a.fd)
	}
	s, err := readRestartStatus(state)
	if err != nil {
		return nil, fmt.Errorf("adoption: %w", err)
	}
	pid := os.Getpid()
	if s.Phase != "exec" || s.PID != pid || s.Start != startTime(pid) || s.ExecTarget != a.target {
		return nil, fmt.Errorf("adoption: restart.json (phase %s, pid %d, exec_target %d) is not the exec of this process (pid %d, exec %d)", s.Phase, s.PID, s.ExecTarget, pid, a.target)
	}
	f := os.NewFile(uintptr(a.fd), path)
	// The lock of this process stays held; a lock that this process does not hold is taken now,
	// or refused when another process holds it.
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		return nil, fmt.Errorf("adoption: restart lock %s: %w", path, err)
	}
	syscall.CloseOnExec(a.fd)
	return f, nil
}

func readRestartStatus(state string) (restartStatus, error) {
	var s restartStatus
	b, err := os.ReadFile(filepath.Join(state, "restart.json"))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

func fcntl(fd, cmd, arg int) (int, error) {
	r, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(cmd), uintptr(arg))
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

// relaunch runs the wrapper's invoked path again (A2). It returns only when the exec did not
// happen: the log is open again, the lock and exec n are kept, and last_exec says why. The caller
// then starts the child again from the resolved path.
func (w *wrapper) relaunch() {
	target := w.exec + 1
	w.out.Printf("restart: the hub asked for a relaunch: exec %d of %s", target, w.invoked)
	if err := w.writeStatusErr("exec", target); err != nil {
		w.execFailed(target, fmt.Errorf("status before the exec: %w", err), false)
		return
	}
	// Nothing below may fail silently: the log closes, so each failure reopens it.
	w.log.close(logEndWait)
	w.out = log.New(w.pane, "", log.LstdFlags)
	fd := int(w.lock.Fd())
	if _, err := fcntl(fd, syscall.F_SETFD, 0); err != nil {
		w.execFailed(target, fmt.Errorf("clear close-on-exec of the lock: %w", err), true)
		return
	}
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LAZYREX_RESTART_LOCK_FD=") && !strings.HasPrefix(kv, "LAZYREX_RESTART_EXEC=") {
			env = append(env, kv)
		}
	}
	env = append(env, "LAZYREX_RESTART_LOCK_FD="+strconv.Itoa(fd), "LAZYREX_RESTART_EXEC="+strconv.Itoa(target))
	err := w.execFn(w.invoked, append([]string{w.invoked}, w.argv...), env)
	syscall.CloseOnExec(fd) // the exec failed: the fd is the wrapper's own again
	w.execFailed(target, err, true)
}

// execFailed records a failed exec attempt and makes the wrapper ready to start its child again.
func (w *wrapper) execFailed(target int, err error, reopen bool) {
	if reopen {
		w.openLog()
	}
	w.out.Printf("restart: exec %s failed: %v; the hub starts again from %s", w.invoked, err, w.bin)
	w.last = &lastExec{Target: target, Outcome: "failed", Error: err.Error(), At: w.now().UTC().Format(time.RFC3339)}
}
