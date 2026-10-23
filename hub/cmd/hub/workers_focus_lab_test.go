package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

func TestSpawnFocusValidation(t *testing.T) {
	for _, raw := range []any{nil, true, false, "window:1", map[string]any{},
		map[string]any{"source_window_id": ""}, map[string]any{"source_window_id": 1},
		map[string]any{"source_window_id": "window:1", "extra": true}} {
		if _, err := parseSpawnFocus(raw); err == nil {
			t.Fatalf("accepted focus %#v", raw)
		}
	}
	focus := map[string]any{"source_window_id": "window:1"}
	if _, err := parseWorkerPlacement(map[string]any{"focus": focus, "detached": true}); err == nil {
		t.Fatal("accepted focus on a detached pane")
	}
	if p, err := parseWorkerPlacement(map[string]any{"focus": focus}); err != nil || p.Focus.SourceWindowID != "window:1" {
		t.Fatalf("focus: %+v %v", p, err)
	}
}

// TestSpawnFocusLab exercises the real spawn path and Rex's server-side focus state.
// It does not claim that an app changes its selected session or tab.
func TestSpawnFocusLab(t *testing.T) {
	r, ctx, server := labRex(t)
	realCreate := spawnCreate
	t.Cleanup(func() { spawnCreate = realCreate })
	newTab := func(t *testing.T, session string) (string, string) {
		t.Helper()
		if err := r.Call(ctx, "session.attach", map[string]any{"session_id": session, "event_scope": "session"}, nil); err != nil {
			t.Fatal(err)
		}
		var out struct {
			Window string   `json:"window_id"`
			Blocks []string `json:"block_ids"`
		}
		if err := r.Call(ctx, "session.new_window", map[string]any{"session_id": session, "focus": false,
			"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell",
				"options": map[string]any{"command": []string{"/bin/sleep", "120"}, "login": false}}}}, &out); err != nil {
			t.Fatal(err)
		}
		return out.Window, out.Blocks[0]
	}
	activate := func(t *testing.T, session, block string) {
		t.Helper()
		if err := r.Call(ctx, "session.focus_block", map[string]any{"session_id": session, "block_id": block}, nil); err != nil {
			t.Fatal(err)
		}
	}
	active := func(t *testing.T, session string) string {
		t.Helper()
		var out struct {
			Window string `json:"active_window_id"`
		}
		if err := r.Call(ctx, "session.view", map[string]any{"session_id": session}, &out); err != nil {
			t.Fatal(err)
		}
		return out.Window
	}
	setup := func(t *testing.T) (*Hub, perform, string, string, string) {
		h, p, session := labSpawnHub(t, r, ctx, server)
		if _, err := h.db.Exec(`DELETE FROM spawns`); err != nil {
			t.Fatal(err)
		}
		window, block := newTab(t, session)
		activate(t, session, block)
		return h, p, session, window, block
	}
	request := func(session, window string) map[string]any {
		return map[string]any{"for": "user", "profile": "probe", "key": "focus-test",
			"placement": map[string]any{"session": session, "focus": map[string]any{"source_window_id": window}}}
	}
	spawn := func(t *testing.T, h *Hub, p perform, req map[string]any) map[string]any {
		t.Helper()
		out, err := h.spawnWorker(ctx, p, req)
		if err != nil {
			t.Fatal(err)
		}
		return out.(map[string]any)
	}
	t.Run("focus-once-and-persist-source", func(t *testing.T) {
		h, p, session, source, sourceBlock := setup(t)
		req := request(session, source)
		out := spawn(t, h, p, req)
		if out["status"] != "open" || active(t, session) != out["window_id"] {
			t.Fatalf("spawn did not focus: %v", out)
		}
		var stored string
		if err := h.db.QueryRow(`SELECT request FROM spawns WHERE spawn_id=?`, out["spawn_id"]).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		var saved map[string]any
		if err := json.Unmarshal([]byte(stored), &saved); err != nil {
			t.Fatal(err)
		}
		if saved["placement"].(map[string]any)["focus"].(map[string]any)["source_window_id"] != source {
			t.Fatalf("source not persisted: %s", stored)
		}
		activate(t, session, sourceBlock)
		again := spawn(t, h, p, req)
		if again["spawn_id"] != out["spawn_id"] || active(t, session) != source {
			t.Fatalf("replay changed focus or identity: %v", again)
		}
		if blocks, err := h.blocksIn(ctx, session); err != nil || len(blocks) != 2 {
			t.Fatalf("replay duplicated a block: %v %v", blocks, err)
		}
	})
	t.Run("default-does-not-focus", func(t *testing.T) {
		h, p, session, source, _ := setup(t)
		req := request(session, source)
		delete(req["placement"].(map[string]any), "focus")
		spawn(t, h, p, req)
		if active(t, session) != source {
			t.Fatal("default spawn changed focus")
		}
	})
	t.Run("agent-cannot-request-focus", func(t *testing.T) {
		h, _, session, source, sourceBlock := setup(t)
		pid, err := h.childPIDIn(ctx, session, sourceBlock)
		if err != nil || pid == 0 {
			t.Fatalf("source process: %d %v", pid, err)
		}
		_, start, ok := procInfo(pid)
		if !ok {
			t.Fatal("source process not alive")
		}
		h.agents["caller"] = &Agent{ID: "caller", Session: session, BlockID: sourceBlock, ChildPID: pid, ChildStart: start}
		var caller perform
		caller.Requester.Principal.PID = pid
		req := request(session, source)
		delete(req, "for")
		if _, err := h.spawnWorker(ctx, caller, req); err == nil || !strings.Contains(err.Error(), "only available for user") {
			t.Fatalf("agent focus: %v", err)
		}
		if blocks, err := h.blocksIn(ctx, session); err != nil || len(blocks) != 1 {
			t.Fatalf("refused request created a pane: %v %v", blocks, err)
		}
	})
	t.Run("user-moves-during-create", func(t *testing.T) {
		h, p, session, source, _ := setup(t)
		otherWindow, otherBlock := newTab(t, session)
		spawnCreate = func(ctx context.Context, h *Hub, method string, req map[string]any, out any) error {
			if err := realCreate(ctx, h, method, req, out); err != nil {
				return err
			}
			return r.Call(ctx, "session.focus_block", map[string]any{"session_id": session, "block_id": otherBlock}, nil)
		}
		defer func() { spawnCreate = realCreate }()
		spawn(t, h, p, request(session, source))
		if active(t, session) != otherWindow {
			t.Fatal("spawn pulled the user back")
		}
	})
	t.Run("source-in-another-session", func(t *testing.T) {
		h, p, session, source, _ := setup(t)
		_, _, foreignSession := labSpawnHub(t, r, ctx, server)
		foreignWindow, _ := newTab(t, foreignSession)
		spawn(t, h, p, request(session, foreignWindow))
		if active(t, session) != source {
			t.Fatal("a source in another session changed focus")
		}
	})
	t.Run("split-focuses-new-block", func(t *testing.T) {
		h, p, session, source, sourceBlock := setup(t)
		req := request(session, source)
		req["placement"].(map[string]any)["beside"] = sourceBlock
		out := spawn(t, h, p, req)
		var view struct {
			Windows []struct {
				ID      string `json:"window_id"`
				Focused string `json:"focused_block_id"`
			} `json:"windows"`
		}
		if err := r.Call(ctx, "session.view", map[string]any{"session_id": session}, &view); err != nil {
			t.Fatal(err)
		}
		if out["status"] != "open" || len(view.Windows) != 1 || view.Windows[0].Focused != out["block_id"] {
			t.Fatalf("split focus: %v %+v", out, view)
		}
	})
	t.Run("unknown-and-late-open-never-focus", func(t *testing.T) {
		h, p, session, source, _ := setup(t)
		var actualBlock, actualWindow string
		spawnCreate = func(ctx context.Context, h *Hub, method string, req map[string]any, out any) error {
			if err := realCreate(ctx, h, method, req, out); err != nil {
				return err
			}
			raw, _ := json.Marshal(out)
			var actual struct {
				Window string   `json:"window_id"`
				Blocks []string `json:"block_ids"`
			}
			json.Unmarshal(raw, &actual)
			actualBlock, actualWindow = actual.Blocks[0], actual.Window
			return &rex.RexError{Code: "disconnected", Message: "lab lost answer"}
		}
		defer func() { spawnCreate = realCreate }()
		req := request(session, source)
		out := spawn(t, h, p, req)
		if out["status"] != "pending" || active(t, session) != source {
			t.Fatalf("unknown spawn: %v", out)
		}
		s, err := h.spawnByID(ctx, out["spawn_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		// A late lifecycle adoption uses openSpawn, with no focus side effect.
		if err := h.openSpawn(ctx, s, actualWindow, actualBlock); err != nil {
			t.Fatal(err)
		}
		spawn(t, h, p, req)
		if active(t, session) != source {
			t.Fatal("late open or replay changed focus")
		}
	})
}
