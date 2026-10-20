package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The schema of app mode before its removal (the tables that removeAppModeState drops).
const oldAppModeSchema = `
CREATE TABLE ui_cycles (cycle TEXT PRIMARY KEY, seq INTEGER NOT NULL, phase TEXT NOT NULL,
  origin TEXT NOT NULL DEFAULT '', problems TEXT NOT NULL DEFAULT '[]', windows INTEGER NOT NULL DEFAULT 0,
  blocks INTEGER NOT NULL DEFAULT 0, restored_windows INTEGER NOT NULL DEFAULT 0, restored_blocks INTEGER NOT NULL DEFAULT 0,
  created TEXT NOT NULL, updated TEXT NOT NULL);
CREATE TABLE ui_hidden (cycle TEXT NOT NULL, session_id TEXT NOT NULL, spos INTEGER NOT NULL,
  wpos INTEGER NOT NULL, window TEXT NOT NULL, active INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'hidden',
  rebuilt_window TEXT NOT NULL DEFAULT '', PRIMARY KEY (cycle, session_id, wpos));
CREATE TABLE ui_exit_keys (key TEXT PRIMARY KEY, cycle TEXT NOT NULL);
CREATE TABLE ui_apps (plugin TEXT PRIMARY KEY, name TEXT NOT NULL);
`

// appModeHub is a hub with the UI schema and no Rex client: a Rex call panics.
func appModeHub(t *testing.T) (*Hub, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(uiSchema); err != nil {
		t.Fatal(err)
	}
	ev, err := openEvents(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ev.f.Close() })
	return &Hub{db: db, ev: ev, ui: &uiState{kick: make(chan struct{}, 1), sess: map[string]string{}}}, filepath.Join(dir, "events.jsonl")
}

// eventsNamed returns the events named name, in order.
func eventsNamed(t *testing.T, path, name string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(l), &e) == nil && e["event"] == name {
			out = append(out, e)
		}
	}
	return out
}

func tableExists(t *testing.T, h *Hub, name string) bool {
	t.Helper()
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// rowsOf returns every row of a query as text, for a before and after comparison.
func rowsOf(t *testing.T, h *Hub, q string) []string {
	t.Helper()
	rows, err := h.db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(vals)
		out = append(out, string(b))
	}
	return out
}

func TestRemoveAppModeState(t *testing.T) {
	h, events := appModeHub(t)
	ctx := context.Background()
	if _, err := h.db.Exec(oldAppModeSchema); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		// One cycle in each phase of the old build, and a refused one.
		`INSERT INTO ui_cycles (cycle, seq, phase, created, updated) VALUES ('c1', 1, 'complete', 't', 't'), ('c2', 2, 'refused', 't', 't'),
		  ('c3', 3, 'capturing', 't', 't'), ('c4', 4, 'hiding', 't', 't'), ('c5', 5, 'active', 't', 't'), ('c6', 6, 'restoring', 't', 't')`,
		`INSERT INTO ui_hidden (cycle, session_id, spos, wpos, window, active) VALUES ('c5', 'session:user', 0, 0, '{}', 1), ('c5', 'session:user', 0, 1, '{}', 0)`,
		`INSERT INTO ui_exit_keys VALUES ('exit-1', 'c1')`,
		`INSERT INTO ui_apps VALUES ('messaging', 'Messages')`,
		`INSERT INTO ui_restores VALUES ('n-hidden', 'hidden:c5:session:user:0', 't'), ('n-view', 'view-1', 't')`,
		`INSERT INTO ui_state VALUES ('active', 'messaging'), ('nav', 'messaging/inbox')`,
		`INSERT INTO ui_sessions VALUES ('*', 'session:home'), ('app:messaging', 'session:app'), ('messaging', 'session:old')`,
		`INSERT INTO views (view_id, plugin, tab, route, status, session_id) VALUES ('view-1', 'messaging', 'inbox', '/', 'open', 'session:app'),
		  ('view-2', 'messaging', 'thread/a', '/thread/a', 'sending', 'session:old'), ('view-3', 'home', 'home', '/', 'open', 'session:home')`,
		`INSERT INTO ui_tabs (plugin, tab, title, route, pos) VALUES ('messaging', 'inbox', 'Inbox', '/', 0)`,
	} {
		if _, err := h.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	kept := func() [][]string {
		return [][]string{rowsOf(t, h, `SELECT * FROM ui_sessions ORDER BY plugin`), rowsOf(t, h, `SELECT * FROM views ORDER BY view_id`),
			rowsOf(t, h, `SELECT * FROM ui_tabs`)}
	}
	before := kept()

	if err := h.removeAppModeState(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range appModeTables {
		if tableExists(t, h, name) {
			t.Errorf("table %s still exists", name)
		}
	}
	if got := rowsOf(t, h, `SELECT nonce FROM ui_restores`); !reflect.DeepEqual(got, []string{`["n-view"]`}) {
		t.Errorf("ui_restores: %v, want only the view restore", got)
	}
	if got := rowsOf(t, h, `SELECT key FROM ui_state`); !reflect.DeepEqual(got, []string{`["nav"]`}) {
		t.Errorf("ui_state: %v, want only nav", got)
	}
	if after := kept(); !reflect.DeepEqual(after, before) {
		t.Errorf("sessions, views or tabs changed:\nbefore %v\nafter  %v", before, after)
	}
	got := eventsNamed(t, events, "app_mode_state_removed")
	if len(got) != 1 {
		t.Fatalf("events: %v, want one", got)
	}
	e := got[0]
	delete(e, "at")
	delete(e, "event")
	delete(e, "generation")
	want := map[string]any{
		"cycles":    map[string]any{"complete": 1.0, "refused": 1.0, "capturing": 1.0, "hiding": 1.0, "active": 1.0, "restoring": 1.0},
		"ui_hidden": 2.0, "ui_exit_keys": 1.0, "ui_apps": 1.0, "restore_placeholders": 1.0, "active_state": 1.0,
		"tables": []any{"ui_cycles", "ui_hidden", "ui_exit_keys", "ui_apps"},
	}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("event %v\nwant  %v", e, want)
	}

	// A second start: nothing to remove, no event.
	before = kept()
	if err := h.removeAppModeState(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(eventsNamed(t, events, "app_mode_state_removed")); n != 1 {
		t.Errorf("a second start emitted again: %d events", n)
	}
	if after := kept(); !reflect.DeepEqual(after, before) {
		t.Error("a second start changed rows")
	}
}

func TestRemoveAppModeStateOnANewDatabase(t *testing.T) {
	h, events := appModeHub(t)
	if err := h.removeAppModeState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(eventsNamed(t, events, "app_mode_state_removed")); n != 0 {
		t.Errorf("%d events on a database without app mode", n)
	}
}

func TestLegacyViewIsAnySessionButTheLazyRexSession(t *testing.T) {
	h, _ := appModeHub(t)
	h.ui.sess = map[string]string{"session:home": uiSessionKey, "session:app": "app:messaging", "session:old": "messaging"}
	for sess, legacy := range map[string]bool{"session:home": false, "session:app": true, "session:old": true, "session:unknown": true} {
		if got := h.legacyView(sess); got != legacy {
			t.Errorf("legacyView(%s) = %v, want %v", sess, got, legacy)
		}
	}
}

// Legacy session cleanup (settleLegacySession) against a real Rex lab: SPAWN_LAB_SOCKET.
func TestSettleLegacySessionLab(t *testing.T) {
	r, ctx := labRex(t)
	h, events := appModeHub(t)
	h.rex = r
	newSession := func(t *testing.T, pane bool) string {
		t.Helper()
		req := map[string]any{"label": "legacy-lab-test"}
		if pane {
			req["initial_windows"] = []any{map[string]any{"window_label": "user", "layout": map[string]any{"block": map[string]any{
				"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"command": []string{"/bin/sleep", "600"}, "login": false}}}}}
		}
		var s struct {
			ID string `json:"session_id"`
		}
		if err := r.Call(ctx, "session.create", req, &s); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Call(context.Background(), "session.destroy", map[string]any{"session_id": s.ID}, nil) })
		if err := r.Call(ctx, "session.attach", map[string]any{"session_id": s.ID, "event_scope": "session"}, nil); err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	record := func(t *testing.T, key, sess string) {
		t.Helper()
		if _, err := h.db.Exec(`INSERT INTO ui_sessions VALUES (?, ?)`, key, sess); err != nil {
			t.Fatal(err)
		}
		h.ui.mu.Lock()
		h.ui.sess[sess] = key
		h.ui.mu.Unlock()
	}
	viewRow := func(t *testing.T, id, plugin, status, sess string) {
		t.Helper()
		if _, err := h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, session_id) VALUES (?, ?, 'inbox', '/', ?, ?)`, id, plugin, status, sess); err != nil {
			t.Fatal(err)
		}
	}
	legacyRows := func(sess string) int {
		var n int
		h.db.QueryRow(`SELECT COUNT(*) FROM ui_sessions WHERE session_id = ?`, sess).Scan(&n)
		return n
	}
	exists := func(sess string) bool {
		_, err := h.blocksIn(ctx, sess)
		if err != nil && !sessionGone(err) {
			t.Fatalf("list_blocks %s: %v", sess, err)
		}
		return err == nil
	}
	gone := func(sess, reason string) bool {
		for _, e := range eventsNamed(t, events, "ui_session_gone") {
			if e["session_id"] == sess && e["reason"] == reason {
				return true
			}
		}
		return false
	}

	t.Run("a session with a view row that is not closed waits; then it is destroyed", func(t *testing.T) {
		sess := newSession(t, false)
		record(t, "app:messaging", sess)
		viewRow(t, "mixed-closed", "messaging", "closed", sess)
		viewRow(t, "mixed-sending", "tunnels", "sending", sess) // another plugin's row, not resolved yet
		viewRow(t, "mixed-exited", "messaging", "exited", sess)
		h.settleLegacySession(ctx, sess)
		var statuses []string
		for _, row := range rowsOf(t, h, `SELECT status FROM views WHERE view_id LIKE 'mixed-%' ORDER BY view_id`) {
			statuses = append(statuses, row)
		}
		if !reflect.DeepEqual(statuses, []string{`["closed"]`, `["exited"]`, `["sending"]`}) {
			t.Fatalf("view rows changed: %v", statuses)
		}
		if legacyRows(sess) != 1 || !exists(sess) {
			t.Fatal("the session was cleaned up while a view row was not closed")
		}
		h.db.Exec(`UPDATE views SET status = 'closed' WHERE view_id IN ('mixed-sending', 'mixed-exited')`)
		h.settleLegacySession(ctx, sess)
		if legacyRows(sess) != 0 || exists(sess) || !gone(sess, "legacy retired") {
			t.Fatalf("rows %d, exists %v: want the session destroyed and forgotten", legacyRows(sess), exists(sess))
		}
	})

	t.Run("a session with user panes is released and kept", func(t *testing.T) {
		sess := newSession(t, true)
		record(t, "app:messaging", sess)
		viewRow(t, "pane-closed", "messaging", "closed", sess)
		h.settleLegacySession(ctx, sess)
		if legacyRows(sess) != 0 {
			t.Fatal("the legacy row stays")
		}
		blocks, err := h.blocksIn(ctx, sess)
		if err != nil || len(blocks) != 1 {
			t.Fatalf("the user pane: %v %v", blocks, err)
		}
		released := eventsNamed(t, events, "legacy_session_released")
		if len(released) == 0 || released[len(released)-1]["session_id"] != sess || released[len(released)-1]["blocks"] != 1.0 {
			t.Fatalf("legacy_session_released: %v", released)
		}
		h.ui.mu.Lock()
		_, managed := h.ui.sess[sess]
		h.ui.mu.Unlock()
		if managed {
			t.Error("the hub still routes events of a released session")
		}
	})

	t.Run("a failed destroy keeps the record; the next settle destroys the session", func(t *testing.T) {
		sess := newSession(t, false)
		record(t, "messaging", sess)
		real := legacyDestroy
		legacyDestroy = func(context.Context, *Hub, string) error { return errors.New("injected destroy failure") }
		h.settleLegacySession(ctx, sess)
		legacyDestroy = real
		if legacyRows(sess) != 1 || !exists(sess) {
			t.Fatal("a failed destroy lost the record or the session")
		}
		failed := eventsNamed(t, events, "legacy_cleanup_failed")
		if len(failed) == 0 || failed[len(failed)-1]["session_id"] != sess || failed[len(failed)-1]["call"] != "session.destroy" {
			t.Fatalf("legacy_cleanup_failed: %v", failed)
		}
		h.settleLegacySession(ctx, sess)
		if legacyRows(sess) != 0 || exists(sess) {
			t.Fatal("the retry did not destroy the session")
		}
	})

	t.Run("a session that Rex no longer has is forgotten", func(t *testing.T) {
		sess := newSession(t, false)
		record(t, "app:tunnels", sess)
		if err := r.Call(ctx, "session.destroy", map[string]any{"session_id": sess}, nil); err != nil {
			t.Fatal(err)
		}
		h.settleLegacySession(ctx, sess)
		if legacyRows(sess) != 0 || !gone(sess, "legacy gone") {
			t.Fatal("the record of a gone session stays")
		}
	})

	t.Run("the LazyRex session is never destroyed", func(t *testing.T) {
		sess := newSession(t, false)
		record(t, "messaging", sess)
		if _, err := h.db.Exec(`INSERT INTO ui_sessions VALUES (?, ?)`, uiSessionKey, sess); err != nil {
			t.Fatal(err)
		}
		h.settleLegacySession(ctx, sess)
		var keys []string
		for _, row := range rowsOf(t, h, `SELECT plugin FROM ui_sessions WHERE session_id = '`+sess+`'`) {
			keys = append(keys, row)
		}
		if !reflect.DeepEqual(keys, []string{`["*"]`}) || !exists(sess) {
			t.Fatalf("rows %v, exists %v: want only the LazyRex row and the session", keys, exists(sess))
		}
		h.db.Exec(`DELETE FROM ui_sessions WHERE session_id = ?`, sess)
	})
}

// Dispatch from a registered plugin: tests/harness/tests/removed_actions.lua.
func TestAppModeActionsAreRemoved(t *testing.T) {
	for _, action := range []string{"hub.ui.exit", "hub.ui.mode"} {
		for _, name := range hubActionNames {
			if name == action {
				t.Errorf("%s is still a hub client action", action)
			}
		}
		for _, tool := range hubToolActions {
			if tool["name"] == action {
				t.Errorf("%s is still a tool action", action)
			}
		}
	}
}
