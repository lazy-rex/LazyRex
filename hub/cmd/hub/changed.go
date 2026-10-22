package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// hub.changed delivery. The payload is a hint: a plugin reads the current state when it gets one.
// So the hub keeps at most one delivery in flight for each registration (client ID) and kind
// ("agents", "workers", "plugins", ...). A change during a delivery marks the pair pending: one
// more delivery follows, however many changes came. Without this, a burst (an agent restart of
// many panes) queued about 30 deliveries per 10 s per plugin, filled Rex's 16-request queue of
// each plugin and blocked their other requests (2026-10-22, the live hub after release 2).
//
// A refused delivery is retried with a backoff (changedRetry), because no new change may come to
// carry the hint. A timeout is not retried: the plugin may still perform it (Rex: a timeout does
// not cancel the action). A registration that ended (a replacement, a stop) ends its deliveries.

type changedKey struct{ client, what string }

// changedState is one pair's delivery. pending: a change came after the current delivery
// started.
type changedState struct{ pending bool }

type changedQueue struct {
	mu    sync.Mutex
	pairs map[changedKey]*changedState // the pairs with a delivery in flight
}

// changedRetry are the waits before each retry of a refused delivery. Their sum (3.1 s) bounds
// the life of a delivery loop after its last change, with the 3 s call limits.
var changedRetry = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}

// changedCall delivers one hub.changed. A test replaces it.
var changedCall = func(ctx context.Context, h *Hub, client, what string) error {
	return h.rex.Call(ctx, "client.perform", map[string]any{"client_id": client, "action": "hub.changed", "args": map[string]any{"what": what}, "timeout_ms": 3000}, nil)
}

// deliverChanged starts a delivery for the pair, or marks the running one pending.
func (h *Hub) deliverChanged(client, name, what string) {
	q := &h.changedQ
	k := changedKey{client, what}
	q.mu.Lock()
	if st, ok := q.pairs[k]; ok {
		st.pending = true
		q.mu.Unlock()
		return
	}
	if q.pairs == nil {
		q.pairs = map[changedKey]*changedState{}
	}
	st := &changedState{}
	q.pairs[k] = st
	q.mu.Unlock()
	go h.changedLoop(k, st, name)
}

// changedLoop delivers until no change is pending, then removes the pair.
func (h *Hub) changedLoop(k changedKey, st *changedState, name string) {
	defer func() {
		q := &h.changedQ
		q.mu.Lock()
		delete(q.pairs, k)
		q.mu.Unlock()
	}()
	for {
		h.changedOnce(k, name)
		q := &h.changedQ
		q.mu.Lock()
		again := st.pending
		st.pending = false
		q.mu.Unlock()
		if !again {
			return
		}
	}
}

// changedOnce makes one delivery, with retries of a refusal while the registration is current.
func (h *Hub) changedOnce(k changedKey, name string) {
	for attempt := 0; ; attempt++ {
		if !h.clientCurrent(k.client) {
			return // the registration ended: its next instance gets its own changes
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := changedCall(ctx, h, k.client, k.what)
		cancel()
		if err == nil || !changedRetryable(err) {
			if err != nil && !strings.Contains(err.Error(), "unknown action") && !strings.Contains(err.Error(), "not_found") {
				log.Printf("hub.changed %s to %s: %v", k.what, name, err)
			}
			return
		}
		if attempt == len(changedRetry) {
			log.Printf("hub.changed %s to %s: %v (gave up after %d attempts)", k.what, name, err, attempt+1)
			return
		}
		time.Sleep(changedRetry[attempt])
	}
}

// changedRetryable: Rex refused the request because the client's queue was full. Rex answers that
// with code client_unavailable and the text "<client> is busy: 16 requests are already waiting"
// (observed on Rex 0.0.1792158932-g88fd41, lab, TestChangedBurstLab before the fix). Any other
// error is final: a timeout (the plugin may still perform it), a gone client, or an action the
// plugin lacks.
func changedRetryable(err error) bool {
	var re *rex.RexError
	return errors.As(err, &re) && re.Code == "client_unavailable" && strings.Contains(re.Message, " is busy: ")
}

// clientCurrent reports whether a registered plugin has the client ID now.
func (h *Hub) clientCurrent(client string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.plugins {
		if p.ClientID == client {
			return true
		}
	}
	return false
}
