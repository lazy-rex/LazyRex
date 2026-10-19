package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Session choice for plugins (the maintainer, 2026-10-17: a new thread asks for its session). The plugin
// keeps exact Rex session IDs. hub.session.list gives the open sessions; hub.session.ensure creates
// a named session once per key; hub.identify gives the caller's own session (agent defaults).

// ensuredSchema: one row per plugin and ensure key. state: prepared (no create sent), sending
// (a create was sent; its outcome is unknown until the answer), done (session_id is the session).
const ensuredSchema = `
CREATE TABLE IF NOT EXISTS ui_ensured (plugin TEXT NOT NULL, key TEXT NOT NULL, name TEXT NOT NULL,
  state TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '', PRIMARY KEY (plugin, key));
`

type rexSession struct {
	ID    string `json:"session_id"`
	Label string `json:"label"`
}

func (h *Hub) rexSessions(ctx context.Context) ([]rexSession, error) {
	var out struct {
		Sessions []rexSession `json:"sessions"`
	}
	if err := h.rex.Call(ctx, "session.list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// sessionList answers hub.session.list: every open session with its exact ID and label.
func (h *Hub) sessionList(ctx context.Context) (any, error) {
	ss, err := h.rexSessions(ctx)
	if err != nil {
		return nil, err
	}
	list := make([]map[string]any, 0, len(ss))
	for _, s := range ss {
		list = append(list, map[string]any{"session_id": s.ID, "label": s.Label})
	}
	return map[string]any{"sessions": list}, nil
}

// rexCall is the shape of rex.Rex.Call; tests replace it.
type rexCall func(ctx context.Context, method string, payload any, out any) error

// sessionEnsure answers hub.session.ensure {key, name}: the session created for this key. The
// first call creates a session with the label name. A repeat answers the same session; another
// name is refused (conflict). Errors with a code (Codex's review):
//   - session_gone: the session made for this key is closed. The hub never creates a second one.
//   - outcome_unknown: a create was sent and no success answer came (an error, a timeout, a
//     dropped connection or a hub stop). The
//     session may exist, but only a label would link it, and a label is not an identity. The user
//     chooses an existing session instead. Only a create that was never sent is retried.
func (h *Hub) sessionEnsure(ctx context.Context, plugin string, args map[string]any) (any, error) {
	return h.ensureSession(ctx, h.rex.Call, plugin, args)
}

func (h *Hub) ensureSession(ctx context.Context, call rexCall, plugin string, args map[string]any) (any, error) {
	key, _ := args["key"].(string)
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if key == "" || len(key) > 1024 {
		return nil, bad("key must be text of 1 to 1024 bytes")
	}
	if name == "" || utf8.RuneCountInString(name) > 200 || strings.ContainsAny(name, "\n\r\t") {
		return nil, bad("name must be a session name of 1 to 200 characters on one line")
	}
	h.ensureMu.Lock() // one create at a time: a concurrent repeat waits for the first
	defer h.ensureMu.Unlock()
	var saved, state, sess string
	err := h.db.QueryRowContext(ctx, `SELECT name, state, session_id FROM ui_ensured WHERE plugin = ? AND key = ?`, plugin, key).Scan(&saved, &state, &sess)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := h.db.ExecContext(ctx, `INSERT INTO ui_ensured (plugin, key, name, state) VALUES (?, ?, ?, 'prepared')`, plugin, key, name); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case saved != name:
		return nil, refuse("conflict", "key %q was used with another name", key)
	case state == "sending":
		return nil, refuse("outcome_unknown", "an earlier create for this key has an unknown outcome; choose an existing session")
	case state == "done":
		err := call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil)
		if sessionGone(err) {
			return nil, refuse("session_gone", "the session made for this key was closed; choose a session again")
		} else if err != nil {
			return nil, err
		}
		return map[string]any{"session_id": sess, "created": false}, nil
	case state != "prepared":
		return nil, fmt.Errorf("ensured session %s/%s: unknown state %q", plugin, key, state)
	}
	// prepared: no create was sent for this key, so one may be sent now.
	if _, err := h.db.ExecContext(ctx, `UPDATE ui_ensured SET state = 'sending' WHERE plugin = ? AND key = ?`, plugin, key); err != nil {
		return nil, err
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	// Every error after the send is uncertain in this phase: the client also reports a dropped
	// connection as a RexError (code disconnected), so an error does not prove that Rex created
	// nothing (Codex's review).
	if err := call(ctx, "session.create", map[string]any{"label": name}, &out); err != nil || !strings.HasPrefix(out.SessionID, "session:") {
		h.ev.emit("session_ensure_unknown", map[string]any{"plugin": plugin, "key": key, "error": errString(err)})
		return nil, refuse("outcome_unknown", "the session create has an unknown outcome; choose an existing session")
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE ui_ensured SET state = 'done', session_id = ? WHERE plugin = ? AND key = ?`, out.SessionID, plugin, key); err != nil {
		return nil, err // the row stays sending: a repeat answers outcome_unknown
	}
	if err := call(ctx, "session.attach", map[string]any{"session_id": out.SessionID, "event_scope": "session"}, nil); sessionGone(err) {
		return nil, refuse("session_gone", "the session made for this key was closed; choose a session again")
	} else if err != nil {
		return nil, err // the row is done: a repeat attaches again
	}
	h.ev.emit("session_ensured", map[string]any{"plugin": plugin, "key": key, "session_id": out.SessionID})
	return map[string]any{"session_id": out.SessionID, "created": true}, nil
}
