//go:build darwin

package main

import (
	"time"

	"golang.org/x/sys/unix"
)

// waitExit waits until the process pid exits or the timeout passes. It does not reap the process.
// It watches the exit with kqueue (EVFILT_PROC, NOTE_EXIT). A process that already exited and is
// not reaped cannot be registered (ESRCH): it counts as exited. On any other error it waits for the
// whole timeout.
func waitExit(pid int, timeout time.Duration) {
	kq, err := unix.Kqueue()
	if err != nil {
		time.Sleep(timeout)
		return
	}
	defer unix.Close(kq)
	ev := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	deadline := time.Now().Add(timeout)
	out := make([]unix.Kevent_t, 1)
	for {
		ts := unix.NsecToTimespec(int64(max(time.Until(deadline), 0)))
		// Any event returns: the exit, or a registration error that kevent puts in out (EV_ERROR).
		_, err := unix.Kevent(kq, []unix.Kevent_t{ev}, out, &ts)
		switch {
		case err == unix.EINTR:
			continue
		case err == unix.ESRCH:
			return
		case err != nil:
			time.Sleep(time.Until(deadline))
		}
		return
	}
}
