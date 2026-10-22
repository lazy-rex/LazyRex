package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The fetch child (plugin manager plan v5, amendments 2 and 3; a prototype, not yet called by the
// manager). The hub runs git through two processes of its own binary, so that no git process
// outlives its fetch, even after a hub crash:
//
//   - The watcher leads process group W. Its stdin is a pipe from the hub.
//   - The gate leads process group G. The watcher starts it. The gate starts git in G only after
//     the hub has recorded W and G and has sent "go". git and its helpers (git-remote-https) are
//     members of G.
//
// Ownership: every pid that the hub or the watcher signals is reserved by an unreaped child. The
// watcher never reaps the gate: the gate stays its child (alive or a zombie) until the watcher
// exits, and then init reaps it. So while W has not exited, the number G is in use. The hub
// signals W (its own unreaped child), and G only while W is stopped and not exited. No process is
// found by its argv.
//
// Stop rules, as a chain of pipes:
//   - The watcher: stdin EOF (the hub closed the pipe or died) -> kill G, exit.
//     The gate reports git's exit -> kill G (helpers too), report, exit.
//   - The gate: EOF on fd 3 (the watcher died, by any cause) -> kill its own group G.
//   - The hub: a cancel closes stdin; after fetchGrace it stops W, kills G and W (fetchHardStop).

// fetchRoleEnv selects the role of the hub binary in a fetch child: "watcher" or "gate".
const fetchRoleEnv = "LAZYREX_FETCH_ROLE"

// fetchStopEnv is a test hook of the watcher: it stops itself (SIGSTOP) at the named point
// ("before-report", "after-go" or "before-exit"), so a test can prove the hub's bound and the
// gate reservation with a stalled watcher. Only fetchCmd.TestStop sets it: runFetchChild removes
// an inherited value, and the manager sets TestStop only in test mode.
const fetchStopEnv = "LAZYREX_FETCH_TEST_STOP"

// fetchOutEnv tells the watcher and the gate that git's stdout goes to the hub: fd 3 of the
// watcher, fd 4 of the gate.
const fetchOutEnv = "LAZYREX_FETCH_STDOUT"

// fetchCmd is one git command for runFetchChild.
type fetchCmd struct {
	Argv     []string
	Dir      string    // git's working directory; "" is the hub's
	Stdout   io.Writer // git's stdout; nil: git's stdout goes to the hub's stderr
	TestStop string    // test mode only: the watcher's stop point (fetchStopEnv)
}

// fetchChildEnv is the environment of a watcher: the hub's, without the fetch variables, plus env.
func fetchChildEnv(env ...string) []string {
	var out []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, fetchRoleEnv+"=") && !strings.HasPrefix(e, fetchStopEnv+"=") && !strings.HasPrefix(e, fetchOutEnv+"=") {
			out = append(out, e)
		}
	}
	return append(out, env...)
}

// fetchGrace is the time from a cancel to the hard stop. The hard stop itself returns at once, so
// a fetch ends within 5 s of its cancel.
var fetchGrace = 4 * time.Second

// fetchSelf is the binary that runs the watcher and the gate: the hub itself.
var fetchSelf = os.Executable

// fetchResult is the outcome of runFetchChild.
type fetchResult struct {
	Watcher, Gate int // the pids that the hub recorded (Gate 0: no report)
	GitStatus     int // git's exit status; -1 when git did not report
	WatcherStatus syscall.WaitStatus
	Stopped       bool // the hub ended the watcher with the hard stop
}

// OK reports a fetch that the hub may publish: git exited 0, the watcher exited 0 by itself.
func (r fetchResult) OK() bool {
	return !r.Stopped && r.GitStatus == 0 && r.WatcherStatus.Exited() && r.WatcherStatus.ExitStatus() == 0
}

// runFetchChild runs fc.Argv (git and its arguments) through a watcher and a gate. record stores
// the two pids before git may start; an error from record starts nothing. record gets ctx and runs
// beside the cancel: a cancel during a slow record stops the fetch, and git never starts. A
// cancel of ctx stops the fetch within fetchGrace plus the hard stop.
func runFetchChild(ctx context.Context, fc fetchCmd, record func(ctx context.Context, watcher, gate int) error) (fetchResult, error) {
	res := fetchResult{GitStatus: -1}
	self, err := fetchSelf()
	if err != nil {
		return res, err
	}
	env := []string{fetchRoleEnv + "=watcher"}
	if fc.TestStop != "" {
		env = append(env, fetchStopEnv+"="+fc.TestStop)
	}
	cmd := exec.Command(self, fc.Argv...)
	cmd.Dir = fc.Dir // the gate and git inherit it
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = os.Stderr
	// closers are the hub's pipe ends. cmd.Wait is never called, so the hub closes them itself, on
	// every return.
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	var outR *os.File
	if fc.Stdout != nil {
		r, w, err := os.Pipe()
		if err != nil {
			return res, err
		}
		closers = append(closers, r, w)
		cmd.ExtraFiles = []*os.File{w}
		env = append(env, fetchOutEnv+"=1")
		outR = r
	}
	cmd.Env = fetchChildEnv(env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return res, err
	}
	closers = append(closers, stdin)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	closers = append(closers, stdout)
	if err := cmd.Start(); err != nil {
		return res, err
	}
	res.Watcher = cmd.Process.Pid
	defer cmd.Process.Release()
	// copied ends when git's stdout reaches EOF: every holder (the watcher, the gate, git and its
	// helpers) ended. The hub closes its write end now, so it holds none.
	copied := make(chan struct{})
	if outR != nil {
		cmd.ExtraFiles[0].Close()
		go func() {
			io.Copy(fc.Stdout, outR)
			close(copied)
		}()
		defer func() {
			select {
			case <-copied:
			case <-time.After(2 * time.Second): // a holder outside the fetch: the close below ends the copy
				outR.Close()
				<-copied
			}
		}()
	}
	// quit ends the reader when the caller returns before EOF.
	quit := make(chan struct{})
	defer close(quit)
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-quit:
				return
			}
		}
	}()
	// The hub reaps the watcher itself (wait4), so that one goroutine decides between "exited"
	// and "still reserved" (fetchHardStop). cmd.Wait is never called.
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var deadline <-chan time.Time
	cancelled := ctx.Done()
	stop := func() {
		if deadline == nil {
			stdin.Close()
			deadline = time.After(fetchGrace)
		}
	}
	var runErr error
	recorded := make(chan error, 1)
	for {
		select {
		case err := <-recorded:
			if err != nil {
				runErr = err
				stop()
			} else if deadline == nil {
				io.WriteString(stdin, "go\n")
			}
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			switch f := strings.Fields(line); {
			case len(f) == 2 && f[0] == "gate" && res.Gate == 0:
				res.Gate, _ = strconv.Atoi(f[1])
				if res.Gate <= 0 {
					runErr = fmt.Errorf("fetch watcher: bad report %q", line)
					stop()
				} else {
					w, g := res.Watcher, res.Gate
					go func() { recorded <- record(ctx, w, g) }()
				}
			case len(f) == 2 && f[0] == "exit":
				res.GitStatus, _ = strconv.Atoi(f[1])
			}
		case <-cancelled:
			cancelled = nil
			runErr = ctx.Err()
			stop()
		case <-deadline:
			res.Stopped = true
			res.WatcherStatus = fetchHardStop(res.Watcher, res.Gate)
			return res, runErr
		case <-tick.C:
			var ws syscall.WaitStatus
			if pid, err := syscall.Wait4(res.Watcher, &ws, syscall.WNOHANG, nil); pid == res.Watcher {
				res.WatcherStatus = ws
				if lines == nil {
					lines = make(chan string)
					close(lines)
				}
				for line := range lines { // the rest of the output, up to EOF (a nil lines is already drained)
					if f := strings.Fields(line); len(f) == 2 && f[0] == "exit" {
						res.GitStatus, _ = strconv.Atoi(f[1])
					}
				}
				stdin.Close()
				return res, runErr
			} else if err != nil && !errors.Is(err, syscall.EINTR) {
				return res, err
			}
		}
	}
}

// fetchHardStop ends a watcher that did not end within the grace time. It stops the watcher
// first: a stopped process cannot exit, so while it is not exited, its gate (an unreaped child)
// keeps the number gate reserved, and the kill of that group reaches only the fetch.
func fetchHardStop(watcher, gate int) syscall.WaitStatus {
	syscall.Kill(watcher, syscall.SIGSTOP)
	var ws syscall.WaitStatus
	// Wait until it is stopped or exited. SIGSTOP cannot be caught, so one of the two comes.
	for {
		pid, err := syscall.Wait4(watcher, &ws, syscall.WUNTRACED, nil)
		if pid == watcher {
			break
		}
		if err != nil && !errors.Is(err, syscall.EINTR) {
			return ws
		}
	}
	// ws.Stopped() is false for SIGSTOP on BSD systems (Go syscall), so test the stop mark: the
	// low byte 0x7f on macOS and Linux.
	if ws&0xff != 0x7f {
		return ws // it exited before the stop: its own stop rule ran
	}
	if gate > 0 {
		syscall.Kill(-gate, syscall.SIGKILL)
	}
	syscall.Kill(-watcher, syscall.SIGKILL)
	// A stop report can still come before the kill (observed on macOS): wait for the end itself.
	for {
		pid, err := syscall.Wait4(watcher, &ws, 0, nil)
		if pid == watcher && (ws.Exited() || ws.Signaled()) {
			return ws
		}
		if err != nil && !errors.Is(err, syscall.EINTR) {
			return ws
		}
	}
}

// fetchChildMain runs the role that fetchRoleEnv names and exits. It returns when the variable is
// not set.
func fetchChildMain() {
	switch os.Getenv(fetchRoleEnv) {
	case "watcher":
		os.Exit(fetchWatcher(os.Args[1:]))
	case "gate":
		os.Exit(fetchGate(os.Args[1:]))
	}
}

func fetchTestStop(point string) {
	if os.Getenv(fetchStopEnv) == point {
		syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	}
}

// fetchWatcher is the watcher role. Exit codes: 0 git exited 0; 1 git failed; 2 stopped by
// stdin EOF; 3 a start error.
func fetchWatcher(argv []string) int {
	self, err := fetchSelf()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch watcher:", err)
		return 3
	}
	goR, goW, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch watcher:", err)
		return 3
	}
	gate := exec.Command(self, argv...)
	gate.Env = append(os.Environ(), fetchRoleEnv+"=gate") // keeps fetchOutEnv for the gate
	gate.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	gate.ExtraFiles = []*os.File{goR}
	var out *os.File
	if os.Getenv(fetchOutEnv) != "" {
		out = os.NewFile(3, "fetch-stdout") // the hub's pipe: fd 4 of the gate
		gate.ExtraFiles = append(gate.ExtraFiles, out)
	}
	gate.Stderr = os.Stderr
	report, err := gate.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch watcher:", err)
		return 3
	}
	if err := gate.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fetch watcher:", err)
		return 3
	}
	goR.Close()
	if out != nil {
		out.Close() // the gate and git hold it now
	}
	g := gate.Process.Pid
	// end kills G. It never reaps the gate, so the number g stays reserved until the watcher
	// exits (see Ownership).
	end := func(code int) int {
		syscall.Kill(-g, syscall.SIGKILL)
		return code
	}
	fetchTestStop("before-report")
	fmt.Printf("gate %d\n", g)
	hub := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			hub <- sc.Text()
		}
		close(hub) // EOF: the hub closed the pipe or died
	}()
	gitExit := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(report)
		for sc.Scan() {
			if f := strings.Fields(sc.Text()); len(f) == 2 && f[0] == "exit" {
				n, _ := strconv.Atoi(f[1])
				gitExit <- n
				return
			}
		}
	}()
	for {
		select {
		case line, ok := <-hub:
			if !ok {
				code := end(2)
				fetchTestStop("before-exit")
				return code
			}
			if line == "go" {
				goW.Write([]byte{'g'}) // goW stays open: its close tells the gate that the watcher ended
				fetchTestStop("after-go")
			}
		case n := <-gitExit:
			code := end(1)
			if n == 0 {
				code = 0
			}
			fmt.Printf("exit %d\n", n)
			fetchTestStop("before-exit")
			return code
		}
	}
}

// fetchGate is the gate role: it starts git (argv) in its own group after the go byte on fd 3,
// and kills its group when fd 3 reaches EOF.
func fetchGate(argv []string) int {
	ctl := os.NewFile(3, "fetch-go")
	b := make([]byte, 1)
	if n, _ := ctl.Read(b); n != 1 {
		return 3 // no go: the watcher ended before the hub recorded the group; git never runs
	}
	go func() {
		io.Copy(io.Discard, ctl)
		syscall.Kill(0, syscall.SIGKILL) // the watcher ended: end git, its helpers and the gate
	}()
	if len(argv) == 0 {
		fmt.Println("exit 127")
		fetchGateWait()
	}
	git := exec.Command(argv[0], argv[1:]...)
	git.Stdout, git.Stderr = os.Stderr, os.Stderr // the gate's stdout carries only the report
	if os.Getenv(fetchOutEnv) != "" {
		git.Stdout = os.NewFile(4, "fetch-stdout")
	}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, fetchRoleEnv+"=") && !strings.HasPrefix(e, fetchOutEnv+"=") && !strings.HasPrefix(e, fetchStopEnv+"=") {
			git.Env = append(git.Env, e)
		}
	}
	status := 127
	if err := git.Run(); err == nil {
		status = 0
	} else if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		status = ee.ExitCode()
	}
	fmt.Printf("exit %d\n", status)
	fetchGateWait()
	return status
}

// fetchGateWait waits for the kill of the group, by the watcher or by the EOF rule.
func fetchGateWait() {
	for {
		time.Sleep(time.Hour)
	}
}
