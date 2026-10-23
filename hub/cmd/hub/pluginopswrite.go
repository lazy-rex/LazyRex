package main

// The writing phase of add and update (C4 plan r2, item 2, with codex-27's correction A): the checks
// under both plugin command flocks before the managed write. The write itself is opWrite
// (pluginops.go).

import (
	"context"
	"encoding/json"
	"fmt"
)

// writeChecks are the checks of an add's or an update's managed write. The caller holds both
// plugin command flocks. They run in one pass, before any write, also when an earlier phase made
// the same check: that check is only a hint.
//  1. The pin: the lock entry of the key is the op's pin (lockBase: the pin, not the cache
//     directory). Else conflict.
//  2. The directory: the lock entry as it is now verifies (lockedDir). Else the phase goes back
//     to locking, which repairs it (C3): it writes the op's own verified entry, or it goes back to
//     fetching when that entry does not verify either.
//  3. The graph, before and after: with the current lock, every refusal that the managed file with
//     the op's entry has, and the current file does not have, is conflict. So no entry that is
//     valid now becomes refused (lockRefusal).
//
// stop reports that the checks set the phase: the caller writes nothing.
func (h *Hub) writeChecks(ctx context.Context, op *pluginOp) (stop bool, err error) {
	var t opResolved
	var pin lockEntry
	if json.Unmarshal([]byte(op.Target), &t) != nil || json.Unmarshal([]byte(op.Pin), &pin) != nil {
		return true, h.setOpPhase(ctx, op, "failed", "", "write: the stored target or pin does not read; nothing was written")
	}
	e := &configEntry{Source: t.Source}
	if err := e.checkSource(""); err != nil {
		return true, h.setOpPhase(ctx, op, "failed", "", "write: "+err.Error())
	}
	lockFile := lockPath(h.cfg.path, h.cfg.Lock)
	lock, err := readLock(lockFile)
	if err != nil {
		return true, h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("the lock %s: %v; nothing more was written", lockFile, err))
	}
	want := lockBase(&pluginLock{Plugins: map[string]*lockEntry{t.Source: &pin}}, t.Source)
	if cur := lockBase(lock, t.Source); cur != want {
		return true, h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("the lock changed since the %s wrote its pin: %s is %s, not %s; nothing more was written", op.Kind, t.Source, cur, want))
	}
	if _, _, err := lockedDir(h.cfg.path, h.cfg.State, lock, e); err != nil {
		return true, h.setOpPhase(ctx, op, "locking", fmt.Sprintf("the lock's cache entry of %s does not verify: %v; locking again", t.Source, err), "")
	}
	msg, err := h.lockRefusal(op, t, lockFile, lock, lock, fmt.Sprintf("the %s of %s", op.Kind, op.Entry))
	if err != nil {
		msg = err.Error()
	}
	if msg != "" {
		return true, h.setOpPhase(ctx, op, "conflict", "", msg+"; nothing more was written")
	}
	return false, nil
}
