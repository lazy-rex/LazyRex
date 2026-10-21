package sqlpolicy

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

var hubOwned = map[string]bool{"lazyrex_ops": true, "lazyrex_notice_outbox": true}

func setup(t *testing.T) (*conn, context.Context) {
	ctx := context.Background()
	k, err := open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	k.trusted = true
	for _, q := range []string{"CREATE TABLE lazyrex_ops (key TEXT PRIMARY KEY, reply TEXT)",
		"CREATE TABLE lazyrex_notice_outbox (seq INTEGER PRIMARY KEY, agent TEXT, pending INTEGER)"} {
		if _, err := k.c.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	k.trusted = false
	return k, ctx
}

func must(t *testing.T, k *conn, ctx context.Context, q string) result {
	t.Helper()
	r := k.exec(ctx, q, hubOwned)
	if r.err != nil {
		t.Fatalf("%s: %v", q, r.err)
	}
	return r
}

// Ordinary DDL passes although SQLite reports its own sqlite_master writes to the authorizer.
func TestPolicyAllowsDDLAndIntrospection(t *testing.T) {
	k, ctx := setup(t)
	for _, q := range []string{
		"CREATE TABLE m (id INTEGER PRIMARY KEY AUTOINCREMENT, sender TEXT, body TEXT) STRICT",
		"CREATE TABLE d (agent TEXT, seq INTEGER REFERENCES m(id) ON DELETE CASCADE, acked INTEGER, PRIMARY KEY (agent, seq)) WITHOUT ROWID",
		"CREATE INDEX d_unread ON d(agent) WHERE acked = 0",
		"CREATE VIEW unread AS SELECT * FROM d WHERE acked = 0",
		"CREATE TRIGGER t AFTER INSERT ON m BEGIN INSERT INTO d VALUES ('a', new.id, 0); END",
		"ALTER TABLE m ADD COLUMN at TEXT",
		"ALTER TABLE m RENAME COLUMN at TO sent_at",
		"CREATE TEMP TABLE scratch (x)",
		"PRAGMA user_version = 4",
		"SELECT * FROM pragma_table_info('m')",
		"PRAGMA foreign_key_check",
		"SELECT json_extract('{\"a\":1}', '$.a')",
		"INSERT INTO m (sender, body) VALUES ('x', 'y') ON CONFLICT DO NOTHING",
		"DELETE FROM sqlite_sequence WHERE name = 'nothing'",
	} {
		must(t, k, ctx, q)
	}
}

func TestPolicyRefusals(t *testing.T) {
	k, ctx := setup(t)
	must(t, k, ctx, "CREATE TABLE m (x)")
	for _, q := range []string{
		"ATTACH 'other.db' AS o",
		"DETACH main",
		"PRAGMA journal_mode = delete",
		"PRAGMA foreign_keys = OFF",
		"PRAGMA writable_schema = ON",
		"PRAGMA synchronous = OFF",
		"SELECT load_extension('x')",
		"BEGIN",
		"SAVEPOINT s",
		"CREATE VIRTUAL TABLE v USING fts5(x)",
		"SELECT * FROM lazyrex_ops",
		"SELECT * FROM LazyRex_Ops",
		"INSERT INTO lazyrex_notice_outbox VALUES (1, 'a', 1)",
		"CREATE TABLE LAZYREX_x (y)",
		"CREATE TEMP TABLE lazyrex_t (y)",
		"CREATE TRIGGER evil AFTER INSERT ON lazyrex_ops BEGIN SELECT 1; END",
		"CREATE INDEX lazyrex_i ON m(x)",
		"ALTER TABLE m RENAME TO lazyrex_m",
		"ALTER TABLE m RENAME TO LazyRex_M",
		"DROP TABLE lazyrex_ops",
		"UPDATE sqlite_master SET sql = 'x'",
	} {
		refused2(t, k, ctx, q)
	}
	// SQLite does not authorize a view body at CREATE; it does at each use, also in a trigger.
	must(t, k, ctx, "CREATE VIEW peek AS SELECT * FROM lazyrex_ops")
	refused2(t, k, ctx, "SELECT * FROM peek")
	must(t, k, ctx, "CREATE TRIGGER sneaky AFTER INSERT ON m BEGIN INSERT INTO m SELECT count(*) FROM lazyrex_ops; END")
	refused2(t, k, ctx, "INSERT INTO m VALUES (1)")
}

// refused2 accepts SQLite's own refusal of a sqlite_master write as a refusal too.
func refused2(t *testing.T, k *conn, ctx context.Context, q string) {
	t.Helper()
	k.trusted = true
	k.c.ExecContext(ctx, "SAVEPOINT probe")
	k.trusted = false
	r := k.exec(ctx, q, hubOwned)
	k.trusted = true
	k.c.ExecContext(ctx, "ROLLBACK TO probe")
	k.c.ExecContext(ctx, "RELEASE probe")
	k.trusted = false
	if r.err == nil {
		t.Errorf("%s: not refused", q)
		return
	}
	t.Logf("%-70s -> %v", q, r.err)
}

// Marks come from the statement's write candidates, and only when total_changes moved: trigger
// writes, INSTEAD OF triggers, RAISE(IGNORE) after a trigger write and cascades count.
func TestPolicyChangeMarks(t *testing.T) {
	k, ctx := setup(t)
	must(t, k, ctx, "CREATE TABLE a (x)")
	must(t, k, ctx, "CREATE TABLE b (x)")
	must(t, k, ctx, "CREATE TABLE p (id INTEGER PRIMARY KEY)")
	must(t, k, ctx, "CREATE TABLE c (pid INTEGER REFERENCES p(id) ON DELETE CASCADE)")
	must(t, k, ctx, "CREATE TRIGGER ign BEFORE INSERT ON a BEGIN INSERT INTO b VALUES (new.x); SELECT RAISE(IGNORE); END")
	must(t, k, ctx, "CREATE VIEW v AS SELECT x FROM b")
	must(t, k, ctx, "CREATE TRIGGER io INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES (new.x); END")
	must(t, k, ctx, "INSERT INTO p VALUES (1)")
	must(t, k, ctx, "INSERT INTO c VALUES (1)")

	cases := []struct {
		q     string
		marks []string
	}{
		{"INSERT INTO a VALUES (1)", []string{"a", "b"}},   // direct changes() = 0, b gained a row
		{"INSERT INTO v VALUES (2)", []string{"b", "v"}},   // INSTEAD OF: changes() = 0
		{"DELETE FROM p WHERE id = 1", []string{"c", "p"}}, // the cascade deletes from c
		{"UPDATE b SET x = x WHERE 0", nil},                // no row: no mark
		{"SELECT * FROM b", nil},                           // a read
		{"PRAGMA user_version = 9", nil},                   // no table change
		{"CREATE TABLE later (x)", []string{"*"}},          // a schema change marks every view
	}
	for _, c := range cases {
		r := must(t, k, ctx, c.q)
		slices.Sort(r.marks)
		if !slices.Equal(r.marks, c.marks) {
			t.Errorf("%s: marks %v (changes %d), want %v", c.q, r.marks, r.changed, c.marks)
		}
	}
}
