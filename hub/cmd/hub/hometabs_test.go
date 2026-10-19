package main

import (
	"context"
	"testing"
	"time"
)

func testHomeHub(t *testing.T) *Hub {
	t.Helper()
	h := testUIHub(t)
	if _, err := h.db.ExecContext(context.Background(), homeSchema); err != nil {
		t.Fatal(err)
	}
	return h
}

// A view in the session that replaced a tab's home is a home view, also after several
// replacements: a close there is a user close, not a legacy retirement (Codex review of b3d5465).
func TestHomeOfFollowsReplacements(t *testing.T) {
	h := testHomeHub(t)
	ctx := context.Background()
	h.db.ExecContext(ctx, `INSERT INTO ui_tabs (plugin, tab, title, route, pos, session_id) VALUES ('messaging', 'thread/a', 'A', '/thread/a', 0, 'session:a')`)
	h.db.ExecContext(ctx, `INSERT INTO ui_homes (session_id, label, moved_to) VALUES ('session:a', 'work', 'session:b'), ('session:b', 'work', 'session:c'), ('session:c', 'work', '')`)
	for sess, want := range map[string]bool{"session:a": true, "session:c": true, "session:b": false, "session:x": false} {
		got, err := h.homeOf(ctx, "messaging", "thread/a", sess)
		if err != nil || got != want {
			t.Errorf("homeOf %s = %v, %v; want %v", sess, got, err, want)
		}
	}
	r, err := h.homeRedirects(ctx)
	if err != nil || r["session:a"] != "session:c" || r["session:b"] != "session:c" || len(r) != 2 {
		t.Fatalf("redirects %v %v: each home goes to the last replacement", r, err)
	}
}

func TestHomeReplacementCycleIsAnError(t *testing.T) {
	h := testHomeHub(t)
	ctx := context.Background()
	h.db.ExecContext(ctx, `INSERT INTO ui_tabs (plugin, tab, title, route, pos, session_id) VALUES ('messaging', 'thread/a', 'A', '/thread/a', 0, 'session:a')`)
	h.db.ExecContext(ctx, `INSERT INTO ui_homes (session_id, moved_to) VALUES ('session:a', 'session:b'), ('session:b', 'session:a')`)
	if _, err := h.homeRedirects(ctx); err == nil {
		t.Error("homeRedirects accepted a cycle")
	}
	if _, err := h.homeOf(ctx, "messaging", "thread/a", "session:z"); err == nil {
		t.Error("homeOf accepted a cycle")
	}
	if h.notHome(ctx, "messaging", "thread/a", "session:z") {
		t.Error("an uncertain home must not retire a view as legacy")
	}
}

func TestHomeReadErrorsFail(t *testing.T) {
	h := testHomeHub(t)
	ctx := context.Background()
	h.db.ExecContext(ctx, `INSERT INTO ui_tabs (plugin, tab, title, route, pos, session_id) VALUES ('messaging', 'thread/a', 'A', '/thread/a', 0, 'session:a')`)
	h.db.ExecContext(ctx, `DROP TABLE ui_homes`)
	if _, err := h.homeRedirects(ctx); err == nil {
		t.Error("homeRedirects hid a read error")
	}
	if _, err := h.resolveHome(ctx, "session:a", true, new(*sessionList)); err == nil {
		t.Error("resolveHome hid a read error")
	}
	if h.notHome(ctx, "messaging", "thread/a", "session:b") {
		t.Error("a read error must not retire a view as legacy")
	}
}

// A session.list answer asked for before a rename event does not overwrite the event's name, also
// when the event comes before the home has a row (its first lookup; Codex review).
func TestOlderListDoesNotOverwriteARenameEvent(t *testing.T) {
	h := testHomeHub(t)
	ctx := context.Background()
	h.ui.sess = map[string]string{}
	asked := time.Now()                            // the first lookup asks for session.list
	h.sessionRenamed(ctx, "session:a", "new name") // the event arrives; no row exists yet
	h.sessionRenamed(ctx, "session:a", "newer")    // a second event, possibly at the same clock reading, still applies
	if err := h.rememberHomeLabel(ctx, "session:a", "old name", asked, false); err != nil {
		t.Fatal(err)
	}
	var label string
	h.db.QueryRowContext(ctx, `SELECT label FROM ui_homes WHERE session_id = 'session:a'`).Scan(&label)
	if label != "newer" {
		t.Fatalf("label %q: an older list answer replaced the events", label)
	}
	if err := h.rememberHomeLabel(ctx, "session:a", "newest", time.Now().Add(time.Millisecond), false); err != nil {
		t.Fatal(err)
	}
	h.db.QueryRowContext(ctx, `SELECT label FROM ui_homes WHERE session_id = 'session:a'`).Scan(&label)
	if label != "newest" {
		t.Fatalf("label %q: a newer list answer must apply", label)
	}
}
