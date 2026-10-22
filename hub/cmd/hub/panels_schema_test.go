package main

import (
	"slices"
	"testing"
)

func replacementColumns(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info('ui_replacements') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// A fresh database gets every ui_replacements column from panelSchema alone, kept included. A
// database of an older hub (no kept, no return_of, then the two ALTERs of uiStart) has the same
// columns.
func TestPanelSchemaColumns(t *testing.T) {
	fresh, _ := workerTestHub(t)
	if _, err := fresh.db.Exec(panelSchema); err != nil {
		t.Fatal(err)
	}
	want := replacementColumns(t, fresh.db)
	for _, c := range []string{"kept", "return_of"} {
		if !slices.Contains(want, c) {
			t.Fatalf("a fresh database has no %s: %v", c, want)
		}
	}
	if _, err := fresh.db.Exec(`INSERT INTO ui_replacements (repl_id, session_id, window_id, new_view, plugin, route, context, state, created)
		VALUES ('r', 's', 'w', 'n', 'p', '/panels/a', '{}', 'confirming', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.db.Exec(`UPDATE ui_replacements SET kept = 1 WHERE repl_id = 'r'`); err != nil {
		t.Fatal(err)
	}
	var kept int
	fresh.db.QueryRow(`SELECT kept FROM ui_replacements WHERE repl_id = 'r'`).Scan(&kept)
	if kept != 1 {
		t.Fatalf("kept %d", kept)
	}

	old, _ := workerTestHub(t)
	for _, q := range []string{
		`CREATE TABLE ui_replacements (repl_id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
			window_id TEXT NOT NULL, old_view TEXT NOT NULL DEFAULT '', new_view TEXT NOT NULL, plugin TEXT NOT NULL,
			route TEXT NOT NULL, context TEXT NOT NULL, focus INTEGER NOT NULL DEFAULT 1, state TEXT NOT NULL,
			next_title TEXT NOT NULL DEFAULT '', created TEXT NOT NULL)`,
		panelSchema,
		`ALTER TABLE ui_replacements ADD COLUMN kept INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE ui_replacements ADD COLUMN return_of TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := old.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if got := replacementColumns(t, old.db); !slices.Equal(got, want) {
		t.Fatalf("an older database has %v, a fresh one %v", got, want)
	}
}
