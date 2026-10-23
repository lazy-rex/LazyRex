package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// restartLook is a look of the restart tests: the hub of /state (nil: none) under the wrapper with
// status st (nil: no wrapper).
func restartLook(socket string, own *hubClient, st *restartStatus) *ctlView {
	v := &ctlView{cfg: Config{Server: "unix://" + socket}, state: "/state", own: own}
	if st != nil {
		v.holder, v.status, v.trusted = st.PID, st, true
	}
	return v
}

// The wrapper of the restart tests: pid 50, at exec 3.
func restartWrapper(exec int, last *lastExec) *restartStatus {
	return &restartStatus{Version: 1, PID: 50, Start: "t50", Phase: "running", Exec: exec, LastExec: last}
}

var restartOld = hubClient{ID: "h1", State: "/state", PID: 100, Exec: 3, Commit: "aaa"}

// relaunchRex is a performRex that answers hub.admin.relaunch {"expect_exec":3} with code ("": a
// success).
func relaunchRex(t *testing.T, code string) *performRex {
	return newPerformRex(t, func(c performCall) (any, string) {
		if c.Action != "hub.admin.relaunch" || c.Client != "h1" || string(c.Args) != `{"expect_exec":3}` {
			return nil, "test_wrong_call"
		}
		return map[string]any{"relaunching": true, "exec": 3}, code
	})
}

// runRestart runs restart from the look first, with later looks from looks (the last one repeats).
func runRestart(t *testing.T, first *ctlView, looks ...*ctlView) (int, string, time.Duration) {
	t.Helper()
	var out bytes.Buffer
	i := 0
	start := time.Now()
	code := first.restart(&out, 3, func() *ctlView {
		v := looks[min(i, len(looks)-1)]
		i++
		return v
	})
	return code, out.String(), time.Since(start)
}

// The checks before the send only read: no hub, a hub without its wrapper, or another exec count
// exits 1 and sends nothing.
func TestRestartChecksBeforeTheSend(t *testing.T) {
	fr := relaunchRex(t, "")
	cases := []struct {
		what string
		v    *ctlView
		want string
	}{
		{"no hub", restartLook(fr.Socket, nil, nil), "no hub of this config runs"},
		{"no wrapper", restartLook(fr.Socket, &restartOld, nil), "runs without a restart wrapper"},
		{"a wrapper in backoff", restartLook(fr.Socket, &restartOld, &restartStatus{Version: 1, PID: 50, Start: "t50", Phase: "backoff"}), "runs without a restart wrapper"},
		{"another exec", restartLook(fr.Socket, &hubClient{ID: "h1", PID: 100, Exec: 4}, restartWrapper(4, nil)), "runs exec 4, not 3"},
	}
	for _, c := range cases {
		exit, out, _ := runRestart(t, c.v, c.v)
		if exit != 1 || !strings.Contains(out, c.want) || !strings.Contains(out, "nothing was sent") {
			t.Errorf("%s: exit %d\n%s", c.what, exit, out)
		}
	}
	if a := fr.actions(); len(a) != 0 {
		t.Errorf("%d sends before a failed check, want 0", len(a))
	}
}

// The hub's refusal (refused, or invalid_input) ends restart at once with the hub's text.
func TestRestartRefusedExitsAtOnce(t *testing.T) {
	defer func(d time.Duration) { ctlRestartWait = d }(ctlRestartWait)
	ctlRestartWait = 2 * time.Second
	for _, code := range []string{"refused", "invalid_input"} {
		fr := relaunchRex(t, code)
		v := restartLook(fr.Socket, &restartOld, restartWrapper(3, nil))
		exit, out, d := runRestart(t, v, v)
		if exit != 1 || !strings.Contains(out, "hub.admin.relaunch: client.perform: "+code+": test") {
			t.Errorf("%s: exit %d\n%s", code, exit, out)
		}
		if d > time.Second {
			t.Errorf("%s: restart took %s, want an exit at once", code, d)
		}
		if a := fr.actions(); len(a) != 1 {
			t.Errorf("%s: %d sends, want 1", code, len(a))
		}
	}
}

// After an accepted send, or one whose outcome is unknown, restart watches. Only a look with the same
// wrapper process ready at exec 4 and a new hub process at exec 4 is a restart. The send is never
// repeated.
func TestRestartConfirmsTheNewHub(t *testing.T) {
	defer func(d time.Duration) { ctlRestartWait = d }(ctlRestartWait)
	ctlRestartWait = 3 * time.Second
	ok := &lastExec{Target: 4, Outcome: "ok"}
	newHub := &hubClient{ID: "h2", State: "/state", PID: 101, Exec: 4, Commit: "bbb"}
	for _, code := range []string{"", "timeout", "client_unavailable", "failed", dropConnection} {
		fr := relaunchRex(t, code)
		looks := []*ctlView{
			restartLook(fr.Socket, &restartOld, restartWrapper(3, nil)),                                                  // the old hub still runs
			restartLook(fr.Socket, nil, &restartStatus{Version: 1, PID: 50, Start: "t50", Phase: "exec", ExecTarget: 4}), // the exec
			restartLook(fr.Socket, nil, &restartStatus{Version: 1, PID: 50, Start: "t50", Phase: "starting", Exec: 4, LastExec: ok}),
			restartLook(fr.Socket, newHub, restartWrapper(4, ok)),
		}
		exit, out, _ := runRestart(t, restartLook(fr.Socket, &restartOld, restartWrapper(3, nil)), looks...)
		if exit != 0 || !strings.Contains(out, "restarted: hub pid 101 (client h2), exec 4, commit bbb; restart wrapper pid 50") {
			t.Errorf("send %q: exit %d\n%s", code, exit, out)
		}
		if code != "" && !strings.Contains(out, "the request may still apply") {
			t.Errorf("send %q: the unknown outcome is not reported\n%s", code, out)
		}
		if a := fr.actions(); len(a) != 1 {
			t.Errorf("send %q: %d sends, want 1", code, len(a))
		}
	}
}

// A failed exec: the same wrapper is ready again with a new hub process at exec 3, and last_exec
// says failed for target 4. restart exits 1 with the exec error.
func TestRestartReportsAFailedExec(t *testing.T) {
	defer func(d time.Duration) { ctlRestartWait = d }(ctlRestartWait)
	ctlRestartWait = 3 * time.Second
	fr := relaunchRex(t, "")
	failed := &lastExec{Target: 4, Outcome: "failed", Error: "exec /bin/current failed: permission denied"}
	again := &hubClient{ID: "h2", State: "/state", PID: 101, Exec: 3, Commit: "aaa"}
	exit, out, _ := runRestart(t, restartLook(fr.Socket, &restartOld, restartWrapper(3, nil)), restartLook(fr.Socket, again, restartWrapper(3, failed)))
	if exit != 1 || !strings.Contains(out, "the exec of the new version failed: exec /bin/current failed: permission denied") ||
		!strings.Contains(out, "the old version runs again: hub pid 101 (client h2), exec 3") {
		t.Errorf("exit %d\n%s", exit, out)
	}
}

// No look decides when it does not show the same wrapper process ready with a new hub process: the
// old hub with a last_exec of an earlier failed attempt, another wrapper (pid or start time) at exec
// 4, no hub, or a hub that cannot be seen. restart exits 4 at the deadline with the last look.
func TestRestartNotConfirmed(t *testing.T) {
	defer func(d time.Duration) { ctlRestartWait = d }(ctlRestartWait)
	ctlRestartWait = time.Second
	ok := &lastExec{Target: 4, Outcome: "ok"}
	earlier := &lastExec{Target: 4, Outcome: "failed", Error: "an earlier attempt"}
	newHub := &hubClient{ID: "h2", State: "/state", PID: 101, Exec: 4}
	for what, look := range map[string]func(socket string) *ctlView{
		"the old hub, an earlier failure": func(s string) *ctlView { return restartLook(s, &restartOld, restartWrapper(3, earlier)) },
		"another wrapper pid": func(s string) *ctlView {
			return restartLook(s, newHub, &restartStatus{Version: 1, PID: 51, Start: "t51", Phase: "running", Exec: 4, LastExec: ok})
		},
		"another wrapper start": func(s string) *ctlView {
			return restartLook(s, newHub, &restartStatus{Version: 1, PID: 50, Start: "t50b", Phase: "running", Exec: 4, LastExec: ok})
		},
		"no hub":         func(s string) *ctlView { return restartLook(s, nil, restartWrapper(4, ok)) },
		"no wrapper":     func(s string) *ctlView { return restartLook(s, newHub, nil) },
		"the Rex server": func(s string) *ctlView { v := restartLook(s, nil, nil); v.rexErr = errors.New("dial failed"); return v },
	} {
		fr := relaunchRex(t, "timeout")
		exit, out, d := runRestart(t, restartLook(fr.Socket, &restartOld, restartWrapper(3, nil)), look(fr.Socket))
		if exit != 4 || !strings.Contains(out, "the restart is not confirmed within 1 s: run lazyrex-hub status") || !strings.Contains(out, "the last look: ") {
			t.Errorf("%s: exit %d\n%s", what, exit, out)
		}
		if d < time.Second-ctlLookMin || d > 2*time.Second {
			t.Errorf("%s: restart took %s, want the deadline of 1 s", what, d)
		}
		if a := fr.actions(); len(a) != 1 {
			t.Errorf("%s: %d sends, want 1", what, len(a))
		}
	}
}
