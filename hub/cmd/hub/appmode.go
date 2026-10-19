package main

// App mode: Home is an app launcher, and each plugin is an app (ui.go). Opening an app hides the
// user's own sessions; "Exit LazyRex" puts them back exactly.
//
// A cycle is one enter and exit. Its phases:
//
//	capturing  the navigation recorded the intent; nothing changed yet
//	hiding     every window of the user's sessions is recorded; the worker detaches their blocks
//	active     every recorded block is detached
//	restoring  hub.ui.exit asked; the worker rebuilds every recorded window
//	complete   done (problems, if any, stay readable on the cycle)
//	refused    the capture found a layout the hub cannot rebuild; nothing was hidden
//
// The capture commits before the first detach. A rebuild commits a window's record only after Rex
// confirms the window. The proof of the rebuild method is in rex-protocol.md, "A whole session
// layout can be rebuilt exactly". The hub never closes or replaces a recorded block: a block that
// was closed or moved while hidden becomes a problem on the cycle.
//
// The user's sessions are every session not recorded in ui_sessions. Detaching keeps processes
// and unsaved input (rex-protocol.md, "Detach and restore preserve view processes").

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const appModeSchema = `
CREATE TABLE IF NOT EXISTS ui_cycles (cycle TEXT PRIMARY KEY, seq INTEGER NOT NULL, phase TEXT NOT NULL,
  origin TEXT NOT NULL DEFAULT '', problems TEXT NOT NULL DEFAULT '[]', windows INTEGER NOT NULL DEFAULT 0,
  blocks INTEGER NOT NULL DEFAULT 0, restored_windows INTEGER NOT NULL DEFAULT 0, restored_blocks INTEGER NOT NULL DEFAULT 0,
  created TEXT NOT NULL, updated TEXT NOT NULL);
-- One recorded window. state: hidden, restored, lost. rebuilt_window: the window that holds it now,
-- recorded as soon as Rex creates it, so a crash in the middle of a rebuild finds it again.
CREATE TABLE IF NOT EXISTS ui_hidden (cycle TEXT NOT NULL, session_id TEXT NOT NULL, spos INTEGER NOT NULL,
  wpos INTEGER NOT NULL, window TEXT NOT NULL, active INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'hidden',
  rebuilt_window TEXT NOT NULL DEFAULT '', PRIMARY KEY (cycle, session_id, wpos));
-- An exit key binds to the cycle it first exited. A retry with that key reports that cycle.
CREATE TABLE IF NOT EXISTS ui_exit_keys (key TEXT PRIMARY KEY, cycle TEXT NOT NULL);
`

type problem struct {
	Session string `json:"session_id,omitempty"`
	Window  string `json:"window,omitempty"` // the recorded tab label
	Block   string `json:"block_id,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type cycle struct {
	ID, Phase, Origin                  string
	Seq                                int
	Problems                           []problem
	Windows, Blocks, RWindows, RBlocks int
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanCycle(row *sql.Row) (*cycle, error) {
	c := &cycle{}
	var probs string
	err := row.Scan(&c.ID, &c.Seq, &c.Phase, &c.Origin, &probs, &c.Windows, &c.Blocks, &c.RWindows, &c.RBlocks)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(probs), &c.Problems)
	return c, nil
}

const cycleCols = `cycle, seq, phase, origin, problems, windows, blocks, restored_windows, restored_blocks`

// currentCycle returns the cycle that is not finished, or nil.
func currentCycle(ctx context.Context, q querier) (*cycle, error) {
	return scanCycle(q.QueryRowContext(ctx, `SELECT `+cycleCols+` FROM ui_cycles
		WHERE phase IN ('capturing', 'hiding', 'active', 'restoring') ORDER BY seq DESC LIMIT 1`))
}

func latestCycle(ctx context.Context, q querier) (*cycle, error) {
	return scanCycle(q.QueryRowContext(ctx, `SELECT `+cycleCols+` FROM ui_cycles ORDER BY seq DESC LIMIT 1`))
}

func cycleByID(ctx context.Context, q querier, id string) (*cycle, error) {
	return scanCycle(q.QueryRowContext(ctx, `SELECT `+cycleCols+` FROM ui_cycles WHERE cycle = ?`, id))
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// newCycle records the intent to enter app mode. The worker captures and hides.
func newCycle(ctx context.Context, tx *sql.Tx) error {
	var seq int
	tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM ui_cycles`).Scan(&seq)
	_, err := tx.ExecContext(ctx, `INSERT INTO ui_cycles (cycle, seq, phase, created, updated) VALUES (?, ?, 'capturing', ?, ?)`,
		randomID("cycle-"), seq, now(), now())
	return err
}

// setPhase moves a cycle from one phase to the next. It reports false when the cycle was not in
// phase from (an exit came first).
func (h *Hub) setPhase(ctx context.Context, c *cycle, from, to string) (bool, error) {
	res, err := h.db.ExecContext(ctx, `UPDATE ui_cycles SET phase = ?, updated = ? WHERE cycle = ? AND phase = ?`, to, now(), c.ID, from)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		c.Phase = to
		h.ev.emit("app_mode", map[string]any{"cycle": c.ID, "phase": to})
		go h.changed("mode")
	}
	return n == 1, nil
}

// appModeStep moves the current cycle on. It reports true when more work remains.
func (h *Hub) appModeStep(ctx context.Context) (bool, error) {
	c, err := currentCycle(ctx, h.db)
	if err != nil {
		return false, err
	}
	h.closeLatePlaceholders(ctx, c)
	if c == nil {
		return false, nil
	}
	switch c.Phase {
	case "capturing":
		if err := h.capture(ctx, c); err != nil {
			return false, err
		}
		return c.Phase == "hiding" || c.Phase == "restoring", nil
	case "hiding":
		return false, h.hide(ctx, c)
	case "restoring":
		h.restoreAttempt(c.ID, h.restore(ctx, c))
	}
	return false, nil
}

// restoreRetry is the progress of a restore that fails and retries. It is in memory: the restore
// intent itself is durable (the cycle and its window records), so a hub restart retries anyway.
type restoreRetry struct {
	cycle    string
	attempts int
	err      string
}

// restoreAttempt records the result of one restore pass. A failure does not end the restore:
// a failure count does not prove that the user's panes are gone. The worker tries again with
// back-off (1 s, doubling, at most 30 s), and hub.ui.mode shows the attempts and the last error.
// Known outcomes (closed, moved, session closed) are problems on the cycle, not failures.
func (h *Hub) restoreAttempt(cycle string, err error) {
	h.ui.mu.Lock()
	defer h.ui.mu.Unlock()
	if err == nil {
		h.ui.restoreRetry = nil
		return
	}
	r := h.ui.restoreRetry
	if r == nil || r.cycle != cycle {
		r = &restoreRetry{cycle: cycle}
		h.ui.restoreRetry = r
	}
	r.attempts++
	r.err = err.Error()
	delay := time.Second << min(r.attempts-1, 5)
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	log.Printf("appmode: restore %s attempt %d: %v (retry in %s)", cycle, r.attempts, err, delay)
	h.ev.emit("app_restore_failed", map[string]any{"cycle": cycle, "attempts": r.attempts, "error": r.err, "retry_ms": delay.Milliseconds()})
	go h.changed("mode")
	time.AfterFunc(delay, func() { h.kickUI("") })
}

// --- Capture ---

// The layout fields the rebuild is proven for. Any other field means a feature the hub cannot
// rebuild yet: the capture refuses before anything is hidden.
var (
	viewKeys   = keys("revision", "session_id", "label", "active_window_id", "windows")
	windowKeys = keys("window_id", "label", "focused_block_id", "zoomed_block_id", "layers")
	layerKeys  = keys("layer_id", "kind", "bounds", "layout", "blocks")
	nodeKeys   = keys("rect", "block_id", "split")
	splitKeys  = keys("split_id", "direction", "ratio", "before", "after")
)

func keys(k ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range k {
		m[s] = true
	}
	return m
}

type lnode struct {
	BlockID string  `json:"block_id,omitempty"`
	Split   *lsplit `json:"split,omitempty"`
}

type lsplit struct {
	Direction string  `json:"direction"`
	Ratio     float64 `json:"ratio"`
	Before    *lnode  `json:"before"`
	After     *lnode  `json:"after"`
}

type llayer struct {
	Kind   string    `json:"kind"`
	Bounds []float64 `json:"bounds"`
	Layout *lnode    `json:"layout,omitempty"`
}

type lwindow struct {
	WindowID string   `json:"window_id"`
	Label    string   `json:"label"`
	Focused  string   `json:"focused_block_id,omitempty"`
	Zoomed   string   `json:"zoomed_block_id,omitempty"`
	Layers   []llayer `json:"layers"`
}

func (n *lnode) first() string {
	if n.Split != nil {
		return n.Split.Before.first()
	}
	return n.BlockID
}

func (n *lnode) leaves(out []string) []string {
	if n == nil {
		return out
	}
	if n.Split != nil {
		return n.Split.After.leaves(n.Split.Before.leaves(out))
	}
	return append(out, n.BlockID)
}

// prune keeps only the leaves in keep. A split with one side left becomes that side.
func (n *lnode) prune(keep map[string]bool) *lnode {
	if n == nil {
		return nil
	}
	if n.Split == nil {
		if keep[n.BlockID] {
			return n
		}
		return nil
	}
	b, a := n.Split.Before.prune(keep), n.Split.After.prune(keep)
	if b == nil {
		return a
	}
	if a == nil {
		return b
	}
	return &lnode{Split: &lsplit{Direction: n.Split.Direction, Ratio: n.Split.Ratio, Before: b, After: a}}
}

func (w *lwindow) leaves() []string {
	var out []string
	for _, l := range w.Layers {
		out = l.Layout.leaves(out)
	}
	return out
}

// checkView reports the first layout feature in a raw session.view that the rebuild does not
// cover, or "" when every field is known.
func checkView(raw map[string]any) string {
	for k := range raw {
		if !viewKeys[k] {
			return "session field " + k
		}
	}
	ws, _ := raw["windows"].([]any)
	for _, wv := range ws {
		w, _ := wv.(map[string]any)
		for k := range w {
			if !windowKeys[k] {
				return "tab field " + k
			}
		}
		ls, _ := w["layers"].([]any)
		for i, lv := range ls {
			l, _ := lv.(map[string]any)
			for k := range l {
				if !layerKeys[k] {
					return "layer field " + k
				}
			}
			kind, _ := l["kind"].(string)
			if (i == 0) != (kind == "tiled") || (kind != "tiled" && kind != "floating") {
				return "layer kind " + kind + " at position " + strconv.Itoa(i)
			}
			if kind == "floating" && l["layout"] == nil {
				return "an empty floating layer"
			}
			if msg := checkNode(l["layout"]); msg != "" {
				return msg
			}
		}
	}
	return ""
}

func checkNode(v any) string {
	if v == nil {
		return ""
	}
	n, _ := v.(map[string]any)
	for k := range n {
		if !nodeKeys[k] {
			return "layout field " + k
		}
	}
	if (n["block_id"] == nil) == (n["split"] == nil) {
		return "a layout node without exactly one of block and split"
	}
	if s, ok := n["split"].(map[string]any); ok {
		for k := range s {
			if !splitKeys[k] {
				return "split field " + k
			}
		}
		if d, _ := s["direction"].(string); d != "horizontal" && d != "vertical" {
			return "split direction " + d
		}
		if r, _ := s["ratio"].(float64); r <= 0 || r >= 1 {
			return "split ratio " + strconv.FormatFloat(r, 'g', -1, 64)
		}
		if m := checkNode(s["before"]); m != "" {
			return m
		}
		return checkNode(s["after"])
	}
	return ""
}

type capturedSession struct {
	id      string
	active  string
	windows []lwindow
	raw     []json.RawMessage
}

// capture records every window of the user's sessions, then moves the cycle to hiding. A layout
// the hub cannot rebuild refuses the cycle: nothing is hidden.
func (h *Hub) capture(ctx context.Context, c *cycle) error {
	var list struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := h.rex.Call(ctx, "session.list", map[string]any{}, &list); err != nil {
		return err
	}
	var caps []capturedSession
	var refusals []problem
	seen := map[string]bool{}
	for _, s := range list.Sessions {
		h.ui.mu.Lock()
		_, ours := h.ui.sess[s.SessionID]
		h.ui.mu.Unlock()
		if ours {
			continue
		}
		if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": s.SessionID, "event_scope": "session"}, nil); err != nil {
			return fmt.Errorf("attach %s: %w", s.SessionID, err)
		}
		var raw map[string]any
		if err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": s.SessionID}, &raw); err != nil {
			return fmt.Errorf("view %s: %w", s.SessionID, err)
		}
		if msg := checkView(raw); msg != "" {
			refusals = append(refusals, problem{Session: s.SessionID, Code: "unsupported",
				Message: "LazyRex cannot restore " + msg + " yet, so it did not hide anything"})
			continue
		}
		var v struct {
			Active  string            `json:"active_window_id"`
			Windows []json.RawMessage `json:"windows"`
		}
		b, _ := json.Marshal(raw)
		json.Unmarshal(b, &v)
		cs := capturedSession{id: s.SessionID, active: v.Active}
		for _, wr := range v.Windows {
			var w lwindow
			if err := json.Unmarshal(wr, &w); err != nil {
				return err
			}
			leaves := w.leaves()
			if len(leaves) == 0 {
				continue // nothing to hide: the tab stays as it is
			}
			for _, l := range leaves {
				if seen[l] {
					refusals = append(refusals, problem{Session: s.SessionID, Block: l, Code: "unsupported",
						Message: "a pane appears twice in the layout, so LazyRex did not hide anything"})
				}
				seen[l] = true
			}
			cs.windows = append(cs.windows, w)
			cs.raw = append(cs.raw, wr)
		}
		if len(cs.windows) > 0 {
			caps = append(caps, cs)
		}
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	to := "hiding"
	if len(refusals) > 0 {
		to = "refused"
		pj, _ := json.Marshal(refusals)
		if _, err := tx.ExecContext(ctx, `UPDATE ui_cycles SET problems = ? WHERE cycle = ?`, string(pj), c.ID); err != nil {
			return err
		}
		// Apps are mounted only in app mode: the navigation into the app ends here. Home shows why.
		if _, err := tx.ExecContext(ctx, `DELETE FROM ui_state WHERE key = 'nav'`); err != nil {
			return err
		}
	} else {
		nw, nb, origin := 0, 0, ""
		for si, cs := range caps {
			if origin == "" {
				origin = cs.id
			}
			for wi, w := range cs.windows {
				if _, err := tx.ExecContext(ctx, `INSERT INTO ui_hidden (cycle, session_id, spos, wpos, window, active) VALUES (?, ?, ?, ?, ?, ?)`,
					c.ID, cs.id, si, wi, string(cs.raw[wi]), w.WindowID == cs.active); err != nil {
					return err
				}
				nw++
				nb += len(w.leaves())
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ui_cycles SET origin = ?, windows = ?, blocks = ? WHERE cycle = ?`, origin, nw, nb, c.ID); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE ui_cycles SET phase = ?, updated = ? WHERE cycle = ? AND phase = 'capturing'`, to, now(), c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil // an exit came first; the cycle restores what nothing hid
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.Phase = to
	h.ev.emit("app_mode", map[string]any{"cycle": c.ID, "phase": to, "sessions": len(caps), "refusals": refusals})
	go h.changed("mode")
	if to == "hiding" {
		return h.hide(ctx, c)
	}
	h.kickUI("") // refused: the next pass detaches app views this pass may have mounted (Codex)
	return nil
}

type hiddenRow struct {
	Session, State, Rebuilt string
	SPos, WPos              int
	Active                  bool
	W                       lwindow
}

func (h *Hub) hiddenRows(ctx context.Context, c *cycle) ([]hiddenRow, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT session_id, spos, wpos, window, active, state, rebuilt_window FROM ui_hidden
		WHERE cycle = ? ORDER BY spos, wpos`, c.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hiddenRow
	for rows.Next() {
		var r hiddenRow
		var w string
		if err := rows.Scan(&r.Session, &r.SPos, &r.WPos, &w, &r.Active, &r.State, &r.Rebuilt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(w), &r.W); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// hide detaches every recorded block. Detach is idempotent, so a retry after a crash repeats it.
// A block that is already gone is not an error here; the restore reports it.
func (h *Hub) hide(ctx context.Context, c *cycle) error {
	rows, err := h.hiddenRows(ctx, c)
	if err != nil {
		return err
	}
	for _, r := range rows {
		for _, b := range r.W.leaves() {
			err := h.rex.Call(ctx, "session.detach_block", map[string]any{"session_id": r.Session, "block_id": b}, nil)
			var re *rex.RexError
			if err != nil && !(errors.As(err, &re) && re.Code == "not_found") {
				return fmt.Errorf("hide %s: %w", b, err)
			}
		}
	}
	_, err = h.setPhase(ctx, c, "hiding", "active")
	return err
}

// --- Restore ---

func (h *Hub) addProblems(ctx context.Context, tx *sql.Tx, c *cycle, ps []problem) error {
	if len(ps) == 0 {
		return nil
	}
	var cur string
	tx.QueryRowContext(ctx, `SELECT problems FROM ui_cycles WHERE cycle = ?`, c.ID).Scan(&cur)
	var all []problem
	json.Unmarshal([]byte(cur), &all)
	// A retry pass repeats every session's last steps: do not record the same problem twice.
	have := map[problem]bool{}
	for _, p := range all {
		have[p] = true
	}
	for _, p := range ps {
		if !have[p] {
			all = append(all, p)
			have[p] = true
		}
	}
	b, _ := json.Marshal(all)
	_, err := tx.ExecContext(ctx, `UPDATE ui_cycles SET problems = ? WHERE cycle = ?`, string(b), c.ID)
	return err
}

// restore rebuilds every recorded window, session by session, then selects the first recorded
// session on every app (the maintainer's choice: Rex does not report which session an app shows).
func (h *Hub) restore(ctx context.Context, c *cycle) error {
	rows, err := h.hiddenRows(ctx, c)
	if err != nil {
		return err
	}
	bySession := map[string][]hiddenRow{}
	var order []string
	for _, r := range rows {
		if _, ok := bySession[r.Session]; !ok {
			order = append(order, r.Session)
		}
		bySession[r.Session] = append(bySession[r.Session], r)
	}
	origin := ""
	for _, sess := range order {
		alive, err := h.restoreSession(ctx, c, sess, bySession[sess])
		if err != nil {
			return err
		}
		if alive && origin == "" {
			origin = sess
		}
	}
	if origin != "" {
		h.selectOnApps(ctx, origin, "", map[string]any{"cycle": c.ID, "exit": true})
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ui_hidden WHERE cycle = ?`, c.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ui_cycles SET phase = 'complete', updated = ? WHERE cycle = ? AND phase = 'restoring'`, now(), c.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.Phase = "complete"
	h.ev.emit("app_mode", map[string]any{"cycle": c.ID, "phase": "complete", "origin": origin})
	go h.changed("mode")
	h.kickUI("") // the next pass detaches the apps' panes
	return nil
}

// restoreSession rebuilds the session's hidden windows, then puts the recorded tabs first in their
// recorded order and focuses the recorded active tab. It reports whether the session still exists.
func (h *Hub) restoreSession(ctx context.Context, c *cycle, sess string, rows []hiddenRow) (bool, error) {
	err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil)
	if sessionGone(err) {
		var lost []problem
		for _, r := range rows {
			if r.State == "hidden" {
				lost = append(lost, problem{Session: sess, Window: r.W.Label, Code: "session_closed",
					Message: "The session was closed while LazyRex was open"})
			}
		}
		return false, h.finishRows(ctx, c, sess, rows, "lost", lost)
	} else if err != nil {
		return false, err
	}
	for i := range rows {
		if rows[i].State != "hidden" {
			continue
		}
		if err := h.restoreWindow(ctx, c, &rows[i]); err != nil {
			return true, err
		}
	}
	var desired []string
	focus := ""
	for _, r := range rows {
		if r.State == "restored" && r.Rebuilt != "" {
			desired = append(desired, r.Rebuilt)
			if r.Active {
				focus = r.Rebuilt
			}
		}
	}
	var sv struct {
		Windows []struct {
			WindowID string `json:"window_id"`
		} `json:"windows"`
	}
	if err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": sess}, &sv); err != nil {
		return true, err
	}
	var current []string
	live := map[string]bool{}
	for _, w := range sv.Windows {
		current = append(current, w.WindowID)
		live[w.WindowID] = true
	}
	var keep []string
	for _, w := range desired {
		if live[w] {
			keep = append(keep, w)
		}
	}
	// A failed order or focus keeps the restore pending: the next pass does it again. Only a
	// confirmed missing tab is a problem (Codex's review).
	if err := h.orderWindows(ctx, sess, keep, current); err == errRetry {
		return true, fmt.Errorf("order tabs in %s: a tab changed; retrying", sess)
	} else if err != nil {
		return true, err
	}
	if focus != "" {
		// focus_window keeps the window's focused block, so it keeps a restored zoom (Codex's review:
		// focus_block on another block would clear it).
		err := h.rex.Call(ctx, "session.focus_window", map[string]any{"session_id": sess, "window_id": focus}, nil)
		var re *rex.RexError
		if errors.As(err, &re) && re.Code == "not_found" {
			return true, h.finishRows(ctx, c, sess, nil, "", []problem{{Session: sess, Code: "focus_missing",
				Message: "The tab that was active was closed during the restore; another tab has focus"}})
		} else if err != nil {
			return true, fmt.Errorf("focus %s: %w", focus, err)
		}
	}
	return true, nil
}

// finishRows marks rows of one session with a final state and records problems, in one commit.
func (h *Hub) finishRows(ctx context.Context, c *cycle, sess string, rows []hiddenRow, state string, ps []problem) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if r.State == "hidden" {
			if _, err := tx.ExecContext(ctx, `UPDATE ui_hidden SET state = ? WHERE cycle = ? AND session_id = ? AND wpos = ?`, state, c.ID, sess, r.WPos); err != nil {
				return err
			}
		}
	}
	if err := h.addProblems(ctx, tx, c, ps); err != nil {
		return err
	}
	return tx.Commit()
}

func restoreTag(c *cycle, r *hiddenRow) string {
	return "hidden:" + c.ID + ":" + r.Session + ":" + strconv.Itoa(r.WPos)
}

// restoreWindow rebuilds one recorded window (rex-protocol.md, "A whole session layout can be
// rebuilt exactly"). A window that a crash left half built is taken apart first: its recorded
// blocks are detached again and its placeholder closed. Then the rebuild starts over.
func (h *Hub) restoreWindow(ctx context.Context, c *cycle, r *hiddenRow) error {
	sess, tag := r.Session, restoreTag(c, r)
	blocks, err := h.blocksIn(ctx, sess)
	if err != nil {
		return err
	}
	recorded := r.W.leaves()

	// 1. A half-built window: the recorded one, or the one that holds a proven placeholder of ours.
	var nonces, holders []string
	holderNonce := map[string]string{} // placeholder block -> its nonce
	rows, err := h.db.QueryContext(ctx, `SELECT nonce FROM ui_restores WHERE view_id = ?`, tag)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		nonces = append(nonces, n)
	}
	rows.Close()
	partial := r.Rebuilt
	for _, n := range nonces {
		for id, b := range blocks {
			if b.Label == "lazyrex-placeholder:"+n {
				if pid, err := h.childIn(ctx, sess, id); err == nil && pid > 0 && argvHas(pid, n) && argvHas(pid, "lazyrex-placeholder") {
					holders = append(holders, id)
					holderNonce[id] = n
					if b.Placement != "detached" {
						partial = b.WindowID
					}
				}
			}
		}
	}

	// 2. Classify the recorded blocks. A block still in its original tab (the hide did not reach
	// it) or in the half-built tab is ours to detach and place. A block in any other tab was moved
	// by the user: it stays there. A missing block was closed.
	if partial == "" {
		allHome := true
		for _, b := range recorded {
			if bl, ok := blocks[b]; !ok || bl.Placement == "detached" || bl.WindowID != r.W.WindowID {
				allHome = false
			}
		}
		if allHome {
			// Never hidden (an exit during the hide): the tab is as it was.
			return h.windowDone(ctx, c, r, r.W.WindowID, len(recorded), nil, nil)
		}
	}
	var ps []problem
	keep := map[string]bool{}
	for _, b := range recorded {
		bl, ok := blocks[b]
		switch {
		case !ok:
			ps = append(ps, problem{Session: sess, Window: r.W.Label, Block: b, Code: "closed",
				Message: "A pane was closed while LazyRex was open"})
		case bl.Placement == "detached":
			keep[b] = true
		case bl.WindowID == partial || bl.WindowID == r.W.WindowID:
			if err := h.rex.Call(ctx, "session.detach_block", map[string]any{"session_id": sess, "block_id": b}, nil); err != nil {
				return fmt.Errorf("restore %s: detach %s: %w", tag, b, err)
			}
			keep[b] = true
		default:
			ps = append(ps, problem{Session: sess, Window: r.W.Label, Block: b, Code: "moved",
				Message: "A pane was moved to another tab while LazyRex was open; it stays there"})
		}
	}
	// Only a nonce whose placeholder is proven and closed is resolved. A nonce with no pane found
	// stays recorded: its new_window outcome is unknown, and the pane may still appear.
	var resolved []string
	for _, id := range holders {
		if err := h.closeBlock(ctx, sess, id); err != nil {
			return fmt.Errorf("restore %s: close old placeholder: %w", tag, err)
		}
		resolved = append(resolved, holderNonce[id])
	}
	if len(holders) > 0 || partial != "" {
		h.db.ExecContext(ctx, `UPDATE ui_hidden SET rebuilt_window = '' WHERE cycle = ? AND session_id = ? AND wpos = ?`, c.ID, sess, r.WPos)
		h.ev.emit("app_restore_reset", map[string]any{"cycle": c.ID, "session_id": sess, "window": r.W.Label})
	}
	if len(keep) == 0 {
		return h.windowDone(ctx, c, r, "", 0, ps, resolved)
	}

	// 3. Rebuild: a placeholder tab, floating layers back to front, then the tiled layer.
	nonce := randomID("ph-")
	if _, err := h.db.ExecContext(ctx, `INSERT INTO ui_restores VALUES (?, ?, ?)`, nonce, tag, now()); err != nil {
		return err
	}
	req := map[string]any{"session_id": sess, "window_label": r.W.Label, "focus": false,
		"layout": map[string]any{"block": map[string]any{
			"flavor": "com.superlogical.terminal.shell",
			"label":  "lazyrex-placeholder:" + nonce,
			"options": map[string]any{
				// A loop, so the shell does not exec sleep: its arguments must keep the nonce.
				"command": []string{"/bin/sh", "-c", "while :; do sleep 600; done", "lazyrex-placeholder", nonce},
				"login":   false,
			},
		}}}
	var out struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
	}
	if err := h.rex.Call(ctx, "session.new_window", req, &out); err != nil {
		return fmt.Errorf("restore %s: %w", tag, err) // the next pass finds a placeholder that did open
	}
	if len(out.BlockIDs) != 1 {
		return fmt.Errorf("restore %s: %d blocks", tag, len(out.BlockIDs))
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE ui_hidden SET rebuilt_window = ? WHERE cycle = ? AND session_id = ? AND wpos = ?`,
		out.WindowID, c.ID, sess, r.WPos); err != nil {
		return err
	}
	holder := out.BlockIDs[0]
	call := func(method string, args map[string]any) error {
		args["session_id"] = sess
		if err := h.rex.Call(ctx, method, args, nil); err != nil {
			return fmt.Errorf("restore %s: %s: %w", tag, method, err)
		}
		return nil
	}
	var expand func(n *lnode) error
	expand = func(n *lnode) error {
		if n.Split == nil {
			return nil
		}
		if err := call("session.move_block", map[string]any{"block_id": n.Split.After.first(), "anchor_block_id": n.Split.Before.first(),
			"direction": n.Split.Direction, "side": "after", "ratio": n.Split.Ratio}); err != nil {
			return err
		}
		if err := expand(n.Split.Before); err != nil {
			return err
		}
		return expand(n.Split.After)
	}
	var tiled *lnode
	for _, l := range r.W.Layers {
		t := l.Layout.prune(keep)
		if t == nil {
			continue
		}
		if l.Kind == "tiled" {
			tiled = t
			continue
		}
		if err := call("session.move_block", map[string]any{"block_id": t.first(), "anchor_block_id": holder}); err != nil {
			return err
		}
		if err := call("session.float_block", map[string]any{"block_id": t.first(), "bounds": l.Bounds}); err != nil {
			return err
		}
		if err := expand(t); err != nil {
			return err
		}
	}
	if tiled != nil {
		if err := call("session.move_block", map[string]any{"block_id": tiled.first(), "anchor_block_id": holder}); err != nil {
			return err
		}
	}
	if f := h.faults.take("restore-window", "exit-mid", ""); f != nil {
		// Lab: stop with the window half built (the placeholder and one moved block).
		h.ev.emit("fault", map[string]any{"point": f.point, "mode": f.mode, "cycle": c.ID, "window": r.W.Label})
		os.Exit(faultExitCode)
	}
	if err := h.closeBlock(ctx, sess, holder); err != nil {
		return fmt.Errorf("restore %s: close placeholder: %w", tag, err)
	}
	if tiled != nil {
		if err := expand(tiled); err != nil {
			return err
		}
	}
	if keep[r.W.Focused] {
		if err := call("session.focus_block", map[string]any{"block_id": r.W.Focused}); err != nil {
			return err
		}
	}
	if keep[r.W.Zoomed] {
		// Last: a focus change clears zoom.
		if err := call("session.zoom_block", map[string]any{"block_id": r.W.Zoomed, "zoom": true}); err != nil {
			return err
		}
	}
	return h.windowDone(ctx, c, r, out.WindowID, len(keep), ps, append(resolved, nonce))
}

// windowDone commits a window's final state, its problems and the restored counts together.
func (h *Hub) windowDone(ctx context.Context, c *cycle, r *hiddenRow, window string, nblocks int, ps []problem, nonces []string) error {
	state := "restored"
	if window == "" {
		state = "lost"
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE ui_hidden SET state = ?, rebuilt_window = ? WHERE cycle = ? AND session_id = ? AND wpos = ?`,
		state, window, c.ID, r.Session, r.WPos); err != nil {
		return err
	}
	if window != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE ui_cycles SET restored_windows = restored_windows + 1, restored_blocks = restored_blocks + ? WHERE cycle = ?`,
			nblocks, c.ID); err != nil {
			return err
		}
	}
	for _, n := range nonces {
		// Each of these placeholders is confirmed closed: its record goes.
		if _, err := tx.ExecContext(ctx, `DELETE FROM ui_restores WHERE nonce = ?`, n); err != nil {
			return err
		}
	}
	if err := h.addProblems(ctx, tx, c, ps); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.State, r.Rebuilt = state, window
	h.ev.emit("app_window_restored", map[string]any{"cycle": c.ID, "session_id": r.Session, "window": r.W.Label,
		"state": state, "blocks": nblocks, "problems": len(ps)})
	go h.changed("mode")
	return nil
}

// --- Services ---

func cycleStatus(c *cycle) map[string]any {
	if c == nil {
		return map[string]any{"mode": "workspace", "cycle": "", "phase": "none", "problems": []problem{},
			"restored": map[string]any{"windows": 0, "blocks": 0}}
	}
	mode := "workspace"
	switch c.Phase {
	case "capturing", "hiding", "active", "restoring":
		mode = "app"
	}
	probs := c.Problems
	if probs == nil {
		probs = []problem{}
	}
	sort.SliceStable(probs, func(i, j int) bool { return probs[i].Session < probs[j].Session })
	return map[string]any{"mode": mode, "cycle": c.ID, "phase": c.Phase, "problems": probs,
		"hidden":   map[string]any{"windows": c.Windows, "blocks": c.Blocks},
		"restored": map[string]any{"windows": c.RWindows, "blocks": c.RBlocks}}
}

// uiMode reports the latest cycle (hub.ui.mode). While a restore fails and retries, "retry" has
// the attempts and the last error.
func (h *Hub) uiMode(ctx context.Context) (any, error) {
	c, err := latestCycle(ctx, h.db)
	if err != nil {
		return nil, err
	}
	st := cycleStatus(c)
	h.ui.mu.Lock()
	if r := h.ui.restoreRetry; r != nil && c != nil && r.cycle == c.ID && c.Phase == "restoring" {
		st["retry"] = map[string]any{"attempts": r.attempts, "error": r.err}
	}
	h.ui.mu.Unlock()
	return st, nil
}

// uiExit starts the restore of the current cycle (hub.ui.exit {for, key}). The key binds to the
// cycle it first exits; a retry with the same key reports that cycle, never a later one.
func (h *Hub) uiExit(ctx context.Context, args map[string]any) (any, error) {
	key, _ := args["key"].(string)
	if key == "" {
		return nil, bad("key is required")
	}
	if who, _ := args["for"].(string); who != "user" {
		return nil, bad("for must be \"user\"")
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var bound string
	if err := tx.QueryRowContext(ctx, `SELECT cycle FROM ui_exit_keys WHERE key = ?`, key).Scan(&bound); err == nil {
		if bound == "" {
			return cycleStatus(nil), nil
		}
		c, err := cycleByID(ctx, tx, bound)
		if err != nil {
			return nil, err
		}
		return cycleStatus(c), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	c, err := currentCycle(ctx, tx)
	if err != nil {
		return nil, err
	}
	if c == nil {
		// No app mode: nothing to exit. The key binds to "no cycle" (Codex's review): a retry of a
		// lost response, sent after a later enter, must not exit that later cycle.
		if _, err := tx.ExecContext(ctx, `INSERT INTO ui_exit_keys VALUES (?, '')`, key); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return cycleStatus(nil), nil
	}
	if c.Phase != "restoring" {
		if _, err := tx.ExecContext(ctx, `UPDATE ui_cycles SET phase = 'restoring', updated = ? WHERE cycle = ?`, now(), c.ID); err != nil {
			return nil, err
		}
		c.Phase = "restoring"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ui_exit_keys VALUES (?, ?)`, key, c.ID); err != nil {
		return nil, err
	}
	// A navigation that waits must not show an app after the exit.
	if _, err := tx.ExecContext(ctx, `DELETE FROM ui_state WHERE key = 'nav'`); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	h.ev.emit("app_mode", map[string]any{"cycle": c.ID, "phase": "restoring"})
	go h.changed("mode")
	h.kickUI("")
	return cycleStatus(c), nil
}

// closeLatePlaceholders handles restore placeholders whose new_window outcome was unknown, outside
// the cycle that restores now (Codex's review). Their records stay after the cycle completes. A
// placeholder that appears late is closed when its label and process arguments carry the nonce;
// only that pane closes, never a user pane. A record goes when its placeholder is closed or its
// session is gone. Otherwise it stays: the pane may still appear. The hub stays attached to each
// such session, and a layout change there wakes this cleanup (uiSessionChanged). A transient
// failure, or a placeholder whose process has not started, retries in 5 s.
func (h *Hub) closeLatePlaceholders(ctx context.Context, current *cycle) {
	rows, err := h.db.QueryContext(ctx, `SELECT nonce, view_id FROM ui_restores WHERE view_id LIKE 'hidden:%'`)
	if err != nil {
		return
	}
	type rec struct{ nonce, sess string }
	var recs []rec
	for rows.Next() {
		var n, tag string
		rows.Scan(&n, &tag)
		// tag: hidden:<cycle>:<session_id>:<wpos>; a session ID contains a colon.
		rest := strings.TrimPrefix(tag, "hidden:")
		cyc, rest, _ := strings.Cut(rest, ":")
		if current != nil && current.Phase == "restoring" && cyc == current.ID {
			continue // restoreWindow resolves its own records
		}
		if i := strings.LastIndex(rest, ":"); i > 0 {
			recs = append(recs, rec{n, rest[:i]})
		}
	}
	rows.Close()
	late := map[string]bool{}
	for _, r := range recs {
		late[r.sess] = true
	}
	h.ui.mu.Lock()
	h.ui.lateSess = late
	h.ui.mu.Unlock()
	for _, r := range recs {
		// The hub reads a user session's blocks only while attached to it.
		err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": r.sess, "event_scope": "session"}, nil)
		if sessionGone(err) {
			h.db.ExecContext(ctx, `DELETE FROM ui_restores WHERE nonce = ?`, r.nonce)
			h.ev.emit("placeholder_record_dropped", map[string]any{"nonce": r.nonce, "why": "session gone"})
			continue
		}
		if err != nil {
			log.Printf("appmode: late placeholders in %s: %v", r.sess, err)
			h.retryLater()
			continue
		}
		blocks, err := h.blocksIn(ctx, r.sess)
		if err != nil {
			log.Printf("appmode: late placeholders in %s: %v", r.sess, err)
			h.retryLater()
			continue
		}
		for id, b := range blocks {
			if b.Label != "lazyrex-placeholder:"+r.nonce {
				continue
			}
			pid, err := h.childIn(ctx, r.sess, id)
			if err != nil || pid == 0 {
				h.retryLater() // not known yet: the process may not have started
				continue
			}
			if !argvHas(pid, r.nonce) || !argvHas(pid, "lazyrex-placeholder") {
				h.ev.emit("placeholder_unproven", map[string]any{"block_id": id, "nonce": r.nonce})
				continue
			}
			if err := h.closeBlock(ctx, r.sess, id); err != nil {
				log.Printf("appmode: close late placeholder %s: %v", id, err)
				h.retryLater()
				continue
			}
			h.db.ExecContext(ctx, `DELETE FROM ui_restores WHERE nonce = ?`, r.nonce)
			h.ev.emit("placeholder_closed", map[string]any{"block_id": id, "nonce": r.nonce, "late": true})
		}
	}
}
