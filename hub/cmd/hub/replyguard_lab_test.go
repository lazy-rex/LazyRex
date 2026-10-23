package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// Lab tests of the reply limit through a real Rex server. Run with
//   REPLY_LAB_SOCKET=/tmp/rexlab-<name>/rex.sock go test -run Lab ./cmd/hub

func replyLabSocket(t *testing.T) string {
	t.Helper()
	socket := os.Getenv("REPLY_LAB_SOCKET")
	if socket == "" {
		t.Skip("set REPLY_LAB_SOCKET to an isolated Rex lab socket")
	}
	if !strings.HasPrefix(socket, "/tmp/rexlab-") || !strings.HasSuffix(socket, "/rex.sock") {
		t.Fatal("refuse a non-lab socket")
	}
	if real, err := filepath.EvalSymlinks(socket); err != nil || !strings.HasPrefix(real, "/private/tmp/rexlab-") && !strings.HasPrefix(real, "/tmp/rexlab-") {
		t.Fatalf("refuse a socket that resolves outside the lab: %s %v", real, err)
	}
	return socket
}

// labResponder is a client that declares lab.big and answers each call with answer.
type labResponder struct {
	r  *rex.Rex
	id string
}

func newLabResponder(t *testing.T, socket string, answer func(r *rex.Rex, callID string)) *labResponder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, socket)
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
	go func() {
		for raw := range r.Events {
			var env struct {
				Payload struct {
					Type    string `json:"type"`
					Payload struct {
						CallID string `json:"call_id"`
					} `json:"payload"`
				} `json:"payload"`
			}
			if json.Unmarshal(raw, &env) == nil && env.Payload.Type == "client_perform_requested" {
				go answer(r, env.Payload.Payload.CallID)
			}
		}
	}()
	obj := map[string]any{"type": "object"}
	if err := r.Call(ctx, "client.keymap", map[string]any{"actions": []any{
		map[string]any{"name": "lab.big", "kind": "client", "title": "Lab big reply", "args": obj},
		map[string]any{"name": "hub.forward", "kind": "client", "title": "Forwarded agent call", "args": obj}}}, nil); err != nil {
		t.Fatal(err)
	}
	return &labResponder{r: r, id: w.Client.ID}
}

// connected asks the server for the responder's own connection state.
func (l *labResponder) connected(t *testing.T, caller *rex.Rex) string {
	t.Helper()
	var ins struct {
		Client struct {
			State string `json:"connection_state"`
		} `json:"client"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := caller.Call(ctx, "client.inspect", map[string]any{"client_id": l.id}, &ins); err != nil {
		return "inspect failed: " + err.Error()
	}
	return ins.Client.State
}

func labPerform(caller *rex.Rex, client string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var res struct {
		Result json.RawMessage `json:"result"`
	}
	err := caller.Call(ctx, "client.perform", map[string]any{"client_id": client, "action": "lab.big", "args": map[string]any{}, "timeout_ms": 5000}, &res)
	return res.Result, err
}

// The server's limits, measured. Two limits apply to a reply:
//   - the replying client's frame: over max_message_bytes, the server closes its connection;
//   - the server's response frame to the caller: over the caller's limit, the caller gets
//     too_large at once, and the replying client stays connected.
//
// The guard's limit (replyLimit) passes both. The responder's reply is its second request (h2).
func TestServerFrameLimitLab(t *testing.T) {
	socket := replyLabSocket(t)
	caller := labCaller(t, socket)
	if caller.MaxMessage() <= 0 {
		t.Fatalf("the welcome states no limit: %s", caller.Welcome)
	}
	max := caller.MaxMessage()
	t.Logf("welcome max_message_bytes %d", max)
	for _, frame := range []int{replyLimit(caller), max - 64, max - 48, max - 32, max - 16, max, max + 1} {
		l := newLabResponder(t, socket, func(r *rex.Rex, callID string) {
			base := map[string]any{"call_id": callID, "result": map[string]any{"text": ""}}
			raw, _ := json.Marshal(base)
			n := frame - (rex.RequestFrameBytes("client.reply", raw) - 18) // the ID h2 has 1 digit, not 19
			raw, _ = json.Marshal(map[string]any{"call_id": callID, "result": map[string]any{"text": strings.Repeat("a", n)}})
			r.Call(context.Background(), "client.reply", json.RawMessage(raw), nil)
		})
		res, err := labPerform(caller, l.id)
		time.Sleep(300 * time.Millisecond)
		state := l.connected(t, caller)
		t.Logf("responder frame %d (max%+d): result %d bytes, caller error %v, responder %s", frame, frame-max, len(res), err, state)
		switch {
		case frame == replyLimit(caller) && (err != nil || l.r.Closed()):
			t.Fatalf("a frame at the guard limit failed: %v", err)
		case frame <= max && l.r.Closed():
			t.Fatalf("a frame within the limit closed the responder")
		case frame > max && (err == nil || !l.r.Closed()):
			t.Fatalf("a frame over the limit: err %v, closed %v", err, l.r.Closed())
		}
	}
}

// sendReply, which both hub reply paths use, keeps the connection for a result over the limit, and
// for an error over the limit. The caller gets reply_too_large at once, not a timeout.
func TestSendReplyKeepsTheConnectionLab(t *testing.T) {
	socket := replyLabSocket(t)
	caller := labCaller(t, socket)
	for _, failed := range []bool{false, true} {
		big := strings.Repeat("<é", caller.MaxMessage()/4) // about 2 times the limit when encoded
		l2 := newLabResponder(t, socket, func(r *rex.Rex, callID string) {
			reply := map[string]any{"call_id": callID, "result": map[string]any{"text": big}}
			if failed {
				reply = map[string]any{"call_id": callID, "error": &rex.RexError{Code: "failed", Message: big}}
			}
			sendReply(context.Background(), r, "lab.big", reply)
		})
		start := time.Now()
		_, err := labPerform(caller, l2.id)
		if err == nil || !strings.Contains(err.Error(), "reply_too_large") || time.Since(start) > 3*time.Second {
			t.Fatalf("failed=%v: %v after %v", failed, err, time.Since(start))
		}
		time.Sleep(300 * time.Millisecond)
		if l2.r.Closed() || l2.connected(t, caller) != "connected" {
			t.Fatalf("failed=%v: the responder lost its connection", failed)
		}
		t.Logf("failed=%v: %v", failed, err)
	}
}

// The agent face path end to end: an agent calls a plugin export; the plugin's result (sent on a
// Go connection, so it is not held to 64 KiB) fits the hub's incoming limit, and the hub's reply
// to the agent is over the guard's limit. The agent gets reply_too_large; the hub's agent face
// stays connected and answers the next call.
func TestAgentFaceReplyGuardLab(t *testing.T) {
	socket := replyLabSocket(t)
	caller := labCaller(t, socket)
	size := 0
	plugin := newLabResponder(t, socket, func(r *rex.Rex, callID string) {
		raw, _ := json.Marshal(map[string]any{"call_id": callID, "result": map[string]any{"text": strings.Repeat("a", size)}})
		r.Call(context.Background(), "client.reply", json.RawMessage(raw), nil)
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h, _ := workerTestHub(t)
	h.entries = []*configEntry{{name: "lab"}}
	r, err := rex.Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	go func() {
		for range r.Events {
		}
	}()
	h.rex = r
	h.plugins = map[string]*Plugin{"lab": {Name: "lab", ClientID: plugin.id, Exports: []string{"lab.big"}}}
	if err := h.agentFaceStart(ctx, socket); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.agentRex.Close() })
	var w struct {
		Client struct {
			ID string `json:"id"`
		} `json:"client"`
	}
	json.Unmarshal(h.agentRex.Welcome, &w)
	face := &labResponder{r: h.agentRex, id: w.Client.ID}
	for _, c := range []struct {
		size int
		want string
	}{{1_048_000, "reply_too_large"}, {1000, ""}} {
		size = c.size
		start := time.Now()
		res, err := labPerform(caller, face.id)
		t.Logf("plugin result %d bytes: reply %d bytes, err %v, after %v", c.size, len(res), err, time.Since(start))
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) || time.Since(start) > 3*time.Second {
			t.Fatalf("plugin result %d bytes: %v", c.size, err)
		}
		time.Sleep(300 * time.Millisecond)
		if h.agentRex.Closed() || face.connected(t, caller) != "connected" {
			t.Fatal("the agent face lost its connection")
		}
	}
}
