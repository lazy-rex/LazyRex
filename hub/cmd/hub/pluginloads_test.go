package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateLoad is a load function that waits for its release, and counts its runs.
type gateLoad struct {
	mu      sync.Mutex
	runs    map[string]int
	release chan struct{}
	err     error
}

func newGateLoad() *gateLoad { return &gateLoad{runs: map[string]int{}, release: make(chan struct{})} }

func (g *gateLoad) run(ctx context.Context, key string) (any, bool, error) {
	g.mu.Lock()
	g.runs[key]++
	n := g.runs[key]
	g.mu.Unlock()
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, true, ctx.Err()
	}
	if g.err != nil {
		return nil, true, g.err
	}
	return fmt.Sprintf("%s#%d", key, n), true, nil
}

func (g *gateLoad) count(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runs[key]
}

func loadsTestHub(t *testing.T, fresh time.Duration, g *gateLoad) (*Hub, *loadCache, *time.Time) {
	t.Helper()
	h, _, _ := opsTestHub(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); h.loads.wait() }) // no load notice outlives the test fakes
	now := time.Unix(1000, 0)
	h.loads.init(ctx)
	h.loads.now = func() time.Time { return now }
	return h, h.loads.cache("test", fresh, g.run), &now
}

func waitLoad(t *testing.T, h *Hub, c *loadCache, key, state string) loadReply {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		h.loads.mu.Lock()
		e := c.entries[key]
		var r loadReply
		if e != nil {
			r = e.reply()
		}
		h.loads.mu.Unlock()
		if r.State == state {
			return r
		}
		if time.Now().After(end) {
			t.Fatalf("%s: state %q, want %q", key, r.State, state)
		}
	}
}

// One load for each key: a second request during a load joins it. A ready result stays fresh for
// the cache's interval; after it, a request returns the old value with state loading and starts a
// load. A refresh starts a load at once, unless one runs. A failure keeps the last good value.
func TestLoadCacheStates(t *testing.T) {
	g := newGateLoad()
	h, c, now := loadsTestHub(t, time.Minute, g)
	if r, err := h.loads.request(c, "a", false); err != nil || r.State != "loading" {
		t.Fatalf("the first request: %+v %v", r, err)
	}
	h.loads.request(c, "a", false)
	h.loads.request(c, "a", true)
	g.release <- struct{}{}
	if r := waitLoad(t, h, c, "a", "ready"); r.Value != "a#1" || g.count("a") != 1 {
		t.Fatalf("one load: %+v, %d runs", r, g.count("a"))
	}
	if r, _ := h.loads.request(c, "a", false); r.State != "ready" || g.count("a") != 1 {
		t.Fatalf("fresh: %+v, %d runs", r, g.count("a"))
	}
	*now = now.Add(time.Minute)
	if r, _ := h.loads.request(c, "a", false); r.State != "loading" || r.Value != "a#1" {
		t.Fatalf("stale: %+v", r)
	}
	g.release <- struct{}{}
	waitLoad(t, h, c, "a", "ready")
	h.loads.request(c, "a", true) // refresh, while fresh
	g.err = errors.New("the remote failed")
	g.release <- struct{}{}
	if r := waitLoad(t, h, c, "a", "failed"); r.Value != "a#2" || r.Error != "the remote failed" || g.count("a") != 3 {
		t.Fatalf("a failure: %+v, %d runs", r, g.count("a"))
	}
}

// A load that the hub's stop cancels ends failed with the cancel.
func TestLoadCacheCancel(t *testing.T) {
	g := newGateLoad()
	h, _, _ := opsTestHub(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	h.loads.init(ctx)
	c := h.loads.cache("test", time.Minute, g.run)
	h.loads.request(c, "a", false)
	cancel()
	if r := waitLoad(t, h, c, "a", "failed"); r.Error != context.Canceled.Error() {
		t.Fatalf("after the stop: %+v", r)
	}
}

// At most loadsAtOnce loads run at the same time, across every cache and key; the others wait in
// state loading. A full cache evicts the key that was requested least recently and has no running
// load; when every key has a running load, the request is refused busy.
func TestLoadCacheBounds(t *testing.T) {
	g := newGateLoad()
	h, c, now := loadsTestHub(t, time.Minute, g)
	other := h.loads.cache("other", time.Minute, g.run)
	for i := 0; i < loadsAtOnce+2; i++ {
		*now = now.Add(time.Second)
		cc := c
		if i%2 == 1 {
			cc = other
		}
		if _, err := h.loads.request(cc, fmt.Sprintf("k%d", i), false); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	g.mu.Lock()
	running := 0
	for _, n := range g.runs {
		running += n
	}
	g.mu.Unlock()
	if running != loadsAtOnce {
		t.Fatalf("%d loads started, want %d", running, loadsAtOnce)
	}
	close(g.release) // every load ends
	for i := 0; i < loadsAtOnce+2; i++ {
		cc := c
		if i%2 == 1 {
			cc = other
		}
		waitLoad(t, h, cc, fmt.Sprintf("k%d", i), "ready")
	}

	g2 := newGateLoad()
	h2, c2, now2 := loadsTestHub(t, time.Minute, g2)
	for i := 0; i < loadKeysMax; i++ {
		*now2 = now2.Add(time.Second)
		h2.loads.request(c2, fmt.Sprintf("k%d", i), false)
	}
	if _, err := h2.loads.request(c2, "new", false); !isRefusal(err, "busy") {
		t.Fatalf("a full cache of running loads: %v", err)
	}
	close(g2.release)
	for i := 0; i < loadKeysMax; i++ {
		waitLoad(t, h2, c2, fmt.Sprintf("k%d", i), "ready")
	}
	*now2 = now2.Add(time.Second)
	h2.loads.request(c2, "k0", false) // k0 is fresh and requested now; k1 is the oldest
	if _, err := h2.loads.request(c2, "new", false); err != nil {
		t.Fatal(err)
	}
	h2.loads.mu.Lock()
	_, k0 := c2.entries["k0"]
	_, k1 := c2.entries["k1"]
	n := len(c2.entries)
	h2.loads.mu.Unlock()
	if !k0 || k1 || n != loadKeysMax {
		t.Fatalf("eviction: k0 %v, k1 %v, %d keys", k0, k1, n)
	}
}

// A load that reports its result stale (accept false: the record changed during the load) leaves
// the entry idle with no value, so the next request starts a new load.
func TestLoadCacheStale(t *testing.T) {
	var stale atomic.Bool
	stale.Store(true)
	run := func(ctx context.Context, key string) (any, bool, error) { return "v", !stale.Load(), nil }
	h, _, _ := opsTestHub(t, "")
	h.loads.init(context.Background())
	c := h.loads.cache("test", time.Minute, run)
	h.loads.request(c, "a", false)
	if r := waitLoad(t, h, c, "a", "idle"); r.Value != nil {
		t.Fatalf("a stale result was kept: %+v", r)
	}
	stale.Store(false)
	h.loads.request(c, "a", false)
	if r := waitLoad(t, h, c, "a", "ready"); r.Value != "v" {
		t.Fatalf("the next load: %+v", r)
	}
}
