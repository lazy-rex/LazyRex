package main

import (
	"context"
	"fmt"
	"strings"
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

	// palette_ctx keeps what the run carried apart from the server's focus: no block_id came in.
	lastCtx := func() map[string]any {
		evs := s.events("palette_ctx", "")
		if len(evs) == 0 {
			t.Fatal("no palette_ctx event")
		}
		return evs[len(evs)-1]
	}
	e := lastCtx()
	if e["kind"] != "command" || e["in_client_id"] != pid || e["in_session_id"] != sess || e["in_block_id"] != nil ||
		e["server_apps"] != float64(2) || e["server_active_window_id"] != v0.ActiveWindowID || e["server_focused_block_id"] != pane {
		t.Fatalf("two apps: palette_ctx %v", e)
	}
	if _, ok := e["in_window_id"]; ok {
		t.Fatalf("two apps: in_window_id without a ctx.window_id: %v", e)
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

// The panel of a tab of its own opens a form through the form effect's slot request, and the form
// ends with Escape (its pane closes). The panel stays open beside the form, so the tab keeps a pane
// and needs no return. The panel is the tab's first view, or it replaced the first view (a form,
// or a form that a form replaced): a view that replaces the tab's root takes the root role. On
// dada9d52 and 93c99d0d the form replaced the panel, and the form's close removed the whole tab
// (codex-25's probes; Tunnels: Forward Port -> the tunnels panel -> New). Split and layer. Lab
// only: LAZYREX_LAB=1.
func TestOwnTabPanelKeepsItsTabLab(t *testing.T) {
	for _, mode := range []string{"split", "layer"} {
		for _, chain := range [][]string{{"/panels/a"}, {"/forms/root", "/panels/a"}, {"/forms/root", "/forms/next", "/panels/a"}} {
			t.Run(mode+":"+strings.Join(chain, ">"), func(t *testing.T) {
				h, r, sess, _ := panelsLabHub(t)
				h.cfg.UISlot = mode
				ctx := context.Background()
				opened, err := h.openFormTab(ctx, "p", chain[0], "Root", formContext{SessionID: sess, Panel: strings.HasPrefix(chain[0], "/panels/")})
				if err != nil || opened["status"] != "opened" {
					t.Fatalf("open the root: %+v %v", opened, err)
				}
				viewID, window := opened["view_id"].(string), opened["window_id"].(string)
				for i, route := range chain[1:] {
					// A form's action opens the next view in the form's place (the form effect).
					fc, _ := h.formOf(ctx, viewID)
					out, err := h.openSlot(ctx, slotRequest{ReplID: fmt.Sprintf("own-tab-step-%d", i), Plugin: "p", Route: route,
						Ctx: childContext(fc, strings.HasPrefix(route, "/panels/")), Focus: true, ExpectOld: viewID})
					if err != nil || out["status"] != "opened" {
						t.Fatalf("open %s: %+v %v", route, out, err)
					}
					viewID = out["view_id"].(string)
				}
				panelID := viewID
				fc, ok := h.formOf(ctx, panelID)
				if !ok || fc.WindowID != window || !fc.Panel {
					t.Fatalf("the panel's context: %+v (ok %v), want a panel in window %s", fc, ok, window)
				}
				child, err := h.openSlot(ctx, slotRequest{ReplID: "own-tab-child", Plugin: "p", Route: "/forms/a", Ctx: childContext(fc, false), Focus: true})
				if err != nil || child["status"] != "opened" {
					t.Fatalf("open the form: %+v %v", child, err)
				}
				formID := child["view_id"].(string)
				if h.viewBlock(panelID) == "" {
					t.Fatal("the form's open closed the panel: the tab now ends with the form")
				}
				cc, _ := h.formOf(ctx, formID)
				if err := h.closeForm(ctx, formID, "Escape"); err != nil {
					t.Fatal(err)
				}
				if _, err := h.formEnded(ctx, formID, cc, false, ""); err != nil {
					t.Fatal(err)
				}
				if h.viewBlock(panelID) == "" {
					t.Fatal("after Escape the panel is closed")
				}
				var sv struct {
					Windows []struct {
						WindowID string `json:"window_id"`
					} `json:"windows"`
				}
				if err := r.Call(ctx, "session.view", map[string]any{"session_id": sess}, &sv); err != nil {
					t.Fatal(err)
				}
				alive := false
				for _, w := range sv.Windows {
					alive = alive || w.WindowID == window
				}
				if !alive {
					t.Fatal("after Escape the panel's tab is gone")
				}
			})
		}
	}
}
