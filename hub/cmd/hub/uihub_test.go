package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestLegacyViewIsAnySessionButTheLazyRexSession(t *testing.T) {
	h, _ := uiTestHub(t)
	h.ui.sess = map[string]string{"session:home": uiSessionKey, "session:app": "app:messaging", "session:old": "messaging"}
	for sess, legacy := range map[string]bool{"session:home": false, "session:app": true, "session:old": true, "session:unknown": true} {
		if got := h.legacyView(sess); got != legacy {
			t.Errorf("legacyView(%s) = %v, want %v", sess, got, legacy)
		}
	}
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
		"ui_tabs":         {"session_id", "revision", "closed_revision", "status"},
		"ui_homes":        {"label_at"},
		"ui_replacements": {"kept", "return_of"},
		"views":           {"context", "attach_state"},
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
