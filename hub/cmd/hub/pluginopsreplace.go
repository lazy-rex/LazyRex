package main

// The replacing phase of update and restart, and the fallback phase of update (C5 plan r1 item 5,
// with r2 A, B and C). A restart replaces the instance that it stored at admission. An update reads
// the record once and picks one call for its state, or no call:
//   - a live instance at the pin, and the selected run of the intended entry at the pin: no call;
//   - a live instance at another run: hub.admin.replace;
//   - desired running and no live instance (waiting, crashing, refused, retained, pending):
//     hub.admin.start, an explicit start, which selects the pin;
//   - desired stopped: no call; the next start runs the pin.
//
// A replace that refuses no_instance before its own effect goes to fallback (opCall), which picks
// once more from a start or no call: it never replaces. Every result with no call first checks that
// the record is still bound to the intended entry. The managed entry and the lock stay updated in
// every outcome: they are what the user asked for.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// opRecord is one read of the record of the op's plugin under h.mu.
type opRecord struct {
	entry   string // entryIdentity of the record's entry, or "none"
	desired string
	live    bool // an owned instance that is not pending, and its child runs
	run     pluginRun
	sel     *selectedRun // the run of the next automatic start (a copy)
}

func (h *Hub) readOpRecord(op *pluginOp) opRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := opRecord{entry: "none"}
	pl := h.plugins[op.Name]
	if pl == nil {
		return r
	}
	if pl.entry != nil {
		r.entry = entryIdentity(pl.entry)
	}
	owned, pending := h.ownedInstance(pl)
	r.desired, r.run = pl.Desired, pl.run
	if pl.selected != nil {
		s := *pl.selected
		r.sel = &s
	}
	r.live = owned != "" && !pending && alive(pl.ChildPID, pl.ChildStart)
	return r
}

// hasAttempt reports whether the op has an attempt row of call.
func (h *Hub) hasAttempt(ctx context.Context, op *pluginOp, call string) (bool, error) {
	var a string
	err := h.db.QueryRowContext(ctx, `SELECT attempt FROM plugin_op_attempts WHERE op_id = ? AND call = ?`, op.ID, call).Scan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// opReplacing runs the replacing phase. A call that has an attempt row already is that call's
// replay (opCall observes it, and never calls again).
func (h *Hub) opReplacing(ctx context.Context, op *pluginOp) error {
	if op.Kind == "restart" {
		return h.opCall(ctx, op, "replace")
	}
	for _, call := range []string{"replace", "start"} {
		if ok, err := h.hasAttempt(ctx, op, call); err != nil || ok {
			if err != nil {
				return err
			}
			return h.opCall(ctx, op, call)
		}
	}
	return h.opPick(ctx, op, true)
}

// opFallback runs the fallback phase: the replace is not_applied (no_instance), and its outcome
// and this phase are stored in one transaction. A start attempt is the record of the pick.
func (h *Hub) opFallback(ctx context.Context, op *pluginOp) error {
	if ok, err := h.hasAttempt(ctx, op, "start"); err != nil || ok {
		if err != nil {
			return err
		}
		return h.opCall(ctx, op, "start")
	}
	return h.opPick(ctx, op, false)
}

// opPick picks the call of an update for its record (the list at the top). replace reports whether
// a live instance at another run is replaced (replacing) or only reported (fallback).
func (h *Hub) opPick(ctx context.Context, op *pluginOp, replace bool) error {
	expect, err := opExpectEntry(op)
	if err != nil {
		return h.setOpPhase(ctx, op, "failed", "", err.Error())
	}
	x, err := opRun(op)
	if err != nil {
		return h.setOpPhase(ctx, op, "failed", "", err.Error())
	}
	want := &expectRun{}
	want.Source, _ = x["source"].(string)
	want.Commit, _ = x["commit"].(string)
	want.Digest, _ = x["digest"].(string)
	r := h.readOpRecord(op)
	if r.entry != expect {
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("%s is bound to %s now; the update wrote %s; nothing was called", op.Name, r.entry, expect))
	}
	// At the pin: the instance runs the pin, and the next automatic start runs it too (the selected
	// run of the intended entry, as "already at" requires). Else a replace selects it.
	atPin := r.live && want.matches(&r.run) && r.sel != nil && r.sel.Entry == expect && want.matches(&r.sel.Run)
	switch {
	case atPin:
		return h.setOpPhase(ctx, op, "done", fmt.Sprintf("%s runs %s", op.Name, runText(&r.run)), "")
	case r.live && replace:
		return h.opCall(ctx, op, "replace")
	case r.live:
		return h.setOpPhase(ctx, op, "done", fmt.Sprintf("%s runs %s; the update did not replace it (its replace found no instance); the next start runs %s", op.Name, runText(&r.run), want), "")
	case r.desired == "running":
		return h.opCall(ctx, op, "start")
	}
	return h.setOpPhase(ctx, op, "done", fmt.Sprintf("%s is stopped; its next start runs %s", op.Name, want), "")
}
