package main

// The background loads of the manager's views (C7 plan r2 items 2 to 4): hub.plugins.tags and
// hub.plugins.log. No request runs git, reads a block or reads a file: a request reads its cache
// entry and starts a load when the entry is not fresh. Bounds (the hub lead's limits):
//   - each cache holds at most loadKeysMax keys; a new key evicts the key that was requested least
//     recently and has no running load; if every key has a running load, the request is busy;
//   - at most loadsAtOnce loads run at the same time, across every cache and key;
//   - one load for each key at a time: a request during a load joins it.
//
// Every state change sends hub.changed {what = "plugins"}, so the view renders again.

import (
	"context"
	"sync"
	"time"
)

const (
	loadKeysMax = 64
	loadsAtOnce = 4
)

// loadFunc runs one load of key. accept false: the result is stale (the record changed during the
// load), and the entry goes back to idle with no value.
type loadFunc func(ctx context.Context, key string) (value any, accept bool, err error)

// loadEntry is one key of a cache. The fields are guarded by pluginLoads.mu.
type loadEntry struct {
	running   bool
	gen       uint64    // the number of the last started load
	requested time.Time // the last request: the eviction order
	readAt    time.Time // the end of the last load that was kept; zero: none
	value     any       // the last good result
	errText   string    // the error of the last kept load; "" after a success
}

// loadReply is what a request returns: state idle, loading, ready or failed.
type loadReply struct {
	State  string
	Value  any
	Error  string
	ReadAt time.Time
}

func (e *loadEntry) reply() loadReply {
	r := loadReply{Value: e.value, Error: e.errText, ReadAt: e.readAt}
	switch {
	case e.running:
		r.State = "loading"
	case e.errText != "":
		r.State = "failed"
	case e.readAt.IsZero():
		r.State = "idle"
	default:
		r.State = "ready"
	}
	return r
}

type loadCache struct {
	name    string
	fresh   time.Duration
	run     loadFunc
	entries map[string]*loadEntry
}

// pluginLoads is the hub's load service: its caches, the shared semaphore, and the hub's run
// context, so a hub stop cancels every load.
type pluginLoads struct {
	mu         sync.Mutex
	tags, logs *loadCache // hub.plugins.tags and hub.plugins.log
	ctx        context.Context
	sem        chan struct{}
	now        func() time.Time // a test sets a fake clock
	call       rexCall          // the Rex call of the log loads; nil: h.rex.Call (a test sets a fake)
	goroutines sync.WaitGroup   // the running loads, with their notices (wait)
}

func (l *pluginLoads) init(ctx context.Context) {
	l.ctx, l.sem, l.now = ctx, make(chan struct{}, loadsAtOnce), time.Now
}

func (l *pluginLoads) cache(name string, fresh time.Duration, run loadFunc) *loadCache {
	return &loadCache{name: name, fresh: fresh, run: run, entries: map[string]*loadEntry{}}
}

// loadRequest returns the entry of key, and starts a load when the entry is not fresh, or for a
// refresh, unless a load runs. Each state change of the load sends hub.changed {what = "plugins"}.
func (h *Hub) loadRequest(c *loadCache, key string, refresh bool) (loadReply, error) {
	return h.loads.requestNotify(c, key, refresh, func() { h.changed("plugins") })
}

// request is loadRequest with no notice (the tests of the cache).
func (l *pluginLoads) request(c *loadCache, key string, refresh bool) (loadReply, error) {
	return l.requestNotify(c, key, refresh, func() {})
}

func (l *pluginLoads) requestNotify(c *loadCache, key string, refresh bool, notify func()) (loadReply, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := c.entries[key]
	if e == nil {
		if len(c.entries) >= loadKeysMax {
			var oldest string
			for k, x := range c.entries {
				if !x.running && (oldest == "" || x.requested.Before(c.entries[oldest].requested)) {
					oldest = k
				}
			}
			if oldest == "" {
				return loadReply{}, refuse("busy", "too many plugin loads; try again")
			}
			delete(c.entries, oldest)
		}
		e = &loadEntry{}
		c.entries[key] = e
	}
	e.requested = now
	if !e.running && (refresh || e.readAt.IsZero() || now.Sub(e.readAt) >= c.fresh) {
		e.running = true
		e.gen++
		l.goroutines.Add(1)
		go l.runLoad(c, key, e.gen, notify)
	}
	return e.reply(), nil
}

// runLoad waits for the semaphore, runs the load, and keeps its result if the entry is still the
// entry of that load.
func (l *pluginLoads) runLoad(c *loadCache, key string, gen uint64, notify func()) {
	defer l.goroutines.Done()
	var value any
	accept, err := true, error(nil)
	select {
	case l.sem <- struct{}{}:
		value, accept, err = c.run(l.ctx, key)
		<-l.sem
	case <-l.ctx.Done():
		err = l.ctx.Err()
	}
	l.mu.Lock()
	e := c.entries[key]
	if e == nil || e.gen != gen { // not possible while it runs (no eviction); kept as a guard
		l.mu.Unlock()
		return
	}
	e.running = false
	switch {
	case !accept:
		e.value, e.errText, e.readAt = nil, "", time.Time{}
	case err != nil:
		e.errText, e.readAt = err.Error(), l.now()
	default:
		e.value, e.errText, e.readAt = value, "", l.now()
	}
	l.mu.Unlock()
	notify()
}

// wait returns when every load goroutine has ended, its notice included. Tests call it after they
// cancel the run context, so no notice outlives the test.
func (l *pluginLoads) wait() { l.goroutines.Wait() }

// loadsStart starts the load service with the hub's run context and makes its caches.
func (h *Hub) loadsStart(ctx context.Context) {
	h.loads.init(ctx)
	h.loads.tags = h.loads.cache("tags", tagsFresh, h.runTagsLoad)
	h.loads.logs = h.loads.cache("logs", logFresh, h.runLogLoad)
}
