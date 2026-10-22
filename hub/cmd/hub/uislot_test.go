package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ui_slot is "split" (the default) or "layer"; any other value stops the config load.
func TestUISlotConfig(t *testing.T) {
	dir := t.TempDir()
	for value, want := range map[string]string{``: "", `"ui_slot": "split",`: "", `"ui_slot": "layer",`: "", `"ui_slot": "float",`: `ui_slot is "split" or "layer"`} {
		path := filepath.Join(dir, "hub.json")
		body := `{` + value + `"server": "unix:///x", "state": "` + dir + `", "agents": [{"id": "user"}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfig(path)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", value, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: got %v, want %q", value, err, want)
		}
	}
}

// slotPane sends the split as it is, or with ui_slot "layer" one new_layer in the window with the
// same block layout and focus, and none of the split fields.
func TestSlotPaneLayer(t *testing.T) {
	layout := map[string]any{"block": map[string]any{"label": "lazyrex-form:v1"}}
	split := map[string]any{"session_id": "session:s", "anchor_block_id": "block:a", "direction": "horizontal",
		"side": "after", "ratio": 0.55, "focus": true, "layout": layout}
	var method string
	var args map[string]any
	call := func(h *Hub, ctx context.Context, m string, a map[string]any, out any) error {
		method, args = m, a
		return nil
	}
	h := &Hub{}
	if err := h.slotPane(context.Background(), "window:w", split, nil, call); err != nil {
		t.Fatal(err)
	}
	if method != "session.new_split" || !reflect.DeepEqual(args, split) {
		t.Errorf("split: %s %v", method, args)
	}
	h.cfg.UISlot = "layer"
	if err := h.slotPane(context.Background(), "window:w", split, nil, call); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"session_id": "session:s", "window_id": "window:w", "bounds": slotLayerBounds, "focus": true, "layout": layout}
	if method != "session.new_layer" || !reflect.DeepEqual(args, want) {
		t.Errorf("layer: %s %v", method, args)
	}
}

// The pair check reads the layer that holds the composer: a pair in a floating layer is found
// there, and a composer in no layer asks for no move.
func TestHasBlock(t *testing.T) {
	leaf := func(b string) layoutNode { return layoutNode{BlockID: b} }
	n := layoutNode{Split: &struct {
		Direction string     `json:"direction"`
		Before    layoutNode `json:"before"`
		After     layoutNode `json:"after"`
	}{Direction: "vertical", Before: leaf("block:t"), After: leaf("block:c")}}
	if !hasBlock(n, "block:c") || !hasBlock(n, "block:t") || hasBlock(n, "block:x") || !hasBlock(leaf("block:c"), "block:c") {
		t.Error("hasBlock")
	}
}
