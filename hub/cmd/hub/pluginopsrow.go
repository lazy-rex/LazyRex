package main

// The plugins row of an add (step 4 plan r2 A, addenda 1 and 3, the hub lead's final row guard
// lead-b8-step4-final, and C4 plan r2 item 3 with codex-27's correction B). The add writes the row
// of the fetched manifest name, desired stopped, before its managed write. So the reload that
// loads the new entry starts nothing, and the bound start of the starting phase is the only start
// of an add.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// opRowPrior is the record of an add's row write (plugin_ops.row_prior): the row's name, whether
// the add inserted it, the values that the add wrote, and, for a reused row, the values that the
// reuse changed, as they were. The guard makes selected_run and the live fields empty before and
// after the write, so they need no prior value.
type opRowPrior struct {
	Name     string     `json:"name"`
	Inserted bool       `json:"inserted"`
	Wrote    opRowStamp `json:"wrote"`
	Prior    *opRowPrev `json:"prior,omitempty"`
}

type opRowStamp struct {
	EntryIdentity string `json:"entry_identity"`
	EntryStamp    string `json:"entry_stamp"`
}

type opRowPrev struct {
	Desired       string `json:"desired"`
	EntryIdentity string `json:"entry_identity"`
	EntryStamp    string `json:"entry_stamp"`
}

// pluginRowCols are the plugins row columns that the row write and its owner check read.
type pluginRowCols struct {
	desired, instance, session, block string
	pid                               int
	start, run, selected              string
	identity, stamp                   string
}

type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readRowCols reads the plugins row of name: nil when there is none.
func readRowCols(ctx context.Context, q rowQueryer, name string) (*pluginRowCols, error) {
	var c pluginRowCols
	err := q.QueryRowContext(ctx, `SELECT desired, instance_id, session_id, block_id, child_pid, child_start, run, selected_run, entry_identity, entry_stamp FROM plugins WHERE name = ?`, name).
		Scan(&c.desired, &c.instance, &c.session, &c.block, &c.pid, &c.start, &c.run, &c.selected, &c.identity, &c.stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// owned reports whether the row still has the values that the add wrote: its entry_identity and
// its stored entry_stamp are the written ones, the stamp computed from the current columns is the
// written stamp, and selected_run is empty. The computed stamp covers desired, instance_id and the
// live fields: a hub that does not know entry_identity (OLD) can change them and keep the stored
// stamp, so the stored stamp alone is not enough.
func (c *pluginRowCols) owned(w opRowStamp) bool {
	return c != nil && c.identity == w.EntryIdentity && c.stamp == w.EntryStamp && c.selected == "" &&
		entryStamp(c.desired, c.instance, c.session, c.block, c.pid, c.start, c.run) == w.EntryStamp
}

// free reports whether the row can be reused: desired stopped, and no instance bound or running
// (session, block, run and child empty; a retired instance_id may stay), and no selected run.
func (c *pluginRowCols) free() bool {
	return c.desired == "stopped" && c.session == "" && c.block == "" && c.pid == 0 && c.start == "" && c.run == "" && c.selected == ""
}

// addRow writes the row of name for the add's entry identity, in the writing phase, after the
// writing checks and before the managed rename. The guard, the row write and the record in the op
// row are one transaction, so the outcome is never unknown and there is no attempt row. The guard:
// no row, or a free row (free); no spawn intent of name that is prepared or sending; no unsettled
// attempt (no proof and not resolved) of an op on name; no open bound handler of an op on name
// (bound_at set, outcome empty), even when proof or an operator settles its attempt. The graph
// check of the writing phase covers "no entry gives name". A failed guard is conflict, and nothing
// more is written.
//   - Insert: desired stopped, the entry identity and a fresh entry_stamp.
//   - Reuse: desired stopped, the entry identity and a fresh entry_stamp; instance_id stays (the
//     retired instance: an earlier op's stop observation of it needs the earlier identity, which
//     no longer matches, so the reuse cannot make a false proof).
//
// A replay with the record: a row that the add still owns is not written again; any other row is
// conflict (setOpPhase then leaves it as it is). stop reports that the phase is set.
func (h *Hub) addRow(ctx context.Context, op *pluginOp, name, identity string) (stop bool, err error) {
	if op.RowPrior != "" {
		var p opRowPrior
		if err := json.Unmarshal([]byte(op.RowPrior), &p); err != nil {
			return true, h.setOpPhase(ctx, op, "failed", "", "write: the stored row record does not read")
		}
		c, err := readRowCols(ctx, h.db, p.Name)
		if err != nil {
			return true, err
		}
		if c.owned(p.Wrote) {
			return false, nil
		}
		return true, h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("the record of %s is not the one that the add wrote; nothing more was written", p.Name))
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	c, err := readRowCols(ctx, tx, name)
	if err != nil {
		return true, err
	}
	var busy int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM spawn_intents WHERE plugin = ? AND status IN ('prepared', 'sending'))
		+ (SELECT COUNT(*) FROM plugin_op_attempts a JOIN plugin_ops o ON o.op_id = a.op_id WHERE o.name = ? AND (a.proof = '' AND a.resolved_by = '' OR a.bound_at != '' AND a.outcome = ''))`, name, name).Scan(&busy); err != nil {
		return true, err
	}
	if busy > 0 || c != nil && !c.free() {
		tx.Rollback()
		return true, h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("a plugin named %s has a record or a start in flight; nothing more was written", name))
	}
	p := opRowPrior{Name: name, Inserted: c == nil}
	instance := ""
	if c != nil {
		instance = c.instance
		p.Prior = &opRowPrev{Desired: c.desired, EntryIdentity: c.identity, EntryStamp: c.stamp}
	}
	p.Wrote = opRowStamp{EntryIdentity: identity, EntryStamp: entryStamp("stopped", instance, "", "", 0, "", "")}
	if c == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO plugins (name, desired, entry_identity, entry_stamp) VALUES (?, 'stopped', ?, ?)`, name, p.Wrote.EntryIdentity, p.Wrote.EntryStamp)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE plugins SET desired = 'stopped', entry_identity = ?, entry_stamp = ? WHERE name = ?`, p.Wrote.EntryIdentity, p.Wrote.EntryStamp, name)
	}
	if err != nil {
		return true, err
	}
	b, _ := json.Marshal(p)
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_ops SET row_prior = ?, updated = ? WHERE op_id = ?`, string(b), time.Now().UnixMilli(), op.ID); err != nil {
		return true, err
	}
	if err := tx.Commit(); err != nil {
		return true, err
	}
	op.RowPrior = string(b)
	if h.opFault("plugin-op-after-row") {
		return true, errOpCrash
	}
	return false, nil
}

// entryRow is addRow for the add's intended entry (canonical JSON): the row of the pin's manifest
// name, with the entry's identity. opWrite calls it before the rename, and on a replay that finds
// the file written, so the add goes on only while it owns the row.
func (h *Hub) entryRow(ctx context.Context, op *pluginOp, intended string) (stop bool, err error) {
	var m managedFileEntry
	var pin lockEntry
	json.Unmarshal([]byte(intended), &m)
	json.Unmarshal([]byte(op.Pin), &pin)
	return h.addRow(ctx, op, pin.Name, entryIdentity(&configEntry{managed: m.ID, Source: m.Source}))
}

// endAddRow stores the end (phase failed, conflict or unknown) of an add that wrote its row. When
// the managed file has no entry of the add, the same transaction removes the row's write (1d): it
// deletes an inserted row, or writes back the prior values of a reused row, only while the add
// still owns the row (owned). A row that another writer changed is left as it is, and the end text
// says so. With the add's entry in the file, the row stays: it is the entry's record.
func (h *Hub) endAddRow(ctx context.Context, op *pluginOp, phase, result, errText string) error {
	var p opRowPrior
	if err := json.Unmarshal([]byte(op.RowPrior), &p); err != nil {
		return err
	}
	_, list, err := readManaged(h.cfg.managedPath)
	tx, txErr := h.db.BeginTx(ctx, nil)
	if txErr != nil {
		return txErr
	}
	defer tx.Rollback()
	switch {
	case err != nil:
		errText += fmt.Sprintf("; the managed file does not read, so the add left the record of %s as it is", p.Name)
	case managedByID(list, op.managedID()) != nil:
	default:
		c, err := readRowCols(ctx, tx, p.Name)
		if err != nil {
			return err
		}
		switch {
		case !c.owned(p.Wrote):
			errText += fmt.Sprintf("; the record of %s changed after the add wrote it, and the add left it as it is", p.Name)
		case p.Inserted:
			_, err = tx.ExecContext(ctx, `DELETE FROM plugins WHERE name = ?`, p.Name)
		default:
			_, err = tx.ExecContext(ctx, `UPDATE plugins SET desired = ?, entry_identity = ?, entry_stamp = ? WHERE name = ?`, p.Prior.Desired, p.Prior.EntryIdentity, p.Prior.EntryStamp, p.Name)
		}
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_ops SET phase = ?, result = ?, error = ?, updated = ? WHERE op_id = ?`,
		phase, result, errText, time.Now().UnixMilli(), op.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	h.opPhaseStored(op, phase, result, errText)
	return nil
}
