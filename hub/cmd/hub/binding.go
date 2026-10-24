package main

// Bindings (docs/hub.md, "Bindings"): a plugin reads an agent, then acts on it later. Between the
// read and the effect the pane can change (a resume moved the conversation, /clear, the CLI exited
// under a live shell, a pid was reused). A binding names one generation of "identity X runs in
// block B under process P", so an effect can refuse an older generation. It is a generation
// check, not a permission: it adds no right to any caller.
//
// A binding is "b-<n>". The report transaction gives a pane row a new n when its tuple (current,
// child_pid, child_start, cli_pid, cli_start) changes, and keeps it for a repeat report of the
// same tuple. Every end of the generation sets the row's binding to 0. n comes from binding_ids
// (AUTOINCREMENT), so it never comes back while hub.db stays. A detached identity has a revision
// instead: every lifecycle change of the identity (a state change, an admitted resume) increments
// it.
//
// Limits (documented): a pid reused with the same start time; a hub.db replaced while agents run
// (every binding ends, and callers read hub.agents again).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

const bindingSchema = `
-- The binding counter: one row per binding ever made; older rows are deleted, and AUTOINCREMENT
-- never reuses an n.
CREATE TABLE IF NOT EXISTS binding_ids (n INTEGER PRIMARY KEY AUTOINCREMENT);
`

var bindingForm = regexp.MustCompile(`^b-[0-9]{1,18}$`)

func bindingName(n int64) string { return "b-" + strconv.FormatInt(n, 10) }

// newBinding gives the next binding number, in the caller's transaction.
func newBinding(ctx context.Context, tx *sql.Tx) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO binding_ids DEFAULT VALUES`)
	if err != nil {
		return 0, err
	}
	n, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM binding_ids WHERE n < ?`, n)
	return n, err
}

// parseBinding reads an optional binding argument: 0 when absent.
func parseBinding(v any) (int64, error) {
	if v == nil {
		return 0, nil
	}
	s, ok := v.(string)
	if !ok || !bindingForm.MatchString(s) {
		return 0, refuse("invalid_input", "binding must be \"b-<digits>\" (from hub.agents)")
	}
	n, err := strconv.ParseInt(s[2:], 10, 64)
	if err != nil || n <= 0 {
		return 0, refuse("invalid_input", "binding must be \"b-<digits>\" (from hub.agents)")
	}
	return n, nil
}

// parseRevision reads an optional revision argument: -1 when absent.
func parseRevision(v any) (int64, error) {
	if v == nil {
		return -1, nil
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f != float64(int64(f)) || f > 1<<53 {
		return 0, refuse("invalid_input", "revision must be a non-negative integer (from hub.agents)")
	}
	return int64(f), nil
}

func notCurrent(b int64) error {
	return refuse("not_current", "binding %s is not current: read hub.agents again", bindingName(b))
}

func revisionNotCurrent(r int64) error {
	return refuse("not_current", "revision %d is not current: read hub.agents again", r)
}

// bindingCurrent checks that b is the binding of identity's current generation: the identity's
// pane row has it, and the bound process (the CLI when the conversation follows one, else the
// pane child) still runs with its recorded start time. The process check covers a process that
// changed while no report or loop has seen it yet (for example during a hub stop).
func bindingCurrent(ctx context.Context, q dbQuerier, identity string, b int64) error {
	var binding int64
	var childPID, cliPID int
	var childStart, cliStart string
	err := q.QueryRowContext(ctx, `SELECT binding, child_pid, child_start, cli_pid, cli_start FROM identity_panes WHERE current = ?`, identity).
		Scan(&binding, &childPID, &childStart, &cliPID, &cliStart)
	if errors.Is(err, sql.ErrNoRows) {
		return notCurrent(b)
	}
	if err != nil {
		return fmt.Errorf("binding check: %w", err)
	}
	pid, start := childPID, childStart
	if cliPID > 0 {
		pid, start = cliPID, cliStart
	}
	if binding != b || !bindingAlive(pid, start) {
		return notCurrent(b)
	}
	return nil
}

// bindingAlive is the process check of bindingCurrent. Tests replace it.
var bindingAlive = alive

// bindingsStart gives a binding to each current pane row that has none and whose recorded process
// still runs: a row from before the binding column, whose agent may report nothing more after the
// hub start. A row whose process is gone keeps 0; identityRecover ends its generation. It runs at
// the start, before hub.agents reads the rows.
func (h *Hub) bindingsStart(ctx context.Context) error {
	h.identMu.Lock()
	defer h.identMu.Unlock()
	type pane struct {
		block, start string
		pid          int
	}
	rows, err := h.db.QueryContext(ctx, `SELECT block_id, CASE WHEN cli_pid > 0 THEN cli_pid ELSE child_pid END, CASE WHEN cli_pid > 0 THEN cli_start ELSE child_start END
		FROM identity_panes WHERE current <> '' AND binding = 0`)
	if err != nil {
		return err
	}
	var panes []pane
	for rows.Next() {
		var p pane
		if err := rows.Scan(&p.block, &p.pid, &p.start); err != nil {
			rows.Close()
			return err
		}
		panes = append(panes, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range panes {
		if !bindingAlive(p.pid, p.start) {
			continue
		}
		tx, err := h.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		n, err := newBinding(ctx, tx)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE identity_panes SET binding = ? WHERE block_id = ? AND binding = 0`, n, p.block)
		}
		if err == nil {
			err = tx.Commit()
		}
		tx.Rollback()
		if err != nil {
			return err
		}
	}
	return nil
}
