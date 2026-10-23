package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// parseProcStat counts the fields after the last ')': the command name can hold spaces and
// parentheses. starttime is in USER_HZ ticks since boot.
func TestParseProcStat(t *testing.T) {
	rest := " S 4242 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 12345 1000 10 18446744073709551615"
	for _, name := range []string{"(sh)", "(a b) c)", "((x))"} {
		r, err := parseProcStat("77 "+name+rest, 1700000000)
		if err != nil || r.ppid != 4242 || r.start != "1700000123.450000" || r.tpgid != -1 {
			t.Errorf("%s: %+v, %v", name, r, err)
		}
	}
	for _, bad := range []string{"77 sh S 1", "77 (sh) S 1 2", "77 (sh) S x" + rest[4:]} {
		if _, err := parseProcStat(bad, 0); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// Linux start times have a 10 ms resolution, so two processes can have equal start times. Equal
// is not "before", in both orders: arrival order decides (identity.go, the stale-report rule).
func TestStartBeforeEqualLinuxTicks(t *testing.T) {
	a, _ := parseProcStat("1 (a) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 500 0 0 0", 1700000000)
	b, _ := parseProcStat("2 (b) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 500 0 0 0", 1700000000)
	if a.start == "" || a.start != b.start {
		t.Fatalf("starts %q %q", a.start, b.start)
	}
	if startBefore(a.start, b.start) || startBefore(b.start, a.start) {
		t.Errorf("equal start times %s: one is before the other", a.start)
	}
}

// procRead gives this process, and errProcGone (a verified answer, not a failed read) for a pid
// that no process has.
func TestProcRead(t *testing.T) {
	r, err := procRead(os.Getpid())
	if err != nil || r.ppid != os.Getppid() || r.start == "" {
		t.Errorf("self: %+v, %v", r, err)
	}
	if _, err := procRead(0x7ffffff0); !errors.Is(err, errProcGone) {
		t.Errorf("a missing pid: %v", err)
	}
	if !argvHas(os.Getpid(), "-test.run="+testRunArg()) && !argvHas(os.Getpid(), os.Args[0]) {
		t.Errorf("argvHas does not find this process's own argument %s", os.Args[0])
	}
}

func testRunArg() string {
	for _, a := range os.Args {
		if v, ok := strings.CutPrefix(a, "-test.run="); ok {
			return v
		}
	}
	return ""
}
