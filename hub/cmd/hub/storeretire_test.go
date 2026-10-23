package main

import (
	"database/sql"
	"fmt"
	"os"
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

// D1: the open drops sql_returns and sql_notice_plugins at once, also with rows. It drops records
// and ops only when each namespace with a row in either table has sql/<namespace>.db; it keeps
// them, and logs each namespace without a database, until then. A directory is not a database.
func TestRetireStoreTables(t *testing.T) {
	state := t.TempDir()
	path, sqlDir := filepath.Join(state, "hub.db"), filepath.Join(state, "sql")
	oldStoreDB(t, path,
		`INSERT INTO records VALUES ('alpha', 'k', '1', 1)`,
		`INSERT INTO ops VALUES ('beta', 'op', 'fp', '{}')`,
		`INSERT INTO sql_returns VALUES ('alpha', 's')`,
		`INSERT INTO sql_notice_plugins VALUES ('alpha')`)
	if err := os.MkdirAll(filepath.Join(sqlDir, "beta.db"), 0o700); err != nil { // not a file
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sqlDir, "alpha.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	reopen(t, path)
	if have := strings.Join(storeTables(t, path), " "); have != "records ops" {
		t.Fatalf("beta has no database: the tables are %q, want records ops", have)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	if err := retireStore(db, sqlDir, func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if len(logged) != 1 || !strings.Contains(logged[0], `"beta"`) {
		t.Errorf("the log: %q, want one line for beta", logged)
	}

	if err := os.Remove(filepath.Join(sqlDir, "beta.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sqlDir, "beta.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reopen(t, path)
	if have := storeTables(t, path); len(have) != 0 {
		t.Errorf("each namespace has a database: the tables %v stay", have)
	}
	reopen(t, path) // an open after the drop
}

// Empty records and ops have no namespace, so the open drops them.
func TestRetireStoreTablesEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	oldStoreDB(t, path)
	reopen(t, path)
	if have := storeTables(t, path); len(have) != 0 {
		t.Errorf("empty tables stay: %v", have)
	}
}
