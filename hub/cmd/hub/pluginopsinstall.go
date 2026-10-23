package main

// The install op (C2; the hub lead's lead-c2inst and lead-c2i2 to lead-c2i5): the hub puts the
// code of its managed GitHub entries into the lock and the cache, so a new hub, a new machine or a
// cleared cache needs no plugin command. At the end of the start recovery and of each reload, the
// scan admits one install op (origin {kind: "hub"}) for each managed GitHub entry whose code is
// not in place:
//   - no lock entry: the op resolves the entry's ref, as an update to "@" does;
//   - a lock entry whose cache entry is missing or changed: the op's target is the lock entry, so
//     it fetches that commit by its ID and must get its digest. A cleared cache never upgrades a
//     plugin.
//
// Any other lock error (the lock entry does not agree with the source) gets no op: the start
// error says what to do. An entry with an open op gets no second one. An install never starts
// and never changes a desired state: when the installs of the scans have all ended, the hub runs
// its normal start of the entries that do not run (installStart), once. A failed install ends
// failed; the next start or reload tries again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
)

// pluginInstalls are the install ops that the scans admitted and that have not all ended.
type pluginInstalls struct {
	mu      sync.Mutex
	pending map[string]string // op ID -> entry key
}

// installScan admits the install ops of the managed GitHub entries whose code is not in place.
// why is "start" or "reload" (the origin ID).
func (h *Hub) installScan(ctx context.Context, why string) {
	for _, r := range h.installRequests(ctx) {
		v, err := h.admitInstall(ctx, why, r)
		if err != nil {
			log.Printf("plugin install %s: %v", r.Entry, err)
			continue
		}
		id, _ := v["op_id"].(string)
		h.pops.installs.mu.Lock()
		if h.pops.installs.pending == nil {
			h.pops.installs.pending = map[string]string{}
		}
		h.pops.installs.pending[id] = r.Entry
		h.pops.installs.mu.Unlock()
	}
}

// installRequests gives the install request of each managed GitHub entry whose code is not in
// place and that has no open op.
func (h *Hub) installRequests(ctx context.Context) []opRequest {
	type ent struct{ key, source string }
	var list []ent
	h.entriesMu.Lock()
	for _, e := range h.entries {
		if e.managed != "" && e.github && e.static == nil {
			list = append(list, ent{opEntryKey(e), e.Source})
		}
	}
	h.entriesMu.Unlock()
	if len(list) == 0 {
		return nil
	}
	lock, err := readLock(lockPath(h.cfg.path, h.cfg.Lock))
	if err != nil {
		return nil // the start error names the lock
	}
	var out []opRequest
	for _, x := range list {
		req := opRequest{Op: "install", Entry: x.key}
		if le := lock.Plugins[x.source]; le != nil {
			e := &configEntry{Source: x.source}
			if e.checkSource("") != nil {
				continue
			}
			_, _, err := lockedDir(h.cfg.path, h.cfg.State, lock, e)
			var cs *cacheStateError
			if !errors.As(err, &cs) {
				continue // in place, or a lock error that an install does not repair
			}
			b, _ := json.Marshal(pinEntry{Source: x.source, lockPin: le.lockPin})
			req.Target = b
		}
		var open int
		if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM plugin_ops WHERE entry = ? AND phase NOT IN ('done', 'failed', 'conflict', 'unknown')`, x.key).Scan(&open); err != nil || open > 0 {
			continue
		}
		out = append(out, req)
	}
	return out
}

// admitInstall admits the install r. The key is the hash of the entry and the target with a
// count, so a try after an ended install is a new op.
func (h *Hub) admitInstall(ctx context.Context, why string, r opRequest) (map[string]any, error) {
	base := hashBytes(append([]byte(r.Entry+"\x00"), r.Target...))
	var n int
	if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM plugin_ops WHERE scope = 'hub' AND key LIKE ?`, "install:"+base+"#%").Scan(&n); err != nil {
		return nil, err
	}
	v, err := h.admitOp(ctx, opOrigin{"hub", why}, "hub", fmt.Sprintf("install:%s#%d", base, n+1), r)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return m, nil
}

// installsEnded runs on the manager goroutine after each pass. When every pending install has
// ended, it records each entry's install error and runs the normal start once.
func (h *Hub) installsEnded(ctx context.Context) {
	h.pops.installs.mu.Lock()
	pending := h.pops.installs.pending
	if len(pending) == 0 {
		h.pops.installs.mu.Unlock()
		return
	}
	ended := map[string]*pluginOp{}
	for id := range pending {
		op, err := h.loadOp(ctx, id)
		if err != nil || !opFinal[op.Phase] {
			h.pops.installs.mu.Unlock()
			return
		}
		ended[id] = op
	}
	h.pops.installs.pending = nil
	h.pops.installs.mu.Unlock()
	h.entriesMu.Lock()
	for _, op := range ended {
		for _, e := range h.entries {
			if opEntryKey(e) != op.Entry {
				continue
			}
			e.installErr = ""
			if op.Phase != "done" {
				e.installErr = op.Error
				if e.installErr == "" {
					e.installErr = op.Phase
				}
			}
		}
	}
	h.entriesMu.Unlock()
	go afterInstalls(h, ctx)
}

// afterInstalls runs when the installs of the scans have ended; a test replaces it.
var afterInstalls = (*Hub).installStart

// installStart is the hub's normal start (startPlugin) of each entry that the graph now names and
// that does not run, in start order, as at hub start: a desired stopped entry stays stopped, and
// a dependency that is not registered makes the entry wait. A waiting or crashing entry keeps its
// own path.
func (h *Hub) installStart(ctx context.Context) {
	h.reloadMu.Lock() // as recoverPlugins: a reload waits
	defer h.reloadMu.Unlock()
	h.entriesMu.Lock()
	order := h.resolveLocked()
	h.entriesMu.Unlock()
	go h.changed("plugins")
	for _, e := range order {
		h.mu.Lock()
		pl := h.plugins[e.name]
		h.mu.Unlock()
		if pl == nil {
			continue
		}
		pl.lifecycle.Lock()
		h.mu.Lock()
		skip := pl.ClientID != "" && alive(pl.ChildPID, pl.ChildStart) || pl.state == "waiting" || pl.crashing()
		h.mu.Unlock()
		if !skip {
			if err := h.startPlugin(ctx, pl); err != nil {
				h.recordLogf(pl, "after install: %v", err)
			}
		}
		pl.lifecycle.Unlock()
	}
}
