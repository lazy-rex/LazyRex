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
//   - Exit 4 after a ready is a relaunch request: the wrapper execs itself (restartexec.go).
//   - Paths (wrapperPaths): at its start, the wrapper finds its invoked path and resolves it once.
//     Every child, also a crash respawn, runs the resolved file. A later switch of a symlink in the
//     invoked path changes no child of this wrapper.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	readyEOFWait    = 2 * time.Second  // after the child's exit; only a leaked fd holds the pipe open
	refusalReadWait = 10 * time.Second // the plugin refusal read after a ready
)

type wrapper struct {
	bin      string                                      // the resolved executable: every child runs it
	invoked  string                                      // the executable path as invoked, absolute, symlinks kept
	argv     []string                                    // the wrapper's own arguments (os.Args[1:]): a relaunch passes them again
	execFn   func(path string, argv, env []string) error // syscall.Exec; returns only on a failure
	adopt    *adoption                                   // the lock and exec target of a self-exec; nil for a fresh wrapper
	args     []string
	pane     io.Writer   // the wrapper's pane (its stderr)
	out      *log.Logger // the wrapper's own lines: the pane, and the log once it is open
	log      *restartLog
	logHook  func(*restartLog) // a test replaces the log's file write (a stalled disk) before the log starts
	now      func() time.Time
	after    func(time.Duration) <-chan time.Time // the stop wait and the ready pipe wait
	backoff  func(time.Duration) <-chan time.Time // the delay before a restart
	stopWait time.Duration
	sigs     chan os.Signal
	refusals func(ctx context.Context, pid int) []string // the refused plugins of the hub with this pid; nil: no read

	state    string // canonical, set by main
	lock     *os.File
	attempt  int
	lastExit string
	exec     int       // the generation: 0 at a first start, +1 at each exec
	last     *lastExec // the last exec attempt, until the next one begins
}

// restartStatus is <state>/restart.json.
type restartStatus struct {
	Version    int       `json:"version"`
	PID        int       `json:"pid"`
	Start      string    `json:"start"` // the wrapper's process start time (process.go)
	Phase      string    `json:"phase"` // starting, running, backoff, gave_up, stopped, exec
	Attempt    int       `json:"attempt"`
	LastExit   string    `json:"last_exit,omitempty"`
	NextStart  string    `json:"next_start,omitempty"`
	Since      string    `json:"since"`
	Exec       int       `json:"exec"`
	ExecTarget int       `json:"exec_target,omitempty"` // phase exec only: the generation that the exec starts
	LastExec   *lastExec `json:"last_exec,omitempty"`
}

// restartMain runs the wrapper. args are the hub's arguments without -restart. ctx ends at the
// first SIGTERM or SIGINT since main installed its handler. main's handler stays while sigs
// receives too, so each signal is in ctx, in sigs, or in both: stopOnDone carries ctx into sigs
// for the wrapper's whole run.
func restartMain(ctx context.Context, cfg Config, args []string) int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stopOnDone(ctx, sigs)()
	w, err := newWrapper(args, sigs)
	if err != nil {
		log.Printf("restart: %v", err)
		return 1
	}
	w.refusals = func(ctx context.Context, pid int) []string { return hubRefusals(cfg, w.state)(ctx, pid) }
	return w.main(cfg.State)
}

// newWrapper takes the inherited exec state and the wrapper's paths, and builds the wrapper that
// runs children with args.
func newWrapper(args []string, sigs chan os.Signal) (*wrapper, error) {
	adopt, err := takeAdoption()
	if err != nil {
		return nil, err
	}
	invoked, bin, err := wrapperPaths(os.Args[0])
	if err != nil {
		return nil, err
	}
	return &wrapper{bin: bin, invoked: invoked, argv: os.Args[1:], execFn: syscall.Exec, adopt: adopt, args: args,
		pane: os.Stderr, now: time.Now, after: time.After, backoff: time.After, stopWait: childStopWait, sigs: sigs}, nil
}

// wrapperPaths returns the wrapper's invoked path and its resolved path. invoked is arg0 made
// absolute against the start directory when it has a slash, or else found with exec.LookPath on
// the start PATH; its symlinks stay. resolved is invoked with every symlink resolved. resolved must
// be the running executable: a path that names another file now refuses the start.
func wrapperPaths(arg0 string) (invoked, resolved string, err error) {
	p := arg0
	if !strings.Contains(arg0, "/") {
		if p, err = exec.LookPath(arg0); err != nil {
			return "", "", fmt.Errorf("the wrapper's path %q: %w", arg0, err)
		}
	}
	if invoked, err = filepath.Abs(p); err != nil {
		return "", "", fmt.Errorf("the wrapper's path %q: %w", arg0, err)
	}
	if resolved, err = filepath.EvalSymlinks(invoked); err != nil {
		return "", "", fmt.Errorf("the wrapper's path %s: %w", invoked, err)
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return "", "", fmt.Errorf("the wrapper's path %s: %w", invoked, err)
	}
	self, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("the running executable: %w", err)
	}
	if selfSt, err := os.Stat(self); err != nil || !os.SameFile(st, selfSt) {
		return "", "", fmt.Errorf("the wrapper's path %s names %s, not the running executable %s", invoked, resolved, self)
	}
	return invoked, resolved, nil
}

// stopOnDone puts SIGTERM into sigs when ctx ends (the signal may also have been SIGINT). The
// wrapper acts on its first signal only, so a signal in both ctx and sigs stops it once. The
// returned func ends the bridge. A ctx that has ended already puts its signal in sigs before
// stopOnDone returns, so the wrapper never starts a first child for it.
func stopOnDone(ctx context.Context, sigs chan os.Signal) func() {
	if ctx.Err() != nil {
		select {
		case sigs <- syscall.SIGTERM:
		default: // a signal is in sigs already
		}
		return func() {}
	}
	quit := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			select {
			case sigs <- syscall.SIGTERM:
			default: // a signal is in sigs already
			}
		case <-quit:
		}
	}()
	return func() { close(quit) }
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
	w.out = log.New(w.pane, "", log.LstdFlags) // until the log is open: no child runs yet
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
	var lock *os.File
	if w.adopt != nil {
		lock, err = adoptRestartLock(state, w.adopt)
	} else {
		lock, err = takeRestartLock(state)
	}
	if err != nil {
		w.out.Printf("restart: %v", err)
		return 1
	}
	w.lock = lock
	defer lock.Close()
	if w.adopt != nil {
		w.exec = w.adopt.target
		w.last = &lastExec{Target: w.exec, Outcome: "ok", At: w.now().UTC().Format(time.RFC3339)}
	}
	// The log opens only under the lock: the wrapper is the only writer of <state>/hub.log.
	w.log = newRestartLog(state, w.pane)
	if w.logHook != nil {
		w.logHook(w.log)
	}
	w.log.start()
	defer w.log.close(logEndWait)
	w.out = log.New(w.log, "", log.LstdFlags)
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

// refuseBesideAWrapper refuses a hub start that no restart wrapper made while a wrapper owns
// the state (docs/hub.md, "Ownership"): in the wrapper's backoff wait or exec gap, such a hub would
// take the state from it. It only asks (F_GETLK on a read-only fd of this process). A wrapper
// that takes the lock after this check still meets hub.lock, which never lets two hubs run.
func refuseBesideAWrapper(state string) error {
	path := filepath.Join(state, "restart.lock")
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("restart lock %s: %w", path, err)
	}
	defer f.Close() // this process holds no lock on the file, so the close releases nothing
	pid, err := lockHolder(f)
	if err != nil {
		return fmt.Errorf("restart lock %s: %w", path, err)
	}
	if pid > 0 {
		return fmt.Errorf("a restart wrapper owns %s (pid %d); use lazyrex-hub restart or stop", state, pid)
	}
	return nil
}

// restartLockHolder returns the pid that holds a write lock on f, or 0 (no lock, or a failed read).
func restartLockHolder(f *os.File) int {
	pid, _ := lockHolder(f)
	return pid
}

// lockHolder asks for the holder of the write lock on f (F_GETLK); 0 when no process holds it.
func lockHolder(f *os.File) (int, error) {
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		return 0, err
	}
	if lk.Type == syscall.F_UNLCK {
		return 0, nil
	}
	return int(lk.Pid), nil
}

func (w *wrapper) writeStatus(phase string, next time.Time) {
	if err := w.putStatus(phase, next, 0); err != nil {
		w.out.Printf("restart: status: %v", err)
	}
}

// writeStatusErr writes phase "exec" with its target, and returns the error: a relaunch does not
// exec without it.
func (w *wrapper) writeStatusErr(phase string, target int) error {
	return w.putStatus(phase, time.Time{}, target)
}

func (w *wrapper) putStatus(phase string, next time.Time, target int) error {
	s := restartStatus{Version: 1, PID: os.Getpid(), Start: startTime(os.Getpid()), Phase: phase,
		Attempt: w.attempt, LastExit: w.lastExit, Since: w.now().UTC().Format(time.RFC3339),
		Exec: w.exec, ExecTarget: target, LastExec: w.last}
	if !next.IsZero() {
		s.NextStart = next.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(s)
	path := filepath.Join(w.state, "restart.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
		if end.code == exitRelaunch && end.ready {
			w.relaunch() // returns only when the exec failed: start the child again at once
			continue
		}
		switch {
		case end.code == 0:
			w.writeStatus("stopped", time.Time{})
			return 0
		case end.code == exitStateMismatch && !end.ready:
			w.out.Printf("restart: the config names another state; this wrapper stops")
			w.writeStatus("stopped", time.Time{})
			return exitStateMismatch
		case !everReady:
			// The hub's output goes to hub.log only. Its last lines say why it failed (for example
			// another hub that holds the state's lock), so the pane gets them too.
			if w.log != nil {
				for _, s := range w.log.lastLines(10) {
					w.out.Printf("restart: hub: %s", s)
				}
			}
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
	or, ow, err := os.Pipe()
	if err != nil {
		r.Close()
		wr.Close()
		w.out.Printf("restart: %v", err)
		return childEnd{stop: 1}
	}
	cmd := exec.Command(w.bin, w.args...)
	cmd.Stdout, cmd.Stderr = ow, ow // one pipe keeps the order of stdout and stderr
	cmd.ExtraFiles = []*os.File{wr} // fd 3
	// Its own process group: Ctrl-C or a closed pane signals only the wrapper, which stops the child.
	// Otherwise a SIGHUP death of the child would count as a failure and look like a restart.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "LAZYREX_READY_FD=3", "LAZYREX_EXPECT_STATE="+w.state)
	err = cmd.Start()
	wr.Close()
	ow.Close()
	if err != nil {
		r.Close()
		or.Close()
		w.out.Printf("restart: start: %v", err)
		return childEnd{stop: 1}
	}
	pid := cmd.Process.Pid

	// The readers start first, before any status I/O, so a slow status write never stops the
	// hub's output.
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
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		w.log.readRun(or)
	}()
	// retire ends this run's pipes: it waits at most readyEOFWait for EOF on both (a descendant of
	// the hub can hold them open), then closes both read ends and waits for both readers. A Close
	// ends a blocked Read of a pipe. Output after that is discarded. It runs before the exit line,
	// so no byte of this run follows that line or mixes into the next run.
	retired := false
	retire := func() {
		if retired {
			return
		}
		retired = true
		limit := w.after(readyEOFWait)
	wait:
		for _, done := range []chan struct{}{readDone, outDone} {
			select {
			case <-done:
			case <-limit:
				w.out.Printf("restart: the hub's output or ready pipe is still open after its exit; later output is discarded")
				break wait
			}
		}
		r.Close()
		or.Close()
		<-readDone
		<-outDone
	}
	defer retire()
	exitc := make(chan *os.ProcessState, 1)
	go func() {
		cmd.Wait()
		exitc <- cmd.ProcessState
	}()
	w.out.Printf("restart: started hub pid %d (attempt %d)", pid, w.attempt)
	w.writeStatus("starting", time.Time{})

	// After the ready, the refused plugins of this run. The read never holds up an exit or a
	// signal: it ends with this run, and a result after the run is discarded.
	readCtx, endRead := context.WithCancel(context.Background())
	defer endRead()
	refusedc := make(chan []string, 1)
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
		if w.refusals != nil {
			go func() {
				ctx, cancel := context.WithTimeout(readCtx, refusalReadWait)
				defer cancel()
				refusedc <- w.refusals(ctx, pid)
			}()
		}
		return true
	}
	for {
		select {
		case lines := <-refusedc:
			for _, l := range lines {
				w.out.Printf("restart: %s", l)
			}
		case line := <-readyc:
			if !onReady(line) {
				w.stopChild(pid, cmd, exitc, retire)
				w.writeStatus("stopped", time.Time{})
				return childEnd{stop: 1}
			}
		case ps := <-exitc:
			// Classify only after the ready pipe ends, so "ready, then an exit" is after ready.
			retire()
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
			code := w.stopChild(pid, cmd, exitc, retire)
			w.writeStatus("stopped", time.Time{})
			return childEnd{stop: code}
		}
	}
}

// stopChild sends SIGTERM to the child the wrapper started, waits at most stopWait, then kills
// it. retire ends the run's pipes before the exit line. It returns the wrapper's exit status: 0
// after a stop, 1 after a kill.
func (w *wrapper) stopChild(pid int, cmd *exec.Cmd, exitc chan *os.ProcessState, retire func()) int {
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case ps := <-exitc:
		retire()
		w.lastExit = exitDesc(ps)
		w.out.Printf("restart: hub pid %d exited: %s", pid, w.lastExit)
		return 0
	case <-w.after(w.stopWait):
	}
	cmd.Process.Kill()
	ps := <-exitc
	retire()
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
