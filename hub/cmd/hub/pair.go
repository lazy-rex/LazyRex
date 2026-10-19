package main

// Transcript pairs (spikes/04-scrollback/DESIGN.md, "B: hub pair lifecycle"). A tab that declares
// layout "transcript" opens as two panes in one window: a transcript printer over the composer,
// 76/24 as a plain ratio (the maintainer, 2026-10-19). The hub honors the layout only when its config sets
// "transcript": true.
//
// The views row is the composer. Focus, panels, forms, reload and the close gate use it as before.
// view_transcripts holds the transcript role: its own pane, process proof, client and attachment.
// The printer only reads (get, subscribe, seen). Every mutation needs the composer's attachment.
//
// The composer opens first, alone in its window, and the reconcile pass adds the transcript before
// it with session.new_split. Observed on Rex 0.0.1792158932-g88fd41 (pair-focus.mjs): this keeps
// the composer as the window's focused block, so an app that shows the tab focuses the composer.
// One new_window with both panes focuses the transcript. The same pass replaces a transcript that
// exited or left the composer's window.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const pairSchema = `
-- The transcript role of a paired view. status: sending (outcome unknown), open, exited (the
-- printer ended; the worker closes its pane), gone (no pane; the worker opens one unless crashes
-- reached maxCrashes), declined (the user closed the transcript; the composer's close gate runs,
-- and no new transcript opens until a navigation to the tab). close_pending: the user closed the
-- pair and the composer has not closed yet; each composer attachment gets view.close_request.
-- retiring: the view closed while a split was in flight; the sweep closes a late pane. The record
-- stays until a pane closed or its session is gone: nothing bounds when an unknown split lands.
-- requested: when the hub last asked Rex for a transcript pane (RFC 3339), for diagnosis.
CREATE TABLE IF NOT EXISTS view_transcripts (view_id TEXT PRIMARY KEY, status TEXT NOT NULL,
  block_id TEXT NOT NULL DEFAULT '', child_pid INTEGER NOT NULL DEFAULT 0, child_start TEXT NOT NULL DEFAULT '',
  client_id TEXT NOT NULL DEFAULT '', attachment TEXT NOT NULL DEFAULT '', crashes INTEGER NOT NULL DEFAULT 0,
  close_pending INTEGER NOT NULL DEFAULT 0, requested TEXT NOT NULL DEFAULT '');
`

const (
	layoutTranscript = "transcript"
	transcriptRatio  = 0.76 // the transcript's share (the before half of the split); the maintainer, 2026-10-19
	transcriptLabel  = "lazyrex-transcript:"
)

type transcriptRole struct {
	ViewID, Status, Block string
	ChildPID              int
	ChildStart            string
	ClientID, Attachment  string
	Crashes               int
}

func (h *Hub) pairStart(ctx context.Context) error {
	if _, err := h.db.ExecContext(ctx, pairSchema); err != nil {
		return err
	}
	for _, m := range []string{
		`ALTER TABLE view_transcripts ADD COLUMN close_pending INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE view_transcripts ADD COLUMN requested TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := h.db.ExecContext(ctx, m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	// Attachments belong to one hub generation. Printers attach again.
	_, err := h.db.ExecContext(ctx, `UPDATE view_transcripts SET client_id = '', attachment = ''`)
	return err
}

// pairLayout is the layout a view of the tab gets: the declared layout when the hub enables it.
func (h *Hub) pairLayout(t tab) string {
	if h.cfg.Transcript && t.Layout == layoutTranscript {
		return layoutTranscript
	}
	return ""
}

// setViewLayout converts an open view in place when its effective layout changes (the config's
// "transcript" or the tab's layout). The composer stays: its renderer reads the layout from each
// snapshot. Enable: the reconcile pass adds the transcript pane. Disable: the transcript pane goes,
// and the composer shows the history again. A split in flight keeps its record (retiring), and
// the sweep resolves it. A pending close of the pair (declined, close_pending) stays.
func (h *Hub) setViewLayout(ctx context.Context, v *view, layout string) error {
	if _, err := h.db.ExecContext(ctx, `UPDATE views SET layout = ? WHERE view_id = ?`, layout, v.ID); err != nil {
		return err
	}
	from := v.Layout
	v.Layout = layout
	if layout == "" && !h.closePending(ctx, v.ID) {
		h.closeTranscript(ctx, v.ID)
	}
	h.ev.emit("view_layout_changed", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "from": from, "to": layout})
	h.kickViews(v.Plugin, true)
	return nil
}

func (h *Hub) closePending(ctx context.Context, viewID string) bool {
	var pending bool
	h.db.QueryRowContext(ctx, `SELECT close_pending FROM view_transcripts WHERE view_id = ?`, viewID).Scan(&pending)
	return pending
}

// effectiveLayout is the SQL for the layout that a view's snapshot reports to its composer
// (views v, view_transcripts x): "transcript" while the view is paired and its transcript can
// show the history, else "". A declined transcript, or one that reached maxCrashes, gives the
// history back to the composer.
var effectiveLayout = fmt.Sprintf(`CASE WHEN v.layout = 'transcript' AND COALESCE(x.status, '') != 'declined'
	AND NOT (COALESCE(x.status, '') = 'gone' AND x.crashes >= %d) THEN 'transcript' ELSE '' END`, maxCrashes)

// transcriptChanged renders the view's snapshots again: the transcript status changes the
// effective layout.
func (h *Hub) transcriptChanged(ctx context.Context, viewID string) {
	var plugin string
	h.db.QueryRowContext(ctx, `SELECT plugin FROM views WHERE view_id = ?`, viewID).Scan(&plugin)
	if plugin != "" {
		h.kickViews(plugin, true)
	}
}

func (h *Hub) loadTranscript(ctx context.Context, viewID string) (*transcriptRole, error) {
	tr := &transcriptRole{ViewID: viewID}
	err := h.db.QueryRowContext(ctx, `SELECT status, block_id, child_pid, child_start, client_id, attachment, crashes
		FROM view_transcripts WHERE view_id = ?`, viewID).Scan(&tr.Status, &tr.Block, &tr.ChildPID, &tr.ChildStart, &tr.ClientID, &tr.Attachment, &tr.Crashes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return tr, err
}

// transcriptByBlock finds the transcript role recorded for a pane, in any status.
func (h *Hub) transcriptByBlock(ctx context.Context, blockID string) (*transcriptRole, error) {
	if blockID == "" {
		return nil, nil
	}
	var id string
	err := h.db.QueryRowContext(ctx, `SELECT view_id FROM view_transcripts WHERE block_id = ?`, blockID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h.loadTranscript(ctx, id)
}

// ensureTranscript makes the paired view v have one live transcript pane above its composer. It
// runs in the reconcile pass after v is open and placed. blocks is the session's panes at the
// start of the pass.
func (h *Hub) ensureTranscript(ctx context.Context, sess string, v *view, blocks map[string]rexBlock) error {
	tr, err := h.loadTranscript(ctx, v.ID)
	if err != nil {
		return err
	}
	if tr != nil && tr.Status == "retiring" {
		// The layout came back while a split of the earlier pairing was unknown: the same row
		// resolves it as a split in flight, so a late pane is adopted at most once.
		h.db.ExecContext(ctx, `UPDATE view_transcripts SET status = 'sending' WHERE view_id = ? AND status = 'retiring'`, v.ID)
		tr.Status = "sending"
	}
	// A pane of this view that the row does not record (a late answer, an earlier hub's pane):
	// close it when its process proves it is ours.
	for id, b := range blocks {
		if b.Label != transcriptLabel+v.ID || (tr != nil && id == tr.Block) {
			continue
		}
		if tr != nil && tr.Status == "sending" {
			continue // resolved below
		}
		if pid, err := h.childIn(ctx, sess, id); err == nil && (pid == 0 || argvHas(pid, v.ID)) {
			h.closeBlock(ctx, sess, id)
			h.ev.emit("transcript_orphan_closed", map[string]any{"view_id": v.ID, "block_id": id})
		}
	}
	if tr == nil {
		if v.Window == "" {
			return nil // a detached composer is restored first
		}
		return h.spawnTranscript(ctx, sess, v, 0)
	}
	switch tr.Status {
	case "declined":
		return nil
	case "gone":
		if v.Window == "" || tr.Crashes >= maxCrashes {
			return nil
		}
		return h.spawnTranscript(ctx, sess, v, tr.Crashes)
	case "sending":
		for id, b := range blocks {
			if b.Label != transcriptLabel+v.ID {
				continue
			}
			if pid, err := h.childIn(ctx, sess, id); err == nil && pid > 0 && argvHas(pid, v.ID) {
				return h.adoptTranscript(ctx, v, id, pid)
			}
		}
		h.setTranscript(ctx, v.ID, "gone", "") // nothing was created; the next pass opens one
		h.kickUI(v.Plugin)
		return nil
	case "exited":
		return h.dropTranscript(ctx, sess, v, tr, "exited")
	case "open":
		b, ok := blocks[tr.Block]
		if !ok {
			// Gone while the hub did not watch: the user closed it (a crash keeps the pane open).
			h.ev.emit("transcript_missing", map[string]any{"view_id": v.ID, "block_id": tr.Block})
			h.partnerClosed(ctx, v, tr, "missing")
			return nil
		}
		if b.Placement == "detached" || b.WindowID != v.Window {
			return h.dropTranscript(ctx, sess, v, tr, "moved")
		}
		if tr.Attachment == "" {
			if pid, err := h.childIn(ctx, sess, tr.Block); err == nil && pid == 0 {
				h.db.ExecContext(ctx, `UPDATE view_transcripts SET crashes = crashes + 1 WHERE view_id = ?`, v.ID)
				return h.dropTranscript(ctx, sess, v, tr, "exited unseen")
			}
		}
		return h.wrapPair(ctx, sess, v, tr)
	}
	return nil
}

// pairAnchor returns the transcript pane of the pair in a window, for a side panel: the panel
// opens beside the history, and wrapPair then puts the composer under the history. Rex has no
// anchor for a window's root split (new_split and move_block take a block anchor).
func (h *Hub) pairAnchor(ctx context.Context, window string) string {
	var block string
	h.db.QueryRowContext(ctx, `SELECT t.block_id FROM view_transcripts t JOIN views v ON v.view_id = t.view_id
		WHERE v.window_id = ? AND v.status = 'open' AND v.tab != ? AND t.status = 'open'`, window, formTab).Scan(&block)
	return block
}

// layoutNode is a node of a session.view layout tree: a leaf with block_id, or a split.
type layoutNode struct {
	BlockID string `json:"block_id"`
	Split   *struct {
		Direction string     `json:"direction"`
		Before    layoutNode `json:"before"`
		After     layoutNode `json:"after"`
	} `json:"split"`
}

// pairShape reports whether the composer sits directly under the transcript (one vertical split
// of the two leaves), and how many panes the tiled layer has.
func pairShape(n layoutNode, transcript, composer string) (paired bool, panes int) {
	if n.Split == nil {
		if n.BlockID != "" {
			return false, 1
		}
		return false, 0
	}
	s := n.Split
	if s.Direction == "vertical" && s.Before.Split == nil && s.After.Split == nil && s.Before.BlockID == transcript && s.After.BlockID == composer {
		paired = true
	}
	p1, n1 := pairShape(s.Before, transcript, composer)
	p2, n2 := pairShape(s.After, transcript, composer)
	return paired || p1 || p2, n1 + n2
}

// pairMove is session.move_block for wrapPair; lab tests replace it to lose the answer.
var pairMove = func(h *Hub, ctx context.Context, args map[string]any) error {
	return h.rex.Call(ctx, "session.move_block", args, nil)
}

// wrapPair restores the pair after a side panel opened beside the transcript:
// [(transcript | panel) / composer] becomes [(transcript / composer) | panel], with the composer
// moved under the transcript (move_block keeps the other splits). Observed on Rex
// 0.0.1792158932-g88fd41 (Codex's panel probe): move_block activates the window. So the move runs
// only while the pair's window is the session's active window, checked just before the call; a
// background pair waits, and its activation (session_view_changed) runs the pass again. Nothing
// is stored: the layout itself says that a move is due, so a hub restart needs no record.
func (h *Hub) wrapPair(ctx context.Context, sess string, v *view, tr *transcriptRole) error {
	if v.Window == "" {
		return nil
	}
	shape := func() (active, due bool, err error) {
		var sv struct {
			Active  string `json:"active_window_id"`
			Windows []struct {
				WindowID string `json:"window_id"`
				Layers   []struct {
					Kind   string     `json:"kind"`
					Layout layoutNode `json:"layout"`
				} `json:"layers"`
			} `json:"windows"`
		}
		if err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": sess}, &sv); err != nil {
			return false, false, err
		}
		for _, w := range sv.Windows {
			if w.WindowID != v.Window || len(w.Layers) == 0 {
				continue
			}
			paired, panes := pairShape(w.Layers[0].Layout, tr.Block, v.Block)
			return sv.Active == w.WindowID, !paired && panes > 2, nil
		}
		return false, false, nil
	}
	active, due, err := shape()
	if err != nil || !due {
		return err
	}
	if !active {
		h.ev.emit("pair_move_waiting", map[string]any{"view_id": v.ID, "window_id": v.Window})
		return nil
	}
	if active, due, err = shape(); err != nil || !due || !active { // again, just before the call
		return err
	}
	if err := pairMove(h, ctx, map[string]any{"session_id": sess, "block_id": v.Block, "anchor_block_id": tr.Block,
		"direction": "vertical", "side": "after", "ratio": transcriptRatio}); err != nil {
		var re *rex.RexError
		if errors.As(err, &re) && re.Code == "not_found" {
			return nil // a pane went meanwhile; the next pass sees it
		}
		if errors.As(err, &re) && re.Code == "disconnected" {
			return nil // unknown: the next pass reads the layout again
		}
		return fmt.Errorf("pair %s: move composer: %w", v.ID, err)
	}
	h.ev.emit("pair_moved", map[string]any{"view_id": v.ID, "window_id": v.Window})
	return nil
}

// spawnTranscript splits the composer's pane: the transcript takes the top 76 percent. It does not
// focus, so the composer stays the window's focused block.
func (h *Hub) spawnTranscript(ctx context.Context, sess string, v *view, crashes int) error {
	if _, err := h.db.ExecContext(ctx, `INSERT INTO view_transcripts (view_id, status, crashes, requested) VALUES (?, 'sending', ?, ?)
		ON CONFLICT (view_id) DO UPDATE SET status = 'sending', block_id = '', child_pid = 0, child_start = '', client_id = '', attachment = '',
		requested = excluded.requested`, v.ID, crashes, now()); err != nil {
		return err
	}
	args := map[string]any{"session_id": sess, "anchor_block_id": v.Block, "direction": "vertical", "side": "before",
		"ratio": transcriptRatio, "focus": false,
		"layout": map[string]any{"block": map[string]any{
			"flavor": "com.superlogical.terminal.shell",
			"label":  transcriptLabel + v.ID,
			"options": map[string]any{
				"command": h.viewCommand(v.ID, v.Plugin, v.Route, "-transcript"),
				"login":   false,
				"exit":    map[string]any{"on_completion": false}, // a crash is child_exited
			},
		}}}
	var out struct {
		BlockIDs []string `json:"block_ids"`
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := transcriptSplit(h, cctx, args, &out); err != nil {
		var re *rex.RexError
		if errors.As(err, &re) && re.Code != "disconnected" {
			// Rex answered: nothing was created. Count it, so a refusal cannot loop.
			h.db.ExecContext(ctx, `UPDATE view_transcripts SET status = 'gone', crashes = crashes + 1 WHERE view_id = ?`, v.ID)
			h.transcriptChanged(ctx, v.ID)
		}
		log.Printf("ui: transcript %s: %v", v.ID, err) // otherwise 'sending' stays unknown
		h.retryLater()                                 // no event may follow: the next attempt or resolution needs a pass
		return nil
	}
	if len(out.BlockIDs) != 1 {
		return fmt.Errorf("transcript %s: %d blocks", v.ID, len(out.BlockIDs))
	}
	pid, err := h.childIn(ctx, sess, out.BlockIDs[0])
	if err != nil {
		return err
	}
	return h.adoptTranscript(ctx, v, out.BlockIDs[0], pid)
}

// transcriptSplit is session.new_split; lab tests replace it to lose the answer.
var transcriptSplit = func(h *Hub, ctx context.Context, args map[string]any, out any) error {
	return h.rex.Call(ctx, "session.new_split", args, out)
}

// adoptTranscript records the pane of a 'sending' row. When the row moved on meanwhile (the view
// closed during the split), the pane is late: the hub closes it.
func (h *Hub) adoptTranscript(ctx context.Context, v *view, block string, pid int) error {
	res, err := h.db.ExecContext(ctx, `UPDATE view_transcripts SET status = 'open', block_id = ?, child_pid = ?, child_start = ?,
		client_id = '', attachment = '' WHERE view_id = ? AND status = 'sending'`, block, pid, startTime(pid), v.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if err := h.closeBlock(ctx, v.Session, block); err != nil {
			return fmt.Errorf("transcript %s: close late pane: %w", v.ID, err)
		}
		h.db.ExecContext(ctx, `DELETE FROM view_transcripts WHERE view_id = ? AND status = 'retiring'`, v.ID)
		h.ev.emit("transcript_late_closed", map[string]any{"view_id": v.ID, "block_id": block})
		return nil
	}
	h.ev.emit("transcript_opened", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "block_id": block})
	return nil
}

func (h *Hub) setTranscript(ctx context.Context, viewID, status, block string) {
	h.db.ExecContext(ctx, `UPDATE view_transcripts SET status = ?, block_id = ?, child_pid = 0, child_start = '', client_id = '',
		attachment = '' WHERE view_id = ?`, status, block, viewID)
	h.transcriptChanged(ctx, viewID)
}

// dropTranscript closes the transcript pane. The next pass opens a new one.
func (h *Hub) dropTranscript(ctx context.Context, sess string, v *view, tr *transcriptRole, why string) error {
	if err := h.closeBlock(ctx, sess, tr.Block); err != nil {
		return fmt.Errorf("transcript %s: close: %w", v.ID, err)
	}
	h.setTranscript(ctx, v.ID, "gone", "")
	h.ev.emit("transcript_dropped", map[string]any{"view_id": v.ID, "block_id": tr.Block, "why": why})
	h.kickUI(v.Plugin)
	return nil
}

// closeTranscript closes the transcript pane of a view that closes, and forgets the role. A
// failed close keeps the row: sweepTranscripts tries again. A split in flight keeps its record
// (retiring): its pane can still appear, and only the record finds it.
func (h *Hub) closeTranscript(ctx context.Context, viewID string) {
	tr, err := h.loadTranscript(ctx, viewID)
	if err != nil || tr == nil || tr.Status == "retiring" {
		return
	}
	if tr.Status == "sending" {
		h.db.ExecContext(ctx, `UPDATE view_transcripts SET status = 'retiring' WHERE view_id = ? AND status = 'sending'`, viewID)
		h.ev.emit("transcript_retiring", map[string]any{"view_id": viewID})
		return
	}
	if tr.Block != "" {
		var sess string
		h.db.QueryRowContext(ctx, `SELECT session_id FROM views WHERE view_id = ?`, viewID).Scan(&sess)
		if err := h.closeBlock(ctx, sess, tr.Block); err != nil {
			log.Printf("ui: close transcript %s: %v", viewID, err)
			return
		}
	}
	h.db.ExecContext(ctx, `DELETE FROM view_transcripts WHERE view_id = ?`, viewID)
	h.ev.emit("transcript_closed", map[string]any{"view_id": viewID, "block_id": tr.Block})
}

// sweepTranscripts closes the transcript roles of closed views (a failed close, a crash between
// the two closes, or a split that was in flight when the view closed), and of open views that are
// no longer paired (a failed close, or a split in flight when the layout went). It keeps a pending
// close of the pair: the composer's gate still runs.
func (h *Hub) sweepTranscripts(ctx context.Context) {
	rows, err := h.db.QueryContext(ctx, `SELECT t.view_id, t.status, t.requested, COALESCE(v.session_id, '') FROM view_transcripts t
		LEFT JOIN views v ON v.view_id = t.view_id WHERE v.view_id IS NULL OR v.status = 'closed'
		OR (v.layout != 'transcript' AND t.close_pending = 0)`)
	if err != nil {
		return
	}
	type row struct{ id, status, requested, sess string }
	var list []row
	for rows.Next() {
		var r row
		rows.Scan(&r.id, &r.status, &r.requested, &r.sess)
		list = append(list, r)
	}
	rows.Close()
	for _, r := range list {
		if r.status != "sending" && r.status != "retiring" {
			h.closeTranscript(ctx, r.id)
			continue
		}
		h.retireLateSplit(ctx, r.id, r.sess)
	}
}

// retireLateSplit resolves the split record of a closed or unpaired view: it closes a pane with the view's
// transcript label and process proof. The record goes only on a definite result: a pane closed,
// or the session is gone. A late pane changes the session's layout (session_view_changed), which
// runs the sweep again, so a waiting record needs no timer.
func (h *Hub) retireLateSplit(ctx context.Context, id, sess string) {
	found := false
	if sess != "" {
		blocks, err := h.blocksIn(ctx, sess)
		if sessionGone(err) {
			h.db.ExecContext(ctx, `DELETE FROM view_transcripts WHERE view_id = ?`, id)
			return
		}
		if err != nil {
			h.retryLater()
			return
		}
		for b, info := range blocks {
			if info.Label != transcriptLabel+id {
				continue
			}
			if pid, err := h.childIn(ctx, sess, b); err == nil && (pid == 0 || argvHas(pid, id)) {
				if err := h.closeBlock(ctx, sess, b); err != nil {
					h.retryLater()
					return
				}
				found = true
				h.ev.emit("transcript_late_closed", map[string]any{"view_id": id, "block_id": b})
			}
		}
	}
	if found || sess == "" {
		h.db.ExecContext(ctx, `DELETE FROM view_transcripts WHERE view_id = ?`, id)
	}
}

// partnerClosed handles the user's close of the transcript (its pane, or Ctrl+Q in it). The
// transcript pane goes and no new one opens. The composer then closes through its own gate:
// the hub asks its renderer (view.close_request), which saves drafts and waits for operations,
// then calls hub.view.close. While the gate does not pass, the composer stays, alone.
// The request persists (close_pending): a composer that is not attached now (for example after a
// hub restart, with an unsaved draft) gets it when it attaches. Only a composer whose renderer
// process is gone closes at once: nothing unsaved can be left in it.
func (h *Hub) partnerClosed(ctx context.Context, v *view, tr *transcriptRole, why string) {
	h.setTranscript(ctx, v.ID, "declined", "")
	h.db.ExecContext(ctx, `UPDATE view_transcripts SET close_pending = 1 WHERE view_id = ?`, v.ID)
	if tr.Block != "" {
		if err := h.closeBlock(ctx, v.Session, tr.Block); err != nil {
			log.Printf("ui: close transcript %s: %v", v.ID, err)
		}
	}
	h.ev.emit("transcript_declined", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "why": why})
	if !alive(v.ChildPID, v.ChildStart) {
		h.closeViewRow(ctx, v.ID, "user")
		h.db.ExecContext(ctx, `UPDATE ui_tabs SET suppressed = 1, closed_revision = revision WHERE plugin = ? AND tab = ?`, v.Plugin, v.Tab)
		if err := h.closeBlock(ctx, v.Session, v.Block); err != nil {
			log.Printf("ui: close %s: %v", v.ID, err)
		}
		h.ev.emit("view_closed", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "tab": v.Tab, "by": "user", "from": "transcript"})
		h.kickUI(v.Plugin)
		return
	}
	if v.Attachment == "" {
		h.ev.emit("view_close_pending", map[string]any{"plugin": v.Plugin, "view_id": v.ID})
		return // composerAttached sends the request
	}
	go h.requestClose(v.Plugin, v.ID, v.ClientID, v.Attachment)
}

// requestClose sends view.close_request to a composer attachment. The renderer can get the
// request just before it records the attachment and refuse it, so a refusal is retried while the
// attachment is current. Each try checks that the close is still pending: a navigation cancels it.
func (h *Hub) requestClose(plugin, viewID, client, attachment string) {
	ctx := context.Background()
	var err error
	for i := 0; i < 3; i++ {
		var pending bool
		h.db.QueryRowContext(ctx, `SELECT close_pending FROM view_transcripts WHERE view_id = ?`, viewID).Scan(&pending)
		if !pending {
			h.ev.emit("view_close_cancelled", map[string]any{"plugin": plugin, "view_id": viewID})
			return
		}
		err = h.rex.Call(ctx, "client.perform", map[string]any{"client_id": client, "action": "view.close_request",
			"args": map[string]any{"view_id": viewID, "attachment": attachment}, "timeout_ms": 5000}, nil)
		if err == nil || h.composerAttachment(ctx, viewID) != attachment {
			break
		}
		time.Sleep(time.Duration(i+1) * 300 * time.Millisecond)
	}
	h.ev.emit("view_close_requested", map[string]any{"plugin": plugin, "view_id": viewID, "error": errString(err)})
}

// composerAttached sends a pending close request to a new composer attachment.
func (h *Hub) composerAttached(ctx context.Context, plugin, viewID, client, attachment string) {
	var pending bool
	h.db.QueryRowContext(ctx, `SELECT close_pending FROM view_transcripts WHERE view_id = ?`, viewID).Scan(&pending)
	if pending {
		go h.requestClose(plugin, viewID, client, attachment)
	}
}

// transcriptBlockClosed handles the close of a transcript pane. It reports whether the pane was one.
func (h *Hub) transcriptBlockClosed(ctx context.Context, blockID string, self bool) bool {
	tr, err := h.transcriptByBlock(ctx, blockID)
	if err != nil || tr == nil {
		return false
	}
	if self || tr.Status != "open" && tr.Status != "exited" {
		return true // the hub's own close; the row already says what comes next
	}
	v, err := h.viewByID(ctx, tr.ViewID)
	if err != nil || v == nil {
		h.db.ExecContext(ctx, `DELETE FROM view_transcripts WHERE view_id = ?`, tr.ViewID)
		return true
	}
	h.partnerClosed(ctx, v, tr, "user")
	return true
}

// transcriptChildExited records a printer that exited in its kept-open pane. The worker replaces
// it, up to maxCrashes times; a navigation to the tab resets the count.
func (h *Hub) transcriptChildExited(ctx context.Context, blockID string) bool {
	tr, err := h.transcriptByBlock(ctx, blockID)
	if err != nil || tr == nil {
		return false
	}
	var crashes int
	h.db.QueryRowContext(ctx, `UPDATE view_transcripts SET status = 'exited', client_id = '', attachment = '', crashes = crashes + 1
		WHERE view_id = ? AND status = 'open' RETURNING crashes`, tr.ViewID).Scan(&crashes)
	var plugin string
	h.db.QueryRowContext(ctx, `SELECT plugin FROM views WHERE view_id = ?`, tr.ViewID).Scan(&plugin)
	h.ev.emit("transcript_exited", map[string]any{"view_id": tr.ViewID, "crashes": crashes})
	h.kickUI(plugin)
	return true
}

func (h *Hub) viewByID(ctx context.Context, id string) (*view, error) {
	v := &view{}
	err := h.db.QueryRowContext(ctx, `SELECT view_id, plugin, tab, route, status, session_id, window_id, block_id, child_pid, child_start,
		client_id, attachment, layout FROM views WHERE view_id = ? AND status = 'open'`, id).Scan(&v.ID, &v.Plugin, &v.Tab, &v.Route,
		&v.Status, &v.Session, &v.Window, &v.Block, &v.ChildPID, &v.ChildStart, &v.ClientID, &v.Attachment, &v.Layout)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

// transcriptAttach binds a printer to the transcript role of its view. The caller must run in
// the transcript's pane.
func (h *Hub) transcriptAttach(ctx context.Context, p perform, id string) (any, error) {
	v, err := h.viewByID(ctx, id)
	if err != nil || v == nil {
		return nil, bad("unknown or closed view %q", id)
	}
	tr, err := h.loadTranscript(ctx, id)
	if err != nil || tr == nil || tr.Status == "declined" || tr.Status == "gone" || tr.Status == "exited" {
		return nil, bad("view %q has no transcript", id)
	}
	if tr.Status != "open" {
		return nil, bad("not_ready: the transcript of view %q is still opening", id)
	}
	if !alive(tr.ChildPID, tr.ChildStart) || !descendsFrom(p.Requester.Principal.PID, tr.ChildPID) {
		return nil, bad("caller is not in the transcript pane of view %q", id)
	}
	attachment := randomID("att-")
	if _, err := h.db.ExecContext(ctx, `UPDATE view_transcripts SET client_id = ?, attachment = ? WHERE view_id = ?`, p.Requester.ClientID, attachment, id); err != nil {
		return nil, err
	}
	h.ev.emit("transcript_attached", map[string]any{"view_id": id, "client_id": p.Requester.ClientID})
	return map[string]any{"view_id": id, "plugin": v.Plugin, "route": v.Route, "user": viewUser, "role": layoutTranscript,
		"hub_generation": revString(h.generation), "attachment": attachment}, nil
}

// readCaller checks that the caller is the view's composer or its transcript. Only reads use it:
// a snapshot, a subscription, and a read notice.
func (h *Hub) readCaller(ctx context.Context, p perform, args map[string]any) (*boundView, error) {
	v, err := h.boundCaller(ctx, p, args)
	if err == nil {
		return v, nil
	}
	if tv, ok := h.transcriptCaller(ctx, p, args); ok {
		return tv, nil
	}
	return nil, err
}

func (h *Hub) transcriptCaller(ctx context.Context, p perform, args map[string]any) (*boundView, bool) {
	id, _ := args["view_id"].(string)
	att, _ := args["attachment"].(string)
	if att == "" {
		return nil, false
	}
	var v boundView
	var client, attachment string
	err := h.db.QueryRowContext(ctx, `SELECT v.plugin, v.route, v.context, t.client_id, t.attachment FROM view_transcripts t
		JOIN views v ON v.view_id = t.view_id WHERE t.view_id = ? AND t.status = 'open' AND v.status = 'open'`, id).
		Scan(&v.Plugin, &v.Route, &v.Context, &client, &attachment)
	if err != nil || att != attachment || client != p.Requester.ClientID {
		return nil, false
	}
	v.ID = id
	return &v, true
}

// transcriptClose is hub.view.close from a transcript (Ctrl+Q in it): the user's close of the pair.
func (h *Hub) transcriptClose(ctx context.Context, p perform, args map[string]any) (any, bool) {
	tv, ok := h.transcriptCaller(ctx, p, args)
	if !ok {
		return nil, false
	}
	v, err := h.viewByID(ctx, tv.ID)
	tr, err2 := h.loadTranscript(ctx, tv.ID)
	if err != nil || err2 != nil || v == nil || tr == nil {
		return nil, false
	}
	// The printer gets its answer before its pane closes.
	go func() {
		time.Sleep(300 * time.Millisecond)
		h.partnerClosed(context.Background(), v, tr, "view")
	}()
	return map[string]any{"status": "closed"}, true
}

// transcriptTargets returns, per view of the plugin, the attached printer: client and attachment.
func (h *Hub) transcriptTargets(ctx context.Context, plugin string) map[string][2]string {
	out := map[string][2]string{}
	rows, err := h.db.QueryContext(ctx, `SELECT t.view_id, t.client_id, t.attachment FROM view_transcripts t
		JOIN views v ON v.view_id = t.view_id WHERE v.plugin = ? AND v.status = 'open' AND t.status = 'open' AND t.attachment != ''`, plugin)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, client, att string
		rows.Scan(&id, &client, &att)
		out[id] = [2]string{client, att}
	}
	return out
}

// transcriptAttachment is the printer's current attachment, for the stale check of view.changed.
func (h *Hub) transcriptAttachment(ctx context.Context, viewID string) string {
	var att string
	h.db.QueryRowContext(ctx, `SELECT attachment FROM view_transcripts WHERE view_id = ? AND status = 'open'`, viewID).Scan(&att)
	return att
}

// resetTranscripts lets a navigation to a tab bring its transcript back: after the user closed it
// (declined) or after too many crashes. It runs in the navigation's transaction: the hub's
// database has one connection, so a second statement outside it would wait forever.
func resetTranscripts(ctx context.Context, tx *sql.Tx, plugin, tabKey string) error {
	_, err := tx.ExecContext(ctx, `UPDATE view_transcripts SET crashes = 0, close_pending = 0, status = CASE status WHEN 'declined' THEN 'gone' ELSE status END
		WHERE view_id IN (SELECT view_id FROM views WHERE plugin = ? AND tab = ? AND status = 'open')`, plugin, tabKey)
	return err
}
