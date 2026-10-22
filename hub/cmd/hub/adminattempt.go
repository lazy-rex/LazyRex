package main

// The op attempt of an admin call (docs/hub.md, "attempt"). hub.admin.start, hub.admin.stop and
// hub.admin.replace take an optional attempt: the ID of a plugin_op_attempts row of the same call.
// Under reloadMu and the lifecycle lock, after the row read, the handler binds the attempt with
// the instance and block that it acts on; then, before its first effect, it commits sent_at. So
// after a hub restart:
//   - bound_at = '' or sent_at = '': no effect of this attempt began (not_applied);
//   - sent_at set: only an observation of the stored identity proves the outcome.
// A call without attempt binds and marks nothing.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// parseAttempt returns the attempt argument: "" when absent; a present attempt must be a
// non-empty string.
func parseAttempt(args map[string]any) (string, error) {
	v, ok := args["attempt"]
	if !ok {
		return "", nil
	}
	s, _ := v.(string)
	if s == "" {
		return "", refuse("invalid_input", "attempt must be a non-empty string")
	}
	return s, nil
}

// bindAttempt binds attempt to call with the target instance and block, once. A row that does
// not exist, of another call, or already bound refuses with attempt_bound, and the call does
// nothing: a replay cannot bind again, and it proves nothing about the attempt that it names.
func (h *Hub) bindAttempt(ctx context.Context, attempt, call, instance, block string) error {
	res, err := h.db.ExecContext(ctx, `UPDATE plugin_op_attempts SET bound_at = ?, target_instance = ?, target_block = ?
		WHERE attempt = ? AND call = ? AND bound_at = ''`, time.Now().UTC().Format(time.RFC3339Nano), instance, block, attempt, call)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	var got string
	err = h.db.QueryRowContext(ctx, `SELECT call FROM plugin_op_attempts WHERE attempt = ?`, attempt).Scan(&got)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return refuse("attempt_bound", "attempt %s: not found; nothing was done", attempt)
	case err != nil:
		return err
	case got != call:
		return refuse("attempt_bound", "attempt %s is for %s, not %s; nothing was done", attempt, got, call)
	}
	return refuse("attempt_bound", "attempt %s is already bound; nothing was done", attempt)
}

// attemptSent commits sent_at of the bound attempt before the call's first effect. The effect
// must not run when this write fails. With no attempt it does nothing.
func (h *Hub) attemptSent(ctx context.Context, attempt string) error {
	if attempt == "" {
		return nil
	}
	if h.opFault("admin-before-sent") {
		return errOpCrash // a test: the hub stops between the binding and sent_at
	}
	res, err := h.db.ExecContext(ctx, `UPDATE plugin_op_attempts SET sent_at = ? WHERE attempt = ? AND bound_at != '' AND sent_at = ''`,
		time.Now().UTC().Format(time.RFC3339Nano), attempt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return refuse("attempt_bound", "attempt %s is not bound, or its sent_at is written; nothing was done", attempt)
	}
	if h.opFault("admin-after-sent") {
		return errOpCrash // a test: the hub stops between sent_at and the first effect
	}
	return nil
}

// insertIntent inserts the spawn intent of instance. In an admin call with a bound attempt
// (pl.opAttempt, under the lifecycle lock), the same transaction writes the attempt's intent_id:
// no intent of the call exists without it.
func (h *Hub) insertIntent(ctx context.Context, pl *Plugin, instance, sess string) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO spawn_intents (instance_id, plugin, status, session_id) VALUES (?, ?, 'prepared', ?)`, instance, pl.Name, sess); err != nil {
		return err
	}
	if pl.opAttempt != "" {
		res, err := tx.ExecContext(ctx, `UPDATE plugin_op_attempts SET intent_id = ? WHERE attempt = ? AND sent_at != '' AND intent_id = ''`, instance, pl.opAttempt)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return errors.Join(err, fmt.Errorf("attempt %s: intent_id was not written", pl.opAttempt))
		}
	}
	return tx.Commit()
}
