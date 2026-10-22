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

// TestHomeRouteRefusedLab: the home plugin declares no tab, so a home view at route "/" in the
// LazyRex session (one that an older hub opened) gets a refused render, and the hub retires it.
// The test renderer holds the retirement (hold mode retire-wait) until the test has seen the
// refusal, so a clean view cannot retire first. Then the row closes once and the pane goes.
// Lab only: HARNESS_LAB=1.
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
	l.snapshot()
	if p := l.hub.stop(); len(p) > 0 {
		l.unproved = append(l.unproved, p...)
		t.Fatalf("the hub did not stop cleanly: %s", strings.Join(p, "; "))
	}
	l.restarts++
	os.Rename(filepath.Join(l.dir, "hub.log"), filepath.Join(l.dir, "hub.1.log"))

	// An older hub's home tab: a test renderer at "/" in the LazyRex session, and its open row.
	const id = "old-home"
	hold := filepath.Join(l.dir, "testview-hold")
	if err := os.WriteFile(hold, []byte("/\nretire-wait\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var w struct {
		Window string   `json:"window_id"`
		Blocks []string `json:"block_ids"`
	}
	cmd := []string{filepath.Join(l.dir, "testview.sh"), "-view-id", id, "-server", l.server, "lazyrex", "/"}
	if err := l.r.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.r.Call(ctx, "session.new_window", map[string]any{"session_id": sess, "window_label": "LazyRex", "focus": false,
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell",
			"options": map[string]any{"command": cmd, "login": false}}}}, &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Blocks) != 1 {
		t.Fatalf("new window: %d blocks", len(w.Blocks))
	}
	block := w.Blocks[0]
	var proc struct {
		Child *struct {
			PID int `json:"pid"`
		} `json:"child"`
	}
	if err := l.r.BlockCall(ctx, "com.superlogical.terminal.process", sess, block, nil, &proc); err != nil || proc.Child == nil {
		t.Fatalf("the renderer pane: %v %+v", err, proc)
	}
	pid := proc.Child.PID
	db, err := openDB(filepath.Join(state, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, session_id, window_id, block_id, child_pid, child_start)
		VALUES (?, 'lazyrex', 'home', '/', 'open', ?, ?, ?, ?, ?)`, id, sess, w.Window, block, pid, startTime(pid)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	row := func() (status, attachment string) {
		db, err := openDB(filepath.Join(state, "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.QueryRow(`SELECT status, attachment FROM views WHERE view_id = ?`, id).Scan(&status, &attachment)
		return
	}
	events := func(name string) int {
		b, _ := os.ReadFile(filepath.Join(state, "events.jsonl"))
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			var e map[string]any
			if json.Unmarshal([]byte(line), &e) == nil && e["event"] == name && e["view_id"] == id {
				n++
			}
		}
		return n
	}
	// pane is "present", "absent", or "unknown" (an error: not a definite answer; ask again).
	pane := func() string {
		var out struct {
			Blocks []struct {
				ID string `json:"block_id"`
			} `json:"blocks"`
		}
		if err := l.r.Call(ctx, "session.list_blocks", map[string]any{"session_id": sess}, &out); err != nil {
			t.Logf("session.list_blocks: %v", err)
			return "unknown"
		}
		for _, b := range out.Blocks {
			if b.ID == block {
				return "present"
			}
		}
		return "absent"
	}

	if err := o.startHub(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitUntil("the renderer attaches and the hub asks view.retire", 20*time.Second, func() bool {
		_, att := row()
		return att != "" && events("view_retire") >= 1
	})
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
	waitUntil("the refused render reaches the renderer", 20*time.Second, func() bool {
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
	if st, _ := row(); st != "open" {
		t.Fatalf("the held view did not wait: status %s", st)
	}

	os.Remove(hold) // the renderer leaves
	waitUntil("the row to close", 20*time.Second, func() bool { st, _ := row(); return st == "closed" })
	waitUntil("the pane to go (a definite answer)", 20*time.Second, func() bool { return pane() == "absent" })
	time.Sleep(2 * time.Second)
	if n := events("view_retired") + events("view_closed"); n != 1 {
		t.Fatalf("completions: %d view_retired + view_closed, want exactly 1", n)
	}
}
