package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hub controls run an argv: the binary, the config, and -real-server only for the real server.
func TestSetupBootHubControls(t *testing.T) {
	exe, cfg := `/the bin/lazyrex-hub`, `/a "dir"/hub.json`
	lua := setupBootLua(exe, "/d", "", hubControls{config: cfg}, nil)
	c := `"-config", ` + luaString(cfg)
	for _, want := range []string{
		`run = lazyrex_pane("LazyRex: Set Up", "LazyRex setup", { ` + luaString(exe) + `, "setup", "-dir", "/d" }),`,
		`name = "lazyrexboot_start"`, `title = "LazyRex: Start Hub"`,
		`run = lazyrex_pane("LazyRex: Start Hub", "LazyRex hub", { ` + luaString(exe) + `, ` + c + `, "-restart" }),`,
		`name = "lazyrexboot_stop"`, `title = "LazyRex: Stop Hub"`,
		`run = lazyrex_pane("LazyRex: Stop Hub", "LazyRex stop", { ` + luaString(exe) + `, "stop", ` + c + ` }),`,
		`name = "lazyrexboot_status"`, `title = "LazyRex: Hub Status"`,
		`run = lazyrex_pane("LazyRex: Hub Status", "LazyRex status", { ` + luaString(exe) + `, "status", ` + c + ` }),`,
		`exit = { on_completion = false }`, `login = false`,
	} {
		if !strings.Contains(lua, want) {
			t.Errorf("no %s in:\n%s", want, lua)
		}
	}
	if strings.Contains(lua, "-real-server") {
		t.Errorf("-real-server for a lab server:\n%s", lua)
	}
	if n := strings.Count(setupBootLua(exe, "/d", "", hubControls{config: cfg, real: true}, nil), c+`, "-real-server"`); n != 3 {
		t.Errorf("%d controls with -real-server, want 3", n)
	}
	only := setupBootLua(exe, "/d", "", hubControls{config: cfg, err: "hub.json names no server"}, nil)
	if !strings.Contains(only, "lazyrexboot_setup") || strings.Contains(only, "lazyrexboot_start") || strings.Contains(only, "-config") {
		t.Errorf("controls without a server:\n%s", only)
	}
}

// -real-server comes from the hub.json that the controls read. Setup never changes an existing
// one, and its server can differ from the server that setup found. A file without a readable
// server gets no controls, and the report says why.
func TestSetupControlsFollowTheConfigFile(t *testing.T) {
	const other = "unix:///other/rex.sock"
	for _, c := range []struct {
		name, existing string
		real           bool   // the boot file has -real-server
		left           string // the controls are left out with this reason
	}{
		{name: "new"},
		{name: "existing, another server", existing: `{"server":"` + other + `"}`, real: true},
		{name: "existing, not JSON", existing: "mine\n", left: "hub.json cannot be read"},
		{name: "existing, no server", existing: `{"state":"/s","managed_plugins":"plugins.json"}`, left: "hub.json names no server"},
	} {
		e, out := setupTestEnv(t, strings.Repeat("y\n", 9), true)
		dir := filepath.Join(e.home, ".lazyrex")
		var asked []string
		e.isReal = func(s string) bool { asked = append(asked, s); return s == other }
		if c.existing != "" {
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, "hub.json"), []byte(c.existing), 0o600)
		}
		if got := runSetup(e, dir, false); got != 0 {
			t.Fatalf("%s: exit %d\n%s", c.name, got, out)
		}
		boot := readFile(filepath.Join(e.home, ".config", "rex", "lazyrex_boot.lua"))
		switch {
		case c.left != "":
			if strings.Contains(boot, "lazyrexboot_start") || !strings.Contains(out.String(), "Start Hub, Stop Hub and Hub Status are left out: "+c.left) || len(asked) != 0 {
				t.Errorf("%s: asked %v\n%s\n%s", c.name, asked, boot, out)
			}
		default:
			want := setupLabServer
			if c.existing != "" {
				want = other
			}
			if strings.Join(asked, ",") != want || !strings.Contains(boot, `"-config", `+luaString(filepath.Join(dir, "hub.json"))) ||
				strings.Contains(boot, "-real-server") != c.real {
				t.Errorf("%s: asked %v\n%s", c.name, asked, boot)
			}
		}
	}
}
