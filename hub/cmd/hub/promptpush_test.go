package main

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePush replaces promptPush. answer decides each call's result; calls records the args.
type fakePush struct {
	mu     sync.Mutex
	calls  []map[string]any
	answer func(ctx context.Context, n int, client string) error
}

func (f *fakePush) install(t *testing.T) {
	old, oldBackoff := promptPush, promptPushBackoff
	t.Cleanup(func() { promptPush, promptPushBackoff = old, oldBackoff })
	promptPushBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	promptPush = func(ctx context.Context, h *Hub, client string, args map[string]any) error {
		f.mu.Lock()
		f.calls = append(f.calls, args)
		n := len(f.calls)
		f.mu.Unlock()
		if f.answer == nil {
			return nil
		}
		return f.answer(ctx, n, client)
	}
}

func (f *fakePush) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePush) call(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

func waitPush(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

// register makes a registration of a plugin with the prompts grant and starts its sender.
func registerTG(h *Hub, instance, client string) *Plugin {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.plugins == nil {
		h.plugins = map[string]*Plugin{}
	}
	pl := h.plugins["telegram"]
	if pl == nil {
		pl = &Plugin{Name: "telegram", entry: &configEntry{index: 1}, run: pluginRun{Prompts: true}}
		h.plugins["telegram"] = pl
	}
	pl.InstanceID, pl.ClientID = instance, client
	h.startPromptSender(pl)
	return pl
}

// The reply to the opened push is lost; the prompt then closes. The next push carries the current
// state (no prompt), so the plugin cannot keep a stale prompt.
func TestPromptPushLostReplyThenClose(t *testing.T) {
	h, a := promptHub(t)
	f := &fakePush{}
	release := make(chan struct{})
	f.answer = func(ctx context.Context, n int, client string) error {
		if n == 1 {
			<-release // the plugin got it, but the reply is lost
			return errors.New("disconnected: the reply was lost")
		}
		return nil
	}
	f.install(t)
	registerTG(h, "i1", "c1")
	setStatus(h, a, statusRecord{State: "blocked", Msg: "Allow?"})
	waitPush(t, "the opened push", func() bool { return f.count() == 1 })
	if f.call(0)["prompt"] == nil {
		t.Fatalf("first push: %+v", f.call(0))
	}
	setStatus(h, a, statusRecord{State: "working"})
	close(release)
	waitPush(t, "the push after the failure", func() bool { return f.count() >= 2 })
	last := f.call(f.count() - 1)
	if last["prompt"] != nil || last["rev"] != "2" || last["closed"].(map[string]any)["reason"] != "state" || last["hub_generation"] != "7" {
		t.Fatalf("after the lost reply: %+v", last)
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.count(); n != 2 {
		t.Fatalf("%d pushes: a delivered state is not sent again", n)
	}
}

// A plugin that never answers: at most one call is outstanding, the goroutines stay bounded, and
// the next answered call carries the current state.
func TestPromptPushOneOutstanding(t *testing.T) {
	h, a := promptHub(t)
	f := &fakePush{}
	var outstanding, most int32
	var answering atomic.Bool
	f.answer = func(ctx context.Context, n int, client string) error {
		cur := atomic.AddInt32(&outstanding, 1)
		defer atomic.AddInt32(&outstanding, -1)
		for {
			m := atomic.LoadInt32(&most)
			if cur <= m || atomic.CompareAndSwapInt32(&most, m, cur) {
				break
			}
		}
		if answering.Load() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Millisecond): // a timeout
			return errors.New("timeout")
		}
	}
	f.install(t)
	registerTG(h, "i1", "c1")
	before := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		state := "working"
		if i%2 == 0 {
			state = "blocked"
		}
		setStatus(h, a, statusRecord{State: state, Msg: "m"})
	}
	time.Sleep(100 * time.Millisecond)
	if g := runtime.NumGoroutine(); g > before+10 {
		t.Errorf("goroutines %d -> %d", before, g)
	}
	answering.Store(true)
	setStatus(h, a, statusRecord{State: "blocked", Msg: "final"})
	waitPush(t, "the final state", func() bool {
		c := f.call(f.count() - 1)
		p, _ := c["prompt"].(map[string]any)
		return p != nil && p["msg"] == "final"
	})
	if m := atomic.LoadInt32(&most); m != 1 {
		t.Fatalf("at most %d calls were outstanding", m)
	}
}

// A call of an old registration that completes after the new registration does not clean the new
// registration's dirty set; the new registration starts with every agent that had a prompt.
func TestPromptPushOldRegistration(t *testing.T) {
	h, a := promptHub(t)
	f := &fakePush{}
	release := make(chan struct{})
	f.answer = func(ctx context.Context, n int, client string) error {
		if client == "c1" {
			<-release
		}
		return nil
	}
	f.install(t)
	registerTG(h, "i1", "c1")
	setStatus(h, a, statusRecord{State: "blocked", Msg: "a"})
	waitPush(t, "the old registration's call", func() bool { return f.count() == 1 })
	registerTG(h, "i2", "c2")
	waitPush(t, "the new registration's push", func() bool { return f.count() == 2 })
	h.mu.Lock()
	s2 := h.promptSenders["telegram"]
	s2.dirty["alice"] = true // the state is due again for c2, as after a change
	h.mu.Unlock()
	close(release) // the old call completes now
	waitPush(t, "the old sender ends", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.promptSenders["telegram"] == s2
	})
	select {
	case s2.kick <- struct{}{}:
	default:
	}
	waitPush(t, "c2 gets the due state", func() bool { return f.count() == 3 })
	if f.call(1)["agent"] != "alice" || f.call(2)["agent"] != "alice" {
		t.Fatalf("pushes: %+v %+v", f.call(1), f.call(2))
	}
	time.Sleep(30 * time.Millisecond)
	if f.count() != 3 {
		t.Fatalf("%d pushes: the old sender must send nothing more", f.count())
	}
}

// setStatus changes the agent's record as blockEvent does, without blockEvent's hub.changed
// broadcast (the test hub has no Rex client and no view service).
func setStatus(h *Hub, a *Agent, r statusRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a.setRecord(r)
	h.syncPrompts()
}
