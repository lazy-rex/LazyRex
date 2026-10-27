package main

// In-place replacement (lead-bug-404, contract rev 3, 2026-10-26). A replacement in the side slot
// of a window keeps the old pane and its renderer process: the pane is neither closed nor
// created. Closing the pane that the Rex app had focused raced the app's own key and focus
// requests, and the app showed "block ... not found in any window" (live Mac hub, Rex
// 0.0.1792787366-g623b2d).
//
//   - The decision point is the only fallback point (inPlaceOK, under the window lock): the old
//     view is open, attached and has a block, and client.inspect of its renderer lists
//     view.become. Otherwise the replacement opens a new pane as before (spawnSlot).
//   - The transfer is one transaction (transferInPlace): the new view row takes the old row's
//     block and child process, the old row closes with closed_by 'replaced' and no pane close, and
//     the replacement row goes to becoming. After the commit the hub never opens a pane for it.
//   - In becoming the hub owes two things, outside the window lock (becomeWorker): the pane label
//     lazyrex-form:<new> (session.set_block_label), and view.become to the old renderer, which
//     re-executes itself as the new view in the same process. The row is done when the label is
//     set and the new view attached from inside the block. A lost reply changes nothing: the
//     attach is the proof. A gone block or a dead renderer ends the row late_closed.
//   - Identity: hub.view.attach of a view that a transfer replaced answers {status: replaced,
//     current_view_id, plugin, route}, with the block's current open row, when the caller runs in
//     that row's process (viewAttach). So a renderer that reloads with an old argv lands on the
//     current view, also after A -> B -> C.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"time"
)

// inspectTimeout bounds client.inspect at the decision point; it runs under the window lock.
const inspectTimeout = time.Second

// inPlaceOK reports whether r can replace its old view in place. It runs under the window lock,
// before anything about the new view is durable, so every failure here means the fallback.
func (h *Hub) inPlaceOK(ctx context.Context, r *slotRow) bool {
	if r.Kind != "slot" || r.Old == "" {
		return false
	}
	var status, attachState, block, client string
	var pid int
	var start string
	if h.db.QueryRowContext(ctx, `SELECT status, attach_state, block_id, client_id, child_pid, child_start FROM views WHERE view_id = ?`, r.Old).
		Scan(&status, &attachState, &block, &client, &pid, &start) != nil {
		return false
	}
	if status != "open" || attachState != "attached" || block == "" || client == "" || !alive(pid, start) {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	var out struct {
		Actions []struct {
			Name string `json:"name"`
		} `json:"actions"`
	}
	if err := h.rex.Call(cctx, "client.inspect", map[string]any{"client_id": client}, &out); err != nil {
		h.ev.emit("slot_in_place_refused", map[string]any{"replacement_id": r.ID, "error": err.Error()})
		return false
	}
	return slices.ContainsFunc(out.Actions, func(a struct {
		Name string `json:"name"`
	}) bool {
		return a.Name == "view.become"
	})
}

// transferInPlace gives the old view's pane to the new view, in one transaction. The caller holds
// the window lock and checked inPlaceOK. A becoming row whose new view is r.Old (a transfer that
// still waits for its proofs) is done: r takes over its block, and its worker stops.
func (h *Hub) transferInPlace(ctx context.Context, r *slotRow) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO views (view_id, plugin, tab, route, status, context, session_id, window_id, block_id, child_pid, child_start, attach_state)
		SELECT ?, ?, ?, ?, 'open', ?, session_id, window_id, block_id, child_pid, child_start, 'never'
		FROM views WHERE view_id = ? AND status = 'open' AND attach_state = 'attached' AND block_id != ''`,
		r.New, r.Plugin, formTab, r.Route, r.Context, r.Old)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errSlotMoved
	}
	// The old row keeps its client and attachment: view.become names that pair.
	if _, err := tx.ExecContext(ctx, `UPDATE views SET status = 'closed', closed_by = 'replaced' WHERE view_id = ? AND status = 'open'`, r.Old); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE ui_replacements SET state = 'becoming' WHERE repl_id = ? AND state = 'opening'`, r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errSlotMoved
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ui_replacements SET state = 'done' WHERE new_view = ? AND state = 'becoming'`, r.Old); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.State = "becoming"
	h.forgetOwn(r.Old)
	h.forgetView(r.Old)
	h.ev.emit("slot_state", map[string]any{"replacement_id": r.ID, "state": "becoming", "in_place": true})
	h.ev.emit("form_closed", map[string]any{"view_id": r.Old, "by": "hub", "why": "replaced"})
	var fc formContext
	json.Unmarshal([]byte(r.Context), &fc)
	h.ev.emit("form_opened", map[string]any{"plugin": r.Plugin, "view_id": r.New, "route": r.Route, "session_id": fc.SessionID, "window_id": fc.WindowID, "source_view_id": fc.SourceViewID})
	h.startBecome(r.ID, r.Window)
	return nil
}

// becomeTarget is what a becoming row's worker reads under the window lock.
type becomeTarget struct {
	block, session, newStatus, attachState string
	pid                                    int
	start                                  string
	oldView, oldClient, oldAttachment      string
	plugin, route                          string
}

func (h *Hub) readBecome(ctx context.Context, r *slotRow) (becomeTarget, error) {
	var t becomeTarget
	err := h.db.QueryRowContext(ctx, `SELECT block_id, session_id, status, attach_state, child_pid, child_start FROM views WHERE view_id = ?`, r.New).
		Scan(&t.block, &t.session, &t.newStatus, &t.attachState, &t.pid, &t.start)
	if err != nil {
		return t, err
	}
	err = h.db.QueryRowContext(ctx, `SELECT view_id, client_id, attachment FROM views WHERE view_id = ?`, r.Old).Scan(&t.oldView, &t.oldClient, &t.oldAttachment)
	t.plugin, t.route = r.Plugin, r.Route
	return t, err
}

// becomeBackoff is the wait between two view.become requests of one row: 1 s, doubled, at most
// 60 s (the plugin respawn rule). An attach of the new view wakes the worker at once.
func becomeBackoff(n int) time.Duration {
	d := time.Second << min(n, 6)
	return min(d, 60*time.Second)
}

// startBecome starts the worker of a becoming row, once per row in this hub process.
func (h *Hub) startBecome(id, window string) {
	h.slots.mu.Lock()
	if h.slots.becoming == nil {
		h.slots.becoming = map[string]chan struct{}{}
	}
	if _, ok := h.slots.becoming[id]; ok {
		h.slots.mu.Unlock()
		return
	}
	wake := make(chan struct{}, 1)
	h.slots.becoming[id] = wake
	h.slots.mu.Unlock()
	go h.becomeWorker(id, window, wake)
}

// wakeBecome wakes the worker of the becoming row whose new view is viewID (its attach happened).
func (h *Hub) wakeBecome(ctx context.Context, viewID string) {
	var id string
	if h.db.QueryRowContext(ctx, `SELECT repl_id FROM ui_replacements WHERE new_view = ? AND state = 'becoming'`, viewID).Scan(&id) != nil {
		return
	}
	h.slots.mu.Lock()
	wake := h.slots.becoming[id]
	h.slots.mu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// becomeWorker owns one becoming row until it ends. Each step re-reads the row under the window
// lock, so a stale worker (its row ended, or a newer transfer took the block) renames nothing and
// notifies nobody. The rename runs under the lock: it is a server call, not a client wait. The
// view.become request runs outside the lock: the renderer can call the hub while it answers.
func (h *Hub) becomeWorker(id, window string, wake chan struct{}) {
	defer func() {
		h.slots.mu.Lock()
		delete(h.slots.becoming, id)
		h.slots.mu.Unlock()
	}()
	ctx := context.Background()
	lock := h.windowLock(window)
	labeled := false
	for n := 0; ; n++ {
		lock.Lock()
		r, err := h.slotRow(ctx, h.db, id)
		if err != nil || r == nil || r.State != "becoming" {
			lock.Unlock()
			return
		}
		t, err := h.readBecome(ctx, r)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			lock.Unlock()
			log.Printf("slot %s: becoming: %v", id, err)
			becomeWait(wake, n)
			continue
		}
		if t.newStatus != "open" {
			// The block was closed or the renderer exited: the paths that saw it closed the new row.
			h.setSlotState(ctx, r, "late_closed")
			lock.Unlock()
			return
		}
		if !alive(t.pid, t.start) {
			// The renderer died and the pane stays (a held pane): no process can attach.
			if won, _ := h.closeViewOnce(ctx, r.New, "exit", false); won {
				h.ev.emit("form_closed", map[string]any{"plugin": r.Plugin, "view_id": r.New, "by": "exit", "why": "exited"})
			}
			h.setSlotState(ctx, r, "late_closed")
			lock.Unlock()
			return
		}
		if !labeled {
			err := h.rex.Call(ctx, "session.set_block_label", map[string]any{"session_id": t.session, "block_id": t.block, "label": "lazyrex-form:" + r.New}, nil)
			labeled = err == nil
			if err != nil {
				log.Printf("slot %s: label: %v", id, err)
			}
		}
		if labeled && t.attachState == "attached" {
			h.setSlotState(ctx, r, "done")
			lock.Unlock()
			return
		}
		lock.Unlock()
		if t.attachState != "attached" && t.oldClient != "" {
			f := h.takeFault(func(f *testFault) bool {
				return f.Point == "view-become" && f.Plugin == r.Plugin && (f.Key == "" || f.Key == r.Route)
			})
			if f != nil && f.Mode == "hold" {
				h.waitRelease(f)
			}
			var err error
			if f == nil || f.Mode != "drop" {
				cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
				err = h.rex.Call(cctx, "client.perform", map[string]any{"client_id": t.oldClient, "action": "view.become",
					"args": map[string]any{"view_id": t.oldView, "attachment": t.oldAttachment, "next_view_id": r.New,
						"next_plugin": t.plugin, "next_route": t.route, "replacement_id": r.ID}, "timeout_ms": 5000}, nil)
				cancel()
			}
			if f != nil && (f.Mode == "drop" || f.Mode == "lose-reply") {
				err = errors.New("test fault " + f.Mode + ": the answer is lost")
			}
			h.ev.emit("slot_become", map[string]any{"replacement_id": r.ID, "view_id": r.New, "error": errString(err)})
		}
		becomeWait(wake, n)
	}
}

// becomeWait waits for the next step of a becoming row: an attach wakes it, else the backoff.
func becomeWait(wake chan struct{}, n int) {
	select {
	case <-wake:
	case <-time.After(becomeBackoff(n)):
	}
}

// currentOfBlock is the open view row that owns the block of the replaced view x, for an attach
// of x: the transfers keep one block and one child process, so the open row with that block and
// child is the end of the chain (A -> B -> C gives C). ok is false when x was not replaced in
// place or the chain ended.
func (h *Hub) currentOfBlock(ctx context.Context, x string) (id, plugin, route string, pid int, start string, ok bool) {
	var block, closedBy string
	var xpid int
	var xstart string
	if h.db.QueryRowContext(ctx, `SELECT block_id, closed_by, child_pid, child_start FROM views WHERE view_id = ? AND status = 'closed'`, x).
		Scan(&block, &closedBy, &xpid, &xstart) != nil || closedBy != "replaced" || block == "" {
		return
	}
	if h.db.QueryRowContext(ctx, `SELECT view_id, plugin, route, child_pid, child_start FROM views
		WHERE block_id = ? AND status = 'open' AND child_pid = ? AND child_start = ? ORDER BY rowid DESC LIMIT 1`, block, xpid, xstart).
		Scan(&id, &plugin, &route, &pid, &start) != nil {
		return
	}
	return id, plugin, route, pid, start, true
}

// blockOwnedByOther reports whether an open row other than viewID owns block. A pane close for a
// view row skips such a block: the pane runs the other view now (an in-place transfer).
func (h *Hub) blockOwnedByOther(ctx context.Context, block, viewID string) bool {
	if block == "" {
		return false
	}
	var n int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM views WHERE block_id = ? AND view_id != ? AND status != 'closed'`, block, viewID).Scan(&n)
	return n > 0
}
