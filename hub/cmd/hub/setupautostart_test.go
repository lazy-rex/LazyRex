package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// setup with the autostart flags refuses a hub.json that lazyrex-hub autostart would refuse, and a
// hub.json of another server, before it proposes a file: exit 1, the cause, and every file of the
// HOME (hub.json, managed, profiles, the boot file, init.lua, hooks, skills) unchanged (the
// coordinator, 2026-10-24: an old hub.json with "session" made each autostart exit 2). A first
// install, and an existing hub.json that names the same server by a label, write the autostart.
func TestSetupAutostartChecksTheConfig(t *testing.T) {
	const label, other = "mylab", "unix:///other/rex.sock"
	endpoint := func(s string) (string, error) {
		switch s {
		case "", setupLabServer, label:
			return setupLabServer, nil
		case other:
			return other, nil
		}
		return "", errors.New("no server answered")
	}
	auto := &setupAutostart{session: "lazyrex-hub", pane: startHubPane}
	for _, c := range []struct {
		name, existing, says string // says: "" when setup writes the autostart
	}{
		{name: "first install"},
		{name: "a label of the same server", existing: `{"server":"` + label + `","state":"/s"}`},
		{name: "session", existing: `{"server":"` + setupLabServer + `","state":"/s","session":"session:x"}`, says: `remove "session"`},
		{name: "no state", existing: `{"server":"` + setupLabServer + `"}`, says: "server and state are required"},
		{name: "not JSON", existing: "mine\n", says: "hub.json cannot be read"},
		{name: "no server", existing: `{"state":"/s"}`, says: "hub.json names no server"},
		{name: "another server", existing: `{"server":"` + other + `","state":"/s"}`, says: "names the server " + other + " (" + other + "), not the server that setup targets (" + setupLabServer + ")"},
		{name: "a server that does not answer", existing: `{"server":"unix:///gone/rex.sock","state":"/s"}`, says: "did not answer: no server answered"},
	} {
		e, out := setupTestEnv(t, strings.Repeat("y\n", 12), true)
		e.endpoint, e.autostart = endpoint, auto
		dir := filepath.Join(e.home, ".lazyrex")
		if c.existing != "" {
			os.MkdirAll(dir, 0o755)
			// "/s" is a state in the HOME: setup proposes the managed file under it.
			os.WriteFile(filepath.Join(dir, "hub.json"), []byte(strings.ReplaceAll(c.existing, `"/s"`, strconv.Quote(filepath.Join(dir, "state")))), 0o600)
		}
		before := strings.Join(tree(e.home), "\n")
		code := runSetup(e, dir, false)
		boot := readFile(filepath.Join(e.home, ".config", "rex", "lazyrex_boot.lua"))
		if c.says == "" {
			if code != 0 || !strings.Contains(boot, `pcall(rex.exec.start, {`) {
				t.Errorf("%s: exit %d, want 0 and the autostart\n%s\n%s", c.name, code, out, boot)
			}
			continue
		}
		if code != 1 || !strings.Contains(out.String(), "setup: -autostart: ") || !strings.Contains(out.String(), c.says) ||
			!strings.Contains(out.String(), "Setup writes nothing.") || strings.Contains(out.String(), "[y/N]") {
			t.Errorf("%s: exit %d, want 1 with %q and no question:\n%s", c.name, code, c.says, out)
		}
		if after := strings.Join(tree(e.home), "\n"); after != before {
			t.Errorf("%s: setup changed files:\nbefore:\n%s\nafter:\n%s", c.name, before, after)
		}
	}
}

// Without the autostart flags, an existing hub.json that the hub refuses changes nothing: setup
// writes the boot file with its controls, as before.
func TestSetupWithoutAutostartSkipsTheCheck(t *testing.T) {
	e, out := setupTestEnv(t, strings.Repeat("y\n", 12), true)
	dir := filepath.Join(e.home, ".lazyrex")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "hub.json"), []byte(`{"server":"`+setupLabServer+`","state":`+strconv.Quote(filepath.Join(dir, "state"))+`,"session":"session:x"}`), 0o600)
	if code := runSetup(e, dir, false); code != 0 || strings.Contains(out.String(), "setup: -autostart:") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if boot := readFile(filepath.Join(e.home, ".config", "rex", "lazyrex_boot.lua")); !strings.Contains(boot, "lazyrexboot_start") || strings.Contains(boot, "rex.exec.start") {
		t.Fatalf("boot file:\n%s", boot)
	}
}
