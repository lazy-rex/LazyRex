package main

import (
	"strings"
	"testing"
)

// The suite's agent mode comes from the manifest key test.agents (docs/testing.md): present, even as
// an empty list, is the roster mode; absent is the identity mode.
func TestManifestAgentMode(t *testing.T) {
	for _, c := range []struct{ name, test, want string }{
		{"no test.agents key", `{}`, "identity"},
		{"an empty list", `{"agents":[]}`, "roster"},
		{"a roster agent", `{"agents":[{"id":"alice","name":"Alice"}]}`, "roster"},
	} {
		dir, err := manifestIn(t, `{"name":"p","main":"p.lua","test":`+c.test+`}`)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		m, err := loadManifest(dir)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := m.agentMode(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A null test.agents decodes as an absent key, so the manifest refuses it: the mode is never a
// silent identity mode for a suite that names the key.
func TestManifestAgentsNullRefused(t *testing.T) {
	_, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{"agents":null}}`)
	if err == nil || !strings.Contains(err.Error(), "test.agents") {
		t.Fatalf("agents null: %v", err)
	}
}
