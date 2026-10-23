package main

// Render IDs and SQL read tracking (docs/sql.md section 9, docs/hub.md "Render IDs"). The hub gives
// each view.render call a new render ID in args.render. The plugin's SDK sends that ID with each
// hub.sql.query of the render, and the hub records the tables that the query read. The ID is open
// from just before the call until the call returns, for any outcome; then it is deleted and never
// used again.
//
// A render ID is a read-only token: every hub.sql action except hub.sql.query refuses it, and so
// does a query in a transaction (renderArg).
//
// A render is untracked when the hub cannot know all that it read: while it is open, a hub call
// of the plugin that is not a hub.sql.query with an open render ID of that plugin instance (an
// untokened query, a closed or unknown ID, hub.store.get, any other hub action), a query that
// failed with an error other than conflict, or a read of a sqlite_ table other than the schema.
// A hub.reply.part of the instance does not untrack its renders: the parts of a reply come after
// the handler returned and read nothing (replyparts.go).
//
// A covered hub read (coveredReads) with an open render ID of the same instance does not untrack
// the render either: it adds its change names to the render's read set (renderHubRead).
//
// Selective refresh is the default. A render is tracked unless its
// view.render reply has a refresh field: "all" is the opt-out (also the SDK's answer for a render
// with a failed hub call), and any other value is a declaration error. The view gets the render's tables
// only when the render succeeded, stayed tracked and read at least one table. In every other case
// the view has no table set, and every SQL commit of the plugin marks it.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// openRender is one view.render call that has not returned.
type openRender struct {
	plugin    string
	client    string // the plugin instance that the hub called
	viewID    string
	epoch     int64           // the plugin's registration epoch at the call
	tables    map[string]bool // the tables that its queries read (lower case)
	changes   map[string]bool // the change names of its covered hub reads (coveredReads)
	untracked bool
}

// coveredReads are the hub reads that a render can track (docs/hub.md, "Hub reads in a render"):
// each action and the change names whose changed() can change its result. Keep the action names
// equal to COVERED_READS in sdk/lua/lazyrex.lua (TestCoveredReadsMatchSDK).
var coveredReads = map[string][]string{
	"hub.agents":         {"agents"},
	"hub.agents.resolve": {"agents"},
	"hub.workers":        {"workers"},
	"hub.profiles":       {"profiles", "workers"}, // its running counts come from the spawns
	// The plugin manager reads (C8): every entry, op phase and load change sends "plugins".
	"hub.plugins":      {"plugins"},
	"hub.plugins.ops":  {"plugins"},
	"hub.plugins.tags": {"plugins"},
	"hub.plugins.log":  {"plugins"},
	// A pure read: it adds no change name. The slice is empty, not nil: a nil entry is not covered.
	"hub.digest": {},
}

// withRenderArg returns a copy of a declared action whose args schema also has the property
// "render" (a string): the SDK sends the render ID with a covered read, and Rex refuses an
// argument that a strict schema does not declare. The shared declaration does not change.
func withRenderArg(a map[string]any) map[string]any {
	args, ok := a["args"].(map[string]any)
	if !ok {
		return a
	}
	props, _ := args["properties"].(map[string]any)
	out, outArgs := maps.Clone(a), maps.Clone(args)
	outProps := maps.Clone(props)
	if outProps == nil {
		outProps = map[string]any{}
	}
	outProps["render"] = map[string]any{"type": "string", "description": "The render ID of view.render. The SDK sends it in a render."}
	outArgs["properties"] = outProps
	out["args"] = outArgs
	return out
}

// newRenderID returns a render ID: 128 random bits, so a closed ID is never made again.
func newRenderID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "r-" + hex.EncodeToString(b)
}

// openRenderID records a render of the view and returns its ID.
func (h *Hub) openRenderID(plugin, client, viewID string, epoch int64) string {
	id := newRenderID()
	h.vs.mu.Lock()
	h.vs.renders[id] = &openRender{plugin: plugin, client: client, viewID: viewID, epoch: epoch, tables: map[string]bool{}, changes: map[string]bool{}}
	h.vs.mu.Unlock()
	return id
}

// closeRenderID ends a render and returns its record: a later call with its ID finds no open
// render.
func (h *Hub) closeRenderID(id string) *openRender {
	h.vs.mu.Lock()
	defer h.vs.mu.Unlock()
	r := h.vs.renders[id]
	delete(h.vs.renders, id)
	return r
}

// untrackLocked marks every open render of the plugin untracked. h.vs.mu must be held.
func (vs *viewSvc) untrackLocked(plugin string) {
	for _, r := range vs.renders {
		if r.plugin == plugin {
			r.untracked = true
		}
	}
}

// renderCall applies the untracked rule to one hub call of a plugin instance, before the call
// runs. A hub.sql.query or a covered read with an open render ID of the same instance records its
// reads later (renderRead, renderHubRead); every other call makes the plugin's open renders
// untracked.
func (h *Hub) renderCall(plugin, client, action string, args map[string]any) {
	h.vs.mu.Lock()
	defer h.vs.mu.Unlock()
	if action == "hub.sql.query" || coveredReads[action] != nil {
		id, _ := args["render"].(string)
		if r := h.vs.renders[id]; r != nil && r.plugin == plugin && r.client == client {
			return
		}
	}
	if action == "hub.reply.part" {
		// The parts of a reply come after the render's handler returned: they read nothing
		// (replyparts.go). They keep the renders of the instance that sends them tracked.
		for _, r := range h.vs.renders {
			if r.plugin == plugin && r.client == client {
				return
			}
		}
	}
	h.vs.untrackLocked(plugin)
}

// renderRead records the reads of a hub.sql.query with a render ID. ok false: the query failed,
// and the hub cannot know its reads. An ID that is not open for this plugin instance (it closed
// after renderCall) makes the plugin's open renders untracked.
func (h *Hub) renderRead(plugin, client, id string, reads map[string]bool, ok bool) {
	h.vs.mu.Lock()
	defer h.vs.mu.Unlock()
	r := h.vs.renders[id]
	if r == nil || r.plugin != plugin || r.client != client {
		h.vs.untrackLocked(plugin)
		return
	}
	if !ok {
		r.untracked = true
		return
	}
	for t := range reads {
		// The schema changes only by DDL, and DDL marks every view ("*"). Other sqlite_ tables
		// (sqlite_sequence, sqlite_stat1) change without a mark.
		if strings.HasPrefix(t, "sqlite_") && t != "sqlite_master" && t != "sqlite_schema" {
			r.untracked = true
			return
		}
		r.tables[t] = true
	}
}

// renderHubRead records the change names of a covered hub read with a render ID, after the call.
// ok false: the call failed, or its result may be incomplete (workersRead), and the hub cannot
// know its reads. An ID that is not open for this plugin instance makes the plugin's open renders
// untracked.
func (h *Hub) renderHubRead(plugin, client, id string, names []string, ok bool) {
	h.vs.mu.Lock()
	defer h.vs.mu.Unlock()
	r := h.vs.renders[id]
	if r == nil || r.plugin != plugin || r.client != client {
		h.vs.untrackLocked(plugin)
		return
	}
	if !ok {
		r.untracked = true
		return
	}
	for _, n := range names {
		r.changes[n] = true
	}
}

// renderTables returns the table set and the change names that a good render installs into its
// view: the render's sets when its reply is tracked (parseRefresh), the render stayed tracked, and
// it read a table or a change name. A render that read only change names gets an empty table set.
// Else nil, nil: every SQL commit of the plugin marks the view.
func renderTables(r *openRender, tracked bool) (tables, changes map[string]bool) {
	if r == nil || !tracked || r.untracked || len(r.tables)+len(r.changes) == 0 {
		return nil, nil
	}
	return r.tables, r.changes
}

// parseRefresh reads the refresh field of a view.render reply. No field (or null): tracked. "all":
// the opt-out. Any other value is an error of the render's declaration, like malformed deps: the
// view is not tracked, and the error goes to view_deps_invalid.
func parseRefresh(raw json.RawMessage) (tracked bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return true, nil
	}
	if s := ""; json.Unmarshal(raw, &s) == nil && s == "all" {
		return false, nil
	}
	v := string(raw)
	if len(v) > 64 {
		v = v[:64] + "..."
	}
	return false, fmt.Errorf(`refresh must be absent or "all"; got %s`, v)
}

// markSQL marks the views of the plugin that a SQL commit of marks can change: a view in flight,
// a view without a table set, a view that read a marked table, and every view for a schema
// change ("*"). A view that has no state yet gets the cause through loose.
func (h *Hub) markSQL(plugin string, marks []string) {
	all := false
	for _, m := range marks {
		all = all || m == "*"
	}
	h.vs.mu.Lock()
	for _, st := range h.vs.views[plugin] {
		if all || st.inFlight || st.tables == nil || readsAny(st.tables, marks) {
			st.dirty |= causeCommit
		}
	}
	h.vs.loose[plugin] |= causeCommit
	h.vs.dirty[plugin] = true
	h.vs.mu.Unlock()
	h.kickViews(plugin, 0)
}

func readsAny(tables map[string]bool, marks []string) bool {
	for _, m := range marks {
		if tables[m] {
			return true
		}
	}
	return false
}

// renderArg reads the render argument of a hub.sql call. A missing or null render is no render.
// The refusals come before the database is opened, so a refused call changes nothing.
func renderArg(action string, args map[string]any) (string, error) {
	v, ok := args["render"]
	if !ok || v == nil {
		return "", nil
	}
	if action != "hub.sql.query" {
		return "", refuse("refused", "a render cannot write")
	}
	if tx, ok := args["tx"]; ok && tx != nil {
		return "", refuse("refused", "a render cannot write")
	}
	id, ok := v.(string)
	if !ok || id == "" {
		return "", refuse("invalid_input", "render is the render ID of view.render")
	}
	return id, nil
}
