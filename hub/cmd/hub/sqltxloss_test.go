package main

import "testing"

// A statement error that ends SQLite's transaction (a conflict with OR ROLLBACK, RAISE(ROLLBACK)
// in a trigger, some engine errors) ends the handler transaction too: its token gives tx_expired,
// and no later statement of that token writes. Before, the token stayed open, and the next
// statement's SAVEPOINT began a new transaction that its RELEASE committed (Codex review of
// 8510d50).
func TestSQLTransactionLoss(t *testing.T) {
	setup := func(t *testing.T) *sqlLab {
		l := newSQLLab(t)
		l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (n INTEGER UNIQUE)"}`)
		l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (1)"}`)
		l.must("hub.sql.exec", `{"sql":"CREATE TABLE u (a)"}`)
		l.must("hub.sql.exec", `{"sql":"CREATE TRIGGER no_zero BEFORE INSERT ON u WHEN NEW.a = 0 BEGIN SELECT RAISE(ROLLBACK, 'no zero'); END"}`)
		return l
	}
	for name, fail := range map[string]func(tx string) (string, string){
		"insert or rollback": func(tx string) (string, string) {
			return "hub.sql.exec", js(map[string]any{"tx": tx, "sql": "INSERT OR ROLLBACK INTO t VALUES (1)"})
		},
		"raise rollback": func(tx string) (string, string) {
			return "hub.sql.exec", js(map[string]any{"tx": tx, "sql": "INSERT INTO u VALUES (0)"})
		},
		"a batch with insert or rollback": func(tx string) (string, string) {
			return "hub.sql.batch", js(map[string]any{"tx": tx, "statements": []any{map[string]any{"sql": "INSERT INTO t VALUES (3)"}, map[string]any{"sql": "INSERT OR ROLLBACK INTO t VALUES (1)"}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := setup(t)
			tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
			l.must("hub.sql.exec", js(map[string]any{"tx": tx, "sql": "INSERT INTO u VALUES (5)"}))
			action, args := fail(tx)
			l.fails("sql_error", action, args)
			l.fails("tx_expired", "hub.sql.exec", js(map[string]any{"tx": tx, "sql": "INSERT INTO t VALUES (2)"}))
			l.fails("tx_expired", "hub.sql.commit", js(map[string]any{"tx": tx}))
			if q := l.must("hub.sql.query", `{"sql":"SELECT (SELECT group_concat(n) FROM t), (SELECT count(*) FROM u)"}`); js(q["rows"]) != `[["1",0]]` {
				t.Fatalf("a write of the lost transaction stays: %s", js(q["rows"]))
			}
			// The file takes a new transaction.
			tx2 := l.must("hub.sql.begin", `{}`)["tx"].(string)
			l.must("hub.sql.exec", js(map[string]any{"tx": tx2, "sql": "INSERT INTO t VALUES (4)"}))
			l.must("hub.sql.commit", js(map[string]any{"tx": tx2}))
		})
	}
	t.Run("an abort keeps the transaction", func(t *testing.T) {
		l := setup(t)
		tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
		l.must("hub.sql.exec", js(map[string]any{"tx": tx, "sql": "INSERT INTO t VALUES (2)"}))
		l.fails("sql_error", "hub.sql.exec", js(map[string]any{"tx": tx, "sql": "INSERT INTO t VALUES (1)"}))
		l.must("hub.sql.exec", js(map[string]any{"tx": tx, "sql": "INSERT INTO t VALUES (3)"}))
		l.must("hub.sql.commit", js(map[string]any{"tx": tx}))
		if q := l.must("hub.sql.query", `{"sql":"SELECT group_concat(n) FROM (SELECT n FROM t ORDER BY n)"}`); js(q["rows"]) != `[["1,2,3"]]` {
			t.Fatalf("rows %s", js(q["rows"]))
		}
	})
}

// A query error in a handler transaction goes through the same loss check as a statement error
// (Codex note on 81da75e). An ordinary query error keeps the transaction: it commits. No read
// error that ends SQLite's transaction is known, so this is a guard, not a reproduction.
func TestSQLQueryErrorKeepsTheTransaction(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (n)"}`)
	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.must("hub.sql.exec", js(map[string]any{"tx": tx, "sql": "INSERT INTO t VALUES (1)"}))
	l.fails("sql_error", "hub.sql.query", js(map[string]any{"tx": tx, "sql": "SELECT * FROM missing"}))
	l.must("hub.sql.commit", js(map[string]any{"tx": tx}))
	if q := l.must("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`); js(q["rows"]) != `[[1]]` {
		t.Fatalf("the transaction after a query error: %s", js(q["rows"]))
	}
}
