package main

// Durable grants (the coordinator, 2026-10-19; Codex's review of the grant contract). A plugin
// that admits deferred work for an agent (a template start that opens its tabs after the agent's
// call returned, also after a restart) cannot use on_behalf: the forward has ended. While it
// handles the agent's forward, the plugin asks for a grant of the exact requests that the work
// will send. Later it sends each request with grant_id, and the hub admits it as that agent only
// when it matches one stored entry exactly.
//
// hub.grant.create {on_behalf, key, entries: [{action, key, request}]} -> {grant_id, state}
//   - Only a registered plugin, only while a hub.forward to that plugin client with
//     requester_pid = on_behalf is in flight (layer 1). The hub stores the agent identity, never
//     the pid.
//   - action: only hub.spawn_window. request: {session?, title?, layout}; session is a session ID
//     or {ensure_key}: the session that hub.session.ensure made for this plugin and key. Any other
//     field is refused (no for, on_behalf, grant_id or placement). Keys are unique.
//   - Idempotent per (plugin name, key): the same agent and entries return the same grant, also
//     a revoked one (it never becomes active again). Anything else is a conflict.
// hub.grant.get {key} -> {found, grant_id, state, agent, entries}: lookup for recovery, owning
//   plugin only. It creates nothing.
// hub.grant.revoke {grant_id} -> {grant_id, state: "revoked"}: owning plugin only, idempotent.
//   It does not cancel a Rex call already sent.
// hub.spawn_window {grant_id, key, session?, title?, layout}: see grantCaller.
//
// The plugin is identified by its registered name, so a grant survives a plugin or hub restart
// (a new client ID). The grant keeps the agent ID that made it; another ID of the same agent
// (identity.go, agentIDs) replays the create, and the spawns it admits follow the same rule.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const grantsSchema = `
CREATE TABLE IF NOT EXISTS grants (plugin TEXT NOT NULL, key TEXT NOT NULL, grant_id TEXT NOT NULL UNIQUE,
  agent TEXT NOT NULL, entries TEXT NOT NULL, state TEXT NOT NULL, created TEXT NOT NULL,
  revoked TEXT NOT NULL DEFAULT '', PRIMARY KEY (plugin, key));
`

// grantActions are the actions a grant can name. A new action needs a hub change.
var grantActions = map[string]bool{"hub.spawn_window": true}

type grantEntry struct {
	Action  string          `json:"action"`
	Key     string          `json:"key"`
	Request json.RawMessage `json:"request"`
}

// canonical is the comparison form of a decoded JSON value: json.Marshal sorts object keys.
func canonical(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// grantEntries checks the entries of a create and returns them in canonical form.
func grantEntries(raw any) ([]grantEntry, error) {
	list, ok := raw.([]any)
	// No count limit: an agent start supports every template the UI supports. The bound is the
	// 64 KiB control message that carries the create (the coordinator, 2026-10-19).
	if !ok || len(list) == 0 {
		return nil, bad("entries must be a nonempty list of requests")
	}
	seen := map[string]bool{}
	out := make([]grantEntry, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok || len(m) != 3 {
			return nil, bad("each entry must be {action, key, request}")
		}
		action, _ := m["action"].(string)
		key, _ := m["key"].(string)
		req, ok := m["request"].(map[string]any)
		if !grantActions[action] {
			return nil, bad("a grant cannot name the action %q", action)
		}
		if key == "" || len(key) > 100 || !ok {
			return nil, bad("each entry needs a key (at most 100 characters) and a request object")
		}
		if seen[action+"\x00"+key] {
			return nil, bad("the key %q is in two entries", key)
		}
		seen[action+"\x00"+key] = true
		if err := checkGrantRequest(req); err != nil {
			return nil, err
		}
		c, err := canonical(req)
		if err != nil {
			return nil, err
		}
		out = append(out, grantEntry{Action: action, Key: key, Request: json.RawMessage(c)})
	}
	return out, nil
}

// checkGrantRequest accepts only the fields of a spawn_window request that a grant can fix.
func checkGrantRequest(req map[string]any) error {
	for k, v := range req {
		switch k {
		case "title":
			if s, ok := v.(string); !ok || s == "" {
				return bad("request.title must be nonempty text")
			}
		case "session":
			if s, ok := v.(string); ok && s != "" {
				continue
			}
			m, ok := v.(map[string]any)
			if ek, _ := m["ensure_key"].(string); !ok || len(m) != 1 || ek == "" {
				return bad("request.session must be a session ID or {ensure_key}")
			}
		case "layout":
			var leaves []*windowLeaf
			if _, err := parseWindowNode(v, &leaves); err != nil {
				return err
			}
		default:
			return bad("a granted request cannot contain %q", k)
		}
	}
	if req["layout"] == nil {
		return bad("request.layout is required")
	}
	return nil
}

func newGrantID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "grant-" + hex.EncodeToString(b), nil
}

func (h *Hub) grantCreate(ctx context.Context, p perform, plugin string, args map[string]any) (any, error) {
	for k := range args {
		if k != "on_behalf" && k != "key" && k != "entries" {
			return nil, bad("unknown field %q", k)
		}
	}
	if args["on_behalf"] == nil {
		return nil, bad("on_behalf is required: a grant is made only while the agent's forwarded call runs")
	}
	key, _ := args["key"].(string)
	if key == "" || len(key) > 200 {
		return nil, bad("key must be text of 1 to 200 bytes")
	}
	entries, err := grantEntries(args["entries"])
	if err != nil {
		return nil, err
	}
	agent, err := h.spawnCaller(ctx, p, map[string]any{"on_behalf": args["on_behalf"]})
	if err != nil {
		return nil, err
	}
	stored, _ := json.Marshal(entries)
	id, err := newGrantID()
	if err != nil {
		return nil, err
	}
	// One statement inserts or keeps the row (no transaction: sameAgent reads the database, and
	// the hub's database has one connection).
	res, err := h.db.ExecContext(ctx, `INSERT INTO grants (plugin, key, grant_id, agent, entries, state, created) VALUES (?, ?, ?, ?, ?, 'active', ?)
		ON CONFLICT (plugin, key) DO NOTHING`, plugin, key, id, agent, string(stored), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var prevID, prevAgent, prevEntries, state string
		if err := h.db.QueryRowContext(ctx, `SELECT grant_id, agent, entries, state FROM grants WHERE plugin = ? AND key = ?`, plugin, key).Scan(&prevID, &prevAgent, &prevEntries, &state); err != nil {
			return nil, err
		}
		same, err := h.sameAgent(ctx, agent, prevAgent)
		if err != nil {
			return nil, err
		}
		if !same || prevEntries != string(stored) {
			return nil, refuse("operation_conflict", "the grant key %q was used for another agent or other requests", key)
		}
		return map[string]any{"grant_id": prevID, "state": state}, nil
	}
	h.ev.emit("grant_created", map[string]any{"plugin": plugin, "key": key, "grant_id": id, "agent": agent, "entries": len(entries)})
	return map[string]any{"grant_id": id, "state": "active"}, nil
}

func (h *Hub) grantGet(ctx context.Context, plugin string, args map[string]any) (any, error) {
	key, _ := args["key"].(string)
	if key == "" || len(args) != 1 {
		return nil, bad("hub.grant.get takes only key")
	}
	var id, agent, entries, state string
	switch err := h.db.QueryRowContext(ctx, `SELECT grant_id, agent, entries, state FROM grants WHERE plugin = ? AND key = ?`, plugin, key).Scan(&id, &agent, &entries, &state); {
	case errors.Is(err, sql.ErrNoRows):
		return map[string]any{"found": false}, nil
	case err != nil:
		return nil, err
	}
	return map[string]any{"found": true, "grant_id": id, "agent": agent, "state": state, "entries": json.RawMessage(entries)}, nil
}

func (h *Hub) grantRevoke(ctx context.Context, plugin string, args map[string]any) (any, error) {
	id, _ := args["grant_id"].(string)
	if id == "" || len(args) != 1 {
		return nil, bad("hub.grant.revoke takes only grant_id")
	}
	res, err := h.db.ExecContext(ctx, `UPDATE grants SET state = 'revoked', revoked = ? WHERE plugin = ? AND grant_id = ? AND state = 'active'`,
		time.Now().UTC().Format(time.RFC3339Nano), plugin, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		h.ev.emit("grant_revoked", map[string]any{"plugin": plugin, "grant_id": id})
		return map[string]any{"grant_id": id, "state": "revoked"}, nil
	}
	var state string
	if err := h.db.QueryRowContext(ctx, `SELECT state FROM grants WHERE plugin = ? AND grant_id = ?`, plugin, id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return nil, refuse("not_found", "no grant %s of this plugin", id)
	} else if err != nil {
		return nil, err
	}
	return map[string]any{"grant_id": id, "state": state}, nil
}

// grantCaller admits a call with grant_id as the grant's agent. The calling plugin must own the
// active grant, and the call must match one entry exactly: action, key, and every request field.
// A session reference {ensure_key} matches only the session that hub.session.ensure made for this
// plugin and key (state done); it never creates or chooses a session.
//
// spawnWindow calls it twice: before its work, and again with its admission transaction (q = tx),
// so a revoke that commits before the admission refuses it. An admission that commits first may
// finish; a revoke does not cancel it or a Rex call already sent.
func (h *Hub) grantCaller(ctx context.Context, q dbQuerier, p perform, action string, args map[string]any) (string, error) {
	for _, k := range []string{"for", "on_behalf", "placement"} {
		if _, ok := args[k]; ok {
			return "", bad("grant_id cannot be combined with %s", k)
		}
	}
	pl := h.pluginByClient(p.Requester.ClientID)
	if pl == nil {
		return "", bad("only a registered plugin may use a grant")
	}
	id, _ := args["grant_id"].(string)
	var agent, entries, state string
	switch err := q.QueryRowContext(ctx, `SELECT agent, entries, state FROM grants WHERE plugin = ? AND grant_id = ?`, pl.Name, id).Scan(&agent, &entries, &state); {
	case errors.Is(err, sql.ErrNoRows):
		return "", refuse("refused", "no grant %q of this plugin", id)
	case err != nil:
		return "", err
	}
	if state != "active" {
		return "", refuse("refused", "the grant %s is %s", id, state)
	}
	var list []grantEntry
	if err := json.Unmarshal([]byte(entries), &list); err != nil {
		return "", err
	}
	key, _ := args["key"].(string)
	req := map[string]any{}
	for k, v := range args {
		if k != "grant_id" && k != "key" {
			req[k] = v
		}
	}
	got, err := canonical(req)
	if err != nil {
		return "", err
	}
	for _, e := range list {
		if e.Action != action || e.Key != key {
			continue
		}
		var want map[string]any
		if err := json.Unmarshal(e.Request, &want); err != nil {
			return "", err
		}
		if ref, ok := want["session"].(map[string]any); ok {
			ek, _ := ref["ensure_key"].(string)
			var sess, st string
			err := q.QueryRowContext(ctx, `SELECT state, session_id FROM ui_ensured WHERE plugin = ? AND key = ?`, pl.Name, ek).Scan(&st, &sess)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && (st != "done" || sess == "")) {
				return "", refuse("refused", "the granted session (ensure key %q) is not made yet", ek)
			} else if err != nil {
				return "", err
			}
			want["session"] = sess
		}
		c, err := canonical(want)
		if err != nil {
			return "", err
		}
		if c != got {
			return "", refuse("refused", "the request for key %q is not the granted request", key)
		}
		return agent, nil
	}
	return "", refuse("refused", "the grant %s does not cover %s with key %q", id, action, key)
}
