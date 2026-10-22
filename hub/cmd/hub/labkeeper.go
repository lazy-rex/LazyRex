package main

// The lab keeper (docs/testing.md, "Lab directories"). lazyrex-hub test starts each lab command (the
// lab server, the hub, the runner) under a keeper: this binary with LAZYREX_LAB_KEEPER=1 and the
// command's argv as its arguments. The keeper leads its own process group and starts the command
// in that group. A kill of the test process (SIGKILL, an agent tool's time limit) closes the
// control pipe, and the keeper then stops the whole group: before the keeper, the server with its
// panes, the hub and the runner ran on under parent 1 (2026-10-22).
//
// Two pipes connect the test process and the keeper (exec.Cmd.ExtraFiles):
//   - fd 3, control (the test process writes): "start" once, then "stop" or EOF.
//   - fd 4, report (the keeper writes), one line of text each: "child <pid> <start time>" or
//     "error <text>"; then "exit <status>" when the command exits; then "stopped" or
//     "escalate <reason>".
//
// The keeper creates nothing before "start" (exit 125), and exits 126 when the command does not
// start. At "stop", or at EOF without "stop", it sends SIGTERM to its group and waits until no other
// member is left (grace 10 s). Then it writes "stopped" and exits 0. When members remain, or a
// listing of the group fails, it writes "escalate <reason>" and sends SIGKILL to its group, itself
// included. So the command's status comes only from the "exit" line, and the keeper's own status
// means only a clean stop (0) or an escalation (SIGKILL). Lab panes are the server's children in
// their own groups: they are outside the keeper's group, and lab.stop checks them as survivors.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// labKeeperEnv selects the keeper role (value "1"). main checks it first.
	labKeeperEnv = "LAZYREX_LAB_KEEPER"
	// labKeeperGraceEnv is test-only: the keeper's grace in ms (50..10000). Only a keeper reads it.
	labKeeperGraceEnv = "LAZYREX_LAB_KEEPER_GRACE_MS"
	labKeeperGrace    = 10 * time.Second
	// keeperNoStart: the control pipe gave no "start" line; the keeper created nothing.
	keeperNoStart = 125
	// keeperStartFailed: the command did not start.
	keeperStartFailed = 126
)

// labKeeper runs the keeper role and never returns: runKeeper returns only an exit status, and its
// escalation blocks until the group SIGKILL ends the process. main calls it first, so no hub code
// runs in a keeper. Its arguments are the command's argv.
func labKeeper() {
	os.Exit(runKeeper(os.NewFile(3, "control"), os.NewFile(4, "report"), os.Args[1:], keeperGrace(os.Getenv(labKeeperGraceEnv))))
}

// keeperGrace is the grace of a keeper whose grace variable is v: 10 s, or v ms within 50..10000.
func keeperGrace(v string) time.Duration {
	ms, err := strconv.Atoi(v)
	if err != nil {
		return labKeeperGrace
	}
	return time.Duration(min(max(ms, 50), 10000)) * time.Millisecond
}

// keeperEnv is the command's environment: the keeper's without the keeper variables, so that a
// command that is this binary (the hub) does not start as a second keeper.
func keeperEnv(env []string) []string {
	var out []string
	for _, e := range env {
		if !strings.HasPrefix(e, labKeeperEnv+"=") && !strings.HasPrefix(e, labKeeperGraceEnv+"=") {
			out = append(out, e)
		}
	}
	return out
}

// keeperReport writes report lines. A failed write is recorded and never stops the keeper's work:
// a test process that is gone cannot read, and the group must still be stopped.
type keeperReport struct {
	mu sync.Mutex
	f  *os.File
}

func (r *keeperReport) line(s string) bool {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.f.WriteString(s + "\n")
	return err == nil
}

func runKeeper(ctl, rep *os.File, argv []string, grace time.Duration) int {
	// The command must not hold the keeper's pipes: a held control pipe would hide the test
	// process's death, and a held report pipe would hide the keeper's end.
	syscall.CloseOnExec(int(ctl.Fd()))
	syscall.CloseOnExec(int(rep.Fd()))
	// A handler, not an ignore: the keeper survives its own group's SIGTERM, and exec resets a
	// handled signal to its default, so the command does not inherit an ignored SIGTERM.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	in := bufio.NewReader(ctl)
	if line, err := in.ReadString('\n'); err != nil || line != "start\n" {
		return keeperNoStart
	}
	r := &keeperReport{f: rep}
	if len(argv) == 0 {
		r.line("error no command")
		return keeperStartFailed
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = keeperEnv(os.Environ())
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		r.line("error " + err.Error())
		return keeperStartFailed
	}
	// The keeper lives until the stop: it must not hold the command's output (the runner's result
	// pipe ends only when every writer closes it).
	if null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		for fd := 0; fd <= 2; fd++ {
			unix.Dup2(int(null.Fd()), fd)
		}
		null.Close()
	}
	reported := r.line(fmt.Sprintf("child %d %s", cmd.Process.Pid, startTime(cmd.Process.Pid)))
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		r.line("exit " + exitText(cmd.ProcessState))
		close(exited)
	}()
	if reported {
		// The control pipe is read only after the command was created: a stop never runs before a
		// late create. Any line but "stop" is ignored.
		for {
			line, err := in.ReadString('\n')
			if err != nil || line == "stop\n" {
				break
			}
		}
	}
	syscall.Kill(0, syscall.SIGTERM)
	reason := ""
	for end := time.Now().Add(grace); ; time.Sleep(100 * time.Millisecond) {
		members, err := groupMembers()
		if err != nil {
			reason = "listing failed: " + err.Error()
			break
		}
		if len(members) == 0 {
			break
		}
		if time.Now().After(end) {
			reason = fmt.Sprintf("members remain after %s: %v", grace, members)
			break
		}
	}
	if reason == "" {
		// The group is empty, so the command is reaped; its exit line follows at once. A command
		// that left the group (setsid) still runs.
		select {
		case <-exited:
			r.line("stopped")
			return 0
		case <-time.After(grace):
			reason = "the command left the group and still runs"
		}
	}
	r.line("escalate " + reason)
	syscall.Kill(0, syscall.SIGKILL)
	select {} // the SIGKILL ends the keeper too
}

// exitText is the status of an "exit" line: "0", "3", or "signal terminated".
func exitText(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return "signal " + ws.Signal().String()
	}
	return strconv.Itoa(ps.ExitCode())
}

// groupMembers lists the other processes of the keeper's group: "ps -eo pid=,pgid=" (the same with
// BSD and procps ps), filtered here. The ps process is in the group too; it is excluded by its pid,
// and Output reaps it first. A listing that fails, or that does not list the keeper, is an error:
// a failure never means "empty".
func groupMembers() ([]int, error) {
	ps := exec.Command("ps", "-eo", "pid=,pgid=")
	out, err := ps.Output()
	if err != nil {
		return nil, err
	}
	self, group := os.Getpid(), syscall.Getpgrp()
	seen := false
	var members []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pgid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("ps line %q", line)
		}
		switch {
		case pid == self:
			seen = true
		case pgid == group && pid != ps.Process.Pid:
			members = append(members, pid)
		}
	}
	if !seen {
		return nil, errors.New("ps did not list the keeper")
	}
	return members, nil
}

// errKeeperNoReport is the command's status when the report ended without an "exit" line: the
// keeper died from outside, or its escalation killed the command.
var errKeeperNoReport = errors.New("the command's status is unknown: the keeper ended without a report")

// kept is a lab command under its keeper, seen from lazyrex-hub test. pid and start are the
// command's (the lab server's pid goes to lab.json); w reaps the keeper, the leader of the group.
type kept struct {
	what     string
	w        *waiter
	ctl      *os.File
	grace    time.Duration
	pid      int
	start    string
	done     chan struct{} // closed when status is known
	status   error         // the "exit" line, or errKeeperNoReport
	reported bool          // an "exit" line came
	ended    chan struct{} // closed at the end of the report
	end      string        // "stopped", "escalate <reason>" or ""; read after ended
	retired  bool
	logf     func(format string, args ...any)
}

// startKept starts argv under a keeper (self in the keeper role) in a new process group, and waits
// up to 10 s for the command's pid. env is the command's environment; the keeper gets it with its
// role added. A keeper that does not report the command is stopped: its control pipe closes.
func startKept(self, what string, argv, env []string, dir string, stdout, stderr io.Writer) (*kept, error) {
	cr, cw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	rr, rw, err := os.Pipe()
	if err != nil {
		cr.Close()
		cw.Close()
		return nil, err
	}
	c := exec.Command(self, argv...)
	c.Env = append(append([]string{}, env...), labKeeperEnv+"=1")
	c.Dir, c.Stdout, c.Stderr = dir, stdout, stderr
	c.ExtraFiles = []*os.File{cr, rw}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = c.Start()
	cr.Close()
	rw.Close()
	if err != nil {
		cw.Close()
		rr.Close()
		return nil, fmt.Errorf("%s keeper: %w", what, err)
	}
	k := &kept{what: what, w: watch(c), ctl: cw, grace: labKeeperGrace, done: make(chan struct{}), ended: make(chan struct{}), logf: log.Printf}
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, labKeeperGraceEnv+"="); ok {
			k.grace = keeperGrace(v)
		}
	}
	rd := bufio.NewReader(rr)
	cw.WriteString("start\n")
	rr.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := rd.ReadString('\n')
	rr.SetReadDeadline(time.Time{})
	f := strings.Fields(line)
	if err == nil && len(f) == 3 && f[0] == "child" {
		if pid, perr := strconv.Atoi(f[1]); perr == nil && pid > 0 {
			k.pid, k.start = pid, f[2]
			go k.read(rd, rr)
			return k, nil
		}
	}
	// No command to track. The closed control pipe makes the keeper stop its group.
	cw.Close()
	rr.Close()
	k.retired = true
	select {
	case <-k.w.done:
	case <-time.After(k.grace + 5*time.Second):
		k.w.signal(syscall.SIGKILL)
		<-k.w.done
	}
	if msg, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "error "); ok {
		return nil, fmt.Errorf("%s: %s", what, msg)
	}
	return nil, fmt.Errorf("%s keeper: no command report (%q, %v)", what, line, err)
}

// read reads the report after the "child" line until it ends.
func (k *kept) read(rd *bufio.Reader, f io.Closer) {
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "exit "):
			if !k.reported {
				k.reported, k.status = true, exitStatus(strings.TrimPrefix(line, "exit "))
				close(k.done)
			}
		case line == "stopped" || strings.HasPrefix(line, "escalate "):
			k.end = line
		default:
			k.logf("lab keeper of the %s: unknown report line %q (ignored)", k.what, line)
		}
	}
	if f != nil {
		f.Close()
	}
	if !k.reported {
		k.status = errKeeperNoReport
		close(k.done)
	}
	close(k.ended)
}

// exitStatus is the error form of an "exit" line's status, as waiter gives it: nil for 0.
func exitStatus(s string) error {
	if sig, ok := strings.CutPrefix(s, "signal "); ok {
		return fmt.Errorf("signal: %s", sig)
	}
	if s == "0" {
		return nil
	}
	return fmt.Errorf("exit status %s", s)
}

// stop retires the keeper: "stop", then the wait for the keeper (its grace + 5 s; then SIGKILL to
// its group). It returns the problems: an escalation, a keeper that did not end, or a command whose
// status is unknown. Each problem means that the cleanup is not proved.
func (k *kept) stop() []string {
	if k == nil || k.retired {
		return nil
	}
	k.retired = true
	k.ctl.WriteString("stop\n")
	k.ctl.Close()
	var probs []string
	var st error
	select {
	case st = <-k.w.done:
	case <-time.After(k.grace + 5*time.Second):
		probs = append(probs, fmt.Sprintf("the keeper of the %s did not end %s after stop; sent SIGKILL to its group", k.what, k.grace+5*time.Second))
		k.w.signal(syscall.SIGKILL)
		st = <-k.w.done
	}
	select {
	case <-k.ended:
	case <-time.After(5 * time.Second):
		return append(probs, fmt.Sprintf("the report of the %s keeper did not end 5 s after the keeper", k.what))
	}
	switch {
	case k.end == "stopped" && st == nil:
	case strings.HasPrefix(k.end, "escalate "):
		probs = append(probs, fmt.Sprintf("the keeper of the %s sent SIGKILL to its group: %s", k.what, strings.TrimPrefix(k.end, "escalate ")))
	default:
		probs = append(probs, fmt.Sprintf("the keeper of the %s ended without a clean stop: %v", k.what, st))
	}
	if !k.reported {
		probs = append(probs, fmt.Sprintf("the %s: %v", k.what, errKeeperNoReport))
	}
	return probs
}

// kill retires the keeper with SIGKILL to its whole group (a hub crash, or a runner after its
// timeout). The command's status is then unknown by design. It returns a problem only when the
// keeper is not reaped in 10 s.
func (k *kept) kill() []string {
	if k == nil || k.retired {
		return nil
	}
	k.retired = true
	k.w.signal(syscall.SIGKILL)
	k.ctl.Close()
	select {
	case <-k.w.done:
		return nil
	case <-time.After(10 * time.Second):
		return []string{fmt.Sprintf("the keeper of the %s was not reaped 10 s after SIGKILL", k.what)}
	}
}
