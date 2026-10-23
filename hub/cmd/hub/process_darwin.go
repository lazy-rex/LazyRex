package main

import (
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// procRead reads the parent pid and the start time of pid with one process information read
// (sysctl kern.proc.pid). An empty answer is a verified answer: errProcGone.
func procRead(pid int) (procRecord, error) {
	list, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return procRecord{}, err
	}
	if len(list) == 0 || list[0].Proc.P_pid == 0 {
		return procRecord{}, errProcGone
	}
	st := list[0].Proc.P_starttime
	// p_stat 5 is SZOMB (sys/proc.h): the process has exited and is not reaped.
	return procRecord{ppid: int(list[0].Eproc.Ppid), start: fmt.Sprintf("%d.%06d", st.Sec, st.Usec), zombie: list[0].Proc.P_stat == 5}, nil
}

// procArgvExact returns the exact arguments of pid from kern.procargs2 (parseProcArgs2).
func procArgvExact(pid int) ([]string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return parseProcArgs2(b)
}

// procArgv returns the command line of pid as words (ps; the words split at spaces).
func procArgv(pid int) ([]string, error) {
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
