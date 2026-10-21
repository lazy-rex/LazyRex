package main

// The per-call line of Hub.answer (docs/hub.md, "Process log"). Every call gets a line with its
// duration, except a fast, successful routine read: those are counted, and one summary line a
// minute gives the counts and the slowest call. The live Messaging plugin makes tens of thousands
// of store reads a minute, and a line for each made the log grow by about 1 GB.

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// slowCall: a routine read that takes this long or longer keeps its line. Store reads take
// milliseconds and plugin deadlines are 3 s, so a slow line shows a hub that slows down early.
const slowCall = 250 * time.Millisecond

// summaryEvery is the interval of the summary line for skipped calls.
const summaryEvery = time.Minute

// routineReads are the actions whose fast, successful calls get no line. Every other action keeps
// its line, also a future one: a call that changes state for another party (admin, register,
// spawn, acting as the user) is the only process-side record of who called what.
var routineReads = map[string]bool{"hub.store.get": true, "hub.store.scan": true}

type callLog struct {
	mu      sync.Mutex
	now     func() time.Time
	print   func(string)
	since   time.Time // start of the current summary window
	skipped map[string]*skipStat
}

type skipStat struct {
	n   int
	max time.Duration
}

func newCallLog() *callLog {
	return &callLog{now: time.Now, print: func(s string) { log.Print(s) }}
}

// record writes the line of one answered call, or counts it as skipped. callErr is the dispatch
// error (a refusal or a failure) and replyErr the reply delivery error. d is the time from the
// start of the dispatch until the reply was sent. A nil callLog writes every line.
func (c *callLog) record(action, client string, pid int, callErr, replyErr error, d time.Duration) {
	line := fmt.Sprintf("%s from %s pid %d -> %v in %s", action, client, pid, callErr, ms(d))
	if c == nil {
		log.Print(line)
		return
	}
	c.mu.Lock()
	now := c.now()
	if c.since.IsZero() {
		c.since = now
	}
	var summary string
	if now.Sub(c.since) >= summaryEvery {
		summary = c.summaryLocked(now)
	}
	skip := routineReads[action] && callErr == nil && replyErr == nil && d < slowCall
	if skip {
		if c.skipped == nil {
			c.skipped = map[string]*skipStat{}
		}
		s := c.skipped[action]
		if s == nil {
			s = &skipStat{}
			c.skipped[action] = s
		}
		s.n++
		if d > s.max {
			s.max = d
		}
	}
	c.mu.Unlock()
	if summary != "" {
		c.print(summary)
	}
	if !skip {
		c.print(line)
	}
}

// summaryLocked returns the summary of the window that ends now ("" without skipped calls) and
// starts a new window. c.mu must be held.
func (c *callLog) summaryLocked(now time.Time) string {
	defer func() { c.since, c.skipped = now, nil }()
	if len(c.skipped) == 0 {
		return ""
	}
	actions := make([]string, 0, len(c.skipped))
	for a := range c.skipped {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	parts := make([]string, 0, len(actions))
	for _, a := range actions {
		s := c.skipped[a]
		parts = append(parts, fmt.Sprintf("%s %d (max %s)", a, s.n, ms(s.max)))
	}
	return fmt.Sprintf("routine calls without a line since %s: %s", c.since.Format("15:04:05"), strings.Join(parts, ", "))
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}
