package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// A hub.db with ui_tabs.rev_known loses the column at open, keeps its rows, and takes an INSERT
// that does not name the column. A second open finds nothing to do.
func TestRetireSchemaDropsRevKnown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE ui_tabs (plugin TEXT NOT NULL, tab TEXT NOT NULL, title TEXT NOT NULL, route TEXT NOT NULL,
		  pos INTEGER NOT NULL, suppressed INTEGER NOT NULL DEFAULT 0, crashes INTEGER NOT NULL DEFAULT 0,
		  session_id TEXT NOT NULL DEFAULT '', revision TEXT NOT NULL DEFAULT '', closed_revision TEXT NOT NULL DEFAULT '',
		  rev_known INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT '', layout TEXT NOT NULL DEFAULT '', PRIMARY KEY (plugin, tab))`,
		`INSERT INTO ui_tabs (plugin, tab, title, route, pos, suppressed, closed_revision, rev_known) VALUES ('p', 't', 'T', '/', 0, 1, 'r1', 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for i := 0; i < 2; i++ {
		db, err := openDB(path)
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		if columnExists(t, db.DB, "ui_tabs", "rev_known") {
			t.Fatalf("open %d: ui_tabs.rev_known is still there", i+1)
		}
		var closed string
		var suppressed bool
		if err := db.QueryRow(`SELECT suppressed, closed_revision FROM ui_tabs WHERE plugin = 'p' AND tab = 't'`).Scan(&suppressed, &closed); err != nil || !suppressed || closed != "r1" {
			t.Fatalf("open %d: the row is %v %q (%v)", i+1, suppressed, closed, err)
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO ui_tabs (plugin, tab, title, route, pos) VALUES ('p', 'u', 'U', '/', 1)`); err != nil {
			t.Fatalf("open %d: an INSERT without rev_known: %v", i+1, err)
		}
		db.Close()
	}
}

// A new hub.db has no ui_tabs at open: the step does nothing, and the UI schema then has no
// rev_known.
func TestRetireSchemaOnANewDatabase(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(uiSchema); err != nil {
		t.Fatal(err)
	}
	if columnExists(t, db.DB, "ui_tabs", "rev_known") {
		t.Fatal("the UI schema creates ui_tabs.rev_known")
	}
}

// A hub.db with plugins.entry_stamp (the rollback guard) loses the column at open; a new hub.db
// never has it.
func TestRetireSchemaDropsEntryStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if columnExists(t, db.DB, "plugins", "entry_stamp") {
		t.Fatal("a new hub.db has plugins.entry_stamp")
	}
	if _, err := db.Exec(`ALTER TABLE plugins ADD COLUMN entry_stamp TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err = openDB(path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if columnExists(t, db.DB, "plugins", "entry_stamp") {
		t.Fatal("plugins.entry_stamp stays after the open")
	}
}
