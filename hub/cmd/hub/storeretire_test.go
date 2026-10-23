package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
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

// A failed drop changes nothing: records, ops and their rows stay, and retireStore returns the
// error (so the open fails); once the cause is gone, the next call drops both. Here a foreign key from another table
// blocks the drop of ops, the second statement of the transaction.
func TestRetireStoreFailedDropKeepsBoth(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "hub.db")
	oldStoreDB(t, path,
		`INSERT INTO records VALUES ('alpha', 'k', '1', 1)`,
		`INSERT INTO ops VALUES ('alpha', 'op', 'fp', '{}')`,
		`CREATE TABLE blocker (ns TEXT, key TEXT, FOREIGN KEY (ns, key) REFERENCES ops (ns, key))`,
		`INSERT INTO blocker VALUES ('alpha', 'op')`)
	if err := os.MkdirAll(filepath.Join(state, "sql"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "sql", "alpha.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := retireStore(db, filepath.Join(state, "sql"), t.Logf); err == nil || !strings.Contains(err.Error(), "DROP TABLE ops") {
		t.Fatalf("the blocked drop: %v, want the error of DROP TABLE ops", err)
	}
	var n int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM records) + (SELECT COUNT(*) FROM ops)`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("after the failed drop: %d rows, %v; want records and ops with their 2 rows", n, err)
	}
	if _, err := db.Exec(`DROP TABLE blocker`); err != nil {
		t.Fatal(err)
	}
	if err := retireStore(db, filepath.Join(state, "sql"), t.Logf); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('records', 'ops')`).Scan(&n); err != nil || n != 0 {
		t.Errorf("without the blocker: %d of the tables stay, %v", n, err)
	}
}

// retiredSchema is well formed: one entry for each table or column; an unconditional entry has a
// check and a statement that name what it removes; a conditional entry has neither, and it is a
// table that retireStore drops.
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
		if s.conditional {
			if s.check != "" || s.statement != "" || s.column != "" || (s.table != "records" && s.table != "ops") {
				t.Errorf("%s: a conditional entry is a table that retireStore drops, with no check or statement", name)
			}
			continue
		}
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
	for _, table := range []string{"records", "ops"} {
		if !seen[table] {
			t.Errorf("retireStore drops %s, and retiredSchema does not name it", table)
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
// gone after the open is a declared entry (the whole table, or that column); every unconditional
// entry is gone; a conditional entry may stay (one namespace without a database) or go.
func TestOpenRemovesOnlyDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, s := range retiredSchema {
		if s.column == "" {
			declared[s.table] = true
		} else {
			declared[s.table+"."+s.column] = true
		}
	}
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("a namespace without a database %v", missing), func(t *testing.T) {
			state := t.TempDir()
			path := filepath.Join(state, "hub.db")
			reopen(t, path) // the current schema, then each object that a step removes
			oldStoreDB(t, path, `CREATE TABLE ui_tabs (key TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', rev_known INTEGER NOT NULL DEFAULT 0)`,
				`INSERT INTO records VALUES ('alpha', 'k', '1', 1)`, `INSERT INTO ops VALUES ('beta', 'op', 'fp', '{}')`)
			if err := os.MkdirAll(filepath.Join(state, "sql"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "sql", "alpha.db"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if !missing {
				if err := os.WriteFile(filepath.Join(state, "sql", "beta.db"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
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
			for _, s := range retiredSchema {
				x := s.table
				if s.column != "" {
					x += "." + s.column
				}
				if !before[x] {
					t.Errorf("the old hub.db of the test has no %s", x)
				}
				if s.conditional {
					if after[x] != missing {
						t.Errorf("%s after the open: %v, want %v", x, after[x], missing)
					}
				} else if after[x] {
					t.Errorf("%s stays after the open", x)
				}
			}
			if !after["ui_tabs.title"] {
				t.Error("the column ui_tabs.title went with rev_known")
			}
		})
	}
}

// "lazyrex-hub -retired-schema" prints every retiredSchema step, in order, as version 1.
func TestRetiredSchemaExport(t *testing.T) {
	var b bytes.Buffer
	if code := retiredSchemaMain(&b); code != 0 {
		t.Fatalf("exit %d", code)
	}
	d := json.NewDecoder(&b)
	d.DisallowUnknownFields()
	var got retiredExport
	if err := d.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || len(got.Retired) != len(retiredSchema) {
		t.Fatalf("export %+v, want version 1 and %d entries", got, len(retiredSchema))
	}
	for i, s := range retiredSchema {
		if e := got.Retired[i]; e.Table != s.table || e.Column != s.column || e.Conditional != s.conditional {
			t.Errorf("entry %d is %+v, want %s %q conditional=%v", i, e, s.table, s.column, s.conditional)
		}
	}
}
