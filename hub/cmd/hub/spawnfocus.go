package main

import (
	"context"
	"log"
)

// spawnFocus records the original form's tab. Only an immediate user spawn may use it.
// Store it in the admitted request, but never apply it on a replay or late adoption.
type spawnFocus struct {
	SourceWindowID string `json:"source_window_id"`
}

func parseSpawnFocus(raw any) (*spawnFocus, error) {
	m, ok := raw.(map[string]any)
	if !ok || len(m) != 1 {
		return nil, bad("focus must be an object with source_window_id")
	}
	id, ok := m["source_window_id"].(string)
	if !ok || id == "" {
		return nil, bad("focus.source_window_id must be nonempty text")
	}
	return &spawnFocus{SourceWindowID: id}, nil
}

// focusSpawn is best-effort presentation after a successful open. A failure must not
// turn a successful spawn into a failed operation. Call only on the first success path.
// Rex has no atomic conditional focus; a focus change can race the check and the call.
func (h *Hub) focusSpawn(ctx context.Context, session, block string, focus *spawnFocus) {
	if focus == nil {
		return
	}
	var view struct {
		ActiveWindowID string `json:"active_window_id"`
	}
	if err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": session}, &view); err != nil {
		log.Printf("spawn focus: cannot read session %s: %v", session, err)
		return
	}
	if view.ActiveWindowID != focus.SourceWindowID {
		return
	}
	if err := h.rex.Call(ctx, "session.focus_block", map[string]any{"session_id": session, "block_id": block}, nil); err != nil {
		log.Printf("spawn focus: cannot focus block %s: %v", block, err)
	}
}

func spawnFocusSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false,
		"description": "User spawns only. Focus on the first successful open, only while this source tab is still active in the target session. Never focus after an unknown outcome or on retry.",
		"properties":  map[string]any{"source_window_id": map[string]any{"type": "string", "minLength": 1}},
		"required":    []string{"source_window_id"}}
}
