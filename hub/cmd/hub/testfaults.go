package main

// Test-mode faults (docs/testing.md, "Faults"). A test arms a fault with hub.test.fault; it fires
// once. Two points:
//   - spawn-create: the Rex request that creates a hub.spawn worker's pane (lose-answer, drop,
//     refuse; op = the spawn key).
//   - action: a hub action that a registered plugin calls (when = before | after,
//     mode = fail | lose-reply | gate; key = the call's args.key; plugin = the calling plugin).
//     gate (before only) is a barrier for a caller that retries: from the call that fires it
//     until hub.test.fault_release, every matching call gets 'unavailable' and runs no handler.
//     A hub request cannot wait: Rex delivers the hub's calls one at a time.
// nth = n fires on the n-th call that matches (default 1).
// hub.test.* actions exist only with -test, so a hub without -test has no faults.

import (
	"context"
	"encoding/json"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

type testFault struct {
	ID                   int
	Point, Mode          string
	Op                   string // spawn-create: the spawn key ("" = any)
	Plugin, Action, When string // action
	Key                  string // action: the call's args.key ("" = any)
	FailCode             string // action, mode fail: the reply's error code (default failed)
	Nth, seen            int    // fire on the Nth matching call; seen counts the matching calls
	Fired, Released      bool   // Released: a gate is open again
	Gated                int    // gate: the calls that it refused
}

var testFaultModes = map[string][]string{"spawn-create": {"lose-answer", "drop", "refuse"}, "action": {"fail", "lose-reply", "gate"}}

// testFaultFields are the fields that each point accepts (plugin is the harness's own field).
var testFaultFields = map[string][]string{"spawn-create": {"point", "mode", "op", "plugin"},
	"action": {"point", "mode", "action", "when", "key", "plugin", "fail_code", "nth"}}

// faultableAction reports a hub action that a plugin can call: not a hub.test.* control.
func faultableAction(name string) bool {
	if contains(hubActionNames, name) {
		return true
	}
	for _, a := range hubToolActions {
		if a["name"] == name {
			return true
		}
	}
	return false
}

func (h *Hub) testFaultArm(args map[string]any, plugin string) (any, error) {
	f := &testFault{Nth: 1}
	for k, v := range args {
		if k == "nth" {
			n, ok := v.(float64)
			if !ok || n < 1 || n != float64(int(n)) {
				return nil, bad("invalid: nth must be a positive integer")
			}
			f.Nth = int(n)
			continue
		}
		if _, ok := v.(string); !ok {
			return nil, bad("invalid: fault field %s must be a string", k)
		}
	}
	f.Point, _ = args["point"].(string)
	f.Mode, _ = args["mode"].(string)
	for k := range args {
		if fields, ok := testFaultFields[f.Point]; ok && !contains(fields, k) {
			return nil, bad("invalid: field %s does not apply to point %s", k, f.Point)
		}
	}
	if modes, ok := testFaultModes[f.Point]; !ok || !contains(modes, f.Mode) {
		return nil, bad("invalid: point must be spawn-create (lose-answer, drop, refuse) or action (fail, lose-reply, gate)")
	}
	switch f.Point {
	case "spawn-create":
		f.Op, _ = args["op"].(string)
	case "action":
		f.Action, _ = args["action"].(string)
		f.When, _ = args["when"].(string)
		f.Key, _ = args["key"].(string)
		if v, ok := args["fail_code"]; ok {
			// lose-reply always means disconnected; only fail takes a code.
			if f.FailCode, _ = v.(string); f.FailCode == "" || f.Mode != "fail" {
				return nil, bad("invalid: fail_code must be nonempty text, and it needs mode fail")
			}
		}
		f.Plugin = plugin
		if f.When != "before" && f.When != "after" {
			return nil, bad("invalid: when must be before or after")
		}
		if f.Mode == "gate" && f.When != "before" {
			return nil, bad("invalid: gate needs when before")
		}
		if !faultableAction(f.Action) {
			return nil, bad("invalid: %q is not a hub action that a plugin can call", f.Action)
		}
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	f.ID = len(h.test.faults) + 1
	h.test.faults = append(h.test.faults, f)
	return map[string]any{"id": f.ID}, nil
}

// takeFault fires the first armed fault that match accepts and whose nth matching call this is.
// Every armed fault that matches counts the call, also when another fault fires. It is nil outside
// test mode.
func (h *Hub) takeFault(match func(*testFault) bool) *testFault {
	if h.test == nil {
		return nil
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	var fired *testFault
	for _, f := range h.test.faults {
		if f.Fired || !match(f) {
			continue
		}
		f.seen++
		if fired == nil && f.seen >= f.Nth {
			f.Fired, fired = true, f
			if f.Mode == "gate" {
				f.Gated++
			}
			h.ev.emit("fault", map[string]any{"point": f.Point, "mode": f.Mode, "id": f.ID, "action": f.Action, "when": f.When})
		}
	}
	return fired
}

// gateClosed reports a fired gate that match accepts and that is not released, and counts the
// refused call.
func (h *Hub) gateClosed(match func(*testFault) bool) bool {
	if h.test == nil {
		return false
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	for _, f := range h.test.faults {
		if f.Mode == "gate" && f.Fired && !f.Released && match(f) {
			f.Gated++
			return true
		}
	}
	return false
}

// peekFault reports an armed fault that match accepts, without firing it.
func (h *Hub) peekFault(match func(*testFault) bool) bool {
	if h.test == nil {
		return false
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	for _, f := range h.test.faults {
		if !f.Fired && match(f) {
			return true
		}
	}
	return false
}

// spawnCreateFaulted sends a worker's create request, or applies an armed spawn-create fault.
func (h *Hub) spawnCreateFaulted(ctx context.Context, key, method string, req map[string]any, out any) error {
	match := func(mode string) func(*testFault) bool {
		return func(f *testFault) bool {
			return f.Point == "spawn-create" && (mode == "" || f.Mode == mode) && (f.Op == "" || f.Op == key)
		}
	}
	// lose-answer, as an after fault: it fires only when Rex answers yes. A genuine error is the
	// result, and the fault stays armed.
	if h.peekFault(match("lose-answer")) {
		if err := spawnCreate(ctx, h, method, req, out); err != nil {
			return err
		}
		if h.takeFault(match("lose-answer")) == nil {
			return nil
		}
		return &rex.RexError{Code: "disconnected", Message: "injected fault: the create answer was lost"}
	}
	f := h.takeFault(match(""))
	if f == nil {
		return spawnCreate(ctx, h, method, req, out)
	}
	switch f.Mode {
	case "drop":
		return &rex.RexError{Code: "disconnected", Message: "injected fault: the create was not sent"}
	}
	return &rex.RexError{Code: "refused", Message: "injected fault: Rex refused the create"}
}

func faultReply(f *testFault) error {
	if f.Mode == "lose-reply" {
		return refuse("disconnected", "injected fault: the reply was lost")
	}
	if f.FailCode != "" {
		return refuse(f.FailCode, "injected fault")
	}
	return refuse("failed", "injected fault")
}

// dispatchFaulted runs dispatch with the armed action faults of the calling plugin.
func (h *Hub) dispatchFaulted(ctx context.Context, p perform) (any, error) {
	if h.test == nil {
		return h.dispatch(ctx, p)
	}
	pl := h.pluginByClient(p.Requester.ClientID)
	if pl == nil {
		return h.dispatch(ctx, p)
	}
	var args struct {
		Key any `json:"key"`
	}
	json.Unmarshal(p.Args, &args)
	key, _ := args.Key.(string)
	match := func(when string) func(*testFault) bool {
		return func(f *testFault) bool {
			return f.Point == "action" && f.When == when && f.Plugin == pl.Name && f.Action == p.Action && (f.Key == "" || f.Key == key)
		}
	}
	if h.gateClosed(match("before")) {
		return nil, refuse("unavailable", "injected fault: a gate is closed") // no handler runs
	}
	if f := h.takeFault(match("before")); f != nil {
		if f.Mode == "gate" {
			return nil, refuse("unavailable", "injected fault: a gate is closed")
		}
		return nil, faultReply(f) // no handler runs
	}
	return h.dispatchAfter(ctx, p, match("after"))
}

// dispatchAfter runs dispatch with the armed after faults (matchAfter) of the calling plugin.
func (h *Hub) dispatchAfter(ctx context.Context, p perform, matchAfter func(*testFault) bool) (any, error) {
	armed := h.peekFault(matchAfter)
	result, err := h.dispatch(ctx, p)
	if err != nil || !armed {
		return result, err // a genuine error is the reply; the fault stays armed
	}
	if f := h.takeFault(matchAfter); f != nil {
		return nil, faultReply(f)
	}
	return result, nil
}

// testFaultRelease opens a gate. Releasing a gate twice, or a fault that is not a gate, is an
// error. A gate that has not fired yet can be released: it then never fires.
func (h *Hub) testFaultRelease(id float64) (any, error) {
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	for _, f := range h.test.faults {
		if float64(f.ID) != id {
			continue
		}
		if f.Mode != "gate" || f.Released {
			return nil, bad("invalid: fault %d is not a closed gate", f.ID)
		}
		f.Released = true
		return map[string]any{"released": true, "fired": f.Fired, "gated": f.Gated}, nil
	}
	return nil, bad("invalid: no fault %v", id)
}
