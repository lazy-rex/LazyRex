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

// uiTestHub is a hub with the UI schema and no Rex client: a Rex call panics.
func uiTestHub(t *testing.T) (*Hub, string) {
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

func TestLegacyViewIsAnySessionButTheLazyRexSession(t *testing.T) {
	h, _ := uiTestHub(t)
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
	h, events := uiTestHub(t)
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

// A new hub.db gets every UI column from the CREATE statements that uiStart runs, in its order: no
// column upgrade adds them.
func TestNewDatabaseHasTheUIColumns(t *testing.T) {
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(uiSchema); err != nil {
		t.Fatal(err)
	}
	if err := formsStart(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{ensuredSchema, panelSchema, homeSchema} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for table, cols := range map[string][]string{
		"ui_tabs":         {"session_id", "revision", "closed_revision", "status", "layout"},
		"ui_homes":        {"label_at"},
		"ui_replacements": {"kept", "return_of"},
		"views":           {"context", "layout", "attach_state"},
	} {
		for _, c := range cols {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, c).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Errorf("a new hub.db has no %s.%s", table, c)
			}
		}
	}
}
