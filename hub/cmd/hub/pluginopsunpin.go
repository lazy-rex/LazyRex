package main

// The unpinning phase of update and remove (C5f plan r4; the hub lead's rulings lead-lockorphan and
// lead-c5f3): after the reload, the op removes the lock entry of the source that it stopped naming,
// so that a successful op does not leave an orphan (plugin status not ok). plugin sync removes the
// orphans of other writers. A kept key never stops the op: its reason is in the record (lock_old)
// and in the end text.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// opLockOld is the record of unpinning (plugin_ops.lock_old): the old key, and what happened to it:
// removed (this op's write:lock-old attempt wrote the lock without it), absent (the lock had no
// entry for it), or kept, with the reason.
type opLockOld struct {
	Key     string `json:"key"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

// oldKey is the lock key of the source of the op's managed entry at admission (op.Base), for an
// update or a remove of a GitHub entry; else "". A local source has no lock key.
func (op *pluginOp) oldKey() string {
	if op.Kind != "update" && op.Kind != "remove" {
		return ""
	}
	var m managedFileEntry
	if json.Unmarshal([]byte(op.Base), &m) != nil || !strings.HasPrefix(m.Source, "github:") {
		return ""
	}
	return m.Source
}

// targetKey is the lock key of an update's target source; "" for every other op.
func (op *pluginOp) targetKey() string {
	var t opResolved
	if op.Kind != "update" || json.Unmarshal([]byte(op.Target), &t) != nil {
		return ""
	}
	return t.Source
}

// endRecord is the lock_old value that the store of phase writes, and the end note. An update or a
// remove that ends (a final phase) after its managed write and before unpinning (for example, a
// failed reload) keeps its old key: the record says so, in the same statement as the final phase.
// The note is "; the old lock entry <key> was kept: <reason>" for a final phase of a kept record.
func (op *pluginOp) endRecord(phase string) (rec, note string) {
	rec = op.LockOld
	if !opFinal[phase] {
		return rec, ""
	}
	if old := op.oldKey(); rec == "" && op.Output != "" && old != "" && old != op.targetKey() {
		b, _ := json.Marshal(opLockOld{Key: old, Outcome: "kept", Reason: "the operation ended before unpinning"})
		rec = string(b)
	}
	var r opLockOld
	if rec != "" && json.Unmarshal([]byte(rec), &r) == nil && r.Outcome == "kept" {
		note = fmt.Sprintf("; the old lock entry %s was kept: %s", r.Key, r.Reason)
	}
	return rec, note
}

// withEndNote adds the end note to the text that the op shows at its end: result for done, error
// for the other final phases.
func withEndNote(phase, result, errText, note string) (string, string) {
	switch {
	case note == "":
	case phase == "done":
		result += note
	default:
		errText += note
	}
	return result, errText
}

// opUnpin runs the unpinning phase under both plugin command flocks, in one pass. The order:
//  1. No old key (a local source), or the old key is the update's target key: the next phase.
//  2. Written: the lock is the output of the op's last write:lock-old attempt, or the lock has no
//     entry for the old key: the next phase, with the record removed or absent.
//  3. A read of the lock, the managed file or the config file fails: kept.
//  4. The managed file is not the op's output (an update's intended entry, or no entry for a
//     remove): kept.
//  5. A config entry on disk, a managed entry or a loaded entry names the old key: kept.
//  6. The key's pin is not the admission snapshot (lockBase, without the cache directory): kept.
//  7. Else the op writes the lock without the old key (a file attempt, write:lock-old).
//
// The record is stored with the next phase (setOpPhase), so a replay never decides twice; a replay
// before the phase store does the whole pass again on the current files.
func (h *Hub) opUnpin(ctx context.Context, op *pluginOp) error {
	next := op.nextPhase("unpinning")
	old := op.oldKey()
	if old == "" || old == op.targetKey() {
		return h.setOpPhase(ctx, op, next, "", "")
	}
	done := func(outcome, reason string) error {
		b, _ := json.Marshal(opLockOld{Key: old, Outcome: outcome, Reason: reason})
		was := op.LockOld
		op.LockOld = string(b)
		if err := h.setOpPhase(ctx, op, next, "", ""); err != nil {
			op.LockOld = was
			return err
		}
		return nil
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
	// leaves the phase, so no later pass settles it.
	b, rerr := os.ReadFile(lockFile)
	if err := h.settleFileAttempt(ctx, op, "write:lock-old", b); err != nil {
		return err
	}
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return done("kept", fmt.Sprintf("the lock %s does not read: %v", lockFile, rerr))
	}
	last, err := h.lastIntended(ctx, op, "write:lock-old")
	if err != nil {
		return err
	}
	if last != "" && b != nil && hashBytes(b) == last {
		return done("removed", "")
	}
	lock, err := readLock(lockFile)
	if err != nil {
		return done("kept", fmt.Sprintf("the lock %s does not read: %v", lockFile, err))
	}
	if lock.Plugins[old] == nil {
		var wrote int
		if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_op_attempts WHERE op_id = ? AND call = 'write:lock-old' AND proof LIKE 'applied%'`, op.ID).Scan(&wrote); err != nil {
			return err
		}
		if wrote > 0 {
			return done("removed", "")
		}
		return done("absent", "")
	}
	_, list, err := readManaged(h.cfg.managedPath)
	if err != nil {
		return done("kept", fmt.Sprintf("the managed file %s does not read: %v", h.cfg.managedPath, err))
	}
	cfg, err := readPluginCmdConfig(h.cfg.path)
	if err != nil {
		return done("kept", fmt.Sprintf("the config file %s does not read: %v", h.cfg.path, err))
	}
	intended, err := op.intended()
	if err != nil {
		return done("kept", "the operation's entry does not read: "+err.Error())
	}
	if cur := canonicalEntry(managedByID(list, op.managedID())); cur != intended {
		return done("kept", fmt.Sprintf("the managed file does not have the operation's %s of %s: it has %s", op.Kind, op.Entry, cur))
	}
	h.entriesMu.Lock()
	loaded := append([]*configEntry{}, h.entries...)
	h.entriesMu.Unlock()
	for _, group := range [][]*configEntry{cfg.config, list, loaded} {
		for _, e := range group {
			if e.Source == old {
				return done("kept", opEntryKey(e)+" names it")
			}
		}
	}
	var snap map[string]string
	base, ok := "", json.Unmarshal([]byte(op.LockBase), &snap) == nil
	if ok {
		base, ok = snap[old]
	}
	if !ok {
		return done("kept", "the admission snapshot has no entry for it")
	}
	if now := lockBase(lock, old); now != base {
		return done("kept", fmt.Sprintf("it changed after the operation was admitted (%s -> %s)", base, now))
	}
	nl := &pluginLock{Plugins: map[string]*lockEntry{}}
	for k, v := range lock.Plugins {
		if k != old {
			nl.Plugins[k] = v
		}
	}
	nb, err := encodeLock(nl)
	if err != nil {
		return err
	}
	if err := h.renameAttempt(ctx, op, "write:lock-old", lockFile, nb, "lock-old-rename"); err != nil {
		return err
	}
	return done("removed", "")
}
