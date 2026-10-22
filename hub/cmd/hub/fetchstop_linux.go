//go:build linux

package main

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// stopSelf stops the process (fetchTestStop). A SIGSTOP from kill(getpid) is for the process, and
// Linux may give it to another thread: the caller then runs on, and an exit can come before the
// stop (TestFetchChildStalledBeforeExit, 2026-10-22). A SIGSTOP for the calling thread starts the
// stop before that thread runs its next instruction.
func stopSelf() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	unix.Tgkill(unix.Getpid(), unix.Gettid(), unix.SIGSTOP)
}
