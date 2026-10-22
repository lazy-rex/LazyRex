package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func noticeTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// notify runs one store batch with a single notify op for agent a and reports whether it made a
// new intent revision (a new nudge).
func notify(t *testing.T, db *DB, op map[string]any) bool {
	t.Helper()
	op["agent"] = "a"
	out, err := db.storeBatch(context.Background(), "messaging", map[string]any{"ops": []any{map[string]any{"notify": op}}}, map[string]bool{"a": true}, true)
	if err != nil {
		t.Fatalf("notify %v: %v", op, err)
	}
	_, nudge := out.intentRev["a"]
	return nudge
}

// One nudge until a read (the user's rule, 2026-10-22): a once send makes a new revision only if
// the row is not pending or the agent read since the last revision. A plain pending:true keeps
// today's rule, one revision for each send.
func TestNoticeOnceUntilRead(t *testing.T) {
	db := noticeTestDB(t)
	once := func() bool { return notify(t, db, map[string]any{"pending": true, "once": true}) }
	read := func() {
		if notify(t, db, map[string]any{"read": true}) {
			t.Fatal("a read made a revision")
		}
	}
	if !once() || once() || once() {
		t.Fatal("three once sends in a row: want one revision, for the first")
	}
	read()
	if !once() || once() {
		t.Fatal("a read, then two once sends: want one revision, for the first")
	}
	if notify(t, db, map[string]any{"pending": false}) || !once() {
		t.Fatal("pending:false, then a once send: want one revision")
	}
	if !notify(t, db, map[string]any{"pending": true}) || !notify(t, db, map[string]any{"pending": true}) {
		t.Fatal("a plain pending:true makes a revision for each send")
	}
	if once() {
		t.Fatal("a once send after a plain send (no read): want no revision")
	}
	// A read for an agent with no row is accepted and does nothing.
	if _, err := db.storeBatch(context.Background(), "other", map[string]any{"ops": []any{map[string]any{"notify": map[string]any{"agent": "a", "read": true}}}}, map[string]bool{"a": true}, true); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM notices WHERE ns = 'other'`).Scan(&n)
	if n != 0 {
		t.Fatal("a read made a notice row")
	}
}

func TestNoticeOpForms(t *testing.T) {
	db := noticeTestDB(t)
	for _, op := range []map[string]any{
		{"read": true, "pending": true},
		{"read": false},
		{"read": "yes"},
		{"pending": true, "once": "yes"},
		{"pending": false, "once": true},
		{"read": true, "once": true},
		{},
	} {
		op["agent"] = "a"
		if _, err := db.storeBatch(context.Background(), "messaging", map[string]any{"ops": []any{map[string]any{"notify": op}}}, map[string]bool{"a": true}, true); !isBad(err) {
			t.Errorf("%v: %v, want a malformed-request error", op, err)
		}
	}
}

// isBad reports a malformed request (bad: a storeError with no code).
func isBad(err error) bool {
	var se *storeError
	return errors.As(err, &se) && se.code == ""
}

// noticeDue decides one pending notice: a first nudge, a repeat, or nothing. A delivery counts
// only for the conversation it reached (block, child pid and start); a repeat comes once, after
// renudge without a read or a new revision.
func TestNoticeDue(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	conv := noticeConv{block: "block:1", pid: 10, start: "s"}
	sent := func(rev int64, ago time.Duration, c noticeConv, repeated bool) pendingNotice {
		return pendingNotice{ns: "messaging", agent: "a", intentRev: 5, delivRev: rev, delivAt: now.Add(-ago).UnixMilli(), delivConv: c, repeated: repeated}
	}
	const after = 30 * time.Minute
	cases := []struct {
		name        string
		n           pendingNotice
		mem         int64
		due, repeat bool
		wakeIn      time.Duration // 0: no wake
	}{
		{"never sent", pendingNotice{intentRev: 5}, 0, true, false, 0},
		{"sent in memory only (the store write failed)", pendingNotice{intentRev: 5}, 5, false, false, 0},
		{"sent to this conversation", sent(5, time.Minute, conv, false), 0, false, false, after - time.Minute},
		{"sent, then a hub restart (memory empty)", sent(5, time.Minute, conv, false), 0, false, false, after - time.Minute},
		{"sent to another conversation", sent(5, time.Minute, noticeConv{block: "block:1", pid: 11, start: "s"}, false), 0, true, false, 0},
		{"an older revision was sent", sent(4, time.Minute, conv, false), 0, true, false, 0},
		{"no read for 30 minutes", sent(5, after, conv, false), 0, true, true, 0},
		{"repeated already", sent(5, 2*after, conv, true), 0, false, false, 0},
		{"read since the nudge", func() pendingNotice { n := sent(5, 2*after, conv, false); n.read = true; return n }(), 0, false, false, 0},
		// Codex review of 8169791: a read before the first write ends that nudge; a repeat that this
		// hub run wrote (its store write failed) is not written again.
		{"read before the first write", pendingNotice{intentRev: 5, read: true}, 0, false, false, 0},
		{"repeated in memory only", sent(5, 2*after, conv, false), 0, false, false, 0},
	}
	for _, c := range cases {
		due, repeat, wake := noticeDue(c.n, conv, c.mem, c.name == "repeated in memory only", now, after)
		var wantWake time.Time
		if c.wakeIn != 0 {
			wantWake = now.Add(c.wakeIn)
		}
		if due != c.due || repeat != c.repeat || !wake.Equal(wantWake) {
			t.Errorf("%s: due %v repeat %v wake %v; want %v %v %v", c.name, due, repeat, wake, c.due, c.repeat, wantWake)
		}
	}
}

// The delivery record survives a reopen of hub.db (a hub restart), and a repeat is recorded once.
func TestNoticeDeliveryStored(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !notify(t, db, map[string]any{"pending": true, "once": true}) {
		t.Fatal("no revision")
	}
	at := time.Unix(2_000_000, 0)
	conv := noticeConv{block: "block:1", pid: 10, start: "s"}
	ctx := context.Background()
	p, _ := db.pendingNotices(ctx)
	if err := db.recordNotice(ctx, p[0], conv, at, false); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err = openDB(filepath.Join(dir, "hub.db")); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, _ = db.pendingNotices(ctx)
	if len(p) != 1 || p[0].delivRev != p[0].intentRev || p[0].delivConv != conv || p[0].delivAt != at.UnixMilli() || p[0].repeated || p[0].read {
		t.Fatalf("after a reopen: %+v", p)
	}
	if err := db.recordNotice(ctx, p[0], conv, at.Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	p, _ = db.pendingNotices(ctx)
	if !p[0].repeated || p[0].delivAt != at.UnixMilli() {
		t.Fatalf("a repeat: %+v (the first delivery time stays)", p[0])
	}
	// A read marks the row; a new revision after it clears read and the repeat.
	notify(t, db, map[string]any{"read": true})
	p, _ = db.pendingNotices(ctx)
	if !p[0].read {
		t.Fatal("the read is not stored")
	}
	if !notify(t, db, map[string]any{"pending": true, "once": true}) {
		t.Fatal("a once send after a read: no revision")
	}
	p, _ = db.pendingNotices(ctx)
	if p[0].read || p[0].delivRev >= p[0].intentRev {
		t.Fatalf("after a new revision: %+v", p[0])
	}
}
