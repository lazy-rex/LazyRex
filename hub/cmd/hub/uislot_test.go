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

// test.ui_slot goes into the lab hub config; another value fails at the manifest load.
func TestTestUISlot(t *testing.T) {
	if _, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{"ui_slot":"float"}}`); err == nil || !strings.Contains(err.Error(), `test.ui_slot is "split" or "layer"`) {
		t.Fatalf("a bad test.ui_slot: %v", err)
	}
	dir, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{"ui_slot":"layer"}}`)
	if err != nil {
		t.Fatal(err)
	}
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	lazyrex := t.TempDir()
	os.MkdirAll(filepath.Join(lazyrex, "plugins", "lazyrex"), 0o700)
	os.WriteFile(filepath.Join(lazyrex, "plugins", "lazyrex", "lazyrex.json"), []byte(`{"name":"lazyrex","main":"lazyrex.lua"}`), 0o600)
	os.WriteFile(filepath.Join(lazyrex, "plugins", "lazyrex", "lazyrex.lua"), nil, 0o600)
	home, err := homePlugin(lazyrex)
	if err != nil {
		t.Fatal(err)
	}
	o := &testOpts{dir: dir, lazyrex: lazyrex, home: home, self: "/bin/true", m: m}
	l := &lab{dir: t.TempDir(), server: "unix:///tmp/none/rex.sock"}
	path, err := o.hubConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil || cfg.UISlot != "layer" {
		t.Fatalf("lab hub config: %q %v", cfg.UISlot, err)
	}
}
