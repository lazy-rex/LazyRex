package main

// Entry identity regressions of the plugin manager (codex-21's review of 4629ece, reproduced by
// codex-27 at c7eefac and 3c4a5f0). An op is bound to the entry that it was admitted for, not to
// the plugin name: a later entry that binds the same name, or the same source, is another entry.
// The fake-boundary tests replace opAdmin (opsTestHub); the last tests call the real admin
// handler (adminAction) with expect_entry.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// opsEntry returns the hub's entry with the key (managed:<id> or a config source).
func opsEntry(t *testing.T, h *Hub, key string) *configEntry {
	t.Helper()
	h.entriesMu.Lock()
	defer h.entriesMu.Unlock()
	for _, e := range h.entries {
		if opEntryKey(e) == key {
			return e
		}
	}
	t.Fatalf("no entry %s", key)
	return nil
}

// opsReplace makes the managed file hold only the entry id at source src, with the manifest name
// name, and binds the record of that name to it: the old entry is gone, and a new entry owns the
// name.
func opsReplace(t *testing.T, h *Hub, dir, id, src, name string) *configEntry {
	t.Helper()
	writeManaged(t, dir, `{"plugins":[{"id":"`+id+`","source":"`+src+`"}]}`)
	opsLoadEntries(t, h)
	e := opsEntry(t, h, "managed:"+id)
	e.name = name
	h.mu.Lock()
	if pl := h.plugins[name]; pl != nil {
		pl.entry = e
	} else {
		h.plugins[name] = &Plugin{Name: name, entry: e}
	}
	h.mu.Unlock()
	return e
}

// expectEntryArg is the expect_entry value for the entry key at the source.
func expectEntryArg(key, source string) string {
	b, _ := json.Marshal([]string{key, source})
	return string(b)
}

// A queued stop of managed:x does not stop a replacement entry that binds the name x later, from
// another source (codex-21's case, with a record that was bound to managed:x at admission).
func TestPluginOpQueuedStopAfterReplacement(t *testing.T) {
	h, c, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	h.plugins["x"] = &Plugin{Name: "x", entry: opsEntry(t, h, "managed:x")}
	id := opsMust(t, h, "stop-old", opRequest{Op: "stop", Entry: "managed:x"})
	opsReplace(t, h, dir, "replacement", filepath.Join(t.TempDir(), "another-source"), "x")
	h.opsRun(context.Background())
	op := opsOf(t, h, id)
	if len(c.list()) != 0 || op.Phase != "conflict" {
		t.Fatalf("the stop of managed:x acted on the replacement: calls %v; op %+v", c.list(), op)
	}
}

// A queued start of managed:x does not start a replacement entry with the same source and the same
// name. Its expect_run is the same run, so only the entry identity tells the entries apart.
func TestPluginOpStartAfterSameSourceReplacement(t *testing.T) {
	h, c, dir := opsTestHub(t, "")
	pd := t.TempDir()
	writeManaged(t, dir, opsLocal(t, pd, "x"))
	opsLoadEntries(t, h)
	h.plugins["x"] = &Plugin{Name: "x", entry: opsEntry(t, h, "managed:x")}
	id := opsMust(t, h, "start-old", opRequest{Op: "start", Entry: "managed:x"})
	opsReplace(t, h, dir, "y", filepath.Join(pd, "x"), "x")
	h.opsRun(context.Background())
	op := opsOf(t, h, id)
	if len(c.list()) != 0 || op.Phase != "conflict" {
		t.Fatalf("the start of managed:x acted on managed:y: calls %v; op %+v", c.list(), op)
	}
}

// The stop phase of a remove of managed:x does not stop a replacement that binds the name x. The
// replacement comes after the remove passed its check (the stored phase is stopping), so the
// check of the file base cannot see it.
func TestPluginOpRemoveAfterReplacement(t *testing.T) {
	h, c, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	h.plugins["x"] = &Plugin{Name: "x", entry: opsEntry(t, h, "managed:x")}
	id := opsMust(t, h, "remove-old", opRequest{Op: "remove", Entry: "managed:x"})
	if _, err := h.db.Exec(`UPDATE plugin_ops SET phase = 'stopping' WHERE op_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	opsReplace(t, h, dir, "replacement", filepath.Join(t.TempDir(), "another-source"), "x")
	h.opsRun(context.Background())
	op := opsOf(t, h, id)
	for _, call := range c.list() {
		if strings.HasPrefix(call, "hub.admin.stop") {
			t.Fatalf("the remove of managed:x stopped the replacement: calls %v; op %+v", c.list(), op)
		}
	}
	if op.Phase != "conflict" {
		t.Fatalf("op %+v", op)
	}
}

// After a crash during a stop of managed:x, the observation does not take a stopped record of
// another entry as the stop's outcome. A row from before the identity was recorded is unknown too,
// never a match.
func TestPluginOpStopObservationAfterReplacement(t *testing.T) {
	for _, c := range []struct {
		name string
		row  func(t *testing.T, h *Hub, dir string)
	}{
		{"a replacement's stopped record", func(t *testing.T, h *Hub, dir string) {
			e := opsReplace(t, h, dir, "replacement", filepath.Join(t.TempDir(), "another-source"), "x")
			pl := &Plugin{Name: "x", entry: e}
			if err := h.loadPluginRow(context.Background(), pl); err != nil {
				t.Fatal(err)
			}
			pl.Desired = "stopped"
			if err := h.savePlugin(context.Background(), pl); err != nil {
				t.Fatal(err)
			}
		}},
		{"a row without an identity", func(t *testing.T, h *Hub, dir string) {
			if _, err := h.db.Exec(`INSERT INTO plugins (name, desired) VALUES ('x', 'stopped')`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, dir := opsTestHub(t, "")
			writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
			opsLoadEntries(t, h)
			h.plugins["x"] = &Plugin{Name: "x", entry: opsEntry(t, h, "managed:x")}
			id := opsMust(t, h, "stop-old", opRequest{Op: "stop", Entry: "managed:x"})
			h.pops.fault = func(p string) bool { return p == "plugin-op-after-stop" }
			h.opsRun(context.Background())
			h.pops.fault = nil
			c.row(t, h, dir)
			h.opsRun(context.Background())
			if op := opsOf(t, h, id); op.Phase != "unknown" {
				t.Fatalf("the observation matched another record: %+v", op)
			}
		})
	}
}

// adminEntryHub is a hub with the record x bound to managed:x, for the real admin handler.
func adminEntryHub(t *testing.T) (*Hub, *Plugin, string) {
	t.Helper()
	h, _, dir := opsTestHub(t, "")
	src := filepath.Join(t.TempDir(), "x")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManaged(t, dir, `{"plugins":[{"id":"x","source":"`+src+`"}]}`)
	opsLoadEntries(t, h)
	pl := &Plugin{Name: "x", entry: opsEntry(t, h, "managed:x")}
	if err := h.loadPluginRow(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	h.plugins["x"] = pl
	return h, pl, src
}

// hub.admin.stop with expect_entry checks the entry after it takes the locks that a rebind needs,
// so the check and the stop are one step. Here a rebind (managed:x removed, managed:y with the same
// source and name adopts the record) waits on reloadMu, as a reload does, while the stop is sent;
// then it completes, and the stop must see managed:y. A check before the lock would see managed:x
// and stop managed:y's instance.
func TestAdminStopExpectEntryRebind(t *testing.T) {
	h, pl, src := adminEntryHub(t)
	h.reloadMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := h.adminAction(context.Background(), "hub.admin.stop", map[string]any{"plugin": "x", "expect_entry": expectEntryArg("managed:x", src)})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // the stop reaches its lock
	y := &configEntry{managed: "y", Source: src, name: "x"}
	h.entriesMu.Lock()
	h.mu.Lock()
	h.entries = []*configEntry{y}
	pl.entry = y
	h.mu.Unlock()
	h.entriesMu.Unlock()
	h.reloadMu.Unlock()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop did not end")
	}
	if !isRefusal(err, "entry_changed") {
		t.Fatalf("the stop after the rebind: %v", err)
	}
	var desired string
	h.db.QueryRow(`SELECT desired FROM plugins WHERE name = 'x'`).Scan(&desired)
	if pl.Desired != "running" || desired != "running" {
		t.Fatalf("the record was stopped: %q, row %q", pl.Desired, desired)
	}
}

// expect_entry: a match stops; a value that is not a non-empty string is invalid_input; a start
// whose entry changed is refused with entry_changed before the start check (the source has no
// manifest, so a start that skips the entry check is refused later, for the manifest).
func TestAdminExpectEntryArgument(t *testing.T) {
	for _, v := range []any{7.0, "", []any{"managed:x"}, map[string]any{}} {
		h, pl, _ := adminEntryHub(t)
		_, err := h.adminAction(context.Background(), "hub.admin.stop", map[string]any{"plugin": "x", "expect_entry": v})
		if !isRefusal(err, "invalid_input") || pl.Desired != "running" {
			t.Errorf("expect_entry %#v: %v (desired %q)", v, err, pl.Desired)
		}
	}

	h, pl, src := adminEntryHub(t)
	_, err := h.adminAction(context.Background(), "hub.admin.start", map[string]any{"plugin": "x", "expect_entry": expectEntryArg("managed:other", src)})
	if !isRefusal(err, "entry_changed") {
		t.Errorf("a start with another entry: %v", err)
	}

	h, pl, src = adminEntryHub(t)
	if _, err := h.adminAction(context.Background(), "hub.admin.stop", map[string]any{"plugin": "x", "expect_entry": expectEntryArg("managed:x", src)}); err != nil || pl.Desired != "stopped" {
		t.Errorf("a matching stop: %v (desired %q)", err, pl.Desired)
	}
}

// Admission reads its entry's fields while a reload changes the same entry in place
// (updateManaged, under reloadMu, entriesMu and mu): every field that admission uses is read under
// entriesMu, so the stored identity is one snapshot (codex-27's TestReviewerEntryAdmissionReloadRace,
// a -race check).
func TestPluginOpAdmissionReloadRace(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	e := opsEntry(t, h, "managed:x")
	a, b := *e, *e
	b.Source = a.Source + "-changed"
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			h.reloadMu.Lock()
			h.entriesMu.Lock()
			h.mu.Lock()
			if i%2 == 0 {
				updateManaged(e, &a)
			} else {
				updateManaged(e, &b)
			}
			h.mu.Unlock()
			h.entriesMu.Unlock()
			h.reloadMu.Unlock()
		}
	}()
	for i := 0; i < 2000; i++ {
		op, err := h.opAdmission(opOrigin{Kind: "user", ID: "reviewer"}, opRequest{Op: "stop", Entry: "managed:x"})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{expectEntryArg("managed:x", a.Source), expectEntryArg("managed:x", b.Source)}; op.EntryIdentity != want[0] && op.EntryIdentity != want[1] {
			t.Fatalf("identity %s is not one snapshot", op.EntryIdentity)
		}
	}
	<-done
}

// A rollback hub (OLD) does not write entry_identity. If OLD binds the name x to another entry,
// starts it and stops it, the row keeps managed:x's identity. A stop of managed:x that a crash
// interrupted before the rollback must not take that stopped row as its outcome: the stop attempt
// names the instance it was for, and OLD's instance is another one (codex-26).
func TestPluginOpStopObservationAfterOldHub(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	pl := &Plugin{Name: "x", entry: opsEntry(t, h, "managed:x")}
	if err := h.loadPluginRow(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	pl.InstanceID, pl.BlockID = "x-1-aaaaaaaa", "block:x1"
	if err := h.savePlugin(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	h.plugins["x"] = pl
	id := opsMust(t, h, "stop-old", opRequest{Op: "stop", Entry: "managed:x"})
	h.pops.fault = func(p string) bool { return p == "plugin-op-after-stop" }
	h.opsRun(context.Background())
	h.pops.fault = nil
	// OLD's savePlugin, which names its columns and has no entry_identity: another instance ran
	// and was stopped.
	if _, err := h.db.Exec(`UPDATE plugins SET desired = 'stopped', instance_id = 'x-2-bbbbbbbb', block_id = '' WHERE name = 'x'`); err != nil {
		t.Fatal(err)
	}
	h.opsRun(context.Background())
	if op := opsOf(t, h, id); op.Phase != "unknown" {
		t.Fatalf("the observation took OLD's stop as the op's: %+v", op)
	}
}
