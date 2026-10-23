package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
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

// stop waits for every holder of the restart lock, also one whose status file does not match.
func TestStopWaitsForTheLockHolder(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = 2 * time.Second
	state, _ := canonicalState(t.TempDir())
	holder := exec.Command(os.Args[0], "-test.run=^TestFakeHub$")
	holder.Env = append(os.Environ(), "FAKE_HUB_PLAN=lock", "FAKE_LOCK_STATE="+state)
	out, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	line := make([]byte, 7)
	if _, err := out.Read(line); err != nil || string(line) != "locked\n" {
		t.Fatalf("holder: %q, %v", line, err)
	}
	// No status file: the holder is not trusted, but it holds the state.
	v := &ctlView{rex: ctlFakeRex(t, "exit 0", `echo '{"clients":[]}'`), state: state,
		own: &hubClient{ID: "h1", PID: 1}, holder: holder.Process.Pid}
	var buf bytes.Buffer
	// The hub client is gone and only the lock stays: stop names the wrapper, not a connected hub.
	if code := v.stop(&buf); code != 1 || strings.Contains(buf.String(), "stopped") ||
		!strings.Contains(buf.String(), "the restart wrapper still holds") || strings.Contains(buf.String(), "still connected") {
		t.Errorf("a holder that stays: exit %d\n%s", code, buf.String())
	}
	time.AfterFunc(300*time.Millisecond, func() { holder.Process.Kill() })
	buf.Reset()
	if code := v.stop(&buf); code != 0 || !strings.Contains(buf.String(), "stopped") {
		t.Errorf("a holder that exits: exit %d\n%s", code, buf.String())
	}
}

// One look has one deadline for the listing and every hub.info: three hubs whose hub.info never
// answers end the look within the bound (not three bounds), and each of them is unknown, so the
// look is "unknown", never "not running".
func TestLookHasOneBound(t *testing.T) {
	defer func(d time.Duration) { ctlLookWait = d }(ctlLookWait)
	ctlLookWait = time.Second
	dir := t.TempDir()
	rexPath := filepath.Join(dir, "rex")
	hub := func(id string) string {
		return `{"client_id":"` + id + `","info":{"label":"lazyrex-hub"},"connection_state":"connected"}`
	}
	list := `{"clients":[` + hub("h1") + `,` + hub("h2") + `,` + hub("h3") + `]}`
	script := "#!/bin/sh\ncase \"$*\" in\n*hub.info*) exec sleep 30;;\n*) echo '" + list + "';;\nesac\n"
	if err := os.WriteFile(rexPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	os.Mkdir(state, 0o700)
	start := time.Now()
	v := ctlLook(Config{Server: "unix:///nonexistent/rex.sock", State: state, Rex: rexPath})
	d := time.Since(start)
	// The bound, plus the 500 ms that a killed rex has for its pipes (rexCmdCtx), plus scheduling.
	if d > ctlLookWait+time.Second {
		t.Errorf("the look took %s, want at most about %s", d, ctlLookWait)
	}
	if v.own != nil || len(v.others) != 3 {
		t.Fatalf("own %v, others %v", v.own, v.others)
	}
	for _, h := range v.others {
		if h.State != "" {
			t.Errorf("hub %s has state %q after a cut hub.info, want unknown", h.ID, h.State)
		}
	}
	if code, text := v.statusText(); code != ctlUnknown || !strings.Contains(text, "did not answer hub.info") {
		t.Errorf("status %d\n%s", code, text)
	}
}

// The hub.info calls of a look run at the same time: two hubs that never answer, listed first, do
// not cut the hub of this state, which answers at once. The look ends within the bound.
func TestLookIsNotStarvedByAHungHub(t *testing.T) {
	defer func(d time.Duration) { ctlLookWait = d }(ctlLookWait)
	ctlLookWait = time.Second
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	os.Mkdir(state, 0o700)
	canon, err := canonicalState(state)
	if err != nil {
		t.Fatal(err)
	}
	hub := func(id string) string {
		return `{"client_id":"` + id + `","info":{"label":"lazyrex-hub"},"connection_state":"connected"}`
	}
	list := `{"clients":[` + hub("h1") + `,` + hub("h2") + `,` + hub("h3") + `]}`
	rexPath := filepath.Join(dir, "rex")
	script := "#!/bin/sh\ncase \"$*\" in\n*'-C h3 do hub.info'*) echo '{\"state\":\"" + canon + "\",\"pid\":7}';;\n*hub.info*) exec sleep 30;;\n*) echo '" + list + "';;\nesac\n"
	if err := os.WriteFile(rexPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	v := ctlLook(Config{Server: "unix:///nonexistent/rex.sock", State: state, Rex: rexPath})
	if d := time.Since(start); d > ctlLookWait+time.Second {
		t.Errorf("the look took %s, want at most about %s", d, ctlLookWait)
	}
	if v.own == nil || v.own.ID != "h3" || v.own.PID != 7 {
		t.Fatalf("own %+v, want h3 (pid 7); others %+v", v.own, v.others)
	}
	if len(v.others) != 2 || v.others[0].ID != "h1" || v.others[1].ID != "h2" || v.others[0].State != "" || v.others[1].State != "" {
		t.Errorf("others %+v, want h1 and h2, unknown, in listing order", v.others)
	}
}
