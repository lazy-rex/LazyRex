package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// A lab test of hub.changed coalescing through a real Rex server (rexLab: LAB=1).

// A slow plugin client and a burst of 50 changes of one kind: the plugin gets the first delivery
// and one more, not 50 (which filled Rex's 16-request queue of the client before the fix).
func TestChangedBurstLab(t *testing.T) {
	l := rexLab(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dial := func() (*rex.Rex, string) {
		r, err := rex.Dial(ctx, l.socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		var w struct {
			Client struct {
				ID string `json:"id"`
			} `json:"client"`
		}
		json.Unmarshal(r.Welcome, &w)
		return r, w.Client.ID
	}
	// The plugin: it declares hub.changed and answers each call after 200 ms, one at a time.
	plugin, pluginID := dial()
	var received atomic.Int32
	go func() {
		for raw := range plugin.Events {
			var env struct {
				Payload struct {
					Type    string `json:"type"`
					Payload struct {
						CallID string `json:"call_id"`
					} `json:"payload"`
				} `json:"payload"`
			}
			if json.Unmarshal(raw, &env) == nil && env.Payload.Type == "client_perform_requested" {
				received.Add(1)
				go func(id string) {
					time.Sleep(200 * time.Millisecond)
					plugin.Call(context.Background(), "client.reply", map[string]any{"call_id": id, "result": map[string]any{}}, nil)
				}(env.Payload.Payload.CallID)
			}
		}
	}()
	if err := plugin.Call(ctx, "client.keymap", map[string]any{"actions": []any{
		map[string]any{"name": "hub.changed", "kind": "client", "title": "Hub data changed", "args": map[string]any{"type": "object"}}}}, nil); err != nil {
		t.Fatal(err)
	}
	hubRex, _ := dial()
	h := changedTestHub(map[string]string{"lab": pluginID})
	h.rex, h.cfg.Server = hubRex, l.server
	for i := 0; i < 50; i++ {
		h.changed("agents")
	}
	time.Sleep(3 * time.Second) // the first delivery, one more, and any later ones (200 ms each)
	n := received.Load()
	t.Logf("50 changes of one kind: %d deliveries reached the plugin", n)
	if n < 1 || n > 2 {
		t.Fatalf("%d deliveries, want the first and one more", n)
	}
}
