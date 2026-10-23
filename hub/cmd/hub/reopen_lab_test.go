package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// A reopen against a real Rex lab (rexLab, as the side-slot lab tests): the old tab
// view's pane is the session's first pane, attached to a lab renderer that releases. The new pane
// takes the old pane's place, and the old pane closes. With a lost split answer, the sweep finds the
// pane by its label, and Rex gets one split only.
func TestReopenTabPaneLab(t *testing.T) {
	for _, lost := range []bool{false, true} {
		h, r, sess, win := panelsLabHub(t)
		ctx := context.Background()
		h.ui.sess = map[string]string{sess: uiSessionKey}
		lr := newLabRenderer(t, h, "released")
		blocks, err := h.blocksIn(ctx, sess)
		if err != nil || len(blocks) != 1 {
			t.Fatalf("lab panes: %v %v", blocks, err)
		}
		var oldBlock string
		for b := range blocks {
			oldBlock = b
		}
		mustExec(t, h, `INSERT INTO ui_tabs (plugin, tab, title, route, pos) VALUES ('p', 'main', 'Main', '/main', 0)`)
		mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, session_id, window_id, block_id, attach_state)
			VALUES ('old', 'p', 'main', '/main', 'open', ?, ?, ?, 'never')`, sess, win, oldBlock)
		attachedTo(t, h, "old", lr)
		fc, _ := json.Marshal(formContext{SessionID: sess, WindowID: win, SourceViewID: "old", SourceRoute: "/main"})
		mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, context, session_id, attach_state) VALUES ('form', 'p', ?, '/forms/a', 'open', ?, ?, 'attached')`, formTab, string(fc), sess)

		splits := 0
		orig := slotSplit
		slotSplit = func(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
			splits++
			return orig(h, ctx, method, args, out)
		}
		if lost {
			loseSplitAnswer(t) // wraps the counter: the split reaches Rex, its answer is lost
		}
		done, err := h.reopenStep(ctx, "p", "k", "form", fullOpen())
		if p := lr.next(t); p.Action != "view.release" {
			t.Fatalf("lost %v: the old renderer got %s", lost, p.Action)
		}
		id := reopenID("p", "k")
		nv := reopenViewID(id)
		if lost {
			if done || err != nil || reopenState(h, id) != "opening" {
				t.Fatalf("a lost answer: %v %v %s", done, err, reopenState(h, id))
			}
			h.slotSweep(ctx)
			done, err = h.reopenStep(ctx, "p", "k", "form", fullOpen())
		}
		slotSplit = orig
		if !done || err != nil || reopenState(h, id) != "done" {
			t.Fatalf("lost %v: %v %v, row %s", lost, done, err, reopenState(h, id))
		}
		if splits != 1 {
			t.Fatalf("lost %v: %d splits", lost, splits)
		}
		blocks, err = h.blocksIn(ctx, sess)
		if err != nil {
			t.Fatal(err)
		}
		var labels []string
		for _, b := range blocks {
			labels = append(labels, b.Label)
		}
		if len(blocks) != 1 || labels[0] != "lazyrex-view:"+nv {
			t.Fatalf("lost %v: panes %v, want only the new view's pane", lost, labels)
		}
		if _, ok := blocks[oldBlock]; ok {
			t.Fatalf("lost %v: the old pane stays", lost)
		}
		st, _, open := viewStatus(h, nv)
		var block string
		h.db.QueryRow(`SELECT block_id FROM views WHERE view_id = ?`, nv).Scan(&block)
		if st != "open" || open != `{"full":true}` || blocks[block].Label == "" {
			t.Fatalf("lost %v: the new view %s %q block %s", lost, st, open, block)
		}
		if st, by, _ := viewStatus(h, "old"); st != "closed" || by != "hub" {
			t.Fatalf("lost %v: the old view %s %s", lost, st, by)
		}
		if !strings.HasPrefix(nv, "view-") {
			t.Fatalf("new view ID %s", nv)
		}
		r.Call(ctx, "session.destroy", map[string]any{"session_id": sess}, nil)
	}
}

// The reconcile pass leaves a reopening tab to its row (PAIR_LAB_SOCKET, real reconcile passes):
// the pane request is lost before Rex (no pane), and the old renderer exits while the row is
// opening. The passes open no third view and close neither view; the exit counts once. After the
// row expires, the next passes replace the exited view: the tab is back with one open view.
func TestReopenReservationLab(t *testing.T) {
	h := pairLabHub(t)
	ctx := context.Background()
	declarePair(t, h, "")
	passes(t, h, 2)
	old := onlyView(t, h)
	if old.Block == "" || old.Session == "" {
		t.Fatalf("the tab view: %+v", old)
	}
	fc, _ := json.Marshal(formContext{SessionID: old.Session, WindowID: old.Window, SourceViewID: old.ID, SourceRoute: old.Route})
	mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, context, session_id, attach_state) VALUES ('form', 'messaging', ?, '/forms/a', 'open', ?, ?, 'attached')`, formTab, string(fc), old.Session)
	orig := slotSplit
	slotSplit = func(*Hub, context.Context, string, map[string]any, any) error {
		return &rex.RexError{Code: "disconnected", Message: "lab: the request is lost before Rex"}
	}
	t.Cleanup(func() { slotSplit = orig })
	done, err := h.reopenStep(ctx, "messaging", "k", "form", fullOpen())
	slotSplit = orig
	id := reopenID("messaging", "k")
	if done || err != nil || reopenState(h, id) != "opening" {
		t.Fatalf("reopen: %v %v, row %s", done, err, reopenState(h, id))
	}
	h.uiChildExited(ctx, old.Block)
	passes(t, h, 3)
	var crashes, live int
	h.db.QueryRow(`SELECT crashes FROM ui_tabs WHERE plugin = 'messaging' AND tab = 'thread/a'`).Scan(&crashes)
	h.db.QueryRow(`SELECT COUNT(*) FROM views WHERE plugin = 'messaging' AND tab = 'thread/a' AND status != 'closed'`).Scan(&live)
	st, _, _ := viewStatus(h, old.ID)
	nv, _, _ := viewStatus(h, reopenViewID(id))
	if live != 2 || st != "exited" || nv != "sending" || crashes != 1 {
		t.Fatalf("during the row: %d live views, old %s, new %s, crashes %d", live, st, nv, crashes)
	}
	blocks, err := h.blocksIn(ctx, old.Session)
	if _, ok := blocks[old.Block]; err != nil || !ok {
		t.Fatalf("the old pane was closed during the row: %v", err)
	}
	mustExec(t, h, `UPDATE ui_replacements SET split_at = ? WHERE repl_id = ?`, time.Now().Add(-reopenBound-time.Second).UTC().Format(time.RFC3339Nano), id)
	h.slotSweep(ctx)
	if s := reopenState(h, id); s != "expired" {
		t.Fatalf("row %s", s)
	}
	passes(t, h, 3)
	v := onlyView(t, h)
	if v.ID == old.ID || v.ID == reopenViewID(id) {
		t.Fatalf("the tab's view after the row: %s", v.ID)
	}
	h.db.QueryRow(`SELECT crashes FROM ui_tabs WHERE plugin = 'messaging' AND tab = 'thread/a'`).Scan(&crashes)
	if crashes != 1 {
		t.Fatalf("crashes %d", crashes)
	}
}

// Lead ruling lead-ux-fix1 (2), in a real Rex lab (PAIR_LAB_SOCKET): the split answer is lost, and
// the new renderer exits at once in its kept-open pane. At the bound the childless labeled pane is
// adopted, one renderer exit is counted, the old pane closes, and the reconcile passes open a new
// view for the tab.
func TestReopenChildlessPaneLab(t *testing.T) {
	h := pairLabHub(t)
	ctx := context.Background()
	declarePair(t, h, "")
	passes(t, h, 2)
	old := onlyView(t, h)
	fc, _ := json.Marshal(formContext{SessionID: old.Session, WindowID: old.Window, SourceViewID: old.ID, SourceRoute: old.Route})
	mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, context, session_id, attach_state) VALUES ('form', 'messaging', ?, '/forms/a', 'open', ?, ?, 'attached')`, formTab, string(fc), old.Session)
	orig := slotSplit
	slotSplit = func(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
		opts := args["layout"].(map[string]any)["block"].(map[string]any)["options"].(map[string]any)
		opts["command"] = []string{"/bin/sh", "-c", "exit 0"} // the renderer exits at once
		orig(h, ctx, method, args, out)
		return &rex.RexError{Code: "disconnected", Message: "lab: the answer is lost"}
	}
	t.Cleanup(func() { slotSplit = orig })
	h.reopenStep(ctx, "messaging", "k", "form", fullOpen())
	slotSplit = orig
	id := reopenID("messaging", "k")
	nv := reopenViewID(id)
	if s := reopenState(h, id); s != "opening" {
		t.Fatalf("row %s", s)
	}
	mustExec(t, h, `UPDATE ui_replacements SET split_at = ? WHERE repl_id = ?`, time.Now().Add(-reopenBound-time.Second).UTC().Format(time.RFC3339Nano), id)
	waitUntilLab(t, "the new renderer exits", func() bool {
		blocks, _ := h.blocksIn(ctx, old.Session)
		for b, info := range blocks {
			if info.Label == "lazyrex-view:"+nv {
				pid, err := h.childIn(ctx, old.Session, b)
				return err == nil && pid == 0
			}
		}
		return false
	})
	h.slotSweep(ctx)
	var crashes int
	h.db.QueryRow(`SELECT crashes FROM ui_tabs WHERE plugin = 'messaging' AND tab = 'thread/a'`).Scan(&crashes)
	st, _, _ := viewStatus(h, nv)
	if s := reopenState(h, id); s != "done" || crashes != 1 || st != "exited" {
		t.Fatalf("row %s, crashes %d, new view %s", s, crashes, st)
	}
	if st, by, _ := viewStatus(h, old.ID); st != "closed" || by != "hub" {
		t.Fatalf("the old view: %s %s", st, by)
	}
	passes(t, h, 3)
	v := onlyView(t, h)
	if v.ID == old.ID || v.ID == nv {
		t.Fatalf("the tab's view: %s", v.ID)
	}
	h.db.QueryRow(`SELECT crashes FROM ui_tabs WHERE plugin = 'messaging' AND tab = 'thread/a'`).Scan(&crashes)
	if crashes != 1 {
		t.Fatalf("crashes %d", crashes)
	}
}

func waitUntilLab(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
