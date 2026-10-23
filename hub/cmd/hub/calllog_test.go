package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testCallLog() (*callLog, *[]string, *time.Time) {
	var lines []string
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	c := &callLog{now: func() time.Time { return now }, print: func(s string) { lines = append(lines, s) }}
	return c, &lines, &now
}

// Only a fast, successful routine read (hub.sql.query) gets no line. A refused or
// failed call, a failed reply, a slow call, and every other action keep their line, with the
// duration. 250 ms is the first duration that keeps the line.
func TestCallLogKeepsEveryLineButFastRoutineReads(t *testing.T) {
	refused := errors.New("refused")
	for _, c := range []struct {
		action   string
		callErr  error
		replyErr error
		d        time.Duration
		line     bool
	}{
		{"hub.sql.query", nil, nil, 2 * time.Millisecond, false},
		{"hub.sql.query", nil, nil, slowCall - time.Microsecond, false},
		{"hub.sql.query", nil, nil, slowCall, true},
		{"hub.sql.query", nil, nil, time.Second, true},
		{"hub.sql.query", refused, nil, time.Millisecond, true},
		{"hub.sql.query", nil, errors.New("reply lost"), time.Millisecond, true},
		{"hub.sql.exec", nil, nil, time.Millisecond, true},
		{"hub.admin.start", nil, nil, time.Millisecond, true},
		{"hub.admin.shutdown", nil, nil, time.Millisecond, true},
		{"hub.register", nil, nil, time.Millisecond, true},
		{"hub.spawn", nil, nil, time.Millisecond, true},
		{"hub.prompt.answer", nil, nil, time.Millisecond, true},
		{"hub.user.call", nil, nil, time.Millisecond, true},
		{"hub.prompts", nil, nil, time.Millisecond, true},
		{"hub.some.future.action", nil, nil, time.Millisecond, true},
	} {
		cl, lines, _ := testCallLog()
		cl.record(c.action, "client:1", 42, c.callErr, c.replyErr, c.d)
		if got := len(*lines) == 1; got != c.line {
			t.Errorf("%s err=%v reply=%v %s: line %v, want %v", c.action, c.callErr, c.replyErr, c.d, got, c.line)
			continue
		}
		if c.line {
			want := c.action + " from client:1 pid 42 -> " + fmt.Sprint(c.callErr) + " in " + ms(c.d)
			if (*lines)[0] != want {
				t.Errorf("line %q, want %q", (*lines)[0], want)
			}
		}
	}
}

// Skipped calls are counted. The first call one minute or more after the window began writes one
// summary line (counts and the slowest call, actions sorted) and starts a new window. A window
// without skipped calls writes no summary.
func TestCallLogSummary(t *testing.T) {
	c, lines, now := testCallLog()
	c.record("hub.sql.query", "c", 1, nil, nil, 3*time.Millisecond)
	c.record("hub.sql.query", "c", 1, nil, nil, 40*time.Millisecond)
	c.record("hub.sql.query", "c", 1, nil, nil, 12*time.Millisecond)
	*now = now.Add(summaryEvery - time.Millisecond)
	c.record("hub.sql.query", "c", 1, nil, nil, time.Millisecond)
	if len(*lines) != 0 {
		t.Fatalf("lines before a minute: %q", *lines)
	}
	*now = now.Add(time.Millisecond) // exactly one minute after the first call
	c.record("hub.admin.start", "c", 1, nil, nil, time.Millisecond)
	want := "routine calls without a line since 15:00:00: hub.sql.query 4 (max 40.0ms)"
	if len(*lines) != 2 || (*lines)[0] != want || !strings.HasPrefix((*lines)[1], "hub.admin.start ") {
		t.Fatalf("lines %q, want the summary %q and then the call line", *lines, want)
	}
	// The next window had no skipped call: no summary.
	*now = now.Add(2 * summaryEvery)
	c.record("hub.admin.stop", "c", 1, nil, nil, time.Millisecond)
	if len(*lines) != 3 || !strings.HasPrefix((*lines)[2], "hub.admin.stop ") {
		t.Fatalf("lines %q", *lines)
	}
}

// A Hub without a call log (a test hub) writes every line.
func TestCallLogNilWritesEveryLine(t *testing.T) {
	var c *callLog
	c.record("hub.sql.query", "c", 1, nil, nil, time.Millisecond) // must not panic
}
