package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHomeRouteRefusedLab: the home plugin refuses a route it does not know. No path opens such a
// view, so the test makes one while the hub is stopped: an open form of the home plugin at route
// "/" in the LazyRex session, with its pane and process. The started hub keeps it (formsSweep), and
// the renderer gets the hub's error tree. The hub log names the plugin's reason.
// Lab only: LAZYREX_LAB=1.
func TestHomeRouteRefusedLab(t *testing.T) {
	bin, root := harnessBinary(t)
	rexBin, err := exec.LookPath("rex")
	if err != nil {
		t.Skip("no rex CLI")
	}
	dir := filepath.Join(root, "tests", "harness")
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	home, err := homePlugin(root)
	if err != nil {
		t.Fatal(err)
	}
	o := &testOpts{dir: dir, lazyrex: root, home: home, self: bin, rexBin: rexBin, m: m}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	l, err := o.startLab(ctx)
	if l != nil {
		defer func() {
			for _, p := range l.stop() {
				t.Error(p)
			}
			endGoLab(t, l.dir, l.unproved)
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := o.startHub(ctx, l); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(l.dir, "hub")
	var sess string
	waitUntil := func(what string, d time.Duration, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(d); !ok(); time.Sleep(200 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	waitUntil("the LazyRex session", 20*time.Second, func() bool {
		db, err := openDB(filepath.Join(state, "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.QueryRow(`SELECT session_id FROM ui_sessions WHERE plugin = ?`, uiSessionKey).Scan(&sess)
		return sess != ""
	})
	if err := stopHubForRestart(ctx, l, "term"); err != nil {
		t.Fatalf("the hub did not stop cleanly: %v", err)
	}
	l.restarts++
	os.Rename(filepath.Join(l.dir, "hub.log"), filepath.Join(l.dir, "hub.1.log"))

	// The form: a test renderer at "/" in a pane with the form's label, and its 'sending' row.
	const id = "home-form"
	var w struct {
		Window string   `json:"window_id"`
		Blocks []string `json:"block_ids"`
	}
	cmd := []string{filepath.Join(l.dir, "testview.sh"), "-view-id", id, "-server", l.server, "lazyrex", "/"}
	if err := l.r.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.r.Call(ctx, "session.new_window", map[string]any{"session_id": sess, "window_label": "Form", "focus": false,
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "label": "lazyrex-form:" + id,
			"options": map[string]any{"command": cmd, "login": false}}}}, &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Blocks) != 1 {
		t.Fatalf("new window: %d blocks", len(w.Blocks))
	}
	// The renderer may attach only from the view's recorded process (hub.view.attach).
	var proc struct {
		Child struct {
			PID int `json:"pid"`
		} `json:"child"`
	}
	if err := l.r.BlockCall(ctx, "com.superlogical.terminal.process", sess, w.Blocks[0], nil, &proc); err != nil || proc.Child.PID == 0 {
		t.Fatalf("the form pane's process: %+v %v", proc, err)
	}
	fc, _ := json.Marshal(formContext{SessionID: sess, WindowID: w.Window})
	db, err := openDB(filepath.Join(state, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, context, session_id, window_id, block_id, child_pid, child_start, attach_state)
		VALUES (?, 'lazyrex', ?, '/', 'open', ?, ?, ?, ?, ?, ?, 'never')`, id, formTab, string(fc), sess, w.Window, w.Blocks[0],
		proc.Child.PID, startTime(proc.Child.PID)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := o.startHub(ctx, l); err != nil {
		t.Fatal(err)
	}
	// The refusal, seen by the renderer: the snapshot is the hub's error tree, and the hub log names
	// the plugin's reason.
	hc := &helperConn{r: l.r}
	var tree struct {
		Tree struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"tree"`
	}
	waitUntil("the refused render reaches the renderer", 30*time.Second, func() bool {
		tv, err := hc.resolveClient(ctx, "lazyrex-testview:"+id)
		if err != nil {
			return false
		}
		raw, err := hc.perform(ctx, tv, "testview.get", nil)
		if err != nil || json.Unmarshal(raw, &tree) != nil {
			return false
		}
		return len(tree.Tree.Nodes) == 1 && tree.Tree.Nodes[0].ID == "$error"
	})
	b, _ := os.ReadFile(filepath.Join(l.dir, "hub.log"))
	if !strings.Contains(string(b), "views lazyrex: render /: ") || !strings.Contains(string(b), "Unknown home route or user") {
		t.Fatal("the hub log does not show the plugin's refusal of route /")
	}
}
