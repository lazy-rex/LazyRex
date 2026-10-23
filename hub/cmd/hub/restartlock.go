package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// After a restart stops the old hub, the new hub starts only when BOTH hold (the hub lead,
// 2026-10-22):
//  1. a non-blocking flock of <state>/hub.lock succeeds (the new hub's own start takes it the
//     same way, and refuses at once when it is held: "another hub owns");
//  2. the old hub is gone: its pid is absent, the pid has another start time, or its state is
//     zombie.
//
// The flock is the deciding check. State zombie is never enough alone: on the Linux lab host, in 2
// of 30 crash restarts (hub 6fbd787), the old hub's thread group leader was already in state Z
// while hub.lock was still held. The file table of a multi-threaded process is released at the
// last thread's exit, after the leader reports Z. A read of the old hub that fails for another
// reason than "no such process" is not gone.
var (
	restartWaitBound = 10 * time.Second
	restartWaitPoll  = 50 * time.Millisecond
	oldHubRead       = procRead // a Go-test seam: a failed process read
)

// waitOldHub waits, bounded, until the old hub (pid with its start time) is gone and hub.lock at
// lockPath is free. Past the bound, the error says which part failed.
func waitOldHub(ctx context.Context, pid int, start, lockPath string) error {
	deadline := time.Now().Add(restartWaitBound)
	for {
		free, lerr := lockFree(lockPath)
		gone, rerr := oldHubGone(pid, start)
		if lerr == nil && free && rerr == nil && gone {
			return nil
		}
		if restartProbeFailed != nil {
			restartProbeFailed()
		}
		if !time.Now().Before(deadline) {
			return oldHubWaitError(pid, free, lerr, gone, rerr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(restartWaitPoll):
		}
	}
}

// oldHubGone reports whether the old hub has exited: its pid is absent, the pid now belongs to a
// process with another start time, or the process is a zombie. A pid of 0 (the keeper reported
// none) leaves the decision to the flock.
func oldHubGone(pid int, start string) (bool, error) {
	if pid <= 0 {
		return true, nil
	}
	r, err := oldHubRead(pid)
	if errors.Is(err, errProcGone) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return r.start != start || r.zombie, nil
}

// lockFree takes a non-blocking exclusive flock of path and releases it at once: true when no
// other open file description holds it. The probe closes its own descriptor before it returns,
// so it never holds the lock when the new hub starts. A missing file is free.
func lockFree(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func oldHubWaitError(pid int, free bool, lerr error, gone bool, rerr error) error {
	wait := restartWaitBound.Seconds()
	switch {
	case rerr != nil:
		return fmt.Errorf("cannot read the old hub (pid %d) after %g s: %v", pid, wait, rerr)
	case lerr != nil:
		return fmt.Errorf("cannot check hub.lock after %g s: %v", wait, lerr)
	case !free && gone:
		return fmt.Errorf("hub.lock is held %g s after the stop; the old hub (pid %d) is gone", wait, pid)
	case !free:
		return fmt.Errorf("hub.lock is held %g s after the stop; the old hub (pid %d) is still running", wait, pid)
	default:
		return fmt.Errorf("the old hub has not exited (pid %d) %g s after the stop", pid, wait)
	}
}
