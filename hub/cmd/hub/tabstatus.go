package main

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Tab status: the program status a view shows on its Rex tab (OSC 7501, written by the
// renderer). A plugin declares it per tab (hub.ui.tabs "status"); a view's render may return
// tree.status, which overrides the declared status while it is present. The renderer reads only
// the snapshot's top-level status.

// parseTabStatus checks a declared status and returns it as stored JSON text. nil or null is no
// status (""). Fields: state (idle, working, done, blocked, error), kind (blocked only:
// permission, question, auth), msg (one line, at most 200 characters).
func parseTabStatus(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", bad("status must be an object or null")
	}
	for k := range m {
		if k != "state" && k != "kind" && k != "msg" {
			return "", bad("status: unknown field %q", k)
		}
	}
	state, _ := m["state"].(string)
	switch state {
	case "idle", "working", "done", "blocked", "error":
	default:
		return "", bad("status.state must be idle, working, done, blocked or error")
	}
	out := map[string]string{"state": state}
	if k, present := m["kind"]; present && k != nil {
		kind, _ := k.(string)
		if state != "blocked" || (kind != "permission" && kind != "question" && kind != "auth") {
			return "", bad("status.kind must be permission, question or auth, and only with state blocked")
		}
		out["kind"] = kind
	}
	if x, present := m["msg"]; present && x != nil {
		msg, ok := x.(string)
		if !ok || strings.ContainsAny(msg, "\n\r") || utf8.RuneCountInString(msg) > 200 {
			return "", bad("status.msg must be text on one line, at most 200 characters")
		}
		if msg = strings.TrimSpace(msg); msg != "" {
			out["msg"] = msg
		}
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// liftTreeStatus removes tree.status from a rendered tree and returns it as JSON text. A status
// that is not valid is dropped (""), so the declared status applies.
func liftTreeStatus(tree map[string]any) string {
	v, ok := tree["status"]
	if !ok {
		return ""
	}
	delete(tree, "status")
	s, err := parseTabStatus(v)
	if err != nil {
		return ""
	}
	return s
}
