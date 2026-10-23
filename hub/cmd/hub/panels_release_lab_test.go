package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// A form's end opens its return panel; the user's next command arrives before the panel's renderer
// attaches (Tunnels forward.lua test 8, 2026-10-19). The lab renderer only sleeps, so the panel never
// attaches: the order is fixed without timing.

// A view that never attached holds no user input. A replacement takes its place without asking it.
func TestSlotReplacesNeverAttachedViewLab(t *testing.T) {
	h, _, sess, win := panelsLabHub(t)
	ctx := context.Background()
	if out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-first", Plugin: "p", Route: "/panels/a", Ctx: panel(sess, win)}); err != nil || out["status"] != "opened" {
		t.Fatalf("first panel: %v %v", out, err)
	}
	out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-second", Plugin: "p", Route: "/panels/b", Ctx: panel(sess, win)})
	if err != nil || out["status"] != "opened" {
		t.Fatalf("replacing a panel that never attached: %v %v", out, err)
	}
	if st := replState(t, h, "repl-second"); st != "done" {
		t.Fatalf("state %s", st)
	}
}

// labRenderer is a second Rex client that plays an old panel's renderer. It answers view.release
// with release and view.resume with resumed, and records each request.
type labRenderer struct {
	id  string
	got chan perform
}

func newLabRenderer(t *testing.T, h *Hub, release string) *labRenderer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	socket, err := rex.SocketPath(h.cfg.Server) // the lab server of the hub under test
	if err != nil {
		t.Fatal(err)
	}
	c, err := rex.Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	var welcome struct {
		Client struct {
			ID string `json:"id"`
		} `json:"client"`
	}
	if err := json.Unmarshal(c.Welcome, &welcome); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "client.keymap", map[string]any{"actions": []any{
		map[string]any{"name": "view.release", "kind": "client", "title": "Release"},
		map[string]any{"name": "view.resume", "kind": "client", "title": "Resume"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	lr := &labRenderer{id: welcome.Client.ID, got: make(chan perform, 8)}
	go func() {
		for raw := range c.Events {
			var frame struct {
				Payload struct {
					Type    string  `json:"type"`
					Payload perform `json:"payload"`
				} `json:"payload"`
			}
			if json.Unmarshal(raw, &frame) != nil || frame.Payload.Type != "client_perform_requested" {
				continue
			}
			p := frame.Payload.Payload
			lr.got <- p
			status := "resumed"
			if p.Action == "view.release" {
				status = release
			}
			c.Call(ctx, "client.reply", map[string]any{"call_id": p.CallID, "result": map[string]any{"status": status}}, nil)
		}
	}()
	return lr
}

// next returns the renderer's next request, or fails after 10 s.
func (lr *labRenderer) next(t *testing.T) perform {
	t.Helper()
	select {
	case p := <-lr.got:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("the renderer got no request")
	}
	return perform{}
}

// attachedTo records an attachment of view to the lab renderer, as viewAttach writes it.
func attachedTo(t *testing.T, h *Hub, view string, lr *labRenderer) {
	t.Helper()
	if _, err := h.db.Exec(`UPDATE views SET client_id = ?, attachment = 'att-lab', attach_state = 'attached' WHERE view_id = ?`, lr.id, view); err != nil {
		t.Fatal(err)
	}
}

func attachState(t *testing.T, h *Hub, view string) string {
	var st string
	if err := h.db.QueryRow(`SELECT attach_state FROM views WHERE view_id = ?`, view).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func openFirstPanel(t *testing.T, h *Hub, sess, win string) string {
	t.Helper()
	if out, err := h.openSlot(context.Background(), slotRequest{ReplID: "repl-first", Plugin: "p", Route: "/panels/a", Ctx: panel(sess, win)}); err != nil || out["status"] != "opened" {
		t.Fatalf("first panel: %v %v", out, err)
	}
	return slotViewID("repl-first")
}

// T2: a renderer that attached first gets view.release, as before.
func TestSlotAttachedViewIsAskedLab(t *testing.T) {
	h, _, sess, win := panelsLabHub(t)
	lr := newLabRenderer(t, h, "released")
	old := openFirstPanel(t, h, sess, win)
	attachedTo(t, h, old, lr)
	out, err := h.openSlot(context.Background(), slotRequest{ReplID: "repl-second", Plugin: "p", Route: "/panels/b", Ctx: panel(sess, win)})
	if err != nil || out["status"] != "opened" {
		t.Fatalf("replace: %v %v", out, err)
	}
	if p := lr.next(t); p.Action != "view.release" {
		t.Fatalf("the attached renderer got %s", p.Action)
	}
}

// T3, T3b: a view that attached before and is not attached now (a hub restart cleared it), and a
// row from an older hub (unknown), keep their place: the renderer can hold input.
func TestSlotDetachedOrUnknownViewConflictLab(t *testing.T) {
	for _, state := range []string{"attached", "unknown"} {
		t.Run(state, func(t *testing.T) {
			h, _, sess, win := panelsLabHub(t)
			old := openFirstPanel(t, h, sess, win)
			h.db.Exec(`UPDATE views SET attachment = '', attach_state = ? WHERE view_id = ?`, state, old)
			_, err := h.openSlot(context.Background(), slotRequest{ReplID: "repl-second", Plugin: "p", Route: "/panels/b", Ctx: panel(sess, win)})
			if err == nil || !strings.Contains(err.Error(), "try again in a moment") {
				t.Fatalf("want the reconnect conflict, got %v", err)
			}
			if st := replState(t, h, "repl-second"); st != "cancelled" {
				t.Fatalf("state %s", st)
			}
			if got := attachState(t, h, old); got != state {
				t.Fatalf("attach_state %s", got)
			}
		})
	}
}

// T4: a renderer with unsaved input answers confirm; the row waits for the user.
func TestSlotDirtyRendererConfirmsLab(t *testing.T) {
	h, _, sess, win := panelsLabHub(t)
	lr := newLabRenderer(t, h, "confirm")
	old := openFirstPanel(t, h, sess, win)
	attachedTo(t, h, old, lr)
	out, err := h.openSlot(context.Background(), slotRequest{ReplID: "repl-second", Plugin: "p", Route: "/panels/b", Ctx: panel(sess, win)})
	if err != nil || out["status"] != "pending_confirmation" {
		t.Fatalf("dirty: %v %v", out, err)
	}
	if st := replState(t, h, "repl-second"); st != "confirming" {
		t.Fatalf("state %s", st)
	}
}

// T5: a renderer that attaches after the claim follows the replacement (the attach reply names it);
// when the new split is refused, the hub resumes that renderer, and the view stays attached.
func TestSlotLateAttachAfterClaimIsResumedLab(t *testing.T) {
	h, _, sess, win := panelsLabHub(t)
	lr := newLabRenderer(t, h, "released")
	if err := h.viewsStart(context.Background()); err != nil { // viewAttach kicks the view workers
		t.Fatal(err)
	}
	old := openFirstPanel(t, h, sess, win)
	var pid int
	h.db.QueryRow(`SELECT child_pid FROM views WHERE view_id = ?`, old).Scan(&pid)
	var attached map[string]any
	orig := slotSplit
	slotSplit = func(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
		slotSplit = orig
		var p perform
		p.Requester.ClientID, p.Requester.Principal.PID = lr.id, pid
		res, err := h.viewAttach(ctx, p, map[string]any{"view_id": old, "schema_version": float64(1)})
		if err != nil {
			t.Errorf("late attach: %v", err)
		}
		attached, _ = res.(map[string]any)
		return &rex.RexError{Code: "refused", Message: "lab: the split is refused"}
	}
	t.Cleanup(func() { slotSplit = orig })
	if _, err := h.openSlot(context.Background(), slotRequest{ReplID: "repl-second", Plugin: "p", Route: "/panels/b", Ctx: panel(sess, win)}); err == nil {
		t.Fatal("a refused split opened the slot")
	}
	repl, _ := attached["replacement"].(map[string]any)
	if repl["replacement_id"] != "repl-second" || repl["state"] != "opening" {
		t.Fatalf("the late attach did not follow the claim: %v", attached)
	}
	if st := replState(t, h, "repl-second"); st != "cancelled" {
		t.Fatalf("state %s", st)
	}
	if got := attachState(t, h, old); got != "attached" {
		t.Fatalf("attach_state %s", got)
	}
	p := lr.next(t)
	var args struct {
		ReplacementID string `json:"replacement_id"`
	}
	json.Unmarshal(p.Args, &args)
	if p.Action != "view.resume" || args.ReplacementID != "repl-second" {
		t.Fatalf("the renderer got %s %s, want view.resume", p.Action, args.ReplacementID)
	}
}

// T5b: a claim whose new split is refused, with no attach: the view is never-attached again.
func TestSlotRefusedClaimIsUndoneLab(t *testing.T) {
	h, _, sess, win := panelsLabHub(t)
	old := openFirstPanel(t, h, sess, win)
	orig := slotSplit
	slotSplit = func(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
		slotSplit = orig
		return &rex.RexError{Code: "refused", Message: "lab: the split is refused"}
	}
	t.Cleanup(func() { slotSplit = orig })
	h.openSlot(context.Background(), slotRequest{ReplID: "repl-second", Plugin: "p", Route: "/panels/b", Ctx: panel(sess, win)})
	if st := replState(t, h, "repl-second"); st != "cancelled" {
		t.Fatalf("state %s", st)
	}
	if got := attachState(t, h, old); got != "never" {
		t.Fatalf("attach_state %s", got)
	}
}

// T6: a crash after a cancel and before its undo: the hub start undoes the claim. A claim of an
// active row stays.
func TestSlotStartUndoesEndedClaimsLab(t *testing.T) {
	h, _, sess, win := panelsLabHub(t)
	old := openFirstPanel(t, h, sess, win)
	h.db.Exec(`UPDATE views SET attach_state = 'claimed' WHERE view_id = ?`, old)
	h.db.Exec(`INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, state, created)
		VALUES ('repl-ended', ?, ?, ?, 'view-x', 'p', '/panels/b', '{}', 'cancelled', '')`, sess, win, old)
	if err := h.slotStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := attachState(t, h, old); got != "never" {
		t.Fatalf("after start: %s", got)
	}
	h.db.Exec(`UPDATE views SET attach_state = 'claimed' WHERE view_id = ?`, old)
	h.db.Exec(`UPDATE ui_replacements SET state = 'opening' WHERE repl_id = 'repl-ended'`)
	h.slotStart(context.Background())
	if got := attachState(t, h, old); got != "claimed" {
		t.Fatalf("an active claim was undone: %s", got)
	}
}

// T7: two replacements claim one never-attached view: exactly one wins; the other is a conflict.
// The loser's cancel keeps the winner's claim, so a third claim cannot win; the winner's own end
// undoes it.
func TestSlotTwoClaimsOneWinsLab(t *testing.T) {
	h, _, sess, win := panelsLabHub(t)
	old := openFirstPanel(t, h, sess, win)
	ctx := context.Background()
	rows := []*slotRow{}
	for _, id := range []string{"repl-a", "repl-b", "repl-c"} {
		h.db.Exec(`INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, state, created)
			VALUES (?, ?, ?, ?, ?, 'p', '/panels/b', '{}', 'releasing', '')`, id, sess, win, old, slotViewID(id))
		r, _ := h.slotRow(ctx, h.db, id)
		rows = append(rows, r)
	}
	won, err := h.claimUnattached(ctx, rows[0])
	if err != nil || !won {
		t.Fatalf("first claim: %v %v", won, err)
	}
	if won, err := h.claimUnattached(ctx, rows[1]); err != nil || won {
		t.Fatalf("second claim: %v %v", won, err)
	}
	if err := h.askRelease(ctx, rows[1]); err == nil || !strings.Contains(err.Error(), "try again in a moment") {
		t.Fatalf("the second replacement: %v", err)
	}
	if replState(t, h, "repl-a") != "opening" || replState(t, h, "repl-b") != "cancelled" {
		t.Fatalf("states %s %s", replState(t, h, "repl-a"), replState(t, h, "repl-b"))
	}
	if got := attachState(t, h, old); got != "claimed" {
		t.Fatalf("the loser's cancel undid the winner's claim: attach_state=%s", got)
	}
	if won, err := h.claimUnattached(ctx, rows[2]); err != nil || won {
		t.Fatalf("third claim: %v %v", won, err)
	}
	if err := h.setSlotState(ctx, rows[2], "cancelled"); err != nil {
		t.Fatal(err)
	}
	if got := attachState(t, h, old); got != "claimed" {
		t.Fatalf("the third row's cancel undid the winner's claim: attach_state=%s", got)
	}
	if err := h.setSlotState(ctx, rows[0], "cancelled"); err != nil {
		t.Fatal(err)
	}
	if got := attachState(t, h, old); got != "never" {
		t.Fatalf("the winner's end kept its claim: attach_state=%s", got)
	}
}
