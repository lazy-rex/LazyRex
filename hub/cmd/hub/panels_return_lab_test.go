package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The return of a closed form in layer mode (contract v2.1, the hub lead with codex-17 and
// codex-28, 2026-10-22). Lab only (rexLab: LAB=1), as panels_lab_test.go.

// sleepBlock is a lab block layout that runs nothing.
func sleepBlock() map[string]any {
	return map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell",
		"options": map[string]any{"command": []string{"/bin/sleep", "600"}, "login": false}}}
}

// layerForm opens a focused panel layer in win, then a form from it, as the palette does. It
// returns the form's view and block, and the tiled block that had the focus before (under).
func layerForm(t *testing.T, h *Hub, sess, win string) (form, formBlock, under string) {
	t.Helper()
	ctx := context.Background()
	_, under, tiled, err := h.slotFocus(ctx, sess, win)
	if err != nil || !tiled {
		t.Fatalf("the lab window's focus: %s %v %v", under, tiled, err)
	}
	if out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-panel", Plugin: "p", Route: "/panels/p", Ctx: panel(sess, win), Focus: true}); err != nil || out["status"] != "opened" {
		t.Fatalf("panel: %v %v", out, err)
	}
	if out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-form", Plugin: "p", Route: "/forms/f", Ctx: formContext{SessionID: sess, WindowID: win}, Focus: true}); err != nil || out["status"] != "opened" {
		t.Fatalf("form: %v %v", out, err)
	}
	form = slotViewID("repl-form")
	fc, _ := h.formOf(ctx, form)
	if fc.Return == nil || fc.UnderBlock != under || fc.Return.Context.UnderBlock != under {
		t.Fatalf("the form keeps the panel's under %s: %+v", under, fc)
	}
	return form, h.viewBlock(form), under
}

// userCloses closes the form's pane as the user does (Esc): Rex closes the layer, then the hub
// gets block_closed.
func userCloses(t *testing.T, h *Hub, sess, form, block string) {
	t.Helper()
	ctx := context.Background()
	if err := h.rex.Call(ctx, "block.close", map[string]any{"session_id": sess, "block_id": block}, nil); err != nil {
		t.Fatal(err)
	}
	if !h.uiBlockClosed(ctx, block, false) {
		t.Fatal("the form's block was not a view")
	}
}

// returnEnd waits for the return row of form to end, and returns its state.
func returnEnd(t *testing.T, h *Hub, form string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := replState(t, h, "return-"+form)
		if st == "done" || st == "skipped" || st == "cancelled" {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("the return of %s did not end: %q", form, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// skipWhy is the why of the form_return_skipped event of view, or "".
func skipWhy(t *testing.T, h *Hub, view string) string {
	t.Helper()
	b, err := os.ReadFile(h.ev.f.Name())
	if err != nil {
		t.Fatal(err)
	}
	why := ""
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["event"] == "form_return_skipped" && e["view_id"] == view {
			why, _ = e["why"].(string)
		}
	}
	return why
}

func layerLab(t *testing.T) (*Hub, string, string) {
	h, _, sess, win := panelsLabHub(t)
	h.cfg.UISlot = "layer"
	return h, sess, win
}

// The main case: panel layer (under = the tiled block) -> form from the panel (keeps under) ->
// the user closes the form -> the panel returns in one floating layer, with the focus.
func TestSlotLayerReturnFocusLab(t *testing.T) {
	h, sess, win := layerLab(t)
	form, block, _ := layerForm(t, h, sess, win)
	userCloses(t, h, sess, form, block)
	if st := returnEnd(t, h, form); st != "done" {
		t.Fatalf("return state %s, why %q", st, skipWhy(t, h, form))
	}
	panelBlock := formPanes(t, h.rex, sess)[slotViewID("return-"+form)]
	if layers := floatingLayers(t, h.rex, sess); len(layers) != 1 || len(layers[0]) != 1 || layers[0][0] != panelBlock {
		t.Fatalf("one floating layer with the panel %s: %v", panelBlock, layers)
	}
	active, focused, _, err := h.slotFocus(context.Background(), sess, win)
	if err != nil || !active || focused != panelBlock {
		t.Fatalf("the returned panel has the focus: active %v, focused %s, want %s (%v)", active, focused, panelBlock, err)
	}
	fc, _ := h.formOf(context.Background(), slotViewID("return-"+form))
	if fc.UnderBlock == "" {
		t.Fatal("the returned panel lost its under block")
	}
}

// Another window is active at the close: no layer opens, and the active window does not change.
func TestSlotLayerReturnOtherWindowLab(t *testing.T) {
	h, sess, win := layerLab(t)
	ctx := context.Background()
	form, block, _ := layerForm(t, h, sess, win)
	var w2 struct {
		WindowID string `json:"window_id"`
	}
	if err := h.rex.Call(ctx, "session.new_window", map[string]any{"session_id": sess, "window_label": "w2", "focus": true, "layout": sleepBlock()}, &w2); err != nil {
		t.Fatal(err)
	}
	userCloses(t, h, sess, form, block)
	if st, why := returnEnd(t, h, form), skipWhy(t, h, form); st != "skipped" || why != "window_not_active" {
		t.Fatalf("return state %s, why %q; want skipped, window_not_active", st, why)
	}
	if layers := floatingLayers(t, h.rex, sess); len(layers) != 0 {
		t.Fatalf("a skipped return opened a layer: %v", layers)
	}
	if active, _, _, _ := h.slotFocus(ctx, sess, w2.WindowID); !active {
		t.Fatal("the return changed the active window")
	}
}

// Another floating layer has the focus at the close: the return does not take it.
func TestSlotLayerReturnOtherLayerLab(t *testing.T) {
	h, sess, win := layerLab(t)
	ctx := context.Background()
	form, block, _ := layerForm(t, h, sess, win)
	var other struct {
		BlockIDs []string `json:"block_ids"`
	}
	if err := h.rex.Call(ctx, "session.new_layer", map[string]any{"session_id": sess, "window_id": win, "bounds": []float64{0.1, 0.1, 0.3, 0.3},
		"focus": true, "layout": sleepBlock()}, &other); err != nil || len(other.BlockIDs) != 1 {
		t.Fatalf("other layer: %v %v", other, err)
	}
	userCloses(t, h, sess, form, block)
	if st, why := returnEnd(t, h, form), skipWhy(t, h, form); st != "skipped" || why != "focus_moved" {
		t.Fatalf("return state %s, why %q; want skipped, focus_moved", st, why)
	}
	if _, focused, _, _ := h.slotFocus(ctx, sess, win); focused != other.BlockIDs[0] {
		t.Fatalf("the focus left the other layer: %s", focused)
	}
	if layers := floatingLayers(t, h.rex, sess); len(layers) != 1 {
		t.Fatalf("only the other layer stays: %v", layers)
	}
}

// codex-28's case: the form opened over tiled pane A; the user moves to tiled pane B of the same
// window; the form closes. The return must not take the focus from B.
func TestSlotLayerReturnOtherTiledPaneLab(t *testing.T) {
	h, sess, win := layerLab(t)
	ctx := context.Background()
	_, a, _, _ := h.slotFocus(ctx, sess, win)
	var split struct {
		BlockIDs []string `json:"block_ids"`
	}
	if err := h.rex.Call(ctx, "session.new_split", map[string]any{"session_id": sess, "anchor_block_id": a, "direction": "horizontal", "side": "after",
		"ratio": 0.5, "focus": false, "layout": sleepBlock()}, &split); err != nil || len(split.BlockIDs) != 1 {
		t.Fatalf("pane B: %v %v", split, err)
	}
	b := split.BlockIDs[0]
	form, block, under := layerForm(t, h, sess, win)
	if under != a {
		t.Fatalf("under %s, want pane A %s", under, a)
	}
	if err := h.rex.Call(ctx, "session.focus_block", map[string]any{"session_id": sess, "block_id": b}, nil); err != nil {
		t.Fatal(err)
	}
	userCloses(t, h, sess, form, block)
	if st, why := returnEnd(t, h, form), skipWhy(t, h, form); st != "skipped" || why != "focus_moved" {
		t.Fatalf("return state %s, why %q; want skipped, focus_moved", st, why)
	}
	if _, focused, _, _ := h.slotFocus(ctx, sess, win); focused != b {
		t.Fatalf("the focus left pane B: %s", focused)
	}
	if layers := floatingLayers(t, h.rex, sess); len(layers) != 0 {
		t.Fatalf("a skipped return opened a layer: %v", layers)
	}
}

// A form row from before under was recorded is skipped (under_unknown). A skip is terminal: a
// hub start and the slot sweep open no layer for it.
func TestSlotLayerReturnUnderUnknownLab(t *testing.T) {
	h, sess, win := layerLab(t)
	ctx := context.Background()
	old := formContext{SessionID: sess, WindowID: win, Return: &slotReturn{Plugin: "p", Route: "/panels/p", Context: panel(sess, win)}}
	if took, err := h.formEnded(ctx, "view-old-form", old, false, ""); took || err != nil {
		t.Fatalf("closed form: %v %v", took, err)
	}
	if st, why := replState(t, h, "return-view-old-form"), skipWhy(t, h, "view-old-form"); st != "skipped" || why != "under_unknown" {
		t.Fatalf("return state %s, why %q; want skipped, under_unknown", st, why)
	}
	if err := h.slotStart(ctx); err != nil {
		t.Fatal(err)
	}
	h.slotSweep(ctx)
	if _, err := h.openSlot(ctx, slotRequest{ReplID: "return-view-old-form"}); err == nil {
		t.Fatal("a replay of the skipped return was not refused")
	}
	if st := replState(t, h, "return-view-old-form"); st != "skipped" {
		t.Fatalf("the skip changed to %s", st)
	}
	if layers := floatingLayers(t, h.rex, sess); len(layers) != 0 {
		t.Fatalf("a skipped return opened a layer: %v", layers)
	}
}
