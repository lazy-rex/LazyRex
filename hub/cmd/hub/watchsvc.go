package main

// File watch service (docs/hub.md, "File watches"): the hub.watch.* actions, one loop per watch,
// and the nudges. The settle rules and the feed are in watch.go.
//
// Each watch has one goroutine (watchRun). It owns the fsnotify watcher, the settler and every
// timer of the watch, so no timer runs outside it: closing the watcher resets the settler, and a
// later timer finds no pending name. The goroutine never holds h.mu.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	watchEntryLimit   = 1000 // listed entries per watch (watch.go, listWatchDir)
	watchHubBudget    = 4000 // descriptors for all watches; at most half the soft RLIMIT_NOFILE
	watchPerPlugin    = 8
	watchChunk        = 256
	watchPageBytes    = 256 << 10
	watchDefaultLimit = 100
	watchMaxLimit     = 500
)

var (
	watchRetry       = 30 * time.Second
	watchScanEvery   = 5 * time.Minute
	watchNudgeDelays = []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}
	watchKeyForm     = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
)

// watchSvc holds the running watch loops and the descriptor budget.
type watchSvc struct {
	h       *Hub
	mu      sync.Mutex
	live    map[string]*liveWatch // by watch_id
	used    map[string]int        // descriptors counted by each open watch
	fdLimit int
}

type liveWatch struct {
	id, plugin, key string
	cmd             chan string // "nudge" (the plugin registered), "acked"
	stop, done      chan struct{}
}

// watchStart creates the tables and starts a loop for each stored watch of a configured plugin.
// A watch of a plugin that the config omits becomes inactive and stays stored.
func (h *Hub) watchStart(ctx context.Context) error {
	if _, err := h.db.ExecContext(ctx, watchSchema); err != nil {
		return err
	}
	h.watches = &watchSvc{h: h, live: map[string]*liveWatch{}, used: map[string]int{}, fdLimit: watchDescriptorLimit()}
	rows, err := h.db.QueryContext(ctx, `SELECT watch_id, plugin, key FROM watches`)
	if err != nil {
		return err
	}
	type row struct{ id, plugin, key string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.plugin, &r.key); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	for _, r := range all {
		if h.plugins[r.plugin] == nil {
			if err := h.db.setWatchState(ctx, r.id, "inactive", ""); err != nil {
				return err
			}
			continue
		}
		h.watches.start(r.id, r.plugin, r.key)
	}
	return nil
}

func watchDescriptorLimit() int {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return watchHubBudget / 4
	}
	n := rl.Cur / 2
	if n > watchHubBudget {
		return watchHubBudget
	}
	return int(n)
}

// reserve sets the descriptors that watch id counts, if the hub budget allows it.
func (s *watchSvc) reserve(id string, n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := n
	for other, c := range s.used {
		if other != id {
			total += c
		}
	}
	if total > s.fdLimit {
		return false
	}
	s.used[id] = n
	return true
}

// fits reports whether n more descriptors fit in the hub budget now.
func (s *watchSvc) fits(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.used {
		n += c
	}
	return n <= s.fdLimit
}

func (s *watchSvc) release(id string) {
	s.mu.Lock()
	delete(s.used, id)
	s.mu.Unlock()
}

func (s *watchSvc) start(id, plugin, key string) {
	lw := &liveWatch{id: id, plugin: plugin, key: key, cmd: make(chan string, 8), stop: make(chan struct{}), done: make(chan struct{})}
	s.mu.Lock()
	s.live[id] = lw
	s.mu.Unlock()
	r := &watchRun{h: s.h, svc: s, lw: lw, set: newSettler(time.Second)}
	go r.loop()
}

// stopWatch ends a watch's loop and waits for it.
func (s *watchSvc) stopWatch(id string) {
	s.mu.Lock()
	lw := s.live[id]
	delete(s.live, id)
	s.mu.Unlock()
	if lw != nil {
		close(lw.stop)
		<-lw.done
	}
}

// send gives a loop a command without waiting. A full queue drops it: the loop repeats nudges
// on its own schedule, and the next scan resumes a paused watch.
func (s *watchSvc) send(id, cmd string) {
	s.mu.Lock()
	lw := s.live[id]
	s.mu.Unlock()
	if lw == nil {
		return
	}
	select {
	case lw.cmd <- cmd:
	default:
	}
}

// pluginRegistered restarts the nudge schedule of the plugin's watches.
func (s *watchSvc) pluginRegistered(plugin string) {
	s.mu.Lock()
	var ids []string
	for id, lw := range s.live {
		if lw.plugin == plugin {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.send(id, "nudge")
	}
}

// watchRoots returns the plugin's configured watch_roots.
func (h *Hub) watchRoots(plugin string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pl := h.plugins[plugin]; pl != nil {
		return pl.WatchRoots
	}
	return nil
}

// insideRoots reports whether the resolved path p is a root or inside one. Each root is resolved
// now, so a root that appears later counts, and a root that is a symlink counts as its target.
func insideRoots(p string, roots []string) bool {
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		// Rel, not a prefix test: a prefix test with a separator fails for the root "/" ("//").
		rel, err := filepath.Rel(rr, p)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func devIno(fi fs.FileInfo) (uint64, uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), st.Ino
	}
	return 0, 0
}

// watchRun is the state of one watch's loop.
type watchRun struct {
	h       *Hub
	svc     *watchSvc
	lw      *liveWatch
	rec     watchRecord
	fsw     *fsnotify.Watcher
	set     *settler
	dev     uint64
	ino     uint64
	retryAt time.Time
	nudgeAt time.Time
	nudges  int  // nudges sent since the schedule started
	shrink  bool // the last state was too_large: reopen only at 90% of the limit
}

func (r *watchRun) loop() {
	defer close(r.lw.done)
	defer r.closeFS()
	scan := time.NewTicker(watchScanEvery)
	defer scan.Stop()
	r.open()
	r.nudgeAt = time.Now() // rows may wait from before a restart; the tick checks
	for {
		var settleC, retryC, nudgeC <-chan time.Time
		if t, ok := r.set.next(); ok {
			settleC = time.After(time.Until(t))
		}
		if !r.retryAt.IsZero() {
			retryC = time.After(time.Until(r.retryAt))
		}
		if !r.nudgeAt.IsZero() {
			nudgeC = time.After(time.Until(r.nudgeAt))
		}
		var events chan fsnotify.Event
		var errs chan error
		if r.fsw != nil {
			events, errs = r.fsw.Events, r.fsw.Errors
		}
		select {
		case <-r.lw.stop:
			return
		case c := <-r.lw.cmd:
			r.command(c)
		case ev, ok := <-events:
			if ok {
				r.event(ev)
			}
		case err, ok := <-errs:
			if ok {
				r.inactive("error", "watcher: "+err.Error(), true)
			}
		case <-settleC:
			r.settle()
		case <-retryC:
			r.retryAt = time.Time{}
			r.open()
		case <-scan.C:
			r.periodic()
		case <-nudgeC:
			r.nudgeTick()
		}
	}
}

func (r *watchRun) ctx() context.Context { return context.Background() }

func (r *watchRun) state() string {
	w, err := r.h.db.watchByID(r.ctx(), r.h.db, r.lw.id)
	if err != nil {
		return ""
	}
	return w.State
}

func (r *watchRun) setState(state, msg string) {
	if err := r.h.db.setWatchState(r.ctx(), r.lw.id, state, msg); err != nil {
		log.Printf("watch %s/%s: state %s: %v", r.lw.plugin, r.lw.key, state, err)
	}
	r.h.ev.emit("watch_state", map[string]any{"plugin": r.lw.plugin, "key": r.lw.key, "watch_id": r.lw.id, "state": state, "error": msg})
}

// closeFS closes the watcher, releases its descriptors and forgets every pending name.
func (r *watchRun) closeFS() {
	if r.fsw != nil {
		r.fsw.Close()
		r.fsw = nil
	}
	r.svc.release(r.lw.id)
	r.set.reset()
}

// inactive closes the watch and records a state that commits nothing. retry: open again after
// watchRetry. Without retry, the 5-minute scan (too_large, backlog_full) or a hub start
// (not_granted) opens it again.
func (r *watchRun) inactive(state, msg string, retry bool) {
	r.closeFS()
	r.setState(state, msg)
	r.shrink = state == "too_large"
	if retry {
		r.retryAt = time.Now().Add(watchRetry)
	}
}

// open checks the grant and the directory, opens the watcher, and scans. Any failure leaves the
// watch closed with its state.
func (r *watchRun) open() {
	r.closeFS()
	rec, err := r.h.db.watchByID(r.ctx(), r.h.db, r.lw.id)
	if err != nil {
		log.Printf("watch %s/%s: %v", r.lw.plugin, r.lw.key, err)
		return
	}
	r.rec = rec
	r.set = newSettler(time.Duration(rec.SettleMs) * time.Millisecond)
	ok, state, msg := r.checkGrant()
	if !ok {
		r.inactive(state, msg, state != "not_granted")
		return
	}
	fi, err := os.Lstat(rec.Resolved)
	if err != nil || !fi.IsDir() {
		r.inactive("missing", "", true)
		return
	}
	dev, ino := devIno(fi)
	limit := watchEntryLimit
	if r.shrink {
		limit = watchEntryLimit * 9 / 10
	}
	l, err := listWatchDir(rec.Resolved, rec.Glob, limit, watchChunk)
	switch {
	case err != nil:
		r.inactive("error", "list: "+err.Error(), true)
		return
	case l.Over:
		r.inactive("too_large", fmt.Sprintf("more than %d entries", limit), false)
		return
	case !r.svc.reserve(r.lw.id, l.Count+1):
		r.inactive("too_large", "the hub descriptor budget is used up", false)
		return
	}
	fsw, err := fsnotify.NewWatcher()
	if err == nil {
		err = fsw.Add(rec.Resolved)
		if err != nil {
			fsw.Close()
		}
	}
	if err != nil {
		r.inactive("error", "watch: "+err.Error(), true)
		return
	}
	r.fsw = fsw
	if fi, err := os.Lstat(rec.Resolved); err != nil || !fi.IsDir() {
		r.inactive("missing", "", true)
		return
	} else if d, i := devIno(fi); d != dev || i != ino {
		r.inactive("error", "the directory was replaced while the watch opened", true)
		return
	}
	r.dev, r.ino, r.shrink = dev, ino, false
	_, pending, err := r.h.db.watchRowStates(r.ctx(), r.lw.id)
	if err != nil {
		r.inactive("error", err.Error(), true)
		return
	}
	if len(pending) > 0 {
		r.setState("initializing", "")
	} else {
		r.setState("active", "")
	}
	r.scan()
}

// checkGrant resolves the watch path again: it must still be the stored resolved path, inside a
// root of the current config.
func (r *watchRun) checkGrant() (bool, string, string) {
	resolved, err := filepath.EvalSymlinks(r.rec.Path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false, "missing", ""
	}
	if err != nil {
		return false, "error", "resolve: " + err.Error()
	}
	if resolved != r.rec.Resolved || !insideRoots(resolved, r.h.watchRoots(r.lw.plugin)) {
		return false, "not_granted", "the path resolves to " + resolved
	}
	return true, "", ""
}

// checkOpen confirms that the open directory is still the watched one. If not, it closes the
// watch and opens it again now (open classifies: missing, not_granted, or a new directory).
func (r *watchRun) checkOpen() bool {
	ok, _, _ := r.checkGrant()
	if ok {
		if fi, err := os.Lstat(r.rec.Resolved); err == nil && fi.IsDir() {
			if d, i := devIno(fi); d == r.dev && i == r.ino {
				return true
			}
		}
	}
	r.closeFS()
	r.retryAt = time.Now()
	return false
}

// scan lists the directory and makes a first observation of each name that differs from its row,
// of each row whose name is gone, and of each pending initial name. It commits nothing.
func (r *watchRun) scan() {
	if r.fsw == nil || !r.checkOpen() {
		return
	}
	l, ok := r.recount()
	if !ok {
		return
	}
	rows, pending, err := r.h.db.watchRowStates(r.ctx(), r.lw.id)
	if err != nil {
		r.inactive("error", err.Error(), true)
		return
	}
	now := time.Now()
	seen := map[string]bool{}
	for _, e := range l.Entries {
		seen[e.Name] = true
		o, err := observeEntry(r.rec.Resolved, e.Name)
		if err != nil {
			continue // the row stays; the next scan tries again
		}
		row, has := rows[e.Name]
		if pending[e.Name] || rowDiffers(row, has, o) {
			r.set.observe(e.Name, o, now)
		}
	}
	for name, row := range rows {
		if !seen[name] && row.present() {
			r.set.observe(name, watchObs{}, now)
		}
	}
	for name := range pending {
		if !seen[name] {
			r.set.observe(name, watchObs{}, now)
		}
	}
}

// recount lists the directory and checks the limits again. Over a limit, the watch closes.
func (r *watchRun) recount() (watchListing, bool) {
	l, err := listWatchDir(r.rec.Resolved, r.rec.Glob, watchEntryLimit, watchChunk)
	switch {
	case err != nil:
		r.inactive("error", "list: "+err.Error(), true)
		return l, false
	case l.Over:
		r.inactive("too_large", fmt.Sprintf("more than %d entries", watchEntryLimit), false)
		return l, false
	case !r.svc.reserve(r.lw.id, l.Count+1):
		r.inactive("too_large", "the hub descriptor budget is used up", false)
		return l, false
	}
	return l, true
}

func (r *watchRun) event(ev fsnotify.Event) {
	if ev.Op&^fsnotify.Chmod == 0 {
		return // Spotlight and backups send many
	}
	if filepath.Clean(ev.Name) == r.rec.Resolved {
		r.closeFS() // the directory itself was removed or renamed
		r.retryAt = time.Now()
		return
	}
	if filepath.Dir(ev.Name) != r.rec.Resolved {
		return
	}
	if ev.Has(fsnotify.Create) {
		if _, ok := r.recount(); !ok {
			return
		}
	}
	name := filepath.Base(ev.Name)
	if !watchName(name, r.rec.Glob) {
		return
	}
	o, err := observeEntry(r.rec.Resolved, name)
	if err != nil {
		return
	}
	r.set.observe(name, o, time.Now())
}

// settle rechecks every name whose timer expired, and commits the names that settled.
func (r *watchRun) settle() {
	now := time.Now()
	due := r.set.due(now)
	if len(due) == 0 {
		return
	}
	if !r.checkOpen() {
		return // a directory renamed away makes no tombstones
	}
	for _, name := range due {
		o, err := observeEntry(r.rec.Resolved, name)
		if err != nil {
			r.set.drop(name)
			continue
		}
		if !r.set.recheck(name, o, now) {
			continue
		}
		changed, err := r.h.db.commitSettled(r.ctx(), r.lw.id, name, o)
		switch {
		case errors.Is(err, errBacklogFull):
			r.inactive("backlog_full", fmt.Sprintf("%d rows", watchRowLimit), false)
			return
		case errors.Is(err, errWatchInactive), errors.Is(err, sql.ErrNoRows):
			r.closeFS()
			return
		case err != nil:
			r.inactive("error", "commit: "+err.Error(), true)
			return
		}
		if changed {
			r.startNudges()
		}
	}
}

// periodic is the 5-minute scan. A paused or too large watch tries to open again.
func (r *watchRun) periodic() {
	if r.fsw != nil {
		r.scan()
		return
	}
	if s := r.state(); s == "too_large" || s == "backlog_full" {
		r.open()
	}
}

func (r *watchRun) command(c string) {
	switch c {
	case "nudge":
		r.nudges, r.nudgeAt = 0, time.Now()
	case "acked":
		if r.fsw == nil && r.state() == "backlog_full" {
			r.open()
		}
	}
}

// startNudges sends a nudge now and starts the repeat schedule.
func (r *watchRun) startNudges() {
	r.nudges, r.nudgeAt = 0, time.Now()
}

// nudgeTick sends a nudge while rows wait (cursor below the watermark) and the plugin is
// registered, then schedules the next one: 5 s, 30 s, then every 5 minutes.
func (r *watchRun) nudgeTick() {
	r.nudgeAt = time.Time{}
	w, err := r.h.db.watchByID(r.ctx(), r.h.db, r.lw.id)
	if err != nil || w.Cursor >= w.Watermark {
		return
	}
	client := r.h.pluginClient(r.lw.plugin)
	if client == "" {
		return // hub.register starts the schedule again
	}
	r.sendNudge(client)
	r.nudgeAt = time.Now().Add(watchNudgeDelays[min(r.nudges, len(watchNudgeDelays)-1)])
	r.nudges++
}

func (r *watchRun) sendNudge(client string) {
	plugin, key, id := r.lw.plugin, r.lw.key, r.lw.id
	if f := r.h.takeFault(func(f *testFault) bool {
		return f.Point == "watch-nudge" && f.Plugin == plugin && (f.Key == "" || f.Key == key)
	}); f != nil {
		return // drop: the nudge is lost
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := r.h.rex.Call(ctx, "client.perform", map[string]any{"client_id": client, "action": "hub.changed",
			"args": map[string]any{"what": "watch", "key": key, "watch_id": id}, "timeout_ms": 3000}, nil)
		if err != nil && !strings.Contains(err.Error(), "unknown action") && !strings.Contains(err.Error(), "not_found") {
			log.Printf("watch nudge %s/%s: %v", plugin, key, err)
		}
	}()
}

// watchAction handles hub.watch.* for the registered plugin ns.
func (h *Hub) watchAction(ctx context.Context, ns, action string, args map[string]any) (any, error) {
	switch action {
	case "hub.watch.ensure":
		return h.watchEnsure(ctx, ns, args)
	case "hub.watch.list":
		return h.watchList(ctx, ns)
	}
	key, _ := args["key"].(string)
	id, _ := args["watch_id"].(string)
	if key == "" || id == "" {
		return nil, bad("key and watch_id are required")
	}
	var current string
	err := h.db.QueryRowContext(ctx, `SELECT watch_id FROM watches WHERE plugin = ? AND key = ?`, ns, key).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if action == "hub.watch.remove" {
		if current != id {
			return map[string]any{"removed": false}, nil // gone already, or a later watch: a late remove changes nothing
		}
		h.watches.stopWatch(id)
		if err := h.db.deleteWatch(ctx, id); err != nil {
			return nil, err
		}
		return map[string]any{"removed": true}, nil
	}
	if current != id {
		return nil, refuse("stale_watch", "watch %s is not the current watch of key %s", id, key)
	}
	switch action {
	case "hub.watch.read":
		w, err := h.db.watchByID(ctx, h.db, id)
		if err != nil {
			return nil, err
		}
		after := w.Cursor
		if v, ok := args["after"]; ok {
			if after, err = parseSeq(v); err != nil {
				return nil, err
			}
		}
		limit := watchDefaultLimit
		if v, ok := args["limit"]; ok {
			n, ok := v.(float64)
			if !ok || n < 1 || n > watchMaxLimit || n != float64(int(n)) {
				return nil, bad("limit is an integer from 1 to %d", watchMaxLimit)
			}
			limit = int(n)
		}
		p, err := h.db.readFeed(ctx, id, after, limit, watchPageBytes)
		if err != nil {
			return nil, err
		}
		rows := make([]any, 0, len(p.Rows))
		for _, r := range p.Rows {
			rows = append(rows, r)
		}
		return map[string]any{"rows": rows, "next": revString(p.Next), "watermark": revString(p.Watermark), "more": p.More, "state": p.State}, nil
	case "hub.watch.ack":
		through, err := parseSeq(args["through"])
		if err != nil {
			return nil, err
		}
		cursor, err := h.db.ackFeed(ctx, id, through)
		if errors.Is(err, errBadCursor) {
			return nil, refuse("bad_cursor", "through %d is past the watermark", through)
		}
		if err != nil {
			return nil, err
		}
		h.watches.send(id, "acked")
		return map[string]any{"cursor": revString(cursor)}, nil
	}
	return nil, bad("unknown action %s", action)
}

// parseSeq reads a sequence value: a decimal string (Lua numbers are doubles).
func parseSeq(v any) (int64, error) {
	s, ok := v.(string)
	n, err := strconv.ParseInt(s, 10, 64)
	if !ok || err != nil || n < 0 {
		return 0, bad("a sequence value is a decimal string")
	}
	return n, nil
}

var watchEnsureFields = map[string]bool{"key": true, "path": true, "glob": true, "settle_ms": true, "emit_existing": true}

func (h *Hub) watchEnsure(ctx context.Context, ns string, args map[string]any) (any, error) {
	for k := range args {
		if k == "recursive" {
			return nil, bad("recursive watches are not supported: one directory per watch")
		}
		if !watchEnsureFields[k] {
			return nil, bad("unknown field %s", k)
		}
	}
	w := watchRecord{Plugin: ns, Glob: "*", SettleMs: 1000}
	w.Key, _ = args["key"].(string)
	w.Path, _ = args["path"].(string)
	if !watchKeyForm.MatchString(w.Key) {
		return nil, bad("key must match [a-z0-9_.-]{1,64}")
	}
	if !filepath.IsAbs(w.Path) || filepath.Clean(w.Path) != w.Path {
		return nil, bad("path must be absolute and clean")
	}
	if v, ok := args["glob"]; ok {
		g, _ := v.(string)
		if _, err := filepath.Match(g, "x"); g == "" || err != nil {
			return nil, bad("glob is not a valid pattern")
		}
		w.Glob = g
	}
	if v, ok := args["settle_ms"]; ok {
		n, ok := v.(float64)
		if !ok || n < 200 || n > 60000 || n != float64(int(n)) {
			return nil, bad("settle_ms is an integer from 200 to 60000")
		}
		w.SettleMs = int(n)
	}
	if v, ok := args["emit_existing"]; ok {
		b, ok := v.(bool)
		if !ok {
			return nil, bad("emit_existing is a boolean")
		}
		w.EmitExisting = b
	}
	for attempt := 0; ; attempt++ {
		reply, found, err := h.watchExisting(ctx, w)
		if found || err != nil {
			return reply, err
		}
		var n int
		if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM watches WHERE plugin = ?`, ns).Scan(&n); err != nil {
			return nil, err
		}
		if n >= watchPerPlugin {
			return nil, refuse("watch_limit", "plugin %s has %d watches", ns, n)
		}
		roots := h.watchRoots(ns)
		if len(roots) == 0 {
			return nil, refuse("watch_not_granted", "the hub config gives plugin %s no watch_roots", ns)
		}
		resolved, err := filepath.EvalSymlinks(w.Path)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, refuse("watch_missing", "%s does not exist", w.Path)
		} else if err != nil {
			return nil, refuse("watch_error", "%v", err)
		}
		if !insideRoots(resolved, roots) {
			return nil, refuse("watch_not_granted", "%s is not inside the watch_roots of plugin %s", resolved, ns)
		}
		if fi, err := os.Lstat(resolved); err != nil || !fi.IsDir() {
			return nil, refuse("watch_missing", "%s is not a directory", w.Path)
		}
		l, err := listWatchDir(resolved, w.Glob, watchEntryLimit, watchChunk)
		switch {
		case err != nil:
			return nil, refuse("watch_error", "list: %v", err)
		case l.Over:
			return nil, refuse("watch_too_large", "%s has more than %d entries", w.Path, watchEntryLimit)
		case !h.watches.fits(l.Count + 1):
			return nil, refuse("watch_too_large", "the hub descriptor budget is used up")
		}
		var names []string
		for _, e := range l.Entries {
			if e.Regular {
				names = append(names, e.Name)
			}
		}
		b := make([]byte, 16)
		rand.Read(b)
		w.ID, w.Resolved = hex.EncodeToString(b), resolved
		if err := h.db.createWatch(ctx, w, names); err != nil {
			if attempt == 0 && strings.Contains(err.Error(), "UNIQUE") {
				continue // a concurrent ensure of the same key won: compare with it
			}
			return nil, err
		}
		h.watches.start(w.ID, ns, w.Key)
		reply, _, err = h.watchExisting(ctx, w)
		return reply, err
	}
}

// watchExisting returns the reply for the stored watch of w's key, or a conflict if its spec differs.
func (h *Hub) watchExisting(ctx context.Context, w watchRecord) (any, bool, error) {
	var id string
	err := h.db.QueryRowContext(ctx, `SELECT watch_id FROM watches WHERE plugin = ? AND key = ?`, w.Plugin, w.Key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	cur, err := h.db.watchByID(ctx, h.db, id)
	if err != nil {
		return nil, false, err
	}
	if cur.Path != w.Path || cur.Glob != w.Glob || cur.SettleMs != w.SettleMs || cur.EmitExisting != w.EmitExisting {
		return nil, true, refuse("watch_conflict", "key %s has another spec; remove it first", w.Key)
	}
	return map[string]any{"key": cur.Key, "watch_id": cur.ID, "state": cur.State, "cursor": revString(cur.Cursor), "watermark": revString(cur.Watermark)}, true, nil
}

func (h *Hub) watchList(ctx context.Context, ns string) (any, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT key, watch_id, path, glob, state, last_error, cursor, watermark FROM watches WHERE plugin = ? ORDER BY key`, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []any{}
	for rows.Next() {
		var key, id, path, glob, state, lastErr string
		var cursor, watermark int64
		if err := rows.Scan(&key, &id, &path, &glob, &state, &lastErr, &cursor, &watermark); err != nil {
			return nil, err
		}
		list = append(list, map[string]any{"key": key, "watch_id": id, "path": path, "glob": glob, "state": state,
			"last_error": lastErr, "cursor": revString(cursor), "watermark": revString(watermark)})
	}
	return map[string]any{"watches": list}, rows.Err()
}
