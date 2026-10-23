package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// An open runs dropTables and dropColumns: the listed table goes with its rows, the listed column
// goes and the other columns of its table stay, a column of a table that does not exist is no
// error (a new hub.db makes ui_tabs later, in uiSchema), and a second open finds nothing to do.
func TestOpenDropsListed(t *testing.T) {
	tables, columns := dropTables, dropColumns
	t.Cleanup(func() { dropTables, dropColumns = tables, columns })
	dropTables = []string{"old_t"}
	dropColumns = []struct{ table, column string }{{"old_c", "gone"}, {"no_such_table", "gone"}}
	path := filepath.Join(t.TempDir(), "hub.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE old_t (k TEXT PRIMARY KEY)`, `INSERT INTO old_t VALUES ('a')`,
		`CREATE TABLE old_c (k TEXT PRIMARY KEY, gone TEXT NOT NULL DEFAULT '')`, `INSERT INTO old_c VALUES ('a', 'x')`,
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	for range 2 {
		db, err := openDB(path)
		if err != nil {
			t.Fatal(err)
		}
		var tableN, goneN, keptN int
		err = db.QueryRow(`SELECT (SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'old_t'),
			(SELECT COUNT(*) FROM pragma_table_info('old_c') WHERE name = 'gone'), (SELECT COUNT(*) FROM old_c WHERE k = 'a')`).Scan(&tableN, &goneN, &keptN)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if tableN != 0 || goneN != 0 || keptN != 1 {
			t.Fatalf("table old_t %d, column old_c.gone %d, row of old_c %d; want 0, 0, 1", tableN, goneN, keptN)
		}
	}
}
