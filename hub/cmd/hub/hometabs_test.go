package main

import (
	"context"
	"os"
	"strings"
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

// Each replaced home goes to the last replacement (Codex review of b3d5465).
func TestHomeRedirectsFollowReplacements(t *testing.T) {
	h := testHomeHub(t)
	ctx := context.Background()
	h.db.ExecContext(ctx, `INSERT INTO ui_tabs (plugin, tab, title, route, pos, session_id) VALUES ('messaging', 'thread/a', 'A', '/thread/a', 0, 'session:a')`)
	h.db.ExecContext(ctx, `INSERT INTO ui_homes (session_id, label, moved_to) VALUES ('session:a', 'work', 'session:b'), ('session:b', 'work', 'session:c'), ('session:c', 'work', '')`)
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

// A tab home that is not a Rex session ID (session:<uuid>) can never name a session: Rex refuses
// its attach with code internal, which is not a definite "not found". Before the fix that error
// failed every reconcile pass, so no view opened (a Messaging fixture stored session:kv-home).
// The home counts as gone: its tabs go to the app session, the other tabs go on, and the hub
// reports the home once. The hub makes no Rex call for it (h.rex is nil here).
func TestInvalidHomeGoesToTheAppSession(t *testing.T) {
	h := testHomeHub(t)
	ctx := context.Background()
	tabs := []tab{{Key: "thread/a", Session: "session:kv-home"}, {Key: "thread/b"}, {Key: "thread/c", Session: "session:kv-home", Suppressed: true}}
	for pass := 0; pass < 2; pass++ {
		groups, err := h.tabGroups(ctx, "session:app", tabs, nil)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if len(groups) != 1 || groups[0].sess != "session:app" || len(groups[0].tabs) != 3 {
			t.Fatalf("pass %d: groups %+v, want every tab in the app session", pass, groups)
		}
	}
	b, _ := os.ReadFile(h.ev.f.Name())
	if n := strings.Count(string(b), `"event":"ui_home_invalid"`); n != 1 {
		t.Fatalf("%d ui_home_invalid events, want 1:\n%s", n, b)
	}
	for _, id := range []string{"session:01a15601-5fca-744d-9590-fe16fd20a936", "session:01A15601-5FCA-744D-9590-FE16FD20A936"} {
		if !rexSessionID(id) {
			t.Errorf("%s is a Rex session ID", id)
		}
	}
	for _, id := range []string{"session:kv-home", "session:", "01a15601-5fca-744d-9590-fe16fd20a936", "session:01a15601-5fca-744d-9590-fe16fd20a93", "session:01a15601x5fca-744d-9590-fe16fd20a936", "session:01a15601-5fca-744d-9590-fe16fd20a93g"} {
		if rexSessionID(id) {
			t.Errorf("%s is not a Rex session ID", id)
		}
	}
}
