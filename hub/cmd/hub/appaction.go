package main

// hub.view.app_action {view_id, attachment, action}: a form or panel asks the Rex app to run one
// app action for it, for example the app's directory browser for Start Agent….
// Observed on the real app (docs/rex-protocol.md, "App action Insert Path…"): pane.insert_path
// opens the app's directory browser where the user is and types the chosen path into the pane
// that has focus in the app. So the hub runs it only when:
//   - the caller is a bound, open form or panel renderer (the view's own pane), and
//   - that pane is the focused pane of its session's active window (the user is in the form), and
//   - exactly one app client shows that session. None: "no_app". More: "ambiguous_app" (the hub
//     cannot know which device the user is on). The form then keeps its in-form folder list.

import (
	"context"
	"encoding/json"
	"errors"
)

// appActions are the app actions a view may ask for.
var appActions = map[string]bool{"pane.insert_path": true}

// appCall sends one Rex call for the app actions below. Tests replace it (no app runs in a lab).
var appCall = func(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
	return h.rex.Call(ctx, method, args, out)
}

// appTimeoutMS is the timeout_ms of a client.perform to the app.
const appTimeoutMS = 5000

// appPerform sends one client.perform to the app client and waits timeout_ms + performMargin
// (perform.go). A cut by that wait is an unknown outcome: Rex can still deliver the call, and the
// app can still run the action (open the browser, switch the session). So the caller's error says
// that the request may still apply, and the hub never sends it again by itself. observeApp sees
// the call's own error.
func appPerform(h *Hub, ctx context.Context, app, action string, args map[string]any) error {
	cctx, cancel := performCtx(ctx, appTimeoutMS)
	defer cancel()
	err := appCall(h, cctx, "client.perform", map[string]any{"client_id": app, "action": action, "args": args, "timeout_ms": appTimeoutMS}, nil)
	h.observeApp(app, action, err)
	if errors.Is(err, context.DeadlineExceeded) {
		return unknownOutcome(ctx, "the app", appTimeoutMS)
	}
	return err
}

func (h *Hub) viewAppAction(ctx context.Context, p perform, args map[string]any) (any, error) {
	v, err := h.boundCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	action, _ := args["action"].(string)
	if !appActions[action] {
		return nil, bad("action %q is not allowed", action)
	}
	var tab, sess, block string
	h.db.QueryRowContext(ctx, `SELECT tab, session_id, block_id FROM views WHERE view_id = ?`, v.ID).Scan(&tab, &sess, &block)
	if tab != formTab || sess == "" || block == "" {
		return nil, bad("only a form or panel can ask for an app action")
	}
	app, err := h.userApp(ctx, sess, block)
	if err != nil {
		return nil, err
	}
	// The app opens the browser and answers at once; the path arrives later as typed input.
	if err := appPerform(h, ctx, app, action, map[string]any{}); err != nil {
		return nil, err
	}
	h.ev.emit("view_app_action", map[string]any{"view_id": v.ID, "action": action, "client_id": app})
	return map[string]any{"status": "opened"}, nil
}

// userApp returns the one app client where the user is in the view's pane: the pane is the
// focused pane of its session's active window, and exactly one connected app shows that session.
func (h *Hub) userApp(ctx context.Context, sess, block string) (string, error) {
	var sv struct {
		ActiveWindowID string `json:"active_window_id"`
		Windows        []struct {
			WindowID       string `json:"window_id"`
			FocusedBlockID string `json:"focused_block_id"`
		} `json:"windows"`
	}
	if err := appCall(h, ctx, "session.view", map[string]any{"session_id": sess}, &sv); err != nil {
		return "", err
	}
	focused := ""
	for _, w := range sv.Windows {
		if w.WindowID == sv.ActiveWindowID {
			focused = w.FocusedBlockID
		}
	}
	if focused != block {
		return "", refuse("not_focused", "the form does not have the focus")
	}
	apps, err := h.sessionApps(ctx, sess)
	if err != nil {
		return "", err
	}
	switch len(apps) {
	case 0:
		return "", refuse("no_app", "no Rex app shows this session")
	case 1:
		return apps[0], nil
	default:
		return "", refuse("ambiguous_app", "more than one Rex app shows this session")
	}
}

// sessionApps lists the connected app clients that show sess.
func (h *Hub) sessionApps(ctx context.Context, sess string) ([]string, error) {
	var cl struct {
		Clients []struct {
			ClientID   string          `json:"client_id"`
			Info       json.RawMessage `json:"info"`
			State      string          `json:"connection_state"`
			SessionIDs []string        `json:"session_ids"`
		} `json:"clients"`
	}
	if err := appCall(h, ctx, "client.list", map[string]any{}, &cl); err != nil {
		return nil, err
	}
	var apps []string
	for _, c := range cl.Clients {
		var info struct {
			Kind string `json:"kind"`
		}
		json.Unmarshal(c.Info, &info)
		if info.Kind != "app" || c.State != "connected" {
			continue
		}
		for _, s := range c.SessionIDs {
			if s == sess {
				apps = append(apps, c.ClientID)
				break
			}
		}
	}
	return apps, nil
}

// hub.app.select_session {view_id, session_id, window_id} switches the app to a session, for
// example after a template start into a new session. A plugin asks for it from one of its own open
// forms. The hub runs the app action session.select ("Go to Session", listed by the real app
// on Rex Beta 0.1.0 (1203); its effect is untested) only when the user is in that form
// (userApp). The target must be another session, and the window must be in it. The hub does not
// make the call idempotent: the plugin records the attempt first and never asks twice.
func (h *Hub) appSelectSession(ctx context.Context, p perform, args map[string]any) (any, error) {
	pl := h.pluginByClient(p.Requester.ClientID)
	if pl == nil {
		return nil, bad("only a registered plugin may switch the app")
	}
	for k := range args {
		if k != "view_id" && k != "session_id" && k != "window_id" {
			return nil, bad("unknown field %q", k)
		}
	}
	viewID, _ := args["view_id"].(string)
	target, _ := args["session_id"].(string)
	window, _ := args["window_id"].(string)
	if viewID == "" || target == "" || window == "" {
		return nil, bad("view_id, session_id and window_id are required")
	}
	var tab, sess, block string
	h.db.QueryRowContext(ctx, `SELECT tab, session_id, block_id FROM views WHERE view_id = ? AND plugin = ? AND status = 'open'`, viewID, pl.Name).Scan(&tab, &sess, &block)
	if tab != formTab || sess == "" || block == "" {
		return nil, bad("the view is not an open form of this plugin")
	}
	if target == sess {
		return nil, bad("the target is the form's own session")
	}
	var tv struct {
		Windows []struct {
			WindowID string `json:"window_id"`
		} `json:"windows"`
	}
	if err := appCall(h, ctx, "session.view", map[string]any{"session_id": target}, &tv); err != nil {
		return nil, err
	}
	found := false
	for _, w := range tv.Windows {
		found = found || w.WindowID == window
	}
	if !found {
		return nil, bad("window %s is not in session %s", window, target)
	}
	app, err := h.userApp(ctx, sess, block)
	if err != nil {
		return nil, err
	}
	if err := appPerform(h, ctx, app, "session.select", map[string]any{"session_id": target, "window_id": window}); err != nil {
		return nil, err
	}
	h.ev.emit("app_select_session", map[string]any{"view_id": viewID, "plugin": pl.Name, "session_id": target, "window_id": window, "client_id": app})
	return map[string]any{"status": "selected"}, nil
}
