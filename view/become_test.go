package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/coder/websocket"
)

// sendBecome sends view.become from requester as the hub would, and returns the renderer's
// client.reply payload.
func sendBecome(t *testing.T, f *protocolFixture, call, requester, repl, view, attachment, next string) map[string]json.RawMessage {
	t.Helper()
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"type": "event", "payload": map[string]any{"type": "client_perform_requested", "payload": map[string]any{
		"call_id": call, "action": "view.become", "requester": map[string]string{"client_id": requester},
		"args": map[string]string{"view_id": view, "attachment": attachment, "next_view_id": next, "next_plugin": "messaging", "next_route": "/forms/next", "replacement_id": repl}}}})
	if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-f.replies:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no reply", call)
		return nil
	}
}

// becomes counts the becomeRequested messages in msgs within d, and returns the last one.
func becomes(msgs <-chan tea.Msg, d time.Duration) (int, becomeRequested) {
	n, last := 0, becomeRequested{}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case msg := <-msgs:
			if b, ok := msg.(becomeRequested); ok {
				n, last = n+1, b
			}
		case <-timer.C:
			return n, last
		}
	}
}

// view.become (hub inplace.go): the renderer checks the requester first, then a replay of the
// request it accepted (ok, nothing changes), then the current view and attachment. It answers an
// accepted request at once and only then asks the model to become the next view, once.
func TestViewBecomeChecks(t *testing.T) {
	f := &protocolFixture{replies: make(chan map[string]json.RawMessage, 8)}
	_, msgs := startFixture(t, f)
	waitMessage(t, msgs, func(msg tea.Msg) bool { c, ok := msg.(connectionMsg); return ok && c.Ready })
	refused := func(r map[string]json.RawMessage, code string) bool {
		var e struct{ Code string }
		json.Unmarshal(r["error"], &e)
		return e.Code == code
	}
	ok := func(r map[string]json.RawMessage) bool {
		var res struct{ Status string }
		json.Unmarshal(r["result"], &res)
		return res.Status == "ok" && r["error"] == nil
	}
	if r := sendBecome(t, f, "c1", "intruder", "R", "v1", "attachment", "v2"); !refused(r, "refused") {
		t.Fatalf("from another client: %s", r)
	}
	if r := sendBecome(t, f, "c2", "hub", "R", "v1", "stale", "v2"); !refused(r, "refused") {
		t.Fatalf("a stale attachment: %s", r)
	}
	if n, _ := becomes(msgs, 200*time.Millisecond); n != 0 {
		t.Fatalf("a refused request switched the view (%d)", n)
	}
	if r := sendBecome(t, f, "c3", "hub", "R", "v1", "attachment", "v2"); !ok(r) {
		t.Fatalf("the request: %s", r)
	}
	if n, b := becomes(msgs, 500*time.Millisecond); n != 1 || b != (becomeRequested{ViewID: "v2", Plugin: "messaging", Route: "/forms/next"}) {
		t.Fatalf("after the request: %d %+v, want one switch to v2", n, b)
	}
	if r := sendBecome(t, f, "c4", "hub", "R", "v1", "attachment", "v2"); !ok(r) {
		t.Fatalf("a replay: %s", r)
	}
	if r := sendBecome(t, f, "c5", "intruder", "R", "v1", "attachment", "v2"); !refused(r, "refused") {
		t.Fatalf("a replay from another client: %s", r)
	}
	if r := sendBecome(t, f, "c6", "hub", "R2", "v1", "attachment", "v3"); !refused(r, "conflict") {
		t.Fatalf("another replacement: %s", r)
	}
	if n, _ := becomes(msgs, 300*time.Millisecond); n != 0 {
		t.Fatalf("a replay or a refusal switched again (%d)", n)
	}
}

// An attach that the hub answers {status: replaced} makes no attachment: the renderer asks to
// become the current view, with its plugin and route, and stops connecting.
func TestAttachReplacedBecomesTheCurrentView(t *testing.T) {
	f := &protocolFixture{attachReplaced: map[string]any{"status": "replaced", "current_view_id": "v9", "plugin": "messaging", "route": "/forms/c"}}
	_, msgs := startFixture(t, f)
	if n, b := becomes(msgs, 2*time.Second); n != 1 || b != (becomeRequested{ViewID: "v9", Plugin: "messaging", Route: "/forms/c"}) {
		t.Fatalf("%d %+v, want one switch to v9", n, b)
	}
	f.mu.Lock()
	attaches := f.attaches
	f.mu.Unlock()
	if attaches != 1 {
		t.Fatalf("%d attaches, want 1: the loop stops", attaches)
	}
}
