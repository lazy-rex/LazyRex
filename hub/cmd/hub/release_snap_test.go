package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// snapDBs makes a plugin database and a hub database with the store tables, and runs setup in them.
func snapDBs(t *testing.T, plugin, hub string) (*sql.DB, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	open := func(name, setup string) *sql.DB {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if _, err := db.Exec(setup); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return db
	}
	p := open("p.db", plugin)
	h := open("hub.db", `CREATE TABLE records (ns TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, rev INTEGER NOT NULL, PRIMARY KEY (ns, key));
CREATE TABLE ops (ns TEXT NOT NULL, key TEXT NOT NULL, fingerprint TEXT NOT NULL, result TEXT NOT NULL, PRIMARY KEY (ns, key));`+hub)
	return p, h
}

const snapSchema = `PRAGMA user_version = 2;
CREATE TABLE drafts (view_id TEXT PRIMARY KEY, dir TEXT, dir_rev INTEGER NOT NULL, score REAL, v);
CREATE TABLE clock (one INTEGER PRIMARY KEY, at INTEGER);
CREATE TABLE lazyrex_ops (key TEXT PRIMARY KEY);
INSERT INTO drafts VALUES ('v1', NULL, 1, 0.5, 1);
INSERT INTO clock VALUES (1, 100);`

func mustSnap(t *testing.T, p, h *sql.DB, keys map[string][]string) *relSnap {
	t.Helper()
	s, err := newSnap(p, h, "profiles", keys)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReleaseSnapRead(t *testing.T) {
	p, h := snapDBs(t, snapSchema, `INSERT INTO records VALUES ('profiles','r1','{"a":1}',3), ('other','r1','x',1);`)
	s := mustSnap(t, p, h, map[string][]string{"clock": {"one"}})
	if s.UserVersion != 2 || len(s.Tables) != 4 || s.Tables["lazyrex_ops"] != nil {
		t.Fatalf("snapshot %+v", s)
	}
	row := s.Tables["drafts"].Rows[`[{"t":116,"v":"v1"}]`]
	if len(row) != 5 || row[1].T != 'n' || row[2] != (relCell{T: 'i', V: "1"}) || row[3] != (relCell{T: 'r', V: "0.5"}) {
		t.Fatalf("drafts row %+v", row)
	}
	if r := s.Tables["hub:records"].Rows; len(r) != 1 {
		t.Fatalf("records of the namespace: %+v", r)
	}
	for name, tc := range map[string]struct {
		schema string
		keys   map[string][]string
		want   string
	}{
		"rowid key":   {snapSchema, nil, "clock has no stable key"},
		"no pk":       {`CREATE TABLE x (a TEXT)`, nil, "x has no stable key"},
		"null key":    {`CREATE TABLE x (a TEXT); INSERT INTO x VALUES (NULL)`, map[string][]string{"x": {"a"}}, "NULL in the key column a"},
		"dup key":     {`CREATE TABLE x (a TEXT, b TEXT); INSERT INTO x VALUES ('k','1'), ('k','2')`, map[string][]string{"x": {"a"}}, "does not identify one row"},
		"unknown col": {`CREATE TABLE x (a TEXT PRIMARY KEY)`, map[string][]string{"x": {"b"}}, "no key column b"},
	} {
		p, h := snapDBs(t, tc.schema, "")
		if _, err := newSnap(p, h, "profiles", tc.keys); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v; want %q", name, err, tc.want)
		}
	}
}

func TestReleaseSnapCheck(t *testing.T) {
	keys := map[string][]string{"clock": {"one"}}
	entry := func(js string) *relSetEntry {
		var e relSetEntry
		if err := json.Unmarshal([]byte(js), &e); err != nil {
			t.Fatal(err)
		}
		return &e
	}
	none := entry(`{}`)
	for name, tc := range map[string]struct {
		change string // SQL run between the snapshots
		hub    string // SQL run in the hub database between the snapshots
		e      *relSetEntry
		want   string // "" passes
	}{
		"no change":       {"", "", none, ""},
		"lost row":        {"DELETE FROM drafts", "", none, "lost the row"},
		"lost table":      {"DROP TABLE clock", "", none, "the schema changed"},
		"lost column":     {"ALTER TABLE drafts DROP COLUMN score", "", none, "the schema changed"},
		"lower version":   {"PRAGMA user_version = 1", "", none, "user_version changed from 2 to 1"},
		"changed value":   {"UPDATE drafts SET dir_rev = 2", "", none, "no changes entry allows it"},
		"type change":     {"UPDATE drafts SET v = '1'", "", none, "no changes entry allows it"},
		"update all":      {"UPDATE drafts SET dir = 'x'", "", entry(`{"changes":[{"table":"drafts","key":{"view_id":"v9"},"column":"dir","expect":"x"}]}`), "no changes entry allows it"},
		"exact change":    {"UPDATE drafts SET dir_rev = 2", "", entry(`{"changes":[{"table":"drafts","key":{"view_id":"v1"},"column":"dir_rev","expect":2}]}`), ""},
		"wrong value":     {"UPDATE drafts SET dir_rev = 3", "", entry(`{"changes":[{"table":"drafts","key":{"view_id":"v1"},"column":"dir_rev","expect":2}]}`), "not the expected 2"},
		"real vs int":     {"UPDATE drafts SET score = 1.0", "", entry(`{"changes":[{"table":"drafts","key":{"view_id":"v1"},"column":"score","expect":1}]}`), "is 1.0, not the expected 1"},
		"sql change":      {"UPDATE clock SET at = 150", "", entry(`{"changes":[{"table":"clock","key":{"one":1},"column":"at","expect":{"sql":":new > :old"}}]}`), ""},
		"sql false":       {"UPDATE clock SET at = 50", "", entry(`{"changes":[{"table":"clock","key":{"one":1},"column":"at","expect":{"sql":":new > :old"}}]}`), "predicate is false"},
		"insert refused":  {"INSERT INTO drafts VALUES ('v2', NULL, 1, NULL, NULL)", "", none, "no inserts entry"},
		"insert allowed":  {"INSERT INTO drafts VALUES ('v2', NULL, 1, NULL, NULL)", "", entry(`{"inserts":[{"table":"drafts","max":1,"invariants":["SELECT 1 WHERE 0"]}]}`), ""},
		"insert over max": {"INSERT INTO drafts VALUES ('v2', NULL, 1, NULL, NULL), ('v3', NULL, 1, NULL, NULL)", "", entry(`{"inserts":[{"table":"drafts","max":1,"invariants":["SELECT 1 WHERE 0"]}]}`), "allows 1"},
		"store write":     {"", "INSERT INTO records VALUES ('profiles','r2','x',1)", none, "hub:records has 1 new rows"},
		"store rev":       {"", "UPDATE records SET rev = 4 WHERE key = 'r1'", none, "no changes entry allows it"},
		"other ns":        {"", "INSERT INTO records VALUES ('other','r2','x',1)", none, ""},
	} {
		t.Run(name, func(t *testing.T) {
			p, h := snapDBs(t, snapSchema, `INSERT INTO records VALUES ('profiles','r1','{"a":1}',3);`)
			ref := mustSnap(t, p, h, keys)
			if tc.change != "" {
				if _, err := p.Exec(tc.change); err != nil {
					t.Fatal(err)
				}
			}
			if tc.hub != "" {
				if _, err := h.Exec(tc.hub); err != nil {
					t.Fatal(err)
				}
			}
			cur, err := newSnap(p, h, "profiles", keys)
			if err != nil {
				t.Fatal(err)
			}
			_, err = checkSnap(ref, cur, tc.e, sqlPredicate(p))
			if tc.want == "" && err != nil {
				t.Fatalf("got %v; want a pass", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v; want an error with %q", err, tc.want)
			}
		})
	}
}

// TestReleaseSnapCumulative: inserts count from the seeded reference over every check, a rerun
// that adds a row is caught, and a row that an insert added must not change or go later.
func TestReleaseSnapCumulative(t *testing.T) {
	p, h := snapDBs(t, snapSchema, "")
	keys := map[string][]string{"clock": {"one"}}
	var e relSetEntry
	json.Unmarshal([]byte(`{"inserts":[{"table":"drafts","max":2,"invariants":["SELECT view_id FROM drafts WHERE dir_rev <> 1"]}]}`), &e)
	ref := mustSnap(t, p, h, keys)
	step := func(sqlText, want string) {
		t.Helper()
		if _, err := p.Exec(sqlText); err != nil {
			t.Fatal(err)
		}
		cur := mustSnap(t, p, h, keys)
		_, err := checkSnap(ref, cur, &e, sqlPredicate(p))
		if err == nil {
			err = checkInvariants(p, &e)
		}
		if want == "" && err != nil {
			t.Fatalf("%s: %v", sqlText, err)
		}
		if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatalf("%s: got %v; want %q", sqlText, err, want)
		}
	}
	step("INSERT INTO drafts VALUES ('v2', NULL, 1, NULL, NULL)", "")
	step("SELECT 1", "") // a rerun that writes nothing
	step("UPDATE drafts SET dir = 'x' WHERE view_id = 'v2'", "no changes entry")
	p.Exec("UPDATE drafts SET dir = NULL WHERE view_id = 'v2'")
	step("INSERT INTO drafts VALUES ('v3', NULL, 1, NULL, NULL)", "")
	step("INSERT INTO drafts VALUES ('v4', NULL, 1, NULL, NULL)", "3 rows more than after the seed")
	p.Exec("DELETE FROM drafts WHERE view_id = 'v4'")
	step("DELETE FROM drafts WHERE view_id = 'v2'", "lost the row")
	p.Exec("INSERT INTO drafts VALUES ('v2', NULL, 1, NULL, NULL)")
	step("UPDATE drafts SET dir_rev = 5 WHERE view_id = 'v1'", "no changes entry")
}

func TestReleaseSnapInvariant(t *testing.T) {
	p, _ := snapDBs(t, snapSchema, "")
	var e relSetEntry
	json.Unmarshal([]byte(`{"inserts":[{"table":"drafts","max":2,"invariants":["SELECT view_id FROM drafts GROUP BY dir_rev HAVING count(*) > 1"]}]}`), &e)
	if err := checkInvariants(p, &e); err != nil {
		t.Fatal(err)
	}
	p.Exec("INSERT INTO drafts VALUES ('v2', NULL, 1, NULL, NULL)")
	if err := checkInvariants(p, &e); err == nil || !strings.Contains(err.Error(), "returns a row") {
		t.Fatalf("got %v", err)
	}
}

func TestReleaseSnapKeyTables(t *testing.T) {
	p, h := snapDBs(t, `CREATE TABLE x (a TEXT PRIMARY KEY)`, "")
	s := mustSnap(t, p, h, nil)
	if err := checkKeyTables(s, map[string][]string{"x": {"a"}}); err != nil {
		t.Fatal(err)
	}
	if err := checkKeyTables(s, map[string][]string{"y": {"a"}}); err == nil || !strings.Contains(err.Error(), "does not have") {
		t.Fatalf("got %v", err)
	}
}

// TestReleaseSnapReviewFindings: the reviews of 360f6e3 and 6dce01c (codex-26). Any schema change
// fails, also a lost UNIQUE or NOT NULL constraint; a JSON null is no key value; a user table named
// sqliteX is read.
func TestReleaseSnapReviewFindings(t *testing.T) {
	keys := map[string][]string{"clock": {"one"}}
	none := &relSetEntry{}
	run := func(t *testing.T, p, h *sql.DB, ref *relSnap, sqlText, want string) {
		t.Helper()
		if _, err := p.Exec(sqlText); err != nil {
			t.Fatal(err)
		}
		cur := mustSnap(t, p, h, keys)
		_, err := checkSnap(ref, cur, none, sqlPredicate(p))
		if want == "" && err != nil {
			t.Fatalf("%s: %v", sqlText, err)
		}
		if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatalf("%s: got %v; want %q", sqlText, err, want)
		}
	}
	// The 4.2 scope (coordinator, 2026-10-22): no schema change of any kind passes.
	for name, change := range map[string]string{
		"new column":       "ALTER TABLE drafts ADD COLUMN extra TEXT DEFAULT 'd'",
		"version up":       "PRAGMA user_version = 3",
		"version down":     "PRAGMA user_version = 1",
		"new table":        "CREATE TABLE extra (id TEXT PRIMARY KEY)",
		"new index":        "CREATE INDEX drafts_rev ON drafts (dir_rev)",
		"dropped index":    "DROP INDEX drafts_dir",
		"changed index":    "DROP INDEX drafts_dir; CREATE INDEX drafts_dir ON drafts (dir_rev)",
		"dropped trigger":  "DROP TRIGGER dt",
		"dropped view":     "DROP VIEW dv",
		"lost unique":      "DROP TABLE u; CREATE TABLE u (k TEXT PRIMARY KEY, v TEXT)",
		"lost not null":    "DROP TABLE n; CREATE TABLE n (k TEXT PRIMARY KEY, v TEXT)",
		"same, other case": "DROP TABLE n; CREATE TABLE n (k TEXT PRIMARY KEY, v TEXT not null)",
	} {
		t.Run(name, func(t *testing.T) {
			p, h := snapDBs(t, snapSchema+`CREATE INDEX drafts_dir ON drafts (dir);
				CREATE VIEW dv AS SELECT view_id FROM drafts; CREATE TRIGGER dt AFTER INSERT ON drafts BEGIN SELECT 1; END;
				CREATE TABLE u (k TEXT PRIMARY KEY, v TEXT UNIQUE); CREATE TABLE n (k TEXT PRIMARY KEY, v TEXT NOT NULL);`, "")
			ref := mustSnap(t, p, h, keys)
			want := "the schema changed"
			if strings.HasPrefix(name, "version") {
				want = "user_version changed"
			}
			run(t, p, h, ref, change, want)
		})
	}
	t.Run("automatic index recorded", func(t *testing.T) {
		p, h := snapDBs(t, snapSchema+"CREATE TABLE u (k TEXT PRIMARY KEY, v TEXT UNIQUE);", "")
		ref := mustSnap(t, p, h, keys)
		var auto int
		for _, r := range ref.Schema {
			if r.SQL == nil && r.TblName == "u" {
				auto++
			}
		}
		if auto != 2 {
			t.Fatalf("automatic indexes of u: %d; schema %v", auto, ref.Schema)
		}
		run(t, p, h, ref, "SELECT 1", "")
	})
	t.Run("statistics ignored", func(t *testing.T) {
		p, h := snapDBs(t, snapSchema+"CREATE INDEX drafts_dir ON drafts (dir);", "")
		ref := mustSnap(t, p, h, keys)
		run(t, p, h, ref, "ANALYZE", "")
	})
	t.Run("hub objects ignored", func(t *testing.T) {
		p, h := snapDBs(t, snapSchema+"CREATE INDEX lazyrex_ops_k ON lazyrex_ops (key);", "")
		ref := mustSnap(t, p, h, keys)
		run(t, p, h, ref, "DROP INDEX lazyrex_ops_k", "")
	})
	t.Run("null key", func(t *testing.T) {
		p, h := snapDBs(t, `CREATE TABLE x (a TEXT PRIMARY KEY, v TEXT); INSERT INTO x VALUES ('', '1');`, "")
		ref := mustSnap(t, p, h, nil)
		p.Exec("UPDATE x SET v = '2'")
		cur := mustSnap(t, p, h, nil)
		var e relSetEntry
		json.Unmarshal([]byte(`{"changes":[{"table":"x","key":{"a":null},"column":"v","expect":"2"}]}`), &e)
		if _, err := checkSnap(ref, cur, &e, sqlPredicate(p)); err == nil || !strings.Contains(err.Error(), "cannot be null") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("sqliteX table", func(t *testing.T) {
		p, h := snapDBs(t, `CREATE TABLE sqliteXdata (a TEXT PRIMARY KEY); INSERT INTO sqliteXdata VALUES ('k');`, "")
		ref := mustSnap(t, p, h, nil)
		if ref.Tables["sqliteXdata"] == nil {
			t.Fatalf("tables %v", ref.Tables)
		}
		run(t, p, h, ref, "DELETE FROM sqliteXdata", "lost the row")
	})
}

// TestReleaseSnapExactText: the review of 6dce01c (codex-26). An exact changes expect compares the
// text as written; HTML characters get no escapes on either side.
func TestReleaseSnapExactText(t *testing.T) {
	p, h := snapDBs(t, `CREATE TABLE x (k TEXT PRIMARY KEY, v TEXT); INSERT INTO x VALUES ('k', 'before');`, "")
	ref := mustSnap(t, p, h, nil)
	var e relSetEntry
	if err := json.Unmarshal([]byte(`{"changes":[{"table":"x","key":{"k":"k"},"column":"v","expect":"a<b>&c"}]}`), &e); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(`UPDATE x SET v = 'a<b>&c'`); err != nil {
		t.Fatal(err)
	}
	if _, err := checkSnap(ref, mustSnap(t, p, h, nil), &e, sqlPredicate(p)); err != nil {
		t.Fatalf("the exact text was refused: %v", err)
	}
	if _, err := p.Exec(`UPDATE x SET v = 'a<b>&d'`); err != nil {
		t.Fatal(err)
	}
	if _, err := checkSnap(ref, mustSnap(t, p, h, nil), &e, sqlPredicate(p)); err == nil || !strings.Contains(err.Error(), "not the expected") {
		t.Fatalf("other text: got %v", err)
	}
}
