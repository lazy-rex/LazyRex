package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// hub.tools listed the actions for the MCP shim, which is gone. The hub does not declare it (so Rex
// refuses it), and dispatch refuses it too; agents use the agent face (agentface.go).
func TestHubToolsIsGone(t *testing.T) {
	if contains(hubActionNames, "hub.tools") {
		t.Error("the hub still declares hub.tools")
	}
	h, _ := workerTestHub(t)
	_, err := h.dispatch(context.Background(), perform{Action: "hub.tools", Args: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "hub.tools") {
		t.Errorf("hub.tools: %v", err)
	}
}
