package main

// The locking phase of add and update (step 4 plan, item 4): the lock writer. It writes the op's
// pin into the lock file under both plugin command flocks, as a CLI "plugin update" does, with a
// file attempt (write:lock). It never writes <lock>.peers.json: a manager change is a local
// change, as a CLI update is.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
)

// lockBase is the text of a key's lock entry for the base check: its pin (lockPin, which does not
// depend on the machine) as JSON, or "absent". The cache directory does not count: a CLI sync that
// repairs the directory of the same pin did not change the pin.
func lockBase(l *pluginLock, key string) string {
	le := l.Plugins[key]
	if le == nil {
		return "absent"
	}
	b, _ := json.Marshal(le.lockPin)
	return string(b)
}

// newManaged is the managed entry that the op leaves in the file, with the stored target's
// source: an add's entry from its input, or an update's base entry with the new source.
func (op *pluginOp) newManaged(t opResolved) (managedFileEntry, error) {
	switch op.Kind {
	case "add":
		var in opAddInput
		if err := json.Unmarshal([]byte(op.Input), &in); err != nil {
			return managedFileEntry{}, fmt.Errorf("the stored input: %v", err)
		}
		return managedFileEntry{ID: in.ID, Source: t.Source, Args: in.Args, WatchRoots: in.WatchRoots}, nil
	case "update":
		var m managedFileEntry
		if err := json.Unmarshal([]byte(op.Base), &m); err != nil {
			return managedFileEntry{}, fmt.Errorf("the stored base: %v", err)
		}
		m.Source = t.Source
		return m, nil
	}
	return managedFileEntry{}, fmt.Errorf("%s has no managed entry", op.Kind)
}

// withManaged is list with the managed entry m in place of the entry with m's id (or after the
// last entry), parsed by the strict loader.
func withManaged(list []*configEntry, m managedFileEntry) ([]*configEntry, error) {
	var file struct {
		Plugins []managedFileEntry `json:"plugins"`
	}
	file.Plugins = []managedFileEntry{}
	found := false
	for _, e := range list {
		if e.managed == m.ID {
			file.Plugins, found = append(file.Plugins, m), true
			continue
		}
		file.Plugins = append(file.Plugins, fileEntry(e))
	}
	if !found {
		file.Plugins = append(file.Plugins, m)
	}
	b, err := json.Marshal(file)
	if err != nil {
		return nil, err
	}
	return parseManagedBytes(b)
}

// lastIntended is the intended hash of the op's last file attempt of call, or "".
func (h *Hub) lastIntended(ctx context.Context, op *pluginOp, call string) (string, error) {
	var intended string
	err := h.db.QueryRowContext(ctx, `SELECT intended FROM plugin_op_attempts WHERE op_id = ? AND call = ? ORDER BY at DESC, rowid DESC LIMIT 1`, op.ID, call).Scan(&intended)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return intended, err
}

// opLock writes the op's pin into the lock file, under both plugin command flocks. Its recovery
// rule, after a crash at any point: the file hash is the intended hash of the op's last write:lock
// attempt -> written; the key's pin is already the op's pin -> written; the key's pin is still the
// base (the pin when the target was stored) -> write; else conflict. Before the write:
//   - the op's cache entry must still verify (a CLI cleanup can remove a directory that no lock
//     names): a pin that does not verify is fetched again;
//   - the graph check (graphRefusals) with the new lock and the managed entries after the op: a
//     refusal that the current lock and entries do not have is conflict, and nothing is written.
//
// The new lock keeps every other entry as the locked read gave it.
func (h *Hub) opLock(ctx context.Context, op *pluginOp) error {
	var t opResolved
	if err := json.Unmarshal([]byte(op.Target), &t); err != nil || t.LockBase == "" {
		return h.setOpPhase(ctx, op, "failed", "", "lock: the stored target has no lock base")
	}
	var pin lockEntry
	if err := json.Unmarshal([]byte(op.Pin), &pin); err != nil {
		return h.setOpPhase(ctx, op, "failed", "", "lock: the stored pin does not read")
	}
	e := &configEntry{Source: t.Source}
	if err := e.checkSource(""); err != nil {
		return h.setOpPhase(ctx, op, "failed", "", "lock: "+err.Error())
	}
	lockFile := lockPath(h.cfg.path, h.cfg.Lock)
	release, err := lockCommands(h.cfg.State, lockFile)
	if err != nil {
		if strings.HasPrefix(err.Error(), "another plugin command runs") {
			return h.opBusy(ctx, op, err)
		}
		return err
	}
	defer release()
	delete(h.pops.busy, op.ID)
	b, err := os.ReadFile(lockFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("the lock %s: %v; nothing was written", lockFile, err))
	}
	if err := h.settleFileAttempt(ctx, op, "write:lock", b); err != nil {
		return err
	}
	lock, err := readLock(lockFile)
	if err != nil {
		return h.setOpPhase(ctx, op, "conflict", "", err.Error()+"; nothing was written")
	}
	if _, _, err := lockedDir(h.cfg.path, h.cfg.State, &pluginLock{Plugins: map[string]*lockEntry{t.Source: &pin}}, e); err != nil {
		if _, err := h.db.ExecContext(ctx, `UPDATE plugin_ops SET pin = '', updated = ? WHERE op_id = ?`, time.Now().UnixMilli(), op.ID); err != nil {
			return err
		}
		op.Pin = ""
		return h.setOpPhase(ctx, op, "fetching", fmt.Sprintf("the fetched entry does not verify: %v; fetching again", err), "")
	}
	last, err := h.lastIntended(ctx, op, "write:lock")
	if err != nil {
		return err
	}
	intended := lockBase(&pluginLock{Plugins: map[string]*lockEntry{t.Source: &pin}}, t.Source)
	cur := lockBase(lock, t.Source)
	switch {
	case last != "" && b != nil && hashBytes(b) == last, cur == intended:
		return h.setOpPhase(ctx, op, op.nextPhase("locking"), "", "") // written (before a crash, or by another writer)
	case cur != t.LockBase:
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("the lock entry of %s changed since the operation resolved it: %s, now %s; nothing was written", t.Source, t.LockBase, cur))
	}
	next := &pluginLock{Plugins: map[string]*lockEntry{}}
	for k, v := range lock.Plugins {
		next.Plugins[k] = v
	}
	next.Plugins[t.Source] = &pin
	if msg, err := h.lockRefusal(op, t, lockFile, lock, next); err != nil || msg != "" {
		if err != nil {
			msg = err.Error()
		}
		return h.setOpPhase(ctx, op, "conflict", "", msg+"; nothing was written")
	}
	nb, err := encodeLock(next)
	if err != nil {
		return err
	}
	if err := h.renameAttempt(ctx, op, "write:lock", lockFile, nb, "lock-rename"); err != nil {
		return err
	}
	return h.setOpPhase(ctx, op, op.nextPhase("locking"), fmt.Sprintf("locked %s at %s", t.Source, pin.Commit), "")
}

// lockRefusal runs the graph check twice: with the current lock and managed entries, and with the
// new lock and the managed entries after the op. A refusal that only the second check has is the
// op's: its text is the result.
func (h *Hub) lockRefusal(op *pluginOp, t opResolved, lockFile string, lock, next *pluginLock) (string, error) {
	_, list, err := readManaged(h.cfg.managedPath)
	if err != nil {
		return "", fmt.Errorf("managed plugin file %s: %v", h.cfg.managedPath, err)
	}
	m, err := op.newManaged(t)
	if err != nil {
		return "", err
	}
	after, err := withManaged(list, m)
	if err != nil {
		return "", fmt.Errorf("the managed entry after the operation is not valid: %v", err)
	}
	before, refused := h.opGraph(lockFile, list, lock), h.opGraph(lockFile, after, next)
	idents := make([]string, 0, len(refused))
	for ident := range refused {
		idents = append(idents, ident)
	}
	sort.Strings(idents)
	for _, ident := range idents {
		if before[ident] == nil {
			return fmt.Sprintf("the new lock refuses %s: %v", ident, refused[ident]), nil
		}
	}
	return "", nil
}
