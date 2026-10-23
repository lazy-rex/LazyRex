package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// writingAdd admits an add of src with id, and runs it to the writing phase.
func writingAdd(t *testing.T, h *Hub, key, src, id string) *pluginOp {
	t.Helper()
	op, err := resolveOp(t, h, key, opRequest{Op: "add", Input: rawJSON(map[string]any{"source": src, "id": id})})
	if err != nil || op.Phase != "fetching" {
		t.Fatalf("resolve: %+v %v", op, err)
	}
	ctx := context.Background()
	if err := h.opFetch(ctx, op); err != nil || op.Phase != "locking" {
		t.Fatalf("fetch: %+v %v", op, err)
	}
	if err := h.opLock(ctx, op); err != nil || op.Phase != "writing" {
		t.Fatalf("lock: %+v %v", op, err)
	}
	return op
}

// rowCols is the plugins row of name, or nil.
func rowCols(t *testing.T, h *Hub, name string) *pluginRowCols {
	t.Helper()
	c, err := readRowCols(context.Background(), h.db, name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// rowHub is a hub whose remote has a plugin with the manifest name notes at tag v1.0.0.
func rowHub(t *testing.T) *Hub {
	t.Helper()
	r, _ := fetchTestRepo(t)
	return fetchTestHub(t, r)
}

const toolsSource = "github:acme/tools@v1.0.0"

var toolsIdentity = `["managed:tools","github:acme/tools@v1.0.0"]`

// The row is named by the fetched manifest name (addendum 1, 1a): add id tools, manifest name notes.
// The row notes is desired stopped with the entry's identity and a fresh stamp, and no row tools
// exists. The start path of the loaded entry then starts nothing: only the bound start of the
// starting phase starts an add.
func TestPluginOpAddRowNamedByManifest(t *testing.T) {
	h := rowHub(t)
	ctx := context.Background()
	op := writingAdd(t, h, "row-1", toolsSource, "tools")
	if err := h.opWrite(ctx, op); err != nil || op.Phase != "reloading" {
		t.Fatalf("the write: %+v %v", op, err)
	}
	if c := rowCols(t, h, "notes"); c == nil || c.desired != "stopped" || c.identity != toolsIdentity || c.instance != "" {
		t.Fatalf("the row notes: %+v", c)
	}
	if c := rowCols(t, h, "tools"); c != nil {
		t.Fatalf("a row tools: %+v", c)
	}
	want, _ := json.Marshal(opRowPrior{Name: "notes", Inserted: true, Wrote: opRowStamp{toolsIdentity, entryStamp("stopped", "", "", "", 0, "", "")}})
	if stored := opsOf(t, h, op.ID).RowPrior; stored != string(want) {
		t.Fatalf("row_prior %s, want %s", stored, want)
	}
	_, list, _ := readManaged(h.cfg.managedPath)
	e := managedByID(list, "tools")
	e.name = "notes"
	pl := &Plugin{Name: "notes", entry: e}
	h.mu.Lock()
	h.plugins["notes"] = pl
	h.mu.Unlock()
	pl.lifecycle.Lock()
	err := h.startPlugin(ctx, pl)
	pl.lifecycle.Unlock()
	var intents int
	h.db.QueryRow(`SELECT COUNT(*) FROM spawn_intents`).Scan(&intents)
	if err != nil || pl.Desired != "stopped" || intents != 0 {
		t.Fatalf("the start path: %v, desired %s, %d intents", err, pl.Desired, intents)
	}
}

// The guard (lead-b8-step4-final, C4 plan NEW 1): a row with an instance bound or running, a
// desired running row, a row with a run or a selected run, a prepared or sending intent, and an
// unsettled attempt of an op on the name are conflict; nothing more is written. An open bound
// handler (bound_at set, outcome empty) is conflict even when proof settles its attempt. An
// unproven attempt (its later observation ended) is settled, but an unresolved spawn intent beside
// it is still conflict. Resolved intents and an unproven attempt with no open handler pass.
func TestPluginOpAddRowGuard(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup []string
		ok    bool
	}{
		{"a live block", []string{`INSERT INTO plugins (name, desired, instance_id, block_id) VALUES ('notes', 'stopped', 'notes-1', 'b1')`}, false},
		{"a child", []string{`INSERT INTO plugins (name, desired, child_pid) VALUES ('notes', 'stopped', 7)`}, false},
		{"desired running", []string{`INSERT INTO plugins (name, desired) VALUES ('notes', 'running')`}, false},
		{"a run", []string{`INSERT INTO plugins (name, desired, run) VALUES ('notes', 'stopped', '{"name":"notes"}')`}, false},
		{"a selected run", []string{`INSERT INTO plugins (name, desired, selected_run) VALUES ('notes', 'stopped', '{"entry":"x"}')`}, false},
		{"a session", []string{`INSERT INTO plugins (name, desired, session_id) VALUES ('notes', 'stopped', 's1')`}, false},
		{"a prepared intent", []string{`INSERT INTO spawn_intents (instance_id, plugin, status) VALUES ('notes-2', 'notes', 'prepared')`}, false},
		{"a sending intent", []string{`INSERT INTO spawn_intents (instance_id, plugin, status) VALUES ('notes-2', 'notes', 'sending')`}, false},
		{"an unsettled attempt", []string{
			`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated) VALUES ('o1', 's', 'k', 'f', '{}', 'start', 'managed:x', 'notes', '', '', '', 'unknown', 1, 1)`,
			`INSERT INTO plugin_op_attempts (attempt, op_id, call, at) VALUES ('a1', 'o1', 'start', 1)`}, false},
		{"resolved intents", []string{
			`INSERT INTO spawn_intents (instance_id, plugin, status) VALUES ('notes-2', 'notes', 'created')`,
			`INSERT INTO spawn_intents (instance_id, plugin, status) VALUES ('notes-3', 'notes', 'abandoned')`}, true},
		{"an unproven attempt", []string{
			`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated) VALUES ('o1', 's', 'k', 'f', '{}', 'start', 'managed:x', 'notes', '', '', '', 'unknown', 1, 1)`,
			`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, outcome) VALUES ('a1', 'o1', 'start', 1, 'unproven: observation ended after 10 passes; error: lost')`}, true},
		{"an unproven attempt and a recorded block (a late effect)", []string{
			`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated) VALUES ('o1', 's', 'k', 'f', '{}', 'start', 'managed:x', 'notes', '', '', '', 'unknown', 1, 1)`,
			`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, outcome) VALUES ('a1', 'o1', 'start', 1, 'unproven: observation ended after 10 passes; error: lost')`,
			`INSERT INTO plugins (name, desired, instance_id, block_id) VALUES ('notes', 'stopped', 'notes-1', 'b1')`}, false},
		{"an unproven attempt and a prepared intent", []string{
			`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated) VALUES ('o1', 's', 'k', 'f', '{}', 'start', 'managed:x', 'notes', '', '', '', 'unknown', 1, 1)`,
			`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, outcome) VALUES ('a1', 'o1', 'start', 1, 'unproven: observation ended after 10 passes; error: lost')`,
			`INSERT INTO spawn_intents (instance_id, plugin, status) VALUES ('notes-2', 'notes', 'prepared')`}, false},
		{"an observed attempt with an unknown outcome", []string{
			`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated) VALUES ('o1', 's', 'k', 'f', '{}', 'start', 'managed:x', 'notes', '', '', '', 'unknown', 1, 1)`,
			`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, bound_at, outcome) VALUES ('a1', 'o1', 'start', 1, 't', 'error: lost')`}, false},
		{"an open bound handler, proved", []string{
			`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated) VALUES ('o1', 's', 'k', 'f', '{}', 'start', 'managed:x', 'notes', '', '', '', 'unknown', 1, 1)`,
			`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, bound_at, proof) VALUES ('a1', 'o1', 'start', 1, 't', 'not_applied')`}, false},
		{"a bound handler with an unproven outcome", []string{
			`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated) VALUES ('o1', 's', 'k', 'f', '{}', 'start', 'managed:x', 'notes', '', '', '', 'unknown', 1, 1)`,
			`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, bound_at, outcome) VALUES ('a1', 'o1', 'start', 1, 't', 'unproven: observation ended after 10 passes; error: lost')`}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := rowHub(t)
			op := writingAdd(t, h, "guard", toolsSource, "tools")
			for _, s := range c.setup {
				if _, err := h.db.Exec(s); err != nil {
					t.Fatal(err)
				}
			}
			before := rowCols(t, h, "notes")
			if err := h.opWrite(context.Background(), op); err != nil {
				t.Fatal(err)
			}
			if c.ok {
				if op.Phase != "reloading" {
					t.Fatalf("%+v, want reloading", op)
				}
				return
			}
			if op.Phase != "conflict" || op.Error != "a plugin named notes has a record or a start in flight; nothing more was written" {
				t.Fatalf("%+v, want the guard's conflict", op)
			}
			after := rowCols(t, h, "notes")
			if managedText(t, h) != "" || opsOf(t, h, op.ID).RowPrior != "" || (before == nil) != (after == nil) || before != nil && *before != *after {
				t.Fatalf("a write after the guard: file %q, row %+v -> %+v", managedText(t, h), before, after)
			}
		})
	}
}

// A reused row (addendum 3, a remove and then an add): the row of an earlier entry, stopped, with
// its retired instance. The add writes desired stopped, the new identity and a fresh stamp; the
// instance_id stays, and row_prior holds the changed values as they were.
func TestPluginOpAddRowReuse(t *testing.T) {
	h := rowHub(t)
	if _, err := h.db.Exec(`INSERT INTO plugins (name, desired, instance_id, entry_identity) VALUES ('notes', 'stopped', 'notes-1-x', '["managed:a","github:acme/a@v1"]')`); err != nil {
		t.Fatal(err)
	}
	op := writingAdd(t, h, "reuse", toolsSource, "tools")
	if err := h.opWrite(context.Background(), op); err != nil || op.Phase != "reloading" {
		t.Fatalf("%+v %v", op, err)
	}
	stamp := entryStamp("stopped", "notes-1-x", "", "", 0, "", "")
	if c := rowCols(t, h, "notes"); c.instance != "notes-1-x" || c.identity != toolsIdentity || c.desired != "stopped" {
		t.Fatalf("the reused row: %+v", c)
	}
	want, _ := json.Marshal(opRowPrior{Name: "notes", Wrote: opRowStamp{toolsIdentity, stamp},
		Prior: &opRowPrev{Desired: "stopped", EntryIdentity: `["managed:a","github:acme/a@v1"]`}})
	if stored := opsOf(t, h, op.ID).RowPrior; stored != string(want) {
		t.Fatalf("row_prior %s, want %s", stored, want)
	}
}

// crashedAfterRow runs an add's writing phase to the fault point after the row commit: the row is
// written and recorded, the managed file is not.
func crashedAfterRow(t *testing.T, h *Hub, key string) *pluginOp {
	t.Helper()
	op := writingAdd(t, h, key, toolsSource, "tools")
	h.pops.fault = func(p string) bool { return p == "plugin-op-after-row" }
	err := h.opWrite(context.Background(), op)
	h.pops.fault = nil
	if !errors.Is(err, errOpCrash) || managedText(t, h) != "" || rowCols(t, h, "notes") == nil {
		t.Fatalf("the crash after the row: %v, file %q", err, managedText(t, h))
	}
	return opsOf(t, h, op.ID)
}

// A crash after the row commit (addendum 1, 1b): the replay finds the row that the add still owns,
// does not write it again, and goes on to the managed rename.
func TestPluginOpAddRowCrashReplay(t *testing.T) {
	h := rowHub(t)
	op := crashedAfterRow(t, h, "crash")
	before, prior := *rowCols(t, h, "notes"), op.RowPrior
	if prior == "" {
		t.Fatal("no row record after the commit")
	}
	if err := h.opWrite(context.Background(), op); err != nil || op.Phase != "reloading" {
		t.Fatalf("the replay: %+v %v", op, err)
	}
	if after := rowCols(t, h, "notes"); *after != before || opsOf(t, h, op.ID).RowPrior != prior || managedSource(t, h, "tools") != toolsSource {
		t.Fatalf("the replay: row %+v -> %+v, file %s", before, after, managedText(t, h))
	}
}

// A crash after the managed rename (lead-c4e 3; codex-27's probe
// TestReviewerWrittenAddChecksRowOwner): the replay finds the file written, and it goes on only
// while the add owns its row. An older hub that changes instance_id and keeps the stored stamp
// makes the row not owned: conflict, no reload. The entry is in the file, so the row stays as it is.
func TestPluginOpAddRowWrittenReplay(t *testing.T) {
	ctx := context.Background()
	renamed := func(t *testing.T, h *Hub, key string) *pluginOp {
		t.Helper()
		op := writingAdd(t, h, key, toolsSource, "tools")
		h.pops.fault = func(p string) bool { return p == "plugin-op-after-rename" }
		err := h.opWrite(ctx, op)
		h.pops.fault = nil
		if !errors.Is(err, errOpCrash) || managedSource(t, h, "tools") != toolsSource {
			t.Fatalf("the crash after the rename: %v, file %s", err, managedText(t, h))
		}
		return opsOf(t, h, op.ID)
	}
	t.Run("an owned row", func(t *testing.T) {
		h := rowHub(t)
		op := renamed(t, h, "renamed-owned")
		before := *rowCols(t, h, "notes")
		if err := h.opWrite(ctx, op); err != nil || op.Phase != "reloading" {
			t.Fatalf("the replay: %+v %v", op, err)
		}
		if after := rowCols(t, h, "notes"); *after != before {
			t.Fatalf("the replay wrote the row: %+v -> %+v", before, after)
		}
	})
	t.Run("a row that the add does not own", func(t *testing.T) {
		h := rowHub(t)
		op := renamed(t, h, "renamed-changed")
		if _, err := h.db.Exec(`UPDATE plugins SET instance_id = 'another-instance' WHERE name = 'notes'`); err != nil {
			t.Fatal(err)
		}
		before := *rowCols(t, h, "notes")
		if err := h.opWrite(ctx, op); err != nil {
			t.Fatal(err)
		}
		if op.Phase != "conflict" || op.Error != "the record of notes is not the one that the add wrote; nothing more was written" {
			t.Fatalf("%+v, want the owner conflict", op)
		}
		if after := rowCols(t, h, "notes"); after == nil || *after != before || managedSource(t, h, "tools") != toolsSource {
			t.Fatalf("the end changed the row or the file: %+v -> %+v, file %s", before, after, managedText(t, h))
		}
	})
}

// The cleanup (addendum 1, 1d; codex-27's correction B): an add that ends without its managed entry
// after its row write deletes an inserted row, and writes back a reused row's prior values, only
// while it owns the row. Here a CLI writer changes the key's pin after the crash, so the replay
// ends conflict at the pin check.
func TestPluginOpAddRowCleanup(t *testing.T) {
	ctx := context.Background()
	changePin := func(t *testing.T, h *Hub) {
		l := readTestLock(t, h)
		l.Plugins[toolsSource] = otherLockEntry()
		writeTestLock(t, h, l)
	}
	t.Run("an inserted row is deleted", func(t *testing.T) {
		h := rowHub(t)
		op := crashedAfterRow(t, h, "clean-insert")
		changePin(t, h)
		if err := h.opWrite(ctx, op); err != nil {
			t.Fatal(err)
		}
		if op.Phase != "conflict" || !strings.Contains(op.Error, "the lock changed since the add wrote its pin") || rowCols(t, h, "notes") != nil {
			t.Fatalf("%+v, row %+v", op, rowCols(t, h, "notes"))
		}
		if stored := opsOf(t, h, op.ID); stored.Phase != "conflict" || stored.Error != op.Error {
			t.Fatalf("the stored end: %+v", stored)
		}
	})
	t.Run("a reused row gets its prior values", func(t *testing.T) {
		h := rowHub(t)
		if _, err := h.db.Exec(`INSERT INTO plugins (name, desired, instance_id, entry_identity) VALUES ('notes', 'stopped', 'notes-1-x', '["managed:a","github:acme/a@v1"]')`); err != nil {
			t.Fatal(err)
		}
		before := *rowCols(t, h, "notes")
		op := crashedAfterRow(t, h, "clean-reuse")
		changePin(t, h)
		if err := h.opWrite(ctx, op); err != nil || op.Phase != "conflict" {
			t.Fatalf("%+v %v", op, err)
		}
		if after := rowCols(t, h, "notes"); after == nil || *after != before {
			t.Fatalf("the restored row %+v, want %+v", after, before)
		}
	})
	// A save of this hub under the same entry identity changes the row after the add's write (a new
	// instance, then a stop). The add does not own the row any more: the cleanup leaves it as it is,
	// and the end says so.
	for _, w := range []struct {
		name, sql string
	}{
		{"a save under the same identity", `UPDATE plugins SET instance_id = 'notes-3-z' WHERE name = 'notes'`},
	} {
		t.Run(w.name+", then the pin changes", func(t *testing.T) {
			h := rowHub(t)
			op := crashedAfterRow(t, h, "clean-other")
			if _, err := h.db.Exec(w.sql); err != nil {
				t.Fatal(err)
			}
			changed := *rowCols(t, h, "notes")
			changePin(t, h)
			if err := h.opWrite(ctx, op); err != nil {
				t.Fatal(err)
			}
			if op.Phase != "conflict" || !strings.HasSuffix(op.Error, "; the record of notes changed after the add wrote it, and the add left it as it is") {
				t.Fatalf("%+v", op)
			}
			if after := rowCols(t, h, "notes"); after == nil || *after != changed {
				t.Fatalf("the row %+v, want it as the other writer left it %+v", after, changed)
			}
		})
		t.Run(w.name+", then the replay", func(t *testing.T) {
			h := rowHub(t)
			op := crashedAfterRow(t, h, "replay-other")
			if _, err := h.db.Exec(w.sql); err != nil {
				t.Fatal(err)
			}
			changed := *rowCols(t, h, "notes")
			if err := h.opWrite(ctx, op); err != nil {
				t.Fatal(err)
			}
			if op.Phase != "conflict" || !strings.HasPrefix(op.Error, "the record of notes is not the one that the add wrote; nothing more was written") ||
				!strings.HasSuffix(op.Error, "and the add left it as it is") || managedText(t, h) != "" {
				t.Fatalf("%+v, file %q", op, managedText(t, h))
			}
			if after := rowCols(t, h, "notes"); after == nil || *after != changed {
				t.Fatalf("the row %+v, want %+v", after, changed)
			}
		})
	}
}

// observePasses runs more than opObservePasses passes of the later observation, each one after
// the poll interval.
func observePasses(h *Hub) {
	for i := 0; i <= opObservePasses; i++ {
		for k, p := range h.pops.later {
			p.last = time.Time{}
			h.pops.later[k] = p
		}
		h.observeLater(context.Background())
	}
}

// Every unproved attempt of an ended op reaches a terminal outcome without an operator (lead-a1h):
// for each call of opAttemptCalls, an attempt with no proof of an op on notes that ended, then more
// than opObservePasses passes: the attempt ends unproven, and an add of notes is admitted. A
// reload or a file write has no sent Rex call to observe and is still counted. An open bound
// attempt (its handler can still act) and an attempt of an op that has not ended stay, and keep
// the add out.
func TestPluginOpEveryAttemptEndsUnproven(t *testing.T) {
	type tc struct {
		name, call, phase, bound, outcome string
		ends                              bool
	}
	var cases []tc
	for _, call := range opAttemptCalls {
		cases = append(cases, tc{call, call, "unknown", "t", "error: lost", true})
	}
	cases = append(cases,
		tc{"write:managed of a failed op", "write:managed", "failed", "t", "unknown: the file is not the intended output", true},
		tc{"an unbound start with no outcome of an ended op", "start", "unknown", "", "", true},
		tc{"an open bound start", "start", "unknown", "t", "", false},
		tc{"a reload of an op that has not ended", "reload", "reloading", "", "error: lost", false})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := rowHub(t)
			ctx := context.Background()
			next := writingAdd(t, h, "next", toolsSource, "tools")
			sent := ""
			if slices.Contains(opEvidenceCalls, c.call) {
				sent = "t"
			}
			for _, q := range [][]any{
				{`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated) VALUES ('old', 's', 'old', 'f', '{}', 'remove', 'managed:old', 'notes', '', '', '', ?, 1, 1)`, c.phase},
				{`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, bound_at, sent_at, outcome) VALUES ('a-old', 'old', ?, 1, ?, ?, ?)`, c.call, c.bound, sent, c.outcome},
			} {
				if _, err := h.db.Exec(q[0].(string), q[1:]...); err != nil {
					t.Fatal(err)
				}
			}
			observePasses(h)
			var outcome, proof string
			if err := h.db.QueryRow(`SELECT outcome, proof FROM plugin_op_attempts WHERE attempt = 'a-old'`).Scan(&outcome, &proof); err != nil {
				t.Fatal(err)
			}
			if ended := outcome == unprovenPrefix+c.outcome && proof == ""; ended != c.ends {
				t.Fatalf("outcome %q, proof %q; want ended %v", outcome, proof, c.ends)
			}
			if err := h.opWrite(ctx, next); err != nil {
				t.Fatal(err)
			}
			if c.ends && next.Phase != "reloading" || !c.ends && next.Phase != "conflict" {
				t.Fatalf("the add: phase %s, error %s", next.Phase, next.Error)
			}
		})
	}
}

// codex-26's reproducer: a reload of a remove on notes returns an error that is not "invalid", so
// its attempt stays unproved and the op ends unknown. After more than opObservePasses passes the
// attempt is unproven, and an add of notes is admitted.
func TestPluginOpUnknownReloadFreesName(t *testing.T) {
	h := rowHub(t)
	ctx := context.Background()
	next := writingAdd(t, h, "next", toolsSource, "tools")
	if _, err := h.db.Exec(`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated)
		VALUES ('old', 's', 'old', 'f', '{}', 'remove', 'managed:old', 'notes', '', '', '', 'reloading', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	old, err := h.loadOp(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	reload := opReload
	t.Cleanup(func() { opReload = reload })
	opReload = func(context.Context, *Hub) (any, error) { return nil, errors.New("reload response lost") }
	if err := h.opCall(ctx, old, "reload"); err != nil || old.Phase != "unknown" {
		t.Fatalf("reload: phase %s, error %v", old.Phase, err)
	}
	observePasses(h)
	if err := h.opWrite(ctx, next); err != nil {
		t.Fatal(err)
	}
	if next.Phase != "reloading" {
		t.Fatalf("the add: phase %s, error %s", next.Phase, next.Error)
	}
}

// The manager schedules the observation of an attempt that its own pass leaves unproved (codex-26):
// opsRun runs a remove whose reload returns an error that is not "invalid", the op ends unknown,
// and opsRun returns opObservePoll, so opsLoop passes again on an idle hub. A pass with nothing to
// observe returns 0.
func TestPluginOpsRunSchedulesObservation(t *testing.T) {
	h := rowHub(t)
	ctx := context.Background()
	if wait := h.opsRun(ctx); wait != 0 {
		t.Fatalf("an idle pass waits %v", wait)
	}
	if _, err := h.db.Exec(`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated)
		VALUES ('old', 's', 'old', 'f', '{}', 'remove', 'managed:old', 'notes', '', '', '', 'reloading', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	reload := opReload
	t.Cleanup(func() { opReload = reload })
	opReload = func(context.Context, *Hub) (any, error) { return nil, errors.New("reload response lost") }
	wait := h.opsRun(ctx)
	if op := opsOf(t, h, "old"); op.Phase != "unknown" {
		t.Fatalf("phase %s", op.Phase)
	}
	if wait != opObservePoll {
		t.Fatalf("opsRun waits %v after the reload left an unproved attempt; want %v", wait, opObservePoll)
	}
}
