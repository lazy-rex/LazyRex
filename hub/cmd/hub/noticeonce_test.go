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

// noticeSeq numbers the outbox rows of the notice tests: a row with a seq that has a receipt is
// skipped.
var noticeSeq int64

// notifyIn applies one hub.sql outbox row of ns with the notice op for agent a, and reports whether
// it made a new intent revision (a new nudge).
func notifyIn(t *testing.T, db *DB, ns string, op map[string]any) bool {
	t.Helper()
	n, err := parseNotice("a", op)
	if err != nil {
		t.Fatalf("notice %v: %v", op, err)
	}
	rev := func() int64 {
		var r int64
		db.QueryRow(`SELECT intent_rev FROM notices WHERE ns = ? AND agent = 'a'`, ns).Scan(&r)
		return r
	}
	before := rev()
	noticeSeq++
	if err := db.applySQLNotices(context.Background(), ns, []outboxRow{{noticeSeq, n}}); err != nil {
		t.Fatalf("notice %v: %v", op, err)
	}
	return rev() != before
}

func notify(t *testing.T, db *DB, op map[string]any) bool { return notifyIn(t, db, "messaging", op) }

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
	notifyIn(t, db, "other", map[string]any{"read": true})
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM notices WHERE ns = 'other'`).Scan(&n)
	if n != 0 {
		t.Fatal("a read made a notice row")
	}
}

func TestNoticeOpForms(t *testing.T) {
	for _, op := range []map[string]any{
		{"read": true, "pending": true},
		{"read": false},
		{"read": "yes"},
		{"pending": true, "once": "yes"},
		{"pending": false, "once": true},
		{"read": true, "once": true},
		{},
	} {
		if _, err := parseNotice("a", op); !isBad(err) {
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
	}
	for _, c := range cases {
		due, repeat, wake := noticeDue(c.n, conv, c.mem, now, after)
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

// The memory record of a delivery stands for its stored row when a store write failed (Codex review
// of a1205c1): a first write that only memory holds still gets its repeat at the right time, a
// repeat that only memory holds is not written again, and a first write to another conversation
// replaces the record, so that conversation gets its own repeat.
func TestNoticeDeliveryMemory(t *testing.T) {
	const after = 30 * time.Minute
	t0 := time.Unix(3_000_000, 0)
	a := noticeConv{block: "block:1", pid: 10, start: "s"}
	b := noticeConv{block: "block:1", pid: 11, start: "t"}
	row := pendingNotice{ns: "messaging", agent: "x", intentRev: 5} // no stored delivery: the store writes failed
	first := noticeDelivery{rev: 5, at: t0.UnixMilli(), conv: a}
	n := row.withDelivery(first, true)
	if due, _, wake := noticeDue(n, a, 5, t0.Add(time.Minute), after); due || !wake.Equal(t0.Add(after)) {
		t.Fatalf("a first write in memory only: due %v wake %v, want the wake at %v", due, wake, t0.Add(after))
	}
	if due, repeat, _ := noticeDue(n, a, 5, t0.Add(after), after); !due || !repeat {
		t.Fatal("a first write in memory only: no repeat at its time")
	}
	rep := first
	rep.repeated = true
	if due, _, _ := noticeDue(row.withDelivery(rep, true), a, 5, t0.Add(2*after), after); due {
		t.Fatal("a repeat in memory only was due again")
	}
	// Child B: its first write replaces the record of A's repeat.
	tB := t0.Add(3 * after)
	if due, repeat, _ := noticeDue(row.withDelivery(rep, true), b, 0, tB, after); !due || repeat {
		t.Fatal("another conversation: want a first write")
	}
	nb := row.withDelivery(noticeDelivery{rev: 5, at: tB.UnixMilli(), conv: b}, true)
	if due, repeat, _ := noticeDue(nb, b, 0, tB.Add(after), after); !due || !repeat {
		t.Fatal("another conversation: no repeat of its own")
	}
	// A stored row and an equal memory record give the same result as the row alone.
	stored := pendingNotice{ns: "messaging", agent: "x", intentRev: 5, delivRev: 5, delivAt: t0.UnixMilli(), delivConv: a}
	if stored.withDelivery(first, true) != stored || stored.withDelivery(noticeDelivery{}, false) != stored {
		t.Fatal("the merge changed a stored row")
	}
}
