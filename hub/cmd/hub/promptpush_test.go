package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePush replaces promptPush. answer decides each call's result; calls records the args.
type fakePush struct {
	mu      sync.Mutex
	calls   []map[string]any
	clients []string // the client of each call
	answer  func(ctx context.Context, n int, client string) error
}

func (f *fakePush) install(t *testing.T) {
	old, oldBackoff := promptPush, promptPushBackoff
	t.Cleanup(func() { promptPush, promptPushBackoff = old, oldBackoff })
	promptPushBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	promptPush = func(ctx context.Context, h *Hub, client string, args map[string]any) error {
		f.mu.Lock()
		f.calls = append(f.calls, args)
		f.clients = append(f.clients, client)
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

// senderLoops counts the running prompt sender goroutines.
func senderLoops() int {
	b := make([]byte, 1<<20)
	n := runtime.Stack(b, true)
	return strings.Count(string(b[:n]), "(*Hub).promptSendLoop(")
}

// A new registration stops the idle sender of the old one; a registration that ends (an exit or
// a lost grant) stops its idle sender at the next plugins change. Found in review: 20 idle
// registrations left 20 loops.
func TestPromptSenderLifecycle(t *testing.T) {
	h, _ := promptHub(t)
	var failing atomic.Bool
	f := &fakePush{answer: func(context.Context, int, string) error {
		if failing.Load() {
			return errors.New("lost")
		}
		return nil
	}}
	f.install(t)
	promptPushBackoff = []time.Duration{time.Hour} // before any sender reads it
	before := senderLoops()
	var pl *Plugin
	for i := 0; i < 20; i++ {
		pl = registerTG(h, fmt.Sprint("i", i), fmt.Sprint("c", i))
		time.Sleep(10 * time.Millisecond) // the sender goes idle before the next registration
	}
	waitPush(t, "one sender loop after 20 registrations", func() bool { return senderLoops()-before == 1 })

	// An exit: the registration ends, and the sender waits idle.
	h.mu.Lock()
	pl.ClientID = ""
	h.mu.Unlock()
	h.reapPromptSenders()
	waitPush(t, "no sender loop after the exit", func() bool { return senderLoops() == before })

	// A lost grant (a kept record has no entry), with the sender in a retry wait.
	failing.Store(true)
	sent := f.count()
	pl = registerTG(h, "i-last", "c-last")
	h.mu.Lock()
	h.markPromptDirty("alice")
	h.mu.Unlock()
	waitPush(t, "a failed call", func() bool { return f.count() > sent })
	h.mu.Lock()
	pl.entry = nil
	h.mu.Unlock()
	h.reapPromptSenders()
	waitPush(t, "no sender loop after the grant loss", func() bool { return senderLoops() == before })
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.promptSenders) != 0 {
		t.Fatalf("senders %v remain", h.promptSenders)
	}
}

// registerPlugin is registerTG for another plugin name: a second consumer with its own sender.
func registerPlugin(h *Hub, name, instance, client string) *Plugin {
	h.mu.Lock()
	defer h.mu.Unlock()
	pl := &Plugin{Name: name, entry: &configEntry{index: 2}, run: pluginRun{Prompts: true}}
	h.plugins[name] = pl
	pl.InstanceID, pl.ClientID = instance, client
	h.startPromptSender(pl)
	return pl
}

// ownerPushHub is promptHub with the legacy agent "zulu" (it sorts after every identity ID), whose
// prompt is open and pushed to telegram.
func ownerPushHub(t *testing.T, f *fakePush) (*Hub, *Agent) {
	t.Helper()
	h, a := promptHub(t)
	a.ID = "zulu"
	h.agents = map[string]*Agent{a.ID: a}
	h.agentOrder = []string{a.ID}
	f.install(t)
	registerTG(h, "instance", "client")
	t.Cleanup(func() {
		h.mu.Lock()
		for _, s := range h.promptSenders {
			h.stopPromptSender(s)
		}
		h.mu.Unlock()
	})
	setStatus(h, a, statusRecord{State: "blocked", Msg: "Allow?"})
	waitPush(t, "the legacy prompt", func() bool { return f.count() == 1 })
	return h, a
}

// takeOver makes the identity id the pane's current owner: the alias of a's legacy ID, or a /clear
// that supersedes the previous identity.
func takeOver(h *Hub, a *Agent, id string, prev *Agent) *Agent {
	c := &Agent{ID: id, Kind: "claude", IdState: "current", Session: a.Session, BlockID: a.BlockID, ChildPID: a.ChildPID, ChildStart: a.ChildStart}
	h.mu.Lock()
	defer h.mu.Unlock()
	c.setRecord(a.Record)
	if h.idents == nil {
		h.idents = map[string]*Agent{}
	}
	if prev != nil {
		prev.IdState = "superseded"
	}
	h.idents[id] = c
	h.aliases = map[string]string{a.ID: "claude:c1"}
	h.syncPrompts()
	return c
}

// pushes takes one snapshot of f under its lock and gives the calls from index from (client ""
// means any client): "open <agent>" or "close <agent>", oldest first.
func pushes(f *fakePush, from int, client string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for i := from; i < len(f.calls); i++ {
		if client != "" && f.clients[i] != client {
			continue
		}
		if f.calls[i]["prompt"] != nil {
			out = append(out, "open "+f.calls[i]["agent"].(string))
		} else {
			out = append(out, "close "+f.calls[i]["agent"].(string))
		}
	}
	return out
}

// The consumer receives each owner transfer as close, then open (the hub lead's sender rule: rev
// order, not agent-ID order): legacy zulu -> claude:c1 (alias), then claude:c1 -> claude:b2 (a
// /clear; b2 sorts before c1).
func TestPromptPushOwnerTransferOrder(t *testing.T) {
	f := &fakePush{}
	h, a := ownerPushHub(t, f)
	c1 := takeOver(h, a, "claude:c1", nil)
	waitPush(t, "the alias transfer", func() bool { return f.count() >= 3 })
	if got := pushes(f, 1, ""); len(got) != 2 || got[0] != "close zulu" || got[1] != "open claude:c1" {
		t.Fatalf("alias transfer pushes %q, want close zulu, open claude:c1", got)
	}
	takeOver(h, a, "claude:b2", c1)
	waitPush(t, "the /clear transfer", func() bool { return f.count() >= 5 })
	if got := pushes(f, 3, ""); len(got) != 2 || got[0] != "close claude:c1" || got[1] != "open claude:b2" {
		t.Fatalf("/clear transfer pushes %q, want close claude:c1, open claude:b2", got)
	}
}

// The reply to the close is lost: the open waits until the close is delivered again, then both
// arrive in order.
func TestPromptPushLostCloseHoldsOpen(t *testing.T) {
	f := &fakePush{}
	f.answer = func(ctx context.Context, n int, client string) error {
		if n == 2 {
			return errors.New("disconnected: the reply was lost")
		}
		return nil
	}
	h, a := ownerPushHub(t, f)
	takeOver(h, a, "claude:c1", nil)
	waitPush(t, "the retry and the open", func() bool { return f.count() >= 4 })
	if got := pushes(f, 1, ""); len(got) != 3 || got[0] != "close zulu" || got[1] != "close zulu" || got[2] != "open claude:c1" {
		t.Fatalf("pushes %q, want close zulu (lost), close zulu, open claude:c1", got)
	}
}

// A consumer whose close fails does not hold back another consumer: each registration has its
// own sender and its own order.
func TestPromptPushOtherConsumerNotHeld(t *testing.T) {
	f := &fakePush{}
	failTG := make(chan struct{})
	f.answer = func(ctx context.Context, n int, client string) error {
		if client == "client" && n > 2 { // telegram's pushes after its first prompt and the other's
			select {
			case <-failTG:
				return nil
			default:
				return errors.New("disconnected: the reply was lost")
			}
		}
		return nil
	}
	h, a := ownerPushHub(t, f)
	registerPlugin(h, "other", "instance-2", "client-2")
	waitPush(t, "the other consumer's prompt", func() bool { return f.count() >= 2 })
	base := f.count()
	takeOver(h, a, "claude:c1", nil)
	waitPush(t, "the other consumer's transfer", func() bool {
		return strings.Join(pushes(f, base, "client-2"), ",") == "close zulu,open claude:c1"
	})
	if got := pushes(f, base, "client"); len(got) == 0 || strings.Contains(strings.Join(got, ","), "open claude:c1") {
		t.Fatalf("telegram (close failing) pushes %q: the open must wait for the close", got)
	}
	close(failTG)
	waitPush(t, "telegram's transfer", func() bool {
		got := pushes(f, base, "client")
		return len(got) >= 2 && got[len(got)-1] == "open claude:c1" && got[len(got)-2] == "close zulu"
	})
}
