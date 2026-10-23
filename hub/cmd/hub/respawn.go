package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// The crash backoff (lead-hf-queue item 2, crash-plan-2). A plugin instance that exits while its
// record is desired running respawns after a delay: 1 s, then twice the last delay, at most 60 s.
// An instance that ran 60 s or more starts the sequence again at 1 s. While the respawn is due,
// hub.plugins reports state "crashing": desired running, the recorded instance has exited, and a
// respawn is due at respawn_at. The record keeps the exited InstanceID and BlockID until the
// respawn's start retires them.
//
// One retry owner at a time. A stop cancels the respawn and resets the delay. A start cancels it
// when it passes its refusals, at its first effect; a refused start leaves it. A respawn whose start
// enters another owner (waiting, an unresolved intent, a refusal) adds no timer. Only a start error
// that leaves no owner schedules the next respawn (respawnRetries).
const (
	respawnFirst  = time.Second
	respawnMax    = 60 * time.Second
	respawnReset  = 60 * time.Second // a run this long starts the delays again at respawnFirst
	exitDetailMax = 200              // bytes of last_exit.detail
)

// respawnState is the crash backoff of one plugin record. h.mu guards it.
type respawnState struct {
	gen     uint64        // a cancel or a new timer increments it: an older callback does nothing
	timer   stopper       // the pending respawn; nil when none
	due     time.Time     // when the pending respawn fires; zero when none
	delay   time.Duration // the last delay; 0 after a stop (the next exit waits respawnFirst)
	entry   *configEntry  // the record's entry when the timer was set
	spawned time.Time     // when the hub recorded the current instance; zero when unknown
	exited  string        // the instance whose exit was counted: a second child_exited counts once
	last    *exitInfo     // the last counted exit in this hub process
}

// exitInfo is hub.plugins last_exit.
type exitInfo struct {
	at     time.Time
	detail string
	ran    time.Duration // -1 when the hub did not record the spawn
}

// stopper is the part of *time.Timer that the backoff uses; a test replaces the timers.
type stopper interface{ Stop() bool }

// clock is h.now, or time.Now.
func (h *Hub) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// after is h.afterFunc, or time.AfterFunc.
func (h *Hub) after(d time.Duration, f func()) stopper {
	if h.afterFunc != nil {
		return h.afterFunc(d, f)
	}
	return time.AfterFunc(d, f)
}

// crashing reports whether a respawn of pl is due: the state that hub.plugins reports as
// "crashing". h.mu must be held.
func (pl *Plugin) crashing() bool {
	r := &pl.respawn
	return !r.due.IsZero() && pl.state == "" && pl.Desired == "running" && pl.entry != nil && pl.entry == r.entry
}

// cancelRespawnLocked cancels a pending respawn of pl. It keeps the delay; a stop resets it. h.mu
// must be held; it takes no lock.
func (pl *Plugin) cancelRespawnLocked() {
	r := &pl.respawn
	r.gen++
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.due = time.Time{}
}

// armRespawnLocked sets the respawn timer of pl for the current delay. h.mu must be held.
func (h *Hub) armRespawnLocked(ctx context.Context, pl *Plugin) {
	pl.cancelRespawnLocked()
	r := &pl.respawn
	gen := r.gen
	r.due, r.entry = h.clock().Add(r.delay), pl.entry
	r.timer = h.after(r.delay, func() { h.respawnDue(ctx, pl, gen) })
}

// crashedLocked counts the exit of pl's current instance and schedules its respawn. h.mu must be
// held.
func (h *Hub) crashedLocked(ctx context.Context, pl *Plugin, detail string) {
	r := &pl.respawn
	now := h.clock()
	ran := time.Duration(-1)
	if !r.spawned.IsZero() {
		ran = now.Sub(r.spawned)
	}
	r.last = &exitInfo{at: now, detail: detail, ran: ran}
	if r.delay == 0 || ran < 0 || ran >= respawnReset {
		r.delay = respawnFirst
	} else {
		r.delay = min(2*r.delay, respawnMax)
	}
	h.armRespawnLocked(ctx, pl)
}

// respawnDue runs the respawn that the timer of generation gen scheduled. It acts only if no cancel
// or new timer came after it, the record is still the hub's record of that name with the same
// entry, the record is desired running and the hub runs. The lock order is the one of a start:
// pl.lifecycle, then h.mu, which it releases before the start path.
func (h *Hub) respawnDue(ctx context.Context, pl *Plugin, gen uint64) {
	pl.lifecycle.Lock()
	defer pl.lifecycle.Unlock()
	h.mu.Lock()
	r := &pl.respawn
	ok := r.gen == gen && h.plugins[pl.Name] == pl && pl.entry != nil && pl.entry == r.entry &&
		pl.Desired == "running" && ctx.Err() == nil
	entry, delay := r.entry, r.delay
	if ok {
		r.timer, r.due = nil, time.Time{} // "crashing" ends: the start path owns the plugin now
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	h.ev.emit("plugin_respawn", map[string]any{"plugin": pl.Name, "delay_ms": delay.Milliseconds()})
	err := h.startAutomatic(ctx, pl) // the selected run (selectedrun.go)
	if err == nil {
		return
	}
	pluginLogf(pl.Name, opEntryKey(entry), "respawn: %v", err)
	h.mu.Lock()
	again := respawnRetries(err) && h.plugins[pl.Name] == pl && pl.entry == entry && pl.Desired == "running" &&
		pl.state == "" && r.due.IsZero() && ctx.Err() == nil
	if again {
		r.delay = min(2*r.delay, respawnMax)
		h.armRespawnLocked(ctx, pl)
	}
	delay = r.delay
	h.mu.Unlock()
	if again {
		h.ev.emit("plugin_respawn_scheduled", map[string]any{"plugin": pl.Name, "delay_ms": delay.Milliseconds(), "reason": err.Error()})
		h.changed("plugins")
	}
}

// respawnRetries reports whether a respawn's start error leaves no other retry owner, so that the
// backoff schedules the next respawn. No owner: an error before the spawn intent (a store read, an
// intent resolution or the prune lock failed, the recorded instance was not retired) and a known
// spawn failure whose intent was abandoned. An owner: a refusal (refused or retained: the next
// hub.admin.start or reload; unresolved_intent: the intent resolution), and a spawn whose outcome
// is unknown or whose intent stays open (the intent resolution).
func respawnRetries(err error) bool {
	var se *spawnError
	if errors.As(err, &se) {
		return se.known && se.open == ""
	}
	var re *storeError
	return !errors.As(err, &re) || re.code == ""
}

// exitDetail is last_exit.detail: the error text of the child_exited payload, else its exit code,
// else the payload. One line, at most exitDetailMax bytes.
func exitDetail(payload json.RawMessage) string {
	var p struct {
		Code  *int   `json:"exit_code"`
		Error string `json:"error"`
	}
	text := string(payload)
	if json.Unmarshal(payload, &p) == nil {
		switch {
		case p.Error != "":
			text = p.Error
		case p.Code != nil:
			text = fmt.Sprintf("exit code %d", *p.Code)
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > exitDetailMax {
		text = text[:exitDetailMax]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return text
}

// respawnFields adds the backoff fields of pl to its hub.plugins item: last_exit after an exit in
// this hub process, and respawn_at and respawn_delay_ms only while crashing. h.mu must be held.
func (pl *Plugin) respawnFields(item map[string]any) {
	r := &pl.respawn
	if r.last != nil {
		le := map[string]any{"at": r.last.at.UTC().Format(time.RFC3339Nano), "detail": r.last.detail}
		if r.last.ran >= 0 {
			le["ran_ms"] = r.last.ran.Milliseconds()
		}
		item["last_exit"] = le
	}
	if pl.crashing() {
		item["respawn_at"] = r.due.UTC().Format(time.RFC3339Nano)
		item["respawn_delay_ms"] = r.delay.Milliseconds()
	}
}
