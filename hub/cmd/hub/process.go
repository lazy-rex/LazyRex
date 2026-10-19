package main

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// procInfo returns the parent pid and the start time of pid. ok is false if the process does not exist.
func procInfo(pid int) (ppid int, start string, ok bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid == 0 {
		return 0, "", false
	}
	st := kp.Proc.P_starttime
	return int(kp.Eproc.Ppid), fmt.Sprintf("%d.%06d", st.Sec, st.Usec), true
}

// startTime identifies one process lifetime together with its pid. "" if the process does not exist.
func startTime(pid int) string {
	_, s, ok := procInfo(pid)
	if !ok {
		return ""
	}
	return s
}

// descendsFrom reports whether pid is ancestor or a descendant of ancestor.
// This is attribution, not security: double-fork, setsid and nohup can break the chain.
func descendsFrom(pid, ancestor int) bool {
	if pid <= 0 || ancestor <= 0 {
		return false
	}
	for seen := 0; pid > 1 && seen < 64; seen++ {
		if pid == ancestor {
			return true
		}
		ppid, _, ok := procInfo(pid)
		if !ok {
			return false
		}
		pid = ppid
	}
	return false
}

// argvHas reports whether the command line of pid contains the argument arg.
// The hub uses it to check that a pane runs the plugin instance it launched.
func argvHas(pid int, arg string) bool {
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return false
	}
	for _, f := range strings.Fields(string(out)) {
		if f == arg {
			return true
		}
	}
	return false
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
