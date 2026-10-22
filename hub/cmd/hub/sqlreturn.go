package main

// hub.admin.sql_return (docs/sql.md section 11): the rollback of a plugin from hub.sql to its store
// version. With the plugin stopped, the hub moves the plugin's SQL file aside and gives the notice
// path back to hub.store.batch. The steps, each safe to run again:
//  1. A marker row sql_returns(plugin, stamp) in hub.db. While it exists, the hub does not open
//     the file (hub.sql is unavailable) and the start drain skips it.
//  2. The outbox drains under the plugin's SQL lock (a drain is crash-safe through its receipts).
//  3. The hub closes its writer and reader of the file.
//  4. <name>.db, then -wal and -shm, move to <state>/sql-returned/<name>-<stamp>/, outside
//     <state>/sql. A file that is already moved is skipped.
//  5. One hub.db transaction deletes the sql_notice_plugins row, the receipts and the marker.
// A crash between two steps leaves the marker: no start replays the outbox or opens the file, and
// the next call continues with the same stamp.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// sqlReturnHook is a test hook: it runs after each step of hub.admin.sql_return ("marked",
// "drained", "closed", "moved"), and an error stops the call there, as a crash does.
var sqlReturnHook func(step string) error

func sqlReturnStep(step string) error {
	if sqlReturnHook != nil {
		return sqlReturnHook(step)
	}
	return nil
}

func (h *Hub) adminSQLReturn(ctx context.Context, args map[string]any) (any, error) {
	for k := range args {
		if k != "plugin" {
			return nil, bad("hub.admin.sql_return: unknown field %q", k)
		}
	}
	name, _ := args["plugin"].(string)
	if !pluginName.MatchString(name) {
		return nil, bad("hub.admin.sql_return: plugin must be a plugin name")
	}
	if h.sql == nil {
		return nil, refuse("unavailable", "the hub has no SQL service")
	}
	h.mu.Lock()
	pl := h.plugins[name]
	h.mu.Unlock()
	if pl != nil {
		// The lifecycle lock: no start of the plugin during the call.
		pl.lifecycle.Lock()
		defer pl.lifecycle.Unlock()
		h.mu.Lock()
		err := h.loadPluginRow(ctx, pl)
		busy := pl.Desired == "running" || pl.ClientID != ""
		h.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if busy {
			return nil, refuse("refused", "plugin %s is running or desired running: stop it first (hub.admin.stop)", name)
		}
	}
	stamp, resumed, err := h.db.markSQLReturn(ctx, name, time.Now().UTC().Format("20060102T150405Z"))
	if err != nil {
		return nil, err
	}
	if err := sqlReturnStep("marked"); err != nil {
		return nil, err
	}
	dest := filepath.Join(h.cfg.State, "sql-returned", name+"-"+stamp)
	moved, err := h.sql.retire(ctx, name, dest)
	if err != nil {
		return nil, err
	}
	if err := h.db.finishSQLReturn(ctx, name); err != nil {
		return nil, err
	}
	// returned names the backup of this return: moved now, or by the earlier call that this one
	// completes.
	returned := ""
	if _, err := os.Stat(dest); err == nil && (moved || resumed) {
		returned = dest
	}
	h.ev.emit("sql_returned", map[string]any{"plugin": name, "returned": returned})
	return map[string]any{"returned": returned}, nil
}

// retire is steps 2 to 4: drain the outbox, close the file, and move the file set to dest. moved:
// it moved a file.
func (s *sqlService) retire(ctx context.Context, name, dest string) (moved bool, err error) {
	path := filepath.Join(s.dir, name+".db")
	if _, err := os.Stat(path); err == nil {
		p, err := s.load(ctx, name)
		if err != nil {
			return false, refuse("unavailable", "open the database of %s: %v", name, err)
		}
		p.lock <- struct{}{}
		if !p.closed {
			if err := s.drain(ctx, p); err != nil {
				p.release()
				return false, refuse("unavailable", "the outbox of %s did not drain: %v; nothing was moved, call again", name, err)
			}
			if err := sqlReturnStep("drained"); err != nil {
				p.release()
				return false, err
			}
			p.rmu.Lock()
			p.end(ctx, "unavailable")
			p.closed = true
			p.w.close()
			p.r.close()
			p.rmu.Unlock()
		}
		s.mu.Lock()
		if s.dbs[name] == p {
			delete(s.dbs, name)
		}
		s.mu.Unlock()
		p.release()
		if err := sqlReturnStep("closed"); err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := path + suffix
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue // never there, or moved by an earlier call
		}
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return false, err
		}
		if err := os.Rename(src, filepath.Join(dest, name+".db"+suffix)); err != nil {
			return false, err
		}
		moved = true
	}
	if moved {
		if err := syncDir(s.dir); err != nil {
			return false, err
		}
		if err := syncDir(dest); err != nil {
			return false, err
		}
	}
	return moved, sqlReturnStep("moved")
}

// markSQLReturn writes the marker of a return, or keeps the one of an earlier call (resumed), and
// returns its stamp.
func (db *DB) markSQLReturn(ctx context.Context, plugin, stamp string) (string, bool, error) {
	res, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO sql_returns VALUES (?, ?)`, plugin, stamp)
	if err != nil {
		return "", false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, err
	}
	err = db.QueryRowContext(ctx, `SELECT stamp FROM sql_returns WHERE plugin = ?`, plugin).Scan(&stamp)
	return stamp, n == 0, err
}

// sqlReturning reports a marker of plugin. A failed read counts as a marker: the file stays
// closed rather than open while a return may run.
func (db *DB) sqlReturning(plugin string) bool {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sql_returns WHERE plugin = ?`, plugin).Scan(&n); err != nil {
		return true
	}
	return n > 0
}

// finishSQLReturn is step 5: the notice path goes back to hub.store.batch, and the marker goes.
func (db *DB) finishSQLReturn(ctx context.Context, plugin string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM sql_notice_plugins WHERE plugin = ?`, `DELETE FROM notice_receipts WHERE plugin = ?`, `DELETE FROM sql_returns WHERE plugin = ?`} {
		if _, err := tx.ExecContext(ctx, q, plugin); err != nil {
			return err
		}
	}
	return tx.Commit()
}
