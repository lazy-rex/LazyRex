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
	err := h.spawn(context.Background(), pl, &pluginRun{Name: "p"})
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
	err = h.spawn(context.Background(), pl, &pluginRun{Name: "p"})
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
