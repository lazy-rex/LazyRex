package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Linux reads /proc (untested on Linux: build-checked only until a Linux lab runs).

var bootTime struct {
	once sync.Once
	sec  int64
	err  error
}

// readBootTime reads btime (the boot time in seconds) from /proc/stat once.
func readBootTime() (int64, error) {
	bootTime.once.Do(func() {
		b, err := os.ReadFile("/proc/stat")
		if err != nil {
			bootTime.err = err
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "btime "); ok {
				bootTime.sec, bootTime.err = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				return
			}
		}
		bootTime.err = errors.New("/proc/stat has no btime line")
	})
	return bootTime.sec, bootTime.err
}

// procRead reads the parent pid and the start time of pid from /proc/<pid>/stat. A missing file
// is a verified answer: errProcGone.
func procRead(pid int) (procRecord, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, fs.ErrNotExist) {
		return procRecord{}, errProcGone
	}
	if err != nil {
		return procRecord{}, err
	}
	bt, err := readBootTime()
	if err != nil {
		return procRecord{}, err
	}
	return parseProcStat(string(b), bt)
}

// procArgv returns the command line of pid from /proc/<pid>/cmdline: the exact arguments.
func procArgv(pid int) ([]string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), nil
}
