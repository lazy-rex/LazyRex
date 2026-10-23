package main

import (
	"strings"
	"testing"
)

// A test.files entry has args and prompts. It has no home: the hub's home plugin is not a test
// setting, and an unknown key is refused (decodeManifest).
func TestManifestFilesKeys(t *testing.T) {
	if _, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{"files":{"x.lua":{"args":{"p":{"k":"v"}},"prompts":false}}}}`); err != nil {
		t.Errorf("args and prompts: %v", err)
	}
	if _, err := manifestIn(t, `{"name":"p","main":"p.lua","test":{"files":{"x.lua":{"home":"p"}}}}`); err == nil || !strings.Contains(err.Error(), `unknown field "home"`) {
		t.Errorf("home: got %v, want the unknown field refusal", err)
	}
}
