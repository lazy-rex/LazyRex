package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// changedFake records hub.changed deliveries and answers them with reply (nil: success). A
// delivery to a held client waits until release.
type changedFake struct {
	mu      sync.Mutex
	calls   []string // "client what"
	reply   func(n int) error
	held    chan struct{}
	started chan struct{}
}

func newChangedFake(t *testing.T) *changedFake {
	f := &changedFake{started: make(chan struct{}, 100)}
	real, retry := changedCall, changedRetry
	t.Cleanup(func() { changedCall, changedRetry = real, retry })
	changedRetry = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	changedCall = func(ctx context.Context, h *Hub, client, what string) error {
		f.mu.Lock()
		f.calls = append(f.calls, client+" "+what)
		n := len(f.calls)
		held, reply := f.held, f.reply
		f.mu.Unlock()
		f.started <- struct{}{}
		if held != nil {
			<-held
		}
		if reply != nil {
			return reply(n)
		}
		return nil
	}
	return f
}

func (f *changedFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func changedTestHub(clients map[string]string) *Hub {
	h := &Hub{plugins: map[string]*Plugin{}, noticeKick: make(chan struct{}, 1),
		vs: &viewSvc{dirty: map[string]bool{}, workers: map[string]chan struct{}{}}}
	for name, client := range clients {
		h.plugins[name] = &Plugin{Name: name, ClientID: client}
		h.vs.workers[name] = make(chan struct{}, 1)
	}
	return h
}

// waitIdle waits until no delivery is in flight.
func waitIdle(t *testing.T, h *Hub) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		h.changedQ.mu.Lock()
		n := len(h.changedQ.pairs)
		h.changedQ.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%d deliveries still in flight", n)
		}
	}
}

// A burst during a held delivery gives exactly one more delivery per plugin and kind. The views
// and the notice loop are still kicked.
func TestChangedCoalescesBurst(t *testing.T) {
	f := newChangedFake(t)
	f.held = make(chan struct{})
	h := changedTestHub(map[string]string{"a": "client-a", "b": "client-b"})
	h.changed("agents")
	<-f.started
	<-f.started // one held delivery per plugin
	for i := 0; i < 50; i++ {
		h.changed("agents", "workers")
	}
	<-f.started
	<-f.started // the first workers delivery per plugin, held too
	close(f.held)
	waitIdle(t, h)
	// a, b x agents: the held call and one more; a, b x workers: the first call and one more.
	if n := f.count(); n != 8 {
		t.Fatalf("%d deliveries: %v", n, f.calls)
	}
	if len(h.noticeKick) != 1 || !h.vs.dirty["a"] || !h.vs.dirty["b"] {
		t.Fatalf("kicks: notice %d, dirty %v", len(h.noticeKick), h.vs.dirty)
	}
}

// A refused delivery is retried with no new change, and the pair ends after a success.
func TestChangedRetriesRefusal(t *testing.T) {
	f := newChangedFake(t)
	f.reply = func(n int) error {
		if n <= 2 {
			return changedBusy()
		}
		return nil
	}
	h := changedTestHub(map[string]string{"a": "client-a"})
	h.changed("workers")
	waitIdle(t, h)
	if n := f.count(); n != 3 {
		t.Fatalf("%d deliveries, want 2 refusals and a success: %v", n, f.calls)
	}
	// The retries are bounded: a client that always refuses gets 1 + len(changedRetry) calls.
	f.mu.Lock()
	f.calls, f.reply = nil, func(int) error { return changedBusy() }
	f.mu.Unlock()
	h.changed("workers")
	waitIdle(t, h)
	if n := f.count(); n != 1+len(changedRetry) {
		t.Fatalf("%d deliveries to a refusing client", n)
	}
}

// A timeout is not retried: the plugin may still perform the call.
func TestChangedTimeoutNotRetried(t *testing.T) {
	f := newChangedFake(t)
	f.reply = func(int) error { return context.DeadlineExceeded }
	h := changedTestHub(map[string]string{"a": "client-a"})
	h.changed("agents")
	waitIdle(t, h)
	if n := f.count(); n != 1 {
		t.Fatalf("%d deliveries after a timeout", n)
	}
}

// A registration that ends while its delivery is pending gets no more deliveries; the new
// registration gets its own.
func TestChangedReplacementWhilePending(t *testing.T) {
	f := newChangedFake(t)
	hold := make(chan struct{})
	f.held = hold
	h := changedTestHub(map[string]string{"a": "client-old"})
	h.changed("plugins")
	<-f.started
	h.changed("plugins") // pending for client-old
	h.mu.Lock()
	h.plugins["a"].ClientID = "client-new"
	h.mu.Unlock()
	f.mu.Lock()
	f.held = nil
	f.mu.Unlock()
	h.changed("plugins") // a new pair for client-new
	<-f.started
	close(hold)
	waitIdle(t, h)
	f.mu.Lock()
	defer f.mu.Unlock()
	if fmt.Sprint(f.calls) != "[client-old plugins client-new plugins]" {
		t.Fatalf("deliveries: %v", f.calls)
	}
}

// changedBusy is Rex's refusal when a client's queue is full (TestChangedBurstLab).
func changedBusy() error {
	return &rex.RexError{Code: "client_unavailable", Message: "client:01a16105 is busy: 16 requests are already waiting"}
}

// Only the full-queue refusal is retried: another client_unavailable (the client is gone) and a
// lost connection are final.
func TestChangedRetryableCodes(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{changedBusy(), true},
		{&rex.RexError{Code: "client_unavailable", Message: "client:x is not connected"}, false},
		{&rex.RexError{Code: "disconnected", Message: "failed to get reader"}, false},
		{&rex.RexError{Code: "not_found", Message: "unknown action hub.changed"}, false},
		{context.DeadlineExceeded, false},
	} {
		if got := changedRetryable(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}
