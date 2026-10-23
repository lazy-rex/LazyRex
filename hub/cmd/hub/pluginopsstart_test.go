package main

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The bound start of an add (C5 plan r1 items 1 and 2): after the row write, the op has the
// manifest name; the start goes to that plugin with expect_entry and an expect_run of the pin
// (source, commit and digest), and the attempt row stores the same expect_run. The handler's
// argument check (parseExpectRun) accepts it.
func TestPluginOpAddStart(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h, c := fetchTestHubCalls(t, r)
	ctx := context.Background()
	reload := opReload
	opReload = func(ctx context.Context, h *Hub) (any, error) {
		res, err := reload(ctx, h)
		// The reload gives the record of the new entry, as the hub's reload does.
		e := opsEntry(t, h, "managed:notes")
		h.mu.Lock()
		h.plugins["notes"] = &Plugin{Name: "notes", entry: e}
		h.mu.Unlock()
		return res, err
	}
	const src = "github:acme/notes@v1.0.0"
	op := writingAdd(t, h, "add-start", src, "notes")
	if err := h.runOp(ctx, op); err != nil || op.Phase != "done" {
		t.Fatalf("the add: %+v %v; calls %v", op, err, c.list())
	}
	if stored := opsOf(t, h, op.ID); stored.Name != "notes" || op.Name != "notes" {
		t.Fatalf("the add's name: stored %q, op %q", stored.Name, op.Name)
	}
	var pin lockEntry
	json.Unmarshal([]byte(op.Pin), &pin)
	run := map[string]any{"source": src, "commit": pin.Commit, "digest": pin.Digest}
	want := map[string]any{"plugin": "notes", "expect_entry": `["managed:notes","` + src + `"]`, "expect_run": run}
	var got map[string]any
	for _, call := range c.list() {
		if s, ok := strings.CutPrefix(call, "hub.admin.start "); ok {
			json.Unmarshal([]byte(s), &got)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the start: %v, want %v", got, want)
	}
	if _, err := parseExpectRun(map[string]any{"expect_run": got["expect_run"]}); err != nil {
		t.Fatalf("the handler refuses the expect_run: %v", err)
	}
	var stored string
	if err := h.db.QueryRow(`SELECT expect_run FROM plugin_op_attempts WHERE op_id = ? AND call = 'start'`, op.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var x map[string]any
	if json.Unmarshal([]byte(stored), &x); !reflect.DeepEqual(x, run) {
		t.Fatalf("the attempt's expect_run: %s, want %v", stored, run)
	}
}

// The row guard sees an add's attempts by its manifest name (codex-27's C5 note): an add that
// wrote its row has the name notes, so its open bound start blocks a second add of notes. The
// guard does not count the op's own attempts.
func TestPluginOpAddNameGuard(t *testing.T) {
	ctx := context.Background()
	t.Run("another add's open start", func(t *testing.T) {
		h := rowHub(t)
		first := crashedAfterRow(t, h, "first")
		if first.Name != "notes" {
			t.Fatalf("the first add's stored name: %q", first.Name)
		}
		if _, err := h.db.Exec(`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, bound_at) VALUES ('s1', ?, 'start', 1, 't')`, first.ID); err != nil {
			t.Fatal(err)
		}
		second := writingAdd(t, h, "second", "github:acme/other@v1.0.0", "other")
		if err := h.opWrite(ctx, second); err != nil {
			t.Fatal(err)
		}
		if second.Phase != "conflict" || second.Error != "a plugin named notes has a record or a start in flight; nothing more was written" {
			t.Fatalf("%+v, want the guard's conflict", second)
		}
	})
	t.Run("the op's own attempt", func(t *testing.T) {
		h := rowHub(t)
		op := writingAdd(t, h, "own", toolsSource, "tools")
		// Another writer puts the add's intended entry in the file: the written path, with no row
		// record yet, runs the guard.
		os.WriteFile(h.cfg.managedPath, []byte(`{"plugins":[{"id":"tools","source":"`+toolsSource+`"}]}`), 0o600)
		if _, err := h.db.Exec(`UPDATE plugin_ops SET name = 'notes' WHERE op_id = ?`, op.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.Exec(`INSERT INTO plugin_op_attempts (attempt, op_id, call, at, bound_at) VALUES ('s1', ?, 'start', 1, 't')`, op.ID); err != nil {
			t.Fatal(err)
		}
		if err := h.opWrite(ctx, op); err != nil || op.Phase != "reloading" {
			t.Fatalf("the own attempt blocked the add: %+v %v", op, err)
		}
	})
}

// The evidence of an update's lifecycle call names the entry after the write (C5 plan r2 D):
// the record of y has the new entry's identity, the attempt's intent is created and its instance
// registered. op.EntryIdentity still names the source at admission.
func TestPluginOpUpdateEvidenceNamesNewEntry(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	ctx := context.Background()
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "y"))
	opsLoadEntries(t, h)
	e := opsEntry(t, h, "managed:y")
	pl := &Plugin{Name: "y", entry: e}
	h.plugins["y"] = pl
	if err := h.loadPluginRow(ctx, pl); err != nil {
		t.Fatal(err)
	}
	if err := h.insertIntent(ctx, pl, "y-1-aaaaaaaa", "session:home"); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	pl.InstanceID, pl.SessionID = "y-1-aaaaaaaa", "session:home"
	h.mu.Unlock()
	createAndRegister(t, h, pl)
	base, _ := json.Marshal(managedFileEntry{ID: "y", Source: "github:acme/y@v0"})
	target, _ := json.Marshal(opResolved{Source: e.Source})
	op := &pluginOp{Kind: "update", Entry: "managed:y", Name: "y", Base: string(base), Target: string(target),
		EntryIdentity: `["managed:y","github:acme/y@v0"]`}
	proof, detail, _, err := h.attemptEvidence(ctx, op, "replace", attemptTarget{intent: "y-1-aaaaaaaa"})
	if err != nil || !strings.HasPrefix(proof, "applied") {
		t.Fatalf("the evidence: proof %q, detail %q, %v", proof, detail, err)
	}
}
