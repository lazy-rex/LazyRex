package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The startup path with the state of an older build's app mode, against a real Rex lab
// (SPAWN_LAB_SOCKET). The lab has a user session whose block an active cycle detached, and an old
// app:<plugin> session with a user pane. The hub starts on a database with the old tables.
// Nothing in Rex changes: the detached block stays detached and alive, and no pane closes.
func TestAppModeStateRemovedAtStartupLab(t *testing.T) {
	r, ctx := labRex(t)
	newSession := func(label string) (string, string) {
		t.Helper()
		var s struct {
			ID string `json:"session_id"`
		}
		if err := r.Call(ctx, "session.create", map[string]any{"label": label, "initial_windows": []any{map[string]any{"window_label": "w",
			"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell",
				"options": map[string]any{"command": []string{"/bin/sleep", "600"}, "login": false}}}}}}, &s); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Call(context.Background(), "session.destroy", map[string]any{"session_id": s.ID}, nil) })
		if err := r.Call(ctx, "session.attach", map[string]any{"session_id": s.ID, "event_scope": "session"}, nil); err != nil {
			t.Fatal(err)
		}
		var out struct {
			Blocks []rexBlock `json:"blocks"`
		}
		if err := r.Call(ctx, "session.list_blocks", map[string]any{"session_id": s.ID}, &out); err != nil || len(out.Blocks) != 1 {
			t.Fatalf("blocks of %s: %v %v", label, out.Blocks, err)
		}
		return s.ID, out.Blocks[0].BlockID
	}
	user, hidden := newSession("appmode-user")
	app, _ := newSession("appmode-old-app")
	if err := r.Call(ctx, "session.detach_block", map[string]any{"session_id": user, "block_id": hidden}, nil); err != nil {
		t.Fatal(err)
	}

	h, _ := workerTestHub(t)
	h.rex = r
	h.selfClosed = map[string]bool{}
	view := filepath.Join(t.TempDir(), "view.sh")
	os.WriteFile(view, []byte("#!/bin/sh\nexec sleep 600\n"), 0o755)
	h.cfg.View = view
	for _, q := range []string{uiSchema, oldAppModeSchema,
		`INSERT INTO ui_cycles (cycle, seq, phase, created, updated) VALUES ('c1', 1, 'active', 't', 't')`,
		`INSERT INTO ui_hidden (cycle, session_id, spos, wpos, window, active) VALUES ('c1', '` + user + `', 0, 0, '{}', 1)`,
		`INSERT INTO ui_apps VALUES ('messaging', 'Messages')`,
		`INSERT INTO ui_sessions VALUES ('app:messaging', '` + app + `')`,
		`INSERT INTO ui_state VALUES ('active', 'messaging')`,
		`INSERT INTO ui_restores VALUES ('n1', 'hidden:c1:` + user + `:0', 't')`,
	} {
		if _, err := h.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	layout := func() map[string]map[string]rexBlock {
		t.Helper()
		out := map[string]map[string]rexBlock{}
		for _, s := range []string{user, app} {
			b, err := h.blocksIn(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			out[s] = b
		}
		return out
	}
	before := layout()
	if before[user][hidden].Placement != "detached" {
		t.Fatalf("the hidden block is not detached: %+v", before[user])
	}
	pid, err := h.childIn(ctx, user, hidden)
	if err != nil || pid == 0 {
		t.Fatalf("the hidden block's process: %d %v", pid, err)
	}

	start := func() {
		t.Helper()
		h.generation++
		if err := h.uiStart(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			var home string
			h.db.QueryRow(`SELECT session_id FROM ui_sessions WHERE plugin = ?`, uiSessionKey).Scan(&home)
			if home != "" {
				r.Call(context.Background(), "session.destroy", map[string]any{"session_id": home}, nil)
			}
		})
		if _, err := h.reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	start()
	for _, name := range appModeTables {
		if tableExists(t, h, name) {
			t.Errorf("table %s still exists", name)
		}
	}
	events := filepath.Join(filepath.Dir(h.ev.f.Name()), "events.jsonl")
	if n := len(eventsNamed(t, events, "app_mode_state_removed")); n != 1 {
		t.Errorf("%d app_mode_state_removed events, want 1", n)
	}
	// The old app session has a user pane: released, not destroyed.
	var appRows int
	h.db.QueryRow(`SELECT COUNT(*) FROM ui_sessions WHERE session_id = ?`, app).Scan(&appRows)
	if appRows != 0 {
		t.Error("the old app session is still recorded")
	}
	if after := layout(); !reflect.DeepEqual(after, before) {
		t.Errorf("the layout changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if p, err := h.childIn(ctx, user, hidden); err != nil || p != pid {
		t.Errorf("the hidden block's process: %d %v, want %d", p, err, pid)
	}

	// A second start: no event, no change.
	start()
	if n := len(eventsNamed(t, events, "app_mode_state_removed")); n != 1 {
		t.Errorf("a second start: %d app_mode_state_removed events", n)
	}
	if after := layout(); !reflect.DeepEqual(after, before) {
		t.Errorf("the second start changed the layout:\nbefore %+v\nafter  %+v", before, after)
	}
}
