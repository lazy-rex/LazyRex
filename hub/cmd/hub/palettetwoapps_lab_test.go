package main

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// A palette run on a session that two apps show opens its panel in the side slot of the window of
// ctx.block_id, as with one app (the user, 2026-10-26): no new tab, and select names the pane's
// window. A run without block_id uses the session's active window (docs/hub.md, "Palette runs with
// two devices"). Lab only: LAZYREX_LAB=1.
func TestPaletteTwoAppsSideSlotLab(t *testing.T) {
	s := newStartLabHome(t, nil)
	s.setConfig("plugins", []any{map[string]any{"source": s.o.home.dir}})
	s.start()
	s.registered("lazyrex")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pc, err := rex.Dial(ctx, s.l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		for range pc.Events {
		}
	}()
	var created struct {
		SessionID string `json:"session_id"`
	}
	win := func(label string) map[string]any {
		return map[string]any{"window_label": label, "layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell",
			"options": map[string]any{"command": []string{"/bin/sleep", "100000"}, "login": false}}}}
	}
	if err := pc.Call(ctx, "session.create", map[string]any{"label": "two-apps", "initial_windows": []any{win("two-apps"), win("other")}}, &created); err != nil {
		t.Fatal(err)
	}
	sess := created.SessionID
	if err := pc.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	type swindow struct {
		WindowID       string `json:"window_id"`
		FocusedBlockID string `json:"focused_block_id"`
		Layers         []struct {
			Blocks []struct {
				BlockID string `json:"block_id"`
				Label   string `json:"label"`
			} `json:"blocks"`
		} `json:"layers"`
	}
	type sview struct {
		ActiveWindowID string    `json:"active_window_id"`
		Windows        []swindow `json:"windows"`
	}
	read := func() sview {
		var v sview
		if err := pc.Call(ctx, "session.view", map[string]any{"session_id": sess}, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	// The run's pane is in the first window; the other window is the active one, so a run that ignored
	// ctx.block_id would open there.
	focusOther := func(v sview) {
		if err := pc.Call(ctx, "session.focus_block", map[string]any{"session_id": sess, "block_id": v.Windows[1].FocusedBlockID}, nil); err != nil {
			t.Fatal(err)
		}
	}
	v0 := read()
	if len(v0.Windows) != 2 {
		t.Fatalf("start: %d windows", len(v0.Windows))
	}
	focusOther(v0)
	v0 = read()
	paneWindow, pane, otherWindow := v0.Windows[0].WindowID, v0.Windows[0].FocusedBlockID, v0.Windows[1].WindowID
	if v0.ActiveWindowID != otherWindow {
		t.Fatalf("start: the active window is %s, want %s", v0.ActiveWindowID, otherWindow)
	}
	label := fmt.Sprintf("two-apps-%d", time.Now().UnixNano())
	if err := pc.Call(ctx, "client.set_label", map[string]any{"label": label}, nil); err != nil {
		t.Fatal(err)
	}
	pid, err := s.c.resolveClient(ctx, label)
	if err != nil {
		t.Fatal(err)
	}
	type runOut struct {
		Result struct {
			Status   string `json:"status"`
			ViewID   string `json:"view_id"`
			WindowID string `json:"window_id"`
			Select   struct {
				SessionID string `json:"session_id"`
				WindowID  string `json:"window_id"`
			} `json:"select"`
		} `json:"result"`
	}
	run := func(rc map[string]any) runOut {
		var out runOut
		if err := pc.Call(ctx, "client.perform", map[string]any{"client_id": s.hub, "action": "hub.palette.run", "timeout_ms": 10000,
			"args": map[string]any{"command": "lazyrex.plugins", "ctx": rc}}, &out); err != nil {
			t.Fatalf("hub.palette.run: %v", err)
		}
		return out
	}

	s.must("hub.test.app", map[string]any{"name": "mac", "sessions": []any{sess}})
	s.must("hub.test.app", map[string]any{"name": "phone", "sessions": []any{sess}})
	two := run(map[string]any{"client_id": pid, "session_id": sess, "block_id": pane, "window_id": paneWindow}).Result
	if two.Status != "opened" || two.Select.SessionID != sess || two.Select.WindowID != paneWindow {
		t.Fatalf("two apps: %+v, want select of the pane's window %s", two, paneWindow)
	}
	// The panel's pane is beside the run's pane, in the pane's window; no tab was added.
	v1 := read()
	var labels []string
	for _, w := range v1.Windows {
		if w.WindowID != paneWindow {
			continue
		}
		for _, l := range w.Layers {
			for _, b := range l.Blocks {
				labels = append(labels, b.Label)
			}
		}
	}
	if len(v1.Windows) != 2 || !slices.Contains(labels, "lazyrex-form:"+two.ViewID) {
		t.Fatalf("two apps: %d windows, the pane's window holds %q", len(v1.Windows), labels)
	}
	s.viewAt(ctx, "/panels/plugins") // the renderer attached

	// palette_ctx keeps what the run carried apart from the server's focus.
	lastCtx := func() map[string]any {
		evs := s.events("palette_ctx", "")
		if len(evs) == 0 {
			t.Fatal("no palette_ctx event")
		}
		return evs[len(evs)-1]
	}
	if e := lastCtx(); e["kind"] != "command" || e["in_client_id"] != pid || e["in_session_id"] != sess || e["in_block_id"] != pane ||
		e["in_window_id"] != paneWindow || e["server_apps"] != float64(2) {
		t.Fatalf("two apps: palette_ctx %v", e)
	}

	// Without block_id: the session's active window (the other one again), still no tab.
	focusOther(v0)
	noBlock := run(map[string]any{"client_id": pid, "session_id": sess}).Result
	if noBlock.Select.WindowID != otherWindow {
		t.Fatalf("two apps, no block_id: select %+v, want the active window %s", noBlock.Select, otherWindow)
	}
	if n := len(read().Windows); n != 2 {
		t.Fatalf("two apps, no block_id: %d windows, want no new tab", n)
	}
	e := lastCtx()
	if e["in_block_id"] != nil || e["server_active_window_id"] != otherWindow || e["server_focused_block_id"] != v0.Windows[1].FocusedBlockID {
		t.Fatalf("two apps, no block_id: palette_ctx %v", e)
	}
	if _, ok := e["in_window_id"]; ok {
		t.Fatalf("two apps, no block_id: in_window_id without a ctx.window_id: %v", e)
	}

	s.must("hub.test.app.close", map[string]any{"name": "phone"})
	one := run(map[string]any{"client_id": pid, "session_id": sess, "block_id": pane, "window_id": paneWindow}).Result
	if one.Select.WindowID != paneWindow {
		t.Fatalf("one app: select %+v, want the pane's window %s", one.Select, paneWindow)
	}
	if n := len(read().Windows); n != 2 {
		t.Fatalf("one app: %d windows, want no new tab", n)
	}
	if e := lastCtx(); e["in_block_id"] != pane || e["in_window_id"] != paneWindow || e["server_apps"] != float64(1) {
		t.Fatalf("one app: palette_ctx %v", e)
	}

	// The restart form's palette entry records the same event.
	var rs runOut
	if err := pc.Call(ctx, "client.perform", map[string]any{"client_id": s.hub, "action": "hub.admin.restart.form", "timeout_ms": 10000,
		"args": map[string]any{"kind": "claude", "ctx": map[string]any{"client_id": pid, "session_id": sess, "block_id": pane}}}, &rs); err != nil {
		t.Fatalf("hub.admin.restart.form: %v", err)
	}
	if e := lastCtx(); e["kind"] != "restart" || e["in_block_id"] != pane || e["in_client_id"] != pid {
		t.Fatalf("restart: palette_ctx %v", e)
	}
}
