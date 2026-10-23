package main

// Reopen (docs/hub.md, "Reopen"): a form action's effect {reopen: {args?: {full: bool}}} replaces
// the pane of the form's source tab view with a NEW view (new view_id, new pane, new attachment)
// in the same place. Plugins keep drafts by user and thread, not by view_id, so the new view reads
// the same draft. The args are stored on the new view (views.open_args): its renders get them as
// args.open, and its snapshot carries them as open.
//
// A reopen is a ui_replacements row of kind tab, on the side slot's lifecycle (panels.go):
//   - The old renderer is asked to release first (view.release; Keep editing / Discard and
//     replace). A never-attached old view is claimed without a renderer.
//   - The new views row (with open_args) exists before the one pane request (new_split anchored at
//     the old pane, label lazyrex-view:<new view>). An unknown outcome is resolved only by the
//     label; the hub never sends a second request.
//   - The old pane closes only after the new pane exists.
//   - No pane within reopenBound of the request, on a complete inventory: the row is expired, the
//     old view resumes, and a late pane is closed. Renderers see expired as cancelled.
//   - While the row is active it owns its tab: the reconcile pass opens no other view for it,
//     closes neither view, and leaves the old view's crash path to the row (reservedTabs). A user
//     close of either pane closes the tab, as any user close does, and ends the row.
//
// Only a form may return reopen (not a panel and not a tab view's own action): a release of the
// acting view would wait on its own pending operation (codex-25's renderer-model probe).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// reopenBound is how long after its pane request a reopen waits for the pane.
var reopenBound = 30 * time.Second

// reopenEffect is the decoded reopen effect. open is the stored open_args: {"full":true} or "".
type reopenEffect struct {
	set  bool
	open string
}

// UnmarshalJSON takes the renderer's rules for open args (view.OpenArgs): args is absent, null or
// an object whose only key is full, with the JSON literal true or false. full is read as raw JSON,
// so null is refused. Anything else is invalid_input.
func (r *reopenEffect) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		return nil // no reopen
	}
	r.set = true
	var spec map[string]json.RawMessage
	if json.Unmarshal(b, &spec) != nil || spec == nil {
		return refuse("invalid_input", "reopen must be an object")
	}
	for k := range spec {
		if k != "args" {
			return refuse("invalid_input", "reopen: unknown key %q", k)
		}
	}
	raw := bytes.TrimSpace(spec["args"])
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil || args == nil {
		return refuse("invalid_input", "reopen: args must be an object")
	}
	for k, v := range args {
		if k != "full" {
			return refuse("invalid_input", "reopen: unknown arg %q", k)
		}
		switch string(bytes.TrimSpace(v)) {
		case "true":
			r.open = `{"full":true}`
		case "false":
		default:
			return refuse("invalid_input", "reopen: full must be true or false")
		}
	}
	return nil
}

// reopenID is the replacement row of the reopen effect of operation key: a retry finds the same
// row, and through it the same new view.
func reopenID(plugin, key string) string {
	sum := sha256.Sum256([]byte("reopen\x00" + plugin + "\x00" + key))
	return "reopen-" + hex.EncodeToString(sum[:8])
}

func reopenViewID(repl string) string {
	sum := sha256.Sum256([]byte("tab\x00" + repl))
	return "view-" + hex.EncodeToString(sum[:8])
}

// tabRowContext is the context of a kind tab row: what the new view row needs.
type tabRowContext struct {
	Tab    string `json:"tab"`
	Layout string `json:"layout,omitempty"`
	Open   string `json:"open,omitempty"`
}

// reopenStep runs the reopen effect of operation key, whose acting view is viewID. done: the row
// is done, and the effect goes on. Not done and no error: the row waits (the user's choice, or its
// pane); its next state wakes the view worker. An error fails the effect.
func (h *Hub) reopenStep(ctx context.Context, plugin, key, viewID string, e reopenEffect) (bool, error) {
	fc, isForm := h.formOf(ctx, viewID)
	if !isForm || fc.Panel {
		return false, refuse("invalid_input", "reopen is valid only from a form")
	}
	id := reopenID(plugin, key)
	// The durable row first: a retry after the row ended (also after the old view closed) answers
	// from the row, not from a fresh check of the source.
	r, err := h.slotRow(ctx, h.db, id)
	if err != nil {
		return false, err
	}
	if r == nil {
		if r, err = h.admitReopen(ctx, id, plugin, fc, e); err != nil {
			return false, err
		}
	} else if r.State == "releasing" {
		// The release step did not finish (a store error after the INSERT): ask again with the
		// same replacement ID; the renderer answers the same replacement the same way.
		lock := h.windowLock(r.Window)
		lock.Lock()
		if r, err = h.slotRow(ctx, h.db, id); err == nil && r != nil && r.State == "releasing" {
			if err = h.askRelease(ctx, r); err == nil || r.State != "releasing" {
				h.advanceSlot(ctx, r)
			}
		}
		lock.Unlock()
		if err != nil && (r == nil || r.State == "releasing") {
			return false, err
		}
	} else if r.State == "closing" {
		// The old pane's close failed before: try it again. closing sends no pane request, so the
		// one-split rule holds.
		lock := h.windowLock(r.Window)
		lock.Lock()
		if r, err = h.slotRow(ctx, h.db, id); err == nil && r != nil && r.State == "closing" {
			h.advanceSlot(ctx, r)
		}
		lock.Unlock()
		if err != nil {
			return false, err
		}
	}
	return h.reopenAnswer(ctx, r)
}

// admitReopen checks the source and records the row, then runs it as far as it goes now.
func (h *Hub) admitReopen(ctx context.Context, id, plugin string, fc formContext, e reopenEffect) (*slotRow, error) {
	if fc.SourceViewID == "" {
		return nil, refuse("conflict", "the source view is closed")
	}
	var srcPlugin, tabKey, route, status, sess, block, layout string
	err := h.db.QueryRowContext(ctx, `SELECT plugin, tab, route, status, session_id, block_id, layout FROM views WHERE view_id = ?`, fc.SourceViewID).
		Scan(&srcPlugin, &tabKey, &route, &status, &sess, &block, &layout)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && status != "open") {
		return nil, refuse("conflict", "the source view is closed")
	}
	if err != nil {
		return nil, err
	}
	if srcPlugin != plugin || tabKey == formTab {
		return nil, refuse("invalid_input", "reopen: the source is not a tab view of this plugin")
	}
	// The window comes from Rex: the user can move a pane, and the row can lag behind.
	window, err := h.formWindow(ctx, sess, block)
	if err != nil {
		return nil, fmt.Errorf("reopen: the source view is not shown: %w", err)
	}
	lock := h.windowLock(window)
	lock.Lock()
	defer lock.Unlock()
	if r, err := h.slotRow(ctx, h.db, id); err != nil || r != nil {
		return r, err
	}
	var busy int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_replacements WHERE kind = 'tab' AND plugin = ? AND json_extract(context, '$.tab') = ?
		AND state IN `+activeStates, plugin, tabKey).Scan(&busy)
	if busy > 0 {
		return nil, refuse("conflict", "the view is reopening")
	}
	// The new pane takes the focus only if the old pane had it.
	active, focused, _, ferr := h.slotFocus(ctx, sess, window)
	focus := ferr == nil && active && focused == block
	b, _ := json.Marshal(tabRowContext{Tab: tabKey, Layout: layout, Open: e.open})
	r := &slotRow{ID: id, Session: sess, Window: window, Old: fc.SourceViewID, New: reopenViewID(id), Plugin: plugin, Route: route,
		Context: string(b), Focus: focus, State: "releasing", Title: "Reopening", Kind: "tab"}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, focus, state, next_title, created, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'tab')`, r.ID, r.Session, r.Window, r.Old, r.New, r.Plugin, r.Route, r.Context, r.Focus, r.State, r.Title, now()); err != nil {
		return nil, err
	}
	h.ev.emit("slot_replacement", map[string]any{"replacement_id": r.ID, "kind": "tab", "window_id": r.Window, "old_view": r.Old, "new_view": r.New, "route": r.Route, "state": r.State})
	if err := h.askRelease(ctx, r); err != nil && r.State == "releasing" {
		return r, err
	}
	if r.State == "opening" || r.State == "closing" {
		if _, err := h.advanceSlot(ctx, r); err != nil {
			log.Printf("reopen %s: %v", r.ID, err) // the row's state says what happened
		}
	}
	return r, nil
}

// reopenAnswer is the effect's outcome for the row's state.
func (h *Hub) reopenAnswer(ctx context.Context, r *slotRow) (bool, error) {
	if r, err := h.slotRow(ctx, h.db, r.ID); err == nil && r != nil {
		return h.reopenAnswerOf(ctx, r)
	}
	return h.reopenAnswerOf(ctx, r)
}

func (h *Hub) reopenAnswerOf(ctx context.Context, r *slotRow) (bool, error) {
	switch r.State {
	case "done":
		return true, nil
	case "releasing", "confirming", "opening", "closing":
		return false, nil
	case "cancelled":
		var kept bool
		var status string
		h.db.QueryRowContext(ctx, `SELECT kept FROM ui_replacements WHERE repl_id = ?`, r.ID).Scan(&kept)
		h.db.QueryRowContext(ctx, `SELECT status FROM views WHERE view_id = ?`, r.Old).Scan(&status)
		switch {
		case kept:
			return false, refuse("conflict", "the view has unsaved input; it was not reopened")
		case status != "open":
			return false, refuse("conflict", "the source view is closed")
		}
		return false, refuse("conflict", "the view did not reopen; try again")
	case "expired":
		return false, refuse("conflict", "the view did not reopen; try again")
	}
	return false, refuse("conflict", "the source view is closed") // user_closed, superseded, late_closed
}

// spawnTabPane asks Rex for the new tab pane, once per row. split_at is written first, then the
// new views row, then the request: a views row in 'sending' means the request may have been sent.
func (h *Hub) spawnTabPane(ctx context.Context, r *slotRow) error {
	var tc tabRowContext
	json.Unmarshal([]byte(r.Context), &tc)
	var oldBlock string
	h.db.QueryRowContext(ctx, `SELECT block_id FROM views WHERE view_id = ? AND status != 'closed'`, r.Old).Scan(&oldBlock)
	if oldBlock == "" {
		return h.slotRefused(ctx, r, errors.New("the source pane is gone"))
	}
	r.SplitAt = now()
	if _, err := h.db.ExecContext(ctx, `UPDATE ui_replacements SET split_at = ? WHERE repl_id = ?`, r.SplitAt, r.ID); err != nil {
		return err
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO views (view_id, plugin, tab, route, status, session_id, layout, attach_state, open_args)
		VALUES (?, ?, ?, ?, 'sending', ?, ?, 'never', ?)`, r.New, r.Plugin, tc.Tab, r.Route, r.Session, tc.Layout, tc.Open); err != nil {
		return err
	}
	if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": r.Session, "event_scope": "session"}, nil); err != nil {
		return h.slotRefused(ctx, r, err) // nothing was sent
	}
	args := map[string]any{"session_id": r.Session, "direction": "horizontal", "side": "after", "ratio": 0.5, "focus": r.Focus,
		"anchor_block_id": oldBlock, // the new pane takes the old pane's area when the old one closes
		"layout": map[string]any{"block": map[string]any{
			"flavor": "com.superlogical.terminal.shell",
			"label":  "lazyrex-view:" + r.New,
			"options": map[string]any{
				"command": h.viewCommand(r.New, r.Plugin, r.Route),
				"login":   false,
				"exit":    map[string]any{"on_completion": false}, // a tab pane keeps open (spawnView)
			},
		}}}
	var out struct {
		BlockIDs []string `json:"block_ids"`
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := slotSplit(h, cctx, "session.new_split", args, &out)
	cancel()
	var re *rex.RexError
	if err != nil && errors.As(err, &re) && re.Code != "disconnected" && !transient(err) {
		return h.slotRefused(ctx, r, err)
	}
	if err != nil || len(out.BlockIDs) != 1 {
		h.ev.emit("slot_unknown", map[string]any{"replacement_id": r.ID, "view_id": r.New, "error": errString(err)})
		h.watchTabRow(r.ID, reopenBound)
		return nil // unresolved: resolveTabPane looks for the label
	}
	return h.adoptTabPane(ctx, r, rexBlock{BlockID: out.BlockIDs[0], WindowID: r.Window})
}

// adoptTabPane records the new pane and moves the row to closing (or closes a late pane).
func (h *Hub) adoptTabPane(ctx context.Context, r *slotRow, b rexBlock) error {
	var tc tabRowContext
	json.Unmarshal([]byte(r.Context), &tc)
	v := &view{ID: r.New, Plugin: r.Plugin, Tab: tc.Tab, Route: r.Route, Layout: tc.Layout}
	if err := h.adoptView(ctx, v, r.Session, b); err != nil {
		return nil // the pane exists; a later look adopts it
	}
	h.ev.emit("view_opened", map[string]any{"plugin": r.Plugin, "view_id": r.New, "tab": tc.Tab, "route": r.Route, "window_id": r.Window, "replacement_id": r.ID})
	if r.State == "superseded" || r.State == "user_closed" || r.State == "expired" {
		if h.slotClose(ctx, r, r.New, "late") == nil {
			h.setSlotState(ctx, r, "late_closed")
		}
		return nil
	}
	if err := h.setSlotState(ctx, r, "closing"); err != nil && !errors.Is(err, errSlotMoved) {
		return err
	}
	return nil
}

// resolveTabPane looks for the new pane of an unresolved tab row by its label. It reports whether
// the row moved on. A pane is the row's when its child's argv carries the new view ID, or when it
// has no child reopenBound after the request (the new renderer exited before the hub saw it). A
// labeled pane whose child is another program is not the row's. A complete inventory without the
// row's pane, reopenBound after the request, expires the row; an incomplete one proves nothing.
func (h *Hub) resolveTabPane(ctx context.Context, r *slotRow) bool {
	blocks, err := h.blocksIn(ctx, r.Session)
	if err != nil && !sessionGone(err) {
		h.watchTabRow(r.ID, 5*time.Second)
		return false
	}
	sent, perr := time.Parse(time.RFC3339Nano, r.SplitAt)
	late := perr == nil && time.Since(sent) >= reopenBound
	var dead *rexBlock
	for b, info := range blocks {
		if info.Label != "lazyrex-view:"+r.New {
			continue
		}
		c, err := h.childIn(ctx, r.Session, b)
		if r.State == "expired" {
			// A late pane: its views row was abandoned at the expiry, so it is closed here, never
			// adopted. Only a pane whose child's argv proves it the row's is closed: a close on the label
			// alone could close a pane that is not the hub's (lead ruling, lead-ux-fix1b). A childless
			// late pane stays for the user.
			if err != nil || c == 0 || !argvHas(c, r.New) {
				continue
			}
			if err := h.closeBlock(ctx, r.Session, b); err == nil {
				h.setSlotState(ctx, r, "late_closed")
				return true
			}
			return false
		}
		switch {
		case err != nil:
			h.watchTabRow(r.ID, 5*time.Second)
			return false
		case c > 0 && argvHas(c, r.New):
			h.adoptTabPane(ctx, r, info)
			return true
		case c == 0:
			info := info
			dead = &info
		}
	}
	if r.State == "expired" {
		return false // no late pane (yet): a layout change of the session looks again
	}
	if dead != nil {
		h.noteTabPane(ctx, r, *dead) // its close or its exit event is the row's from now on
	}
	if r.State != "opening" || !late {
		d := 5 * time.Second
		if r.State == "opening" && perr == nil {
			d = reopenBound - time.Since(sent)
		}
		h.watchTabRow(r.ID, d)
		return false
	}
	if dead != nil {
		h.tabPaneExited(ctx, r, *dead)
		return true
	}
	h.expireTab(ctx, r)
	return true
}

// noteTabPane records the pane of a tab row's new view on its views row, which stays 'sending'
// until the pane is adopted. A close or a child exit of that block is then the row's
// (tabReplPaneClosed, tabReplPaneExited).
func (h *Hub) noteTabPane(ctx context.Context, r *slotRow, b rexBlock) {
	h.db.ExecContext(ctx, `UPDATE views SET block_id = ?, window_id = ? WHERE view_id = ? AND status = 'sending'`, b.BlockID, b.WindowID, r.New)
}

// tabPaneExited adopts the new pane of a tab row whose renderer exited before the hub saw it, and
// runs the crash path once (uiChildExited). The row goes on: the old pane is replaced anyway, and
// the reconcile pass replaces the exited view (or not, at the crash limit). The caller holds the
// window lock.
func (h *Hub) tabPaneExited(ctx context.Context, r *slotRow, b rexBlock) {
	if err := h.adoptTabPane(ctx, r, b); err != nil {
		log.Printf("reopen %s: %v", r.ID, err)
	}
	var status string
	h.db.QueryRowContext(ctx, `SELECT status FROM views WHERE view_id = ? AND block_id = ?`, r.New, b.BlockID).Scan(&status)
	if status != "open" {
		return // not adopted: a later look tries again
	}
	h.uiChildExited(ctx, b.BlockID)
	if r.State == "closing" {
		if _, err := h.advanceSlot(ctx, r); err != nil {
			log.Printf("reopen %s: %v", r.ID, err)
		}
	}
}

// expireTab ends a tab row whose pane did not appear in time: the new views row is abandoned, the
// old view resumes, and a pane that appears later is closed (slotSweep).
func (h *Hub) expireTab(ctx context.Context, r *slotRow) {
	if err := h.setSlotState(ctx, r, "expired"); err != nil {
		return // another path moved the row first
	}
	h.closeViewRow(ctx, r.New, "abandoned")
	h.resumeOld(r)
	h.ev.emit("reopen_expired", map[string]any{"replacement_id": r.ID, "view_id": r.New})
}

// watchTabRow runs an unresolved tab row on after d (opening: its pane and the bound; closing: a
// failed close of the old pane), so the row moves without a later event. One timer per row.
func (h *Hub) watchTabRow(id string, d time.Duration) {
	if d < time.Second {
		d = time.Second
	}
	h.slots.mu.Lock()
	if h.slots.watch == nil {
		h.slots.watch = map[string]bool{}
	}
	if h.slots.watch[id] {
		h.slots.mu.Unlock()
		return
	}
	h.slots.watch[id] = true
	h.slots.mu.Unlock()
	time.AfterFunc(d, func() {
		h.slots.mu.Lock()
		delete(h.slots.watch, id)
		h.slots.mu.Unlock()
		ctx := context.Background()
		r, err := h.slotRow(ctx, h.db, id)
		if err != nil || r == nil || (r.State != "opening" && r.State != "closing") {
			return
		}
		lock := h.windowLock(r.Window)
		lock.Lock()
		defer lock.Unlock()
		if r, err = h.slotRow(ctx, h.db, id); err == nil && r != nil && (r.State == "opening" || r.State == "closing") {
			if _, err := h.advanceSlot(ctx, r); err != nil {
				log.Printf("reopen %s: %v", id, err)
			}
		}
	})
}

// closeRetry is the delay before the next try of a tab row's failed old-pane close: 1 s, then
// doubling to 30 s. The tries go on while the row is closing; each one is a single block.close.
func (h *Hub) closeRetry(id string) time.Duration {
	h.slots.mu.Lock()
	defer h.slots.mu.Unlock()
	if h.slots.tries == nil {
		h.slots.tries = map[string]int{}
	}
	n := h.slots.tries[id]
	h.slots.tries[id] = n + 1
	d := time.Second << min(n, 5)
	return min(d, 30*time.Second)
}

// slotClose closes view id of row r: a form or panel with closeForm, a tab view with closeTabView.
func (h *Hub) slotClose(ctx context.Context, r *slotRow, id, why string) error {
	if r.Kind == "tab" {
		return h.closeTabView(ctx, r, id, why)
	}
	return h.closeForm(ctx, id, why)
}

// closeTabView closes a tab view of a reopen: its pane, then its row. A failed block.close keeps
// the row, so the replacement tries again (closeView ignores the error; a replacement must not).
func (h *Hub) closeTabView(ctx context.Context, r *slotRow, id, why string) error {
	var sess, block, status, tabKey string
	if err := h.db.QueryRowContext(ctx, `SELECT session_id, block_id, status, tab FROM views WHERE view_id = ?`, id).Scan(&sess, &block, &status, &tabKey); err != nil || status == "closed" {
		return nil
	}
	if block != "" {
		if err := h.closeBlock(ctx, sess, block); err != nil && !sessionGone(err) {
			return fmt.Errorf("close %s: %w", id, err)
		}
	}
	h.closeViewRow(ctx, id, "hub")
	h.ev.emit("view_closed", map[string]any{"plugin": r.Plugin, "view_id": id, "tab": tabKey, "by": "hub", "why": why, "replacement_id": r.ID})
	return nil
}

// rendererState is a row state as a renderer sees it: expired is internal, and a renderer treats
// it as cancelled (its old pane resumes editing).
func rendererState(state string) string {
	if state == "expired" {
		return "cancelled"
	}
	return state
}

// reservedTabs are the tabs of plugin that an active tab row owns, by tab key.
func (h *Hub) reservedTabs(ctx context.Context, plugin string) map[string]*slotRow {
	out := map[string]*slotRow{}
	rows, err := h.db.QueryContext(ctx, `SELECT repl_id FROM ui_replacements WHERE kind = 'tab' AND plugin = ? AND state IN `+activeStates, plugin)
	if err != nil {
		return out
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if r, err := h.slotRow(ctx, h.db, id); err == nil && r != nil {
			var tc tabRowContext
			json.Unmarshal([]byte(r.Context), &tc)
			out[tc.Tab] = r
		}
	}
	return out
}

// tabReplUserClosed ends the active tab rows of a view that the user closed (its pane, or Ctrl+Q):
// the user closed the tab. A late new pane is closed.
func (h *Hub) tabReplUserClosed(ctx context.Context, viewID string) {
	for _, r := range h.tabRowsOf(ctx, viewID, `(old_view = ? OR new_view = ?)`, viewID, viewID) {
		if err := h.setSlotState(ctx, r, "user_closed"); err == nil {
			go h.slotSweep(context.Background()) // closes a new pane that exists
		}
	}
}

// tabReplExited ends a tab row whose old renderer exited before any pane was asked for. Later, the
// row goes on: the old pane is replaced anyway.
func (h *Hub) tabReplExited(ctx context.Context, viewID string) {
	for _, r := range h.tabRowsOf(ctx, viewID, `old_view = ?`, viewID) {
		if r.State == "releasing" || r.State == "confirming" {
			h.setSlotState(ctx, r, "cancelled")
		}
	}
}

// tabReplOldGone ends a waiting tab row whose old pane is gone (a complete inventory without it):
// the prompt cannot be answered any more.
func (h *Hub) tabReplOldGone(ctx context.Context, r *slotRow) {
	if r.State == "releasing" || r.State == "confirming" {
		h.setSlotState(ctx, r, "user_closed")
	}
}

func (h *Hub) tabRowsOf(ctx context.Context, viewID, where string, args ...any) []*slotRow {
	rows, err := h.db.QueryContext(ctx, `SELECT repl_id FROM ui_replacements WHERE kind = 'tab' AND `+where+` AND state IN `+activeStates, args...)
	if err != nil {
		return nil
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []*slotRow
	for _, id := range ids {
		if r, err := h.slotRow(ctx, h.db, id); err == nil && r != nil {
			out = append(out, r)
		}
	}
	return out
}

// Active reports whether the row still runs.
func (r *slotRow) Active() bool {
	switch r.State {
	case "releasing", "confirming", "opening", "closing":
		return true
	}
	return false
}

// openingTabRows are the tab rows in opening whose new view has no pane yet, in session sess, or
// in every session when sess is "".
func (h *Hub) openingTabRows(ctx context.Context, sess string) []*slotRow {
	rows, err := h.db.QueryContext(ctx, `SELECT r.repl_id FROM ui_replacements r JOIN views v ON v.view_id = r.new_view
		WHERE r.kind = 'tab' AND r.state = 'opening' AND v.status = 'sending' AND (? = '' OR r.session_id = ?)`, sess, sess)
	if err != nil {
		return nil
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []*slotRow
	for _, id := range ids {
		if r, err := h.slotRow(ctx, h.db, id); err == nil && r != nil {
			out = append(out, r)
		}
	}
	return out
}

// tabReplCreated runs on block_created in sess. block_created carries no label (Rex 1792158932,
// lab), so while a tab row waits for its pane in sess, the hub reads the new block's label once and
// notes the row's pane: a close or a child exit that follows is then the row's. A close or exit
// event of the block waits for this read (tabReplWait).
func (h *Hub) tabReplCreated(sess, block string) {
	ctx := context.Background()
	if len(h.openingTabRows(ctx, sess)) == 0 {
		return
	}
	done := make(chan struct{})
	h.slots.mu.Lock()
	if h.slots.noting == nil {
		h.slots.noting = map[string]chan struct{}{}
	}
	h.slots.noting[block] = done
	h.slots.mu.Unlock()
	go func() {
		defer func() {
			h.slots.mu.Lock()
			delete(h.slots.noting, block)
			h.slots.mu.Unlock()
			close(done)
		}()
		blocks, err := h.blocksIn(ctx, sess)
		if err != nil {
			return
		}
		b, ok := blocks[block]
		if !ok {
			return
		}
		for _, r := range h.openingTabRows(ctx, sess) {
			if b.Label == "lazyrex-view:"+r.New {
				h.noteTabPane(ctx, r, b)
			}
		}
	}()
}

// tabReplWait waits (at most 3 s) for a label read of block that tabReplCreated started.
func (h *Hub) tabReplWait(block string) {
	h.slots.mu.Lock()
	done := h.slots.noting[block]
	h.slots.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// tabReplPaneClosed handles a pane close of a tab row's new pane before the hub adopted it: the
// user closed the tab, as with any user close (uiBlockClosed), and the row ends. It reports whether
// block was such a pane.
func (h *Hub) tabReplPaneClosed(ctx context.Context, block string) bool {
	h.tabReplWait(block)
	var id, plugin, tabKey string
	if h.db.QueryRowContext(ctx, `SELECT v.view_id, v.plugin, v.tab FROM views v JOIN ui_replacements r ON r.new_view = v.view_id
		WHERE v.block_id = ? AND v.status = 'sending' AND r.kind = 'tab' AND r.state IN `+activeStates, block).Scan(&id, &plugin, &tabKey) != nil {
		return false
	}
	h.closeViewRow(ctx, id, "user")
	h.db.ExecContext(ctx, `UPDATE ui_tabs SET suppressed = 1, closed_revision = revision WHERE plugin = ? AND tab = ?`, plugin, tabKey)
	h.ev.emit("view_closed", map[string]any{"plugin": plugin, "view_id": id, "tab": tabKey, "by": "user"})
	h.tabReplUserClosed(ctx, id)
	h.kickUI(plugin)
	return true
}

// tabReplPaneExited handles a child exit in a block that is no open view: the new pane of a tab
// row whose renderer exited before the hub adopted it. The pane is the row's when it was noted, or
// when it carries the row's label and has no child now.
func (h *Hub) tabReplPaneExited(ctx context.Context, block string) {
	h.tabReplWait(block)
	for _, r := range h.openingTabRows(ctx, "") {
		var noted string
		h.db.QueryRowContext(ctx, `SELECT block_id FROM views WHERE view_id = ?`, r.New).Scan(&noted)
		blocks, err := h.blocksIn(ctx, r.Session)
		if err != nil {
			continue
		}
		b, ok := blocks[block]
		if !ok || (noted != block && b.Label != "lazyrex-view:"+r.New) {
			continue
		}
		if pid, err := h.childIn(ctx, r.Session, block); err != nil || pid != 0 {
			continue
		}
		lock := h.windowLock(r.Window)
		lock.Lock()
		if r, err := h.slotRow(ctx, h.db, r.ID); err == nil && r != nil && r.State == "opening" {
			h.tabPaneExited(ctx, r, b)
		}
		lock.Unlock()
		return
	}
}
