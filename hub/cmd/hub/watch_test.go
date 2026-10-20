package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestSettlerSettlesOnTwoEqualObservations(t *testing.T) {
	s := newSettler(time.Second)
	t0 := time.Unix(1000, 0)
	a := watchObs{Exists: true, Eligible: true, Ino: 1, Size: 10, MtimeNs: 1}
	s.observe("x", a, t0)
	if d := s.due(t0.Add(999 * time.Millisecond)); len(d) != 0 {
		t.Fatalf("due before settle time: %v", d)
	}
	// A write that the backend did not report: the timer starts again.
	b := a
	b.Size = 20
	if s.recheck("x", b, t0.Add(time.Second)) {
		t.Fatal("a different observation settled")
	}
	if next, _ := s.next(); !next.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("next = %v", next)
	}
	if !s.recheck("x", b, t0.Add(2*time.Second)) {
		t.Fatal("an equal observation did not settle")
	}
	if _, ok := s.next(); ok {
		t.Fatal("a settled name stayed pending")
	}
}

// Many events for one name (many writes, duplicates, a rename over it) restart one timer, so the
// name settles once.
func TestSettlerManyEventsOneSettle(t *testing.T) {
	s := newSettler(time.Second)
	t0 := time.Unix(1000, 0)
	for i := 0; i < 10; i++ {
		s.observe("x", watchObs{Exists: true, Eligible: true, Size: int64(i)}, t0.Add(time.Duration(i)*100*time.Millisecond))
	}
	if d := s.due(t0.Add(1800 * time.Millisecond)); len(d) != 0 {
		t.Fatalf("due during writes: %v", d)
	}
	if d := s.due(t0.Add(1900 * time.Millisecond)); len(d) != 1 || d[0] != "x" {
		t.Fatalf("due = %v", d)
	}
	s.reset()
	if d := s.due(t0.Add(time.Hour)); len(d) != 0 {
		t.Fatalf("due after reset: %v", d)
	}
}

func TestWatchNameRules(t *testing.T) {
	for _, c := range []struct {
		name, glob string
		want       bool
	}{
		{"a.md", "*", true}, {"a.md", "*.md", true}, {"a.txt", "*.md", false},
		{".a.md", "*", false}, {".DS_Store", "*", false}, {".a.md.swp", "*.md", false},
	} {
		if got := watchName(c.name, c.glob); got != c.want {
			t.Errorf("watchName(%q, %q) = %v", c.name, c.glob, got)
		}
	}
}

// B5: the budget counts every entry that kqueue opens, ignored names included, and the listing
// stops once the count passes the limit.
func TestListWatchDirCountsIgnoredEntriesAndStopsEarly(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "note.md"), "x")
	for i := 0; i < 1299; i++ {
		write(t, filepath.Join(dir, fmt.Sprintf("ignored-%04d.txt", i)), "")
	}
	l, err := listWatchDir(dir, "*.md", 1000, 256)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Over || l.Chunks != 4 || l.Count != 1024 {
		t.Fatalf("over %v, chunks %d, count %d; want true, 4, 1024", l.Over, l.Chunks, l.Count)
	}
	// After it shrinks: under the limit, every chunk read, sockets and pipes not counted.
	for i := 0; i < 1299; i++ {
		os.Remove(filepath.Join(dir, fmt.Sprintf("ignored-%04d.txt", i)))
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(dir, "sub"), 0o700)
	os.Symlink("/", filepath.Join(dir, "link.md"))
	l, err = listWatchDir(dir, "*.md", 1000, 256)
	if err != nil {
		t.Fatal(err)
	}
	if l.Over || l.Count != 3 {
		t.Fatalf("over %v, count %d; want false, 3 (file, directory, symlink)", l.Over, l.Count)
	}
	regular := map[string]bool{}
	for _, e := range l.Entries {
		regular[e.Name] = e.Regular
	}
	if len(regular) != 3 || !regular["note.md"] || regular["link.md"] || regular["pipe.md"] {
		t.Fatalf("entries %v", l.Entries)
	}
}

// The listing bound counts every entry: entries without a descriptor (named pipes) still stop the
// listing early (review of 3036308).
func TestListWatchDirBoundsEntriesWithoutDescriptors(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 1300; i++ {
		if err := syscall.Mkfifo(filepath.Join(dir, fmt.Sprintf("pipe-%04d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l, err := listWatchDir(dir, "*", 1000, 256)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Over || l.Chunks != 4 || l.Seen != 1024 || l.Count != 0 || len(l.Entries) != 1024 {
		t.Fatalf("over %v, chunks %d, seen %d, count %d, entries %d; want true, 4, 1024, 0, 1024",
			l.Over, l.Chunks, l.Seen, l.Count, len(l.Entries))
	}
}

func TestObserveEntryDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "target"), "data")
	os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "link"))
	o, err := observeEntry(dir, "link")
	if err != nil || !o.Exists || o.Eligible {
		t.Fatalf("symlink: %+v, %v", o, err)
	}
	o, err = observeEntry(dir, "gone")
	if err != nil || o.Exists {
		t.Fatalf("missing: %+v, %v", o, err)
	}
	o, err = observeEntry(dir, "target")
	if err != nil || !o.present() || o.Size != 4 || o.Ino == 0 {
		t.Fatalf("file: %+v, %v", o, err)
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// feedTest is a watch in a fresh store. Observations are made up: the feed rules do not stat.
type feedTest struct {
	t  *testing.T
	db *DB
	id string
}

func newFeedTest(t *testing.T, emitExisting bool, names ...string) *feedTest {
	db, err := openDB(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(watchSchema); err != nil {
		t.Fatal(err)
	}
	f := &feedTest{t: t, db: db, id: "w1"}
	err = db.createWatch(context.Background(), watchRecord{Plugin: "p", Key: "inbox", ID: f.id, Path: "/in", Resolved: "/in",
		Glob: "*", SettleMs: 1000, EmitExisting: emitExisting}, names)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func file(size int64) watchObs {
	return watchObs{Exists: true, Eligible: true, Ino: 7, Size: size, MtimeNs: 1}
}

var gone = watchObs{}

func (f *feedTest) commit(name string, o watchObs) {
	f.t.Helper()
	if _, err := f.db.commitSettled(context.Background(), f.id, name, o); err != nil {
		f.t.Fatal(err)
	}
}

func (f *feedTest) read(after int64, limit, maxBytes int) feedPage {
	f.t.Helper()
	p, err := f.db.readFeed(context.Background(), f.id, after, limit, maxBytes)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *feedTest) ack(through int64) {
	f.t.Helper()
	if _, err := f.db.ackFeed(context.Background(), f.id, through); err != nil {
		f.t.Fatal(err)
	}
}

func (f *feedTest) state() string {
	w, err := f.db.watchByID(context.Background(), f.db, f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	return w.State
}

func kinds(p feedPage) string {
	s := ""
	for _, r := range p.Rows {
		s += fmt.Sprintf("%s:%s ", r["name"], r["kind"])
	}
	return s
}

func seqOf(r map[string]any) int64 {
	n, _ := strconv.ParseInt(r["seq"].(string), 10, 64)
	return n
}

// R1: the reviewer's case. A (created_seq 10, latest seq 100) and B (seq 50), read from 0 with
// limit 1. Page two must return A as created: the page cursor does not classify.
func TestFeedPageCursorDoesNotClassify(t *testing.T) {
	f := newFeedTest(t, false)
	f.commit("A", file(1))
	f.commit("B", file(1))
	f.commit("A", file(2))
	p := f.read(0, 1, 1<<18)
	if kinds(p) != "B:created " || !p.More {
		t.Fatalf("page one: %s more %v", kinds(p), p.More)
	}
	p = f.read(p.Next, 1, 1<<18)
	if kinds(p) != "A:created " || p.More {
		t.Fatalf("page two: %s more %v", kinds(p), p.More)
	}
}

func TestFeedKindsFollowAcks(t *testing.T) {
	f := newFeedTest(t, false)
	f.commit("A", file(1))
	p := f.read(0, 100, 1<<18)
	f.ack(p.Next)
	f.commit("A", file(2))
	p = f.read(p.Next, 100, 1<<18)
	if kinds(p) != "A:changed " {
		t.Fatalf("after a change: %s", kinds(p))
	}
	f.ack(p.Next)
	f.commit("A", gone)
	p = f.read(p.Next, 100, 1<<18)
	if kinds(p) != "A:removed " {
		t.Fatalf("after a removal: %s", kinds(p))
	}
	f.ack(p.Next)
	var n int
	f.db.QueryRow(`SELECT count(*) FROM watch_rows`).Scan(&n)
	if n != 0 {
		t.Fatalf("an acked tombstone stayed: %d rows", n)
	}
}

// Amendment 2: acked create, settled removal, settled recreation, read: created, a new appearance.
func TestFeedRecreationIsANewAppearance(t *testing.T) {
	f := newFeedTest(t, false)
	f.commit("A", file(1))
	p := f.read(0, 100, 1<<18)
	first := p.Rows[0]["appearance_key"]
	f.ack(p.Next)
	f.commit("A", gone)
	f.commit("A", file(1))
	p = f.read(p.Next, 100, 1<<18)
	if kinds(p) != "A:created " || p.Rows[0]["appearance_key"] == first {
		t.Fatalf("%s, appearance %v (first %v)", kinds(p), p.Rows[0]["appearance_key"], first)
	}
}

// Amendment 3: created, the consumer commits its start-once action, the ack is lost, the file is
// removed. The next read has a new event_key and the same appearance_key.
func TestFeedLostAckThenRemoval(t *testing.T) {
	f := newFeedTest(t, false)
	f.commit("A", file(1))
	p := f.read(0, 100, 1<<18)
	created := p.Rows[0]
	f.commit("A", gone)
	p = f.read(0, 100, 1<<18)
	r := p.Rows[0]
	if r["kind"] != "created_removed" || r["event_key"] == created["event_key"] || r["appearance_key"] != created["appearance_key"] {
		t.Fatalf("created %v, then %v", created, r)
	}
}

// At-least-once: a row that changes after it was read moves past the ack, and comes again as
// created with the same appearance_key.
func TestFeedRowMovedPastAckComesAgain(t *testing.T) {
	f := newFeedTest(t, false)
	f.commit("A", file(1))
	p := f.read(0, 100, 1<<18)
	key := p.Rows[0]["appearance_key"]
	f.commit("A", file(2))
	f.ack(p.Next)
	p = f.read(p.Next, 100, 1<<18)
	if kinds(p) != "A:created " || p.Rows[0]["appearance_key"] != key {
		t.Fatalf("%s %v", kinds(p), p.Rows)
	}
}

// R2: pages of 100 with an ack after each page end, without a loop.
func TestFeedPagesEnd(t *testing.T) {
	f := newFeedTest(t, false)
	for i := 0; i < 300; i++ {
		f.commit(fmt.Sprintf("f%03d", i), file(1))
	}
	var after int64
	pages := 0
	for {
		p := f.read(after, 100, 1<<18)
		if len(p.Rows) == 0 {
			break
		}
		pages++
		f.ack(p.Next)
		after = p.Next
		if !p.More {
			break
		}
	}
	if pages != 3 {
		t.Fatalf("%d pages", pages)
	}
	if p := f.read(after, 100, 1<<18); len(p.Rows) != 0 || p.More {
		t.Fatalf("rows after the last page: %s", kinds(p))
	}
}

func TestFeedAckCursorRules(t *testing.T) {
	f := newFeedTest(t, false)
	f.commit("A", file(1))
	p := f.read(0, 100, 1<<18)
	if _, err := f.db.ackFeed(context.Background(), f.id, p.Watermark+1); !errors.Is(err, errBadCursor) {
		t.Fatalf("an ack past the watermark: %v", err)
	}
	if p := f.read(0, 100, 1<<18); kinds(p) != "A:created " {
		t.Fatalf("a refused ack changed the feed: %s", kinds(p))
	}
	f.ack(p.Next)
	c, err := f.db.ackFeed(context.Background(), f.id, p.Next-1)
	if err != nil || c != p.Next {
		t.Fatalf("a stale ack: %d, %v", c, err)
	}
}

func TestFeedBacklogFullPausesNewRows(t *testing.T) {
	old := watchRowLimit
	watchRowLimit = 2
	defer func() { watchRowLimit = old }()
	f := newFeedTest(t, false)
	f.commit("A", file(1))
	f.commit("B", file(1))
	if _, err := f.db.commitSettled(context.Background(), f.id, "C", file(1)); !errors.Is(err, errBacklogFull) {
		t.Fatalf("a third row: %v", err)
	}
	f.commit("A", file(2)) // a change to a row adds no row
	f.commit("A", gone)
	p := f.read(0, 100, 1<<18)
	f.ack(p.Next) // deletes A's tombstone
	f.commit("C", file(1))
}

func TestFeedInactiveWatchCommitsNothing(t *testing.T) {
	f := newFeedTest(t, false)
	for _, s := range []string{"missing", "error", "too_large", "not_granted", "backlog_full", "inactive"} {
		f.db.setWatchState(context.Background(), f.id, s, "")
		if _, err := f.db.commitSettled(context.Background(), f.id, "A", file(1)); !errors.Is(err, errWatchInactive) {
			t.Fatalf("state %s: %v", s, err)
		}
	}
}

// A regular file replaced by a symlink settles not eligible: a tombstone, because it has a row.
func TestFeedNotEligibleWithARowIsATombstone(t *testing.T) {
	f := newFeedTest(t, false)
	f.commit("A", file(1))
	p := f.read(0, 100, 1<<18)
	f.ack(p.Next)
	f.commit("A", watchObs{Exists: true, Ino: 9})
	f.commit("B", watchObs{Exists: true, Ino: 9}) // no row: nothing
	if p = f.read(p.Next, 100, 1<<18); kinds(p) != "A:removed " {
		t.Fatalf("%s", kinds(p))
	}
}

// Amendment 1: one baseline name settles and the watch becomes active; the baseline file is hidden
// until it changes.
func TestFeedBaselineInitialization(t *testing.T) {
	f := newFeedTest(t, false, "a", "b")
	if f.state() != "initializing" {
		t.Fatal(f.state())
	}
	f.commit("new", file(1)) // a later arrival during initialization
	f.commit("a", file(1))
	f.commit("b", gone) // settled absent: dropped, no row
	if f.state() != "active" {
		t.Fatal(f.state())
	}
	p := f.read(0, 100, 1<<18)
	if kinds(p) != "new:created " || p.Next != p.Watermark {
		t.Fatalf("%s next %d watermark %d", kinds(p), p.Next, p.Watermark)
	}
	f.ack(p.Next)
	f.commit("a", file(2))
	if p = f.read(p.Next, 100, 1<<18); kinds(p) != "a:changed " {
		t.Fatalf("%s", kinds(p))
	}
}

func TestFeedEmitExistingInitialization(t *testing.T) {
	f := newFeedTest(t, true, "a")
	f.commit("a", file(1))
	if f.state() != "active" {
		t.Fatal(f.state())
	}
	if p := f.read(0, 100, 1<<18); kinds(p) != "a:created " {
		t.Fatalf("%s", kinds(p))
	}
	if g := newFeedTest(t, true); g.state() != "active" {
		t.Fatalf("empty listing: %s", g.state())
	}
}

// next never passes a visible row that was not returned, at a row limit and at a byte limit, with
// hidden baseline rows around the boundary.
func TestFeedNextAtPageBoundaries(t *testing.T) {
	for _, c := range []struct {
		name            string
		limit, maxBytes int
	}{{"rows", 1, 1 << 18}, {"bytes", 100, 1}} {
		t.Run(c.name, func(t *testing.T) {
			f := newFeedTest(t, false, "base1", "base2", "base3")
			f.commit("base1", file(1))
			f.commit("v1", file(1))
			f.commit("base2", file(1))
			f.commit("v2", file(1))
			f.commit("base3", file(1))
			p := f.read(0, c.limit, c.maxBytes)
			if kinds(p) != "v1:created " || !p.More {
				t.Fatalf("page one: %s more %v", kinds(p), p.More)
			}
			v1 := seqOf(p.Rows[0])
			p2 := f.read(p.Next, c.limit, c.maxBytes)
			if kinds(p2) != "v2:created " || p2.More || p2.Next != p2.Watermark {
				t.Fatalf("page two: %s more %v next %d", kinds(p2), p2.More, p2.Next)
			}
			if p.Next < v1 || p.Next >= seqOf(p2.Rows[0]) {
				t.Fatalf("next %d is not between v1 %d and v2 %d", p.Next, v1, seqOf(p2.Rows[0]))
			}
		})
	}
}
