package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A failed observation is unknown, never not running.
func TestStatusUnknownWhenNotSeen(t *testing.T) {
	expect := func(what string, v *ctlView, want string) {
		t.Helper()
		if code, text := v.statusText(); code != ctlUnknown || !strings.HasPrefix(text, "unknown: ") || !strings.Contains(text, want) {
			t.Errorf("%s: %d\n%s", what, code, text)
		}
	}
	expect("Rex cannot be asked", &ctlView{state: "/state", rexErr: errors.New("dial failed")}, "cannot be asked")
	expect("a hub without hub.info", &ctlView{state: "/state", others: []hubClient{{ID: "c1"}}}, "did not answer hub.info")

	dir := t.TempDir()
	cfg := Config{Server: "unix:///nonexistent/rex.sock"}
	loop := filepath.Join(dir, "loop")
	os.Symlink(loop, loop)
	cfg.State = loop
	expect("a state that cannot be resolved", ctlLook(cfg), "cannot be resolved")

	state := filepath.Join(dir, "state")
	os.Mkdir(state, 0o700)
	os.WriteFile(filepath.Join(state, "restart.lock"), nil, 0)
	cfg.State = state
	expect("a restart.lock that cannot be read", ctlLook(cfg), "restart.lock")
}

// ctlFakeRex writes a shell script that stands in for rex: shutdown is the reply to hub.admin.shutdown,
// list the reply to everything else.
func ctlFakeRex(t *testing.T, shutdown, list string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rex")
	script := "#!/bin/sh\ncase \"$*\" in\n*hub.admin.shutdown*) " + shutdown + ";;\n*) " + list + ";;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// stop reports success only when a client listing shows that the hub is gone, and all of stop has
// one deadline.
func TestStopNeedsASeenExit(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = time.Second
	stop := func(what, rex string) {
		t.Helper()
		v := &ctlView{rex: rex, state: "/state", own: &hubClient{ID: "h1", PID: 1}}
		var out bytes.Buffer
		start := time.Now()
		if code := v.stop(&out); code != 1 || strings.Contains(out.String(), "stopped") {
			t.Errorf("%s: exit %d\n%s", what, code, out.String())
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s: stop took %s, its bound is %s", what, d, ctlStopWait)
		}
	}
	stop("the listing fails", ctlFakeRex(t, "exit 0", "exit 1"))
	stop("the listing hangs", ctlFakeRex(t, "exit 0", "exec sleep 30"))
	stop("the shutdown hangs", ctlFakeRex(t, "exec sleep 30", "exit 1"))
	// The hub is still listed.
	stop("the hub stays", ctlFakeRex(t, "exit 0", `echo '{"clients":[{"client_id":"h1","info":{"label":"lazyrex-hub"},"connection_state":"connected"}]}'`))
}
