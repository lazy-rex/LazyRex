package main

import "testing"

// The names that start with lazyrex_ are the hub's in main only. In temp, every such name is
// refused, also a hub table's name: a temp table with that name shadows the hub's own unqualified
// SQL (Codex review of 8510d50).
func TestSQLTempReservedNames(t *testing.T) {
	l := newSQLLab(t)
	l.must("hub.sql.exec", `{"sql":"CREATE TEMP TABLE fake (key TEXT, fingerprint TEXT, reply TEXT, at INTEGER)"}`)
	l.fails("refused", "hub.sql.exec", `{"sql":"ALTER TABLE temp.fake RENAME TO lazyrex_ops"}`)
	l.fails("refused", "hub.sql.exec", `{"sql":"CREATE TEMP TABLE lazyrex_ops (key TEXT)"}`)
	l.fails("refused", "hub.sql.exec", `{"sql":"CREATE TEMP TABLE lazyrex_mine (a)"}`)
	l.fails("refused", "hub.sql.exec", `{"sql":"CREATE TEMP VIEW lazyrex_notice_outbox AS SELECT 1"}`)
	l.must("hub.sql.exec", `{"sql":"CREATE TEMP TABLE mine (a)"}`)
}
