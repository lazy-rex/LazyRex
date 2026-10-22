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
//
// Selective refresh is opt-in for each render: the view.render reply has refresh = "reads". The
// view then gets the render's tables only when the render succeeded, stayed tracked and read at
// least one table. In every other case the view has no table set, and every SQL commit of the
// plugin marks it (the behavior before read tracking).

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
)

// openRender is one view.render call that has not returned.
type openRender struct {
	plugin    string
	client    string // the plugin instance that the hub called
	viewID    string
	epoch     int64           // the plugin's registration epoch at the call
	tables    map[string]bool // the tables that its queries read (lower case)
	untracked bool
}

// refreshLogMax bounds the unknown refresh values that the hub remembers for its log.
const refreshLogMax = 64

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
	if h.vs.renders == nil {
		h.vs.renders = map[string]*openRender{}
	}
	h.vs.renders[id] = &openRender{plugin: plugin, client: client, viewID: viewID, epoch: epoch, tables: map[string]bool{}}
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
// runs. A hub.sql.query with an open render ID of the same instance records its reads later
// (renderRead); every other call makes the plugin's open renders untracked.
func (h *Hub) renderCall(plugin, client, action string, args map[string]any) {
	if h.vs == nil {
		return
	}
	h.vs.mu.Lock()
	defer h.vs.mu.Unlock()
	if action == "hub.sql.query" {
		id, _ := args["render"].(string)
		if r := h.vs.renders[id]; r != nil && r.plugin == plugin && r.client == client {
			return
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

// renderTables returns the table set that a good render installs into its view: the render's
// tables when the reply opted in (refresh = "reads"), the render stayed tracked, and it read a
// table. Else nil: every SQL commit of the plugin marks the view.
func (h *Hub) renderTables(plugin string, r *openRender, refresh json.RawMessage) map[string]bool {
	if !h.refreshReads(plugin, refresh) || r == nil || r.untracked || len(r.tables) == 0 {
		return nil
	}
	return r.tables
}

// refreshReads reads the refresh field of a view.render reply: true for "reads". No field, null
// and "writes" are false. Any other value is false too, and the hub logs it once for each plugin
// and value: a newer SDK may send a value that this hub does not know.
func (h *Hub) refreshReads(plugin string, raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && (s == "reads" || s == "writes") {
		return s == "reads"
	}
	v := string(raw)
	if len(v) > 64 {
		v = v[:64] + "..."
	}
	key := plugin + "\x00" + v
	h.vs.mu.Lock()
	if h.vs.logged == nil {
		h.vs.logged = map[string]bool{}
	}
	logIt := !h.vs.logged[key] && len(h.vs.logged) < refreshLogMax
	if logIt {
		h.vs.logged[key] = true
	}
	h.vs.mu.Unlock()
	if logIt {
		log.Printf("views %s: unknown refresh value %s in a view.render reply: every SQL commit renders the view", plugin, v)
	}
	return false
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
	h.vs.initLocked()
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
