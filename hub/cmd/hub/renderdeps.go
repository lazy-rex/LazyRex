package main

// Declared render dependencies (docs/hub.md, "Render dependencies"). A view.render reply can add
// "deps": {"prefixes": [...], "changes": [...]}. A commit of the plugin then renders the view
// again only when a committed key starts with one of its prefixes, and a changed() renders it only
// for the names in changes. A missing field keeps the broad behavior; an empty list turns that
// trigger off. Other triggers mark one view or every view of the plugin, whatever the declaration.
//
// The state of each view is under h.vs.mu. A render takes the view's causes and marks it in
// flight. While it is in flight, every trigger marks the view, without the filter: the hub does
// not know the declaration that the running render gives. The result installs its declaration
// only if the view's state is the same and the plugin has no newer registration (the epoch).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// renderCause is a set of the reasons for a render. hub.test.renders counts each one.
type renderCause uint16

const (
	causeCommit renderCause = 1 << iota
	causeInvalidate
	causeRegister
	causeAttach
	causeSnapshot
	causeStatus
	causeLayout
	causeChanged
	causeOperation
	causeTest
)

var causeNames = []string{"commit", "invalidate", "register", "attach", "snapshot", "status", "layout", "changed", "operation", "test"}

// changeNames are the names that changed() uses, and that deps.changes can list.
var changeNames = []string{"agents", "plugins", "workers", "profiles"}

const (
	depsMaxPrefixes  = 64
	depsMaxPrefixLen = 512
)

// viewDeps is a declaration. A nil list keeps the broad trigger; an empty list turns it off.
type viewDeps struct {
	prefixes []string
	changes  []string
}

// viewState is the render state of one view.
type viewState struct {
	dirty    renderCause // the causes of the next render
	inFlight bool
	deps     *viewDeps       // nil: no declaration
	depsErr  string          // the last malformed declaration (view_deps_invalid is emitted when it changes)
	tables   map[string]bool // the tables that the last good, tracked render read; nil: every SQL commit marks the view (renderreads.go)
}

// initLocked makes the maps of the render state (a test can build a viewSvc without them).
// h.vs.mu must be held.
func (vs *viewSvc) initLocked() {
	if vs.views == nil {
		vs.views, vs.all, vs.loose, vs.epoch = map[string]map[string]*viewState{}, map[string]renderCause{}, map[string]renderCause{}, map[string]int64{}
	}
	if vs.dirty == nil {
		vs.dirty = map[string]bool{}
	}
}

// stateLocked returns the state of the view, and makes it. h.vs.mu must be held.
func (vs *viewSvc) stateLocked(plugin, id string) *viewState {
	vs.initLocked()
	m := vs.views[plugin]
	if m == nil {
		m = map[string]*viewState{}
		vs.views[plugin] = m
	}
	st := m[id]
	if st == nil {
		st = &viewState{dirty: vs.loose[plugin]}
		m[id] = st
	}
	return st
}

// markView marks one view of the plugin and wakes the worker.
func (h *Hub) markView(plugin, id string, cause renderCause) {
	h.vs.mu.Lock()
	h.vs.stateLocked(plugin, id).dirty |= cause
	h.vs.dirty[plugin] = true
	h.vs.mu.Unlock()
	h.kickViews(plugin, 0)
}

// markCommit marks the views of the plugin that a commit of keys can change: a view in flight, a
// view without prefixes, and a view with a matching prefix. A view that has no state yet gets it
// through loose.
func (h *Hub) markCommit(plugin string, keys []string) {
	h.vs.mu.Lock()
	h.vs.initLocked()
	for _, st := range h.vs.views[plugin] {
		if st.inFlight || st.deps == nil || st.deps.prefixes == nil || matchesPrefix(keys, st.deps.prefixes) {
			st.dirty |= causeCommit
		}
	}
	h.vs.loose[plugin] |= causeCommit
	h.vs.dirty[plugin] = true
	h.vs.mu.Unlock()
	h.kickViews(plugin, 0)
}

// markChanged marks the views of the plugin that changed(what) can change, as markCommit does.
// A name that a declaration cannot list marks every view.
func (h *Hub) markChanged(plugin, what string) {
	if !slices.Contains(changeNames, what) {
		h.kickViews(plugin, causeChanged)
		return
	}
	h.vs.mu.Lock()
	h.vs.initLocked()
	for _, st := range h.vs.views[plugin] {
		if st.inFlight || st.deps == nil || st.deps.changes == nil || slices.Contains(st.deps.changes, what) {
			st.dirty |= causeChanged
		}
	}
	h.vs.loose[plugin] |= causeChanged
	h.vs.dirty[plugin] = true
	h.vs.mu.Unlock()
	h.kickViews(plugin, 0)
}

// clearDeclarations is for a new registration of the plugin: no declaration of an older instance
// stays, and a render result from before it installs nothing.
func (h *Hub) clearDeclarations(plugin string) {
	h.vs.mu.Lock()
	h.vs.initLocked()
	h.vs.epoch[plugin]++
	for _, st := range h.vs.views[plugin] {
		st.deps, st.depsErr, st.tables = nil, "", nil
	}
	h.vs.mu.Unlock()
}

// forgetView deletes the render state of a closed view. A render of it that is in flight then
// installs nothing.
func (h *Hub) forgetView(id string) {
	h.vs.retireNotifiers(id)
	h.vs.mu.Lock()
	for _, m := range h.vs.views {
		delete(m, id)
	}
	h.vs.mu.Unlock()
}

func matchesPrefix(keys, prefixes []string) bool {
	for _, k := range keys {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				return true
			}
		}
	}
	return false
}

// parseDeps reads the deps field of a view.render reply. No field (or null) is no declaration.
func parseDeps(raw json.RawMessage) (*viewDeps, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("deps must be an object")
	}
	d := &viewDeps{}
	for k, v := range m {
		l, err := depsList(v)
		if err != nil {
			return nil, fmt.Errorf("deps.%s: %w", k, err)
		}
		switch k {
		case "prefixes":
			if len(l) > depsMaxPrefixes {
				return nil, fmt.Errorf("deps.prefixes: more than %d prefixes", depsMaxPrefixes)
			}
			for _, p := range l {
				if len(p) > depsMaxPrefixLen {
					return nil, fmt.Errorf("deps.prefixes: a prefix is longer than %d bytes", depsMaxPrefixLen)
				}
			}
			d.prefixes = l
		case "changes":
			for _, c := range l {
				if !slices.Contains(changeNames, c) {
					return nil, fmt.Errorf("deps.changes: unknown name %q (use %s)", c, strings.Join(changeNames, ", "))
				}
			}
			d.changes = l
		default:
			return nil, fmt.Errorf("deps has an unknown field %q", k)
		}
	}
	return d, nil
}

// depsList reads a list of strings. Rex Lua encodes an empty table as {}: that is the empty list.
func depsList(raw json.RawMessage) ([]string, error) {
	if string(raw) == "{}" {
		return []string{}, nil
	}
	var l []string
	if err := json.Unmarshal(raw, &l); err != nil || l == nil {
		return nil, errors.New("must be a list of strings")
	}
	return l, nil
}

// uiInvalidate handles hub.ui.invalidate {routes?}: the plugin's data changed outside the store
// (for example a scan finished). No routes: every open view of the plugin renders again. routes:
// only the open views whose route equals one of the strings.
func (h *Hub) uiInvalidate(ctx context.Context, plugin string, args map[string]any) (any, error) {
	raw, ok := args["routes"]
	if !ok {
		h.kickViews(plugin, causeInvalidate)
		return map[string]any{"status": "accepted"}, nil
	}
	list, _ := raw.([]any)
	if len(list) < 1 || len(list) > depsMaxPrefixes {
		return nil, bad("invalid: routes must be a list of 1 to %d routes", depsMaxPrefixes)
	}
	routes := make([]any, 0, len(list))
	for _, r := range list {
		s, _ := r.(string)
		if s == "" {
			return nil, bad("invalid: each route must be a non-empty string")
		}
		routes = append(routes, s)
	}
	n, err := h.markViews(ctx, plugin, causeInvalidate, `route IN (?`+strings.Repeat(", ?", len(routes)-1)+`)`, routes...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "accepted", "views": n}, nil
}

// markViews marks the open views of the plugin that the SQL condition where selects, with cause.
// It returns the number of views.
func (h *Hub) markViews(ctx context.Context, plugin string, cause renderCause, where string, args ...any) (int, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT view_id FROM views WHERE plugin = ? AND status = 'open' AND `+where, append([]any{plugin}, args...)...)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	h.vs.mu.Lock()
	for _, id := range ids {
		h.vs.stateLocked(plugin, id).dirty |= cause
	}
	h.vs.dirty[plugin] = true
	h.vs.mu.Unlock()
	h.kickViews(plugin, 0)
	return len(ids), nil
}

// renderFinished ends a render of the view that started at epoch, with its state st. A good
// declaration (ok) is installed, else the view goes back to no declaration. A view that was
// closed (its state is gone or new) or a plugin that registered again (a newer epoch) gets
// nothing from this result: its state stays as the newer event left it. Causes that came during
// the render stay in st.dirty, and give one more pass.
func (h *Hub) renderFinished(plugin, id, route string, st *viewState, epoch int64, deps *viewDeps, ok bool, errText string, tables map[string]bool) {
	emit := false
	h.vs.mu.Lock()
	st.inFlight = false
	if h.vs.views[plugin][id] == st && h.vs.epoch[plugin] == epoch {
		if ok {
			st.deps = deps
		} else {
			st.deps = nil
		}
		st.tables = tables
		emit = errText != "" && errText != st.depsErr
		st.depsErr = errText
	}
	again := h.vs.views[plugin][id] == st && st.dirty != 0
	if again {
		h.vs.dirty[plugin] = true
	}
	h.vs.mu.Unlock()
	if emit {
		h.ev.emit("view_deps_invalid", map[string]any{"plugin": plugin, "view_id": id, "route": route, "error": errText})
	}
	if again {
		h.kickViews(plugin, 0)
	}
}

// renderCount is the test-mode count of the renders of one view (hub.test.renders).
type renderCount struct {
	Plugin    string         `json:"-"`
	ViewID    string         `json:"view_id"`
	Route     string         `json:"route"`
	Attempts  int            `json:"attempts"`
	OK        int            `json:"ok"`
	Failed    int            `json:"failed"`
	Transient int            `json:"transient"`
	Causes    map[string]int `json:"causes"`
	// Declared is the declaration that the view has now, {prefixes, changes}; absent for none.
	Declared any `json:"declared,omitempty"`
	// SQLTracked: the view has a table set from a tracked render (renderreads.go). No
	// table names: they are the plugin's storage.
	SQLTracked bool `json:"sql_tracked"`
	// LastCallMS: the time of the last view.render call, with its reply parts (replyparts.go).
	LastCallMS float64 `json:"last_call_ms"`
}

// testRenderCallTime records the time of a view.render call (test mode).
func (h *Hub) testRenderCallTime(id string, d time.Duration) {
	if h.test == nil {
		return
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	if c := h.test.renders[id]; c != nil {
		c.LastCallMS = float64(d.Microseconds()) / 1000
	}
}

// testRenderAttempt counts a view.render call and its causes.
func (h *Hub) testRenderAttempt(plugin, id, route string, causes renderCause) {
	if h.test == nil {
		return
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	c := h.test.renders[id]
	if c == nil {
		c = &renderCount{Plugin: plugin, ViewID: id, Causes: map[string]int{}}
		for _, n := range causeNames {
			c.Causes[n] = 0
		}
		h.test.renders[id] = c
	}
	c.Route = route
	c.Attempts++
	for i, n := range causeNames {
		if causes&(1<<i) != 0 {
			c.Causes[n]++
		}
	}
}

// testRenderOutcome counts how a render ended: ok, failed or transient.
func (h *Hub) testRenderOutcome(id, outcome string) {
	if h.test == nil {
		return
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	if c := h.test.renders[id]; c != nil {
		switch outcome {
		case "ok":
			c.OK++
		case "failed":
			c.Failed++
		default:
			c.Transient++
		}
	}
}

// testRenders handles hub.test.renders {plugin}: the counts of each view of the plugin, in
// view_id order.
func (h *Hub) testRenders(plugin string) any {
	declared, tracked := map[string]any{}, map[string]bool{}
	h.vs.mu.Lock()
	for id, st := range h.vs.views[plugin] {
		tracked[id] = st.tables != nil
		if st.deps != nil {
			declared[id] = map[string]any{"prefixes": st.deps.prefixes, "changes": st.deps.changes}
		}
	}
	h.vs.mu.Unlock()
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	out := []*renderCount{}
	for _, c := range h.test.renders {
		if c.Plugin == plugin {
			cc := *c
			cc.Causes = maps.Clone(c.Causes)
			cc.Declared = declared[c.ViewID]
			cc.SQLTracked = tracked[c.ViewID]
			out = append(out, &cc)
		}
	}
	slices.SortFunc(out, func(a, b *renderCount) int { return strings.Compare(a.ViewID, b.ViewID) })
	return map[string]any{"views": out}
}

// testRenderHold holds a render result of the plugin's route at an armed view-render fault: after
// the plugin answered, before the worker uses the result. It waits until hub.test.fault_release
// (at most 30 s, or hold_ms ms). It runs in the plugin's view worker, not on the request queue.
func (h *Hub) testRenderHold(plugin, route string) {
	if h.test == nil {
		return
	}
	f := h.takeFault(func(f *testFault) bool {
		return f.Point == "view-render" && f.Mode == "hold" && f.Plugin == plugin && (f.Key == "" || f.Key == route)
	})
	if f == nil {
		return
	}
	h.waitRelease(f)
}
