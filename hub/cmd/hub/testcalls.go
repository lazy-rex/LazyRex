package main

// The call record (test mode only). The hub records each hub service call of a registered current
// plugin instance (the actions after the registration check in dispatch) when it arrives, before
// the action runs, so a call that fails is recorded too. A test reads the record to prove what a
// plugin did not call, for example that a migration never scans message/ records.
//
// hub.test.calls {plugin, action?, after?} -> {calls: [{seq, action, instance_id, args}], next, dropped}
//   after: only calls with seq > after (default 0). action: only calls of this action.
//   next: the highest seq that the hub gave, for any plugin. A test keeps it as a baseline.
//   dropped: the hub dropped a call of the plugin with seq > after (the bound), so the record is
//   not complete. A test that proves "never" asserts dropped == false.

import (
	"encoding/json"
	"strconv"
)

// testCallsKept is the number of calls that the hub keeps for each plugin.
const testCallsKept = 1000

type testCall struct {
	Seq        int64           `json:"seq"`
	Action     string          `json:"action"`
	InstanceID string          `json:"instance_id"`
	Args       json.RawMessage `json:"args"`
}

// testRecordCall records one call of plugin. raw is the call's args as the hub received them.
func (h *Hub) testRecordCall(plugin, instance, action string, raw json.RawMessage) {
	args := append(json.RawMessage(nil), raw...)
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	h.test.callSeq++
	calls := append(h.test.calls[plugin], testCall{Seq: h.test.callSeq, Action: action, InstanceID: instance, Args: args})
	if len(calls) > testCallsKept {
		h.test.callsDropped[plugin] = calls[len(calls)-testCallsKept-1].Seq
		calls = append([]testCall(nil), calls[len(calls)-testCallsKept:]...)
	}
	h.test.calls[plugin] = calls
}

func (h *Hub) testCalls(args map[string]any) (any, error) {
	plugin, _ := args["plugin"].(string)
	if plugin == "" {
		return nil, bad("invalid: plugin is required")
	}
	action, _ := args["action"].(string)
	var after int64
	switch v := args["after"].(type) {
	case nil:
	case float64:
		after = int64(v)
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, bad("invalid: after must be a number")
		}
		after = n
	default:
		return nil, bad("invalid: after must be a number")
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	out := []testCall{}
	for _, c := range h.test.calls[plugin] {
		if c.Seq > after && (action == "" || c.Action == action) {
			out = append(out, c)
		}
	}
	return map[string]any{"calls": out, "next": h.test.callSeq, "dropped": h.test.callsDropped[plugin] > after}, nil
}
