package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

func TestSpawnWindowValidation(t *testing.T) {
	leaf := map[string]any{"leaf": map[string]any{"profile": "probe"}}
	for _, raw := range []any{nil, "x", map[string]any{}, map[string]any{"leaf": map[string]any{}},
		map[string]any{"leaf": map[string]any{"profile": "probe", "cwd": "rel"}},
		map[string]any{"leaf": map[string]any{"profile": "probe", "label": "x"}},
		map[string]any{"leaf": map[string]any{"profile": "probe"}, "split": map[string]any{}},
		map[string]any{"split": map[string]any{"direction": "diagonal", "ratio": 0.5, "before": leaf, "after": leaf}},
		map[string]any{"split": map[string]any{"direction": "horizontal", "ratio": 1.0, "before": leaf, "after": leaf}},
		map[string]any{"split": map[string]any{"direction": "horizontal", "ratio": 0.5, "before": leaf}},
		map[string]any{"split": map[string]any{"direction": "horizontal", "ratio": 0.5, "before": leaf, "after": leaf, "side": "after"}},
	} {
		var leaves []*windowLeaf
		if _, err := parseWindowNode(raw, &leaves); err == nil {
			t.Fatalf("accepted %#v", raw)
		}
	}
}

// TestSpawnWindowLab runs hub.spawn_window against an isolated Rex server (rexLab: LAZYREX_LAB=1).
func TestSpawnWindowLab(t *testing.T) {
	l := rexLab(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) // first, so it runs after every cleanup that uses r
	go func() {
		for range r.Events {
		}
	}()
	session := func(t *testing.T) string {
		var out struct {
			ID string `json:"session_id"`
		}
		if err := r.Call(ctx, "session.create", map[string]any{"label": "spawn-window-test"}, &out); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := r.Call(cleanup, "session.attach", map[string]any{"session_id": out.ID, "event_scope": "session"}, nil); err == nil {
				r.Call(cleanup, "session.destroy", map[string]any{"session_id": out.ID}, nil)
			}
		})
		return out.ID
	}
	const pluginClient = "client:00000000-0000-0000-0000-000000000001"
	setup := func(t *testing.T) (*Hub, perform) {
		h, _ := workerTestHub(t)
		if _, err := h.db.Exec(spawnWindowSchema + `DELETE FROM spawns; DELETE FROM spawn_windows`); err != nil {
			t.Fatal(err)
		}
		h.rex, h.cfg.Server = r, l.server
		h.ui = &uiState{sess: map[string]string{session(t): uiSessionKey}, kick: make(chan struct{}, 1)}
		h.selfClosed = map[string]bool{}
		h.vs = newViewSvc()
		h.vs.workers["test"] = make(chan struct{}, 1)
		h.plugins = map[string]*Plugin{"test": {Name: "test", ClientID: pluginClient}}
		h.cfg.Profiles = t.TempDir() + "/profiles.json"
		if err := os.WriteFile(h.cfg.Profiles, []byte(`{"profiles":[
			{"id":"probe","name":"Probe","kind":"accessory","command":["/bin/sleep","120"]},
			{"id":"task","name":"Task","kind":"accessory","command":["/bin/sh","-c","sleep 120","${task}"],"args":[{"name":"task","default":"d"}]},
			{"id":"bot","name":"Bot","kind":"agent","command":["/bin/sleep","120"]}]}`), 0600); err != nil {
			t.Fatal(err)
		}
		var p perform
		p.Requester.ClientID = pluginClient
		return h, p
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	leaf := func(profile, cwd string, extra map[string]any) map[string]any {
		l := map[string]any{"profile": profile, "cwd": cwd}
		for k, v := range extra {
			l[k] = v
		}
		return map[string]any{"leaf": l}
	}
	split := func(dir string, ratio float64, before, after any) map[string]any {
		return map[string]any{"split": map[string]any{"direction": dir, "ratio": ratio, "before": before, "after": after}}
	}
	// A 3-pane tab, nested on the after side: probe | (task / bot).
	layout := split("horizontal", 0.3, leaf("probe", dirA, nil),
		split("vertical", 0.6, leaf("task", dirB, map[string]any{"args": map[string]any{"task": "a; $(x)"}, "title": "Tests"}), leaf("bot", dirA, nil)))
	call := func(t *testing.T, h *Hub, p perform, args map[string]any) map[string]any {
		t.Helper()
		// Round trip through JSON, as a client.perform request arrives.
		raw, _ := json.Marshal(args)
		var m map[string]any
		json.Unmarshal(raw, &m)
		out, err := h.spawnWindow(ctx, p, m)
		if err != nil {
			t.Fatal(err)
		}
		return out.(map[string]any)
	}

	t.Run("OpenRetrySpec", func(t *testing.T) {
		h, p := setup(t)
		target := session(t)
		args := map[string]any{"for": "user", "key": "tpl/run1/0", "session": target, "title": "Dev", "layout": layout}
		out := call(t, h, p, args)
		if out["status"] != "open" || out["session_id"] != target {
			t.Fatalf("open: %#v", out)
		}
		leaves := out["leaves"].([]any)
		if len(leaves) != 3 {
			t.Fatalf("leaves: %#v", leaves)
		}
		var ids []string
		for _, l := range leaves {
			lm := l.(map[string]any)
			if lm["status"] != "open" || lm["window_id"] != out["window_id"] {
				t.Fatalf("leaf: %#v", lm)
			}
			ids = append(ids, lm["block_id"].(string))
		}
		if leaves[2].(map[string]any)["agent_id"] == nil {
			t.Fatal("the agent leaf has no agent")
		}
		// Rex's tree has the requested shape, the leaves in depth-first order, and the labels.
		var view struct {
			Windows []struct {
				Label  string `json:"label"`
				Layers []struct {
					Layout *lnode `json:"layout"`
				} `json:"layers"`
			} `json:"windows"`
		}
		if err := r.Call(ctx, "session.view", map[string]any{"session_id": target}, &view); err != nil {
			t.Fatal(err)
		}
		if len(view.Windows) != 1 || view.Windows[0].Label != "Dev" {
			t.Fatalf("windows: %#v", view.Windows)
		}
		n := view.Windows[0].Layers[0].Layout
		if n.Split == nil || n.Split.Direction != "horizontal" || n.Split.Ratio != 0.3 || n.Split.Before.BlockID != ids[0] ||
			n.Split.After.Split == nil || n.Split.After.Split.Direction != "vertical" || n.Split.After.Split.Ratio != 0.6 ||
			n.Split.After.Split.Before.BlockID != ids[1] || n.Split.After.Split.After.BlockID != ids[2] {
			t.Fatalf("layout: %#v", n)
		}
		blocks, err := h.blocksIn(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range leaves {
			if want := "lazyrex-spawn:" + l.(map[string]any)["spawn_id"].(string); blocks[ids[i]].Label != want {
				t.Fatalf("label %d: %q", i, blocks[ids[i]].Label)
			}
		}
		// The pane's process runs in the leaf's directory.
		realB, _ := filepath.EvalSymlinks(dirB)
		deadline := time.Now().Add(5 * time.Second)
		for {
			var proc struct {
				Child struct {
					Cwd string `json:"cwd"`
				} `json:"child"`
			}
			err := r.BlockCall(ctx, "com.superlogical.terminal.process", target, ids[1], nil, &proc)
			if err == nil && proc.Child.Cwd == realB {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("cwd: %q %v", proc.Child.Cwd, err)
			}
			time.Sleep(100 * time.Millisecond)
		}

		// A retry returns the same spawns and creates nothing.
		again := call(t, h, p, args)
		if again["status"] != "open" || len(again["leaves"].([]any)) != 3 ||
			again["leaves"].([]any)[1].(map[string]any)["spawn_id"] != leaves[1].(map[string]any)["spawn_id"] {
			t.Fatalf("retry: %#v", again)
		}
		if blocks, _ := h.blocksIn(ctx, target); len(blocks) != 3 {
			t.Fatalf("retry created panes: %d", len(blocks))
		}
		// A changed request with the same key is refused.
		changed := map[string]any{"for": "user", "key": "tpl/run1/0", "session": target, "title": "Other", "layout": layout}
		raw, _ := json.Marshal(changed)
		var m map[string]any
		json.Unmarshal(raw, &m)
		if _, err := h.spawnWindow(ctx, p, m); err == nil || !strings.Contains(err.Error(), "different request") {
			t.Fatalf("changed request: %v", err)
		}

		// The spec of a leaf is what a template saves.
		spec, err := h.spawnSpec(ctx, p, map[string]any{"block_id": ids[1]})
		if err != nil {
			t.Fatal(err)
		}
		sm := spec.(map[string]any)
		if sm["found"] != true || sm["profile"] != "task" || sm["args"].(map[string]string)["task"] != "a; $(x)" || sm["cwd"] != realB || sm["title"] != "Tests" ||
			sm["requested_title"] != "Tests" || sm["title_source"] != "request" {
			t.Fatalf("spec: %#v", sm)
		}
		// A hub.spawn row has the same spec.
		single, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "task", "key": "single", "args": map[string]any{"task": "t"},
			"cwd": dirA, "placement": map[string]any{"session": target}})
		if err != nil {
			t.Fatal(err)
		}
		spec, _ = h.spawnSpec(ctx, p, map[string]any{"block_id": single.(map[string]any)["block_id"].(string)})
		if sm := spec.(map[string]any); sm["profile"] != "task" || sm["args"].(map[string]string)["task"] != "t" ||
			sm["requested_title"] != nil || sm["title_source"] != "request" || sm["title"] == "" {
			t.Fatalf("single spec: %#v", sm)
		}
		// The spec has the args after defaults.
		dflt, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "task", "key": "dflt", "placement": map[string]any{"session": target}})
		if err != nil {
			t.Fatal(err)
		}
		spec, _ = h.spawnSpec(ctx, p, map[string]any{"block_id": dflt.(map[string]any)["block_id"].(string)})
		if sm := spec.(map[string]any); sm["args"].(map[string]string)["task"] != "d" || sm["args_source"] != "effective" {
			t.Fatalf("default spec: %#v", sm)
		}
		if spec, _ := h.spawnSpec(ctx, p, map[string]any{"block_id": "block:none"}); spec.(map[string]any)["found"] != false {
			t.Fatalf("missing spec: %#v", spec)
		}
		var stranger perform
		stranger.Requester.ClientID = "client:00000000-0000-0000-0000-000000000009"
		if _, err := h.spawnSpec(ctx, stranger, map[string]any{"block_id": ids[1]}); err == nil {
			t.Fatal("a non-plugin read a spec")
		}
		// hub.spawn refuses a key in the window namespace.
		if _, err := h.spawnWorker(ctx, p, map[string]any{"for": "user", "profile": "probe", "key": "window/tpl/run1/0/0"}); err == nil {
			t.Fatal("hub.spawn accepted a window key")
		}
	})

	t.Run("NotCreated", func(t *testing.T) {
		h, p := setup(t)
		target := session(t)
		real := windowCreate
		t.Cleanup(func() { windowCreate = real })
		calls := 0
		windowCreate = func(ctx context.Context, h *Hub, req map[string]any, out any) error {
			calls++
			return real(ctx, h, req, out)
		}
		// The sending transition of the batch fails: no create, and every leaf fails as not created.
		allow := rejectSending(t, h)
		out := call(t, h, p, map[string]any{"for": "user", "key": "refused", "session": target, "layout": layout})
		allow()
		if out["status"] != "failed" || calls != 0 {
			t.Fatalf("refused: %#v, %d calls", out, calls)
		}
		for _, l := range out["leaves"].([]any) {
			if l.(map[string]any)["status"] != "failed" || !strings.HasPrefix(l.(map[string]any)["error"].(string), "not_created: ") {
				t.Fatalf("refused leaf: %#v", l)
			}
		}
		if blocks, _ := h.blocksIn(ctx, target); len(blocks) != 0 {
			t.Fatalf("refused: %d panes", len(blocks))
		}
		// The failure write fails too: an error, not a failed result that is not stored. The
		// replay reads pending until resolveNext stores the failures.
		allowSending, allowFailed := rejectSending(t, h), rejectFailed(t, h)
		args := map[string]any{"for": "user", "key": "lost", "session": target, "layout": layout}
		raw, _ := json.Marshal(args)
		var m map[string]any
		json.Unmarshal(raw, &m)
		var unrec *spawnUnrecorded
		if out, err := h.spawnWindow(ctx, p, m); !errors.As(err, &unrec) || out != nil || calls != 0 {
			t.Fatalf("lost: %v %v, %d calls", out, err, calls)
		}
		allowFailed()
		allowSending()
		if out := call(t, h, p, args); out["status"] != "pending" || calls != 0 {
			t.Fatalf("lost replay: %#v, %d calls", out, calls)
		}
		if _, err := h.resolveNext(ctx); err != nil {
			t.Fatal(err)
		}
		if out := call(t, h, p, args); out["status"] != "failed" || calls != 0 {
			t.Fatalf("lost after recovery: %#v, %d calls", out, calls)
		}
	})

	t.Run("UnknownOutcomes", func(t *testing.T) {
		h, p := setup(t)
		target := session(t)
		real := windowCreate
		t.Cleanup(func() { windowCreate = real })
		calls := 0
		// A lost answer: Rex may or may not have created the tab.
		windowCreate = func(ctx context.Context, h *Hub, req map[string]any, out any) error {
			calls++
			return &rex.RexError{Code: "disconnected", Message: "lab: answer lost"}
		}
		args := map[string]any{"for": "user", "key": "lost", "session": target, "layout": layout}
		out := call(t, h, p, args)
		if out["status"] != "pending" || calls != 1 {
			t.Fatalf("lost: %#v, %d calls", out, calls)
		}
		for _, l := range out["leaves"].([]any) {
			if l.(map[string]any)["status"] != "pending" {
				t.Fatalf("lost leaf: %#v", l)
			}
		}
		// A retry never sends a second create.
		if again := call(t, h, p, args); again["status"] != "pending" || calls != 1 {
			t.Fatalf("lost retry: %#v, %d calls", again, calls)
		}
		// A success answer with the wrong shape: the tab exists, the outcome stays unknown.
		windowCreate = func(ctx context.Context, h *Hub, req map[string]any, out any) error {
			calls++
			if err := real(ctx, h, req, out); err != nil {
				return err
			}
			out.(*struct {
				WindowID string   `json:"window_id"`
				BlockIDs []string `json:"block_ids"`
			}).BlockIDs = nil
			return nil
		}
		out = call(t, h, p, map[string]any{"for": "user", "key": "garbled", "session": target, "layout": layout})
		if out["status"] != "pending" || calls != 2 {
			t.Fatalf("garbled: %#v, %d calls", out, calls)
		}
		if blocks, _ := h.blocksIn(ctx, target); len(blocks) != 3 {
			t.Fatalf("garbled: %d panes", len(blocks))
		}
		var failed int
		h.db.QueryRow(`SELECT COUNT(*) FROM spawns WHERE status = 'failed'`).Scan(&failed)
		if failed != 0 {
			t.Fatalf("%d leaves marked failed", failed)
		}
	})

	t.Run("Focus", func(t *testing.T) {
		h, p := setup(t)
		// A session with a source tab; Rex makes a new window active if none is.
		var made struct {
			ID string `json:"session_id"`
		}
		if err := r.Call(ctx, "session.create", map[string]any{"label": "spawn-window-focus", "initial_windows": []any{
			map[string]any{"window_label": "src", "layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell",
				"options": map[string]any{"command": []string{"/bin/sleep", "120"}, "login": false}}}}}}, &made); err != nil {
			t.Fatal(err)
		}
		target := made.ID
		t.Cleanup(func() { r.Call(context.Background(), "session.destroy", map[string]any{"session_id": target}, nil) })
		if err := r.Call(ctx, "session.attach", map[string]any{"session_id": target, "event_scope": "session"}, nil); err != nil {
			t.Fatal(err)
		}
		active := func() (string, string) {
			var v struct {
				Active  string `json:"active_window_id"`
				Windows []struct {
					ID      string `json:"window_id"`
					Focused string `json:"focused_block_id"`
				} `json:"windows"`
			}
			if err := r.Call(ctx, "session.view", map[string]any{"session_id": target}, &v); err != nil {
				t.Fatal(err)
			}
			for _, w := range v.Windows {
				if w.ID == v.Active {
					return w.ID, w.Focused
				}
			}
			return v.Active, ""
		}
		src, _ := active()
		focused := map[string]any{"for": "user", "key": "focus-1", "session": target, "layout": layout,
			"placement": map[string]any{"focus": map[string]any{"source_window_id": src}}}
		out := call(t, h, p, focused)
		win, block := active()
		if out["status"] != "open" || win != out["window_id"] || block != out["leaves"].([]any)[0].(map[string]any)["block_id"] {
			t.Fatalf("focus: active %s %s, result %#v", win, block, out)
		}
		// The user goes back to the source tab; a retry does not take the focus again.
		if err := r.Call(ctx, "session.focus_window", map[string]any{"session_id": target, "window_id": src}, nil); err != nil {
			t.Fatal(err)
		}
		call(t, h, p, focused)
		if win, _ := active(); win != src {
			t.Fatalf("a retry moved the focus to %s", win)
		}
		// Another source tab than the active one: no focus.
		other := map[string]any{"for": "user", "key": "focus-2", "session": target, "layout": layout,
			"placement": map[string]any{"focus": map[string]any{"source_window_id": "window:not-active"}}}
		call(t, h, p, other)
		if win, _ := active(); win != src {
			t.Fatalf("focus without the source tab active: %s", win)
		}
		// Focus is part of the request: the same key with another source is refused.
		changed := map[string]any{"for": "user", "key": "focus-1", "session": target, "layout": layout}
		raw, _ := json.Marshal(changed)
		var m map[string]any
		json.Unmarshal(raw, &m)
		if _, err := h.spawnWindow(ctx, p, m); err == nil || !strings.Contains(err.Error(), "different request") {
			t.Fatalf("changed focus: %v", err)
		}
		for _, bad := range []any{"x", map[string]any{}, map[string]any{"focus": map[string]any{}}, map[string]any{"focus": map[string]any{"source_window_id": src}, "beside": "b"}} {
			raw, _ := json.Marshal(map[string]any{"for": "user", "key": "focus-bad", "session": target, "layout": layout, "placement": bad})
			json.Unmarshal(raw, &m)
			if _, err := h.spawnWindow(ctx, p, m); err == nil {
				t.Fatalf("accepted placement %#v", bad)
			}
		}
	})

	t.Run("RefusalsAdmitNothing", func(t *testing.T) {
		h, p := setup(t)
		target := session(t)
		count := func() (n int) {
			h.db.QueryRow(`SELECT COUNT(*) FROM spawns`).Scan(&n)
			return
		}
		for _, wrong := range []map[string]any{{"session": 42}, {"title": false}, {"for": 1}, {"key": 7}} {
			args := map[string]any{"for": "user", "key": "typed", "session": target, "layout": layout}
			for k, v := range wrong {
				args[k] = v
			}
			if _, err := h.spawnWindow(ctx, p, args); err == nil || count() != 0 {
				t.Fatalf("accepted %v: %d rows", wrong, count())
			}
		}
		bad := split("horizontal", 0.5, leaf("probe", dirA, nil), leaf("nope", dirA, nil))
		raw, _ := json.Marshal(map[string]any{"for": "user", "key": "k1", "session": target, "layout": bad})
		var m map[string]any
		json.Unmarshal(raw, &m)
		if _, err := h.spawnWindow(ctx, p, m); err == nil || count() != 0 {
			t.Fatalf("unknown profile: %v, %d rows", err, count())
		}
		// A locked directory in one leaf refuses the whole window.
		if _, err := h.db.Exec(`INSERT INTO cwd_locks VALUES (?, 'worktrees', 'op', '')`, func() string { d, _ := filepath.EvalSymlinks(dirB); return d }()); err != nil {
			t.Fatal(err)
		}
		raw, _ = json.Marshal(map[string]any{"for": "user", "key": "k2", "session": target, "layout": layout})
		json.Unmarshal(raw, &m)
		if _, err := h.spawnWindow(ctx, p, m); err == nil || !strings.Contains(err.Error(), "locked") || count() != 0 {
			t.Fatalf("locked: %v, %d rows", err, count())
		}
		var windows int
		h.db.QueryRow(`SELECT COUNT(*) FROM spawn_windows`).Scan(&windows)
		if blocks, _ := h.blocksIn(ctx, target); len(blocks) != 0 || windows != 0 {
			t.Fatalf("refusal created %d panes, %d windows", len(blocks), windows)
		}
	})
}

// requestedTitle tells an explicit title from none given, and old or malformed requests from both.
func TestRequestedTitle(t *testing.T) {
	for request, want := range map[string][2]string{
		`{"profile":"p","title":"Build","args":{}}`: {"Build", "request"},
		`{"profile":"p","title":"","args":{}}`:      {"", "request"},
		`{"profile":"p","title":"Claude 1"}`:        {"Claude 1", "request"}, // an explicit title equal to a default
		`{}`:                                        {"", "unknown"},
		`{"title":7}`:                               {"", "unknown"},
		`not json`:                                  {"", "unknown"},
		``:                                          {"", "unknown"},
	} {
		title, source := requestedTitle(request)
		if title != want[0] || source != want[1] {
			t.Errorf("%s: %q %q, want %q %q", request, title, source, want[0], want[1])
		}
	}
}
