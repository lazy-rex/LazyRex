package main

import "testing"

// The plugin lifecycle events (docs/hub.md): for one instance_id, plugin_spawned and
// plugin_registered come in either order. codex-26's set lab at 1e6350aa (Linux, OLD hub
// 25f8c7db) wrote plugin_registered of lazyrex-8-e5753c28, then its plugin_spawned 4 ms later, and
// registeredNow refused that instance until the verify deadline. A spawn of another instance and
// a stop still end its registration, and an exit of the instance ends it for good (hub lead).
func TestReleaseRegisteredNowOrder(t *testing.T) {
	ev := func(event, instance string) map[string]any {
		m := map[string]any{"event": event, "plugin": "lazyrex"}
		if instance != "" {
			m["instance_id"] = instance
		}
		return m
	}
	// events.jsonl lines 330-331 of the kept lab /tmp/rexlab-release-t1944626set (Linux).
	lab := []map[string]any{
		{"at": "2026-10-23T06:24:11.099889106Z", "event": "plugin_registered", "plugin": "lazyrex", "instance_id": "lazyrex-8-e5753c28", "client_id": "client:lab"},
		{"at": "2026-10-23T06:24:11.103767988Z", "event": "plugin_spawned", "plugin": "lazyrex", "instance_id": "lazyrex-8-e5753c28", "block_id": "block:lab"},
	}
	yes := true
	cur := func(id string) relEntryState {
		return relEntryState{Name: "lazyrex", State: "running", InstanceID: id, Registered: &yes}
	}
	for _, c := range []struct {
		name   string
		e      relEntryState
		events []map[string]any
		want   bool
	}{
		{"the lab order: registered, then spawned, same instance", cur("lazyrex-8-e5753c28"), lab, true},
		{"registered X, spawned X", cur("X"), []map[string]any{ev("plugin_registered", "X"), ev("plugin_spawned", "X")}, true},
		{"spawned X, registered X", cur("X"), []map[string]any{ev("plugin_spawned", "X"), ev("plugin_registered", "X")}, true},
		{"spawned A, registered A, spawned B (a replacement)", cur("A"), []map[string]any{ev("plugin_spawned", "A"), ev("plugin_registered", "A"), ev("plugin_spawned", "B")}, false},
		{"registered X, spawned Y", cur("X"), []map[string]any{ev("plugin_registered", "X"), ev("plugin_spawned", "Y")}, false},
		{"registered X, stopped", cur("X"), []map[string]any{ev("plugin_registered", "X"), ev("plugin_stopped", "")}, false},
		{"registered X, spawned X, stopped", cur("X"), []map[string]any{ev("plugin_registered", "X"), ev("plugin_spawned", "X"), ev("plugin_stopped", "")}, false},
		{"registered X, exited X", cur("X"), []map[string]any{ev("plugin_registered", "X"), ev("plugin_exited", "X")}, false},
		{"registered X, exited X, spawned X (the exit is final for X)", cur("X"), []map[string]any{ev("plugin_registered", "X"), ev("plugin_exited", "X"), ev("plugin_spawned", "X")}, false},
		{"exited X, registered X (the exit is final for X)", cur("X"), []map[string]any{ev("plugin_exited", "X"), ev("plugin_registered", "X")}, false},
		{"exited X, spawned X, registered X (the exit is final for X)", cur("X"), []map[string]any{ev("plugin_exited", "X"), ev("plugin_spawned", "X"), ev("plugin_registered", "X")}, false},
		{"registered X, exited Y (another instance)", cur("X"), []map[string]any{ev("plugin_registered", "X"), ev("plugin_exited", "Y")}, true},
		{"registered X, stopped, spawned X (no registration after the stop)", cur("X"), []map[string]any{ev("plugin_registered", "X"), ev("plugin_stopped", ""), ev("plugin_spawned", "X")}, false},
		{"registered Y only", cur("X"), []map[string]any{ev("plugin_registered", "Y")}, false},
		{"no events", cur("X"), nil, false},
	} {
		if got := registeredNow(c.e, c.events); got != c.want {
			t.Errorf("%s: registeredNow = %v, want %v", c.name, got, c.want)
		}
	}
}
