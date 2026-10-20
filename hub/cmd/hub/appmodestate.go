package main

// App mode is removed, with its state (the user's decision, 2026-10-19). An older build hid the
// user's sessions while an app was open (app mode) and kept a plugin's tabs in its own app session
// (app:<plugin>). removeAppModeState deletes the state of app mode at startup. It changes only
// hub records: it makes no Rex call. So a pane that an old cycle detached stays detached, and its
// process keeps running (docs/hub.md, "Removed: app mode"). The ui_sessions rows of old sessions
// stay: settleLegacySession (ui.go) cleans those sessions up at runtime.

import (
	"context"
	"database/sql"
)

var appModeTables = []string{"ui_cycles", "ui_hidden", "ui_exit_keys", "ui_apps"}

func (h *Hub) removeAppModeState(ctx context.Context) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	removed := map[string]any{}
	dropped := []string{}
	for _, t := range appModeTables {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, t).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if t == "ui_cycles" {
			phases, err := cyclePhases(ctx, tx)
			if err != nil {
				return err
			}
			if len(phases) > 0 {
				removed["cycles"] = phases
			}
		} else {
			var rows int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&rows); err != nil {
				return err
			}
			if rows > 0 {
				removed[t] = rows
			}
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE `+t); err != nil {
			return err
		}
		dropped = append(dropped, t)
	}
	for name, q := range map[string]string{
		"restore_placeholders": `DELETE FROM ui_restores WHERE view_id LIKE 'hidden:%'`,
		"active_state":         `DELETE FROM ui_state WHERE key = 'active'`,
	} {
		res, err := tx.ExecContext(ctx, q)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed[name] = n
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(dropped) > 0 || len(removed) > 0 {
		removed["tables"] = dropped
		h.ev.emit("app_mode_state_removed", removed)
	}
	return nil
}

// cyclePhases counts the old cycles by phase.
func cyclePhases(ctx context.Context, tx *sql.Tx) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT phase, COUNT(*) FROM ui_cycles GROUP BY phase`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	phases := map[string]int{}
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		phases[p] = n
	}
	return phases, rows.Err()
}
