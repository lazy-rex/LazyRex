package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// Discard in the old renderer's prompt: the hub replies to hub.view.release_confirm before it does
// any pane work, because the advance closes the caller's own pane. On the live hub (2026-10-26) the
// close came first, and Rex refused the late reply ("no call is waiting"). A repeat of the choice
// returns the row's state and advances nothing.
func TestReleaseConfirmRepliesBeforeTheAdvance(t *testing.T) {
	h := releaseTestHub(t)
	h.selfClosed = map[string]bool{}
	h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, session_id, block_id, client_id, attachment, attach_state) VALUES ('v', 'p', ?, '/forms/a', 'open', 's', 'b-old', 'c', 'att', 'attached')`, formTab)
	h.db.Exec(`INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, state, next_title, created)
		VALUES ('R', 's', 'w', 'v', 'n', 'p', '/forms/b', '{}', 'confirming', 'Next', '')`)
	fr := newPerformRex(t, func(performCall) (any, string) { return map[string]any{}, "" })
	h.rex = fr.dial(t)
	choice := func() {
		var p perform
		p.Action, p.CallID = "hub.view.release_confirm", "call-1"
		p.Requester.ClientID = "c"
		p.Args, _ = json.Marshal(map[string]any{"view_id": "v", "attachment": "att", "replacement_id": "R", "choice": "discard"})
		h.answer(context.Background(), p)
	}
	choice()
	m := fr.methodsSeen()
	if len(m) < 2 || m[0] != "client.reply" {
		t.Fatalf("requests %v, want client.reply first, then the advance", m)
	}
	var state string
	h.db.QueryRow(`SELECT state FROM ui_replacements WHERE repl_id = 'R'`).Scan(&state)
	if state == "confirming" {
		t.Fatalf("the row is still confirming")
	}

	before := len(fr.methodsSeen())
	choice()
	time.Sleep(200 * time.Millisecond) // nothing runs after the second reply
	if m := fr.methodsSeen()[before:]; !slices.Equal(m, []string{"client.reply"}) {
		t.Fatalf("a repeated choice made requests %v, want only its reply", m)
	}
}
