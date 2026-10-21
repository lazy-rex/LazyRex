package main

// hub.plugins.reload (docs/plugin-sources.md, section 13): the hub reads the managed file again
// while it runs. Config entries stay restart-only. A removed entry's record becomes a kept record
// (2d part 1): its instance runs on with its registration and exports until a stop, gets no
// notices and never respawns. Lock order: reloadMu, then a lifecycle lock, then entriesMu, then mu.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sort"
)

// reloadAction checks the caller of hub.plugins.reload. An operator (a process outside agent and
// plugin panes) or the registered current instance of an entry-backed plugin may reload. The
// reload checks a plugin caller again after it takes reloadMu: a queued caller can become kept.
func (h *Hub) reloadAction(ctx context.Context, p perform) (any, error) {
	caller := h.pluginByClient(p.Requester.ClientID)
	if caller == nil {
		in, err := h.callerInManagedPane(p.Requester.Principal.PID)
		if err != nil {
			return nil, unverifiedCaller(err)
		}
		if in {
			return nil, bad("hub.plugins.reload is refused from agent panes and from plugin processes that are not the registered instance")
		}
	}
	return h.reload(ctx, caller, p.Requester.ClientID)
}

// unverifiedCaller is the refusal of an admin guard whose ancestry walk is incomplete: the
// caller can be in an agent or plugin pane, so the guard refuses.
func unverifiedCaller(err error) error {
	return bad("cannot check the caller's process ancestry: %v; admin actions need a verified caller", err)
}

// sameManaged reports whether a managed entry is unchanged: the same source, args and watch roots.
func sameManaged(a, b *configEntry) bool {
	return a.Source == b.Source && maps.Equal(a.Args, b.Args) && slices.Equal(a.WatchRoots, b.WatchRoots)
}

// updateManaged gives the entry o of a changed managed id every parsed field of m, and keeps its
// bound name. The resolution fields (cand, err, seen) are m's zero values: resolveLocked sets them
// again under the same entriesMu section. A copy of the whole entry keeps a later parsed field
// (for example a GitHub source's owner, repo, subdir and ref) from going stale.
func updateManaged(o, m *configEntry) {
	name := o.name
	*o = *m
	o.name = name
}

// setKeptLocked gives a record whose entry was removed the kept state: retained while its child
// lives, else refused. h.mu must be held.
func (h *Hub) setKeptLocked(pl *Plugin) {
	pl.state, pl.refusal, pl.waiting = "refused", keptError(pl), nil
	if pl.BlockID != "" && pl.ChildPID > 0 && alive(pl.ChildPID, pl.ChildStart) {
		pl.state = "retained"
	}
}

// reload reads the managed file and applies it. A malformed file changes nothing but the reported
// error. A valid file: the managed part of h.entries is swapped under entriesMu and mu (an
// unchanged id keeps its entry; a changed id is updated in place, so its bound name stays; a new id
// is added; a missing id is removed). Then, under each record's lifecycle lock: a removed record
// takes the kept path, a re-added kept record takes the re-add start, and an added or changed entry
// starts unless it runs (the hub.admin.start rule).
func (h *Hub) reload(ctx context.Context, caller *Plugin, clientID string) (any, error) {
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	if caller != nil {
		h.mu.Lock()
		ok := clientID != "" && caller.ClientID == clientID && caller.entry != nil
		h.mu.Unlock()
		if !ok {
			return nil, refuse("retained", "plugin %s may not reload: its instance is not the registered instance of an entry", caller.Name)
		}
	}
	managed, err := parseManaged(h.cfg.managedPath)
	if err != nil {
		err = fmt.Errorf("managed plugin file %s: %w", h.cfg.managedPath, err)
		h.entriesMu.Lock()
		h.managedErr = err
		h.entriesMu.Unlock()
		h.ev.emit("plugins_reload_refused", map[string]any{"error": err.Error()})
		go h.changed("plugins")
		return nil, refuse("invalid", "%v; the managed entries are unchanged", err)
	}

	h.entriesMu.Lock()
	var config, old []*configEntry
	for _, e := range h.entries {
		if e.managed == "" {
			config = append(config, e)
		} else {
			old = append(old, e)
		}
	}
	oldByID := map[string]*configEntry{}
	for _, e := range old {
		oldByID[e.managed] = e
	}
	var next, starts, gone []*configEntry
	added, changed, removed := []string{}, []string{}, []string{}
	h.mu.Lock() // a changed entry is updated in place, and watchRoots reads entries under mu
	for _, m := range managed {
		o := oldByID[m.managed]
		delete(oldByID, m.managed)
		switch {
		case o == nil:
			next, starts, added = append(next, m), append(starts, m), append(added, m.managed)
		case sameManaged(o, m):
			next = append(next, o)
		default:
			updateManaged(o, m)
			next, starts, changed = append(next, o), append(starts, o), append(changed, o.managed)
		}
	}
	for _, e := range old {
		if oldByID[e.managed] != nil {
			gone, removed = append(gone, e), append(removed, e.managed)
		}
	}
	for _, m := range next {
		m.static = nil // addManaged sets it again
	}
	h.entries, h.managedErr = addManaged(config, next), nil
	var removedRecs []*Plugin
	keptBefore := map[*Plugin]bool{}
	for _, pl := range h.plugins {
		if pl.entry != nil && slices.Contains(gone, pl.entry) {
			pl.entry = nil
			h.setKeptLocked(pl)
			removedRecs = append(removedRecs, pl)
		}
		if pl.entry == nil {
			keptBefore[pl] = true
		}
	}
	h.mu.Unlock()
	h.resolveLocked() // binds the new names; a kept record of the same name and source is adopted
	var readded, startRecs []*Plugin
	h.mu.Lock()
	for pl := range keptBefore {
		if pl.entry != nil {
			readded = append(readded, pl)
		}
	}
	for _, e := range starts {
		if pl := h.plugins[e.name]; e.name != "" && pl != nil && pl.entry == e && !keptBefore[pl] {
			startRecs = append(startRecs, pl)
		}
	}
	h.mu.Unlock()
	h.entriesMu.Unlock()
	h.ev.emit("plugins_reloaded", map[string]any{"added": added, "changed": changed, "removed": removed})
	h.kickWaiting() // a removal can refuse a waiting dependent
	go h.changed("plugins")

	errs := map[string]string{}
	byName := func(l []*Plugin) {
		sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	}
	byName(removedRecs)
	byName(readded)
	for _, pl := range removedRecs {
		pl.lifecycle.Lock()
		h.mu.Lock()
		err := h.loadPluginRow(ctx, pl)
		kept := pl.entry == nil
		h.mu.Unlock()
		if err == nil && kept {
			if err = h.recoverKept(ctx, pl); isRefusal(err, "refused") {
				err = nil // the kept state is the expected result of a removal
			}
		}
		pl.lifecycle.Unlock()
		if err != nil {
			errs[pl.Name] = err.Error()
		}
	}
	for _, pl := range readded {
		pl.lifecycle.Lock()
		if err := h.startPluginWith(ctx, pl, true); err != nil {
			errs[pl.Name] = err.Error()
		}
		pl.lifecycle.Unlock()
	}
	for _, pl := range startRecs {
		pl.lifecycle.Lock()
		h.mu.Lock()
		running := pl.ClientID != "" && alive(pl.ChildPID, pl.ChildStart)
		h.mu.Unlock()
		if !running {
			if err := h.startPlugin(ctx, pl); err != nil {
				errs[pl.Name] = err.Error()
			}
		}
		pl.lifecycle.Unlock()
	}
	for name, e := range errs {
		log.Printf("plugin %s: reload: %s", name, e)
	}
	names := func(l []*Plugin) []string {
		out := []string{}
		for _, pl := range l {
			out = append(out, pl.Name)
		}
		return out
	}
	return map[string]any{"added": added, "changed": changed, "removed": removed, "readded": names(readded), "errors": errs}, nil
}

// isRefusal reports whether err is a hub refusal with this code (refuse).
func isRefusal(err error, code string) bool {
	var e *storeError
	return errors.As(err, &e) && e.code == code
}
