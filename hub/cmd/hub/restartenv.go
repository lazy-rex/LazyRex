package main

// The hub side of the restart wrapper (docs/hub.md, "Restart wrapper"). The wrapper gives its child
// three environment variables. main reads and unsets them first, so no process that the hub starts
// inherits them:
//   - LAZYREX_READY_FD: the write end of the readiness pipe. After hub_started, the hub writes
//     "ready <canonical state>\n" once and closes it. Ready means the hub runs; it does not mean
//     that the plugins are healthy.
//   - LAZYREX_EXPECT_STATE: the canonical state that the wrapper owns. A hub whose config names
//     another state (or a state that does not exist) exits 3 before any side effect of start.
//   - LAZYREX_EXEC: the wrapper's exec count (restartexec.go). hub.info reports it, and
//     hub.admin.relaunch compares its expect_exec with it.
//   - LAZYREX_SDK and LAZYREX_VIEW: the SDK directory and the view that the wrapper resolved at its
//     start ("" for none). A child uses them exactly, so every child of one wrapper process, also a
//     crash restart after an install switched its links, has the same paths. Both are required
//     with LAZYREX_EXPECT_STATE, and either one without it is an error.

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
)

// exitStateMismatch is the hub's exit status when its config state is not the wrapper's state.
const exitStateMismatch = 3

type restartEnv struct {
	readyFD     int // -1: no readiness pipe
	expectState string
	exec        int    // the wrapper's exec count; 0 without a wrapper
	sdk, view   string // the wrapper's resolved paths; used only with expectState
}

// takeRestartEnv reads and unsets the wrapper's variables. It sets FD_CLOEXEC on the ready fd, so
// no child of the hub holds the pipe open.
func takeRestartEnv() (restartEnv, error) {
	e := restartEnv{readyFD: -1, expectState: os.Getenv("LAZYREX_EXPECT_STATE")}
	s, ok := os.LookupEnv("LAZYREX_READY_FD")
	execText, hasExec := os.LookupEnv("LAZYREX_EXEC")
	sdk, hasSDK := os.LookupEnv("LAZYREX_SDK")
	view, hasView := os.LookupEnv("LAZYREX_VIEW")
	for _, v := range []string{"LAZYREX_READY_FD", "LAZYREX_EXPECT_STATE", "LAZYREX_EXEC", "LAZYREX_SDK", "LAZYREX_VIEW"} {
		os.Unsetenv(v)
	}
	switch {
	case e.expectState != "" && (!hasSDK || !hasView):
		return e, fmt.Errorf("a restart wrapper's child needs LAZYREX_SDK and LAZYREX_VIEW")
	case e.expectState == "" && (hasSDK || hasView):
		return e, fmt.Errorf("LAZYREX_SDK and LAZYREX_VIEW come only from a restart wrapper, with LAZYREX_EXPECT_STATE")
	}
	e.sdk, e.view = sdk, view
	if hasExec {
		n, err := strconv.Atoi(execText)
		if err != nil || n < 0 {
			return e, fmt.Errorf("LAZYREX_EXEC %q is not an exec count", execText)
		}
		e.exec = n
	}
	if !ok {
		return e, nil
	}
	fd, err := strconv.Atoi(s)
	if err != nil || fd < 3 {
		return e, fmt.Errorf("LAZYREX_READY_FD %q is not a descriptor above 2", s)
	}
	syscall.CloseOnExec(fd)
	e.readyFD = fd
	return e, nil
}

// canonicalState is the one spelling of a state directory: absolute, symlinks resolved. The hub,
// the wrapper, the ready line and hub.info use it. The directory must exist.
func canonicalState(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// checkExpectedState compares the config state with the wrapper's state, before the hub makes the
// directory or takes its lock. It returns the canonical state.
func checkExpectedState(state, expect string) (string, error) {
	got, err := canonicalState(state)
	if err != nil {
		return "", fmt.Errorf("the config state %s cannot be resolved (%v), but this restart wrapper owns %s; start a new wrapper", state, err, expect)
	}
	if got != expect {
		return "", fmt.Errorf("the config state is now %s, but this restart wrapper owns %s; start a new wrapper", got, expect)
	}
	return got, nil
}

// signalReady writes the ready line once, without blocking, and closes the fd. It returns a failed
// or short write for main to log; the hub runs on, and the wrapper sees EOF without a ready line. A
// closed pipe gives EPIPE, not SIGPIPE, because the fd is not 1 or 2.
func signalReady(fd int, state string) error {
	if fd < 0 {
		return nil
	}
	defer syscall.Close(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("ready fd: %w", err)
	}
	line := []byte("ready " + state + "\n")
	n, err := syscall.Write(fd, line)
	if err != nil {
		return fmt.Errorf("ready fd: %w", err)
	}
	if n < len(line) {
		return fmt.Errorf("ready fd: wrote %d of %d bytes", n, len(line))
	}
	return nil
}

// catchSIGPIPE runs at the top of main under the wrapper. There stdout and stderr are a pipe to
// the wrapper, and a wrapper that is gone must not kill the hub: a write then fails with EPIPE and
// the line is lost. SIGPIPE is caught (Notify), not ignored, so children of the hub (git, rex)
// start with the default action; an ignored signal would stay ignored across exec. The channel is
// never read; a dropped signal is fine.
func catchSIGPIPE() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}
