package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// Side-slot recovery against a real Rex lab, with the new_split answer lost on demand (slotSplit).
// Lab only (rexLab: HARNESS_LAB=1). The "renderer" is a script that sleeps, so
// no plugin or view runs; the tests check the hub's rows and the panes that Rex reports.
func panelsLabHub(t *testing.T) (*Hub, *rex.Rex, string, string) {
	t.Helper()
	l := rexLab(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) // first, so it runs after every cleanup that uses r
	go func() {
		for range r.Events {
		}
	}()
	h, _ := workerTestHub(t)
	h.rex, h.cfg.Server = r, l.server
	h.selfClosed = map[string]bool{}
	h.vs = newViewSvc() // closeViewRow forgets the view's render state
	view := filepath.Join(t.TempDir(), "view.sh")
	os.WriteFile(view, []byte("#!/bin/sh\nwhile :; do sleep 600; done\n"), 0o755) // no exec: the shell keeps the argv with the view ID (the hub's proof)
	h.cfg.View = view
	if err := h.uiStart(ctx); err != nil {
		t.Fatal(err)
	}
	var s struct {
		ID string `json:"session_id"`
	}
	if err := r.Call(ctx, "session.create", map[string]any{"label": "panels-lab-test", "initial_windows": []any{map[string]any{"window_label": "w",
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"command": []string{"/bin/sleep", "600"}, "login": false}}}}}}, &s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Call(context.Background(), "session.destroy", map[string]any{"session_id": s.ID}, nil) })
	r.Call(ctx, "session.attach", map[string]any{"session_id": s.ID, "event_scope": "session"}, nil)
	var sv struct {
		Windows []struct {
			WindowID string `json:"window_id"`
		} `json:"windows"`
	}
	if err := r.Call(ctx, "session.view", map[string]any{"session_id": s.ID}, &sv); err != nil || len(sv.Windows) != 1 {
		t.Fatalf("lab session view: %v", err)
	}
	return h, r, s.ID, sv.Windows[0].WindowID
}

// loseSplitAnswer makes the next new_split reach Rex, but drops its answer (an unknown outcome).
func loseSplitAnswer(t *testing.T) {
	orig := slotSplit
	slotSplit = func(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
		slotSplit = orig
		orig(h, ctx, method, args, out)
		return &rex.RexError{Code: "disconnected", Message: "lab: the answer is lost"}
	}
	t.Cleanup(func() { slotSplit = orig })
}

func formPanes(t *testing.T, r *rex.Rex, sess string) map[string]string {
	t.Helper()
	h := &Hub{rex: r}
	blocks, err := h.blocksIn(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, b := range blocks {
		if strings.HasPrefix(b.Label, "lazyrex-form:") {
			out[strings.TrimPrefix(b.Label, "lazyrex-form:")] = b.BlockID
		}
	}
	return out
}

func replState(t *testing.T, h *Hub, id string) string {
	var st string
	if err := h.db.QueryRow(`SELECT state FROM ui_replacements WHERE repl_id = ?`, id).Scan(&st); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return st
}

func panel(sess, window string) formContext {
	return formContext{SessionID: sess, WindowID: window, Panel: true}
}

// An unknown new_split outcome leaves the row opening; the sweep finds the pane by its label and
// adopts it. The hub never sends a second new_split.
func TestSlotUnknownSplitLateAdoptionLab(t *testing.T) {
	h, r, sess, win := panelsLabHub(t)
	ctx := context.Background()
	loseSplitAnswer(t)
	out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-unknown", Plugin: "p", Route: "/panels/a", Ctx: panel(sess, win)})
	if err != nil || out["status"] != "opening" {
		t.Fatalf("lost answer: %v %v", out, err)
	}
	if st := replState(t, h, "repl-unknown"); st != "opening" {
		t.Fatalf("state %s", st)
	}
	h.slotSweep(ctx)
	if st := replState(t, h, "repl-unknown"); st != "done" {
		t.Fatalf("after the sweep: %s", st)
	}
	panes := formPanes(t, r, sess)
	if len(panes) != 1 || panes[slotViewID("repl-unknown")] == "" {
		t.Fatalf("one adopted pane: %v", panes)
	}
	var status, block string
	h.db.QueryRow(`SELECT status, block_id FROM views WHERE view_id = ?`, slotViewID("repl-unknown")).Scan(&status, &block)
	if status != "open" || block != panes[slotViewID("repl-unknown")] {
		t.Fatalf("view row: %s %s", status, block)
	}
}

// A newer open supersedes an unresolved row. The late pane of the old row is closed, never kept.
func TestSlotSupersededLateCleanupLab(t *testing.T) {
	h, r, sess, win := panelsLabHub(t)
	ctx := context.Background()
	loseSplitAnswer(t)
	if out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-old", Plugin: "p", Route: "/panels/a", Ctx: panel(sess, win)}); err != nil || out["status"] != "opening" {
		t.Fatalf("lost answer: %v %v", out, err)
	}
	out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-new", Plugin: "p", Route: "/panels/b", Ctx: panel(sess, win)})
	if err != nil || out["status"] != "opened" {
		t.Fatalf("newer open: %v %v", out, err)
	}
	if st := replState(t, h, "repl-old"); st != "superseded" {
		t.Fatalf("old row: %s", st)
	}
	h.slotSweep(ctx)
	if st := replState(t, h, "repl-old"); st != "late_closed" {
		t.Fatalf("old row after the sweep: %s", st)
	}
	time.Sleep(300 * time.Millisecond)
	panes := formPanes(t, r, sess)
	if len(panes) != 1 || panes[slotViewID("repl-new")] == "" {
		t.Fatalf("only the newer pane stays: %v", panes)
	}
}

// The user closes the old view while the new_split answer is lost: the row is user_closed, and
// the late pane is closed. (The old view here is a row only; the renderer side has view tests.)
func TestSlotUserCloseAfterUnansweredSplitLab(t *testing.T) {
	h, r, sess, win := panelsLabHub(t)
	ctx := context.Background()
	loseSplitAnswer(t)
	if out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-uc", Plugin: "p", Route: "/panels/a", Ctx: panel(sess, win)}); err != nil || out["status"] != "opening" {
		t.Fatalf("lost answer: %v %v", out, err)
	}
	h.db.Exec(`UPDATE ui_replacements SET old_view = 'view-old' WHERE repl_id = 'repl-uc'`)
	h.slotOldClosed(ctx, "view-old")
	if st := replState(t, h, "repl-uc"); st != "user_closed" {
		t.Fatalf("state: %s", st)
	}
	h.slotSweep(ctx)
	if st := replState(t, h, "repl-uc"); st != "late_closed" {
		t.Fatalf("after the sweep: %s", st)
	}
	time.Sleep(300 * time.Millisecond)
	if panes := formPanes(t, r, sess); len(panes) != 0 {
		t.Fatalf("the late pane is closed: %v", panes)
	}
}

// A closed form's return opens only into an empty slot, and never takes the focus.
func TestSlotReturnOccupiedAndNoFocusLab(t *testing.T) {
	h, r, sess, win := panelsLabHub(t)
	ctx := context.Background()
	if out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-user", Plugin: "p", Route: "/panels/user", Ctx: panel(sess, win)}); err != nil || out["status"] != "opened" {
		t.Fatalf("user panel: %v %v", out, err)
	}
	ret := &slotReturn{Plugin: "p", Route: "/panels/ret", Context: panel(sess, win)}
	closed := formContext{SessionID: sess, WindowID: win, Return: ret}
	if took, err := h.formEnded(ctx, "view-closed-1", closed, false, ""); took || err != nil {
		t.Fatalf("closed form: %v %v", took, err)
	}
	if st := replState(t, h, "return-view-closed-1"); st != "" {
		t.Fatalf("an occupied slot admits no return row: %s", st)
	}
	if panes := formPanes(t, r, sess); len(panes) != 1 {
		t.Fatalf("the user's panel stays alone: %v", panes)
	}
	if err := h.closeForm(ctx, slotViewID("repl-user"), "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.formEnded(ctx, "view-closed-2", closed, false, ""); err != nil {
		t.Fatal(err)
	}
	var st string
	var focus bool
	h.db.QueryRow(`SELECT state, focus FROM ui_replacements WHERE repl_id = 'return-view-closed-2'`).Scan(&st, &focus)
	if st != "done" || focus {
		t.Fatalf("return into the empty slot: state %s, focus %v", st, focus)
	}
}

// floatingLayers gives the block IDs of each floating layer of the session's windows.
func floatingLayers(t *testing.T, r *rex.Rex, sess string) [][]string {
	t.Helper()
	var sv struct {
		Windows []struct {
			Layers []struct {
				Kind   string     `json:"kind"`
				Layout layoutNode `json:"layout"`
			} `json:"layers"`
		} `json:"windows"`
	}
	if err := r.Call(context.Background(), "session.view", map[string]any{"session_id": sess}, &sv); err != nil {
		t.Fatal(err)
	}
	var leaves func(n layoutNode) []string
	leaves = func(n layoutNode) []string {
		if n.Split == nil {
			return []string{n.BlockID}
		}
		return append(leaves(n.Split.Before), leaves(n.Split.After)...)
	}
	var out [][]string
	for _, w := range sv.Windows {
		for _, l := range w.Layers {
			if l.Kind == "floating" {
				out = append(out, leaves(l.Layout))
			}
		}
	}
	return out
}

// With ui_slot "layer", a panel opens as one floating layer, a replacement leaves one layer (the
// new pane's), and the close of the last view removes the layer.
func TestSlotLayerLab(t *testing.T) {
	h, r, sess, win := panelsLabHub(t)
	h.cfg.UISlot = "layer"
	ctx := context.Background()
	if out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-l1", Plugin: "p", Route: "/panels/one", Ctx: panel(sess, win)}); err != nil || out["status"] != "opened" {
		t.Fatalf("first panel: %v %v", out, err)
	}
	one := formPanes(t, r, sess)[slotViewID("repl-l1")]
	if layers := floatingLayers(t, r, sess); len(layers) != 1 || len(layers[0]) != 1 || layers[0][0] != one {
		t.Fatalf("one floating layer with the panel %s: %v", one, layers)
	}
	if out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-l2", Plugin: "p", Route: "/panels/two", Ctx: panel(sess, win)}); err != nil || out["status"] != "opened" {
		t.Fatalf("replacement: %v %v", out, err)
	}
	panes := formPanes(t, r, sess)
	two := panes[slotViewID("repl-l2")]
	if _, old := panes[slotViewID("repl-l1")]; old || two == "" {
		t.Fatalf("the replacement closed the old pane: %v", panes)
	}
	if layers := floatingLayers(t, r, sess); len(layers) != 1 || len(layers[0]) != 1 || layers[0][0] != two {
		t.Fatalf("one floating layer with the new panel %s: %v", two, layers)
	}
	if err := h.closeForm(ctx, slotViewID("repl-l2"), "test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(floatingLayers(t, r, sess)) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the close left a floating layer: %v", floatingLayers(t, r, sess))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
