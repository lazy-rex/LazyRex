package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// autostartFakeRex is a rex that answers the calls of autostart from fixed text, and logs each call
// to the file that it returns.
func autostartFakeRex(t *testing.T, clients, sessions string, blocks map[string]string) (rex, calls string) {
	t.Helper()
	dir := t.TempDir()
	rex, calls = filepath.Join(dir, "rex"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + shellWord(calls) + "\ncase \"$*\" in\n" +
		"*\"client ls --json\"*) echo " + shellWord(clients) + ";;\n" +
		"*\" ls --json\"*) echo " + shellWord(sessions) + ";;\n"
	for s, b := range blocks {
		script += "*\"block ls -s " + s + " --json\"*) echo " + shellWord(b) + ";;\n"
	}
	script += "*\"api call\"*) echo '{}';;\n*) echo \"unexpected: $*\" >&2; exit 1;;\nesac\n"
	if err := os.WriteFile(rex, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return rex, calls
}

// autostart starts the hub pane only when (a) no hub is connected and (b) no hub pane exists: a pane
// with the given label or the Start Hub label. A lost hub client does not count. The pane goes into
// a new window of the session with the given label, or into a new session with that label.
func TestAutostartConditions(t *testing.T) {
	const (
		noHub    = `{"clients":[{"client_id":"client:old","info":{"label":"lazyrex-hub"},"connection_state":"lost"}]}`
		hub      = `{"clients":[{"client_id":"client:h","info":{"label":"lazyrex-hub"},"connection_state":"connected"}]}`
		sessions = `{"sessions":[{"session_id":"session:a","label":"work"},{"session_id":"session:b","label":"lazyrex-live-keep"}]}`
		shells   = `{"blocks":[{"block_id":"block:1","label":"shell"}]}`
	)
	argv := []string{"/usr/bin/env", "DEV_REAL=1", "/r/dev.sh", "/r/hub-live.json"}
	for _, c := range []struct {
		name, clients, sessions string
		blocks                  map[string]string
		session, want, call     string // call: the api call that autostart makes, or ""
	}{
		{"a hub runs", hub, sessions, map[string]string{"session:a": shells, "session:b": shells}, "lazyrex-live-keep",
			`not started: a hub runs (client:h)`, ""},
		{"the pane exists", noHub, sessions, map[string]string{"session:a": shells, "session:b": `{"blocks":[{"label":"lazyrex-dev-process"}]}`}, "lazyrex-live-keep",
			`not started: a pane labeled "lazyrex-dev-process" is in session "lazyrex-live-keep"`, ""},
		{"a Start Hub pane exists", noHub, sessions, map[string]string{"session:a": `{"blocks":[{"label":"LazyRex hub"}]}`, "session:b": shells}, "lazyrex-live-keep",
			`not started: a pane labeled "LazyRex hub" is in session "work"`, ""},
		{"into the session", noHub, sessions, map[string]string{"session:a": shells, "session:b": shells}, "lazyrex-live-keep",
			`started: pane "lazyrex-dev-process" in session "lazyrex-live-keep"`, "api call session.new_window "},
		{"a new session", noHub, `{"sessions":[]}`, nil, "lazyrex-live-keep",
			`started: pane "lazyrex-dev-process" in session "lazyrex-live-keep"`, "api call session.create "},
	} {
		rex, calls := autostartFakeRex(t, c.clients, c.sessions, c.blocks)
		v := &ctlView{rex: rex, cfg: Config{Server: "unix:///lab/rex.sock"}}
		got, err := v.autostart(c.session, "lazyrex-dev-process", argv)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
			continue
		}
		b, _ := os.ReadFile(calls)
		log := string(b)
		i := strings.Index(log, "api call")
		if c.call == "" {
			if i >= 0 {
				t.Errorf("%s: a pane was started:\n%s", c.name, log)
			}
			continue
		}
		if !strings.Contains(log, c.call) {
			t.Fatalf("%s: no %s in:\n%s", c.name, c.call, log)
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(log[i+len(c.call):])), &req); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, log)
		}
		layout, _ := req["layout"].(map[string]any)
		if c.call == "api call session.create " {
			if req["label"] != c.session {
				t.Errorf("%s: session label %v", c.name, req["label"])
			}
			layout = req["initial_windows"].([]any)[0].(map[string]any)["layout"].(map[string]any)
		} else if req["session_id"] != "session:b" {
			t.Errorf("%s: session %v", c.name, req["session_id"])
		}
		blk := layout["block"].(map[string]any)
		opts := blk["options"].(map[string]any)
		if blk["label"] != "lazyrex-dev-process" || opts["login"] != false || opts["exit"].(map[string]any)["on_completion"] != false ||
			fmtJSON(opts["command"]) != fmtJSON(argv) {
			t.Errorf("%s: layout %v", c.name, layout)
		}
	}
}

func fmtJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A server that does not answer within autostartWait: not started, with the cause.
func TestAutostartNoServer(t *testing.T) {
	defer func(d time.Duration) { autostartWait = d }(autostartWait)
	autostartWait = 500 * time.Millisecond
	rex := filepath.Join(t.TempDir(), "rex")
	os.WriteFile(rex, []byte("#!/bin/sh\necho 'no server' >&2\nexit 1\n"), 0o700)
	v := &ctlView{rex: rex, cfg: Config{Server: "unix:///lab/rex.sock"}}
	if _, err := v.autostart("s", "p", []string{"/bin/true"}); err == nil || !strings.Contains(err.Error(), "did not answer within 500ms") || !strings.Contains(err.Error(), "no server") {
		t.Fatalf("%v", err)
	}
}

// setup -autostart: the boot file runs lazyrex-hub autostart detached at each load, with the Start
// Hub command or the -autostart-cmd argv, and Set Up keeps the flags. Without a hub config, no
// autostart is written.
func TestSetupBootAutostart(t *testing.T) {
	exe, cfg := `/the bin/lazyrex-hub`, `/d/hub.json`
	ctl := hubControls{config: cfg, real: true}
	if lua := setupBootLua(exe, "/d", "", ctl, nil); strings.Contains(lua, "autostart") {
		t.Errorf("an autostart without -autostart:\n%s", lua)
	}
	c := `"-config", "/d/hub.json", "-real-server"`
	lua := setupBootLua(exe, "/d", "", ctl, &setupAutostart{session: "lazyrex-hub", pane: "LazyRex hub"})
	for _, want := range []string{
		`run = lazyrex_pane("LazyRex: Set Up", "LazyRex setup", { "/the bin/lazyrex-hub", "setup", "-dir", "/d", "-autostart", "-autostart-session", "lazyrex-hub", "-autostart-pane", "LazyRex hub" }),`,
		`pcall(rex.exec.start, { "/the bin/lazyrex-hub", "autostart", ` + c + `, "-session", "lazyrex-hub", "-pane", "LazyRex hub", "--", "/the bin/lazyrex-hub", ` + c + `, "-restart" }, { detach = true, timeout = 120 })`,
	} {
		if !strings.Contains(lua, want) {
			t.Errorf("no %s in:\n%s", want, lua)
		}
	}
	auto := &setupAutostart{argv: []string{"/usr/bin/env", "DEV_REAL=1", `/r "x"/dev.sh`}, session: "lazyrex-live-keep", pane: "lazyrex-dev-process"}
	lua = setupBootLua(exe, "/d", "", ctl, auto)
	for _, want := range []string{
		`"-autostart", "-autostart-cmd", ` + luaString(`["/usr/bin/env","DEV_REAL=1","/r \"x\"/dev.sh"]`) + `, "-autostart-session", "lazyrex-live-keep", "-autostart-pane", "lazyrex-dev-process" }),`,
		`"-session", "lazyrex-live-keep", "-pane", "lazyrex-dev-process", "--", "/usr/bin/env", "DEV_REAL=1", ` + luaString(`/r "x"/dev.sh`) + ` }, { detach = true, timeout = 120 })`,
	} {
		if !strings.Contains(lua, want) {
			t.Errorf("no %s in:\n%s", want, lua)
		}
	}
	if lua := setupBootLua(exe, "/d", "", hubControls{config: cfg, err: "hub.json names no server"}, auto); strings.Contains(lua, "rex.exec.start") || !strings.Contains(lua, `"-autostart-cmd"`) {
		t.Errorf("without a hub config: no autostart, and Set Up keeps the flags:\n%s", lua)
	}
	for _, args := range [][]string{{"-autostart-cmd", "/r/dev.sh"}, {"-autostart-cmd", "[]"}, {"-autostart", "-autostart-pane", ""}} {
		if got := setupMain(args); got != 2 {
			t.Errorf("setup %q: exit %d, want 2", args, got)
		}
	}
}
