package main

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procRecord is what one process information read gives: the parent pid, the start time and the
// terminal's foreground process group. The start time is "<seconds>.<microseconds>" on every
// system, so stored values and startBefore compare the same way. procRead (process_darwin.go,
// process_linux.go) reads it.
type procRecord struct {
	ppid   int
	start  string
	zombie bool // the process has exited and is not reaped (Linux state Z, darwin SZOMB)
	// tpgid is the foreground process group of the process's controlling terminal: an interactive
	// shell gives the command that it runs in the foreground its own group (lab, 2026-10-23). 0 or
	// less: no terminal.
	tpgid int
}

// errProcGone is procRead's verified answer that the system has no such process. Every other
// error is a failed read: no answer.
var errProcGone = errors.New("no such process")

// procUserHZ is the clock tick rate of /proc/<pid>/stat starttime (USER_HZ). Linux exports
// /proc times in USER_HZ, which is 100 on the usual architectures. So a Linux start time has a
// 10 ms resolution: two processes that start in the same tick have equal start times.
const procUserHZ = 100

// parseProcStat reads the state (field 3), the parent pid (field 4), tpgid (field 8) and starttime (field 22) of a
// /proc/<pid>/stat line. Field 2, the command name in parentheses, can hold spaces and parentheses, so the fields
// are counted after the last ')'. btime is the boot time in seconds.
func parseProcStat(line string, btime int64) (procRecord, error) {
	i := strings.LastIndexByte(line, ')')
	if i < 0 {
		return procRecord{}, fmt.Errorf("stat line without a command name: %q", line)
	}
	f := strings.Fields(line[i+1:]) // f[0] is field 3 (state)
	if len(f) < 20 {
		return procRecord{}, fmt.Errorf("stat line with %d fields after the command name", len(f))
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return procRecord{}, fmt.Errorf("stat ppid %q: %v", f[1], err)
	}
	tpgid, err := strconv.Atoi(f[5])
	if err != nil {
		return procRecord{}, fmt.Errorf("stat tpgid %q: %v", f[5], err)
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return procRecord{}, fmt.Errorf("stat starttime %q: %v", f[19], err)
	}
	sec := btime + ticks/procUserHZ
	usec := (ticks % procUserHZ) * (1000000 / procUserHZ)
	return procRecord{ppid: ppid, start: fmt.Sprintf("%d.%06d", sec, usec), zombie: f[0] == "Z", tpgid: tpgid}, nil
}

// procInfo returns the parent pid and the start time of pid. ok is false if the process does not
// exist or the read failed: use it only where a failed read must count as "not this process".
func procInfo(pid int) (ppid int, start string, ok bool) {
	r, err := procRead(pid)
	if err != nil {
		return 0, "", false
	}
	return r.ppid, r.start, true
}

// startTime identifies one process lifetime together with its pid. "" if the process does not exist.
func startTime(pid int) string {
	_, s, ok := procInfo(pid)
	if !ok {
		return ""
	}
	return s
}

// ancestryOf returns pid and its ancestors, nearest first, until pid 1. Only a walk that reaches
// pid 1 is complete. An error means the walk is incomplete: no caller pid (0 or less), a read
// failed, a process in the chain ended, a parent of 0 or less, or the walk passed 64 processes.
// The pids read before the error are still returned: a match among them is a real match.
func ancestryOf(read func(int) (procRecord, error), pid int) ([]int, error) {
	if pid <= 0 {
		return nil, errors.New("the caller has no process id") // a missing Rex-attested pid decodes to 0
	}
	var chain []int
	for seen := 0; pid != 1; seen++ {
		if seen == 64 {
			return chain, errors.New("the walk passed 64 processes")
		}
		chain = append(chain, pid)
		r, err := read(pid)
		if err != nil {
			return chain, fmt.Errorf("process %d: %w", pid, err)
		}
		if r.ppid <= 0 {
			return chain, fmt.Errorf("process %d has parent %d: the walk does not reach pid 1", pid, r.ppid)
		}
		pid = r.ppid
	}
	return chain, nil
}

// descendsFrom reports whether pid is ancestor or a descendant of ancestor. It is positive
// attribution only: a failed walk gives false, so a caller that refuses on false fails closed.
// A guard that allows on false must use ancestryOf and refuse on its error.
// This is attribution, not security: double-fork, setsid and nohup can break the chain.
func descendsFrom(pid, ancestor int) bool {
	if pid <= 0 || ancestor <= 0 {
		return false
	}
	chain, _ := ancestryOf(procRead, pid)
	return slices.Contains(chain, ancestor)
}

// argvHas reports whether the command line of pid contains the argument arg.
// The hub uses it to check that a pane runs the plugin instance it launched.
func argvHas(pid int, arg string) bool {
	argv, err := procArgv(pid)
	return err == nil && slices.Contains(argv, arg)
}

// alive reports whether pid still runs the same process lifetime.
func alive(pid int, start string) bool {
	return pid > 0 && start != "" && startTime(pid) == start
}

// stopProcess sends SIGTERM, then SIGKILL, and waits until the verified process lifetime is gone.
func stopProcess(pid int, start string) error {
	if !alive(pid, start) {
		return nil
	}
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		if !alive(pid, start) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if alive(pid, start) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	for i := 0; i < 30; i++ {
		if !alive(pid, start) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("pid %d did not exit", pid)
}
