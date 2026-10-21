//go:build linux

package main

import (
	"time"

	"golang.org/x/sys/unix"
)

// waitExit waits until the process pid exits or the timeout passes. It does not reap the process:
// waitid with WNOWAIT leaves it a zombie. On timeout the goroutine stays blocked until the caller
// reaps the process.
func waitExit(pid int, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		var info unix.Siginfo
		for unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil) == unix.EINTR {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
