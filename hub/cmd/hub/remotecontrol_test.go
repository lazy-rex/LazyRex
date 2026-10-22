package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// The app's refusal as the hub gets it: a RexError that rex.Call wraps with the method name.
var appRefusal = fmt.Errorf("client.perform: %w", &rex.RexError{Code: "refused",
	Message: "Rex.app is not accepting actions. Turn on Remote Control in its Settings under Rex Server."})

func TestRemoteControlState(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"the app ran it", nil, "on"},
		{"the refusal, wrapped as rex.Call does", appRefusal, "off"},
		{"the refusal, not wrapped", &rex.RexError{Code: "refused", Message: "Rex.app is not accepting actions."}, "off"},
		{"refused, another message", fmt.Errorf("client.perform: %w", &rex.RexError{Code: "refused", Message: "no grant"}), ""},
		{"another code, the same message", &rex.RexError{Code: "forbidden", Message: "Rex.app is not accepting actions."}, ""},
		{"a timeout", fmt.Errorf("client.perform: %w", &rex.RexError{Code: "timeout", Message: "no answer"}), ""},
		{"no_app", refuse("no_app", "no Rex app shows this session"), ""},
		{"a plain error with the text", errors.New("refused: Rex.app is not accepting actions"), ""},
	} {
		if got := remoteControlState(c.err); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// failingApp is fakeApp with a chosen client.perform result.
type failingApp struct {
	fakeApp
	err error
}

func (f *failingApp) call(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
	if err := f.fakeApp.call(h, ctx, method, args, out); err != nil || method != "client.perform" {
		return err
	}
	return f.err
}

// The record sites: only the result of the client.perform to the app counts.
func TestRemoteControlRecordSites(t *testing.T) {
	f := &failingApp{fakeApp: fakeApp{focused: "form-block", clients: oneApp}}
	h, p := appActionHub(t, &f.fakeApp)
	appCall = f.call
	ctx := context.Background()
	obs := func() (rcObservation, bool) {
		h.rc.mu.Lock()
		defer h.rc.mu.Unlock()
		o, ok := h.rc.seen["app-1"]
		return o, ok
	}
	insert := map[string]any{"view_id": "view-form-1", "attachment": "att-1", "action": "pane.insert_path"}

	// session.view and client.list answered, then a refusal before the perform: no record.
	f.focused = "tab-block"
	if _, err := h.viewAppAction(ctx, p, insert); !isRefusal(err, "not_focused") {
		t.Fatalf("not focused: %v", err)
	}
	if _, ok := obs(); ok {
		t.Fatal("a refusal before the perform recorded an observation")
	}
	f.focused = "form-block"

	f.err = appRefusal
	if _, err := h.viewAppAction(ctx, p, insert); err == nil {
		t.Fatal("the refusal did not fail the action")
	}
	o, ok := obs()
	if !ok || o.state != "off" || o.action != "pane.insert_path" {
		t.Fatalf("after the refusal: %+v %v", o, ok)
	}

	// An error that says nothing about the setting keeps the observation and its time.
	f.err = fmt.Errorf("client.perform: %w", &rex.RexError{Code: "timeout", Message: "no answer"})
	h.viewAppAction(ctx, p, insert)
	if o2, _ := obs(); o2 != o {
		t.Fatalf("a timeout changed the observation: %+v, was %+v", o2, o)
	}

	f.err = nil
	if _, err := h.viewAppAction(ctx, p, insert); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if o, _ := obs(); o.state != "on" || o.action != "pane.insert_path" {
		t.Fatalf("after a run: %+v", o)
	}

	// hub.app.select_session records too.
	g := &failingApp{fakeApp: fakeApp{focused: "form-block", clients: oneApp}, err: appRefusal}
	hs, ps := selectHub(t, &g.fakeApp)
	appCall = g.call
	sel := map[string]any{"view_id": "view-start", "session_id": "s2", "window_id": "w-new"}
	hs.appSelectSession(ctx, ps, sel)
	if o := hs.rc.seen["app-1"]; o.state != "off" || o.action != "session.select" {
		t.Fatalf("after a refused select: %+v", o)
	}
	g.err = nil
	if _, err := hs.appSelectSession(ctx, ps, sel); err != nil {
		t.Fatalf("select: %v", err)
	}
	if o := hs.rc.seen["app-1"]; o.state != "on" {
		t.Fatalf("after a select: %+v", o)
	}
}

// listCall answers client.list with a fixed reply (or an error) and runs during() inside the call.
func listCall(reply string, err error, during func()) func(*Hub, context.Context, string, map[string]any, any) error {
	return func(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
		if method != "client.list" {
			return fmt.Errorf("unexpected %s", method)
		}
		if during != nil {
			during()
		}
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(reply), out)
	}
}

func readApps(t *testing.T, h *Hub) []map[string]any {
	t.Helper()
	out, err := h.remoteControlRead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var r struct {
		Apps []map[string]any `json:"apps"`
	}
	json.Unmarshal(b, &r)
	return r.Apps
}

func TestRemoteControlRead(t *testing.T) {
	orig := appCall
	t.Cleanup(func() { appCall = orig })
	h := &Hub{}
	h.observeApp("app-on", "session.select", nil)
	h.observeApp("app-kept", "pane.insert_path", appRefusal)
	h.observeApp("app-gone", "session.select", nil)
	kept := h.rc.seen["app-kept"]

	list := `{"clients":[
	 {"client_id":"app-on","info":{"kind":"app","label":"Rex Beta","name":"Rex"},"connection_state":"connected"},
	 {"client_id":"app-new","info":{"kind":"app","name":"Rex"},"connection_state":"connected"},
	 {"client_id":"app-kept","info":{"kind":"app","label":"Rex Beta"},"connection_state":"reconnecting"},
	 {"client_id":"hub","info":{"kind":"other","label":"lazyrex-hub"},"connection_state":"connected"}]}`
	appCall = listCall(list, nil, nil)
	apps := readApps(t, h)
	got, _ := json.Marshal(apps)
	if len(apps) != 2 || apps[0]["client_id"] != "app-on" || apps[0]["state"] != "on" || apps[0]["label"] != "Rex Beta" ||
		apps[0]["action"] != "session.select" || apps[0]["at"] == nil ||
		apps[1]["client_id"] != "app-new" || apps[1]["state"] != "unknown" || apps[1]["label"] != "Rex" ||
		apps[1]["action"] != nil || apps[1]["at"] != nil {
		t.Fatalf("apps: %s", got)
	}
	// A client that Rex keeps while it reconnects keeps its observation (not listed); an absent
	// client loses it.
	if o, ok := h.rc.seen["app-kept"]; !ok || o != kept {
		t.Errorf("the reconnecting app lost its observation: %+v %v", o, ok)
	}
	if _, ok := h.rc.seen["app-gone"]; ok {
		t.Error("the absent client kept its observation")
	}
	// The same client ID back as connected: the observation and its original time.
	appCall = listCall(strings.Replace(list, `"reconnecting"`, `"connected"`, 1), nil, nil)
	for _, a := range readApps(t, h) {
		if a["client_id"] == "app-kept" && (a["state"] != "off" || a["at"] != kept.at.Format(time.RFC3339)) {
			t.Errorf("after the reconnect: %v", a)
		}
	}

	appCall = listCall(`{"clients":[{"client_id":"hub","info":{"kind":"other"},"connection_state":"connected"}]}`, nil, nil)
	if apps := readApps(t, h); apps == nil || len(apps) != 0 {
		t.Errorf("no connected app: %v", apps)
	}
	appCall = listCall("", errors.New("client.list: lost"), nil)
	if out, err := h.remoteControlRead(context.Background()); err == nil {
		t.Errorf("a failed client.list gave %v", out)
	}
}

// A read prunes only observations that it saw before its client.list: one recorded during the
// call stays, though the list (taken before it) does not name its client.
func TestRemoteControlReadKeepsARecordDuringTheCall(t *testing.T) {
	orig := appCall
	t.Cleanup(func() { appCall = orig })
	h := &Hub{}
	h.observeApp("app-old", "session.select", nil)
	appCall = listCall(`{"clients":[]}`, nil, func() {
		h.observeApp("app-new", "session.select", appRefusal)
		h.observeApp("app-old", "pane.insert_path", appRefusal) // a newer record of a client the read saw
	})
	readApps(t, h)
	if o, ok := h.rc.seen["app-new"]; !ok || o.state != "off" {
		t.Errorf("the record during the call was pruned: %+v %v", o, ok)
	}
	if o, ok := h.rc.seen["app-old"]; !ok || o.action != "pane.insert_path" {
		t.Errorf("the newer record of a seen client was pruned: %+v %v", o, ok)
	}
	appCall = listCall(`{"clients":[]}`, nil, nil)
	readApps(t, h)
	if len(h.rc.seen) != 0 {
		t.Errorf("the next read kept %v", h.rc.seen)
	}
}

func TestRemoteControlLines(t *testing.T) {
	// An observation from an earlier day: the line keeps its date and zone (it is historical).
	at := time.Date(2026, 10, 19, 20, 5, 0, 0, time.UTC)
	local := at.Local().Format(time.RFC3339)
	app := func(id, label, state, action string) string {
		a := map[string]any{"client_id": id, "label": label, "state": state}
		if action != "" {
			a["action"], a["at"] = action, at.Format(time.RFC3339)
		}
		b, _ := json.Marshal(a)
		return string(b)
	}
	for _, c := range []struct {
		name string
		raw  string
		err  error
		want []string
	}{
		{"on", `{"apps":[` + app("c1", "Rex Beta", "on", "session.select") + `]}`, nil,
			[]string{"Remote Control (app Rex Beta c1): last observed on at " + local + " (session.select)"}},
		{"off", `{"apps":[` + app("c1", "", "off", "pane.insert_path") + `]}`, nil,
			[]string{"Remote Control (app c1): last observed off at " + local + " (pane.insert_path refused). Turn it on in the Rex app: Settings, Rex Server, Remote Control."}},
		{"unknown", `{"apps":[` + app("c1", "Rex Beta", "unknown", "") + `]}`, nil,
			[]string{"Remote Control (app Rex Beta c1): unknown: no retained app action observation for this app"}},
		{"two apps with one label", `{"apps":[` + app("c1", "Rex Beta", "on", "session.select") + `,` + app("c2", "Rex Beta", "unknown", "") + `]}`, nil,
			[]string{"(app Rex Beta c1): last observed on", "(app Rex Beta c2): unknown"}},
		{"no app", `{"apps":[]}`, nil, []string{"Remote Control: no Rex app is connected"}},
		{"no list", `{}`, nil, []string{"Remote Control: unknown: hub.app.remote_control has no apps list"}},
		{"bad JSON", `{`, nil, []string{"Remote Control: unknown: hub.app.remote_control:"}},
		{"an unknown state", `{"apps":[` + app("c1", "", "maybe", "") + `]}`, nil, []string{`Remote Control: unknown: app c1: state "maybe"`}},
		{"an older hub", ``, errors.New(`exit status 1: "lazyrex-hub" does not offer "hub.app.remote_control"`), []string{"Remote Control: unknown: exit status 1:"}},
		{"pane-safe", `{"apps":[` + app("c1", "Rex\u001b]0;x\u0007", "on", "session.select") + `]}`, nil, []string{"(app Rex?]0;x? c1)"}},
	} {
		got := remoteControlLines([]byte(c.raw), c.err)
		if len(got) != len(c.want) {
			t.Errorf("%s: %q", c.name, got)
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: line %d %q has no %q", c.name, i, got[i], w)
			}
		}
	}
}
