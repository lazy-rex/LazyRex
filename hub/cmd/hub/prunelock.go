package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// The prune lock (plugin-sources.md, section 4, "Prune"): <state>/plugins/prune.lock. Each
// admitted start holds a shared flock on it from before the resolution that reads the lock file
// until spawn has saved the run in hub.db, or until the start ends without a spawn. `plugin prune`
// takes an exclusive flock without waiting. So a cache entry that a start can use is always in a
// lock file that prune reads, or in a stored run.
//
// Each start opens the file itself: a flock belongs to the open file description, so the release
// of one start does not release the flock of another start.

// startLease is the shared flock of one start. The zero value and nil hold nothing.
type startLease struct{ f *os.File }

// takeStartLease opens the prune lock and takes a shared flock. It waits while prune holds the
// exclusive flock (prune holds it only while it reads and deletes). A hub with no state directory
// (a unit test hub) uses no cache, and takes nothing.
func (h *Hub) takeStartLease() (*startLease, error) {
	if h.cfg.State == "" {
		return &startLease{}, nil
	}
	dir := filepath.Join(h.cfg.State, "plugins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("the prune lock: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "prune.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the prune lock: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("the prune lock: flock: %w", err)
	}
	return &startLease{f: f}, nil
}

// release closes the flock. It can be called again, and on nil.
func (l *startLease) release() {
	if l != nil && l.f != nil {
		l.f.Close()
		l.f = nil
	}
}
