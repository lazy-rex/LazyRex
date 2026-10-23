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

// lockSnapshot is the admission snapshot of the lock (lock_base): the lockBase of each key, as
// JSON. The read takes no flock: the lock is replaced by a rename, so it reads one whole version.
func (h *Hub) lockSnapshot(keys []string) (string, error) {
	lock, err := readLock(lockPath(h.cfg.path, h.cfg.Lock))
	if err != nil {
		return "", refuse("refused", "%v", err)
	}
	snap := map[string]string{}
	for _, k := range keys {
		snap[k] = lockBase(lock, k)
	}
	b, _ := json.Marshal(snap)
	return string(b), nil
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
// attempt -> written; the key's pin is already the op's pin -> written (but if the entry has
// another cache directory that does not verify, the op writes its own entry: a repair); the key's
// pin is still the base (its admission snapshot, lock_base) -> write; else conflict. Before the op
// goes on, also when the pin is written:
//   - the op's cache entry must still verify (a CLI cleanup can remove a directory that no lock
//     names): a pin that does not verify is fetched again;
//   - the graph check (graphRefusals) with the new lock and the managed entries after the op: a
//     refusal that the current lock and entries do not have is conflict, and nothing more is
//     written.
//
// The new lock keeps every other entry as the locked read gave it.
func (h *Hub) opLock(ctx context.Context, op *pluginOp) error {
	var t opResolved
	var snap map[string]string
	if err := json.Unmarshal([]byte(op.Target), &t); err != nil {
		return h.setOpPhase(ctx, op, "failed", "", "lock: the stored target does not read")
	}
	base, ok := "", json.Unmarshal([]byte(op.LockBase), &snap) == nil
	if ok {
		base, ok = snap[t.Source]
	}
	if !ok {
		return h.setOpPhase(ctx, op, "failed", "", "lock: the admission snapshot has no entry for "+t.Source)
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
	// The open attempt is settled first, also when the lock does not read (b is nil then): the op
	// ends, so no later pass settles it.
	b, rerr := os.ReadFile(lockFile)
	if err := h.settleFileAttempt(ctx, op, "write:lock", b); err != nil {
		return err
	}
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("the lock %s: %v; nothing was written", lockFile, rerr))
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
	// written: by this op before a crash, or by another writer.
	written := last != "" && b != nil && hashBytes(b) == last || cur == intended
	// repair: the lock has the op's pin with another writer's cache directory, and that directory
	// does not verify. The op writes its own verified entry: the pin does not change, so the base
	// does not count.
	repair := false
	if written && lock.Plugins[t.Source].Dir != pin.Dir {
		if _, _, err := lockedDir(h.cfg.path, h.cfg.State, lock, e); err != nil {
			written, repair = false, true
		}
	}
	if !written && !repair && cur != base {
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("the lock changed during the %s; run it again (the entry of %s was %s at admission, now %s); nothing was written", op.Kind, t.Source, base, cur))
	}
	next := &pluginLock{Plugins: map[string]*lockEntry{}}
	for k, v := range lock.Plugins {
		next.Plugins[k] = v
	}
	next.Plugins[t.Source] = &pin
	// The graph check runs also when the pin is written: the managed entries can have changed, and
	// another writer's pin had no check of this op's entry.
	if msg, err := h.lockRefusal(op, t, lockFile, lock, next, "the new lock"); err != nil || msg != "" {
		if err != nil {
			msg = err.Error()
		}
		if written {
			return h.setOpPhase(ctx, op, "conflict", "", msg+"; the lock has the operation's pin, and the managed file is unchanged")
		}
		return h.setOpPhase(ctx, op, "conflict", "", msg+"; nothing was written")
	}
	if written {
		return h.setOpPhase(ctx, op, op.nextPhase("locking"), "", "")
	}
	nb, err := encodeLock(next)
	if err != nil {
		return err
	}
	if err := h.renameAttempt(ctx, op, "write:lock", lockFile, nb, "lock-rename"); err != nil {
		return err
	}
	if repair {
		return h.setOpPhase(ctx, op, op.nextPhase("locking"), fmt.Sprintf("the lock had %s at %s with a cache entry that does not verify; wrote the fetched entry", t.Source, pin.Commit), "")
	}
	return h.setOpPhase(ctx, op, op.nextPhase("locking"), fmt.Sprintf("locked %s at %s", t.Source, pin.Commit), "")
}

// lockRefusal runs the graph check twice: with the current lock and managed entries, and with the
// lock next and the managed entries after the op. A refusal of the op's own entry in the second
// check, and a refusal that only the second check has, are the op's: the text, which names what
// (the new lock, or the op), is the result.
func (h *Hub) lockRefusal(op *pluginOp, t opResolved, lockFile string, lock, next *pluginLock, what string) (string, error) {
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
	// The op's own entry must pass, also when the current file already has it (C4 plan r3 A):
	// before and after then refuse it alike, so the new-refusal check alone misses it.
	if own := "managed:" + m.ID; refused[own] != nil {
		return fmt.Sprintf("%s refuses %s: %v", what, own, refused[own]), nil
	}
	idents := make([]string, 0, len(refused))
	for ident := range refused {
		idents = append(idents, ident)
	}
	sort.Strings(idents)
	for _, ident := range idents {
		if before[ident] == nil {
			return fmt.Sprintf("%s refuses %s: %v", what, ident, refused[ident]), nil
		}
	}
	return "", nil
}
