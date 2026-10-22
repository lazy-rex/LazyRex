package main

// hub.admin.replace (docs/plugin-sources.md, section 14): replace the running instance of a plugin
// with a new instance from its current candidate. The caller names the instance it expects, so a
// replay or a race with another change cannot replace a different instance. Lock order: reloadMu,
// then the plugin's lifecycle lock, then entriesMu (to resolve only), then mu. A reload cannot swap
// the entries during a replace, so the candidate and the entry membership stay stable from the
// check to the spawn.
//
// Until retirement begins, a refusal retires nothing and spawns nothing; the old instance keeps its
// state, client, exports and run. Step 2 (the intent reconciliation) can still change ownership
// before a refusal: then the text names the current owned instance. After retirement begins, the
// old instance is gone or partly stopped, and the error says so.

import (
	"context"
	"errors"
	"fmt"
)

// ownedInstance returns the instance that the record owns: the recorded instance while it has a
// block, else the instance of an open spawn intent (pending), else "". clearInstance leaves
// InstanceID set, so InstanceID alone is not the owned instance. h.mu must be held.
func (h *Hub) ownedInstance(pl *Plugin) (id string, pending bool) {
	if pl.BlockID != "" {
		return pl.InstanceID, false
	}
	var open string
	if err := h.db.QueryRow(`SELECT instance_id FROM spawn_intents WHERE plugin = ? AND status IN ('prepared', 'sending') ORDER BY rowid DESC LIMIT 1`, pl.Name).Scan(&open); err != nil {
		return "", false
	}
	return open, true
}

// validateCandidate resolves the entries and returns the candidate of pl's entry, or its refusal.
// Unlike checkStart, it writes no state, refusal, waiting state or ownership of the record. The
// caller holds reloadMu, so the entry does not change.
func (h *Hub) validateCandidate(pl *Plugin) (*configEntry, *pluginRun, error) {
	h.entriesMu.Lock()
	defer h.entriesMu.Unlock()
	h.resolveLocked()
	h.mu.Lock()
	defer h.mu.Unlock()
	e := pl.entry
	if e == nil {
		return nil, nil, errKept
	}
	return e, e.cand, e.err
}

// spawnStageText names a known spawn failure: what failed before or at the create request.
var spawnStageText = map[string]string{
	"session": "no work session",
	"intent":  "the spawn intent was not recorded",
	"sending": "the spawn intent was not marked as sending",
	"create":  "Rex refused the new block",
	"save":    "the run was not saved",
}

// openIntentText tells what a start does with an intent that a known failure left open
// (resolveIntents): a prepared intent was never sent; a sending intent stays unresolved.
var openIntentText = map[string]string{
	"prepared": "hub.admin.start resolves it, because a prepared intent was never sent",
	"sending":  "hub.admin.start refuses with unresolved_intent until the intent's session closes",
}

func (h *Hub) adminReplace(ctx context.Context, args map[string]any) (any, error) {
	for k := range args {
		if k != "plugin" && k != "expect_instance" && k != "expect_run" && k != "attempt" {
			return nil, bad("invalid: unknown field %s", k)
		}
	}
	name, _ := args["plugin"].(string)
	expect, _ := args["expect_instance"].(string)
	if name == "" || expect == "" {
		return nil, bad("invalid: replace needs plugin and a nonempty expect_instance")
	}
	x, err := parseExpectRun(args)
	if err != nil {
		return nil, err
	}
	attempt, err := parseAttempt(args)
	if err != nil {
		return nil, err
	}
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	h.mu.Lock()
	pl := h.plugins[name]
	h.mu.Unlock()
	if pl == nil {
		return nil, bad("unknown plugin %q", name)
	}
	pl.lifecycle.Lock()
	defer pl.lifecycle.Unlock()
	defer h.kickWaiting() // the wake rule: a start or a stop of any plugin

	// The row read inserts no row: up to sent_at the call has no effect (adminattempt.go).
	h.mu.Lock()
	err = h.readPluginRow(ctx, pl)
	target, block := pl.InstanceID, pl.BlockID
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if attempt != "" {
		if err := h.bindAttempt(ctx, attempt, "replace", target, block); err != nil {
			return nil, err
		}
		pl.opAttempt = attempt // the spawn of this call writes its intent_id (spawn)
		defer func() { pl.opAttempt = "" }()
	}
	// The first effect: the insert of an absent row, then the resolution of earlier intents. That
	// resolution belongs to the earlier intents: a refusal after it says that this call retired and
	// spawned nothing, not that nothing changed.
	if err := h.attemptSent(ctx, attempt); err != nil {
		return nil, err
	}
	if err := h.ensurePluginRow(ctx, pl.Name); err != nil {
		return nil, err
	}
	if err := h.resolveIntents(ctx, pl); err != nil {
		return nil, err // unresolved_intent: no second spawn while ownership is unresolved
	}
	h.mu.Lock()
	desired := pl.Desired
	owned, pending := h.ownedInstance(pl)
	live := owned != "" && !pending && alive(pl.ChildPID, pl.ChildStart)
	kept := pl.entry == nil
	nowBlock := pl.BlockID
	h.mu.Unlock()
	if desired != "running" {
		return nil, refuse("stopped", "plugin %s is stopped; use hub.admin.start", name)
	}
	if !live {
		return nil, refuse("no_instance", "plugin %s has no instance; use hub.admin.start", name)
	}
	if owned != expect {
		return nil, refuse("instance_changed", "plugin %s runs instance %s, not %s; nothing was replaced", name, owned, expect)
	}
	if attempt != "" && (owned != target || nowBlock != block) {
		// The intent resolution adopted another instance or block: the bound target is not what a
		// retire would act on.
		return nil, refuse("instance_changed", "plugin %s: the intent resolution changed the instance from %s to %s; this replace retired and spawned nothing", name, target, owned)
	}
	if kept {
		return nil, refuse("refused", "plugin %s has no config entry: stop it with hub.admin.stop, or start its entry by source", name)
	}
	lease, err := h.takeStartLease() // the prune lock, as in startPluginWith
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", name, err)
	}
	defer lease.release()
	e, cand, err := h.validateCandidate(pl)
	if err == errKept {
		return nil, refuse("refused", "plugin %s has no config entry: stop it with hub.admin.stop, or start its entry by source", name)
	}
	if err != nil {
		return nil, refuse("refused", "%v; the running instance %s is unchanged", err, owned)
	}
	if x != nil && !x.matches(cand) {
		return nil, candidateChanged(name, cand, x, "the running instance "+owned+" is unchanged; nothing was retired or spawned")
	}
	h.mu.Lock()
	var missing string
	for _, d := range cand.Depends {
		if !h.registeredLocked(d) {
			missing = d
			break
		}
	}
	h.mu.Unlock()
	if missing != "" {
		return nil, refuse("dependency_not_ready", "plugin %s: dependency %s is not registered; the running instance %s is unchanged", name, missing, owned)
	}

	// Retirement begins: from here the old instance can be gone or partly stopped.
	ok, err := h.retire(ctx, pl)
	if err != nil {
		return nil, refuse("retire_failed", "plugin %s: instance %s was not retired: %v; nothing was spawned", name, owned, err)
	}
	if !ok {
		h.mu.Lock()
		desired = pl.Desired
		h.mu.Unlock()
		if desired == "stopped" {
			return nil, refuse("stopped", "plugin %s: the block of instance %s was closed; the plugin is stopped", name, owned)
		}
		return nil, refuse("retire_failed", "plugin %s: instance %s was not retired: its block holds a process that the hub cannot verify (see the plugin_conflict event); nothing was spawned", name, owned)
	}
	if err := h.spawn(ctx, pl, cand, lease); err != nil {
		var se *spawnError
		if errors.As(err, &se) && se.known && se.open != "" {
			// No block can exist, but the store keeps the intent open: say so, as hub.plugins does.
			return nil, refuse("spawn_failed", "plugin %s: %s: %v; the old instance %s is retired, and hub.plugins shows the open intent as pending; %s", name, spawnStageText[se.stage], se.err, owned, openIntentText[se.open])
		}
		if errors.As(err, &se) && se.known {
			return nil, refuse("spawn_failed", "plugin %s: %s: %v; the old instance %s is retired, and the plugin has no instance; start it with hub.admin.start", name, spawnStageText[se.stage], se.err, owned)
		}
		h.mu.Lock()
		id, sess := pl.InstanceID, pl.SessionID
		h.mu.Unlock()
		return nil, refuse("spawn_unknown", "plugin %s: the old instance %s is retired; spawn %s in session %s may still create a block (%v); do not retry; hub.plugins shows the open intent", name, owned, id, sess, err)
	}
	h.mu.Lock()
	if pl.entry == e {
		pl.state, pl.refusal, pl.waiting = "", "", nil // the candidate passed: a retained record is ready again
	}
	run := map[string]any{"source": pl.run.Source, "dir": pl.run.Dir}
	if pl.run.Commit != "" {
		run["commit"] = pl.run.Commit // a GitHub source; a local source has none (as in hub.plugins)
	}
	res := map[string]any{"plugin": name, "previous_instance": owned, "instance_id": pl.InstanceID, "run": run}
	h.mu.Unlock()
	h.ev.emit("plugin_replaced", res)
	go h.changed("plugins")
	return res, nil
}
