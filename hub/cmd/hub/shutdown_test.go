package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// hub.admin.shutdown: an operator call is accepted with {"stopping": true}. dispatch does not stop
// the hub (answer stops it after the reply). An argument, or a caller in a plugin pane, is refused,
// and the hub keeps running.
func TestAdminShutdownDispatch(t *testing.T) {
	h, _ := workerTestHub(t)
	parents := map[int]int{40: 30, 30: 20, 20: 1}
	h.procRead = func(pid int) (procRecord, error) { return procRecord{ppid: parents[pid]}, nil }
	h.plugins = map[string]*Plugin{"p": {Name: "p", ChildPID: 30}}
	stops := 0
	h.shutdown = func() { stops++ }
	call := func(pid int, args string) (any, error) {
		var p perform
		json.Unmarshal([]byte(fmt.Sprintf(`{"action":"hub.admin.shutdown","args":%s,"requester":{"client_id":"c9","principal":{"pid":%d}}}`, args, pid)), &p)
		return h.dispatch(context.Background(), p)
	}

	out, err := call(20, `{}`)
	if err != nil || !reflect.DeepEqual(out, map[string]any{"stopping": true}) {
		t.Fatalf("operator call: %v, %v", out, err)
	}
	if _, err := call(20, `null`); err != nil {
		t.Fatalf("operator call without args: %v", err)
	}
	if _, err := call(20, `{"now":true}`); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("argument: %v", err)
	}
	if _, err := call(40, `{}`); err == nil || !strings.Contains(err.Error(), "refused from agent and plugin panes") {
		t.Errorf("plugin pane caller: %v", err)
	}
	if stops != 0 {
		t.Errorf("dispatch stopped the hub %d times; only answer stops it, after the reply", stops)
	}
}

// The stop starts only after a delivered reply. A failed reply stops nothing: the caller sees an
// error or a timeout, and the hub still runs, so a retry is correct.
func TestAdminShutdownStopsOnlyAfterADeliveredReply(t *testing.T) {
	h := &Hub{}
	stops := 0
	h.shutdown = func() { stops++ }
	h.afterReply("hub.admin.shutdown", nil, errors.New("reply lost"))
	h.afterReply("hub.admin.shutdown", errors.New("refused"), nil)
	h.afterReply("hub.admin.stop", nil, nil)
	if stops != 0 {
		t.Fatalf("stops after a failed reply, a refusal or another action: %d", stops)
	}
	h.afterReply("hub.admin.shutdown", nil, nil)
	if stops != 1 {
		t.Fatalf("stops after a delivered reply: %d, want 1", stops)
	}
}
