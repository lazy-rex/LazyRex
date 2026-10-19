package main

import (
	tea "charm.land/bubbletea/v2"
	"encoding/json"
	"testing"
	"time"
)

func TestRetireRequiresCurrentAttachment(t *testing.T) {
	f := &protocolFixture{replies: make(chan map[string]json.RawMessage, 8)}
	c, msgs := startFixture(t, f)
	waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	send := func(hub, view, attachment string) map[string]json.RawMessage {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"type": "client_perform_requested", "payload": map[string]any{"call_id": "retire", "action": "view.retire", "requester": map[string]string{"client_id": hub}, "args": map[string]string{"view_id": view, "attachment": attachment}}})
		c.event(raw)
		select {
		case reply := <-f.replies:
			return reply
		case <-time.After(time.Second):
			t.Fatal("No retirement reply")
			return nil
		}
	}
	for _, ids := range [][3]string{{"other", "v1", "attachment"}, {"hub", "other", "attachment"}, {"hub", "v1", "old"}} {
		if reply := send(ids[0], ids[1], ids[2]); reply["error"] == nil {
			t.Fatal("Invalid retirement accepted")
		}
	}
	reply := send("hub", "v1", "attachment")
	var result map[string]string
	if err := json.Unmarshal(reply["result"], &result); err != nil || result["status"] != "accepted" {
		t.Fatalf("Wrong acceptance: %s", reply["result"])
	}
	waitMessage(t, msgs, func(msg tea.Msg) bool { _, ok := msg.(retireRequested); return ok })
}
