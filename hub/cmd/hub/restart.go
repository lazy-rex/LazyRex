package main

// The restart wrapper (docs/hub.md, "Restart wrapper"): `lazyrex-hub -config <cfg> -restart` runs
// the hub as its child and starts it again after a failure.
//   - Ownership: one wrapper for each state. Before the first child, the wrapper takes an fcntl
//     write lock on <state>/restart.lock and holds it for its life. A second wrapper starts no hub.
//   - The child gets LAZYREX_READY_FD (the ready pipe) and LAZYREX_EXPECT_STATE (restartenv.go).
//   - Exit 0 of the child is a deliberate stop: the wrapper exits 0. Exit 3 is a changed config
//     state: the wrapper exits 3. A failure before the first ready ends the wrapper with the
//     child's status. Every other failure is restarted after 1, 2, 4 ... 60 s; a fifth restart
//     within 10 minutes is refused (gave_up, exit 1).
//   - SIGTERM, SIGINT or SIGHUP: SIGTERM to the child, at most 10 s, then SIGKILL; never a restart.
//   - <state>/restart.json is the status for readers (stop and status), replaced as a whole.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	restartWindow   = 10 * time.Minute
	restartMax      = 4 // restarts within restartWindow; the next one is refused
	restartMaxDelay = 60 * time.Second
	childStopWait   = 10 * time.Second
	readyEOFWait    = 2 * time.Second // after the child's exit; only a leaked fd holds the pipe open
)

type wrapper struct {
	bin      string
	args     []string
	out      *log.Logger // the wrapper's own lines: its pane
	now      func() time.Time
	after    func(time.Duration) <-chan time.Time // the stop wait and the ready pipe wait
	backoff  func(time.Duration) <-chan time.Time // the delay before a restart
	stopWait time.Duration
	sigs     chan os.Signal

	state    string // canonical, set by main
	attempt  int
	lastExit string
}

// restartStatus is <state>/restart.json.
type restartStatus struct {
	Version   int    `json:"version"`
	PID       int    `json:"pid"`
	Start     string `json:"start"` // the wrapper's process start time (process.go)
	Phase     string `json:"phase"` // starting, running, backoff, gave_up, stopped
	Attempt   int    `json:"attempt"`
	LastExit  string `json:"last_exit,omitempty"`
	NextStart string `json:"next_start,omitempty"`
	Since     string `json:"since"`
}

// restartMain runs the wrapper. args are the hub's arguments without -restart.
func restartMain(cfg Config, args []string) int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	bin, err := os.Executable()
	if err != nil {
		log.Print(err)
		return 1
	}
	w := &wrapper{bin: bin, args: args, out: log.New(os.Stderr, "", log.LstdFlags), now: time.Now,
		after: time.After, backoff: time.After, stopWait: childStopWait, sigs: sigs}
	return w.main(cfg.State)
}

// restartArgs drops -restart from the hub's arguments for the child.
func restartArgs(args []string) []string {
	var out []string
	for _, a := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && name == "restart" {
			continue // every form: -restart, --restart, -restart=<any value>
		}
		out = append(out, a)
	}
	return out
}

// main resolves and owns the state, then runs children until a stop.
func (w *wrapper) main(stateDir string) int {
	// The ready line is "ready <state>\n": a newline in the state cannot be framed.
	if strings.Contains(stateDir, "\n") {
		w.out.Printf("restart: the state path %q contains a newline; use another state directory", stateDir)
		return 1
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		w.out.Printf("restart: %v", err)
		return 1
	}
	state, err := canonicalState(stateDir)
	if err != nil {
		w.out.Printf("restart: %v", err)
		return 1
	}
	if strings.Contains(state, "\n") {
		w.out.Printf("restart: the state path %q contains a newline; use another state directory", state)
		return 1
	}
	w.state = state
	lock, err := takeRestartLock(state)
	if err != nil {
		w.out.Printf("restart: %v", err)
		return 1
	}
	defer lock.Close()
	return w.run()
}

// takeRestartLock takes the write lock on <state>/restart.lock without waiting. A lock held by
// another process names its pid when F_GETLK gives one. POSIX locks belong to the process: a
// close of any descriptor of the file releases them, so the wrapper opens the file only here.
func takeRestartLock(state string) (*os.File, error) {
	path := filepath.Join(state, "restart.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("restart lock: %w", err)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
			pid := restartLockHolder(f)
			f.Close()
			if pid > 0 {
				return nil, fmt.Errorf("another restart wrapper owns %s (pid %d)", state, pid)
			}
			return nil, fmt.Errorf("another restart wrapper owns %s", state)
		}
		f.Close()
		return nil, fmt.Errorf("restart lock %s: %w", path, err)
	}
	return f, nil
}

// restartLockHolder returns the pid that holds a write lock on f, or 0 (no lock, or a failed read).
func restartLockHolder(f *os.File) int {
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil || lk.Type == syscall.F_UNLCK {
		return 0
	}
	return int(lk.Pid)
}

func (w *wrapper) writeStatus(phase string, next time.Time) {
	s := restartStatus{Version: 1, PID: os.Getpid(), Start: startTime(os.Getpid()), Phase: phase,
		Attempt: w.attempt, LastExit: w.lastExit, Since: w.now().UTC().Format(time.RFC3339)}
	if !next.IsZero() {
		s.NextStart = next.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(s)
	path := filepath.Join(w.state, "restart.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		w.out.Printf("restart: status: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		w.out.Printf("restart: status: %v", err)
	}
}

// childEnd is how one child ended. stop >= 0: the wrapper exits with stop.
type childEnd struct {
	ready bool
	code  int // the exit status; -1 for a signal death
	desc  string
	stop  int
}

func (w *wrapper) run() int {
	select {
	case s := <-w.sigs:
		w.out.Printf("restart: %v before the first start: stopped", s)
		w.writeStatus("stopped", time.Time{})
		return 0
	default:
	}
	everReady := false
	var restarts []time.Time
	for {
		w.attempt++
		end := w.runChild()
		if end.stop >= 0 {
			return end.stop
		}
		everReady = everReady || end.ready
		switch {
		case end.code == 0:
			w.writeStatus("stopped", time.Time{})
			return 0
		case end.code == exitStateMismatch && !end.ready:
			w.out.Printf("restart: the config names another state; this wrapper stops")
			w.writeStatus("stopped", time.Time{})
			return exitStateMismatch
		case !everReady:
			w.out.Printf("restart: the hub failed before its first ready; no restart")
			w.writeStatus("stopped", time.Time{})
			if end.code < 0 {
				return 1
			}
			return end.code
		}
		now := w.now()
		kept := restarts[:0]
		for _, t := range restarts {
			if now.Sub(t) < restartWindow {
				kept = append(kept, t)
			}
		}
		restarts = kept
		if len(restarts) >= restartMax {
			w.out.Printf("restart: gave up: %d restarts within %s; last exit %s", restartMax, restartWindow, end.desc)
			w.writeStatus("gave_up", time.Time{})
			return 1
		}
		restarts = append(restarts, now)
		delay := min(restartMaxDelay, time.Second<<(len(restarts)-1))
		w.out.Printf("restart: restart in %s (restart %d within %s)", delay, len(restarts), restartWindow)
		w.writeStatus("backoff", now.Add(delay))
		select {
		case <-w.backoff(delay):
		case s := <-w.sigs:
			w.out.Printf("restart: %v during backoff: stopped", s)
			w.writeStatus("stopped", time.Time{})
			return 0
		}
	}
}

// runChild starts one hub and waits until it exits or a signal stops it.
func (w *wrapper) runChild() childEnd {
	r, wr, err := os.Pipe()
	if err != nil {
		w.out.Printf("restart: %v", err)
		return childEnd{stop: 1}
	}
	cmd := exec.Command(w.bin, w.args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.ExtraFiles = []*os.File{wr} // fd 3
	// Its own process group: Ctrl-C or a closed pane signals only the wrapper, which stops the child.
	// Otherwise a SIGHUP death of the child would count as a failure and look like a restart.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "LAZYREX_READY_FD=3", "LAZYREX_EXPECT_STATE="+w.state)
	err = cmd.Start()
	wr.Close()
	if err != nil {
		r.Close()
		w.out.Printf("restart: start: %v", err)
		return childEnd{stop: 1}
	}
	pid := cmd.Process.Pid
	w.out.Printf("restart: started hub pid %d (attempt %d)", pid, w.attempt)
	w.writeStatus("starting", time.Time{})

	// The ready line, then EOF: the hub closes its end after the line, or at its exit.
	readyc := make(chan string, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		line, _ := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
		if line != "" {
			readyc <- line
		}
		io.Copy(io.Discard, r)
	}()
	// On every return: close the read end and end the reader, also when a descendant still holds
	// the write end (the 2 s limit below). A Close ends a blocked Read of a pipe.
	defer func() {
		r.Close()
		<-readDone
	}()
	exitc := make(chan *os.ProcessState, 1)
	go func() {
		cmd.Wait()
		exitc <- cmd.ProcessState
	}()

	ready := false
	onReady := func(line string) bool {
		got, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "ready ")
		if !ok || got != w.state {
			w.out.Printf("restart: the hub reported %q, not ready for %s: a defect", strings.TrimSpace(line), w.state)
			return false
		}
		ready = true
		w.out.Printf("restart: hub pid %d ready", pid)
		w.writeStatus("running", time.Time{})
		return true
	}
	for {
		select {
		case line := <-readyc:
			if !onReady(line) {
				w.stopChild(pid, cmd, exitc)
				w.writeStatus("stopped", time.Time{})
				return childEnd{stop: 1}
			}
		case ps := <-exitc:
			// Classify only after the ready pipe ends, so "ready, then an exit" is after ready.
			select {
			case <-readDone:
			case <-w.after(readyEOFWait):
				w.out.Printf("restart: the ready pipe is still open after the hub's exit")
			}
			select {
			case line := <-readyc:
				if !onReady(line) {
					w.writeStatus("stopped", time.Time{})
					return childEnd{stop: 1}
				}
			default:
			}
			end := childEnd{ready: ready, code: ps.ExitCode(), desc: exitDesc(ps), stop: -1}
			w.lastExit = end.desc
			w.out.Printf("restart: hub pid %d exited: %s", pid, end.desc)
			return end
		case s := <-w.sigs:
			w.out.Printf("restart: %v: stopping hub pid %d", s, pid)
			code := w.stopChild(pid, cmd, exitc)
			w.writeStatus("stopped", time.Time{})
			return childEnd{stop: code}
		}
	}
}

// stopChild sends SIGTERM to the child the wrapper started, waits at most stopWait, then kills
// it. It returns the wrapper's exit status: 0 after a stop, 1 after a kill.
func (w *wrapper) stopChild(pid int, cmd *exec.Cmd, exitc chan *os.ProcessState) int {
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case ps := <-exitc:
		w.lastExit = exitDesc(ps)
		w.out.Printf("restart: hub pid %d exited: %s", pid, w.lastExit)
		return 0
	case <-w.after(w.stopWait):
	}
	cmd.Process.Kill()
	ps := <-exitc
	w.lastExit = exitDesc(ps)
	w.out.Printf("restart: the hub did not stop within %s; killed (%s)", w.stopWait, w.lastExit)
	return 1
}

func exitDesc(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return "signal " + ws.Signal().String()
	}
	return fmt.Sprintf("status %d", ps.ExitCode())
}
