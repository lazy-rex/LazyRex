package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// The start barrier of a plugin release (plan c4, A1; startEnded) on the real SDK (sdk/lua), in a
// harness lab (the hub runs with -test). Two SDK 0 plugins:
//   - bp: migration 2 waits 3 s, then creates table b. The wait is a CPU loop on os.clock, under the
//     hub's 5 s transaction idle limit (sqlTxIdle): in the A1 probe, rex.sleep never returned inside
//     the SDK's rex.after handler, and io.popen failed in a plugin.
//   - bf: migration 2 fails after 2 s.
//
// view.seen from a peer gets the SDK refusal only after the register handler has ended: for bp the
// refusal comes after the migration, from the same instance; for bf there is no refusal, and the
// SDK's failure text is in the pane of the failed instance. The rex CLI form of both answers is the
// form that startEnded parses (rexFailure).
func TestReleaseBarrierLab(t *testing.T) {
	s := newStartLab(t, "bp", "bf")
	root, _ := filepath.Abs("../../..")
	s.args = []string{"-sdk", filepath.Join(root, "sdk", "lua")}
	s.writeSource("bp", `{"name":"bp","main":"main.lua","sdk":"0"}`, "")
	os.WriteFile(filepath.Join(s.src, "bp", "main.lua"), []byte(`local app = require('lazyrex').plugin('bp')
app.db.migrate({
  'CREATE TABLE a (x)',
  function(db) local t0 = os.clock(); while os.clock() - t0 < 3 do end; db.exec('CREATE TABLE b (y)') end,
})
`), 0o600)
	s.writeSource("bf", `{"name":"bf","main":"main.lua","sdk":"0"}`, "")
	os.WriteFile(filepath.Join(s.src, "bf", "main.lua"), []byte(`local app = require('lazyrex').plugin('bf')
app.db.migrate({
  'CREATE TABLE a (x)',
  function(db) local t0 = os.clock(); while os.clock() - t0 < 2 do end; error('probe: migration 2 fails') end,
})
`), 0o600)
	s.start()

	// registeredNow polls (100 ms) until name is registered and returns its status and the time.
	registeredNow := func(name string) (startStatus, time.Time) {
		t.Helper()
		end := time.Now().Add(30 * time.Second)
		for time.Now().Before(end) {
			if st := s.status(name); st.Registered && st.ClientID != "" {
				return st, time.Now()
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s did not register", name)
		return startStatus{}, time.Time{}
	}
	tables := func(name string) string {
		r, err := s.call("hub.test.sql", map[string]any{"plugin": name, "sql": "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name"})
		if err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprint(r["rows"])
	}
	barrier := func(st startStatus) (time.Duration, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		t0 := time.Now()
		_, err := s.c.perform(ctx, st.ClientID, "view.seen", map[string]any{})
		return time.Since(t0), err
	}
	describe := func(err error) string {
		var re *rex.RexError
		if errors.As(err, &re) {
			return fmt.Sprintf("code=%q message=%q", re.Code, re.Message)
		}
		return fmt.Sprintf("%T %v", err, err)
	}
	// cli is client.perform view.seen through the rex CLI, as startEnded sends it.
	cli := func(client string) (string, error) {
		arg := fmt.Sprintf(`{"client_id":%q,"action":"view.seen","args":{},"timeout_ms":5000}`, client)
		cmd := exec.Command(s.o.rexBin, "-S", s.l.server, "--autostart=false", "api", "call", "client.perform", arg)
		cmd.Env = s.l.env
		var e bytes.Buffer
		cmd.Stderr = &e
		err := cmd.Run()
		return e.String(), err
	}

	// bp: a delayed migration.
	st, reg := registeredNow("bp")
	before := tables("bp")
	d, err := barrier(st)
	after := tables("bp")
	st2 := s.status("bp")
	t.Logf("bp: instance %s; tables at registration %s; answer after %s (registration +%s): %s; tables after %s",
		st.InstanceID, before, d.Round(time.Millisecond), time.Since(reg).Round(time.Millisecond), describe(err), after)
	var re *rex.RexError
	if !errors.As(err, &re) || re.Code != "refused" || re.Message != "Only the hub calls view.seen" {
		t.Errorf("bp: the answer %s, not the SDK refusal", describe(err))
	}
	if strings.Contains(before, "[b]") {
		t.Errorf("bp: migration 2 was complete at registration, so the test proves nothing: %s", before)
	}
	if d < 2*time.Second {
		t.Errorf("bp: the answer came after %s, before the 3 s migration could end", d)
	}
	if !strings.Contains(after, "[a] [b]") {
		t.Errorf("bp: migration 2 is not complete after the answer: %s", after)
	}
	if st2.InstanceID != st.InstanceID || st2.ClientID != st.ClientID || st2.ChildPID != st.ChildPID || !st2.Registered {
		t.Errorf("bp: the instance changed: %+v -> %+v", st, st2)
	}
	stderr, err := cli(st.ClientID)
	if m := rexFailure.FindStringSubmatch(strings.TrimSpace(stderr)); err == nil || m == nil || m[1] != "Only the hub calls view.seen" || m[2] != "refused" {
		t.Errorf("bp: the CLI refusal %q (%v) is not the form that startEnded parses", stderr, err)
	}

	// bf: a failed migration. The owned log is the pane of the failed instance. The hub closes that
	// pane when it starts the next instance, so a test fault holds the respawn for 6 s (lab -test).
	st, reg = registeredNow("bf")
	s.must("hub.test.fault", map[string]any{"point": "start-admission", "mode": "hold-before", "key": "bf", "plugin": "bf", "hold_ms": 6000})
	d, err = barrier(st)
	capture := filepath.Join(s.l.dir, "bf-pane.txt")
	out, cerr := exec.Command(s.o.rexBin, "-S", s.l.server, "--autostart=false", "capture", "-b", st.BlockID, "--trim").CombinedOutput()
	os.WriteFile(capture, out, 0o600)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "the start of bf failed") {
			t.Logf("bf pane: %s", strings.TrimSpace(line))
		}
	}
	t.Logf("bf: instance %s; answer after %s (registration +%s): %s", st.InstanceID, d.Round(time.Millisecond), time.Since(reg).Round(time.Millisecond), describe(err))
	st2 = s.status("bf")
	if err == nil || (errors.As(err, &re) && re.Code == "refused") {
		t.Errorf("bf: the barrier passed for a failed start: %s", describe(err))
	}
	if st2.InstanceID == st.InstanceID && st2.Registered {
		t.Errorf("bf: the failed instance %s is still registered", st.InstanceID)
	}
	if cerr != nil || !strings.Contains(string(out), "the start of bf failed") || !strings.Contains(string(out), "probe: migration 2 fails") {
		t.Errorf("bf: the pane of %s (%s) has not the intended migration failure (%v):\n%s", st.InstanceID, capture, cerr, out)
	}
	stderr, err = cli(st.ClientID)
	if m := rexFailure.FindStringSubmatch(strings.TrimSpace(stderr)); err == nil || m == nil || m[2] == "refused" {
		t.Errorf("bf: the CLI answer %q (%v) for the failed instance", stderr, err)
	}
	// The hub respawns bf after each exit (no backoff at this hub): stop the loop.
	if _, err := s.call("hub.admin.stop", map[string]any{"plugin": "bf"}); err != nil {
		t.Errorf("hub.admin.stop bf: %v", err)
	}
}
