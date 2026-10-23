package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTimer is a timer of fakeClock: the test runs its function.
type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { t.stopped = true; return true }

// fakeClock is the crash backoff clock of a test hub: now moves only when the test moves it, and
// no timer fires by itself.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*fakeTimer
}

func (c *fakeClock) install(h *Hub) {
	h.now = func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
	h.afterFunc = func(d time.Duration, f func()) stopper {
		c.mu.Lock()
		defer c.mu.Unlock()
		t := &fakeTimer{d: d, f: f}
		c.timers = append(c.timers, t)
		return t
	}
}

func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// last gives the newest timer and the number of timers.
func (c *fakeClock) last(t *testing.T) (*fakeTimer, int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.timers) == 0 {
		t.Fatal("no respawn timer")
	}
	return c.timers[len(c.timers)-1], len(c.timers)
}

func (c *fakeClock) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.timers) }

// respawnHub is a hub without Rex on a config of local plugins. plugins is the config plugins
// field; each manifests key is a plugin directory under the temp dir, with its lazyrex.json. The
// hub has no work session, so a spawn fails before anything goes to Rex (stage session).
func respawnHub(t *testing.T, manifests map[string]string, plugins func(dir string) string) (*Hub, *fakeClock, string) {
	t.Helper()
	dir := cacheTemp(t) // a start publishes a read-only local snapshot under the state
	for name, body := range manifests {
		writePlugin(t, filepath.Join(dir, name), body)
	}
	c, err := writeConfig(t, dir, plugins(dir))
	if err != nil {
		t.Fatal(err)
	}
	h, events := uiTestHub(t)
	h.cfg, h.entries, h.plugins, h.selfClosed = c, c.entries, map[string]*Plugin{}, map[string]bool{}
	h.faults = &faults{}
	h.entriesMu.Lock()
	h.resolveLocked()
	h.entriesMu.Unlock()
	clock := &fakeClock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	clock.install(h)
	return h, clock, events
}

// oneHub is respawnHub with plugin p only.
func oneHub(t *testing.T) (*Hub, *fakeClock, string, *Plugin) {
	h, clock, events := respawnHub(t, map[string]string{"p": `{"name":"p","main":"p.lua"}`},
		func(dir string) string { return `[{"source":"` + filepath.Join(dir, "p") + `"}]` })
	pl := h.plugins["p"]
	if pl == nil || pl.entry == nil || pl.entry.err != nil {
		t.Fatalf("the record of p: %+v", pl)
	}
	return h, clock, events, pl
}

// running records an instance of pl that the hub spawned now, as spawn does: from the published
// run of its candidate, which becomes the selected run of the automatic starts.
func running(t *testing.T, h *Hub, pl *Plugin, instance string) {
	t.Helper()
	h.mu.Lock()
	e := pl.entry
	h.mu.Unlock()
	var run *pluginRun
	if e != nil && e.cand != nil {
		var err error
		if run, err = h.publishLocal(e, e.cand); err != nil {
			t.Fatalf("publish the run of %s: %v", pl.Name, err)
		}
	}
	h.mu.Lock()
	if run != nil {
		// Only the selection is saved: the instance below stays in memory, so a start that reads the
		// row retires nothing (a retirement calls Rex).
		pl.run, pl.selected = *run, &selectedRun{Entry: entryIdentity(e), Run: *run}
		sel, _ := json.Marshal(pl.selected)
		if err := h.ensurePluginRow(context.Background(), pl.Name); err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.Exec(`UPDATE plugins SET selected_run = ? WHERE name = ?`, string(sel), pl.Name); err != nil {
			t.Fatal(err)
		}
	}
	pl.Desired, pl.InstanceID, pl.BlockID, pl.ClientID = "running", instance, "block:"+instance, "client:"+instance
	pl.respawn.spawned = h.clock()
	h.mu.Unlock()
}

// exited sends child_exited for pl's instance.
func exited(h *Hub, pl *Plugin, ctx context.Context, payload string) {
	h.mu.Lock()
	block := pl.BlockID
	h.mu.Unlock()
	h.pluginExited(ctx, pl, block, json.RawMessage(payload))
}

// dropForTest forgets the exited instance, so that the respawn's start does not retire it (a
// retirement calls Rex). The lab tests cover the retirement.
func dropForTest(h *Hub, pl *Plugin) {
	h.mu.Lock()
	pl.clearInstance()
	h.mu.Unlock()
}

func respawnFields(h *Hub, pl *Plugin) (due time.Time, delay time.Duration, gen uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return pl.respawn.due, pl.respawn.delay, pl.respawn.gen
}

// The delays are 1, 2, 4 ... 60 s, then 60 s; an instance that ran 60 s starts them again at 1 s.
// The record keeps the exited instance while crashing.
func TestRespawnDelays(t *testing.T) {
	h, clock, _, pl := oneHub(t)
	ctx := context.Background()
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i, w := range want {
		id := "p-" + string(rune('a'+i))
		running(t, h, pl, id)
		clock.advance(time.Second)
		exited(h, pl, ctx, `{"exit_code":1,"error":"exit status 1"}`)
		tm, n := clock.last(t)
		if tm.d != w*time.Second || n != i+1 {
			t.Fatalf("exit %d: delay %v (%d timers), want %v", i+1, tm.d, n, w*time.Second)
		}
		h.mu.Lock()
		state, inst, block := pl.reportedState(), pl.InstanceID, pl.BlockID
		h.mu.Unlock()
		if state != "crashing" || inst != id || block != "block:"+id {
			t.Fatalf("exit %d: state %s, instance %s, block %s", i+1, state, inst, block)
		}
	}
	running(t, h, pl, "p-long")
	clock.advance(respawnReset)
	exited(h, pl, ctx, `{"exit_code":1}`)
	if tm, _ := clock.last(t); tm.d != respawnFirst {
		t.Fatalf("after a run of 60 s: delay %v", tm.d)
	}
}

// A second child_exited for the same instance counts once: no second timer, the same delay and
// the same deadline (codex-21's edge 1).
func TestRespawnDuplicateExit(t *testing.T) {
	h, clock, events, pl := oneHub(t)
	ctx := context.Background()
	running(t, h, pl, "p-1")
	exited(h, pl, ctx, `{"exit_code":1}`)
	due, delay, gen := respawnFields(h, pl)
	clock.advance(300 * time.Millisecond)
	exited(h, pl, ctx, `{"exit_code":1}`)
	due2, delay2, gen2 := respawnFields(h, pl)
	if clock.count() != 1 || due2 != due || delay2 != delay || gen2 != gen {
		t.Fatalf("after a duplicate: %d timers, due %v -> %v, delay %v -> %v, gen %d -> %d", clock.count(), due, due2, delay, delay2, gen, gen2)
	}
	if n := len(eventsNamed(t, events, "plugin_exited")); n != 1 {
		t.Fatalf("%d plugin_exited events", n)
	}
}

// A stop while crashing cancels the respawn before it saves "stopped", and resets the delay. The
// fired callback starts nothing.
func TestRespawnStopWhileCrashing(t *testing.T) {
	h, clock, events, pl := oneHub(t)
	ctx := context.Background()
	running(t, h, pl, "p-1")
	exited(h, pl, ctx, `{"exit_code":1}`)
	tm, _ := clock.last(t)
	pl.lifecycle.Lock()
	h.markStopped(ctx, pl, "admin stop")
	pl.lifecycle.Unlock()
	if !tm.stopped {
		t.Error("the stop did not stop the timer")
	}
	tm.f()
	h.mu.Lock()
	state, attempted, delay := pl.reportedState(), pl.attempted, pl.respawn.delay
	h.mu.Unlock()
	if state != "stopped" || attempted || delay != 0 {
		t.Fatalf("after the stop: state %s, a start ran %v, delay %v", state, attempted, delay)
	}
	if n := len(eventsNamed(t, events, "plugin_respawn")); n != 0 {
		t.Fatalf("%d respawns after a stop", n)
	}
}

// The callback has fired and waits for pl.lifecycle while a stop holds it (claude-50's condition
// 1, codex-21's edge 4): the stop changes the generation first, so the callback does nothing.
func TestRespawnFiredDuringStop(t *testing.T) {
	h, clock, events, pl := oneHub(t)
	ctx := context.Background()
	running(t, h, pl, "p-1")
	exited(h, pl, ctx, `{"exit_code":1}`)
	tm, _ := clock.last(t)
	pl.lifecycle.Lock() // the stop handler holds the lifecycle lock until markStopped returns
	done := make(chan struct{})
	go func() { tm.f(); close(done) }()
	time.Sleep(50 * time.Millisecond) // the callback now waits for pl.lifecycle
	h.markStopped(ctx, pl, "admin stop")
	pl.lifecycle.Unlock()
	<-done
	h.mu.Lock()
	state, attempted := pl.reportedState(), pl.attempted
	h.mu.Unlock()
	if state != "stopped" || attempted || len(eventsNamed(t, events, "plugin_respawn")) != 0 {
		t.Fatalf("the waiting callback acted after the stop: state %s, a start ran %v", state, attempted)
	}
}

// A start that passes its refusals while crashing cancels the respawn at its first effect and
// keeps the delay; the old callback is a no-op (the race of a start between the timer and
// respawnDue's locks). Both start paths: the plain start and the expected start (expect_run).
func TestRespawnPassingStartCancels(t *testing.T) {
	for _, c := range []struct {
		name  string
		start func(h *Hub, pl *Plugin) error
	}{
		{"start", func(h *Hub, pl *Plugin) error { return h.startPlugin(context.Background(), pl) }},
		{"expect_run", func(h *Hub, pl *Plugin) error {
			return h.startPluginExpect(context.Background(), pl, false, &expectRun{Source: pl.entry.cand.Source})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, clock, events, pl := oneHub(t)
			ctx := context.Background()
			running(t, h, pl, "p-1")
			exited(h, pl, ctx, `{"exit_code":1}`)
			running(t, h, pl, "p-2")
			clock.advance(time.Second)
			exited(h, pl, ctx, `{"exit_code":1}`)
			tm, _ := clock.last(t)
			dropForTest(h, pl)
			pl.lifecycle.Lock()
			err := c.start(h, pl) // passes every refusal; the spawn fails: no work session
			pl.lifecycle.Unlock()
			var se *spawnError
			if !errors.As(err, &se) || se.stage != "session" {
				t.Fatalf("the start: %v", err)
			}
			due, delay, _ := respawnFields(h, pl)
			if !tm.stopped || !due.IsZero() || delay != 2*time.Second {
				t.Fatalf("after a passing start: timer stopped %v, due %v, delay %v", tm.stopped, due, delay)
			}
			tm.f()
			if n := len(eventsNamed(t, events, "plugin_respawn")); n != 0 {
				t.Fatalf("the old callback respawned (%d)", n)
			}
		})
	}
}

// A refused start while crashing (candidate_changed, dependency_not_ready) leaves the timer, the
// delay and "crashing"; the respawn then fires once (the lead's corrected rule). An unresolved
// intent refuses before the cancel point too, but its search calls Rex: the code order shows it.
func TestRespawnRefusedStartKeepsTimer(t *testing.T) {
	for _, c := range []struct {
		name  string
		start func(h *Hub, pl *Plugin) error
		code  string
	}{
		{"candidate_changed", func(h *Hub, pl *Plugin) error {
			return h.startPluginExpect(context.Background(), pl, false, &expectRun{Source: "/elsewhere"})
		}, "candidate_changed"},
		{"dependency_not_ready", func(h *Hub, pl *Plugin) error {
			return h.startPluginExpect(context.Background(), pl, false, &expectRun{Source: pl.entry.cand.Source})
		}, "dependency_not_ready"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, clock, events := respawnHub(t, map[string]string{
				"p": `{"name":"p","main":"p.lua","depends":["q"]}`,
				"q": `{"name":"q","main":"p.lua"}`,
			}, func(dir string) string {
				return `[{"source":"` + filepath.Join(dir, "q") + `"},{"source":"` + filepath.Join(dir, "p") + `"}]`
			})
			pl := h.plugins["p"]
			ctx := context.Background()
			running(t, h, pl, "p-1")
			exited(h, pl, ctx, `{"exit_code":1}`)
			tm, _ := clock.last(t)
			due, delay, gen := respawnFields(h, pl)
			dropForTest(h, pl)
			pl.lifecycle.Lock()
			err := c.start(h, pl)
			pl.lifecycle.Unlock()
			if !isRefusal(err, c.code) {
				t.Fatalf("the start: %v", err)
			}
			due2, delay2, gen2 := respawnFields(h, pl)
			h.mu.Lock()
			state := pl.reportedState()
			h.mu.Unlock()
			if tm.stopped || due2 != due || delay2 != delay || gen2 != gen || state != "crashing" {
				t.Fatalf("after a refused start: stopped %v, due %v -> %v, delay %v -> %v, gen %d -> %d, state %s",
					tm.stopped, due, due2, delay, delay2, gen, gen2, state)
			}
			tm.f()
			if n := len(eventsNamed(t, events, "plugin_respawn")); n != 1 {
				t.Fatalf("%d respawns, want 1", n)
			}
		})
	}
}

// A respawn whose start enters waiting hands the plugin to the wake: no new timer, and the state
// is waiting, not crashing. So the wake and a timer cannot both start it (one retry owner).
func TestRespawnIntoWaitingAddsNoTimer(t *testing.T) {
	h, clock, _ := respawnHub(t, map[string]string{
		"p": `{"name":"p","main":"p.lua","depends":["q"]}`,
		"q": `{"name":"q","main":"p.lua"}`,
	}, func(dir string) string {
		return `[{"source":"` + filepath.Join(dir, "q") + `"},{"source":"` + filepath.Join(dir, "p") + `"}]`
	})
	pl := h.plugins["p"]
	ctx := context.Background()
	running(t, h, pl, "p-1")
	exited(h, pl, ctx, `{"exit_code":1}`)
	tm, n := clock.last(t)
	dropForTest(h, pl)
	tm.f()
	h.mu.Lock()
	state, waiting, due, timer := pl.reportedState(), pl.waiting, pl.respawn.due, pl.respawn.timer
	h.mu.Unlock()
	if state != "waiting" || len(waiting) != 1 || waiting[0] != "q" || !due.IsZero() || timer != nil || clock.count() != n {
		t.Fatalf("after the respawn: state %s %v, due %v, timer %v, %d timers (was %d)", state, waiting, due, timer, clock.count(), n)
	}
}

// A respawn whose start fails before the spawn with no other owner (here: no work session, a known
// spawn failure) schedules the next respawn with the next delay and keeps last_exit.
func TestRespawnFailureReschedules(t *testing.T) {
	h, clock, events, pl := oneHub(t)
	ctx := context.Background()
	running(t, h, pl, "p-1")
	exited(h, pl, ctx, `{"exit_code":3,"error":"exit status 3"}`)
	tm, _ := clock.last(t)
	dropForTest(h, pl)
	tm.f()
	tm2, n := clock.last(t)
	if n != 2 || tm2.d != 2*time.Second {
		t.Fatalf("after a failed respawn: %d timers, delay %v", n, tm2.d)
	}
	sched := eventsNamed(t, events, "plugin_respawn_scheduled")
	if len(sched) != 2 || !strings.Contains(sched[1]["reason"].(string), "the LazyRex session is gone") {
		t.Fatalf("the scheduled events: %v", sched)
	}
	item := entryItem(t, h)
	if le, _ := item["last_exit"].(map[string]any); item["state"] != "crashing" || le["detail"] != "exit status 3" {
		t.Fatalf("hub.plugins: %v", item)
	}
}

// An old timer does nothing after its record is replaced or removed (codex-21's edge 2), and after
// the hub context ends.
func TestRespawnStaleRecordAndShutdown(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(h *Hub, pl *Plugin, cancel func())
	}{
		{"replaced", func(h *Hub, pl *Plugin, _ func()) {
			h.plugins["p"] = &Plugin{Name: "p", entry: pl.entry, Desired: "running"}
		}},
		{"removed", func(h *Hub, pl *Plugin, _ func()) { pl.entry = nil }},
		{"shutdown", func(_ *Hub, _ *Plugin, cancel func()) { cancel() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, clock, events, pl := oneHub(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			running(t, h, pl, "p-1")
			exited(h, pl, ctx, `{"exit_code":1}`)
			tm, _ := clock.last(t)
			h.mu.Lock()
			c.change(h, pl, cancel)
			h.mu.Unlock()
			tm.f()
			if n := len(eventsNamed(t, events, "plugin_respawn")); n != 0 || pl.attempted {
				t.Fatalf("the stale timer respawned (%d, a start ran %v)", n, pl.attempted)
			}
		})
	}
}

// hub.plugins: "crashing" with respawn_at and respawn_delay_ms while the respawn is due, and
// last_exit from the first exit on. The detail is one line of at most 200 bytes.
func TestRespawnPluginsFields(t *testing.T) {
	h, clock, _, pl := oneHub(t)
	ctx := context.Background()
	item := entryItem(t, h)
	if _, ok := item["last_exit"]; ok {
		t.Fatalf("last_exit before any exit: %v", item)
	}
	running(t, h, pl, "p-1")
	clock.advance(1500 * time.Millisecond)
	long := strings.Repeat("é", 150) // 300 bytes
	exited(h, pl, ctx, `{"exit_code":2,"error":"line one\nline two `+long+`"}`)
	item = entryItem(t, h)
	le, _ := item["last_exit"].(map[string]any)
	detail, _ := le["detail"].(string)
	if item["state"] != "crashing" || item["respawn_delay_ms"] != float64(1000) || item["respawn_at"] != "2026-10-22T12:00:02.5Z" {
		t.Fatalf("crashing: %v", item)
	}
	if le["at"] != "2026-10-22T12:00:01.5Z" || le["ran_ms"] != float64(1500) || len(detail) > 200 ||
		strings.Contains(detail, "\n") || !strings.HasPrefix(detail, "line one line two é") {
		t.Fatalf("last_exit: %v (%d bytes)", le, len(detail))
	}
	pl.lifecycle.Lock()
	h.markStopped(ctx, pl, "admin stop")
	pl.lifecycle.Unlock()
	item = entryItem(t, h)
	if _, ok := item["respawn_at"]; ok || item["state"] != "stopped" || item["last_exit"] == nil {
		t.Fatalf("after the stop: %v", item)
	}
}

func TestExitDetail(t *testing.T) {
	for payload, want := range map[string]string{
		`{"exit_code":7,"error":"exit status 7"}`: "exit status 7",
		`{"exit_code":0}`:                         "exit code 0",
		`{"signal":"killed"}`:                     `{"signal":"killed"}`,
		"not json\n  at all":                      "not json at all",
	} {
		if got := exitDetail(json.RawMessage(payload)); got != want {
			t.Errorf("%s: %q, want %q", payload, got, want)
		}
	}
}

// The errors of a respawn's start that schedule the next respawn: only those that leave no other
// retry owner.
func TestRespawnRetries(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errors.New("plugin p: the recorded instance is not retired"), true},
		{&spawnError{stage: "session", known: true}, true},
		{&spawnError{stage: "create", known: true, open: "sending"}, false},
		{&spawnError{stage: "create"}, false},
		{refuse("refused", "x"), false},
		{refuse("unresolved_intent", "x"), false},
	} {
		if got := respawnRetries(c.err); got != c.want {
			t.Errorf("%v: %v", c.err, got)
		}
	}
}
