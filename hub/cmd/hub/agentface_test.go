package main

import "testing"

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
