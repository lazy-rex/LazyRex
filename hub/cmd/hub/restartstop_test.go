package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// Only the hub's answer with a code in the allowlist is a refusal. Everything else is unknown.
func TestPerformOutcome(t *testing.T) {
	answer := func(code string) error {
		return fmt.Errorf("client.perform: %w", &rex.RexError{Code: code, Message: "test"})
	}
	stop, relaunch := []string{"refused"}, []string{"refused", "invalid_input"}
	cases := []struct {
		what     string
		err      error
		refusals []string
		code     string
		definite bool
	}{
		{"refused", answer("refused"), stop, "refused", true},
		{"refused, not allowed", answer("refused"), []string{"invalid_input"}, "refused", false},
		{"invalid_input for relaunch", answer("invalid_input"), relaunch, "invalid_input", true},
		{"invalid_input for stop", answer("invalid_input"), stop, "invalid_input", false},
		{"a Rex code", answer("client_unavailable"), stop, "client_unavailable", false},
		{"a lost connection", answer("disconnected"), stop, "disconnected", false},
		{"the local deadline", context.DeadlineExceeded, stop, "", false},
		{"a dial error", errors.New("dial /x: connection refused"), stop, "", false},
		{"no error", nil, stop, "", false},
	}
	for _, c := range cases {
		code, definite := performOutcome(c.err, c.refusals)
		if code != c.code || definite != c.definite {
			t.Errorf("%s: (%q, %v), want (%q, %v)", c.what, code, definite, c.code, c.definite)
		}
	}
}

// listRex is a fake rex for the listings: it shows the hub h1 until the file gone exists. With
// failAfter, every listing after the first fails.
func listRex(t *testing.T, failAfter bool) (rex, gone string) {
	t.Helper()
	dir := t.TempDir()
	gone = filepath.Join(dir, "gone")
	count := filepath.Join(dir, "count")
	listed := `echo '{"clients":[{"client_id":"h1","info":{"label":"lazyrex-hub"},"connection_state":"connected"}]}'`
	list := "if [ -f " + gone + " ]; then echo '{\"clients\":[]}'; else " + listed + "; fi"
	if failAfter {
		list = "if [ -f " + count + " ]; then echo 'Error: dial failed' >&2; exit 1; fi; touch " + count + "; " + listed
	}
	return ctlFakeRex(t, "exit 9", list), gone
}

// runStop runs stop with the listings of rexPath and the shutdown sent to the server at socket.
func runStop(t *testing.T, rexPath, socket string) (int, string, time.Duration) {
	t.Helper()
	v := &ctlView{rex: rexPath, cfg: Config{Server: "unix://" + socket}, state: "/state", own: &hubClient{ID: "h1", PID: 1}}
	var out bytes.Buffer
	start := time.Now()
	code := v.stop(&out)
	return code, out.String(), time.Since(start)
}

// shutdownRex is a performRex that answers hub.admin.shutdown with code ("" is a success).
func shutdownRex(t *testing.T, code string) *performRex {
	return newPerformRex(t, func(c performCall) (any, string) {
		if c.Action != "hub.admin.shutdown" || c.Client != "h1" || string(c.Args) != "{}" {
			return nil, "test_wrong_call"
		}
		return map[string]any{"stopping": true}, code
	})
}

// The hub's refusal ends stop at once with the hub's error: the request did not apply.
func TestStopRefusedExitsAtOnce(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = 2 * time.Second
	list, _ := listRex(t, false)
	fr := shutdownRex(t, "refused")
	exit, out, d := runStop(t, list, fr.Socket)
	if exit != 1 || !strings.Contains(out, "refused: test") || strings.Contains(out, "may still apply") {
		t.Errorf("refused: exit %d\n%s", exit, out)
	}
	if d > time.Second {
		t.Errorf("refused: stop took %s, want an exit at once", d)
	}
	if a := fr.actions(); len(a) != 1 {
		t.Errorf("refused: %d sends, want 1", len(a))
	}
}

// The shutdown request is sent once and never again. A failed call whose outcome is unknown does
// not end stop: Rex can still deliver the request, and the hub can still stop.
func TestStopWaitsAfterAnUnknownOutcome(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = 2 * time.Second
	for _, code := range []string{"timeout", "client_unavailable", "invalid_input", "failed", dropConnection} {
		list, gone := listRex(t, false)
		fr := shutdownRex(t, code)
		time.AfterFunc(300*time.Millisecond, func() { os.WriteFile(gone, nil, 0o600) })
		if exit, out, _ := runStop(t, list, fr.Socket); exit != 0 || !strings.Contains(out, "stopped") {
			t.Errorf("%s, then the hub goes: exit %d\n%s", code, exit, out)
		}
		if a := fr.actions(); len(a) != 1 {
			t.Errorf("%s: %d sends, want 1", code, len(a))
		}
	}
}

// At the deadline stop exits 1 with the lead's text. It names a still connected hub, with the
// failed shutdown call, only from a successful listing after the send.
func TestStopDeadlineText(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = time.Second
	const head = "the hub did not stop within 1 s; the shutdown request may still apply: run lazyrex-hub status"

	list, _ := listRex(t, false)
	exit, out, _ := runStop(t, list, shutdownRex(t, "timeout").Socket)
	if exit != 1 || !strings.Contains(out, head) || !strings.Contains(out, "the hub client is still connected") || !strings.Contains(out, "timeout: test") {
		t.Errorf("the hub stays: exit %d\n%s", exit, out)
	}

	list, _ = listRex(t, true)
	exit, out, _ = runStop(t, list, shutdownRex(t, "timeout").Socket)
	if exit != 1 || !strings.Contains(out, head) || strings.Contains(out, "still connected") ||
		!strings.Contains(out, "the last successful check") || !strings.Contains(out, "dial failed") || !strings.Contains(out, "timeout: test") {
		t.Errorf("the listings fail after one that showed the hub: exit %d\n%s", exit, out)
	}
}

// A shutdown call that uses the whole bound leaves no listing after the send: stop reports the
// status look from before the send, never a connected hub (review of 81e3451, codex-28).
func TestStopHungShutdownHasNoFreshLook(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = 300 * time.Millisecond
	fr := newPerformRex(t, func(performCall) (any, string) { time.Sleep(time.Hour); return nil, "" })
	exit, out, d := runStop(t, ctlFakeRex(t, "exit 9", `echo '{"clients":[]}'`), fr.Socket)
	if exit != 1 || strings.Contains(out, "still connected") || !strings.Contains(out, "the last successful check") {
		t.Errorf("the shutdown hangs: exit %d\n%s", exit, out)
	}
	if d > ctlStopWait+time.Second {
		t.Errorf("the shutdown hangs: stop took %s", d)
	}
}

// rawPeer is a control socket whose peer runs script on each accepted connection, before any
// WebSocket upgrade.
func rawPeer(t *testing.T, script func(c net.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rp") // a unix socket path must be short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "rex.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go script(c)
		}
	}()
	return socket
}

// wsPeer is a control socket whose peer upgrades and then runs script.
func wsPeer(t *testing.T, script func(c *websocket.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "rex.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"rex.control.v1"}})
		if err == nil {
			script(c)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return socket
}

// A Rex that stalls in the handshake or stops reading cannot hold stop past its bound: the dial,
// the hello and the call share the bound, and the connection closes at once.
func TestStopHandshakeStallsKeepTheBound(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = 500 * time.Millisecond
	peers := map[string]string{
		"no upgrade answer": rawPeer(t, func(net.Conn) { time.Sleep(time.Hour) }),
		"no welcome": wsPeer(t, func(c *websocket.Conn) {
			c.Read(context.Background())
			time.Sleep(time.Hour)
		}),
		"a silent peer after the welcome": wsPeer(t, func(c *websocket.Conn) {
			c.Read(context.Background())
			c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"welcome"}`))
			time.Sleep(time.Hour) // reads nothing more: the request and the close frame wait
		}),
	}
	for what, socket := range peers {
		list, _ := listRex(t, false)
		exit, out, d := runStop(t, list, socket)
		if exit != 1 || !strings.Contains(out, "may still apply") || strings.Contains(out, "stopped") {
			t.Errorf("%s: exit %d\n%s", what, exit, out)
		}
		if d > ctlStopWait+time.Second {
			t.Errorf("%s: stop took %s, its bound is %s", what, d, ctlStopWait)
		}
	}
}

// A restart.lock that cannot be read is not a held lock, and not a stopped hub.
func TestStopUnreadableLock(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = time.Second
	state, _ := canonicalState(t.TempDir())
	os.WriteFile(filepath.Join(state, "restart.lock"), nil, 0)
	v := &ctlView{rex: ctlFakeRex(t, "exit 9", `echo '{"clients":[]}'`), cfg: Config{Server: "unix://" + shutdownRex(t, "").Socket},
		state: state, own: &hubClient{ID: "h1", PID: 1}, holder: 1}
	var out bytes.Buffer
	if code := v.stop(&out); code != 1 || strings.Contains(out.String(), "stopped") ||
		strings.Contains(out.String(), "still holds") || !strings.Contains(out.String(), "restart lock cannot be read") {
		t.Errorf("an unreadable lock: exit %d\n%s", code, out.String())
	}
}
