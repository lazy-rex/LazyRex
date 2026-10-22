package main

// The lab Rex server of lazyrex-hub test: one isolated server in its own directory, the lab proof
// (lab.json, checked by checkLabProof), and the processes that were started on it, tracked by pid
// and start time and stopped without a signal to any other process. Moved from testcmd.go without
// a change in behavior, so that lazyrex-hub release rehearse can use the same code.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// lab is one isolated Rex server and the processes that the harness started on it.
type lab struct {
	dir, socket, server string
	env                 []string
	srv, hub            *exec.Cmd
	runner              *exec.Cmd // the current runner: a snapshot root while it lives
	srvW, hubW          *waiter
	srvExit, hubExit    chan error
	r                   *rex.Rex
	tracked             map[int]string               // pid -> start time: every process seen under the server or the hub
	args                map[string]map[string]string // plugin args after expansion (manifest test.args)
	watchRoots          map[string][]string          // plugin watch roots after expansion (manifest test.watch_roots)
	profiles            string                       // LAB/profiles.json, the hub's profile file
	restarts            int                          // hub:restart() calls (restartHub)
}

// home is the lab HOME, the working directory of every lab process.
func (l *lab) home() string { return filepath.Join(l.dir, "home") }

func newNonce() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// startServer starts the Rex server of the lab in l.dir (rex.sock, state, blocks), with l.env and
// the working directory l.home(), writes the lab proof, and waits until the server answers (10 s).
func (l *lab) startServer(ctx context.Context, rexBin string) error {
	logf, err := os.Create(filepath.Join(l.dir, "server.log"))
	if err != nil {
		return err
	}
	l.srv = exec.Command(rexBin, "-v", "server", "run", "--listen", l.server, "--state-dir", filepath.Join(l.dir, "state"),
		"--block-dir", filepath.Join(l.dir, "blocks"), "--config", "/dev/null")
	l.srv.Env, l.srv.Dir, l.srv.Stdout, l.srv.Stderr = l.env, l.home(), logf, logf
	l.srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := l.srv.Start(); err != nil {
		return err
	}
	l.track(l.srv.Process.Pid)
	l.srvW = watch(l.srv)
	l.srvExit = l.srvW.done
	proof, _ := json.Marshal(map[string]any{"socket": l.server, "server_pid": l.srv.Process.Pid, "nonce": newNonce()})
	if err := os.WriteFile(filepath.Join(l.dir, "lab.json"), proof, 0o600); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(l.socket); err == nil {
			dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			r, err := rex.Dial(dctx, l.socket)
			cancel()
			if err == nil {
				l.r = r
				return nil
			}
		}
		select {
		case err := <-l.srvExit:
			l.srvExit = nil
			return fmt.Errorf("lab server exited: %v (see %s)", err, filepath.Join(l.dir, "server.log"))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.New("lab server: not ready after 10 s")
		}
	}
}

// waiter reaps one started process. It reaps only while it holds mu, and signal holds mu too. So
// a process that signal finds unreaped still owns its pid, and its process group ID (the same
// number) cannot belong to another process. The harness never calls exec.Cmd.Wait for it.
type waiter struct {
	pid    int
	mu     sync.Mutex
	reaped bool
	done   chan error // the exit status, sent once
}

func watch(c *exec.Cmd) *waiter {
	w := &waiter{pid: c.Process.Pid, done: make(chan error, 1)}
	go func() {
		for {
			w.mu.Lock()
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(w.pid, &ws, syscall.WNOHANG, nil)
			if err == syscall.EINTR {
				w.mu.Unlock()
				continue
			}
			if pid == w.pid || err != nil {
				w.reaped = true
				w.mu.Unlock()
				switch {
				case err != nil:
					w.done <- err
				case ws.Signaled():
					w.done <- fmt.Errorf("signal: %v", ws.Signal())
				case ws.ExitStatus() != 0:
					w.done <- fmt.Errorf("exit status %d", ws.ExitStatus())
				default:
					w.done <- nil
				}
				return
			}
			w.mu.Unlock()
			time.Sleep(25 * time.Millisecond)
		}
	}()
	return w
}

// signal sends sig to the process group of the process. It sends nothing and returns false when
// the process is already reaped: its group ID can then name an unrelated group.
func (w *waiter) signal(sig syscall.Signal) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reaped {
		return false
	}
	syscall.Kill(-w.pid, sig)
	return true
}

func (l *lab) track(pid int) {
	if s := startTime(pid); s != "" {
		l.tracked[pid] = s
	}
}

// snapshot records every live descendant of the server and the hub (lab panes are the server's
// children, in their own process groups: observed on Rex 0.0.1792158932-g88fd41, lab).
func (l *lab) snapshot() {
	out, err := exec.Command("ps", "-eo", "pid=,ppid=").Output() // -eo: the same list with BSD and procps ps
	if err != nil {
		return
	}
	children := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		children[ppid] = append(children[ppid], pid)
	}
	// A root is a process that the harness started, in the lifetime that it recorded at the start.
	// A pid that now belongs to another process is not a root. A descendant is claimed only when
	// its parent, checked now, is a claimed process: a pid in the old ps listing can be reused.
	type proc struct {
		pid   int
		start string
	}
	var roots []proc
	for _, c := range []*exec.Cmd{l.srv, l.hub, l.runner} {
		if c == nil || c.Process == nil {
			continue
		}
		pid := c.Process.Pid
		if s, ok := l.tracked[pid]; ok && s != "" && startTime(pid) == s {
			roots = append(roots, proc{pid, s})
		}
	}
	for len(roots) > 0 {
		p := roots[0]
		roots = roots[1:]
		for _, c := range children[p.pid] {
			ppid, s, ok := procInfo(c)
			if !ok || ppid != p.pid || startTime(p.pid) != p.start {
				continue // gone, or not a child of the claimed process any more
			}
			l.tracked[c] = s
			roots = append(roots, proc{c, s})
		}
	}
}

// stop ends the hub, then the server, and checks that every tracked process is gone. A process
// that survives makes the cleanup fail; it gets no signal.
func (l *lab) stop() []string {
	var problems []string
	l.snapshot()
	if l.r != nil {
		l.r.Close()
	}
	// A leader that already exited gets no group signal (waiter.signal). Its group members are in
	// tracked; a member that survives fails the cleanup below.
	stopGroup := func(w *waiter, exit chan error, what string) {
		if w == nil || exit == nil {
			return
		}
		w.signal(syscall.SIGTERM)
		select {
		case <-exit:
		case <-time.After(10 * time.Second):
			problems = append(problems, what+" did not stop in 10 s after SIGTERM; sent SIGKILL")
			w.signal(syscall.SIGKILL)
			<-exit
		}
	}
	stopGroup(l.hubW, l.hubExit, "hub")
	l.snapshot()
	stopGroup(l.srvW, l.srvExit, "lab server")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var alive []int
		for pid, start := range l.tracked {
			if startTime(pid) == start {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 {
			break
		}
		if time.Now().After(deadline) {
			// The harness sends a survivor no signal: between a lifetime check and kill() its pid
			// can be reused, and macOS has no process handle to close that gap. A survivor is a
			// harness bug. The run fails and names it.
			sort.Ints(alive)
			for _, pid := range alive {
				argv, _ := procArgv(pid)
				problems = append(problems, fmt.Sprintf("cleanup: process survived the lab server: pid %d, start %s, command %s (not signaled)",
					pid, l.tracked[pid], strings.Join(argv, " ")))
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return problems
}
