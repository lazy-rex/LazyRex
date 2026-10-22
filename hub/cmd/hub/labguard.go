package main

// The lab guard (docs/testing.md, "Lab directories"). lazyrex-hub test starts the lab server, the
// hub and the runner each in its own process group, so a kill of the test process (SIGKILL, an
// agent tool's time limit) used to leave them running: the server with its panes, and the runner
// (observed 2026-10-22: a runner outlived its server). For each lab, the test process also starts
// this guard: the same binary, with the role in the environment, and a pipe from the test process
// on its stdin. The test process writes one line "group <pid> <start time>" for each process group
// that it starts for the lab (lab.track). After its own stop of the lab, the test process writes
// "release" and closes the pipe: the guard ends and signals nothing, so that lab.stop reports a
// survivor and does not signal it. When the pipe ends without "release" (the test process was
// killed), the guard stops each group whose leader is still the same process (pid and start
// time): SIGTERM, then SIGKILL after 10 s. The lab panes are
// the server's children; they end with the server. The guard ends after that, so it never outlives
// its test process by more than the grace, and it runs in "/", so it holds no lab directory open.

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// labGuardEnv selects the guard role (value "1").
const labGuardEnv = "LAZYREX_LAB_GUARD"

// labGuardMain runs the guard role and exits. It returns when the variable is not set.
func labGuardMain() {
	if os.Getenv(labGuardEnv) != "1" {
		return
	}
	os.Exit(labGuard(os.Stdin, 10*time.Second))
}

type labGroup struct {
	pid   int
	start string
}

// labGuard reads the groups from in until EOF, then stops them; after "release" it stops none. A
// guard without a pipe on stdin
// (for example started by hand from a terminal) exits at once and signals nothing.
func labGuard(in *os.File, grace time.Duration) int {
	if fi, err := in.Stat(); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		return 2
	}
	var groups []labGroup
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		if sc.Text() == "release" {
			return 0
		}
		f := strings.Fields(sc.Text())
		if len(f) != 3 || f[0] != "group" {
			continue
		}
		if pid, err := strconv.Atoi(f[1]); err == nil && pid > 1 {
			groups = append(groups, labGroup{pid, f[2]})
		}
	}
	stopLabGroups(groups, grace)
	return 0
}

// stopLabGroups sends SIGTERM to each group whose leader is still the same process, waits up to
// grace, then sends SIGKILL to each that still lives.
func stopLabGroups(groups []labGroup, grace time.Duration) {
	live := func() []labGroup {
		var out []labGroup
		for _, g := range groups {
			if alive(g.pid, g.start) {
				out = append(out, g)
			}
		}
		return out
	}
	for _, g := range live() {
		syscall.Kill(-g.pid, syscall.SIGTERM)
	}
	for end := time.Now().Add(grace); time.Now().Before(end) && len(live()) > 0; {
		time.Sleep(100 * time.Millisecond)
	}
	for _, g := range live() {
		syscall.Kill(-g.pid, syscall.SIGKILL)
	}
}

// guardGroup reports a process group of the lab to its guard. A failed write is ignored: the
// guard then has fewer groups, and the lab's own stop still runs.
func (l *lab) guardGroup(pid int, start string) {
	if l.guardIn != nil && start != "" {
		l.guardIn.Write([]byte("group " + strconv.Itoa(pid) + " " + start + "\n"))
	}
}
