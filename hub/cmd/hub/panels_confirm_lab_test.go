package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// The confirm step of a side-slot replacement, against a real Rex lab (rexLab: HARNESS_LAB=1):
// cases B, C, E and F of the former tests/panels-lab.mjs (its case A is
// TestSlotReturnOccupiedAndNoFocusLab). The old panel's renderer is a lab client that answers
// view.release with confirm; the user's choice is the call that the real renderer sends,
// hub.view.release_confirm, from that client with its attachment. The renderer's own side (the
// prompt, Keep editing keeps the input, a restarted renderer prompts again) is in view/panels_test.go
// and view/panels_recovery_test.go. The tests check the hub's rows and the panes that Rex reports.

// confirmLabHub is panelsLabHub with an inert view worker for the plugin p: a kick is recorded,
// and no worker runs (the lab DB has no view_ops schema, and nothing stops a worker before the DB
// closes). The cleanup checks that no other worker started.
func confirmLabHub(t *testing.T) (*Hub, *rex.Rex, string, string) {
	t.Helper()
	h, r, sess, win := panelsLabHub(t)
	inertViewWorkers(t, h)
	return h, r, sess, win
}

func inertViewWorkers(t *testing.T, h *Hub) {
	t.Helper()
	h.vs.workers["p"] = make(chan struct{}, 1)
	t.Cleanup(func() {
		h.vs.mu.Lock()
		defer h.vs.mu.Unlock()
		if len(h.vs.workers) != 1 {
			t.Errorf("view workers %v, want only the inert p channel", h.vs.workers)
		}
	})
}

// choose sends the user's choice as the renderer client with the attachment att.
func choose(h *Hub, client, view, att, repl, choice string) (map[string]any, error) {
	var p perform
	p.Requester.ClientID = client
	out, err := h.releaseConfirm(context.Background(), p, map[string]any{"view_id": view, "attachment": att, "replacement_id": repl, "choice": choice})
	m, _ := out.(map[string]any)
	return m, err
}

func answerState(out map[string]any) string {
	r, _ := out["replacement"].(map[string]any)
	s, _ := r["state"].(string)
	return s
}

func slotViewRow(t *testing.T, h *Hub, id string) (status, block string) {
	t.Helper()
	if err := h.db.QueryRow(`SELECT status, block_id FROM views WHERE view_id = ?`, id).Scan(&status, &block); err != nil {
		t.Fatalf("view %s: %v", id, err)
	}
	return status, block
}

func replKept(t *testing.T, h *Hub, repl string) bool {
	t.Helper()
	var k bool
	if err := h.db.QueryRow(`SELECT kept FROM ui_replacements WHERE repl_id = ?`, repl).Scan(&k); err != nil {
		t.Fatal(err)
	}
	return k
}

// askAndConfirm replaces the slot's view with repl; the renderer answers confirm, so the row waits
// for the user.
func askAndConfirm(t *testing.T, h *Hub, lr *labRenderer, sess, win, repl string) {
	t.Helper()
	out, err := h.openSlot(context.Background(), slotRequest{ReplID: repl, Plugin: "p", Route: "/panels/b", Ctx: panel(sess, win)})
	if err != nil || out["status"] != "pending_confirmation" {
		t.Fatalf("%s: %v %v", repl, out, err)
	}
	if p := lr.next(t); p.Action != "view.release" {
		t.Fatalf("the renderer got %s, want view.release", p.Action)
	}
	if st := replState(t, h, repl); st != "confirming" {
		t.Fatalf("%s: state %s, want confirming", repl, st)
	}
}

// B: Keep editing keeps the old pane and records kept; a later Discard replaces it.
func TestSlotConfirmKeepThenDiscardLab(t *testing.T) {
	h, r, sess, win := confirmLabHub(t)
	lr := newLabRenderer(t, h, "confirm")
	old := openFirstPanel(t, h, sess, win)
	attachedTo(t, h, old, lr)
	_, oldBlock := slotViewRow(t, h, old)

	askAndConfirm(t, h, lr, sess, win, "repl-keep")
	out, err := choose(h, lr.id, old, "att-lab", "repl-keep", "keep")
	if err != nil || answerState(out) != "cancelled" {
		t.Fatalf("keep: %v %v", out, err)
	}
	if st := replState(t, h, "repl-keep"); st != "cancelled" || !replKept(t, h, "repl-keep") {
		t.Fatalf("after keep: state %s, kept %v", st, replKept(t, h, "repl-keep"))
	}
	if st, _ := slotViewRow(t, h, old); st != "open" {
		t.Fatalf("after keep: the old view is %s", st)
	}
	if panes := formPanes(t, r, sess); len(panes) != 1 || panes[old] != oldBlock {
		t.Fatalf("after keep: panes %v, want only the old pane %s", panes, oldBlock)
	}

	askAndConfirm(t, h, lr, sess, win, "repl-discard")
	if out, err := choose(h, lr.id, old, "att-lab", "repl-discard", "discard"); err != nil {
		t.Fatalf("discard: %v %v", out, err)
	}
	if st := replState(t, h, "repl-discard"); st != "done" {
		t.Fatalf("after discard: state %s, want done", st)
	}
	if st, _ := slotViewRow(t, h, old); st != "closed" {
		t.Fatalf("after discard: the old view is %s", st)
	}
	panes := formPanes(t, r, sess)
	if len(panes) != 1 || panes[slotViewID("repl-discard")] == "" {
		t.Fatalf("after discard: panes %v, want only the new view's", panes)
	}
}

// E: the user closes the old pane while it asks. The row ends user_closed; a late Discard is
// refused, and no new pane is ever made.
func TestSlotConfirmUserCloseLab(t *testing.T) {
	h, r, sess, win := confirmLabHub(t)
	lr := newLabRenderer(t, h, "confirm")
	old := openFirstPanel(t, h, sess, win)
	attachedTo(t, h, old, lr)
	_, oldBlock := slotViewRow(t, h, old)
	askAndConfirm(t, h, lr, sess, win, "repl-close")

	ctx := context.Background()
	if err := r.Call(ctx, "block.close", map[string]any{"session_id": sess, "block_id": oldBlock}, nil); err != nil {
		t.Fatal(err)
	}
	h.uiBlockClosed(ctx, oldBlock, false) // the hub's handler of that Rex event (panelsLabHub drains the events)
	if st := replState(t, h, "repl-close"); st != "user_closed" {
		t.Fatalf("after the user close: state %s, want user_closed", st)
	}
	if _, err := choose(h, lr.id, old, "att-lab", "repl-close", "discard"); err == nil || !strings.Contains(err.Error(), "stale_view") {
		t.Fatalf("a late discard: %v, want stale_view", err)
	}
	h.slotSweep(ctx)
	if st := replState(t, h, "repl-close"); st != "user_closed" {
		t.Fatalf("after the sweep: state %s, want user_closed", st)
	}
	if panes := formPanes(t, r, sess); len(panes) != 0 {
		t.Fatalf("panes %v, want none", panes)
	}
}

// C: the hub restarts while the renderer asks. A fresh Hub (a new DB connection to the same
// database, a new Rex connection, the normal start: uiStart and viewsStart) keeps the row
// confirming. The old attachment is stale; the renderer attaches again (viewAttach), and the attach
// reply names the replacement as confirming; Discard with the new attachment completes it once.
// The old Hub is retired first: its Rex connection and DB connection close. The renderer's own
// side (the prompt comes back, Discard goes out with the new attachment) is
// TestRestartedRendererPromptsAgainAndDiscards in view/panels_recovery_test.go.
func TestSlotConfirmSurvivesHubRestartLab(t *testing.T) {
	h, r, sess, win := confirmLabHub(t)
	lr := newLabRenderer(t, h, "confirm")
	old := openFirstPanel(t, h, sess, win)
	attachedTo(t, h, old, lr)
	askAndConfirm(t, h, lr, sess, win, "repl-c")
	var pid int
	h.db.QueryRow(`SELECT child_pid FROM views WHERE view_id = ?`, old).Scan(&pid)

	// Retire the old Hub.
	var seq int
	var name, path string
	if err := h.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil || path == "" {
		t.Fatalf("the hub db path: %q %v", path, err)
	}
	r.Close()
	h.db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ev, err := openEvents(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ev.f.Close() })
	socket, err := rex.SocketPath(h.cfg.Server)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := rex.Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r2.Close() })
	go func() {
		for range r2.Events {
		}
	}()
	h2 := &Hub{db: db, ev: ev, agents: map[string]*Agent{}, cfg: h.cfg, rex: r2, selfClosed: map[string]bool{}, vs: newViewSvc()}
	if err := h2.uiStart(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h2.viewsStart(ctx); err != nil { // the start makes a new view service
		t.Fatal(err)
	}
	inertViewWorkers(t, h2)

	if st := replState(t, h2, "repl-c"); st != "confirming" {
		t.Fatalf("after the restart: state %s, want confirming", st)
	}
	var attachment string
	h2.db.QueryRow(`SELECT attachment FROM views WHERE view_id = ?`, old).Scan(&attachment)
	if attachment != "" {
		t.Fatalf("after the restart: attachment %q, want none", attachment)
	}
	if _, err := choose(h2, lr.id, old, "att-lab", "repl-c", "discard"); err == nil || !strings.Contains(err.Error(), "stale_attachment") {
		t.Fatalf("a choice with the old attachment: %v, want stale_attachment", err)
	}
	if st := replState(t, h2, "repl-c"); st != "confirming" {
		t.Fatalf("after the stale choice: state %s, want confirming", st)
	}

	var p perform
	p.Requester.ClientID, p.Requester.Principal.PID = lr.id, pid
	res, err := h2.viewAttach(ctx, p, map[string]any{"view_id": old, "schema_version": float64(1)})
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	attached, _ := res.(map[string]any)
	repl, _ := attached["replacement"].(map[string]any)
	att, _ := attached["attachment"].(string)
	if repl["replacement_id"] != "repl-c" || repl["state"] != "confirming" || att == "" || att == "att-lab" {
		t.Fatalf("the attach reply: %v", attached)
	}

	if out, err := choose(h2, lr.id, old, att, "repl-c", "discard"); err != nil {
		t.Fatalf("discard: %v %v", out, err)
	}
	if st := replState(t, h2, "repl-c"); st != "done" {
		t.Fatalf("after discard: state %s, want done", st)
	}
	want := slotViewID("repl-c")
	panes := formPanes(t, r2, sess)
	if len(panes) != 1 || panes[want] == "" {
		t.Fatalf("after discard: panes %v, want only %s", panes, want)
	}
	// The choice completes once: a repeat (a lost answer) answers the row's state and opens nothing.
	if _, err := choose(h2, lr.id, old, att, "repl-c", "discard"); err == nil || !strings.Contains(err.Error(), "stale_view") {
		t.Fatalf("a repeated discard: %v, want stale_view (the old view is closed)", err)
	}
	if again := formPanes(t, r2, sess); len(again) != 1 || again[want] != panes[want] {
		t.Fatalf("after a repeat: panes %v, want %v", again, panes)
	}
}

// F: a form that replaced a panel is submitted while it holds unsaved input: its return asks.
// Keep editing keeps the form; a replay of that submit makes no new row; a new submit (a new
// operation key) gets a new return row, and the panel comes back. IDs come from the rows.
func TestSlotFormKeepThenNewSubmitLab(t *testing.T) {
	h, r, sess, win := confirmLabHub(t)
	ctx := context.Background()
	lr := newLabRenderer(t, h, "confirm")
	form := openFirstPanel(t, h, sess, win) // the view in the slot plays the form
	attachedTo(t, h, form, lr)
	fc := formContext{SessionID: sess, WindowID: win, Return: &slotReturn{Plugin: "p", Route: "/panels/ret", Context: panel(sess, win)}}
	rows := func() []string {
		t.Helper()
		q, err := h.db.Query(`SELECT repl_id FROM ui_replacements WHERE old_view = ? ORDER BY rowid`, form)
		if err != nil {
			t.Fatal(err)
		}
		defer q.Close()
		var ids []string
		for q.Next() {
			var id string
			q.Scan(&id)
			ids = append(ids, id)
		}
		return ids
	}

	if owned, err := h.formEnded(ctx, form, fc, true, "op-1"); !owned || err != nil {
		t.Fatalf("the first submit: %v %v", owned, err)
	}
	if p := lr.next(t); p.Action != "view.release" {
		t.Fatalf("the form renderer got %s", p.Action)
	}
	ids := rows()
	if len(ids) != 1 || replState(t, h, ids[0]) != "confirming" {
		t.Fatalf("the first return: rows %v", ids)
	}
	first := ids[0]
	if out, err := choose(h, lr.id, form, "att-lab", first, "keep"); err != nil || answerState(out) != "cancelled" {
		t.Fatalf("keep: %v %v", out, err)
	}
	if replState(t, h, first) != "cancelled" || !replKept(t, h, first) {
		t.Fatalf("after keep: state %s", replState(t, h, first))
	}
	if st, _ := slotViewRow(t, h, form); st != "open" {
		t.Fatalf("after keep: the form is %s", st)
	}

	// A replay of the kept submit: the action is complete, and nothing new is made.
	panes := formPanes(t, r, sess)
	if owned, err := h.formEnded(ctx, form, fc, true, "op-1"); !owned || err != nil {
		t.Fatalf("the replay: %v %v", owned, err)
	}
	if got := rows(); len(got) != 1 || got[0] != first || replState(t, h, first) != "cancelled" {
		t.Fatalf("after the replay: rows %v, state %s", got, replState(t, h, first))
	}
	if got := formPanes(t, r, sess); len(got) != len(panes) || got[form] != panes[form] {
		t.Fatalf("after the replay: panes %v, want %v", got, panes)
	}

	// The user restores the input and submits again: the renderer releases now.
	lr2 := newLabRenderer(t, h, "released")
	attachedTo(t, h, form, lr2)
	if owned, err := h.formEnded(ctx, form, fc, true, "op-2"); !owned || err != nil {
		t.Fatalf("the second submit: %v %v", owned, err)
	}
	ids = rows()
	if len(ids) != 2 || ids[0] != first || ids[1] == first {
		t.Fatalf("the second return: rows %v, want a second, distinct row", ids)
	}
	second := ids[1]
	if st := replState(t, h, second); st != "done" {
		t.Fatalf("the second return: state %s, want done", st)
	}
	if st, _ := slotViewRow(t, h, form); st != "closed" {
		t.Fatalf("after the second return: the form is %s", st)
	}
	var newView, route string
	if err := h.db.QueryRow(`SELECT new_view FROM ui_replacements WHERE repl_id = ?`, second).Scan(&newView); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT route FROM views WHERE view_id = ?`, newView).Scan(&route); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	got := formPanes(t, r, sess)
	if route != "/panels/ret" || len(got) != 1 || got[newView] == "" {
		t.Fatalf("the return panel: route %q, panes %v, want only %s", route, got, newView)
	}
}
