package main

// Test-mode faults (docs/testing.md, "Faults"). A test arms a fault with hub.test.fault; it fires
// once. Two points:
//   - spawn-create: the Rex request that creates a hub.spawn worker's pane (lose-answer, drop,
//     refuse; op = the spawn key).
//   - action: a hub action that a registered plugin calls (when = before | after,
//     mode = fail | lose-reply; key = the call's args.key; plugin = the calling plugin).
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
	Fired                bool
}

var testFaultModes = map[string][]string{"spawn-create": {"lose-answer", "drop", "refuse"}, "action": {"fail", "lose-reply"}}

// testFaultFields are the fields that each point accepts (plugin is the harness's own field).
var testFaultFields = map[string][]string{"spawn-create": {"point", "mode", "op", "plugin"},
	"action": {"point", "mode", "action", "when", "key", "plugin"}}

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
	for k, v := range args {
		if _, ok := v.(string); !ok {
			return nil, bad("invalid: fault field %s must be a string", k)
		}
	}
	f := &testFault{}
	f.Point, _ = args["point"].(string)
	f.Mode, _ = args["mode"].(string)
	for k := range args {
		if fields, ok := testFaultFields[f.Point]; ok && !contains(fields, k) {
			return nil, bad("invalid: field %s does not apply to point %s", k, f.Point)
		}
	}
	if modes, ok := testFaultModes[f.Point]; !ok || !contains(modes, f.Mode) {
		return nil, bad("invalid: point must be spawn-create (lose-answer, drop, refuse) or action (fail, lose-reply)")
	}
	switch f.Point {
	case "spawn-create":
		f.Op, _ = args["op"].(string)
	case "action":
		f.Action, _ = args["action"].(string)
		f.When, _ = args["when"].(string)
		f.Key, _ = args["key"].(string)
		f.Plugin = plugin
		if f.When != "before" && f.When != "after" {
			return nil, bad("invalid: when must be before or after")
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

// takeFault returns the first armed fault that match accepts, and marks it fired. It is nil
// outside test mode.
func (h *Hub) takeFault(match func(*testFault) bool) *testFault {
	if h.test == nil {
		return nil
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	for _, f := range h.test.faults {
		if !f.Fired && match(f) {
			f.Fired = true
			h.ev.emit("fault", map[string]any{"point": f.Point, "mode": f.Mode, "id": f.ID, "action": f.Action, "when": f.When})
			return f
		}
	}
	return nil
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

func faultReply(mode string) error {
	if mode == "lose-reply" {
		return refuse("disconnected", "injected fault: the reply was lost")
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
	if f := h.takeFault(match("before")); f != nil {
		return nil, faultReply(f.Mode) // no handler runs
	}
	armed := h.peekFault(match("after"))
	result, err := h.dispatch(ctx, p)
	if err != nil || !armed {
		return result, err // a genuine error is the reply; the fault stays armed
	}
	if f := h.takeFault(match("after")); f != nil {
		return nil, faultReply(f.Mode)
	}
	return result, nil
}
