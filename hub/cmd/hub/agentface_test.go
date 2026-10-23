package main

import (
	"encoding/json"
	"slices"
	"testing"
)

// hub.plugins.op is a hub tool of the agent face (lead-c8-p34, P4): the parity check of a view
// action that names it finds it, and the face declares exactly one descriptor of it.
func TestAgentFacePluginOp(t *testing.T) {
	n := 0
	face := map[string]bool{}
	for _, a := range agentHubFace() {
		name := a["name"].(string)
		if name == "hub.plugins.op" {
			n++
		}
		if agentHubActions[name] {
			face[name] = true
		}
	}
	if n != 1 {
		t.Errorf("the agent face has %d hub.plugins.op descriptors, want 1", n)
	}
	d := actionDesc{Tool: "hub.plugins.op", toolSet: true}
	if p := parityProblem(d, map[string]bool{}, map[string]bool{}, face); p != "" {
		t.Errorf("a view action with tool hub.plugins.op: %s", p)
	}
}

// restart is on the agent face (lead-c8-b2b): the view's Restart names tool hub.plugins.op, and an
// agent gets the same op with the same admission (TestPluginOpRestartAdmission, an agent origin).
func TestAgentFacePluginOpRestart(t *testing.T) {
	for _, a := range pluginOpActions {
		if a["name"] != "hub.plugins.op" {
			continue
		}
		props := a["args"].(map[string]any)["properties"].(map[string]any)
		if ops := props["op"].(map[string]any)["enum"].([]string); !slices.Contains(ops, "restart") {
			t.Fatalf("the op enum has no restart: %v", ops)
		}
		key, req, err := agentOpRequest(map[string]json.RawMessage{"key": json.RawMessage(`"k"`), "op": json.RawMessage(`"restart"`), "entry": json.RawMessage(`"managed:x"`)})
		if err != nil || key != "k" || req.Op != "restart" || req.Entry != "managed:x" {
			t.Fatalf("an agent restart request: %q %+v %v", key, req, err)
		}
		return
	}
	t.Fatal("no hub.plugins.op descriptor")
}
