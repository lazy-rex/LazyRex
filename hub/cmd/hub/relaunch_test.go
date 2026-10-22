package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// relaunchTestHub is a hub at exec 2 whose pid 40 runs in a plugin pane (pid 20 is an operator).
func relaunchTestHub(t *testing.T) (*Hub, *int) {
	h, _ := workerTestHub(t)
	parents := map[int]int{40: 30, 30: 20, 20: 1}
	h.procRead = func(pid int) (procRecord, error) { return procRecord{ppid: parents[pid]}, nil }
	h.plugins = map[string]*Plugin{"p": {Name: "p", ChildPID: 30}}
	h.exec = 2
	stops := 0
	h.shutdown = func() { stops++ }
	return h, &stops
}

func adminCall(h *Hub, action string, pid int, args string) (any, error) {
	var p perform
	json.Unmarshal([]byte(fmt.Sprintf(`{"action":%q,"args":%s,"requester":{"client_id":"c9","principal":{"pid":%d}}}`, action, args, pid)), &p)
	return h.dispatch(context.Background(), p)
}

// wantRefusal fails the test unless err is a reply error with this code and message.
func wantRefusal(t *testing.T, what string, err error, code, msg string) {
	t.Helper()
	var se *storeError
	if !errors.As(err, &se) || se.code != code || se.msg != msg {
		t.Errorf("%s: %#v, want code %q message %q", what, err, code, msg)
	}
}

// Before the latch: a hub that no wrapper started, a bad or missing expect_exec, or another exec
// count is refused, and the latch stays open, so a correct call still relaunches. dispatch does
// not stop the hub (afterReply does).
func TestRelaunchRefusals(t *testing.T) {
	h, stops := relaunchTestHub(t)
	call := func(args string) error { _, err := adminCall(h, "hub.admin.relaunch", 20, args); return err }

	wantRefusal(t, "no wrapper", call(`{"expect_exec":2}`), "refused", "refused: no restart wrapper; stop and start the hub by hand")
	h.wrapped.Store(true)
	for _, args := range []string{`{}`, `null`, `{"expect_exec":-1}`, `{"expect_exec":2.5}`, `{"expect_exec":"2"}`, `{"expect_exec":null}`} {
		wantRefusal(t, args, call(args), "invalid_input", "invalid_input: expect_exec is the exec count from hub.info")
	}
	wantRefusal(t, "another argument", call(`{"expect_exec":2,"now":true}`), "invalid_input", "invalid_input: hub.admin.relaunch takes only expect_exec")
	wantRefusal(t, "an older exec", call(`{"expect_exec":1}`), "refused", "refused: already restarted: exec is 2, expected 1")
	wantRefusal(t, "a newer exec", call(`{"expect_exec":3}`), "refused", "refused: already restarted: exec is 2, expected 3")
	if _, err := adminCall(h, "hub.admin.relaunch", 40, `{"expect_exec":2}`); err == nil || err.Error() != "admin actions are refused from agent and plugin panes" {
		t.Errorf("plugin pane caller: %v", err)
	}
	if h.relaunching.Load() || *stops != 0 {
		t.Fatalf("a refusal set the latch (%v) or stopped the hub (%d)", h.relaunching.Load(), *stops)
	}
	out, err := adminCall(h, "hub.admin.relaunch", 20, `{"expect_exec":2}`)
	if err != nil || !reflect.DeepEqual(out, map[string]any{"relaunching": true, "exec": 2}) {
		t.Fatalf("operator call: %v, %v", out, err)
	}
	if !h.relaunching.Load() || *stops != 0 {
		t.Errorf("after the accepted call: latch %v, stops %d (want true, 0)", h.relaunching.Load(), *stops)
	}
}

// After an accepted relaunch, while the hub stops: every admin call from an operator gets
// "relaunch in progress", also a resend of the same request and hub.admin.shutdown. The admin
// guard runs first, so a plugin pane learns nothing. Other actions still answer.
func TestRelaunchLatch(t *testing.T) {
	h, stops := relaunchTestHub(t)
	h.wrapped.Store(true)
	if _, err := adminCall(h, "hub.admin.relaunch", 20, `{"expect_exec":2}`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ action, args string }{
		{"hub.admin.relaunch", `{"expect_exec":2}`},
		{"hub.admin.relaunch", `{"expect_exec":1}`},
		{"hub.admin.relaunch", `{"bad":1}`},
		{"hub.admin.shutdown", `{}`},
		{"hub.admin.stop", `{}`},
	} {
		_, err := adminCall(h, c.action, 20, c.args)
		wantRefusal(t, c.action+" "+c.args, err, "refused", "refused: relaunch in progress")
	}
	if _, err := adminCall(h, "hub.admin.relaunch", 40, `{"expect_exec":2}`); err == nil || err.Error() != "admin actions are refused from agent and plugin panes" {
		t.Errorf("plugin pane caller: %v", err)
	}
	if out, err := adminCall(h, "hub.info", 40, `{}`); err != nil || out.(map[string]any)["exec"] != 2 {
		t.Errorf("hub.info during the relaunch: %v, %v", out, err)
	}
	if *stops != 0 {
		t.Errorf("dispatch stopped the hub %d times", *stops)
	}
}

// An accepted relaunch stops the hub also when its reply is lost: a resend gets "relaunch in
// progress" from this hub or "already restarted" from a new one. A refused relaunch stops nothing.
func TestRelaunchStopsAlsoWhenItsReplyIsLost(t *testing.T) {
	h := &Hub{}
	stops := 0
	h.shutdown = func() { stops++ }
	h.afterReply("hub.admin.relaunch", errors.New("refused"), nil)
	h.afterReply("hub.admin.relaunch", errors.New("refused"), errors.New("reply lost"))
	if stops != 0 {
		t.Fatalf("stops after a refusal: %d", stops)
	}
	h.afterReply("hub.admin.relaunch", nil, errors.New("reply lost"))
	if stops != 1 {
		t.Fatalf("stops after an accepted relaunch with a lost reply: %d, want 1", stops)
	}
	h.afterReply("hub.admin.relaunch", nil, nil)
	if stops != 2 {
		t.Fatalf("stops after an accepted relaunch: %d, want 2", stops)
	}
}
