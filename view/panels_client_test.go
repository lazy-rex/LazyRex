package main

import (
	tea "charm.land/bubbletea/v2"
	"encoding/json"
	"testing"
	"time"
)

func TestReleaseReplyWaitsForModelAndChecksAttachment(t *testing.T) {
	f := &protocolFixture{replies: make(chan map[string]json.RawMessage, 8)}
	c, msgs := startFixture(t, f)
	waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	m := newModel(c)
	m.ready = true
	send := func(attachment string) {
		raw, _ := json.Marshal(map[string]any{"type": "client_perform_requested", "payload": map[string]any{"call_id": "release", "action": "view.release", "requester": map[string]string{"client_id": "hub"}, "args": map[string]string{"view_id": "v1", "attachment": attachment, "replacement_id": "r1", "next_title": "Worktrees"}}})
		c.event(raw)
	}
	send("old")
	select {
	case reply := <-f.replies:
		if reply["error"] == nil {
			t.Fatal("Obsolete attachment accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("No refusal")
	}
	send("attachment")
	var request replacementRequested
	waitMessage(t, msgs, func(msg tea.Msg) bool { var ok bool; request, ok = msg.(replacementRequested); return ok })
	select {
	case <-f.replies:
		t.Fatal("The wire answered before the model froze input")
	default:
	}
	_, cmd := m.Update(request)
	if m.replacement == nil || cmd == nil {
		t.Fatal("The model did not freeze")
	}
	cmd()
	select {
	case reply := <-f.replies:
		var result map[string]string
		json.Unmarshal(reply["result"], &result)
		if result["status"] != "released" {
			t.Fatalf("Wrong answer: %s", reply["result"])
		}
	case <-time.After(time.Second):
		t.Fatal("No release answer")
	}
}
