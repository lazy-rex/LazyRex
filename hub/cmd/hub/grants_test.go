package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Grant checks that need no Rex: entry validation, ownership, state and exact matching. The real
// path (create during a forward, spawn after it, restart, migration) runs in the harness
// (tests/harness/tests/grants.lua).
func TestGrantEntriesValidation(t *testing.T) {
	layout := map[string]any{"leaf": map[string]any{"profile": "shell"}}
	entry := func(m map[string]any) []any { return []any{m} }
	for name, raw := range map[string]any{
		"empty":          []any{},
		"not a list":     map[string]any{},
		"wrong action":   entry(map[string]any{"action": "hub.stop", "key": "k", "request": map[string]any{"layout": layout}}),
		"no key":         entry(map[string]any{"action": "hub.spawn_window", "key": "", "request": map[string]any{"layout": layout}}),
		"extra field":    entry(map[string]any{"action": "hub.spawn_window", "key": "k", "request": map[string]any{"layout": layout}, "x": 1}),
		"for inside":     entry(map[string]any{"action": "hub.spawn_window", "key": "k", "request": map[string]any{"layout": layout, "for": "user"}}),
		"placement":      entry(map[string]any{"action": "hub.spawn_window", "key": "k", "request": map[string]any{"layout": layout, "placement": map[string]any{}}}),
		"grant inside":   entry(map[string]any{"action": "hub.spawn_window", "key": "k", "request": map[string]any{"layout": layout, "grant_id": "g"}}),
		"bad session":    entry(map[string]any{"action": "hub.spawn_window", "key": "k", "request": map[string]any{"layout": layout, "session": map[string]any{"x": "y"}}}),
		"bad layout":     entry(map[string]any{"action": "hub.spawn_window", "key": "k", "request": map[string]any{"layout": map[string]any{"leaf": map[string]any{"profile": "shell", "x": 1}}}}),
		"no layout":      entry(map[string]any{"action": "hub.spawn_window", "key": "k", "request": map[string]any{"title": "T"}}),
		"duplicate keys": []any{map[string]any{"action": "hub.spawn_window", "key": "k", "request": map[string]any{"layout": layout}}, map[string]any{"action": "hub.spawn_window", "key": "k", "request": map[string]any{"layout": layout}}},
	} {
		if _, err := grantEntries(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := grantEntries([]any{map[string]any{"action": "hub.spawn_window", "key": "k",
		"request": map[string]any{"layout": layout, "title": "T", "session": map[string]any{"ensure_key": "e"}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestGrantCallerMatching(t *testing.T) {
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(grantsSchema + ensuredSchema); err != nil {
		t.Fatal(err)
	}
	h.plugins = map[string]*Plugin{"templates": {Name: "templates", ClientID: "client:t"}, "other": {Name: "other", ClientID: "client:o"}}
	layout := map[string]any{"split": map[string]any{"direction": "horizontal", "ratio": 0.3,
		"before": map[string]any{"leaf": map[string]any{"profile": "shell", "cwd": "/tmp"}},
		"after":  map[string]any{"leaf": map[string]any{"profile": "task", "args": map[string]any{"task": "a"}}}}}
	entries, err := grantEntries([]any{
		map[string]any{"action": "hub.spawn_window", "key": "t/1", "request": map[string]any{"session": map[string]any{"ensure_key": "s1"}, "title": "Dev", "layout": layout}},
		map[string]any{"action": "hub.spawn_window", "key": "t/2", "request": map[string]any{"session": "session:fixed", "layout": layout}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := json.Marshal(entries)
	h.db.Exec(`INSERT INTO grants (plugin, key, grant_id, agent, entries, state, created) VALUES ('templates', 'run/1', 'grant-a', 'alice', ?, 'active', 'x')`, string(stored))
	from := func(client string) perform {
		var p perform
		p.Requester.ClientID = client
		return p
	}
	// call builds the args as a plugin sends them (a JSON round trip).
	call := func(client string, args map[string]any) (string, error) {
		raw, _ := json.Marshal(args)
		var m map[string]any
		json.Unmarshal(raw, &m)
		return h.grantCaller(context.Background(), from(client), "hub.spawn_window", m)
	}
	args := func(key string, session any, title string, l any) map[string]any {
		m := map[string]any{"grant_id": "grant-a", "key": key, "layout": l}
		if session != nil {
			m["session"] = session
		}
		if title != "" {
			m["title"] = title
		}
		return m
	}
	refused := func(name string, client string, a map[string]any, want string) {
		t.Helper()
		if _, err := call(client, a); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", name, err, want)
		}
	}
	// The ensure reference matches only a done session of this plugin and key.
	refused("session not made", "client:t", args("t/1", "session:new", "Dev", layout), "not made yet")
	h.db.Exec(`INSERT INTO ui_ensured VALUES ('templates', 's1', 'n', 'sending', '')`)
	refused("session sending", "client:t", args("t/1", "session:new", "Dev", layout), "not made yet")
	h.db.Exec(`UPDATE ui_ensured SET state = 'done', session_id = 'session:new'`)
	if who, err := call("client:t", args("t/1", "session:new", "Dev", layout)); err != nil || who != "alice" {
		t.Fatalf("exact match: %q %v", who, err)
	}
	if who, err := call("client:t", args("t/2", "session:fixed", "", layout)); err != nil || who != "alice" {
		t.Fatalf("fixed session: %q %v", who, err)
	}
	refused("other session", "client:t", args("t/1", "session:other", "Dev", layout), "not the granted request")
	refused("changed title", "client:t", args("t/1", "session:new", "Dev2", layout), "not the granted request")
	refused("missing title", "client:t", args("t/1", "session:new", "", layout), "not the granted request")
	changed := map[string]any{"split": map[string]any{"direction": "horizontal", "ratio": 0.4,
		"before": layout["split"].(map[string]any)["before"], "after": layout["split"].(map[string]any)["after"]}}
	refused("changed layout", "client:t", args("t/1", "session:new", "Dev", changed), "not the granted request")
	otherArgs := map[string]any{"split": map[string]any{"direction": "horizontal", "ratio": 0.3,
		"before": layout["split"].(map[string]any)["before"],
		"after":  map[string]any{"leaf": map[string]any{"profile": "task", "args": map[string]any{"task": "b"}}}}}
	refused("changed args", "client:t", args("t/1", "session:new", "Dev", otherArgs), "not the granted request")
	refused("other key", "client:t", args("t/3", "session:new", "Dev", layout), "does not cover")
	withFor := args("t/1", "session:new", "Dev", layout)
	withFor["for"] = "user"
	refused("with for", "client:t", withFor, "cannot be combined")
	withPlacement := args("t/1", "session:new", "Dev", layout)
	withPlacement["placement"] = map[string]any{}
	refused("with placement", "client:t", withPlacement, "cannot be combined")
	refused("another plugin", "client:o", args("t/1", "session:new", "Dev", layout), "no grant")
	refused("not a plugin", "client:x", args("t/1", "session:new", "Dev", layout), "only a registered plugin")
	if _, err := h.grantCaller(context.Background(), from("client:t"), "hub.spawn", args("t/1", "session:new", "Dev", layout)); err == nil {
		t.Fatal("another action was accepted")
	}
	// Revoke: idempotent, then every call is refused.
	for i := 0; i < 2; i++ {
		out, err := h.grantRevoke(context.Background(), "templates", map[string]any{"grant_id": "grant-a"})
		if err != nil || out.(map[string]any)["state"] != "revoked" {
			t.Fatalf("revoke %d: %v %v", i, out, err)
		}
	}
	if _, err := h.grantRevoke(context.Background(), "other", map[string]any{"grant_id": "grant-a"}); err == nil {
		t.Fatal("another plugin revoked the grant")
	}
	refused("revoked", "client:t", args("t/1", "session:new", "Dev", layout), "revoked")
	got, _ := h.grantGet(context.Background(), "templates", map[string]any{"key": "run/1"})
	if m := got.(map[string]any); m["found"] != true || m["state"] != "revoked" || m["agent"] != "alice" {
		t.Fatalf("get: %#v", m)
	}
	if got, _ := h.grantGet(context.Background(), "other", map[string]any{"key": "run/1"}); got.(map[string]any)["found"] != false {
		t.Fatal("another plugin sees the grant")
	}
}

func TestGrantCreateRefusals(t *testing.T) {
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(grantsSchema); err != nil {
		t.Fatal(err)
	}
	h.plugins = map[string]*Plugin{"templates": {Name: "templates", ClientID: "client:t"}}
	var p perform
	p.Requester.ClientID = "client:t"
	entries := []any{map[string]any{"action": "hub.spawn_window", "key": "k", "request": map[string]any{"layout": map[string]any{"leaf": map[string]any{"profile": "shell"}}}}}
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"no on_behalf":  {map[string]any{"key": "g", "entries": entries}, "on_behalf is required"},
		"no forward":    {map[string]any{"on_behalf": float64(7), "key": "g", "entries": entries}, "no agent call"},
		"unknown field": {map[string]any{"on_behalf": float64(7), "key": "g", "entries": entries, "for": "user"}, "unknown field"},
		"no key":        {map[string]any{"on_behalf": float64(7), "entries": entries}, "key must be"},
	} {
		if _, err := h.grantCreate(context.Background(), p, "templates", c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}
