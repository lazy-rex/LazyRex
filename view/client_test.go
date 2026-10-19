package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/coder/websocket"
)

// protocolFixture supplies Rex framing and hub actions. It is not a Rex lab proof.
type protocolFixture struct {
	appCalls                                     int
	appError                                     *wireError
	replies                                      chan map[string]json.RawMessage
	mu                                           sync.Mutex
	conn                                         *websocket.Conn
	attachment                                   string
	attaches, admissions, lookups, subscriptions int
	seen                                         int
	complete                                     bool
	effects                                      string
	replayAdmission                              bool
	dropReply                                    bool
	firstNotReady                                bool
	admitted                                     chan struct{}
	// Replacement recovery (panels.go): the replacement in each attach reply, a view.resume that
	// the fixture sends before the first attach reply, and the answers to hub.view.replacement in
	// order (a *wireError is an error answer). reads counts the reads.
	attachReplacement  map[string]any
	resumeBeforeAttach string
	readAnswers        []any
	reads              int
}

func (f *protocolFixture) serve(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"rex.control.v1"}})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	write := func(value any) { b, _ := json.Marshal(value); _ = c.Write(ctx, websocket.MessageText, b) }
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		var req struct {
			Type, ID, Method string
			Payload          map[string]json.RawMessage
		}
		if json.Unmarshal(b, &req) != nil {
			return
		}
		if req.Type == "hello" {
			write(map[string]any{"type": "welcome", "client": map[string]string{"id": "renderer"}})
			continue
		}
		var result any = map[string]any{}
		var problem *wireError
		switch req.Method {
		case "client.reply":
			if f.replies != nil {
				f.replies <- req.Payload
			}
		case "target.resolve":
			result = map[string]any{"results": []any{map[string]any{"status": "ambiguous", "candidates": []any{map[string]string{"client_id": "old-hub"}, map[string]string{"client_id": "hub"}}}}}
		case "client.inspect":
			var id string
			json.Unmarshal(req.Payload["client_id"], &id)
			state := "connected"
			if id == "old-hub" {
				state = "lost"
			}
			result = map[string]any{"client": map[string]string{"connection_state": state}}
		case "client.perform":
			var action string
			var args map[string]any
			json.Unmarshal(req.Payload["action"], &action)
			json.Unmarshal(req.Payload["args"], &args)
			f.mu.Lock()
			switch action {
			case "hub.view.app_action":
				f.appCalls++
				if args["view_id"] != "v1" || args["attachment"] != f.attachment || args["action"] != "pane.insert_path" {
					problem = &wireError{Code: "invalid", Message: "Wrong browser request"}
				} else if f.appError != nil {
					problem = f.appError
				} else {
					result = map[string]any{"status": "opened"}
				}
			case "hub.view.attach":
				f.attaches++
				if f.firstNotReady && f.attaches == 1 {
					problem = &wireError{Code: "refused", Message: "not_ready: the pane is opening"}
				} else {
					if f.resumeBeforeAttach != "" {
						// The hub's cancel sends view.resume before the renderer has the attach reply.
						write(map[string]any{"type": "event", "payload": map[string]any{"type": "client_perform_requested", "payload": map[string]any{"call_id": "early-resume", "action": "view.resume", "requester": map[string]string{"client_id": "hub"}, "args": map[string]string{"view_id": "v1", "attachment": "attachment", "replacement_id": f.resumeBeforeAttach}}}})
						f.resumeBeforeAttach = ""
					}
					f.attachment = "attachment"
					f.conn = c
					attached := map[string]any{"view_id": "v1", "plugin": "messaging", "route": "/thread/42", "attachment": f.attachment, "hub_generation": "1"}
					if f.attachReplacement != nil {
						attached["replacement"] = f.attachReplacement
					}
					result = attached
				}
			case "hub.view.close":
				if args["view_id"] != "v1" || args["attachment"] != f.attachment {
					problem = &wireError{Code: "stale_attachment", Message: "Wrong close attachment"}
				} else {
					result = map[string]any{"status": "closed"}
				}
			case "hub.view.replacement":
				f.reads++
				if args["attachment"] != f.attachment {
					problem = &wireError{Code: "refused", Message: "stale_attachment: attach again"}
				} else if len(f.readAnswers) > 0 {
					answer := f.readAnswers[0]
					f.readAnswers = f.readAnswers[1:]
					if e, ok := answer.(*wireError); ok {
						problem = e
					} else {
						result = map[string]any{"replacement": answer}
					}
				} else {
					result = map[string]any{"replacement": nil}
				}
			case "hub.view.seen":
				f.seen++
				result = map[string]any{"status": "accepted"}
			case "hub.view.subscribe":
				f.subscriptions++
				result = testSnapshot("saved", "1")
			case "hub.view.get":
				result = testSnapshot("saved", "1")
			case "hub.view.action", "hub.view.draft.save", "hub.view.send":
				f.admissions++
				result = map[string]any{"status": "accepted", "operation_key": args["operation_key"]}
				if f.replayAdmission {
					result = map[string]any{"status": "succeeded", "effects": f.effects, "operation_key": args["operation_key"]}
				}
				select {
				case f.admitted <- struct{}{}:
				default:
				}
				if f.dropReply {
					f.complete = true
					f.mu.Unlock()
					return
				}
			case "hub.view.operation":
				f.lookups++
				status := "pending"
				if f.complete {
					status = "succeeded"
				}
				result = map[string]any{"status": status, "operation_key": args["operation_key"], "result": map[string]any{"draft_revision": "2", "text": "hello"}, "effects": f.effects, "effects_error": "Could not open the view"}
			}
			f.mu.Unlock()
			result = map[string]any{"result": result}
		}
		reply := map[string]any{"type": "response", "id": req.ID, "payload": result}
		if problem != nil {
			delete(reply, "payload")
			reply["error"] = problem
		}
		write(reply)
	}
}

func startFixture(t *testing.T, f *protocolFixture) (*viewClient, <-chan tea.Msg) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lr-view-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "rex.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go server.Serve(l)
	ctx, cancel := context.WithCancel(context.Background())
	msgs := make(chan tea.Msg, 64)
	client := &viewClient{Socket: sock, ViewID: "v1", Plugin: "messaging", Route: "/thread/42", Requests: make(chan mutation, 4), changed: make(chan struct{}, 1), seen: make(chan struct{}, 1), emit: func(msg tea.Msg) {
		select {
		case msgs <- msg:
		case <-ctx.Done():
		}
	}}
	stopped := make(chan struct{})
	go func() { client.run(ctx); close(stopped) }()
	t.Cleanup(func() {
		cancel()
		server.Close()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Error("The client did not stop")
		}
	})
	return client, msgs
}

func waitMessage(t *testing.T, msgs <-chan tea.Msg, match func(tea.Msg) bool) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case msg := <-msgs:
			if match(msg) {
				return
			}
		case <-timer.C:
			t.Fatal("Timed out waiting for the client")
		}
	}
}

func TestClientWaitsForCompletionNoticeWithoutPolling(t *testing.T) {
	f := &protocolFixture{firstNotReady: true, admitted: make(chan struct{}, 1)}
	c, msgs := startFixture(t, f)
	waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	c.Requests <- mutation{Method: "hub.view.draft.save", Node: "composer", Key: "key1", Args: map[string]any{"operation_key": "key1", "node_id": "composer", "text": "hello", "base_revision": "1"}}
	select {
	case <-f.admitted:
	case <-time.After(time.Second):
		t.Fatal("No admission")
	}
	// A healthy accepted operation must not create a periodic query.
	time.Sleep(200 * time.Millisecond)
	f.mu.Lock()
	lookups, attaches, subscriptions := f.lookups, f.attaches, f.subscriptions
	f.complete = true
	conn := f.conn
	attachment := f.attachment
	f.mu.Unlock()
	if lookups != 0 || attaches != 2 || subscriptions != 1 {
		t.Fatalf("lookups=%d attaches=%d subscriptions=%d", lookups, attaches, subscriptions)
	}
	notice := map[string]any{"type": "event", "payload": map[string]any{"type": "client_perform_requested", "payload": map[string]any{"call_id": "changed", "action": "view.changed", "requester": map[string]string{"client_id": "hub"}, "args": map[string]string{"view_id": "v1", "attachment": attachment, "revision": "1"}}}}
	b, _ := json.Marshal(notice)
	if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	waitMessage(t, msgs, func(msg tea.Msg) bool {
		r, ok := msg.(completed)
		return ok && r.Request.Key == "key1" && r.Outcome.Status == "succeeded"
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookups != 1 || f.admissions != 1 {
		t.Fatalf("lookups=%d admissions=%d", f.lookups, f.admissions)
	}
}

func TestClientRecoversCommittedOperationAfterLostReply(t *testing.T) {
	f := &protocolFixture{dropReply: true, admitted: make(chan struct{}, 1)}
	c, msgs := startFixture(t, f)
	waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	c.Requests <- mutation{Method: "hub.view.send", Node: "composer", Key: "send1", Args: map[string]any{"operation_key": "send1", "node_id": "composer", "text": "hello", "draft_revision": "1"}}
	waitMessage(t, msgs, func(msg tea.Msg) bool {
		r, ok := msg.(completed)
		return ok && r.Request.Key == "send1" && r.Outcome.Status == "succeeded"
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.admissions != 1 || f.attaches != 2 || f.lookups != 1 {
		t.Fatalf("admissions=%d attaches=%d lookups=%d", f.admissions, f.attaches, f.lookups)
	}
}

func TestIdleClientReattachesAfterHubDisconnectEvent(t *testing.T) {
	f := &protocolFixture{admitted: make(chan struct{}, 1)}
	_, msgs := startFixture(t, f)
	waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	event := map[string]any{"type": "event", "payload": map[string]any{"type": "session_event", "payload": map[string]any{"event": map[string]any{"type": "client_disconnected", "payload": map[string]string{"client_id": "hub"}}}}}
	b, _ := json.Marshal(event)
	if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && !s.Ready })
	waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attaches != 2 || f.admissions != 0 {
		t.Fatalf("attaches=%d admissions=%d", f.attaches, f.admissions)
	}
}

func TestNewHubNoticeWakesRecovery(t *testing.T) {
	v := &viewClient{hub: "old", changed: make(chan struct{}, 1), seen: make(chan struct{}, 1)}
	v.event(json.RawMessage(`{"type":"client_changed","payload":{"client":{"client_id":"new","connection_state":"connected","info":{"label":"lazyrex-hub"}}}}`))
	if !v.available {
		t.Fatal("The new connected hub did not wake recovery")
	}
	select {
	case <-v.changed:
	default:
		t.Fatal("No recovery signal")
	}
	v.available = false
	v.event(json.RawMessage(`{"type":"client_changed","payload":{"client":{"client_id":"unrelated","connection_state":"connected","info":{"label":"another-client"}}}}`))
	if v.available {
		t.Fatal("An unrelated client woke recovery")
	}
}

func TestClientWaitsForEffectsAfterReplayedAdmission(t *testing.T) {
	f := &protocolFixture{complete: true, effects: "pending", replayAdmission: true, admitted: make(chan struct{}, 1)}
	c, msgs := startFixture(t, f)
	waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	c.Requests <- mutation{Method: "hub.view.action", Node: "new", Key: "create-key", Args: map[string]any{"operation_key": "create-key"}}
	<-f.admitted
	timer := time.NewTimer(100 * time.Millisecond)
	waiting := true
	for waiting {
		select {
		case msg := <-msgs:
			if _, ok := msg.(completed); ok {
				t.Fatal("pending effects completed early")
			}
		case <-timer.C:
			waiting = false
		}
	}
	f.mu.Lock()
	f.effects = "failed"
	conn, attachment := f.conn, f.attachment
	f.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"type": "event", "payload": map[string]any{"type": "client_perform_requested", "payload": map[string]any{"call_id": "effect-changed", "action": "view.changed", "requester": map[string]string{"client_id": "hub"}, "args": map[string]string{"view_id": "v1", "attachment": attachment, "revision": "1"}}}})
	if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	waitMessage(t, msgs, func(msg tea.Msg) bool {
		r, ok := msg.(completed)
		return ok && r.Outcome.Status == "succeeded" && r.Outcome.Effects == "failed" && r.Outcome.EffectsError != ""
	})
}
