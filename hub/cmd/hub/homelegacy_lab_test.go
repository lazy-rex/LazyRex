package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHomeLegacyViewRetiresLab: a home plugin that declares no tabs, and an open view of it in an
// older hub's per-plugin session (a legacy session) with a live test renderer. The hub asks the
// view to retire (view.retire). While the renderer has simulated unsaved text (testview hold mode
// retire-wait; not a real dirty renderer) the view and its pane stay; a hub restart asks again;
// when the text is gone the renderer exits, the row closes once and the pane goes.
// Lab only: HARNESS_LAB=1.
func TestHomeLegacyViewRetiresLab(t *testing.T) {
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
	o := &testOpts{dir: dir, lazyrex: root, home: home, self: bin, rexBin: rexBin, m: m,
		fileHome: "harnessfix", fileArgs: map[string]map[string]string{"harnessfix": {"tabs": "none"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
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
	stopHub := func() {
		t.Helper()
		// The hub's keeper: SIGTERM, then the wait (labkeeper.go), as restartHub does.
		l.snapshot()
		if p := l.hub.stop(); len(p) > 0 {
			l.unproved = append(l.unproved, p...)
			t.Fatalf("the hub did not stop cleanly: %s", strings.Join(p, "; "))
		}
		l.restarts++
		os.Rename(filepath.Join(l.dir, "hub.log"), filepath.Join(l.dir, fmt.Sprintf("hub.%d.log", l.restarts)))
	}
	stopHub() // the first start made the database and the LazyRex session

	// The legacy session: an older hub's session of the home plugin, with a live test renderer.
	const id = "legacy-home"
	var s struct {
		ID      string `json:"session_id"`
		Windows []struct {
			Window string   `json:"window_id"`
			Blocks []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	cmd := []string{filepath.Join(l.dir, "testview.sh"), "-view-id", id, "-server", l.server, "harnessfix", "/home"}
	if err := l.r.Call(ctx, "session.create", map[string]any{"label": "harnessfix", "initial_windows": []any{map[string]any{
		"window_label": "Fixture home", "layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell",
			"options": map[string]any{"command": cmd, "login": false}}}}}}, &s); err != nil {
		t.Fatal(err)
	}
	sess, win, block := s.ID, s.Windows[0].Window, s.Windows[0].Blocks[0]
	if err := l.r.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	var proc struct {
		Child *struct {
			PID int `json:"pid"`
		} `json:"child"`
	}
	if err := l.r.BlockCall(ctx, "com.superlogical.terminal.process", sess, block, nil, &proc); err != nil || proc.Child == nil {
		t.Fatalf("the renderer pane: %v %+v", err, proc)
	}
	pid := proc.Child.PID
	// Unsaved text: the renderer accepts view.retire and stays until the hold file is gone.
	hold := filepath.Join(l.dir, "testview-hold")
	if err := os.WriteFile(hold, []byte("/home\nretire-wait\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(l.dir, "hub")
	db, err := openDB(filepath.Join(state, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ui_sessions (plugin, session_id) VALUES ('harnessfix', ?)`, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, session_id, window_id, block_id, child_pid, child_start)
		VALUES (?, 'harnessfix', 'home', '/home', 'open', ?, ?, ?, ?, ?)`, id, sess, win, block, pid, startTime(pid)); err != nil {
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
	waitFor := func(what string, d time.Duration, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(d); !ok(); time.Sleep(200 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	// pane is the pane's state from a definite answer: "present", "absent" (the session lists no such
	// block, or the session is gone), or "unknown" (any other error, for example a timeout). An unknown
	// answer never counts as absence: the checks below ask again.
	pane := func() string {
		var out struct {
			Blocks []struct {
				ID string `json:"block_id"`
			} `json:"blocks"`
		}
		if err := l.r.Call(ctx, "session.list_blocks", map[string]any{"session_id": sess}, &out); err != nil {
			if sessionGone(err) {
				return "absent"
			}
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
	// paneIs waits a short time for a definite answer equal to want; an answer of the other kind fails.
	paneIs := func(want string) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			switch got := pane(); {
			case got == want:
				return
			case got != "unknown":
				t.Fatalf("the pane is %s, want %s", got, want)
			case time.Now().After(end):
				t.Fatalf("no definite answer about the pane, want %s", want)
			}
		}
	}

	if err := o.startHub(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitFor("the renderer attaches and the hub asks view.retire", 20*time.Second, func() bool {
		_, att := row()
		return att != "" && events("view_retire") >= 1
	})
	time.Sleep(2 * time.Second) // passes run; the waiting view stays
	if st, _ := row(); st != "open" {
		t.Fatalf("a view with unsaved text did not wait: status %s", st)
	}
	paneIs("present")
	asked := events("view_retire")

	stopHub()
	if err := o.startHub(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitFor("the new hub asks view.retire again", 30*time.Second, func() bool { return events("view_retire") > asked })
	if st, _ := row(); st != "open" {
		t.Fatalf("after the restart: status %s", st)
	}
	paneIs("present")

	os.Remove(hold) // the text is saved: the renderer leaves
	waitFor("the row to close", 20*time.Second, func() bool { st, _ := row(); return st == "closed" })
	waitFor("the pane to go (a definite answer)", 20*time.Second, func() bool { return pane() == "absent" })
	time.Sleep(2 * time.Second)
	if n := events("view_retired") + events("view_closed"); n != 1 {
		t.Fatalf("completions: %d view_retired + view_closed, want exactly 1", n)
	}
	if st, _ := row(); st != "closed" {
		t.Fatalf("status %s", st)
	}
}
