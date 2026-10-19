package main

// Fake Rex apps for tests (H2; the coordinator, 2026-10-19: test mode only). No app runs in a lab,
// so hub.app.select_session and hub.view.app_action refuse with no_app. A test asks the hub for a
// fake app: a second Rex connection whose hello says kind "app", as the real app's does
// (templates-lab.mjs did the same from Node). It declares the app actions, shows the sessions
// that the test names (session.attach), answers each client.perform with {} (or a chosen error)
// and records it. userApp finds it like a real app. It lives in the hub process: a hub restart
// closes it.
//
// hub.test.app {name, sessions?, fail?} opens the app (or reuses it) and attaches sessions.
//   fail: an error code that the app answers instead of {} (empty: answer {}).
// hub.test.app.calls {name} -> {client_id, calls: [{action, args}]}
// hub.test.app.close {name}

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// fakeAppActions are the app actions that a fake app declares.
var fakeAppActions = []string{"session.select", "pane.insert_path"}

type testFakeApp struct {
	r     *rex.Rex
	mu    sync.Mutex
	calls []map[string]any
	fail  string
}

func (h *Hub) testApp(ctx context.Context, args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return nil, bad("invalid: name is required")
	}
	h.test.mu.Lock()
	app := h.test.apps[name]
	h.test.mu.Unlock()
	if app == nil {
		socket, err := rex.SocketPath(h.cfg.Server)
		if err != nil {
			return nil, err
		}
		r, err := rex.DialHello(ctx, socket, map[string]any{"client": map[string]any{"kind": "app", "name": name, "label": name}})
		if err != nil {
			return nil, err
		}
		app = &testFakeApp{r: r}
		var actions []map[string]any
		for _, a := range fakeAppActions {
			actions = append(actions, map[string]any{"name": a, "kind": "client", "title": a})
		}
		if err := r.Call(ctx, "client.keymap", map[string]any{"actions": actions}, nil); err != nil {
			r.Close()
			return nil, err
		}
		go app.serve()
		h.test.mu.Lock()
		h.test.apps[name] = app
		h.test.mu.Unlock()
	}
	// Without fail, the answer does not change (App:show only attaches).
	if fail, ok := args["fail"].(string); ok {
		app.mu.Lock()
		app.fail = fail
		app.mu.Unlock()
	}
	list, _ := args["sessions"].([]any)
	for _, s := range list {
		id, _ := s.(string)
		if err := app.r.Call(ctx, "session.attach", map[string]any{"session_id": id, "event_scope": "session"}, nil); err != nil {
			return nil, err
		}
	}
	return map[string]any{"name": name}, nil
}

// serve answers and records each client.perform until the connection ends.
func (a *testFakeApp) serve() {
	for raw := range a.r.Events {
		var env struct {
			Payload struct {
				Type    string `json:"type"`
				Payload struct {
					CallID string         `json:"call_id"`
					Action string         `json:"action"`
					Args   map[string]any `json:"args"`
				} `json:"payload"`
			} `json:"payload"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Payload.Type != "client_perform_requested" {
			continue
		}
		ev := env.Payload.Payload
		a.mu.Lock()
		a.calls = append(a.calls, map[string]any{"action": ev.Action, "args": ev.Args})
		fail := a.fail
		a.mu.Unlock()
		reply := map[string]any{"call_id": ev.CallID, "result": map[string]any{}}
		if fail != "" {
			reply = map[string]any{"call_id": ev.CallID, "error": &rex.RexError{Code: fail, Message: "fake app error"}}
		}
		a.r.Call(context.Background(), "client.reply", reply, nil)
	}
}

func (h *Hub) testAppCalls(args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	h.test.mu.Lock()
	app := h.test.apps[name]
	h.test.mu.Unlock()
	if app == nil {
		return nil, bad("invalid: no fake app %q", name)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	calls := append([]map[string]any{}, app.calls...)
	return map[string]any{"calls": calls}, nil
}

func (h *Hub) testAppClose(args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	h.test.mu.Lock()
	app := h.test.apps[name]
	delete(h.test.apps, name)
	h.test.mu.Unlock()
	if app == nil {
		return nil, bad("invalid: no fake app %q", name)
	}
	app.r.Close()
	return map[string]any{}, nil
}
