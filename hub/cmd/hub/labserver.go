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
	srv, hub            *kept // each under its keeper (labkeeper.go)
	runner              *kept // the current runner: a snapshot root while it lives
	r                   *rex.Rex
	tracked             map[int]string               // pid -> start time: every process seen under the server or the hub
	args                map[string]map[string]string // plugin args after expansion (manifest test.args)
	watchRoots          map[string][]string          // plugin watch roots after expansion (manifest test.watch_roots)
	profiles            string                       // LAB/profiles.json, the hub's profile file
	restarts            int                          // hub:restart() calls (restartHub)
	unproved            []string                     // stop: why the cleanup is not proved (a survivor, a keeper)
}

// home is the lab HOME, the working directory of every lab process.
func (l *lab) home() string { return filepath.Join(l.dir, "home") }

func newNonce() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// startServer starts the Rex server of the lab in l.dir (rex.sock, state, blocks), with l.env and
// the working directory l.home(), under its keeper (self in the keeper role, labkeeper.go), writes
// the lab proof, and waits until the server answers (10 s).
func (l *lab) startServer(ctx context.Context, self, rexBin string) error {
	// Lab panes inherit REX_SERVER from the server. Without it, a hook in a lab pane uses Rex's
	// default endpoint, which is the user's real server (hook.go; the hub lead, 2026-10-22). The
	// keeper gives the server l.env without the keeper's own variables (keeperEnv).
	if err := labServerEnv(l.env, l.server); err != nil {
		return err
	}
	logf, err := os.Create(filepath.Join(l.dir, "server.log"))
	if err != nil {
		return err
	}
	l.srv, err = startKept(self, "lab server", []string{rexBin, "-v", "server", "run", "--listen", l.server,
		"--state-dir", filepath.Join(l.dir, "state"), "--block-dir", filepath.Join(l.dir, "blocks"), "--config", "/dev/null"},
		l.env, l.home(), logf, logf)
	if err != nil {
		return err
	}
	l.trackKept(l.srv)
	proof, _ := json.Marshal(map[string]any{"socket": l.server, "server_pid": l.srv.pid, "nonce": newNonce()})
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
		case <-l.srv.done:
			return fmt.Errorf("lab server exited: %v (see %s)", l.srv.status, filepath.Join(l.dir, "server.log"))
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

// trackKept records a kept command and its keeper. The command is a snapshot root.
func (l *lab) trackKept(k *kept) {
	if k.start != "" {
		l.tracked[k.pid] = k.start
	}
	if s := startTime(k.w.pid); s != "" {
		l.tracked[k.w.pid] = s
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
	for _, k := range []*kept{l.srv, l.hub, l.runner} {
		if k == nil {
			continue
		}
		pid := k.pid
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

// stop retires the keepers of the runner, the hub and the server, in that order, and checks that
// every tracked process is gone. A keeper's problem or a process that survives makes the cleanup
// fail (l.unproved); a survivor gets no signal.
func (l *lab) stop() []string {
	var problems []string
	l.snapshot()
	if l.r != nil {
		l.r.Close()
	}
	for _, k := range []*kept{l.runner, l.hub, l.srv} {
		if k == l.srv {
			l.snapshot()
		}
		p := k.stop()
		problems = append(problems, p...)
		l.unproved = append(l.unproved, p...)
	}
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
				p := fmt.Sprintf("cleanup: process survived the lab server: pid %d, start %s, command %s (not signaled)",
					pid, l.tracked[pid], strings.Join(argv, " "))
				problems = append(problems, p)
				l.unproved = append(l.unproved, p)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return problems
}

// labServerEnv checks the environment of a lab server: exactly one REX_SERVER, the lab's own
// endpoint, and no REX_SESSION, REX_BLOCK or REX_CLIENT of the pane that started the test.
func labServerEnv(env []string, server string) error {
	var servers []string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "REX_SERVER":
			servers = append(servers, v)
		case "REX_SESSION", "REX_BLOCK", "REX_CLIENT":
			return fmt.Errorf("the lab server environment has %s", k)
		}
	}
	if len(servers) != 1 || servers[0] != server {
		return fmt.Errorf("the lab server environment must set REX_SERVER=%s once (lab panes inherit it), got %q", server, servers)
	}
	return nil
}
