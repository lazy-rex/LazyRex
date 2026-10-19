package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginArgsConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(args string) (Config, error) {
		p := filepath.Join(dir, "hub.json")
		body := `{"server":"unix:///tmp/lrt-x/rex.sock","state":"` + dir + `","agents":[{"id":"user","name":"User"}],
			"plugins":[{"name":"w","path":"w.lua","args":` + args + `}]}`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return loadConfig(p, false)
	}
	c, err := write(`{"worktrees_root":"/tmp/x"}`)
	if err != nil || c.Plugins[0].Args["worktrees_root"] != "/tmp/x" {
		t.Fatalf("valid args: %v %v", c.Plugins, err)
	}
	for _, bad := range []string{`{"instance_id":"x"}`, `{"hub_client_id":"x"}`, `{"plugin_session":"x"}`, `{"Bad-Key":"x"}`, `{"":"x"}`} {
		if _, err := write(bad); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%s: got %v", bad, err)
		}
	}
}
