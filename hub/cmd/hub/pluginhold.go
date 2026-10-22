package main

// The release hold (docs/hub.md, "The release hold"; the hub lead's lead-b8-final with
// addendum 1, addendum 2 and its audit). A release tool acquires the hold before it freezes the
// plugin store: while the row exists, the manager admits no new write op. The tool freezes only
// on state held: no admitted write op is open, and every attempt has a proof or an operator
// resolution. The row survives a hub restart; no timeout and no lost connection clears it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var holdID = regexp.MustCompile(`^[a-z0-9._-]{1,128}$`)

// immediate runs fn in one BEGIN IMMEDIATE transaction: the write lock is taken at the start, so
// a check and the write that it guards are one step (the atomic boundary of the hold).
func (h *Hub) immediate(ctx context.Context, fn func(c *sql.Conn) error) (err error) {
	c, err := h.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			c.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := fn(c); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "COMMIT")
	return err
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// holder is the hold row: release_id and acquired_at, "" when there is no row.
func holder(ctx context.Context, q queryer) (id, at string, err error) {
	err = q.QueryRowContext(ctx, `SELECT release_id, acquired_at FROM plugin_hold WHERE one = 1`).Scan(&id, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return id, at, err
}

// heldRefusal is the admission's refusal while a hold row exists.
func heldRefusal(id string) error {
	return refuse("held", "the release %s holds the plugin manager; no new write operation starts until it releases the hold", id)
}

func holdArgs(args map[string]json.RawMessage, want bool) (string, error) {
	for k := range args {
		if k != "release_id" || !want {
			return "", refuse("invalid_input", "unknown field %s", k)
		}
	}
	if !want {
		return "", nil
	}
	var id string
	if raw, ok := args["release_id"]; !ok || json.Unmarshal(raw, &id) != nil || !holdID.MatchString(id) {
		return "", refuse("invalid_input", "release_id must match [a-z0-9._-]{1,128}")
	}
	return id, nil
}

// holdAction answers hub.plugins.hold.acquire, hub.plugins.hold.release and
// hub.plugins.hold.status (the operator caller path, never the agent face).
func (h *Hub) holdAction(ctx context.Context, action string, args map[string]json.RawMessage) (any, error) {
	switch action {
	case "hub.plugins.hold.status":
		if _, err := holdArgs(args, false); err != nil {
			return nil, err
		}
		return h.holdStatus(ctx)
	case "hub.plugins.hold.acquire", "hub.plugins.hold.release":
	default:
		return nil, bad("unknown action %s", action)
	}
	id, err := holdArgs(args, true)
	if err != nil {
		return nil, err
	}
	changed := false
	err = h.immediate(ctx, func(c *sql.Conn) error {
		cur, _, err := holder(ctx, c)
		if err != nil {
			return err
		}
		switch {
		case cur != "" && cur != id:
			return refuse("held_by_other", "the hold belongs to release_id %s", cur)
		case action == "hub.plugins.hold.acquire" && cur == "":
			changed = true
			_, err = c.ExecContext(ctx, `INSERT INTO plugin_hold (one, release_id, acquired_at) VALUES (1, ?, ?)`, id, time.Now().UTC().Format(time.RFC3339Nano))
		case action == "hub.plugins.hold.release" && cur == id:
			changed = true
			_, err = c.ExecContext(ctx, `DELETE FROM plugin_hold WHERE one = 1 AND release_id = ?`, id)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if changed {
		h.ev.emit("plugin_hold", map[string]any{"action": action, "release_id": id})
		h.changed("plugins")
	}
	// The state comes from a read after the commit: each op is either refused at admission or in
	// this state's lists.
	return h.holdStatus(ctx)
}

// holdStatus is the reply of every hold action: {release_id, state, acquired_at, inflight,
// unresolved}.
//   - inflight: every admitted write op in a non-final phase, queued ones included. The only
//     exclusion: an agent add or update with no stored confirmation (a request).
//   - unresolved: every attempt of an op with no proof and no operator resolution, when the op is
//     final or the attempt has an outcome (an open attempt of an open op is inflight).
//
// state: none (no row), draining (inflight), unresolved, or held.
func (h *Hub) holdStatus(ctx context.Context) (any, error) {
	id, at, err := holder(ctx, h.db)
	if err != nil {
		return nil, err
	}
	inflight := []map[string]any{}
	rows, err := h.db.QueryContext(ctx, `SELECT op_id, kind, phase FROM plugin_ops
		WHERE phase NOT IN ('done', 'failed', 'conflict', 'unknown')
		AND NOT (json_extract(origin, '$.kind') = 'agent' AND kind IN ('add', 'update') AND confirmed = '')
		ORDER BY created, rowid`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var op, kind, phase string
		if err := rows.Scan(&op, &kind, &phase); err != nil {
			rows.Close()
			return nil, err
		}
		inflight = append(inflight, map[string]any{"op_id": op, "kind": kind, "phase": phase})
	}
	rows.Close()
	unresolved := []map[string]any{}
	rows, err = h.db.QueryContext(ctx, `SELECT a.op_id, a.attempt, a.call, a.at FROM plugin_op_attempts a JOIN plugin_ops o ON o.op_id = a.op_id
		WHERE a.proof = '' AND a.resolved_by = '' AND (a.outcome != '' OR o.phase IN ('done', 'failed', 'conflict', 'unknown'))
		ORDER BY a.at, a.rowid`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var op, attempt, call string
		var since int64
		if err := rows.Scan(&op, &attempt, &call, &since); err != nil {
			rows.Close()
			return nil, err
		}
		unresolved = append(unresolved, map[string]any{"op_id": op, "attempt": attempt, "call": call,
			"since": time.UnixMilli(since).UTC().Format(time.RFC3339Nano)})
	}
	rows.Close()
	state := "held"
	switch {
	case id == "":
		state = "none"
	case len(inflight) > 0:
		state = "draining"
	case len(unresolved) > 0:
		state = "unresolved"
	}
	return map[string]any{"release_id": id, "state": state, "acquired_at": at, "inflight": inflight, "unresolved": unresolved}, nil
}

// admitUnderHold inserts an admitted op with insert, in one BEGIN IMMEDIATE transaction with the
// hold check: a hold row refuses the op (code held), and nothing is inserted.
func (h *Hub) admitUnderHold(ctx context.Context, insert func(c *sql.Conn) error) error {
	return h.immediate(ctx, func(c *sql.Conn) error {
		id, _, err := holder(ctx, c)
		if err != nil {
			return err
		}
		if id != "" {
			return heldRefusal(id)
		}
		return insert(c)
	})
}

// attemptResolve answers hub.plugins.attempt.resolve {op_id, attempt, outcome, note} (operator
// only, never on the agent face; lead-b8-add2 C and its audit). It records the operator's decision
// about an unresolved attempt after a look at Rex. It is attribution, not proof: resolved_by is a
// separate field, written once, and the attempt's outcome and proof stay as they are. It never
// retries a mutation.
//   - outcome: applied or not_applied; note: 1 to 1000 bytes, kept as written.
//   - The same outcome and note again returns the same record. Another outcome or note for a
//     resolved attempt, a proved attempt, or an open attempt (no outcome yet: its call or its
//     observation can still end it) is conflict, and nothing changes.
//   - An unknown op or attempt fails, and nothing changes.
func (h *Hub) attemptResolve(ctx context.Context, args map[string]json.RawMessage) (any, error) {
	var op, attempt, outcome, note string
	for k, raw := range args {
		var dst *string
		switch k {
		case "op_id":
			dst = &op
		case "attempt":
			dst = &attempt
		case "outcome":
			dst = &outcome
		case "note":
			dst = &note
		default:
			return nil, refuse("invalid_input", "unknown field %s", k)
		}
		if json.Unmarshal(raw, dst) != nil {
			return nil, refuse("invalid_input", "%s must be a string", k)
		}
	}
	switch {
	case op == "" || attempt == "":
		return nil, refuse("invalid_input", "op_id and attempt are required")
	case outcome != "applied" && outcome != "not_applied":
		return nil, refuse("invalid_input", "outcome must be applied or not_applied")
	case note == "" || len(note) > 1000:
		return nil, refuse("invalid_input", "note must be 1 to 1000 bytes")
	}
	var record string
	stored := false
	err := h.immediate(ctx, func(c *sql.Conn) error {
		var cur, proof, done string
		err := c.QueryRowContext(ctx, `SELECT resolved_by, proof, outcome FROM plugin_op_attempts WHERE op_id = ? AND attempt = ?`, op, attempt).Scan(&cur, &proof, &done)
		if errors.Is(err, sql.ErrNoRows) {
			return refuse("not_found", "op %s has no attempt %s; nothing changed", op, attempt)
		}
		if err != nil {
			return err
		}
		if cur != "" {
			var r struct{ Outcome, Note string }
			json.Unmarshal([]byte(cur), &r)
			if r.Outcome != outcome || r.Note != note {
				return refuse("conflict", "attempt %s is resolved already (%s); nothing changed", attempt, cur)
			}
			record = cur
			return nil
		}
		switch {
		case proof != "":
			return refuse("conflict", "attempt %s is proved (%s); an operator cannot resolve it", attempt, proof)
		case done == "":
			return refuse("conflict", "attempt %s is still open: its call or its observation can still end it", attempt)
		}
		b, _ := json.Marshal(map[string]string{"by": "operator", "at": time.Now().UTC().Format(time.RFC3339Nano), "outcome": outcome, "note": note})
		record = string(b)
		res, err := c.ExecContext(ctx, `UPDATE plugin_op_attempts SET resolved_by = ? WHERE op_id = ? AND attempt = ? AND resolved_by = '' AND proof = ''`, record, op, attempt)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return refuse("conflict", "attempt %s changed during the resolution; nothing changed", attempt)
		}
		stored = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if stored {
		h.ev.emit("plugin_op_resolved", map[string]any{"op_id": op, "attempt": attempt, "outcome": outcome})
		h.changed("plugins")
	}
	var r map[string]any
	json.Unmarshal([]byte(record), &r)
	return map[string]any{"op_id": op, "attempt": attempt, "resolved_by": r, "note": "resolved by operator (attribution, not proof)"}, nil
}
