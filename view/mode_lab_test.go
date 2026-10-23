package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newModeLab is a chunk lab (lab server, hub -test, real renderer panes) with the plugin
// testdata/modelab. Lab only: MODE_LAB=1.
func newModeLab(t *testing.T) *chunkLab {
	if os.Getenv("MODE_LAB") != "1" {
		t.Skip("set MODE_LAB=1 to run the thread view mode labs (they start their own lab server)")
	}
	t.Setenv("CHUNK_LAB", "1") // newChunkLab's own gate
	l := newChunkLab(t, "modelab", "view/testdata/modelab")
	b, _ := json.Marshal(map[string]any{"plugin": "modelab", "sql": "SELECT count(*) FROM clicked", "params": []any{}})
	f := filepath.Join(l.lab, "tables.json")
	os.WriteFile(f, b, 0o600)
	l.waitFor("the plugin's tables", 30*time.Second, func() bool {
		_, err := l.cli("-C", "lazyrex-hub", "do", "hub.test.sql", "--args", "@"+f)
		return err == nil
	})
	l.sql("INSERT INTO stage VALUES (0)", true)
	return l
}

// resize sizes a taken pane.
func (l *chunkLab) resize(pane string, columns, rows int) {
	l.t.Helper()
	if err := l.owner.call(l.ctx, "block.method", map[string]any{"session_id": l.session, "block_id": pane, "method": "resize", "args": map[string]int{"columns": columns, "rows": rows}}, nil); err != nil {
		l.t.Fatal(err)
	}
}

func (l *chunkLab) key(pane, key string) {
	l.t.Helper()
	l.must("send-key", "-s", l.session, "-b", pane, key)
}

// TestFocusWindowLab (lead-tv-c2rules, B9): a real 10-row pane in thread mode gets an error tree
// with 12 actions. Tab and Shift+Tab reach every action, the focused one is on screen with the
// more-above and more-below rows, and Enter on the last action runs it.
func TestFocusWindowLab(t *testing.T) {
	l := newModeLab(t)
	pane := l.open("lazyrex_modelab_mode")
	l.resize(pane, 80, 10)
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last screen:\n%s", last)
		}
	})
	l.waitFor("the thread frame", 30*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "LAB-NOTICE")
	})
	l.sql("UPDATE stage SET n = 1", true)
	label := func(i int) string { return fmt.Sprintf("Action w%02d", i) }
	focused := func(i int, step string) {
		t.Helper()
		l.waitFor(step+": "+label(i)+" on screen", 10*time.Second, func() bool {
			last = l.screen(pane)
			return strings.Contains(last, label(i))
		})
	}
	focused(1, "the error tree")
	if !strings.Contains(last, "more below") || strings.Contains(last, "more above") {
		t.Fatalf("the first window has no more-below row, or has a more-above row:\n%s", last)
	}
	for i := 2; i <= 12; i++ {
		l.key(pane, "tab")
		focused(i, fmt.Sprintf("Tab %d", i-1))
	}
	if !strings.Contains(last, "more above") || strings.Contains(last, "more below") {
		t.Fatalf("the last window has no more-above row, or has a more-below row:\n%s", last)
	}
	t.Logf("the last control focused:\n%s", last)
	for i := 11; i >= 1; i-- {
		l.key(pane, "shift+tab")
		focused(i, fmt.Sprintf("Shift+Tab to %d", i))
	}
	for i := 2; i <= 12; i++ {
		l.key(pane, "tab")
		focused(i, fmt.Sprintf("Tab again %d", i-1))
	}
	l.key(pane, "enter")
	l.waitFor("the last action runs", 20*time.Second, func() bool {
		rows := l.sql("SELECT name FROM clicked", false)
		return len(rows) == 1 && rows[0][0] == "do-w12"
	})
}

// tabPane waits for the plugin's open, attached tab view on route, takes its pane and sizes it.
// It returns the pane's session and block.
func (l *chunkLab) tabPane(route string, columns, rows int) (session, pane string) {
	l.t.Helper()
	l.waitFor("the tab view "+route, 30*time.Second, func() bool {
		var r struct {
			Views []struct {
				Route    string `json:"route"`
				Attached bool   `json:"attached"`
				Session  string `json:"session_id"`
				Block    string `json:"block_id"`
			} `json:"views"`
		}
		l.hubTest("hub.test.views", map[string]any{}, &r)
		for _, v := range r.Views {
			if v.Route == route && v.Attached && v.Block != "" {
				session, pane = v.Session, v.Block
			}
		}
		return pane != ""
	})
	if err := l.owner.call(l.ctx, "session.attach", map[string]any{"session_id": session, "event_scope": "session"}, nil); err != nil {
		l.t.Fatal(err)
	}
	if err := l.owner.call(l.ctx, "session.own_block", map[string]any{"session_id": session, "block_ids": []string{pane}, "mode": "take"}, nil); err != nil {
		l.t.Fatal(err)
	}
	if err := l.owner.call(l.ctx, "block.method", map[string]any{"session_id": session, "block_id": pane, "method": "resize", "args": map[string]int{"columns": columns, "rows": rows}}, nil); err != nil {
		l.t.Fatal(err)
	}
	return session, pane
}

// TestReloadLeavesNoLiveRowsLab (v1.2, C11): a thread tab reloads its renderer (hub.ui.reload;
// the process and the pane stay). The old renderer's last frame is empty, so after the new
// renderer draws, the pane shows the live region once.
func TestReloadLeavesNoLiveRowsLab(t *testing.T) {
	l := newModeLab(t)
	session, pane := l.tabPane("/thread/lab", 80, 20)
	screen := func() string {
		out, _ := l.cli("block", "capture", "-s", session, "-b", pane)
		return out
	}
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last screen:\n%s", last)
		}
	})
	l.waitFor("the thread frame", 30*time.Second, func() bool {
		last = screen()
		return strings.Count(last, "LAB-NOTICE") == 1
	})
	events := filepath.Join(l.lab, "state-hub", "events.jsonl")
	reloads := func() int { return strings.Count(string(must(os.ReadFile(events))), `"event":"view_reload"`) }
	before := reloads()
	b, _ := json.Marshal(map[string]any{"plugin": "modelab"})
	f := filepath.Join(l.lab, "reload.json")
	os.WriteFile(f, b, 0o600)
	out := l.must("-C", "lazyrex-hub", "do", "hub.ui.reload", "--args", "@"+f)
	t.Logf("hub.ui.reload: %s", strings.TrimSpace(out))
	l.waitFor("the hub's reload request", 10*time.Second, func() bool { return reloads() > before })
	// The new renderer draws "Connecting…" first, then the tree.
	time.Sleep(2 * time.Second)
	l.waitFor("the new renderer's thread frame", 30*time.Second, func() bool {
		last = screen()
		return strings.Contains(last, "LAB-NOTICE") && !strings.Contains(last, "Connecting")
	})
	if n := strings.Count(last, "LAB-NOTICE"); n != 1 {
		t.Fatalf("the pane shows the live region %d times after the reload:\n%s", n, last)
	}
	t.Logf("after the reload:\n%s", last)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
