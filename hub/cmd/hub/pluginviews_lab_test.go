package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// The real Plugins… panel of the home plugin, opened from a pane as the Rex palette does, follows a
// lock change with no action (lead-c8-b2 item 4, positive tracking): a GitHub entry runs commit A;
// plugin update pins B; after the reload the open list renders again by itself, with the running
// commit A and "pinned B" (codex-27, plugins-c8b2-source-blockers item 2). Lab only: HARNESS_LAB=1.
func TestPluginsPanelFollowsLockLab(t *testing.T) {
	s := newStartLabHome(t)
	gitTestEnv(t)
	r := newTestRepo(t)
	first := r.commit(map[string]string{"lazyrex.json": `{"name":"gh","main":"main.lua"}`, "main.lua": fmt.Sprintf(startFixture, "gh")})
	const src = "github:lab/r@main"
	s.setConfig("plugins", []any{map[string]any{"source": s.o.home.dir}, map[string]any{"source": src}})
	opts := pluginCmdOpts{remote: func(owner, repo string) string { return r.url() }}
	if err := pluginSync(context.Background(), s.cfg, opts); err != nil {
		t.Fatal(err)
	}
	s.start()
	s.startRun("gh")
	s.registered("lazyrex")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A pane to run the command from, and a client of our own: hub.palette.run takes the caller's
	// client ID in ctx, as Rex sets it for a palette action. Our client attaches to the pane's
	// session (session.list_blocks needs that).
	pc, err := rex.Dial(ctx, s.l.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		for range pc.Events {
		}
	}()
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := pc.Call(ctx, "session.create", map[string]any{"label": "c8-palette", "initial_windows": []any{map[string]any{"window_label": "c8-palette",
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"command": []string{"/bin/sleep", "100000"}, "login": false}}}}}}, &created); err != nil {
		t.Fatal(err)
	}
	if err := pc.Call(ctx, "session.attach", map[string]any{"session_id": created.SessionID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	var blocks struct {
		Blocks []struct {
			BlockID string `json:"block_id"`
		} `json:"blocks"`
	}
	if err := pc.Call(ctx, "session.list_blocks", map[string]any{"session_id": created.SessionID}, &blocks); err != nil || len(blocks.Blocks) != 1 {
		t.Fatalf("the palette pane: %v %v", blocks, err)
	}
	label := fmt.Sprintf("c8-palette-%d", time.Now().UnixNano())
	if err := pc.Call(ctx, "client.set_label", map[string]any{"label": label}, nil); err != nil {
		t.Fatal(err)
	}
	pid, err := s.c.resolveClient(ctx, label)
	if err != nil {
		t.Fatal(err)
	}
	var run json.RawMessage
	if err := pc.Call(ctx, "client.perform", map[string]any{"client_id": s.hub, "action": "hub.palette.run", "timeout_ms": 10000, "args": map[string]any{
		"command": "lazyrex.plugins", "ctx": map[string]any{"client_id": pid, "session_id": created.SessionID, "block_id": blocks.Blocks[0].BlockID}}}, &run); err != nil {
		t.Fatalf("hub.palette.run: %v", err)
	}

	var viewID string
	s.wait("the Plugins… panel", func() bool {
		var out struct {
			Views []struct {
				ViewID   string `json:"view_id"`
				Route    string `json:"route"`
				Attached bool   `json:"attached"`
			} `json:"views"`
		}
		b, _ := json.Marshal(s.must("hub.test.views", map[string]any{"plugin": "lazyrex"}))
		json.Unmarshal(b, &out)
		for _, v := range out.Views {
			if v.Route == "/panels/plugins" && v.Attached {
				viewID = v.ViewID
			}
		}
		return viewID != ""
	})
	// detail is the gh row of the list in the renderer's snapshot.
	detail := func() string {
		tv, err := s.c.resolveClient(ctx, "lazyrex-testview:"+viewID)
		if err != nil {
			return ""
		}
		raw, err := s.c.perform(ctx, tv, "testview.get", nil)
		if err != nil {
			return ""
		}
		var snap struct {
			Tree struct {
				Nodes []struct {
					ID    string `json:"id"`
					Items []struct {
						ID     string `json:"id"`
						Detail string `json:"detail"`
					} `json:"items"`
				} `json:"nodes"`
			} `json:"tree"`
		}
		json.Unmarshal(raw, &snap)
		for _, n := range snap.Tree.Nodes {
			for _, i := range n.Items {
				if n.ID == "entries" && i.ID == src {
					return i.Detail
				}
			}
		}
		return ""
	}
	// renders gives the render attempts of the view, and if a plugins change marks it: with no
	// declaration, or no changes list in it, every change marks the view (docs/hub.md, deps).
	renders := func() (attempts int, tracks bool) {
		var out struct {
			Views []struct {
				ViewID   string `json:"view_id"`
				Attempts int    `json:"attempts"`
				Declared *struct {
					Changes *[]string `json:"changes"`
				} `json:"declared"`
			} `json:"views"`
		}
		b, _ := json.Marshal(s.must("hub.test.renders", map[string]any{"plugin": "lazyrex"}))
		json.Unmarshal(b, &out)
		for _, v := range out.Views {
			if v.ViewID == viewID {
				d := v.Declared
				return v.Attempts, d == nil || d.Changes == nil || contains(*d.Changes, "plugins")
			}
		}
		return 0, false
	}
	s.wait("the list shows commit A", func() bool { return strings.Contains(detail(), first[:12]) })
	if d := detail(); strings.Contains(d, "pinned") || !strings.Contains(d, src) {
		t.Fatalf("before the update: %q", d)
	}
	before, tracks := renders()
	if !tracks {
		t.Fatal("a plugins change does not mark the panel")
	}

	second := r.commit(map[string]string{"main.lua": fmt.Sprintf(startFixture, "gh") + "-- second\n"})
	var out bytes.Buffer
	if err := runPluginCmd(context.Background(), "update", s.cfg, nil, opts, &out); err != nil {
		t.Fatalf("update: %v %s", err, out.String())
	}
	s.must("hub.plugins.reload", nil)
	s.wait("the open list shows the pinned commit B", func() bool { return strings.Contains(detail(), "pinned "+second[:12]) })
	if d := detail(); !strings.Contains(d, first[:12]) {
		t.Fatalf("the running commit A is gone from the row: %q", d)
	}
	if after, _ := renders(); after <= before {
		t.Fatalf("render attempts %d -> %d: the list did not render again", before, after)
	}
}
