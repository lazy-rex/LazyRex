package main

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeApp answers the three Rex calls of hub.view.app_action and records the performs.
type fakeApp struct {
	focused  string
	clients  string
	performs []map[string]any
}

func (f *fakeApp) call(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
	switch method {
	case "session.view":
		if args["session_id"] == "s2" {
			return json.Unmarshal([]byte(`{"active_window_id":"w9","windows":[{"window_id":"w-new","focused_block_id":"b-new"}]}`), out)
		}
		return json.Unmarshal([]byte(`{"active_window_id":"w1","windows":[{"window_id":"w0","focused_block_id":"form-block"},{"window_id":"w1","focused_block_id":"`+f.focused+`"}]}`), out)
	case "client.list":
		return json.Unmarshal([]byte(f.clients), out)
	case "client.perform":
		f.performs = append(f.performs, args)
	}
	return nil
}

func appActionHub(t *testing.T, f *fakeApp) (*Hub, perform) {
	t.Helper()
	h, _ := workerTestHub(t)
	ctx := context.Background()
	if _, err := h.db.Exec(uiSchema); err != nil {
		t.Fatal(err)
	}
	if err := formsStart(ctx, h.db); err != nil {
		t.Fatal(err)
	}
	h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, session_id, block_id, client_id, attachment) VALUES
		('view-form-1', 'profiles', ?, '/forms/start', 'open', 's1', 'form-block', 'renderer', 'att-1'),
		('view-tab-1', 'profiles', 'home', '/', 'open', 's1', 'tab-block', 'renderer', 'att-2')`, formTab)
	orig := appCall
	appCall = f.call
	t.Cleanup(func() { appCall = orig })
	var p perform
	p.Requester.ClientID = "renderer"
	return h, p
}

const oneApp = `{"clients":[
 {"client_id":"app-1","info":{"kind":"app"},"connection_state":"connected","session_ids":["s0","s1"]},
 {"client_id":"other","info":{},"connection_state":"connected","session_ids":["s1"]},
 {"client_id":"app-gone","info":{"kind":"app"},"connection_state":"disconnected","session_ids":["s1"]}]}`

func TestViewAppActionRunsOnTheOneApp(t *testing.T) {
	f := &fakeApp{focused: "form-block", clients: oneApp}
	h, p := appActionHub(t, f)
	out, err := h.viewAppAction(context.Background(), p, map[string]any{"view_id": "view-form-1", "attachment": "att-1", "action": "pane.insert_path"})
	if err != nil || out.(map[string]any)["status"] != "opened" {
		t.Fatalf("%v %v", out, err)
	}
	if len(f.performs) != 1 || f.performs[0]["client_id"] != "app-1" || f.performs[0]["action"] != "pane.insert_path" {
		t.Fatalf("performs: %v", f.performs)
	}
}

func TestViewAppActionRefusals(t *testing.T) {
	two := `{"clients":[{"client_id":"a","info":{"kind":"app"},"connection_state":"connected","session_ids":["s1"]},
		{"client_id":"b","info":{"kind":"app"},"connection_state":"connected","session_ids":["s1"]}]}`
	none := `{"clients":[{"client_id":"a","info":{"kind":"app"},"connection_state":"connected","session_ids":["s0"]}]}`
	for _, c := range []struct {
		name, focused, clients, view, att, action, code string
	}{
		{"not focused", "tab-block", oneApp, "view-form-1", "att-1", "pane.insert_path", "not_focused"},
		{"no app", "form-block", none, "view-form-1", "att-1", "pane.insert_path", "no_app"},
		{"two apps", "form-block", two, "view-form-1", "att-1", "pane.insert_path", "ambiguous_app"},
		{"other action", "form-block", oneApp, "view-form-1", "att-1", "session.close", ""},
		{"not a form", "tab-block", oneApp, "view-tab-1", "att-2", "pane.insert_path", ""},
		{"stale attachment", "form-block", oneApp, "view-form-1", "old", "pane.insert_path", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeApp{focused: c.focused, clients: c.clients}
			h, p := appActionHub(t, f)
			_, err := h.viewAppAction(context.Background(), p, map[string]any{"view_id": c.view, "attachment": c.att, "action": c.action})
			if err == nil || len(f.performs) != 0 {
				t.Fatalf("must refuse with no perform: %v %v", err, f.performs)
			}
			if c.code != "" && codeOf(err) != c.code {
				t.Fatalf("code %q, want %q (%v)", codeOf(err), c.code, err)
			}
		})
	}
}

// selectHub adds the templates plugin, with an open form in s1, to appActionHub.
func selectHub(t *testing.T, f *fakeApp) (*Hub, perform) {
	h, _ := appActionHub(t, f)
	h.plugins = map[string]*Plugin{"templates": {Name: "templates", ClientID: "templates-client"}, "profiles": {Name: "profiles", ClientID: "profiles-client"}}
	h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, session_id, block_id, client_id, attachment) VALUES
		('view-start', 'templates', ?, '/forms/start', 'open', 's1', 'form-block', 'renderer', 'att-3'),
		('view-start-closed', 'templates', ?, '/forms/start', 'closed', 's1', 'form-block', 'renderer', 'att-4')`, formTab, formTab)
	var p perform
	p.Requester.ClientID = "templates-client"
	return h, p
}

func TestAppSelectSessionRunsOnTheOneApp(t *testing.T) {
	f := &fakeApp{focused: "form-block", clients: oneApp}
	h, p := selectHub(t, f)
	out, err := h.appSelectSession(context.Background(), p, map[string]any{"view_id": "view-start", "session_id": "s2", "window_id": "w-new"})
	if err != nil || out.(map[string]any)["status"] != "selected" {
		t.Fatalf("%v %v", out, err)
	}
	if len(f.performs) != 1 || f.performs[0]["client_id"] != "app-1" || f.performs[0]["action"] != "session.select" {
		t.Fatalf("performs: %v", f.performs)
	}
	if a := f.performs[0]["args"].(map[string]any); a["session_id"] != "s2" || a["window_id"] != "w-new" {
		t.Fatalf("args: %v", a)
	}
}

func TestAppSelectSessionRefusals(t *testing.T) {
	two := `{"clients":[{"client_id":"a","info":{"kind":"app"},"connection_state":"connected","session_ids":["s1"]},
		{"client_id":"b","info":{"kind":"app"},"connection_state":"connected","session_ids":["s1"]}]}`
	// An app that shows only the new session does not show the user's form.
	elsewhere := `{"clients":[{"client_id":"a","info":{"kind":"app"},"connection_state":"connected","session_ids":["s2"]}]}`
	for _, c := range []struct {
		name, caller, focused, clients, view, session, window, extra, code string
	}{
		{"not focused", "templates-client", "tab-block", oneApp, "view-start", "s2", "w-new", "", "not_focused"},
		{"no app", "templates-client", "form-block", elsewhere, "view-start", "s2", "w-new", "", "no_app"},
		{"two apps", "templates-client", "form-block", two, "view-start", "s2", "w-new", "", "ambiguous_app"},
		{"existing session target", "templates-client", "form-block", oneApp, "view-start", "s1", "w1", "", ""},
		{"window of another session", "templates-client", "form-block", oneApp, "view-start", "s2", "w1", "", ""},
		{"closed form", "templates-client", "form-block", oneApp, "view-start-closed", "s2", "w-new", "", ""},
		{"another plugin's form", "profiles-client", "form-block", oneApp, "view-start", "s2", "w-new", "", ""},
		{"not a form", "profiles-client", "tab-block", oneApp, "view-tab-1", "s2", "w-new", "", ""},
		{"not a plugin", "renderer", "form-block", oneApp, "view-start", "s2", "w-new", "", ""},
		{"unknown field", "templates-client", "form-block", oneApp, "view-start", "s2", "w-new", "focus", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeApp{focused: c.focused, clients: c.clients}
			h, p := selectHub(t, f)
			p.Requester.ClientID = c.caller
			args := map[string]any{"view_id": c.view, "session_id": c.session, "window_id": c.window}
			if c.extra != "" {
				args[c.extra] = true
			}
			_, err := h.appSelectSession(context.Background(), p, args)
			if err == nil || len(f.performs) != 0 {
				t.Fatalf("must refuse with no perform: %v %v", err, f.performs)
			}
			if c.code != "" && codeOf(err) != c.code {
				t.Fatalf("code %q, want %q (%v)", codeOf(err), c.code, err)
			}
		})
	}
}
