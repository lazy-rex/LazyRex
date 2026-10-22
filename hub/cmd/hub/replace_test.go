package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A spawn failure before session.new_block is sent is known, and it names its stage: no work
// session, or an intent that the store did not record. Nothing went to Rex (the hub has no Rex
// client: a Rex call panics).
func TestSpawnLocalFailureStages(t *testing.T) {
	h, _ := appModeHub(t)
	pl := &Plugin{Name: "p"}
	err := h.spawn(context.Background(), pl, &pluginRun{Name: "p"}, nil)
	var se *spawnError
	if !errors.As(err, &se) || se.stage != "session" || !se.known {
		t.Errorf("no work session: %#v", err)
	}
	if !strings.Contains(err.Error(), "the LazyRex session is gone") {
		t.Errorf("the text: %v", err)
	}

	h.ui.sess["session:home"] = uiSessionKey
	if _, err := h.db.Exec(`DROP TABLE spawn_intents`); err != nil {
		t.Fatal(err)
	}
	err = h.spawn(context.Background(), pl, &pluginRun{Name: "p"}, nil)
	if !errors.As(err, &se) || se.stage != "intent" || !se.known {
		t.Errorf("an intent insert failure: %#v", err)
	}
	if pl.InstanceID != "" {
		t.Errorf("the record took instance %s without an intent", pl.InstanceID)
	}
}

// hub.admin.replace checks its arguments before it takes a lock.
func TestReplaceArguments(t *testing.T) {
	h := &Hub{plugins: map[string]*Plugin{}}
	for _, args := range []map[string]any{
		{"plugin": "p"},
		{"plugin": "p", "expect_instance": ""},
		{"expect_instance": "p-1-x"},
		{"plugin": "p", "expect_instance": "p-1-x", "force": "1"},
		{"plugin": "p", "expect_instance": 7.0},
	} {
		if _, err := h.adminReplace(context.Background(), args); err == nil || !strings.HasPrefix(err.Error(), "invalid") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := h.adminReplace(context.Background(), map[string]any{"plugin": "q", "expect_instance": "q-1-x"}); err == nil || !strings.Contains(err.Error(), `unknown plugin "q"`) {
		t.Errorf("an unknown plugin: %v", err)
	}
}

// A known spawn failure after the intent insert abandons the intent, and checks that it did. An
// intent that the store keeps open is in the error (open), and ownedInstance reports it as
// pending. A SQLite trigger refuses the status updates; a plugin-create fault refuses the create.
// Nothing reaches Rex (the hub has no Rex client).
func TestSpawnKnownFailureIntents(t *testing.T) {
	for _, c := range []struct {
		name, refuse string // refuse: the statuses that the trigger refuses
		rexRefuses   bool
		stage, open  string // open: the status that stays, "" for abandoned
	}{
		{"the sending update", "'sending'", false, "sending", ""},
		{"the sending update and its abandonment", "'sending', 'abandoned'", false, "sending", "prepared"},
		{"a Rex refusal", "'none'", true, "create", ""},
		{"a Rex refusal and its abandonment", "'abandoned'", true, "create", "sending"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := appModeHub(t)
			h.faults = &faults{}
			h.test = &testState{}
			if c.rexRefuses {
				h.test.faults = []*testFault{{Point: "plugin-create", Mode: "refuse", Nth: 1}}
			}
			h.ui.sess["session:home"] = uiSessionKey
			if _, err := h.db.Exec(`CREATE TRIGGER refuse_status BEFORE UPDATE OF status ON spawn_intents
				WHEN NEW.status IN (` + c.refuse + `) BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
				t.Fatal(err)
			}
			pl := &Plugin{Name: "p"}
			err := h.spawn(context.Background(), pl, &pluginRun{Name: "p"}, nil)
			var se *spawnError
			if !errors.As(err, &se) || !se.known || se.stage != c.stage || se.open != c.open {
				t.Fatalf("spawn: %#v", err)
			}
			var status string
			if err := h.db.QueryRow(`SELECT status FROM spawn_intents WHERE instance_id = ?`, pl.InstanceID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			want := c.open
			if want == "" {
				want = "abandoned"
			}
			if status != want {
				t.Errorf("the intent is %s; want %s", status, want)
			}
			id, pending := h.ownedInstance(pl)
			if c.open != "" && (id != pl.InstanceID || !pending) {
				t.Errorf("ownedInstance: %q, %v; want the open intent %s", id, pending, pl.InstanceID)
			}
			if c.open == "" && id != "" {
				t.Errorf("ownedInstance: %q after the abandonment", id)
			}
			if c.open != "" && !strings.Contains(err.Error(), "stays "+c.open) {
				t.Errorf("the text: %v", err)
			}
		})
	}
}

// expect_run is checked before any lock, on replace and on start: its shape, and commit and
// digest exactly for a GitHub source. hub.admin.stop has no expect_run.
func TestExpectRunArguments(t *testing.T) {
	h := &Hub{plugins: map[string]*Plugin{}}
	c, d := strings.Repeat("a", 40), "sha256:"+strings.Repeat("b", 64)
	for _, x := range []any{
		"src/a",
		map[string]any{},
		map[string]any{"source": ""},
		map[string]any{"source": "src/a", "commit": c},
		map[string]any{"source": "src/a", "digest": d},
		map[string]any{"source": "github:o/r@main"},
		map[string]any{"source": "github:o/r@main", "commit": c},
		map[string]any{"source": "github:o/r@main", "commit": strings.ToUpper(c), "digest": d},
		map[string]any{"source": "github:o/r@main", "commit": c, "digest": "sha256:x"},
		map[string]any{"source": "github:o/r@main", "commit": c, "digest": d, "dir": "x"},
		map[string]any{"source": 7.0},
	} {
		if _, err := h.adminReplace(context.Background(), map[string]any{"plugin": "p", "expect_instance": "p-1-x", "expect_run": x}); err == nil || !strings.HasPrefix(err.Error(), "invalid") {
			t.Errorf("replace %v: %v", x, err)
		}
		if _, err := h.adminAction(context.Background(), "hub.admin.start", map[string]any{"plugin": "p", "expect_run": x}); err == nil || !strings.HasPrefix(err.Error(), "invalid") {
			t.Errorf("start %v: %v", x, err)
		}
	}
	if _, err := h.adminAction(context.Background(), "hub.admin.stop", map[string]any{"plugin": "p", "expect_run": map[string]any{"source": "src/p"}}); err == nil || !strings.HasPrefix(err.Error(), "invalid") {
		t.Errorf("stop: %v", err)
	}
	// Valid shapes pass the parse and reach the plugin lookup.
	for _, x := range []map[string]any{{"source": "src/p"}, {"source": "github:o/r/sub@v1", "commit": c, "digest": d}} {
		if _, err := h.adminReplace(context.Background(), map[string]any{"plugin": "p", "expect_instance": "p-1-x", "expect_run": x}); err == nil || !strings.Contains(err.Error(), `unknown plugin "p"`) {
			t.Errorf("valid %v: %v", x, err)
		}
	}
	x := &expectRun{"github:o/r/one@main", c, d}
	for name, r := range map[string]*pluginRun{
		"subdir":                   {Source: "github:o/r/two@main", Commit: c, Digest: d},
		"commit":                   {Source: x.Source, Commit: strings.Repeat("c", 40), Digest: d},
		"digest":                   {Source: x.Source, Commit: c, Digest: "sha256:" + strings.Repeat("e", 64)},
		"old run without a digest": {Source: x.Source, Commit: c},
	} {
		if x.matches(r) {
			t.Errorf("%s: matches", name)
		}
	}
	if !x.matches(&pluginRun{Source: x.Source, Commit: c, Digest: d}) || x.matches(nil) {
		t.Error("the exact run")
	}
}

// A guarded start whose entry was removed after the resolution refuses before any write: the
// desired state stays, and the nil db shows that nothing was saved (codex-23's regression).
func TestExpectRunRemovedEntry(t *testing.T) {
	h := &Hub{}
	pl := &Plugin{Name: "p", Desired: "stopped"} // pl.entry is nil: the entry was removed
	err := h.startExpected(context.Background(), pl, false, &expectRun{Source: "src/p"}, &configEntry{}, &pluginRun{Source: "src/p"}, nil)
	if err == nil || !strings.Contains(err.Error(), "refused: plugin p: its entry was removed during the start; nothing was retired or spawned") {
		t.Fatalf("got %v", err)
	}
	if pl.Desired != "stopped" {
		t.Fatalf("desired %q", pl.Desired)
	}
}
