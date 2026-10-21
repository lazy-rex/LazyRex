package main

// Test-mode faults (docs/testing.md, "Faults"). A test arms a fault with hub.test.fault; it fires
// once. Four points:
//   - spawn-create: the Rex request that creates a hub.spawn worker's pane (lose-answer, drop,
//     refuse; op = the spawn key).
//   - action: a hub action that a registered plugin calls (when = before | after,
//     mode = fail | lose-reply | gate; key = the call's args.key; plugin = the calling plugin).
//     gate (before only) is a barrier for a caller that retries: from the call that fires it
//     until hub.test.fault_release, every matching call gets 'unavailable' and runs no handler.
//     A hub request cannot wait: Rex delivers the hub's calls one at a time.
//   - watch-nudge: a hub.changed nudge of a watch to the plugin (watchsvc.go; mode drop; key =
//     the watch key, "" = any). The nudge is not sent.
//   - status-read: a read of a pane's program status (agents.go readStatus; key = the session ID,
//     "" = any). fail: the read fails. hold: noticeLoop's read of an unknown state waits after the
//     read until hub.test.fault_release, so a test can change the status in between.
//   - start-admission: the start path of a plugin (plugins.go startPluginWith; key = the plugin
//     name, "" = any). hold-before waits after the lifecycle lock, before the admission;
//     hold-after waits after the admission, before the retire and the spawn. Both wait until
//     hub.test.fault_release, at most 30 s, or hold_ms ms. A hub request that waits on the held
//     start (for example on its lifecycle lock) holds the hub's request queue, so the release
//     cannot arrive: such a test uses hold_ms.
// nth = n fires on the n-th call that matches (default 1). A call that a closed gate refuses
// counts for no fault: it does not reach the hub.
// hub.test.* actions exist only with -test, so a hub without -test has no faults.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

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
	HoldMS               int    // start-admission: the hold ends by itself after this time (default 30 s)
	Fired, Released      bool   // Released: a gate is open again
	Gated                int    // gate: the calls that it refused
}

var testFaultModes = map[string][]string{"spawn-create": {"lose-answer", "drop", "refuse"}, "action": {"fail", "lose-reply", "gate"},
	"watch-nudge": {"drop"}, "status-read": {"fail", "hold"}, "start-admission": {"hold-before", "hold-after"}}

// testFaultFields are the fields that each point accepts (plugin is the harness's own field).
var testFaultFields = map[string][]string{"spawn-create": {"point", "mode", "op", "plugin"},
	"action":          {"point", "mode", "action", "when", "key", "plugin", "fail_code", "nth"},
	"watch-nudge":     {"point", "mode", "key", "plugin"},
	"status-read":     {"point", "mode", "key", "plugin", "nth"},
	"start-admission": {"point", "mode", "key", "plugin", "nth", "hold_ms"}}

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
		if k == "nth" || k == "hold_ms" {
			n, ok := v.(float64)
			if !ok || n < 1 || n != float64(int(n)) {
				return nil, bad("invalid: %s must be a positive integer", k)
			}
			if k == "nth" {
				f.Nth = int(n)
			} else {
				f.HoldMS = int(n)
			}
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
		return nil, bad("invalid: point must be spawn-create (lose-answer, drop, refuse), action (fail, lose-reply, gate), watch-nudge (drop), status-read (fail, hold) or start-admission (hold-before, hold-after)")
	}
	switch f.Point {
	case "spawn-create":
		f.Op, _ = args["op"].(string)
	case "watch-nudge":
		f.Key, _ = args["key"].(string)
		f.Plugin = plugin
	case "status-read":
		f.Key, _ = args["key"].(string)
	case "start-admission":
		f.Key, _ = args["key"].(string)
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
	return h.takeFaultLocked(match)
}

// takeFaultLocked is takeFault; the caller holds h.test.mu.
func (h *Hub) takeFaultLocked(match func(*testFault) bool) *testFault {
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

// takeBefore applies the before faults to one call, in one step: a closed gate refuses the call
// (gated true), and then no fault counts it. Otherwise takeFault selects the fault that fires.
func (h *Hub) takeBefore(match func(*testFault) bool) (gated bool, fired *testFault) {
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	for _, f := range h.test.faults {
		if f.Mode == "gate" && f.Fired && !f.Released && match(f) {
			f.Gated++
			return true, nil
		}
	}
	return false, h.takeFaultLocked(match)
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
	gated, f := h.takeBefore(match("before"))
	if gated || f != nil && f.Mode == "gate" {
		return nil, refuse("unavailable", "injected fault: a gate is closed") // no handler runs
	}
	if f != nil {
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

// testFaultRelease opens a gate that has fired. Releasing a gate that has not fired, a gate twice,
// or a fault that is not a gate, is an error.
func (h *Hub) testFaultRelease(id float64) (any, error) {
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	for _, f := range h.test.faults {
		if float64(f.ID) != id {
			continue
		}
		if f.Mode != "gate" && !strings.HasPrefix(f.Mode, "hold") || f.Released {
			return nil, bad("invalid: fault %d is not a closed gate or hold", f.ID)
		}
		if !f.Fired {
			return nil, bad("invalid: gate %d has not fired", f.ID)
		}
		f.Released = true
		return map[string]any{"released": true, "fired": f.Fired, "gated": f.Gated}, nil
	}
	return nil, bad("invalid: no fault %v", id)
}

// statusReadFault fires an armed status-read fault of this mode for a status read of a pane in
// session sess (key = the session ID, "" = any). It is nil outside test mode.
func (h *Hub) statusReadFault(sess, mode string) *testFault {
	return h.takeFault(func(f *testFault) bool {
		return f.Point == "status-read" && f.Mode == mode && (f.Key == "" || f.Key == sess)
	})
}

// waitRelease holds the caller until hub.test.fault_release opens f, at most 30 s or f.HoldMS.
func (h *Hub) waitRelease(f *testFault) {
	limit := 30 * time.Second
	if f.HoldMS > 0 {
		limit = time.Duration(f.HoldMS) * time.Millisecond
	}
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		h.test.mu.Lock()
		released := f.Released
		h.test.mu.Unlock()
		if released {
			return
		}
	}
}

// startFault holds the start path of plugin at an armed start-admission fault of this mode,
// until hub.test.fault_release. It does nothing outside test mode.
func (h *Hub) startFault(plugin, mode string) {
	f := h.takeFault(func(f *testFault) bool {
		return f.Point == "start-admission" && f.Mode == mode && (f.Key == "" || f.Key == plugin)
	})
	if f == nil {
		return
	}
	h.ev.emit("fault", map[string]any{"point": f.Point, "mode": f.Mode, "plugin": plugin})
	h.waitRelease(f)
}
