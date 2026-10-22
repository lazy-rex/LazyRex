package main

import (
	"context"
	"testing"
)

// hub.sql.commit notices take the three forms of a notify op (docs/hub.md "Notices"): a once send
// makes no new revision while the agent has not read, and a read ends it. Before, the commit
// refused once and read (invalid_input), so an SQL plugin could not use notice v2.
func TestSQLNoticesV2(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	l := newSQLLab(t)
	l.s.admitNotices = func(string, []noticeOp) error { return nil }
	l.s.applyNotices = func(plugin string, rows []outboxRow) error { return h.db.applySQLNotices(ctx, plugin, rows) }
	commit := func(notices string) {
		t.Helper()
		tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
		l.must("hub.sql.commit", `{"tx":"`+tx+`", "notices":`+notices+`}`)
	}
	row := func() (rev int64, pending, read int) {
		h.db.QueryRow(`SELECT intent_rev, pending, read FROM notices WHERE ns = 'p' AND agent = 'a'`).Scan(&rev, &pending, &read)
		return
	}

	commit(`[{"agent":"a", "pending":true, "once":true}]`)
	first, pending, read := row()
	if first == 0 || pending != 1 || read != 0 {
		t.Fatalf("the first once send: rev %d, pending %d, read %d", first, pending, read)
	}
	commit(`[{"agent":"a", "pending":true, "once":true}]`)
	if rev, _, _ := row(); rev != first {
		t.Fatalf("a once send before a read made revision %d (want %d)", rev, first)
	}
	commit(`[{"agent":"a", "read":true}]`)
	if rev, pending, read := row(); rev != first || pending != 1 || read != 1 {
		t.Fatalf("after the read: rev %d, pending %d, read %d", rev, pending, read)
	}
	commit(`[{"agent":"a", "pending":true, "once":true}]`)
	if rev, _, read := row(); rev <= first || read != 0 {
		t.Fatalf("a once send after the read: rev %d (want > %d), read %d", rev, first, read)
	}
	second, _, _ := row()
	commit(`[{"agent":"a", "pending":true}]`) // a plain send makes a revision each time
	if rev, _, _ := row(); rev <= second {
		t.Fatalf("a plain send: rev %d (want > %d)", rev, second)
	}

	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	for _, bad := range []string{
		`[{"agent":"a", "read":true, "pending":true}]`,
		`[{"agent":"a", "read":false}]`,
		`[{"agent":"a", "pending":false, "once":true}]`,
		`[{"agent":"a", "pending":true, "once":"yes"}]`,
		`[{"agent":"a", "pending":true, "other":1}]`,
		`[{"agent":"a", "read":true}, {"agent":"a", "pending":true}]`,
	} {
		l.fails("invalid_input", "hub.sql.commit", `{"tx":"`+tx+`", "notices":`+bad+`}`)
	}
	l.must("hub.sql.rollback", `{"tx":"`+tx+`"}`)
}
