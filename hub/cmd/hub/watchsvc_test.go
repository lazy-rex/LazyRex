package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newWatchRunTest opens a real watch on a temp directory without its loop: the test drives
// settle itself, so it controls whether the watcher's events were handled.
func newWatchRunTest(t *testing.T) (*watchRun, string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "root")
	dir := filepath.Join(root, "inbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(filepath.Join(tmp, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(watchSchema); err != nil {
		t.Fatal(err)
	}
	ev, err := openEvents(filepath.Join(tmp, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Hub{db: db, ev: ev, plugins: map[string]*Plugin{"p": {Name: "p", entry: &configEntry{WatchRoots: []string{root}}}}}
	h.watches = &watchSvc{h: h, live: map[string]*liveWatch{}, used: map[string]int{}, fdLimit: watchHubBudget}
	w := watchRecord{Plugin: "p", Key: "inbox", ID: "w1", Path: dir, Resolved: dir, Glob: "*", SettleMs: 200}
	if err := db.createWatch(t.Context(), w, nil); err != nil {
		t.Fatal(err)
	}
	r := &watchRun{h: h, svc: h.watches, lw: &liveWatch{id: "w1", plugin: "p", key: "inbox"}, set: newSettler(time.Second)}
	r.open()
	t.Cleanup(r.closeFS)
	if r.fsw == nil {
		t.Fatalf("the watch did not open: %s", r.state())
	}
	return r, dir
}

func (r *watchRun) rowsOf(t *testing.T) map[string]watchObs {
	rows, _, err := r.h.db.watchRowStates(t.Context(), r.lw.id)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A directory renamed away while a timer waits: the timer commits nothing, also when the
// watcher's event for the directory was not handled yet (design v3, C).
func TestWatchSettleAfterDirectoryRenamedAwayCommitsNothing(t *testing.T) {
	r, dir := newWatchRunTest(t)
	f := filepath.Join(dir, "a.md")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, _ := observeEntry(dir, "a.md")
	if _, err := r.h.db.commitSettled(t.Context(), "w1", "a.md", o); err != nil {
		t.Fatal(err)
	}
	os.Remove(f)
	r.set.observe("a.md", watchObs{}, time.Now().Add(-time.Hour)) // due now, and absent twice
	if err := os.Rename(dir, dir+".away"); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if row := r.rowsOf(t)["a.md"]; !row.present() {
		t.Fatalf("a removal was committed after the directory moved away: %+v", row)
	}
	if r.fsw != nil {
		t.Fatal("the watch stayed open")
	}
}

// The same timer with the directory in place commits the removal (the control case).
func TestWatchSettleCommitsARemoval(t *testing.T) {
	r, dir := newWatchRunTest(t)
	f := filepath.Join(dir, "a.md")
	os.WriteFile(f, []byte("x"), 0o600)
	o, _ := observeEntry(dir, "a.md")
	if _, err := r.h.db.commitSettled(t.Context(), "w1", "a.md", o); err != nil {
		t.Fatal(err)
	}
	os.Remove(f)
	r.set.observe("a.md", watchObs{}, time.Now().Add(-time.Hour))
	r.settle()
	if row, ok := r.rowsOf(t)["a.md"]; !ok || row.present() {
		t.Fatalf("no tombstone: %+v %v", row, ok)
	}
}

// A root grants itself and every directory below it, also the filesystem root, but not a sibling
// that shares its prefix (review of 2367084).
func TestInsideRootsContainment(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p     string
		roots []string
		want  bool
	}{
		{dir, []string{"/"}, true},
		{dir, []string{dir}, true},
		{filepath.Join(dir, "a", "b"), []string{dir}, true},
		{dir + "-sibling", []string{dir}, false},
		{filepath.Dir(dir), []string{dir}, false},
	} {
		if got := insideRoots(c.p, c.roots); got != c.want {
			t.Errorf("insideRoots(%q, %q) = %v", c.p, c.roots, got)
		}
	}
}

// The budget counts the backend's own descriptors: fsnotify's kqueue backend opens a kqueue and a
// close pipe (two ends) for each watcher, besides the directory and its entries (review of 2367084).
func TestWatchBudgetCountsBackendDescriptors(t *testing.T) {
	r, _ := newWatchRunTest(t)
	if got := r.svc.used[r.lw.id]; got != 4 {
		t.Fatalf("an empty directory reserved %d descriptors, want 4", got)
	}
	r.closeFS()
	r.svc.fdLimit = 3
	r.open()
	if r.fsw != nil || r.state() != "too_large" {
		t.Fatalf("opened with a budget of 3: open %v, state %s", r.fsw != nil, r.state())
	}
	if r.svc.fits(4) {
		t.Fatal("fits(4) with a budget of 3")
	}
}

// The 5-minute scan (periodic, called directly) closes a watch that grew past the entry limit, and
// opens it again once the directory has at most 90% of the limit; the scan then settles every file.
// From the reviewer's test of 2367084.
func TestWatchPeriodicReopensAfterShrink(t *testing.T) {
	r, dir := newWatchRunTest(t)
	for i := 0; i < 1001; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.periodic()
	if r.fsw != nil || r.state() != "too_large" {
		t.Fatalf("growth: open %v, state %s", r.fsw != nil, r.state())
	}
	for i := 0; i < 2; i++ { // 999 entries: under the limit, over 90% of it
		os.Remove(filepath.Join(dir, fmt.Sprintf("f%04d", i)))
	}
	r.periodic()
	if r.fsw != nil {
		t.Fatal("reopened above 90% of the limit")
	}
	for i := 2; i < 102; i++ {
		os.Remove(filepath.Join(dir, fmt.Sprintf("f%04d", i)))
	}
	r.periodic()
	if r.fsw == nil {
		t.Fatalf("did not reopen: %s", r.state())
	}
	time.Sleep(250 * time.Millisecond)
	r.settle()
	if got := len(r.rowsOf(t)); got != 899 {
		t.Fatalf("settled %d files, want 899", got)
	}
}
