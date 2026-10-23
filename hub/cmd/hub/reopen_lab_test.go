package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A reopen against a real Rex lab (PANELS_LAB_SOCKET, as the side-slot lab tests): the old tab
// view's pane is the session's first pane, attached to a lab renderer that releases. The new pane
// takes the old pane's place, and the old pane closes. With a lost split answer, the sweep finds the
// pane by its label, and Rex gets one split only.
func TestReopenTabPaneLab(t *testing.T) {
	for _, lost := range []bool{false, true} {
		h, r, sess, win := panelsLabHub(t)
		ctx := context.Background()
		h.ui.sess = map[string]string{sess: uiSessionKey}
		lr := newLabRenderer(t, "released")
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
