package main

// File watches (docs/hub.md, "File watches"). This file holds the rules that need no fsnotify:
// the settle state of each name, the bounded directory listing, and the change feed.
//
// The feed has one row per name with the latest settled state of that name. A settled change
// gives the row a new seq from the database-wide revision counter, so a seq is never reused, also
// by a later watch with the same key. The hub, not the plugin, derives the kind of each row from
// acked_present, because each watch has exactly one consumer (its plugin).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const watchSchema = `
-- state: initializing, active, missing, error, too_large, not_granted, backlog_full, inactive
CREATE TABLE IF NOT EXISTS watches (plugin TEXT NOT NULL, key TEXT NOT NULL, watch_id TEXT NOT NULL UNIQUE,
	path TEXT NOT NULL, resolved TEXT NOT NULL, glob TEXT NOT NULL, settle_ms INTEGER NOT NULL,
	emit_existing INTEGER NOT NULL, state TEXT NOT NULL, last_error TEXT NOT NULL DEFAULT '',
	cursor INTEGER NOT NULL DEFAULT 0, watermark INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (plugin, key));
-- One row per name. present = 0 is a tombstone. created_seq is the seq of the latest appearance.
-- baseline = 1: a file of the ensure listing (emit_existing false) that has not changed since; read hides it.
CREATE TABLE IF NOT EXISTS watch_rows (watch_id TEXT NOT NULL, name TEXT NOT NULL, present INTEGER NOT NULL,
	ino INTEGER NOT NULL, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL, seq INTEGER NOT NULL,
	created_seq INTEGER NOT NULL, acked_present INTEGER NOT NULL, baseline INTEGER NOT NULL,
	PRIMARY KEY (watch_id, name));
CREATE INDEX IF NOT EXISTS watch_rows_seq ON watch_rows (watch_id, seq);
-- The names of the ensure listing that have not settled yet. Initialization ends when none remain.
CREATE TABLE IF NOT EXISTS watch_pending (watch_id TEXT NOT NULL, name TEXT NOT NULL, baseline INTEGER NOT NULL,
	PRIMARY KEY (watch_id, name));
`

// watchRowLimit is the row count (present and tombstones) at which a watch pauses (backlog_full).
// Only an ack deletes tombstones, so a consumer that stops acking cannot grow the feed past it.
var watchRowLimit = 20000

var (
	errBacklogFull   = errors.New("backlog_full")
	errWatchInactive = errors.New("the watch is not initializing or active")
)

// watchObs is one Lstat of a name. The hub never follows a symlink and never opens a file.
type watchObs struct {
	Exists, Eligible   bool // Eligible: a regular file
	Ino, Size, MtimeNs int64
}

func (o watchObs) present() bool { return o.Exists && o.Eligible }

func obsFromInfo(fi fs.FileInfo) watchObs {
	o := watchObs{Exists: true, Eligible: fi.Mode().IsRegular(), Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		o.Ino = int64(st.Ino)
	}
	return o
}

// observeEntry returns the observation of dir/name. A missing name is an absent observation. Any
// other error is returned: the caller skips the name in this pass and keeps its row.
func observeEntry(dir, name string) (watchObs, error) {
	fi, err := os.Lstat(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return watchObs{}, nil
	}
	if err != nil {
		return watchObs{}, err
	}
	return obsFromInfo(fi), nil
}

// watchName reports whether a watch with this glob tracks name. Names that start with "." are
// never tracked (editor temp files, .DS_Store). glob was checked at ensure.
func watchName(name, glob string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	ok, _ := filepath.Match(glob, name)
	return ok
}

// settler holds the names whose changes have not settled. A name settles when two observations
// settle_ms apart are equal. It has no timers of its own: the fs loop asks next and due.
type settler struct {
	wait    time.Duration
	pending map[string]*settling
}

type settling struct {
	last watchObs
	due  time.Time
}

func newSettler(wait time.Duration) *settler {
	return &settler{wait: wait, pending: map[string]*settling{}}
}

// observe records a first or a new observation of name and starts its timer again.
func (s *settler) observe(name string, o watchObs, now time.Time) {
	s.pending[name] = &settling{last: o, due: now.Add(s.wait)}
}

// recheck takes the observation made when name's timer expired. Equal to the last one: the name
// settled and leaves the settler (true). Different: the timer starts again (false).
func (s *settler) recheck(name string, o watchObs, now time.Time) bool {
	p := s.pending[name]
	if p == nil {
		return false
	}
	if p.last == o {
		delete(s.pending, name)
		return true
	}
	p.last, p.due = o, now.Add(s.wait)
	return false
}

// due returns the names whose timers expired at now, in name order.
func (s *settler) due(now time.Time) []string {
	var out []string
	for name, p := range s.pending {
		if !p.due.After(now) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// next returns the earliest expiry.
func (s *settler) next() (time.Time, bool) {
	var t time.Time
	for _, p := range s.pending {
		if t.IsZero() || p.due.Before(t) {
			t = p.due
		}
	}
	return t, !t.IsZero()
}

// reset forgets every pending name (the watch left initializing or active).
func (s *settler) reset() { s.pending = map[string]*settling{} }

// watchEntry is one tracked name of a listing.
type watchEntry struct {
	Name    string
	Regular bool
}

// watchListing is a bounded listing of a watched directory.
type watchListing struct {
	Entries []watchEntry // the names that watchName tracks
	Count   int          // entries that the kqueue backend opens a descriptor for: all but sockets and named pipes
	Over    bool         // Count passed the limit, and the listing stopped there
	Chunks  int          // ReadDir calls
}

// listWatchDir reads dir in chunks and stops as soon as Count passes limit, so a large directory is
// never read whole. A listing error is returned as is: the caller commits nothing from it.
func listWatchDir(dir, glob string, limit, chunk int) (watchListing, error) {
	var l watchListing
	f, err := os.Open(dir)
	if err != nil {
		return l, err
	}
	defer f.Close()
	for {
		ents, err := f.ReadDir(chunk)
		l.Chunks++
		for _, e := range ents {
			t := e.Type()
			if t&(fs.ModeSocket|fs.ModeNamedPipe) == 0 {
				l.Count++
			}
			if watchName(e.Name(), glob) {
				l.Entries = append(l.Entries, watchEntry{Name: e.Name(), Regular: t.IsRegular()})
			}
		}
		if l.Count > limit {
			l.Over = true
			return l, nil
		}
		if errors.Is(err, io.EOF) {
			return l, nil
		}
		if err != nil {
			return l, err
		}
	}
}

// watchRecord is a watch as stored.
type watchRecord struct {
	Plugin, Key, ID, Path, Resolved, Glob string
	SettleMs                              int
	EmitExisting                          bool
	State, LastError                      string
	Cursor, Watermark                     int64
}

// createWatch stores a new watch and the eligible names of its ensure listing as pending initial
// names, in one transaction. An empty listing is active at once.
func (db *DB) createWatch(ctx context.Context, w watchRecord, names []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state := "initializing"
	if len(names) == 0 {
		state = "active"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO watches (plugin, key, watch_id, path, resolved, glob, settle_ms, emit_existing, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, w.Plugin, w.Key, w.ID, w.Path, w.Resolved, w.Glob, w.SettleMs, w.EmitExisting, state); err != nil {
		return err
	}
	for _, n := range names {
		if _, err := tx.ExecContext(ctx, `INSERT INTO watch_pending (watch_id, name, baseline) VALUES (?, ?, ?)`, w.ID, n, !w.EmitExisting); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) watchByID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (watchRecord, error) {
	var w watchRecord
	err := q.QueryRowContext(ctx, `SELECT plugin, key, watch_id, path, resolved, glob, settle_ms, emit_existing, state, last_error, cursor, watermark
		FROM watches WHERE watch_id = ?`, id).Scan(&w.Plugin, &w.Key, &w.ID, &w.Path, &w.Resolved, &w.Glob, &w.SettleMs, &w.EmitExisting,
		&w.State, &w.LastError, &w.Cursor, &w.Watermark)
	return w, err
}

func (db *DB) setWatchState(ctx context.Context, id, state, lastError string) error {
	_, err := db.ExecContext(ctx, `UPDATE watches SET state = ?, last_error = ? WHERE watch_id = ?`, state, lastError, id)
	return err
}

// commitSettled applies a settled observation of name to the feed. It returns whether the feed
// changed. Only an initializing or active watch commits (errWatchInactive). A new row in a watch at
// watchRowLimit returns errBacklogFull and changes nothing.
func (db *DB) commitSettled(ctx context.Context, id, name string, o watchObs) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM watches WHERE watch_id = ?`, id).Scan(&state); err != nil {
		return false, err
	}
	if state != "initializing" && state != "active" {
		return false, errWatchInactive
	}
	changed, err := applySettled(ctx, tx, id, name, o)
	if err != nil {
		return false, err
	}
	if state == "initializing" {
		var left int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM watch_pending WHERE watch_id = ?`, id).Scan(&left); err != nil {
			return false, err
		}
		if left == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE watches SET state = 'active' WHERE watch_id = ?`, id); err != nil {
				return false, err
			}
		}
	}
	return changed, tx.Commit()
}

func applySettled(ctx context.Context, tx *sql.Tx, id, name string, o watchObs) (bool, error) {
	var baseline bool
	err := tx.QueryRowContext(ctx, `SELECT baseline FROM watch_pending WHERE watch_id = ? AND name = ?`, id, name).Scan(&baseline)
	pending := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if pending {
		if _, err := tx.ExecContext(ctx, `DELETE FROM watch_pending WHERE watch_id = ? AND name = ?`, id, name); err != nil {
			return false, err
		}
	}
	var present, acked bool
	var ino, size, mtime int64
	err = tx.QueryRowContext(ctx, `SELECT present, acked_present, ino, size, mtime_ns FROM watch_rows WHERE watch_id = ? AND name = ?`, id, name).
		Scan(&present, &acked, &ino, &size, &mtime)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	switch {
	case !exists && !o.present():
		return false, nil // absent or not eligible, with no row: nothing to report
	case !exists:
		var rows int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM watch_rows WHERE watch_id = ?`, id).Scan(&rows); err != nil {
			return false, err
		}
		if rows >= watchRowLimit {
			return false, errBacklogFull
		}
		seq, err := watchSeq(ctx, tx, id)
		if err != nil {
			return false, err
		}
		// A baseline file is a file the consumer counts as known: acked and hidden until it changes.
		base := pending && baseline
		_, err = tx.ExecContext(ctx, `INSERT INTO watch_rows (watch_id, name, present, ino, size, mtime_ns, seq, created_seq, acked_present, baseline)
			VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?)`, id, name, o.Ino, o.Size, o.MtimeNs, seq, seq, base, base)
		return true, err
	case present && !o.present():
		seq, err := watchSeq(ctx, tx, id)
		if err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE watch_rows SET present = 0, seq = ?, baseline = 0 WHERE watch_id = ? AND name = ?`, seq, id, name)
		return true, err
	case present:
		if ino == o.Ino && size == o.Size && mtime == o.MtimeNs {
			return false, nil
		}
		seq, err := watchSeq(ctx, tx, id)
		if err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE watch_rows SET ino = ?, size = ?, mtime_ns = ?, seq = ?, baseline = 0 WHERE watch_id = ? AND name = ?`,
			o.Ino, o.Size, o.MtimeNs, seq, id, name)
		return true, err
	case o.present():
		// A tombstone becomes present: a new appearance, unknown to the consumer.
		seq, err := watchSeq(ctx, tx, id)
		if err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE watch_rows SET present = 1, ino = ?, size = ?, mtime_ns = ?, seq = ?, created_seq = ?, acked_present = 0, baseline = 0
			WHERE watch_id = ? AND name = ?`, o.Ino, o.Size, o.MtimeNs, seq, seq, id, name)
		return true, err
	}
	return false, nil // a tombstone that stays absent
}

// watchSeq takes the next seq and records it as the watch's watermark.
func watchSeq(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	seq, err := nextRev(tx)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE watches SET watermark = ? WHERE watch_id = ?`, seq, id)
	return seq, err
}

// feedKind derives a row's kind from its state and the consumer's acks.
func feedKind(present, acked bool) string {
	switch {
	case present && acked:
		return "changed"
	case present:
		return "created"
	case acked:
		return "removed"
	}
	return "created_removed"
}

// feedPage is one hub.watch.read reply.
type feedPage struct {
	Rows      []map[string]any
	Next      int64 // the highest seq scanned before the first visible row that was not returned
	Watermark int64
	More      bool
	State     string
}

// readFeed returns the rows with seq > after in seq order, in one snapshot. Baseline rows are
// scanned but hidden. The page stops before the first visible row that would pass limit rows or
// maxBytes of encoded rows, so next never passes a visible row that was not returned. The first
// visible row is always returned.
func (db *DB) readFeed(ctx context.Context, id string, after int64, limit, maxBytes int) (feedPage, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return feedPage{}, err
	}
	defer tx.Rollback()
	w, err := db.watchByID(ctx, tx, id)
	if err != nil {
		return feedPage{}, err
	}
	page := feedPage{Next: after, Watermark: w.Watermark, State: w.State}
	rows, err := tx.QueryContext(ctx, `SELECT name, present, acked_present, baseline, size, mtime_ns, seq, created_seq
		FROM watch_rows WHERE watch_id = ? AND seq > ? ORDER BY seq`, id, after)
	if err != nil {
		return feedPage{}, err
	}
	defer rows.Close()
	bytes := 0
	for rows.Next() {
		var name string
		var present, acked, baseline bool
		var size, mtime, seq, created int64
		if err := rows.Scan(&name, &present, &acked, &baseline, &size, &mtime, &seq, &created); err != nil {
			return feedPage{}, err
		}
		if baseline {
			page.Next = seq
			continue
		}
		r := map[string]any{"name": name, "path": filepath.Join(w.Path, name), "kind": feedKind(present, acked),
			"seq": revString(seq), "event_key": fmt.Sprintf("%s:%d", id, seq), "appearance_key": fmt.Sprintf("%s:%d", id, created)}
		if present {
			r["size"], r["mtime_ns"] = revString(size), revString(mtime)
		}
		b, err := json.Marshal(r)
		if err != nil {
			return feedPage{}, err
		}
		if len(page.Rows) > 0 && (len(page.Rows) == limit || bytes+len(b) > maxBytes) {
			page.More = true
			break
		}
		bytes += len(b)
		page.Rows = append(page.Rows, r)
		page.Next = seq
	}
	return page, rows.Err()
}

// errBadCursor refuses an ack past the watermark: it would mark rows the consumer has not read.
var errBadCursor = errors.New("bad_cursor")

// ackFeed records that the consumer handled every row with seq <= through. Rows up to through keep
// their present state as acked_present; tombstones up to through are deleted. An ack at or below
// the stored cursor changes nothing. It returns the cursor.
func (db *DB) ackFeed(ctx context.Context, id string, through int64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	w, err := db.watchByID(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if through > w.Watermark {
		return 0, errBadCursor
	}
	if through <= w.Cursor {
		return w.Cursor, nil
	}
	for _, q := range []string{
		`UPDATE watch_rows SET acked_present = present WHERE watch_id = ? AND seq <= ?`,
		`DELETE FROM watch_rows WHERE watch_id = ? AND seq <= ? AND present = 0`,
		`UPDATE watches SET cursor = ?2 WHERE watch_id = ?1`,
	} {
		if _, err := tx.ExecContext(ctx, q, id, through); err != nil {
			return 0, err
		}
	}
	return through, tx.Commit()
}
