package main

// The pane script of an agent batch, and the tool side of its "until" items. An until item repeats
// in the same pane: the same caller, the same args file and key (a replay). After each attempt the
// pane waits for the tool's decision: again (with the seconds left), next, or stop. The window
// covers all attempts of the item and the waits between them.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// relUntilWindow is the time that an until item has for all its attempts.
var relUntilWindow = 60 * time.Second

// batchScript is the pane script of an agent batch in dir: it waits for the go file, joins as
// relSeedAgent through the hub client, then runs each call through the agent face.
func batchScript(dir, server, hubClient string, calls []relSetItem) string {
	rexDo := func(timeout, client string) string {
		return fmt.Sprintf("rex -S %s --autostart=false --timeout %s -C %s do", shQuote(server), timeout, shQuote(client))
	}
	lines := []string{"#!/bin/sh",
		"cd " + shQuote(dir),
		`printf '\033]7;file://localhost%s\033\\' "$PWD"`, // the pane reports its directory (OSC 7)
		"while [ ! -e go ]; do sleep 0.1; done",
		fmt.Sprintf(`%s hub.join --args '{"agent":"%s"}' > join.out 2>&1; echo $? > join.code`, rexDo("60s", hubClient), relSeedAgent)}
	for i, c := range calls {
		if len(c.Until) == 0 {
			lines = append(lines, fmt.Sprintf(`%s %s --args @args-%d.json > out-%d 2>&1; echo $? > code-%d`, rexDo("60s", agentLabel), shQuote(c.Action), i, i, i))
			continue
		}
		w := int(relUntilWindow / time.Second)
		lines = append(lines,
			fmt.Sprintf(`: > start-%d; n=1; t=%d`, i, w),
			`while :; do`,
			fmt.Sprintf(`  %s %s --args @args-%d.json > out-%d-$n 2>&1; echo $? > code-%d-$n.tmp; mv code-%d-$n.tmp code-%d-$n`, rexDo(`"${t}s"`, agentLabel), shQuote(c.Action), i, i, i, i, i),
			fmt.Sprintf(`  while [ ! -e go-%d-$n ]; do sleep 0.1; done`, i),
			fmt.Sprintf(`  read v t < go-%d-$n`, i),
			`  [ "$v" = again ] || break`,
			`  n=$((n+1)); sleep 1`,
			`done`,
			`[ "$v" = next ] || exit 0`,
			fmt.Sprintf(`cp out-%d-$n out-%d; cp code-%d-$n code-%d`, i, i, i, i))
	}
	return strings.Join(append(lines, "echo done > done", ""), "\n")
}

// untilDecide decides after attempt n of an until item, whose first attempt started at start: an
// error when the reply came after the window; "next" when the reply meets the until expects;
// else "again <seconds>" while the window has time for another call (after the 1 s pause); else
// an error with the last reply. A nonzero exit is a reply that does not meet the expects.
func untilDecide(it relSetItem, n, code int, out []byte, vals map[string]string, start, now time.Time) (string, error) {
	reply := strings.TrimSpace(string(out))
	// A reply after the window does not count, even when it meets the expects.
	if now.Sub(start) > relUntilWindow {
		return "", fmt.Errorf("attempt %d ended %s after the first attempt started, after the window of %s; the last reply (exit %d): %s", n, now.Sub(start).Round(time.Millisecond), relUntilWindow, code, reply)
	}
	// An error reply is not met: the plugin can answer unavailable while it starts, and a replay
	// with the same key is safe.
	miss := fmt.Errorf("exit %d", code)
	if code == 0 {
		miss = checkFilled(out, it.Until, vals)
	}
	if miss == nil {
		return "next", nil
	}
	left := relUntilWindow - now.Sub(start) - time.Second
	if left < time.Second {
		return "", fmt.Errorf("the until expects are not met after %d attempts in %s (%v); the last reply (exit %d): %s", n, relUntilWindow, miss, code, reply)
	}
	return fmt.Sprintf("again %d", int(math.Floor(left.Seconds()))), nil
}

// serveBatch waits until the batch script in dir is done. It answers the attempts of each until
// item on the way, and logs each attempt with say. It returns the outputs of the calls; an until
// item that fails stops the pane script, and serveBatch returns the error.
func serveBatch(dir string, calls []relSetItem, vals map[string]string, say func(string, ...any), deadline time.Time) ([]relCallOut, error) {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	type untilState struct {
		n     int
		start time.Time // the mtime of the start marker, which the script writes before attempt 1
		ended bool
		last  string // the last completed attempt: "attempt n (exit c): reply"
	}
	state := map[int]*untilState{}
	for i, c := range calls {
		if len(c.Until) > 0 {
			state[i] = &untilState{n: 1}
		}
	}
	answer := func(i, n int, v string) error {
		p := filepath.Join(dir, fmt.Sprintf("go-%d-%d", i, n))
		if err := os.WriteFile(p+".tmp", []byte(v+"\n"), 0o600); err != nil {
			return err
		}
		return os.Rename(p+".tmp", p)
	}
	for !exists("done") {
		now := time.Now()
		if now.After(deadline) {
			return nil, relFail("the agent pane did not finish; see %s", dir)
		}
		for i := range calls {
			st := state[i]
			if st == nil || st.ended {
				continue
			}
			c := calls[i]
			if st.start.IsZero() {
				fi, err := os.Stat(filepath.Join(dir, fmt.Sprintf("start-%d", i)))
				if err != nil {
					continue
				}
				st.start = fi.ModTime()
			}
			code := fmt.Sprintf("code-%d-%d", i, st.n)
			if !exists(code) {
				// The window ends while an attempt runs: stop the script after that call, and fail now.
				if now.Sub(st.start) > relUntilWindow {
					answer(i, st.n, "stop")
					last := st.last
					if last == "" {
						last = "no attempt completed"
					}
					return nil, relFail("item %d (%s): the window of %s ended while attempt %d ran; the last reply: %s", i+1, c.Action, relUntilWindow, st.n, last)
				}
				continue
			}
			out := mustReadFile(filepath.Join(dir, fmt.Sprintf("out-%d-%d", i, st.n)))
			n := readCode(filepath.Join(dir, code))
			st.last = fmt.Sprintf("attempt %d (exit %d): %s", st.n, n, strings.TrimSpace(string(out)))
			say("item %d (%s): until attempt %d: exit %d: %s", i+1, c.Action, st.n, n, strings.TrimSpace(string(out)))
			v, err := untilDecide(c, st.n, n, out, vals, st.start, now)
			if err != nil {
				answer(i, st.n, "stop")
				if jc := readCode(filepath.Join(dir, "join.code")); exists("join.code") && jc != 0 {
					return nil, relFail("hub.join exited %d: %s", jc, strings.TrimSpace(string(mustReadFile(filepath.Join(dir, "join.out")))))
				}
				return nil, relFail("item %d (%s): %v", i+1, c.Action, err)
			}
			if err := answer(i, st.n, v); err != nil {
				return nil, err
			}
			if v == "next" {
				st.ended = true
			} else {
				st.n++
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code := readCode(filepath.Join(dir, "join.code")); code != 0 {
		return nil, relFail("hub.join exited %d: %s", code, strings.TrimSpace(string(mustReadFile(filepath.Join(dir, "join.out")))))
	}
	outs := make([]relCallOut, len(calls))
	for i := range calls {
		outs[i] = relCallOut{Code: readCode(filepath.Join(dir, fmt.Sprintf("code-%d", i))), Out: mustReadFile(filepath.Join(dir, fmt.Sprintf("out-%d", i))), Vals: vals}
	}
	return outs, nil
}
