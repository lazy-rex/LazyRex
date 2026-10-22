package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestViewCommandPassesRealServerOnlyOnRealRuns(t *testing.T) {
	h := &Hub{}
	h.cfg.View = "/bin/view"
	// The hub passes its own endpoint, as loaded from hub.json: a Linux pane has no REX_SERVER, and
	// the renderer never guesses one (codex-24's review of 598b8e5).
	h.cfg.Server = "unix:///tmp/rexlab-ui/rex.sock"
	if got := h.viewCommand("v1", "messaging", "/"); !reflect.DeepEqual(got, []string{"/bin/view", "-view-id", "v1", "-server", h.cfg.Server, "messaging", "/"}) {
		t.Fatalf("lab argv: %v", got)
	}
	h.cfg.Real = true
	if got := h.viewCommand("v1", "messaging", "/"); !reflect.DeepEqual(got, []string{"/bin/view", "-view-id", "v1", "-server", h.cfg.Server, "-real-server", "messaging", "/"}) {
		t.Fatalf("real argv: %v", got)
	}
}

// A plugin declares its tabs again when only a title changes (an unread count). The new
// declaration must keep the user's close (suppressed) and the renderer crash count.
func TestDeclaringTabsAgainKeepsSuppressionAndCrashes(t *testing.T) {
	db, err := openDB(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	h := &Hub{db: db}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, uiSchema); err != nil {
		t.Fatal(err)
	}
	// No worker runs, so kickUI only signals the channel.
	h.ui = &uiState{kick: make(chan struct{}, 1)}
	if _, err := db.ExecContext(ctx, `INSERT INTO ui_tabs (plugin, tab, title, route, pos, suppressed, crashes) VALUES ('messaging', 'thread/team', 'Team', '/thread/team', 0, 1, 2)`); err != nil {
		t.Fatal(err)
	}
	tabs := []any{map[string]any{"key": "thread/team", "title": "Team (3)", "route": "/thread/team"}}
	if _, err := h.uiTabs(ctx, "messaging", map[string]any{"tabs": tabs}); err != nil {
		t.Fatal(err)
	}
	var title string
	var suppressed bool
	var crashes int
	if err := db.QueryRowContext(ctx, `SELECT title, suppressed, crashes FROM ui_tabs WHERE plugin = 'messaging' AND tab = 'thread/team'`).Scan(&title, &suppressed, &crashes); err != nil {
		t.Fatal(err)
	}
	if title != "Team (3)" || !suppressed || crashes != 2 {
		t.Fatalf("got title %q suppressed %v crashes %d", title, suppressed, crashes)
	}
}

func TestNewRevisionReopensAClosedTab(t *testing.T) {
	h := testUIHub(t)
	ctx := context.Background()
	declare := func(rev string) {
		t.Helper()
		if _, err := h.uiTabs(ctx, "messaging", map[string]any{"tabs": []any{
			map[string]any{"key": "thread/a", "title": "A", "route": "/thread/a", "session_id": "session:home", "revision": rev}}}); err != nil {
			t.Fatal(err)
		}
	}
	state := func() (suppressed bool, session string) {
		t.Helper()
		h.db.QueryRowContext(ctx, `SELECT suppressed, session_id FROM ui_tabs WHERE plugin = 'messaging' AND tab = 'thread/a'`).Scan(&suppressed, &session)
		return
	}
	declare("m1")
	h.db.ExecContext(ctx, `UPDATE ui_tabs SET suppressed = 1, closed_revision = revision WHERE plugin = 'messaging' AND tab = 'thread/a'`) // a manual close
	declare("m1")
	if s, home := state(); !s || home != "session:home" {
		t.Fatalf("the same revision keeps the tab closed: suppressed=%v home=%q", s, home)
	}
	declare("m2")
	if s, _ := state(); s {
		t.Fatal("a new revision opens the closed tab again")
	}
}

func testUIHub(t *testing.T) *Hub {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(dir + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), uiSchema); err != nil {
		t.Fatal(err)
	}
	ev, err := openEvents(dir + "/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return &Hub{db: db, ev: ev, ui: &uiState{kick: make(chan struct{}, 1)}}
}

func TestPreRevisionCloseTakesTheFirstRevisionAsBaseline(t *testing.T) {
	h := testUIHub(t)
	ctx := context.Background()
	// A tab the user closed before revisions existed.
	h.db.ExecContext(ctx, `INSERT INTO ui_tabs (plugin, tab, title, route, pos, suppressed) VALUES ('messaging', 'thread/a', 'A', '/thread/a', 0, 1)`)
	declare := func(rev string) bool {
		t.Helper()
		if _, err := h.uiTabs(ctx, "messaging", map[string]any{"tabs": []any{map[string]any{"key": "thread/a", "title": "A", "route": "/thread/a", "revision": rev}}}); err != nil {
			t.Fatal(err)
		}
		var suppressed bool
		h.db.QueryRowContext(ctx, `SELECT suppressed FROM ui_tabs WHERE plugin = 'messaging' AND tab = 'thread/a'`).Scan(&suppressed)
		return suppressed
	}
	if !declare("m5") {
		t.Fatal("the first revision is the baseline: the tab stays closed")
	}
	if !declare("m5") {
		t.Fatal("same revision: still closed")
	}
	if declare("m6") {
		t.Fatal("a new revision reopens it")
	}
}

func TestEmptyThreadClosedReopensOnFirstMessage(t *testing.T) {
	h := testUIHub(t)
	ctx := context.Background()
	declare := func(rev string) bool {
		t.Helper()
		if _, err := h.uiTabs(ctx, "messaging", map[string]any{"tabs": []any{map[string]any{"key": "thread/new", "title": "New", "route": "/thread/new", "revision": rev}}}); err != nil {
			t.Fatal(err)
		}
		var suppressed bool
		h.db.QueryRowContext(ctx, `SELECT suppressed FROM ui_tabs WHERE plugin = 'messaging' AND tab = 'thread/new'`).Scan(&suppressed)
		return suppressed
	}
	declare("") // created, no messages yet
	h.db.ExecContext(ctx, `UPDATE ui_tabs SET suppressed = 1, closed_revision = revision WHERE plugin = 'messaging' AND tab = 'thread/new'`)
	if !declare("") {
		t.Fatal("still empty: stays closed")
	}
	if declare("m1") {
		t.Fatal("the first message reopens an empty thread the user closed")
	}
}

// The home plugin declares no tabs: a non-empty list is refused and stores nothing, and the empty
// list is valid.
// Another plugin declares tabs as before.
func TestHomePluginDeclaresNoTabs(t *testing.T) {
	h := testUIHub(t)
	ctx := context.Background()
	one := map[string]any{"tabs": []any{map[string]any{"key": "home", "title": "Home", "route": "/"}}}
	if _, err := h.uiTabs(ctx, "lazyrex", one); err == nil || !strings.Contains(err.Error(), "the home plugin declares no tabs") {
		t.Fatalf("home tabs: %v", err)
	}
	var n int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_tabs WHERE plugin = 'lazyrex'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused list stored %d home tabs (%v)", n, err)
	}
	if _, err := h.uiTabs(ctx, "lazyrex", map[string]any{"tabs": []any{}}); err != nil {
		t.Fatalf("home, no tabs: %v", err)
	}
	if _, err := h.uiTabs(ctx, "messaging", one); err != nil {
		t.Fatalf("another plugin: %v", err)
	}
}
