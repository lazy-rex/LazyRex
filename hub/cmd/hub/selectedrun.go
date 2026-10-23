package main

// The selected run (the P1 plan r6): the run that an automatic start uses. An explicit start (hub
// start recovery, hub.admin.start, a reload, a replace) resolves the current pin and selects its
// run: spawn saves it with the run, and a start that waits saves it before its lease ends. An
// automatic start (a crash respawn, a restart after a session loss, a wake) never reads a pin: it
// runs the selected run, so a lock change takes effect at the next explicit start.
//
// The lifetime: a crash, a failed spawn, a retirement and a session loss keep it; a stop
// (markStopped), the entry's removal and hub start (loadKept) clear it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// selectedRun is the plugins row selected_run: the run, and the entry that selected it
// (entryIdentity). An automatic start under another entry does not use it.
type selectedRun struct {
	Entry string    `json:"entry"`
	Run   pluginRun `json:"run"`
}

// startAutomatic is the start path of an automatic start. The caller holds pl.lifecycle. The order:
//  1. Load the row (with selected_run) and resolve the spawn intents, as startPlugin does. A kept
//     record takes the kept path; a record that is not desired running starts nothing.
//  2. Take the start lease: from before the run check until spawn saves the run.
//  3. Admission: the selected run must come from the record's current entry. Else it refuses: "no
//     run is recorded; start it again". The current candidate and the current graph do not count.
//  4. The run check (checkRun) and the selected graph: a cycle among the selected runs that
//     contains the plugin refuses, and so does a dependency that no entry gives. A refusal gives
//     state refused, so no backoff retries it (respawnRetries).
//  5. Retire the recorded instance, then spawn the selected run, or wait (startOrWait) while a
//     dependency is not registered. A refused or stopped dependency shows in the waiting text.
func (h *Hub) startAutomatic(ctx context.Context, pl *Plugin) error {
	pl.attempted = true
	h.startFault(pl.Name, "hold-before")
	h.mu.Lock()
	err := h.loadPluginRow(ctx, pl)
	before := pl.waiting
	kept := pl.entry == nil
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		changed := !slices.Equal(before, pl.waiting)
		h.mu.Unlock()
		if changed {
			go h.changed("plugins")
		}
	}()
	if err != nil {
		return err
	}
	if kept {
		return h.recoverKept(ctx, pl)
	}
	if err := h.resolveIntents(ctx, pl); err != nil {
		return err
	}
	if pl.Desired != "running" {
		return nil
	}
	lease, err := h.takeStartLease()
	if err != nil {
		return fmt.Errorf("plugin %s: %w", pl.Name, err)
	}
	defer lease.release()
	h.mu.Lock()
	e, sel := pl.entry, pl.selected
	if e == nil {
		h.mu.Unlock()
		return h.recoverKept(ctx, pl)
	}
	if sel == nil || sel.Entry != entryIdentity(e) {
		h.mu.Unlock()
		return h.refuseStart(pl, fmt.Errorf("plugin %s: no run is recorded; start it again", pl.Name))
	}
	run := sel.Run
	h.mu.Unlock()
	if err := h.checkRun(&run); err != nil {
		return h.refuseStart(pl, err)
	}
	h.mu.Lock()
	if cycle := h.selectedCycle(pl.Name, run.Depends); cycle != nil {
		h.mu.Unlock()
		return h.refuseStart(pl, fmt.Errorf("plugin %s: dependency cycle among the selected runs (%s); start it explicitly", pl.Name, strings.Join(cycle, " -> ")))
	}
	for _, d := range run.Depends {
		if dp := h.plugins[d]; dp == nil || dp.entry == nil {
			h.mu.Unlock()
			return h.refuseStart(pl, fmt.Errorf("plugin %s: dependency %s of run %s is not available", pl.Name, d, runIdentity(&run)))
		}
	}
	pl.cancelRespawnLocked() // the start passed its refusals: it owns the plugin now (respawn.go)
	pl.refusal = ""
	if pl.state != "waiting" {
		pl.state = ""
	}
	h.mu.Unlock()
	h.startFault(pl.Name, "hold-after")
	if pl.BlockID != "" {
		ok, err := h.retire(ctx, pl)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("plugin %s: the recorded instance is not retired (see the plugin_conflict or plugin_stopped event)", pl.Name)
		}
	}
	err = h.startOrWait(ctx, pl, e, &run, lease)
	h.mu.Lock()
	if pl.state == "waiting" {
		for _, d := range pl.waiting {
			// A reload can remove d during the retirement (h.mu is free then): no record, no text.
			dp := h.plugins[d]
			if dp == nil {
				continue
			}
			if st := dp.reportedState(); st == "refused" || st == "retained" || st == "stopped" {
				pl.refusal = fmt.Sprintf("waiting for %s (%s)", d, st)
				break
			}
		}
	}
	h.mu.Unlock()
	return err
}

// checkRun checks a selected run before an automatic start: its published entry exists and has
// the run's digest (the whole entry), its plugin directory is a directory, and the hub can start
// its sdk. A source run (testSourceRun) has no entry: its dir and its main file must exist. It
// reads no lock.
func (h *Hub) checkRun(run *pluginRun) error {
	gone := fmt.Errorf("plugin %s: run %s is no longer available; start it again", run.Name, runIdentity(run))
	switch {
	case run.Entry == "" && h.testSourceRun(run.Dir):
		if fi, err := os.Stat(run.Path); err != nil || !fi.Mode().IsRegular() {
			return gone
		}
	case run.Entry == "":
		return gone // a run from before the selected run recorded its entry
	default:
		d, err := dirDigest(filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(run.Entry)))
		if err != nil || d != run.Digest {
			return gone
		}
	}
	if fi, err := os.Stat(run.Dir); err != nil || !fi.IsDir() {
		return gone
	}
	if run.SDK != "" && h.cfg.SDK == "" {
		return fmt.Errorf("plugin %s: the manifest declares sdk, and the hub has no SDK directory (-sdk, with boot.lua)", run.Name)
	}
	return nil
}

// runIdentity names a run in a text: its commit, or the digest of a local snapshot.
func runIdentity(run *pluginRun) string {
	if run.Commit != "" {
		return run.Commit
	}
	return run.Digest
}

// selectedCycle gives a cycle among the selected runs through the plugin name, whose run has the
// dependencies deps: the names from name back to name, or nil. Only selected runs give edges;
// no pin is read. h.mu must be held.
func (h *Hub) selectedCycle(name string, deps []string) []string {
	path, seen := []string{name}, map[string]bool{}
	var walk func(deps []string) bool
	walk = func(deps []string) bool {
		for _, d := range deps {
			if d == name {
				path = append(path, d)
				return true
			}
			if seen[d] {
				continue
			}
			seen[d] = true
			if dp := h.plugins[d]; dp != nil && dp.selected != nil {
				path = append(path, d)
				if walk(dp.selected.Run.Depends) {
					return true
				}
				path = path[:len(path)-1]
			}
		}
		return false
	}
	if walk(deps) {
		return path
	}
	return nil
}
