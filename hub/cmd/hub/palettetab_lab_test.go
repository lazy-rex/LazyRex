package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// A palette run on a session that two apps show (ux-multidevice-forms). Rex gives a run without
// block_id the session's last focus, which can be the other device's (docs/rex-protocol.md,
// "Palette runs with two clients"). So the panel opens in a tab of its own, the session's active
// window does not change, and select names the new tab. With one app, a run opens the side slot of
// the window of ctx.block_id, as before. Lab only: LAZYREX_LAB=1.
func TestPaletteTwoAppsOwnTabLab(t *testing.T) {
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
	if err := pc.Call(ctx, "session.create", map[string]any{"label": "two-apps", "initial_windows": []any{map[string]any{"window_label": "two-apps",
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"command": []string{"/bin/sleep", "100000"}, "login": false}}}}}}, &created); err != nil {
		t.Fatal(err)
	}
	sess := created.SessionID
	if err := pc.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	type sview struct {
		ActiveWindowID string `json:"active_window_id"`
		Windows        []struct {
			WindowID       string `json:"window_id"`
			FocusedBlockID string `json:"focused_block_id"`
			Layers         []struct {
				Blocks []struct {
					BlockID string `json:"block_id"`
					Label   string `json:"label"`
				} `json:"blocks"`
			} `json:"layers"`
		} `json:"windows"`
	}
	read := func() sview {
		var v sview
		if err := pc.Call(ctx, "session.view", map[string]any{"session_id": sess}, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	v0 := read()
	if len(v0.Windows) != 1 {
		t.Fatalf("start: %d windows", len(v0.Windows))
	}
	paneWindow, pane := v0.Windows[0].WindowID, v0.Windows[0].FocusedBlockID
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
	two := run(map[string]any{"client_id": pid, "session_id": sess}).Result
	if two.Status != "opened" || two.WindowID == "" || two.WindowID == paneWindow {
		t.Fatalf("two apps: %+v (the pane's window is %s)", two, paneWindow)
	}
	if two.Select.SessionID != sess || two.Select.WindowID != two.WindowID {
		t.Fatalf("two apps: select %+v, the form's tab is %s", two.Select, two.WindowID)
	}
	v1 := read()
	if v1.ActiveWindowID != v0.ActiveWindowID {
		t.Fatalf("two apps: the active window moved %s -> %s (the tab must open without focus)", v0.ActiveWindowID, v1.ActiveWindowID)
	}
	var tabLabels []string
	for _, w := range v1.Windows {
		if w.WindowID != two.WindowID {
			continue
		}
		for _, l := range w.Layers {
			for _, b := range l.Blocks {
				tabLabels = append(tabLabels, b.Label)
			}
		}
	}
	if len(v1.Windows) != 2 || len(tabLabels) != 1 || tabLabels[0] != "lazyrex-form:"+two.ViewID {
		t.Fatalf("two apps: %d windows, the new tab holds %q", len(v1.Windows), tabLabels)
	}
	s.viewAt(ctx, "/panels/plugins") // the renderer attached

	s.must("hub.test.app.close", map[string]any{"name": "phone"})
	one := run(map[string]any{"client_id": pid, "session_id": sess, "block_id": pane}).Result
	if one.Select.WindowID != paneWindow {
		t.Fatalf("one app: select %+v, want the pane's window %s", one.Select, paneWindow)
	}
	if n := len(read().Windows); n != 2 {
		t.Fatalf("one app: %d windows, want no new tab", n)
	}
}
