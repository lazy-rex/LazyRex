package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// unseenHub is reopenTestHub with one more open, unattached tab view per id in ids (pane b-<id>),
// each inserted as this hub process would (own) when own is true.
func unseenHub(t *testing.T, own bool, ids ...string) (*Hub, *reopenRex) {
	t.Helper()
	h, x, _ := reopenTestHub(t)
	mustExec(t, h, `UPDATE views SET status = 'closed' WHERE view_id = 'old'`)
	for _, id := range ids {
		mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, session_id, window_id, block_id, attach_state)
			VALUES (?, 'p', 'main', '/main', 'open', 's', 'w', ?, 'never')`, id, "b-"+id)
		if own {
			h.markOwn(id)
		}
	}
	return h, x
}

func unseenView(t *testing.T, h *Hub, id string) *view {
	t.Helper()
	v, err := h.viewByBlock(context.Background(), "b-"+id)
	if err != nil || v == nil {
		t.Fatalf("view %s: %v %v", id, v, err)
	}
	return v
}

func tabCrashes(h *Hub) (crashes, suppressed int) {
	h.db.QueryRow(`SELECT crashes, suppressed FROM ui_tabs WHERE plugin = 'p' AND tab = 'main'`).Scan(&crashes, &suppressed)
	return
}

// A pass that finds the renderer of an own view gone counts the crash; the exit event that comes
// later counts nothing more (U1). The event first, then the pass: one count (U2).
func TestUnseenExitCountsOnce(t *testing.T) {
	ctx := context.Background()
	h, x := unseenHub(t, true, "v1")
	h.unseenExit(ctx, unseenView(t, h, "v1"))
	h.uiChildExited(ctx, "b-v1")
	if c, _ := tabCrashes(h); c != 1 {
		t.Fatalf("the pass first: crashes %d, want 1", c)
	}
	if s, by, _ := viewStatus(h, "v1"); s != "closed" || by != "hub" || len(x.closed) != 1 {
		t.Fatalf("the view: %s by %s, closed panes %v", s, by, x.closed)
	}

	h, _ = unseenHub(t, true, "v2")
	v := unseenView(t, h, "v2") // the pass read the row before the event
	h.uiChildExited(ctx, "b-v2")
	h.unseenExit(ctx, v)
	if c, _ := tabCrashes(h); c != 1 {
		t.Fatalf("the event first: crashes %d, want 1", c)
	}
}

// Both paths read the row as open, then run at the same time: one transition, one count (U3).
func TestUnseenExitConcurrent(t *testing.T) {
	ctx := context.Background()
	h, _ := unseenHub(t, true, "v1")
	v := unseenView(t, h, "v1")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				h.rendererExited(ctx, v)
			} else {
				h.uiChildExited(ctx, "b-v1")
			}
		}()
	}
	wg.Wait()
	if c, _ := tabCrashes(h); c != 1 {
		t.Fatalf("crashes %d, want 1", c)
	}
}

// A dead renderer of an earlier hub process (not own) closes with no count, also three times, so
// hub restarts never suppress a tab (U4). Three own unseen exits suppress it (U5).
func TestUnseenExitOwnOnly(t *testing.T) {
	ctx := context.Background()
	ids := []string{"v1", "v2", "v3"}
	h, x := unseenHub(t, false, ids...)
	for _, id := range ids {
		h.unseenExit(ctx, unseenView(t, h, id))
	}
	if c, s := tabCrashes(h); c != 0 || s != 0 || len(x.closed) != 3 {
		t.Fatalf("earlier views: crashes %d, suppressed %d, closed %v", c, s, x.closed)
	}

	h, _ = unseenHub(t, true, ids...)
	for _, id := range ids {
		h.unseenExit(ctx, unseenView(t, h, id))
	}
	if c, s := tabCrashes(h); c != maxCrashes || s != 1 {
		t.Fatalf("own views: crashes %d, suppressed %d; want %d and 1", c, s, maxCrashes)
	}
	h.ui.mu.Lock()
	left := fmt.Sprint(h.ui.own)
	h.ui.mu.Unlock()
	if left != "map[]" {
		t.Fatalf("a closed view stays in the own set: %s", left)
	}
}

// Every path that closes a view row deletes it from the own set: the close of its session, a
// close that only one caller wins, and Ctrl+Q in the view (viewClose).
func TestUnseenOwnForgotten(t *testing.T) {
	ctx := context.Background()
	h, _ := unseenHub(t, true, "v1", "v2")
	h.forgetSession(ctx, "s", "test")
	if h.ownView("v1") || h.ownView("v2") {
		t.Fatalf("a session close keeps own views: %v", h.ui.own)
	}

	h, _ = unseenHub(t, true, "v1")
	if won, err := h.closeViewOnce(ctx, "v1", "hub", false); !won || err != nil {
		t.Fatalf("closeViewOnce: %v %v", won, err)
	}
	if h.ownView("v1") {
		t.Fatal("closeViewOnce keeps the own view")
	}

	h, _, _ = reopenTestHub(t)
	h.markOwn("old")
	var p perform
	p.Requester.ClientID = "c"
	if _, err := h.viewClose(ctx, p, map[string]any{"view_id": "old", "attachment": "att"}); err != nil {
		t.Fatal(err)
	}
	if h.ownView("old") {
		t.Fatal("viewClose keeps the own view")
	}
}

// The pass reads its rows (the view open, the tab not suppressed), then the exit event counts the
// third crash. The pass's unseenExit counts nothing more, and its stale tab opens no view: the
// insert reads the suppression (codex-25's order).
func TestUnseenEventWinsAtLimit(t *testing.T) {
	ctx := context.Background()
	h, _ := unseenHub(t, true, "v1")
	mustExec(t, h, `UPDATE ui_tabs SET crashes = ?`, maxCrashes-1)
	v := unseenView(t, h, "v1")
	stale := tab{Key: "main", Title: "Main", Route: "/main"}
	h.uiChildExited(ctx, "b-v1")
	h.unseenExit(ctx, v)
	if c, s := tabCrashes(h); c != maxCrashes || s != 1 {
		t.Fatalf("crashes %d, suppressed %d; want %d and 1", c, s, maxCrashes)
	}
	nv, err := h.spawnView(ctx, "p", "s", stale)
	var n int
	h.db.QueryRow(`SELECT count(*) FROM views WHERE plugin = 'p' AND tab = 'main' AND status != 'closed'`).Scan(&n)
	if nv != nil || err != nil || n != 0 {
		t.Fatalf("a suppressed tab got a view: %v %v, %d rows not closed", nv, err, n)
	}
}

// A view in a session that is not its tab's target (a home that the tab left) closes its pane
// first, then its row (closeMoved). When block.close fails, the row stays open, with no
// view_closed, so the pass opens no second view; a later close succeeds (codex-25's review of the
// view.retire removal).
func TestMovedViewClosesItsPaneFirst(t *testing.T) {
	ctx := context.Background()
	h, x, fr := reopenTestHub(t)
	v := &view{ID: "old", Plugin: "p", Tab: "main", Session: "other", Block: "b-old"}
	fail := true
	fr.mu.Lock()
	base := fr.other
	fr.other = func(m string, p json.RawMessage) (any, string) {
		if m == "block.close" && fail {
			return nil, "internal"
		}
		return base(m, p)
	}
	fr.mu.Unlock()
	if h.closeMoved(ctx, v) {
		t.Fatal("closeMoved reported a close that failed")
	}
	if s, _, _ := viewStatus(h, "old"); s != "open" || len(loggedEvents(t, h, "view_closed")) != 0 {
		t.Fatalf("a failed pane close: the row is %s, events %v; want open and no view_closed", s, loggedEvents(t, h, "view_closed"))
	}
	fr.mu.Lock()
	fail = false
	fr.mu.Unlock()
	if !h.closeMoved(ctx, v) {
		t.Fatal("closeMoved failed after the pane close succeeded")
	}
	e := loggedEvents(t, h, "view_closed")
	if s, by, _ := viewStatus(h, "old"); s != "closed" || by != "hub" || len(e) != 1 || e[0]["why"] != "moved" || len(x.closed) != 1 {
		t.Fatalf("after the close: the row is %s by %s, events %v, closed panes %v", s, by, e, x.closed)
	}
}

// A failed close of a moved view's pane blocks its tab in every group of the pass: the tab's
// target session is another group than the old view's (codex-25's probe).
func TestMovedViewBlocksItsTargetGroup(t *testing.T) {
	h, x, fr := reopenTestHub(t)
	h.ui.labeled = map[string]string{}
	mustExec(t, h, homeSchema)
	mustExec(t, h, `INSERT INTO ui_sessions (plugin, session_id) VALUES (?, 's')`, uiSessionKey)
	mustExec(t, h, `UPDATE views SET session_id = 'former-home', status = 'exited', attachment = '', client_id = '' WHERE view_id = 'old'`)
	mustExec(t, h, `UPDATE ui_tabs SET session_id = 'session:01a15601-5fca-744d-9590-fe16fd20a936' WHERE plugin = 'p' AND tab = 'main'`)
	fr.mu.Lock()
	fr.other = func(m string, p json.RawMessage) (any, string) {
		switch m {
		case "block.close":
			return nil, "internal"
		case "session.new_window":
			return map[string]any{"window_id": "w-new", "block_ids": []string{"b-new"}}, ""
		}
		return x.answer(m, p)
	}
	fr.mu.Unlock()
	if _, err := h.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.db.QueryRow(`SELECT count(*) FROM views WHERE plugin = 'p' AND tab = 'main' AND view_id != 'old' AND status != 'closed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a failed close of the old pane opened %d new views in the target group", n)
	}
	if s, _, _ := viewStatus(h, "old"); s == "closed" {
		t.Fatal("a failed pane close closed the old row")
	}
}
