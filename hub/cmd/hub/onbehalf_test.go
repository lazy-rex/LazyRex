package main

import (
	"context"
	"strings"
	"testing"
)

// on_behalf refusals happen before any Rex call, so they need no lab. The accepted path runs on
// the real hub in tests/harness/tests/onbehalf.lua.
func TestOnBehalfRefusals(t *testing.T) {
	const pluginA, pluginB = "client:a", "client:b"
	h := &Hub{plugins: map[string]*Plugin{"a": {Name: "a", ClientID: pluginA}, "b": {Name: "b", ClientID: pluginB}}}
	from := func(client string) perform {
		var p perform
		p.Requester.ClientID = client
		return p
	}
	refused := func(name string, p perform, args map[string]any, want string) {
		t.Helper()
		_, err := h.spawnCaller(context.Background(), p, args)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", name, err, want)
		}
	}
	refused("no forward", from(pluginA), map[string]any{"on_behalf": float64(42)}, "no agent call")
	h.beginForward(pluginA, 42)
	refused("another plugin during the forward", from(pluginB), map[string]any{"on_behalf": float64(42)}, "no agent call")
	refused("another pid during the forward", from(pluginA), map[string]any{"on_behalf": float64(43)}, "no agent call")
	refused("not a registered plugin", from("client:agent"), map[string]any{"on_behalf": float64(42)}, "only a registered plugin")
	refused("with for", from(pluginA), map[string]any{"on_behalf": float64(42), "for": "user"}, "cannot be combined")
	for _, v := range []any{"42", float64(0), float64(-1), 42.5, nil} {
		refused("bad value", from(pluginA), map[string]any{"on_behalf": v}, "requester_pid")
	}
	if !h.forwarding(pluginA, 42) {
		t.Fatal("the forward is in flight")
	}
	// Two forwards of the same pid: the grant ends with the last one.
	h.beginForward(pluginA, 42)
	h.endForward(pluginA, 42)
	if !h.forwarding(pluginA, 42) {
		t.Fatal("one forward is still in flight")
	}
	h.endForward(pluginA, 42)
	refused("after the forward ended", from(pluginA), map[string]any{"on_behalf": float64(42)}, "no agent call")
	if len(h.forwards) != 0 {
		t.Fatalf("forwards left: %#v", h.forwards)
	}
}
