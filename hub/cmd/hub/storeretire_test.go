package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// retiredStoreTables are the hub.db tables of the removed hub.store (D1 of the hub.store removal).
var retiredStoreTables = []string{"records", "ops", "sql_returns", "sql_notice_plugins"}

// oldStoreDB makes a hub.db at path with the tables that a hub before the hub.store removal made,
// and runs the extra statements.
func oldStoreDB(t *testing.T, path string, extra ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range append([]string{
		`CREATE TABLE records (ns TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, rev INTEGER NOT NULL, PRIMARY KEY (ns, key))`,
		`CREATE TABLE ops (ns TEXT NOT NULL, key TEXT NOT NULL, fingerprint TEXT NOT NULL, result TEXT NOT NULL, PRIMARY KEY (ns, key))`,
		`CREATE TABLE sql_returns (plugin TEXT PRIMARY KEY, stamp TEXT NOT NULL)`,
		`CREATE TABLE sql_notice_plugins (plugin TEXT PRIMARY KEY)`,
	}, extra...) {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
}

// storeTables lists which of retiredStoreTables the hub.db at path has.
func storeTables(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var have []string
	for _, name := range retiredStoreTables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			have = append(have, name)
		}
	}
	return have
}

func reopen(t *testing.T, path string) {
	t.Helper()
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
}

// A new hub.db has none of the hub.store tables.
func TestNewHubDBHasNoStoreTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	reopen(t, path)
	if have := storeTables(t, path); len(have) != 0 {
		t.Errorf("a new hub.db has %v", have)
	}
}

// D1: the open drops records, ops, sql_returns and sql_notice_plugins, also with rows, and with no
// plugin database. Their rows are not kept. An open after the drop finds nothing to do.
func TestRetireStoreTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	oldStoreDB(t, path,
		`INSERT INTO records VALUES ('alpha', 'k', '1', 1)`,
		`INSERT INTO ops VALUES ('beta', 'op', 'fp', '{}')`,
		`INSERT INTO sql_returns VALUES ('alpha', 's')`,
		`INSERT INTO sql_notice_plugins VALUES ('alpha')`)
	reopen(t, path)
	if have := storeTables(t, path); len(have) != 0 {
		t.Errorf("the tables %v stay", have)
	}
	reopen(t, path) // an open after the drop
}

// Empty tables go too.
func TestRetireStoreTablesEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	oldStoreDB(t, path)
	reopen(t, path)
	if have := storeTables(t, path); len(have) != 0 {
		t.Errorf("empty tables stay: %v", have)
	}
}

// retiredSchema is well formed: one entry for each table or column, with a check and a statement
// that name what it removes.
func TestRetiredSchemaIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range retiredSchema {
		name := s.table
		if s.column != "" {
			name += "." + s.column
		}
		if s.table == "" || seen[name] {
			t.Errorf("%q: an empty or duplicate entry", name)
		}
		seen[name] = true
		if s.check == "" || !strings.Contains(s.check, "'"+s.table+"'") || !strings.Contains(s.statement, " "+s.table) {
			t.Errorf("%s: the check or the statement does not name the table", name)
		}
		drop := "DROP TABLE " + s.table
		if s.column != "" {
			drop = "DROP COLUMN " + s.column
		}
		if !strings.Contains(s.statement, drop) || !strings.Contains(s.check, "'"+s.column+"'") && s.column != "" {
			t.Errorf("%s: the statement %q is not %s", name, s.statement, drop)
		}
	}
	for _, table := range retiredStoreTables {
		if !seen[table] {
			t.Errorf("retiredSchema does not name %s", table)
		}
	}
}

// hubSchema lists each table and each column of the hub.db at path: "t" and "t.c".
func hubSchema(t *testing.T, path string) map[string]bool {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT m.name, p.name FROM sqlite_master m, pragma_table_info(m.name) p WHERE m.type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		out[table], out[table+"."+column] = true, true
	}
	return out
}

// An open removes only what retiredSchema names: every table or column of the old hub.db that is
// gone after the open is a declared entry (the whole table, or that column), and every entry is gone.
func TestOpenRemovesOnlyDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, s := range retiredSchema {
		if s.column == "" {
			declared[s.table] = true
		} else {
			declared[s.table+"."+s.column] = true
		}
	}
	path := filepath.Join(t.TempDir(), "hub.db")
	reopen(t, path) // the current schema, then each object that a step removes
	oldStoreDB(t, path, `CREATE TABLE ui_tabs (key TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', rev_known INTEGER NOT NULL DEFAULT 0, layout TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE views (view_id TEXT PRIMARY KEY, layout TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE view_transcripts (view_id TEXT PRIMARY KEY, status TEXT NOT NULL)`,
		`INSERT INTO view_transcripts VALUES ('v1', 'open')`,
		`ALTER TABLE plugins ADD COLUMN entry_stamp TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE bindings (agent TEXT PRIMARY KEY, block_id TEXT NOT NULL, child_pid INTEGER NOT NULL, child_start TEXT NOT NULL)`,
		`CREATE TABLE dyn_agents (agent TEXT PRIMARY KEY, name TEXT NOT NULL, profile TEXT NOT NULL, spawn_id TEXT NOT NULL, created TEXT NOT NULL)`,
		`CREATE TABLE identity_aliases (legacy TEXT PRIMARY KEY, identity TEXT NOT NULL, created TEXT NOT NULL)`,
		`INSERT INTO records VALUES ('alpha', 'k', '1', 1)`, `INSERT INTO ops VALUES ('beta', 'op', 'fp', '{}')`)
	before := hubSchema(t, path)
	reopen(t, path)
	after := hubSchema(t, path)
	for x := range before {
		if after[x] {
			continue
		}
		table, _, _ := strings.Cut(x, ".")
		if !declared[x] && !declared[table] {
			t.Errorf("the open removed %s, which retiredSchema does not name", x)
		}
	}
	for x := range declared {
		if !before[x] {
			t.Errorf("the old hub.db of the test has no %s", x)
		}
		if after[x] {
			t.Errorf("%s stays after the open", x)
		}
	}
	if !after["ui_tabs.title"] {
		t.Error("the column ui_tabs.title went with rev_known")
	}
	if !after["views.view_id"] {
		t.Error("the column views.view_id went with layout")
	}
}
