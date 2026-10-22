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

// cliLine is the rex CLI's error line for an answer with code.
func cliLine(code string) string {
	return `Error: stream response "control-1" failed: test answer (status=500 code=` + code + `)`
}

// Only the whole CLI error line with a code in the allowlist is a refusal (timeout audit, row 3).
func TestCLIOutcome(t *testing.T) {
	failed := func(stderr string) error { return &cliError{err: errors.New("exit status 1"), stderr: stderr} }
	stop, relaunch := []string{"refused"}, []string{"refused", "invalid_input"}
	cases := []struct {
		what     string
		err      error
		refusals []string
		code     string
		definite bool
	}{
		{"refused", failed(cliLine("refused") + "\n"), stop, "refused", true},
		{"refused, not allowed", failed(cliLine("refused")), []string{"invalid_input"}, "refused", false},
		{"invalid_input for relaunch", failed(cliLine("invalid_input")), relaunch, "invalid_input", true},
		{"invalid_input for stop", failed(cliLine("invalid_input")), stop, "invalid_input", false},
		{"a transport code", failed(cliLine("timeout")), stop, "timeout", false},
		{"a new code", failed(cliLine("brand_new")), stop, "brand_new", false},
		{"refused in the message only", failed(`Error: stream response "control-1" failed: code=refused (status=503 code=client_unavailable)`), stop, "client_unavailable", false},
		{"text after the parenthesis", failed(cliLine("refused") + " more"), stop, "", false},
		{"another first word", failed("Warning: " + cliLine("refused")[len("Error: "):]), stop, "", false},
		{"earlier lines, then the error line", failed("note: a line\n" + cliLine("refused") + "\n\n"), stop, "refused", true},
		{"the error line, then another line", failed(cliLine("refused") + "\nnote: a line\n"), stop, "", false},
		{"malformed", failed("garbage"), stop, "", false},
		{"a killed rex with no output", failed(""), stop, "", false},
		{"not a cliError", errors.New(cliLine("refused")), stop, "", false},
		{"no error, the output looks like a refusal", nil, stop, "", false},
	}
	for _, c := range cases {
		code, definite := cliOutcome(c.err, c.refusals)
		if code != c.code || definite != c.definite {
			t.Errorf("%s: (%q, %v), want (%q, %v)", c.what, code, definite, c.code, c.definite)
		}
	}
}

// stopRex is a fake rex: hub.admin.shutdown prints shutdownErr on stderr and exits 1 (or exits 0
// for ""); a listing shows the hub h1 until the file gone exists. With failAfter, every listing
// after the first fails.
func stopRex(t *testing.T, shutdownErr string, failAfter bool) (rex, gone string) {
	t.Helper()
	dir := t.TempDir()
	gone = filepath.Join(dir, "gone")
	count := filepath.Join(dir, "count")
	shutdown := "exit 0"
	if shutdownErr != "" {
		shutdown = "echo '" + shutdownErr + "' >&2; exit 1"
	}
	listed := `echo '{"clients":[{"client_id":"h1","info":{"label":"lazyrex-hub"},"connection_state":"connected"}]}'`
	list := "if [ -f " + gone + " ]; then echo '{\"clients\":[]}'; else " + listed + "; fi"
	if failAfter {
		list = "if [ -f " + count + " ]; then echo 'Error: dial failed' >&2; exit 1; fi; touch " + count + "; " + listed
	}
	return ctlFakeRex(t, shutdown, list), gone
}

func runStop(t *testing.T, rex string) (int, string, time.Duration) {
	t.Helper()
	v := &ctlView{rex: rex, state: "/state", own: &hubClient{ID: "h1", PID: 1}}
	var out bytes.Buffer
	start := time.Now()
	code := v.stop(&out)
	return code, out.String(), time.Since(start)
}

// The shutdown request is sent once and never resent. A failed call whose outcome is unknown does
// not end stop: the hub can still get the request and stop. Only the hub's refusal ends it at once.
// Before, every failed call exited 1 at once, also when the hub stopped right after.
func TestStopWaitsAfterAnUnknownOutcome(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = 2 * time.Second
	for _, code := range []string{"timeout", "brand_new", "failed"} {
		rex, gone := stopRex(t, cliLine(code), false)
		time.AfterFunc(300*time.Millisecond, func() { os.WriteFile(gone, nil, 0o600) })
		if exit, out, _ := runStop(t, rex); exit != 0 || !strings.Contains(out, "stopped") {
			t.Errorf("code=%s, then the hub goes: exit %d\n%s", code, exit, out)
		}
	}
	rex, gone := stopRex(t, "garbage", false)
	time.AfterFunc(300*time.Millisecond, func() { os.WriteFile(gone, nil, 0o600) })
	if exit, out, _ := runStop(t, rex); exit != 0 || !strings.Contains(out, "stopped") {
		t.Errorf("malformed output, then the hub goes: exit %d\n%s", exit, out)
	}
}

// At the deadline stop exits 1 with the lead's text. It names a still connected hub, with the
// failed shutdown call, only from a successful listing.
func TestStopDeadlineText(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = time.Second
	const head = "the hub did not stop within 1 s; the shutdown request may still apply: run lazyrex-hub status"

	rex, _ := stopRex(t, cliLine("timeout"), false)
	exit, out, _ := runStop(t, rex)
	if exit != 1 || !strings.Contains(out, head) || !strings.Contains(out, "the hub client is still connected") || !strings.Contains(out, "code=timeout") {
		t.Errorf("the hub stays: exit %d\n%s", exit, out)
	}

	rex, _ = stopRex(t, cliLine("timeout"), true)
	exit, out, _ = runStop(t, rex)
	if exit != 1 || !strings.Contains(out, head) || strings.Contains(out, "still connected") ||
		!strings.Contains(out, "the last successful check") || !strings.Contains(out, "dial failed") || !strings.Contains(out, "code=timeout") {
		t.Errorf("the listings fail after one that showed the hub: exit %d\n%s", exit, out)
	}
}

// The hub's refusal ends stop at once with the hub's text: the request did not apply.
func TestStopRefusedExitsAtOnce(t *testing.T) {
	defer func(d time.Duration) { ctlStopWait = d }(ctlStopWait)
	ctlStopWait = 2 * time.Second
	rex, _ := stopRex(t, `Error: stream response "control-1" failed: admin actions are refused from agent and plugin panes (status=500 code=refused)`, false)
	exit, out, d := runStop(t, rex)
	if exit != 1 || !strings.Contains(out, "admin actions are refused") || strings.Contains(out, "may still apply") {
		t.Errorf("refused: exit %d\n%s", exit, out)
	}
	if d > time.Second {
		t.Errorf("refused: stop took %s, want an exit at once", d)
	}
}
