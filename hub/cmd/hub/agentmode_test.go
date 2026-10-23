package main

import (
	"strings"
	"testing"
)

// The roster mode is gone: a manifest with the key test.agents (a list, an empty list or null) is
// refused, so no suite runs its agents in a mode that no longer exists.
func TestManifestAgentsRefused(t *testing.T) {
	for _, agents := range []string{`[]`, `[{"id":"alice","name":"Alice"}]`, `null`} {
		_, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{"agents":`+agents+`}}`)
		if err == nil || !strings.Contains(err.Error(), "test.agents is removed") {
			t.Errorf("agents %s: %v", agents, err)
		}
	}
	if _, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{}}`); err != nil {
		t.Fatalf("no test.agents key: %v", err)
	}
}
