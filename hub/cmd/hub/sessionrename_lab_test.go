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

// The user's name for the hub's LazyRex session is kept (hometabs.go sessionRenamed and
// mayLabel): a rename while the hub runs, across a restart, and a rename while the hub is
// stopped. The harness cannot act between a stop and a start, so this is a Go lab test.
func TestSessionRenameKeptLab(t *testing.T) {
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
	startHub := func() {
		t.Helper()
		if err := o.startHub(ctx, l); err != nil {
			t.Fatal(err)
		}
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
	waitFor := func(what string, d time.Duration, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(d); !ok(); time.Sleep(200 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	state := filepath.Join(l.dir, "hub")
	query := func(q string, args ...any) (s string) {
		t.Helper()
		db, err := openDB(filepath.Join(state, "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.QueryRow(q, args...).Scan(&s)
		return s
	}
	dbExec := func(q string, args ...any) {
		t.Helper()
		db, err := openDB(filepath.Join(state, "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	label := func(sess string) string {
		var out sessionList
		if err := l.r.Call(ctx, "session.list", map[string]any{}, &out); err != nil {
			t.Logf("session.list: %v", err)
			return ""
		}
		for _, s := range out.Sessions {
			if s.SessionID == sess {
				return s.Label
			}
		}
		return ""
	}
	// renamed is the session_renamed events of sess with this label; each one's "while" field.
	renamed := func(sess, lbl string) []string {
		b, _ := os.ReadFile(filepath.Join(state, "events.jsonl"))
		var whiles []string
		for _, line := range strings.Split(string(b), "\n") {
			var e map[string]any
			if json.Unmarshal([]byte(line), &e) == nil && e["event"] == "session_renamed" && e["session_id"] == sess && e["label"] == lbl {
				w, _ := e["while"].(string)
				whiles = append(whiles, w)
			}
		}
		return whiles
	}
	// rename is the user's rename, from the test's own Rex client.
	rename := func(sess, lbl string) {
		t.Helper()
		if err := l.r.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := l.r.Call(ctx, "session.set_label", map[string]any{"session_id": sess, "label": lbl}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// stays checks that the session keeps lbl while the hub's passes run.
	stays := func(sess, lbl, when string) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			if got := label(sess); got != lbl {
				t.Fatalf("%s: the session is labeled %q, want %q", when, got, lbl)
			}
		}
	}
	appSession := func() string {
		return query(`SELECT session_id FROM ui_sessions WHERE plugin = ?`, uiSessionKey)
	}

	// 1. The hub makes its session with its own label.
	startHub()
	var sess string
	waitFor("the hub's session", 30*time.Second, func() bool { sess = appSession(); return sess != "" })
	waitFor("the hub's label on it", 30*time.Second, func() bool { return label(sess) == uiSessionLabel })

	// 2. The user renames it while the hub runs: the hub records the name as the user's.
	const a, b = "My LazyRex A", "Renamed while stopped B"
	rename(sess, a)
	waitFor("session_renamed with the user's name", 10*time.Second, func() bool { return len(renamed(sess, a)) == 1 })
	if w := renamed(sess, a)[0]; w != "" {
		t.Fatalf("the rename while the hub ran is recorded as %q, want no while", w)
	}
	if got := query(`SELECT user_label FROM ui_session_names WHERE session_id = ?`, sess); got != a {
		t.Fatalf("user_label %q, want %q", got, a)
	}

	// 3. After a restart, the hub keeps the user's name.
	stopHub()
	startHub()
	waitFor("the restarted hub's session", 30*time.Second, func() bool { return appSession() == sess })
	stays(sess, a, "after the restart") // the start pass asks mayLabel at once (ui.go appSession)

	// 4. The user renames it while the hub is stopped. As the old lab script did, the database first
	// says the hub labeled the session last and holds no user name, so only mayLabel's comparison
	// of the current label with the hub's last label can keep the new name.
	stopHub()
	dbExec(`INSERT INTO ui_session_names (session_id, hub_label, user_label) VALUES (?, ?, '')
		ON CONFLICT (session_id) DO UPDATE SET hub_label = excluded.hub_label, user_label = ''`, sess, uiSessionLabel)
	rename(sess, b)

	// 5. After the start, the hub keeps that name too, and records it as the user's.
	startHub()
	stays(sess, b, "after the start") // the start pass decides before this check ends
	if w := renamed(sess, b); len(w) != 1 || w[0] != "hub stopped" {
		t.Fatalf("session_renamed of the rename while stopped: %q, want one with while = hub stopped", w)
	}
	if got := query(`SELECT user_label FROM ui_session_names WHERE session_id = ?`, sess); got != b {
		t.Fatalf("user_label %q, want %q", got, b)
	}
}
