package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// confirmReplyHub is a hub with one form v (block b-old, attached by client c) and its replacement
// R in confirming, on a fake Rex. replyCode is the error code that Rex gives client.reply ("" for
// success).
func confirmReplyHub(t *testing.T, replyCode string) (*Hub, *performRex) {
	t.Helper()
	h := releaseTestHub(t)
	h.selfClosed = map[string]bool{}
	h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, session_id, block_id, client_id, attachment, attach_state) VALUES ('v', 'p', ?, '/forms/a', 'open', 's', 'b-old', 'c', 'att', 'attached')`, formTab)
	h.db.Exec(`INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, state, next_title, created)
		VALUES ('R', 's', 'w', 'v', 'n', 'p', '/forms/b', '{}', 'confirming', 'Next', '')`)
	fr := newPerformRex(t, func(performCall) (any, string) { return map[string]any{}, "" })
	fr.other = func(method string, _ json.RawMessage) (any, string) {
		if method == "client.reply" {
			return nil, replyCode
		}
		return nil, ""
	}
	h.rex = fr.dial(t)
	return h, fr
}

func discardPerform() perform {
	var p perform
	p.Action, p.CallID = "hub.view.release_confirm", "call-1"
	p.Requester.ClientID = "c"
	p.Args, _ = json.Marshal(map[string]any{"view_id": "v", "attachment": "att", "replacement_id": "R", "choice": "discard"})
	return p
}

func discardArgs() map[string]any {
	return map[string]any{"view_id": "v", "attachment": "att", "replacement_id": "R", "choice": "discard"}
}

func confirmCaller() perform {
	var p perform
	p.Requester.ClientID = "c"
	return p
}

// paneWork reports whether the hub made a request that starts the advance (it opens the new pane).
func paneWork(m []string) bool {
	return slices.Contains(m, "session.attach") || slices.Contains(m, "session.new_split") || slices.Contains(m, "block.close")
}

// Discard in the old renderer's prompt: the hub replies to hub.view.release_confirm before it does
// any pane work, because the advance closes the caller's own pane. On the live hub (2026-10-26) the
// close came first, and Rex refused the late reply ("no call is waiting"). A repeat of the choice
// returns the row's state and advances nothing.
func TestReleaseConfirmRepliesBeforeTheAdvance(t *testing.T) {
	h, fr := confirmReplyHub(t, "")
	h.answer(context.Background(), discardPerform())
	m := fr.methodsSeen()
	if len(m) < 2 || m[0] != "client.reply" || !paneWork(m) {
		t.Fatalf("requests %v, want client.reply first, then the advance", m)
	}
	var state string
	h.db.QueryRow(`SELECT state FROM ui_replacements WHERE repl_id = 'R'`).Scan(&state)
	if state == "confirming" {
		t.Fatalf("the row is still confirming")
	}

	before := len(fr.methodsSeen())
	h.answer(context.Background(), discardPerform())
	time.Sleep(200 * time.Millisecond) // nothing runs after the second reply
	if m := fr.methodsSeen()[before:]; !slices.Equal(m, []string{"client.reply"}) {
		t.Fatalf("a repeated choice made requests %v, want only its reply", m)
	}
}

// While the reply is in flight, no other path advances the row: a sweep (a layout event) and a
// newer open in the window do nothing to it. The callback then advances it.
func TestReleaseConfirmGateHoldsTheSweep(t *testing.T) {
	h, fr := confirmReplyHub(t, "")
	ctx := context.Background()
	out, err := h.releaseConfirm(ctx, confirmCaller(), discardArgs())
	tr, ok := out.(thenResult)
	if err != nil || !ok {
		t.Fatalf("releaseConfirm: %v %v, want a thenResult", out, err)
	}
	h.slotSweep(ctx)
	r, _ := h.slotRow(ctx, h.db, "R")
	if _, err := h.advanceSlot(ctx, r); err != nil {
		t.Fatalf("advanceSlot: %v", err)
	}
	h.cfg.View = "lazyrex-view"
	newer := slotRequest{ReplID: "R2", Plugin: "p", Route: "/forms/c", Ctx: formContext{SessionID: "s", WindowID: "w"}}
	if _, err := h.openSlot(ctx, newer); err == nil {
		t.Fatal("a newer open in the window was accepted while the reply was in flight")
	}
	if m := fr.methodsSeen(); paneWork(m) {
		t.Fatalf("pane work before the reply: %v", m)
	}
	var state string
	h.db.QueryRow(`SELECT state FROM ui_replacements WHERE repl_id = 'R'`).Scan(&state)
	if state != "opening" {
		t.Fatalf("the row is %q before the reply, want opening", state)
	}
	tr.then()
	if m := fr.methodsSeen(); !paneWork(m) {
		t.Fatalf("the callback did not advance the row: %v", m)
	}
}

// A failed reply (the caller is gone, or Rex refused it) still advances the row.
func TestReleaseConfirmFailedReplyAdvances(t *testing.T) {
	h, fr := confirmReplyHub(t, "not_found")
	h.answer(context.Background(), discardPerform())
	if m := fr.methodsSeen(); len(m) == 0 || m[0] != "client.reply" || !paneWork(m) {
		t.Fatalf("requests %v, want the failed reply, then the advance", m)
	}
}

// A hub that stops after the choice and before the callback loses the gate (memory only). The row
// is durably opening, and the restart sweep advances it.
func TestReleaseConfirmRestartSweepAdvances(t *testing.T) {
	h, fr := confirmReplyHub(t, "")
	ctx := context.Background()
	if _, err := h.releaseConfirm(ctx, confirmCaller(), discardArgs()); err != nil {
		t.Fatal(err)
	}
	var state string
	h.db.QueryRow(`SELECT state FROM ui_replacements WHERE repl_id = 'R'`).Scan(&state)
	if state != "opening" {
		t.Fatalf("the row is %q, want opening", state)
	}
	h.slots = slotState{} // a restarted hub: its memory has no gate; the callback never runs
	h.slotSweep(ctx)
	if m := fr.methodsSeen(); !paneWork(m) {
		t.Fatalf("the restart sweep did not advance the row: %v", m)
	}
}
