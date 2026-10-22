package main

import "testing"

// The id of an exec reply (last_insert_rowid) has the exact integer form of a result cell: a value
// beyond 2^53 is {"int": "<decimal>"}, also in a batch result (Codex review of 8510d50).
func TestSQLExecIDExact(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (id INTEGER PRIMARY KEY)"}`)
	out := l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (?)", "params":[{"int":"9007199254740993"}]}`)
	if js(out["id"]) != `{"int":"9007199254740993"}` {
		t.Fatalf("exec id %s", js(out["id"]))
	}
	b := l.must("hub.sql.batch", `{"statements":[{"sql":"INSERT INTO t VALUES (?)", "params":[{"int":"9007199254740995"}]}, {"sql":"INSERT INTO t VALUES (7)"}]}`)
	if got := js(b["results"]); got != `[{"changes":1,"id":{"int":"9007199254740995"}},{"changes":1,"id":7}]` {
		t.Fatalf("batch results %s", got)
	}
}
