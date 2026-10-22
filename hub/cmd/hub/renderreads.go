package main

// Render IDs (docs/sql.md section 9). The hub gives each view.render call a new render ID in
// args.render. The plugin's SDK sends that ID with each hub.sql.query of the render, so the hub
// can know the tables that the render read. The ID is open from just before the call until the
// call returns, for any outcome; then it is deleted and never used again.
//
// A render ID is a read-only token: every hub.sql action except hub.sql.query refuses it, and so
// does a query in a transaction (renderArg).

import (
	"crypto/rand"
	"encoding/hex"
)

// openRender is one view.render call that has not returned.
type openRender struct {
	plugin string
	client string // the plugin instance that the hub called
	viewID string
	epoch  int64 // the plugin's registration epoch at the call
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
	if h.vs.renders == nil {
		h.vs.renders = map[string]*openRender{}
	}
	h.vs.renders[id] = &openRender{plugin: plugin, client: client, viewID: viewID, epoch: epoch}
	h.vs.mu.Unlock()
	return id
}

// closeRenderID ends a render: a later call with its ID finds no open render.
func (h *Hub) closeRenderID(id string) {
	h.vs.mu.Lock()
	delete(h.vs.renders, id)
	h.vs.mu.Unlock()
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
