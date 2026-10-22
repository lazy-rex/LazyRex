package main

// Side panels (the maintainer, 2026-10-17: "clicking on a plugin opens that plugin's landing as a side
// panel"; each tab has at most one panel).
//
// Every window (tab) has one side slot. Forms and panels share it. A panel is a form view whose
// stored context has panel: true; it stays open after its actions, and Esc or Ctrl+Q closes it
// (hub.view.close). A command with surface "panel" opens a panel; a view action can open one with
// the effect {panel: {command} | {route} | {plugin, command}}.
//
// Opening into a slot that holds a view replaces it (contract agreed with Codex, 2026-10-17):
//   - The hub asks the old renderer view.release {replacement_id}. "released": the renderer froze
//     its input. "confirm": unsaved input; the renderer asks the user (Keep editing / Discard and
//     replace) and answers with hub.view.release_confirm.
//   - A view that no renderer has attached to (attach_state never) holds no input: the hub claims
//     it without view.release (claimUnattached). A renderer that attaches later follows the
//     replacement from hub.view.attach and reads its state once (hub.view.replacement). A view
//     that attached before and is not attached now, or a row from an older hub, is a conflict.
//   - The new pane opens with new_split anchored at the old pane, then the hub closes the old pane.
//     The new pane gets the old pane's area (lab: spikes/lab/slot-replace-probe.mjs).
//   - Each replacement is a durable row (ui_replacements). The new pane is asked for once per
//     row: an unknown new_split outcome leaves the row unresolved, and the hub only looks for the
//     pane by its unique label (session events, start). It never sends a second new_split.
//   - A form that replaced a panel records the panel as its return. When the form ends, the panel
//     opens again in that window, if the window still exists and its slot is empty.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// states: releasing (view.release asked), confirming (the user decides), opening (new_split sent;
// an unknown outcome stays here), closing (the new pane exists; the old one closes), done,
// cancelled, superseded (an unresolved row that a newer open in its window replaced; the old view
// was resumed with the old ID first; the hub still closes the late pane), user_closed (the user
// closed the old view while the row waited; the hub closes a late pane), late_closed, skipped (a
// closed form's return in layer mode whose focus check failed; terminal, no pane was asked for).
// return_of is the closed form of such a return (” for every other row).
const panelSchema = `
CREATE TABLE IF NOT EXISTS ui_replacements (repl_id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
  window_id TEXT NOT NULL, old_view TEXT NOT NULL DEFAULT '', new_view TEXT NOT NULL, plugin TEXT NOT NULL,
  route TEXT NOT NULL, context TEXT NOT NULL, focus INTEGER NOT NULL DEFAULT 1, state TEXT NOT NULL,
  next_title TEXT NOT NULL DEFAULT '', created TEXT NOT NULL, return_of TEXT NOT NULL DEFAULT '');
`

// slotReturn is the panel that a form replaced. The form's end opens it again.
type slotReturn struct {
	Plugin  string      `json:"plugin"`
	Route   string      `json:"route"`
	Context formContext `json:"context"`
}

type slotState struct {
	mu   sync.Mutex
	wins map[string]*sync.Mutex // window -> lock; one replacement at a time per window
}

func (h *Hub) windowLock(window string) *sync.Mutex {
	h.slots.mu.Lock()
	defer h.slots.mu.Unlock()
	if h.slots.wins == nil {
		h.slots.wins = map[string]*sync.Mutex{}
	}
	m := h.slots.wins[window]
	if m == nil {
		m = &sync.Mutex{}
		h.slots.wins[window] = m
	}
	return m
}

func slotViewID(repl string) string {
	sum := sha256.Sum256([]byte("slot\x00" + repl))
	return "view-form-" + hex.EncodeToString(sum[:8])
}

type slotRequest struct {
	ReplID      string // idempotency: the same ID answers the same replacement
	Plugin      string
	Route       string
	Ctx         formContext // session, window, source; Panel set for a panel
	Focus       bool
	ExpectOld   string // replace only this view (the form that ran the action); release is still asked
	ExpectEmpty bool   // open only into an empty slot (a return after the form closed)
	NextTitle   string // shown by the old renderer while it waits
	// ReturnOf is the closed form of a return in layer mode: the pane opens, with focus, only if
	// the focus is still on Ctx.UnderBlock just before the open (spawnSlot); else the row is skipped.
	ReturnOf string
}

type slotRow struct {
	ID, Session, Window, Old, New, Plugin, Route, Context, State, Title, ReturnOf string
	Focus                                                                         bool
}

func (h *Hub) slotRow(ctx context.Context, q querier, id string) (*slotRow, error) {
	var r slotRow
	err := q.QueryRowContext(ctx, `SELECT repl_id, session_id, window_id, old_view, new_view, plugin, route, context, focus, state, next_title, return_of
		FROM ui_replacements WHERE repl_id = ?`, id).Scan(&r.ID, &r.Session, &r.Window, &r.Old, &r.New, &r.Plugin, &r.Route, &r.Context, &r.Focus, &r.State, &r.Title, &r.ReturnOf)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

// slotOccupant is the open (or opening) side view of a window, if any. New views of unresolved
// replacements do not count: they are not known to exist.
func (h *Hub) slotOccupant(ctx context.Context, window string) (id, block, raw string, err error) {
	err = h.db.QueryRowContext(ctx, `SELECT view_id, block_id, context FROM views
		WHERE tab = ? AND status = 'open' AND json_extract(context, '$.window_id') = ? ORDER BY rowid DESC LIMIT 1`, formTab, window).Scan(&id, &block, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", nil
	}
	return
}

// openSlot opens plugin's route in the side slot of req.Ctx.WindowID. It answers {status, view_id,
// replacement_id}: status opened, pending_confirmation, or opening (the outcome is unknown; the
// hub watches for the pane).
func (h *Hub) openSlot(ctx context.Context, req slotRequest) (map[string]any, error) {
	if h.cfg.View == "" {
		return nil, errors.New("no renderer: set \"view\" in the hub config")
	}
	if req.ReplID == "" {
		req.ReplID = randomID("repl-")
	}
	lock := h.windowLock(req.Ctx.WindowID)
	lock.Lock()
	defer lock.Unlock()
	r, err := h.slotRow(ctx, h.db, req.ReplID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		if r, err = h.newSlotRow(ctx, req); err != nil {
			return nil, err
		}
		if r.Old != "" {
			if err := h.askRelease(ctx, r); err != nil {
				return nil, err
			}
		}
	}
	return h.advanceSlot(ctx, r)
}

// newSlotRow records a replacement: it checks the slot, supersedes an unresolved row of the same
// window, and keeps a panel's return on a form that replaces it.
func (h *Hub) newSlotRow(ctx context.Context, req slotRequest) (*slotRow, error) {
	var busy int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_replacements WHERE window_id = ? AND state IN ('releasing', 'confirming', 'closing')`, req.Ctx.WindowID).Scan(&busy)
	if busy > 0 {
		return nil, refuse("conflict", "the side panel of this tab is changing; try again")
	}
	if req.ExpectEmpty {
		// A return opens only into an empty slot: a view the user opened after the form closed
		// (or one still opening) keeps it (Codex's review). The window lock makes this exact.
		var n int
		h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_replacements WHERE window_id = ? AND state = 'opening'`, req.Ctx.WindowID).Scan(&n)
		if old, _, _, err := h.slotOccupant(ctx, req.Ctx.WindowID); err != nil {
			return nil, err
		} else if old != "" || n > 0 {
			return nil, refuse("conflict", "the side panel of this tab holds another view")
		}
	}
	// An unresolved row of this window is superseded. Its old renderer is frozen for it: the hub
	// resumes it with the old ID before it asks for a new release (Codex's review).
	prevRows, err := h.db.QueryContext(ctx, `SELECT repl_id FROM ui_replacements WHERE window_id = ? AND state = 'opening'`, req.Ctx.WindowID)
	if err != nil {
		return nil, err
	}
	var prevIDs []string
	for prevRows.Next() {
		var id string
		prevRows.Scan(&id)
		prevIDs = append(prevIDs, id)
	}
	prevRows.Close()
	for _, id := range prevIDs {
		r, err := h.slotRow(ctx, h.db, id)
		if err != nil || r == nil {
			return nil, fmt.Errorf("supersede %s: %v", id, err)
		}
		if err := h.setSlotState(ctx, r, "superseded"); err != nil && !errors.Is(err, errSlotMoved) {
			return nil, err
		}
		if r.State == "superseded" && r.Old != "" {
			h.resumeOldSync(ctx, r)
		}
	}
	old, _, raw, err := h.slotOccupant(ctx, req.Ctx.WindowID)
	if err != nil {
		return nil, err
	}
	if req.ExpectOld != "" && old != "" && old != req.ExpectOld {
		// The form that ran the action is gone and another view holds the slot: leave it.
		return nil, refuse("conflict", "the side panel of this tab holds another view")
	}
	if old != "" && !req.Ctx.Panel {
		// A form replaces a panel: the form's end opens the panel again. A form that replaces a
		// form keeps that form's return.
		var prev formContext
		json.Unmarshal([]byte(raw), &prev)
		if prev.Panel {
			var plugin, route string
			h.db.QueryRowContext(ctx, `SELECT plugin, route FROM views WHERE view_id = ?`, old).Scan(&plugin, &route)
			pc := prev
			pc.Return = nil
			req.Ctx.Return = &slotReturn{Plugin: plugin, Route: route, Context: pc}
		} else {
			req.Ctx.Return = prev.Return
		}
	}
	if h.cfg.UISlot == "layer" {
		// The under block is captured once, before the slot's first layer has the focus. A
		// replacement keeps the replaced view's value (also an empty one from an older row); a
		// return keeps the value of the panel that it opens again.
		if old != "" {
			var prev formContext
			json.Unmarshal([]byte(raw), &prev)
			req.Ctx.UnderBlock = prev.UnderBlock
		} else if req.ReturnOf == "" {
			req.Ctx.UnderBlock = h.slotUnder(ctx, req.Ctx.SessionID, req.Ctx.WindowID)
		}
	}
	b, _ := json.Marshal(req.Ctx)
	state := "opening"
	if old != "" {
		state = "releasing"
	}
	r := &slotRow{ID: req.ReplID, Session: req.Ctx.SessionID, Window: req.Ctx.WindowID, Old: old, New: slotViewID(req.ReplID),
		Plugin: req.Plugin, Route: req.Route, Context: string(b), Focus: req.Focus, State: state, Title: req.NextTitle, ReturnOf: req.ReturnOf}
	if r.Title == "" {
		r.Title = h.commandTitle(ctx, r.Plugin, r.Route)
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, focus, state, next_title, created, return_of)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, r.Session, r.Window, r.Old, r.New, r.Plugin, r.Route, r.Context, r.Focus, r.State, r.Title, now(), r.ReturnOf); err != nil {
		return nil, err
	}
	h.ev.emit("slot_replacement", map[string]any{"replacement_id": r.ID, "window_id": r.Window, "old_view": r.Old, "new_view": r.New, "route": r.Route, "state": r.State})
	return r, nil
}

// errSlotMoved: another path (a user close) changed the row's state first. r holds the new state.
var errSlotMoved = errors.New("the replacement state changed")

// setSlotState moves r from its current state to to. It fails with errSlotMoved when the row is no
// longer in r's state (slotOldClosed runs without the window lock).
func (h *Hub) setSlotState(ctx context.Context, r *slotRow, to string) error {
	res, err := h.db.ExecContext(ctx, `UPDATE ui_replacements SET state = ? WHERE repl_id = ? AND state = ?`, to, r.ID, r.State)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		h.db.QueryRowContext(ctx, `SELECT state FROM ui_replacements WHERE repl_id = ?`, r.ID).Scan(&r.State)
		return errSlotMoved
	}
	r.State = to
	h.ev.emit("slot_state", map[string]any{"replacement_id": r.ID, "state": to})
	if (to == "cancelled" || to == "superseded") && r.Old != "" {
		// The replacement ended without the slot: a view it claimed can attach as new again. A view
		// that attached meanwhile stays attached and gets view.resume. The claim stays while another
		// replacement of the view is active: that one can hold it (a loser's cancel must not undo the
		// winner's claim). The rule is the same as in slotStart.
		h.db.ExecContext(ctx, `UPDATE views SET attach_state = 'never' WHERE view_id = ? AND attach_state = 'claimed' AND view_id NOT IN
			(SELECT old_view FROM ui_replacements WHERE state IN ('releasing', 'confirming', 'opening', 'closing'))`, r.Old)
	}
	return nil
}

// claimUnattached gives a never-attached old view to r without asking a renderer: no renderer has
// shown the view, so it holds no user input. The claim and the move to opening are one
// transaction. It reports false (and changes nothing) when the view attached, closed or was
// claimed first. A renderer that attaches after the claim gets the replacement from
// hub.view.attach and freezes (view/panels.go), as one that answered released.
func (h *Hub) claimUnattached(ctx context.Context, r *slotRow) (bool, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE views SET attach_state = 'claimed' WHERE view_id = ? AND status = 'open' AND attachment = '' AND attach_state = 'never'`, r.Old)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	res, err = tx.ExecContext(ctx, `UPDATE ui_replacements SET state = 'opening' WHERE repl_id = ? AND state = 'releasing'`, r.ID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	r.State = "opening"
	h.ev.emit("slot_release_unattached", map[string]any{"replacement_id": r.ID, "view_id": r.Old})
	h.ev.emit("slot_state", map[string]any{"replacement_id": r.ID, "state": "opening"})
	return true, nil
}

// askRelease asks the old renderer to give up the slot. It moves the row to opening (released, or
// a never-attached view claimed), confirming (the user decides), or cancelled (no answer, a
// refusal, or a renderer that attached before and is not attached now: it can hold input).
func (h *Hub) askRelease(ctx context.Context, r *slotRow) error {
	var client, attachment, attachState string
	read := func() {
		client, attachment, attachState = "", "", ""
		h.db.QueryRowContext(ctx, `SELECT client_id, attachment, attach_state FROM views WHERE view_id = ? AND status = 'open'`, r.Old).Scan(&client, &attachment, &attachState)
	}
	read()
	if attachment == "" && attachState == "never" {
		claimed, err := h.claimUnattached(ctx, r)
		if err != nil {
			return err
		}
		if claimed {
			return nil
		}
		read() // once: the attach usually won; anything else is the conflict below
	}
	var out struct {
		Status string `json:"status"`
	}
	var err error
	if attachment == "" {
		err = errors.New("the side panel's renderer is reconnecting; try again in a moment")
	} else {
		cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		var res struct {
			Result json.RawMessage `json:"result"`
		}
		err = h.rex.Call(cctx, "client.perform", map[string]any{"client_id": client, "action": "view.release",
			"args": map[string]any{"view_id": r.Old, "attachment": attachment, "replacement_id": r.ID, "next_title": r.Title}, "timeout_ms": 5000}, &res)
		cancel()
		if err == nil {
			err = json.Unmarshal(res.Result, &out)
		}
	}
	switch {
	case err == nil && out.Status == "released":
		err = h.setSlotState(ctx, r, "opening")
	case err == nil && out.Status == "confirm":
		err = h.setSlotState(ctx, r, "confirming")
	default:
		goto refused
	}
	if errors.Is(err, errSlotMoved) {
		return nil // advanceSlot answers from the new state
	}
	return err
refused:
	// The terminal state is durable before the renderer resumes, so a reattach or a
	// release_confirm never sees releasing (Codex's review).
	if serr := h.setSlotState(ctx, r, "cancelled"); serr != nil && !errors.Is(serr, errSlotMoved) {
		return serr
	}
	h.resumeOld(r)
	if err == nil {
		err = fmt.Errorf("answer %q", out.Status)
	}
	return refuse("conflict", "the side panel did not give up its place: %v", err)
}

// resumeOld tells the old renderer that the replacement ended without it: it unfreezes. Best
// effort; a renderer that reattaches learns the state from hub.view.attach.
func (h *Hub) resumeOld(r *slotRow) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		h.resumeOldSync(ctx, r)
	}()
}

func (h *Hub) resumeOldSync(ctx context.Context, r *slotRow) {
	var client, attachment string
	h.db.QueryRowContext(ctx, `SELECT client_id, attachment FROM views WHERE view_id = ? AND status = 'open'`, r.Old).Scan(&client, &attachment)
	if attachment == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	err := h.rex.Call(cctx, "client.perform", map[string]any{"client_id": client, "action": "view.resume",
		"args": map[string]any{"view_id": r.Old, "attachment": attachment, "replacement_id": r.ID}, "timeout_ms": 5000}, nil)
	h.ev.emit("slot_resume", map[string]any{"replacement_id": r.ID, "view_id": r.Old, "error": errString(err)})
}

// advanceSlot moves a row as far as it can go now. The caller holds the window lock.
func (h *Hub) advanceSlot(ctx context.Context, r *slotRow) (map[string]any, error) {
	answer := func(status string) map[string]any {
		return map[string]any{"status": status, "view_id": r.New, "replacement_id": r.ID}
	}
	switch r.State {
	case "confirming", "releasing":
		return answer("pending_confirmation"), nil
	case "cancelled":
		return nil, refuse("conflict", "the replacement was cancelled")
	case "superseded", "user_closed":
		// A newer open replaced this one (the old view was resumed), or the user closed the old
		// view: a new pane that exists is closed, never kept.
		var status string
		h.db.QueryRowContext(ctx, `SELECT status FROM views WHERE view_id = ?`, r.New).Scan(&status)
		if status == "open" && h.closeForm(ctx, r.New, "late") == nil {
			h.setSlotState(ctx, r, "late_closed")
		}
		return nil, refuse("conflict", "the side panel changed while this one opened")
	case "late_closed":
		return nil, refuse("conflict", "the side panel changed while this one opened")
	case "skipped":
		return nil, refuse("conflict", "the panel did not open again: the focus is not where the form opened")
	case "done":
		return answer("opened"), nil
	case "opening":
		var status string
		err := h.db.QueryRowContext(ctx, `SELECT status FROM views WHERE view_id = ?`, r.New).Scan(&status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := h.spawnSlot(ctx, r); err != nil {
				return nil, err
			}
			if r.State == "opening" {
				return answer("opening"), nil // unknown outcome: the hub watches for the pane
			}
		case err != nil:
			return nil, err
		case status == "sending":
			if !h.resolveSlotPane(ctx, r) {
				return answer("opening"), nil
			}
		default:
			if err := h.setSlotState(ctx, r, "closing"); errors.Is(err, errSlotMoved) {
				return h.advanceSlot(ctx, r)
			} else if err != nil {
				return nil, err
			}
		}
	}
	if r.State == "closing" {
		if r.Old != "" {
			if err := h.closeForm(ctx, r.Old, "replaced"); err != nil {
				return nil, err
			}
		}
		if err := h.setSlotState(ctx, r, "done"); err != nil && !errors.Is(err, errSlotMoved) {
			return nil, err
		}
	}
	return h.advanceSlot(ctx, r)
}

// spawnSlot asks Rex for the new pane, once per row. A definite refusal cancels the row and
// resumes the old view; an unknown outcome leaves the view row 'sending' and the row 'opening'.
func (h *Hub) spawnSlot(ctx context.Context, r *slotRow) error {
	if r.ReturnOf != "" {
		// The last check before new_layer, under the window lock. The read and the open are not
		// one step: the focus can still move in between (docs/hub.md).
		var fc formContext
		json.Unmarshal([]byte(r.Context), &fc)
		if why := h.returnFocusWhy(ctx, r.Session, r.Window, fc.UnderBlock); why != "" {
			return h.skipReturn(ctx, r, why)
		}
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO views (view_id, plugin, tab, route, status, context, session_id, attach_state) VALUES (?, ?, ?, ?, 'sending', ?, ?, 'never')`,
		r.New, r.Plugin, formTab, r.Route, r.Context, r.Session); err != nil {
		return err
	}
	if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": r.Session, "event_scope": "session"}, nil); err != nil {
		return h.slotRefused(ctx, r, err) // nothing was sent
	}
	args := map[string]any{"session_id": r.Session, "direction": "horizontal", "side": "after", "ratio": 0.55, "focus": r.Focus,
		"layout": map[string]any{"block": map[string]any{
			"flavor": "com.superlogical.terminal.shell",
			"label":  "lazyrex-form:" + r.New,
			"options": map[string]any{
				"command": h.viewCommand(r.New, r.Plugin, r.Route),
				"login":   false,
				"exit":    map[string]any{"on_completion": true},
			},
		}}}
	var oldBlock string
	if r.Old != "" {
		h.db.QueryRowContext(ctx, `SELECT block_id FROM views WHERE view_id = ? AND status = 'open'`, r.Old).Scan(&oldBlock)
	}
	if oldBlock != "" {
		args["anchor_block_id"] = oldBlock // the new pane takes the old pane's area
	} else if tb := h.pairAnchor(ctx, r.Window); tb != "" {
		args["anchor_block_id"] = tb // beside the history; wrapPair then puts the composer under it (pair.go)
	} else {
		args["window_id"] = r.Window
	}
	var out struct {
		BlockIDs []string `json:"block_ids"`
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := h.slotPane(cctx, r.Window, args, &out, slotSplit)
	cancel()
	var re *rex.RexError
	if err != nil && errors.As(err, &re) && re.Code != "disconnected" && !transient(err) {
		return h.slotRefused(ctx, r, err)
	}
	if err != nil || len(out.BlockIDs) != 1 {
		h.ev.emit("slot_unknown", map[string]any{"replacement_id": r.ID, "view_id": r.New, "error": errString(err)})
		return nil // unresolved: resolveSlotPane looks for the label later
	}
	var fc formContext
	json.Unmarshal([]byte(r.Context), &fc)
	if err := h.adoptForm(ctx, &view{ID: r.New, Plugin: r.Plugin, Tab: formTab, Route: r.Route}, fc, out.BlockIDs[0]); err != nil {
		return nil // the pane exists; a later look adopts it
	}
	if err := h.setSlotState(ctx, r, "closing"); err != nil && !errors.Is(err, errSlotMoved) {
		return err
	}
	return nil // a moved row (superseded) closes the new pane in advanceSlot
}

// slotSplit sends the one pane request of a replacement. Lab tests replace it to lose the answer.
var slotSplit = func(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
	return h.rex.Call(ctx, method, args, out)
}

// slotLayerBounds is the floating layer of a form or panel with ui_slot "layer": [x, y, w, h],
// normalized (the bounds that the user checked on the Rex app, build 1347).
var slotLayerBounds = []float64{0.15, 0.15, 0.7, 0.6}

// slotPane opens the pane of a form or panel with call: the split of args (ui_slot "split", the
// default), or with ui_slot "layer" a frontmost floating layer in window, with the same block and
// focus. Observed on Rex 0.0.1792158932-g88fd41 (lab, 2026-10-22): block.close of a layer's only
// block, or the block's exit, removes the layer, and focus goes back to the block under it; with
// two layers, focus goes to the other layer's block. So a form's close closes its layer, and a
// replacement (a new layer, then the old block closes) needs no layer call. The answer of
// new_layer has block_ids, as new_split, and no anchor_block_id.
func (h *Hub) slotPane(ctx context.Context, window string, args map[string]any, out any, call func(*Hub, context.Context, string, map[string]any, any) error) error {
	if h.cfg.UISlot != "layer" {
		return call(h, ctx, "session.new_split", args, out)
	}
	layer := map[string]any{"session_id": args["session_id"], "window_id": window, "bounds": slotLayerBounds,
		"focus": args["focus"], "layout": args["layout"]}
	return call(h, ctx, "session.new_layer", layer, out)
}

// slotFocus reads the focus of window: whether it is the session's active window, its focused
// block, and whether that block is in its tiled layer. A window that is not in the session is an
// error.
func (h *Hub) slotFocus(ctx context.Context, sess, window string) (active bool, focused string, tiled bool, err error) {
	var sv struct {
		ActiveWindowID string `json:"active_window_id"`
		Windows        []struct {
			WindowID       string `json:"window_id"`
			FocusedBlockID string `json:"focused_block_id"`
			Layers         []struct {
				Kind   string `json:"kind"`
				Blocks []struct {
					BlockID string `json:"block_id"`
				} `json:"blocks"`
			} `json:"layers"`
		} `json:"windows"`
	}
	if err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": sess}, &sv); err != nil {
		return false, "", false, err
	}
	for _, w := range sv.Windows {
		if w.WindowID != window {
			continue
		}
		for _, l := range w.Layers {
			for _, b := range l.Blocks {
				if l.Kind == "tiled" && b.BlockID != "" && b.BlockID == w.FocusedBlockID {
					tiled = true
				}
			}
		}
		return sv.ActiveWindowID == window, w.FocusedBlockID, tiled, nil
	}
	return false, "", false, fmt.Errorf("window %s is not in session %s", window, sess)
}

// slotUnder is the under block of a new slot layer: the window's focused block if it is in the
// tiled layer, else "" (the focus is in a floating layer, or the read failed).
func (h *Hub) slotUnder(ctx context.Context, sess, window string) string {
	_, focused, tiled, err := h.slotFocus(ctx, sess, window)
	if err != nil || !tiled {
		return ""
	}
	return focused
}

// returnFocusWhy is why a closed form's return must not open (layer mode), or "": the window is
// active and its focus is on the tiled block that was focused before the slot opened. A return
// never takes the focus from another place (the user, 2026-10-22: "never steal focus").
func (h *Hub) returnFocusWhy(ctx context.Context, sess, window, under string) string {
	if under == "" {
		return "under_unknown" // also a row from before under was recorded
	}
	active, focused, tiled, err := h.slotFocus(ctx, sess, window)
	switch {
	case err != nil:
		return "read_failed"
	case !active:
		return "window_not_active"
	case focused != under || !tiled:
		return "focus_moved"
	}
	return ""
}

// skipReturn ends a return without a pane: skipped is terminal, so no later pass opens it.
func (h *Hub) skipReturn(ctx context.Context, r *slotRow, why string) error {
	if err := h.setSlotState(ctx, r, "skipped"); err != nil && !errors.Is(err, errSlotMoved) {
		return err
	}
	if r.State == "skipped" {
		h.ev.emit("form_return_skipped", map[string]any{"view_id": r.ReturnOf, "replacement_id": r.ID, "why": why})
	}
	return nil
}

// slotDirect is a slotPane call without a test hook.
func slotDirect(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
	return h.rex.Call(ctx, method, args, out)
}

func (h *Hub) slotRefused(ctx context.Context, r *slotRow, cause error) error {
	h.closeViewRow(ctx, r.New, "abandoned")
	if err := h.setSlotState(ctx, r, "cancelled"); err != nil && !errors.Is(err, errSlotMoved) {
		return err
	}
	if r.Old != "" && r.State == "cancelled" {
		h.resumeOld(r) // after the durable state (Codex's review)
	}
	return fmt.Errorf("open %s: %w", r.Route, cause)
}

// resolveSlotPane looks for the pane of an unresolved row by its unique label. Found, the pane is
// adopted (opening -> closing), or closed when a newer open superseded the row. It reports
// whether the row moved on.
func (h *Hub) resolveSlotPane(ctx context.Context, r *slotRow) bool {
	block, found, err := h.findFormBlock(ctx, r.Session, r.New)
	if err != nil || !found {
		return false
	}
	var fc formContext
	json.Unmarshal([]byte(r.Context), &fc)
	if err := h.adoptForm(ctx, &view{ID: r.New, Plugin: r.Plugin, Tab: formTab, Route: r.Route}, fc, block); err != nil {
		return false
	}
	if r.State == "superseded" || r.State == "user_closed" {
		if h.closeForm(ctx, r.New, "late") == nil {
			h.setSlotState(ctx, r, "late_closed")
		}
		return true
	}
	return h.setSlotState(ctx, r, "closing") == nil
}

// slotSweep moves unresolved rows on. It runs at start and on layout changes of their sessions.
func (h *Hub) slotSweep(ctx context.Context) {
	rows, err := h.db.QueryContext(ctx, `SELECT repl_id, window_id FROM ui_replacements WHERE state IN ('opening', 'closing', 'superseded', 'user_closed')`)
	if err != nil {
		return
	}
	type item struct{ id, window string }
	var items []item
	for rows.Next() {
		var it item
		rows.Scan(&it.id, &it.window)
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		lock := h.windowLock(it.window)
		lock.Lock()
		r, err := h.slotRow(ctx, h.db, it.id)
		if err == nil && r != nil {
			var status string
			h.db.QueryRowContext(ctx, `SELECT status FROM views WHERE view_id = ?`, r.New).Scan(&status)
			switch {
			case status == "sending":
				h.resolveSlotPane(ctx, r)
			case (r.State == "superseded" || r.State == "user_closed") && status == "open":
				if h.closeForm(ctx, r.New, "late") == nil {
					h.setSlotState(ctx, r, "late_closed")
				}
			}
			if r.State == "opening" || r.State == "closing" {
				if _, err := h.advanceSlot(ctx, r); err != nil {
					log.Printf("slot %s: %v", r.ID, err)
				}
			}
		}
		lock.Unlock()
	}
}

// slotSessions reports whether sess has an unresolved replacement (its layout events matter).
func (h *Hub) slotSessions(ctx context.Context, sess string) bool {
	var n int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_replacements WHERE session_id = ? AND state IN ('opening', 'closing', 'superseded', 'user_closed')`, sess).Scan(&n)
	return n > 0
}

// isSlotView reports whether view id is the new view of a replacement row (formsSweep leaves it
// to slotSweep: an unknown outcome is never abandoned on an inventory).
func (h *Hub) isSlotView(ctx context.Context, id string) bool {
	var n int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_replacements WHERE new_view = ?`, id).Scan(&n)
	return n > 0
}

// releaseConfirm answers hub.view.release_confirm {view_id, attachment, replacement_id, choice}:
// the user's choice in the old renderer's prompt. discard goes on with the same replacement; keep
// cancels it.
func (h *Hub) releaseConfirm(ctx context.Context, p perform, args map[string]any) (any, error) {
	v, err := h.boundCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	id, _ := args["replacement_id"].(string)
	choice, _ := args["choice"].(string)
	if choice != "discard" && choice != "keep" {
		return nil, bad("choice must be discard or keep")
	}
	var window string
	if err := h.db.QueryRowContext(ctx, `SELECT window_id FROM ui_replacements WHERE repl_id = ? AND old_view = ?`, id, v.ID).Scan(&window); err != nil {
		return nil, bad("unknown replacement %q for this view", id)
	}
	lock := h.windowLock(window)
	lock.Lock()
	defer lock.Unlock()
	r, err := h.slotRow(ctx, h.db, id)
	if err != nil {
		return nil, err
	}
	answer := func() map[string]any {
		return map[string]any{"replacement": map[string]any{"state": r.State, "next_title": r.Title}}
	}
	if r.State != "confirming" {
		return answer(), nil // a repeat, or a state that moved on: the row's state is the answer
	}
	if choice == "keep" {
		// kept: the user's choice, not a refusal. A return that the user kept is a completed action.
		h.db.ExecContext(ctx, `UPDATE ui_replacements SET kept = 1 WHERE repl_id = ?`, r.ID)
		if err := h.setSlotState(ctx, r, "cancelled"); err != nil {
			return nil, err
		}
		return answer(), nil
	}
	if err := h.setSlotState(ctx, r, "opening"); err != nil {
		return nil, err
	}
	if _, err := h.advanceSlot(ctx, r); err != nil {
		log.Printf("slot %s: %v", r.ID, err) // the row's state says what happened
	}
	return answer(), nil
}

// slotAttachState is the replacement an attaching renderer must follow: {replacement_id, state,
// next_title}, from the durable row. The active replacement of the view comes first, also when the
// renderer names no ID (a restarted renderer lost it; Codex's review). A named ID the hub does not
// know for this view is cancelled. nil: nothing to follow.
func (h *Hub) slotAttachState(ctx context.Context, viewID, repl string) map[string]any {
	var id, state, title string
	err := h.db.QueryRowContext(ctx, `SELECT repl_id, state, next_title FROM ui_replacements
		WHERE old_view = ? AND state IN ('releasing', 'confirming', 'opening', 'closing') ORDER BY created DESC LIMIT 1`, viewID).Scan(&id, &state, &title)
	if err != nil && repl != "" {
		id = repl
		if h.db.QueryRowContext(ctx, `SELECT state, next_title FROM ui_replacements WHERE repl_id = ? AND old_view = ?`, repl, viewID).Scan(&state, &title) != nil {
			state = "cancelled"
		}
	} else if err != nil {
		return nil
	}
	return map[string]any{"replacement_id": id, "state": state, "next_title": title}
}

// viewReplacement handles hub.view.replacement {view_id, attachment, replacement_id}: the durable
// state of that replacement of that view, for a renderer that froze from an attach reply
// (view/panels.go). It reads only. {replacement: null}: the hub has no replacement with that ID for
// that view, so nothing holds the renderer.
func (h *Hub) viewReplacement(ctx context.Context, p perform, args map[string]any) (any, error) {
	v, err := h.boundCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	repl, _ := args["replacement_id"].(string)
	if repl == "" {
		return nil, bad("replacement_id is required")
	}
	var state, title string
	err = h.db.QueryRowContext(ctx, `SELECT state, next_title FROM ui_replacements WHERE repl_id = ? AND old_view = ?`, repl, v.ID).Scan(&state, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{"replacement": nil}, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"replacement": map[string]any{"replacement_id": repl, "state": state, "next_title": title}}, nil
}

// slotStart runs at hub start: a row still releasing sent no split, so it is cancelled. Rows
// that are confirming stay (the renderer shows its prompt again on reattach).
func (h *Hub) slotStart(ctx context.Context) error {
	_, err := h.db.ExecContext(ctx, `UPDATE ui_replacements SET state = 'cancelled' WHERE state = 'releasing'`)
	if err != nil {
		return err
	}
	// A claim whose replacement ended (also by the cancel above, or before a crash) is undone. The
	// query is the same at every start, so a repeat changes nothing.
	_, err = h.db.ExecContext(ctx, `UPDATE views SET attach_state = 'never' WHERE attach_state = 'claimed' AND view_id NOT IN
		(SELECT old_view FROM ui_replacements WHERE state IN ('releasing', 'confirming', 'opening', 'closing'))`)
	return err
}

// slotOldClosed records the user's close of a view that a replacement waits on: the row is
// user_closed, so a late new pane is closed and never kept (Codex's review).
func (h *Hub) slotOldClosed(ctx context.Context, viewID string) {
	h.db.ExecContext(ctx, `UPDATE ui_replacements SET state = 'user_closed' WHERE old_view = ? AND state IN ('releasing', 'confirming', 'opening')`, viewID)
}

// formEnded opens the panel that a form replaced, when the form ends. open: the form is still
// open (an action ended it); its pane is replaced and closed by the replacement, and is not closed
// before the release succeeds. It reports whether a return was requested: true, the replacement
// owns the form's pane (also when it was refused or the user kept editing; the form stays and err
// says why); false, no return, and the caller closes the form. A closed form's return opens only
// into an empty slot. In split mode it never takes the focus: the hub does not know where the user
// was (Codex's review). In layer mode an unfocused layer would cover the user's pane, so the return
// opens with the focus, and only while the focus is still on the form's under block; otherwise it
// is skipped (form_return_skipped).
// opKey is the operation key of the action that ended an open form: a new submit after Keep
// editing gets a new return row; a replay gets the same one (Codex's review).
func (h *Hub) formEnded(ctx context.Context, viewID string, fc formContext, open bool, opKey string) (bool, error) {
	if fc.Panel || fc.Return == nil {
		return false, nil
	}
	// The window must still exist; the hub never recreates a closed tab.
	var sv struct {
		ActiveWindowID string `json:"active_window_id"`
		Windows        []struct {
			WindowID       string `json:"window_id"`
			FocusedBlockID string `json:"focused_block_id"`
		} `json:"windows"`
	}
	if err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": fc.Return.Context.SessionID}, &sv); err != nil {
		if open && transient(err) {
			return true, err // retry; the form stays
		}
		if !open && h.cfg.UISlot == "layer" {
			h.ev.emit("form_return_skipped", map[string]any{"view_id": viewID, "why": "read_failed"})
		}
		return false, nil
	}
	rc := fc.Return.Context
	rc.Panel = true
	rc.UnderBlock = fc.UnderBlock // the form's value: a replacement chain keeps it
	focused, alive := "", false
	for _, w := range sv.Windows {
		if w.WindowID == rc.WindowID {
			alive, focused = true, w.FocusedBlockID
		}
	}
	if !alive {
		return false, nil
	}
	replID := "return-" + viewID
	if opKey != "" {
		sum := sha256.Sum256([]byte(viewID + "\x00" + opKey))
		replID = "return-" + hex.EncodeToString(sum[:8])
	}
	req := slotRequest{ReplID: replID, Plugin: fc.Return.Plugin, Route: fc.Return.Route, Ctx: rc}
	if open {
		// The return takes the form's place; the form is asked to release. It gets the focus only
		// if the form had it.
		req.ExpectOld = viewID
		req.Focus = sv.ActiveWindowID == rc.WindowID && focused != "" && focused == h.viewBlock(viewID)
	} else {
		req.ExpectEmpty = true
		if h.cfg.UISlot == "layer" {
			req.ReturnOf, req.Focus = viewID, true // spawnSlot checks the focus just before the open
		}
	}
	if _, err := h.openSlot(ctx, req); err != nil {
		log.Printf("slot: return to %s/%s: %v", fc.Return.Plugin, fc.Return.Route, err)
		if !open {
			return false, nil // the form is gone; nothing to keep
		}
		var state string
		var kept bool
		h.db.QueryRowContext(ctx, `SELECT state, kept FROM ui_replacements WHERE repl_id = ?`, req.ReplID).Scan(&state, &kept)
		if state == "cancelled" && kept && h.viewBlock(viewID) != "" {
			return true, nil // the user kept editing (or a replay of it): the action is complete; the form stays
		}
		return true, err // a refused release: the form stays, the effect reports the failure
	}
	return open, nil
}

// panelTarget resolves a {panel: …} effect of plugin to the plugin and route to open.
func (h *Hub) panelTarget(ctx context.Context, plugin string, spec map[string]any) (string, string, error) {
	target, _ := spec["plugin"].(string)
	command, _ := spec["command"].(string)
	route, _ := spec["route"].(string)
	if target != "" && target != plugin {
		if plugin != h.homePlugin() {
			return "", "", fmt.Errorf("only the home plugin opens another plugin's panel")
		}
		if command == "" || route != "" {
			return "", "", fmt.Errorf("another plugin's panel is named by its command")
		}
	}
	if target == "" {
		target = plugin
	}
	if command != "" {
		var raw string
		h.db.QueryRowContext(ctx, `SELECT commands FROM ui_commands WHERE plugin = ?`, target).Scan(&raw)
		var list []paletteCommand
		json.Unmarshal([]byte(raw), &list)
		for _, c := range list {
			if c.ID == command && c.Surface == "panel" {
				return target, c.Route, nil
			}
		}
		return "", "", fmt.Errorf("plugin %s has no panel command %q", target, command)
	}
	if !strings.HasPrefix(route, "/panels/") {
		return "", "", fmt.Errorf("a panel route must start with /panels/")
	}
	return target, route, nil
}

// commandTitle is the title of a plugin's command with route, for the old renderer's message.
func (h *Hub) commandTitle(ctx context.Context, plugin, route string) string {
	var raw string
	h.db.QueryRowContext(ctx, `SELECT commands FROM ui_commands WHERE plugin = ?`, plugin).Scan(&raw)
	var list []paletteCommand
	json.Unmarshal([]byte(raw), &list)
	for _, c := range list {
		if c.Route == route {
			return c.Title
		}
	}
	return route
}

// landingPanel is the ID of plugin's landing panel command, or "".
func (h *Hub) landingPanel(ctx context.Context, plugin string) string {
	var raw string
	h.db.QueryRowContext(ctx, `SELECT commands FROM ui_commands WHERE plugin = ?`, plugin).Scan(&raw)
	var list []paletteCommand
	json.Unmarshal([]byte(raw), &list)
	for _, c := range list {
		if c.Surface == "panel" && c.Landing {
			return c.ID
		}
	}
	return ""
}

// viewBlock is the pane of an open view, or "".
func (h *Hub) viewBlock(id string) string {
	var block string
	h.db.QueryRowContext(context.Background(), `SELECT block_id FROM views WHERE view_id = ? AND status = 'open'`, id).Scan(&block)
	return block
}
