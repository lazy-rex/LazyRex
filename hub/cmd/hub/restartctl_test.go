package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
