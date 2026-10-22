package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The lines come from the "refused" list of hub.plugins (plugin-sources.md): one per refused entry,
// and the managed-file item once. "retained" is another state and is not in that list. A failed
// read or a reply without the list is unknown, never "no refusals". Text from the hub reaches a
// terminal, so control characters become "?".
func TestPluginRefusalLines(t *testing.T) {
	notInstalled := "plugin github:lazy-rex/Messaging@v0.1.2 is not installed: run lazyrex-hub plugin sync -config /h/hub.json"
	for _, c := range []struct {
		name  string
		reply string
		err   error
		want  []string
	}{
		{name: "none", reply: `{"entries":[],"refused":[]}`},
		{name: "an entry and the managed file", reply: `{"entries":[{"entry":2,"source":"x","state":"retained","error":"kept"}],"refused":[` +
			`{"entry":1,"source":"github:lazy-rex/Messaging@v0.1.2","name":null,"state":"refused","error":"` + notInstalled + `"},` +
			`{"managed":"m1","source":"/p/q","name":"q","state":"refused","error":"bad \u001b[2Jmanifest\u009b"},` +
			`{"managed_file":"/h/managed.json","error":"invalid JSON"}]}`,
			want: []string{"plugin github:lazy-rex/Messaging@v0.1.2: refused: " + notInstalled, "plugin /p/q: refused: bad ?[2Jmanifest?", "plugins: managed file /h/managed.json: invalid JSON"}},
		{name: "a failed read", err: errors.New("rex: client not found"), want: []string{"plugins: unknown: rex: client not found"}},
		{name: "not JSON", reply: `oops`, want: []string{"plugins: unknown: hub.plugins: invalid character 'o' looking for beginning of value"}},
		{name: "no refused list", reply: `{"entries":[]}`, want: []string{"plugins: unknown: hub.plugins has no refused list"}},
		{name: "an item without a source", reply: `{"refused":[{"state":"refused","error":"e"}]}`, want: []string{"plugins: unknown: a refused item without a source: e"}},
	} {
		if got := pluginRefusalLines([]byte(c.reply), c.err); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// Hub Status: a running hub with refused plugins is still running (exit 0); the refusals follow
// the process line.
func TestStatusShowsRefusedPlugins(t *testing.T) {
	reply := `echo '{"refused":[{"entry":1,"source":"github:lazy-rex/Profiles@v0.1.1","state":"refused","error":"not installed"}]}'`
	for _, c := range []struct {
		name, rex string
		want      string
	}{
		{"refused", reply, "\nplugin github:lazy-rex/Profiles@v0.1.1: refused: not installed"},
		{"a failed read", "echo boom >&2; exit 1", "\nplugins: unknown: "},
		{"none", `echo '{"refused":[]}'`, ""},
	} {
		v := &ctlView{rex: ctlFakeRex(t, "exit 0", c.rex), state: "/state", own: &hubClient{ID: "h1", PID: 7}}
		v.readPlugins()
		code, text := v.statusText()
		if code != ctlRunning || !strings.HasPrefix(text, "running: hub pid 7") {
			t.Fatalf("%s: %d\n%s", c.name, code, text)
		}
		if c.want == "" && strings.Contains(text, "plugin") || c.want != "" && !strings.Contains(text, c.want) {
			t.Errorf("%s: want %q:\n%s", c.name, c.want, text)
		}
	}
	// No hub of this state: no plugin read.
	v := &ctlView{rex: ctlFakeRex(t, "exit 0", "exit 1"), state: "/state"}
	if v.readPlugins(); v.plugins != nil {
		t.Errorf("a read without a hub: %q", v.plugins)
	}
}

// The Start Hub pane: after the ready, the wrapper prints the refusals. A refusal never restarts
// the hub.
func TestRestartShowsRefusedPlugins(t *testing.T) {
	wr := newWrapperTest(t, "ready-wait", "ready-exit0")
	wr.w.refusals = func(context.Context, int) []string { return []string{"plugin p: refused: not installed"} }
	done := wr.runAsync()
	waitText(t, wr, "restart: plugin p: refused: not installed")
	wr.w.sigs <- syscall.SIGHUP
	if code := waitCode(t, done); code != 0 {
		t.Fatalf("exit %d\n%s", code, wr.text())
	}
	if n := wr.startCount(); n != 1 {
		t.Errorf("%d starts, want 1\n%s", n, wr.text())
	}
}

// A stalled plugin read does not hold up a stop: the signal stops the hub within the usual bound,
// and the read's context ends with the run. A result that comes after the run is not printed.
func TestRestartRefusalReadIsBoundToTheRun(t *testing.T) {
	wr := newWrapperTest(t, "ready-wait", "ready-exit0")
	var mu sync.Mutex
	var ended error
	started := make(chan struct{})
	wr.w.refusals = func(ctx context.Context, _ int) []string {
		close(started)
		<-ctx.Done()
		mu.Lock()
		ended = ctx.Err()
		mu.Unlock()
		return []string{"plugin late: refused: too late"}
	}
	done := wr.runAsync()
	<-started
	begin := time.Now()
	wr.w.sigs <- syscall.SIGTERM
	if code := waitCode(t, done); code != 0 {
		t.Fatalf("exit %d\n%s", code, wr.text())
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("the stop took %s with a stalled read", d)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(ended, context.Canceled) || strings.Contains(wr.text(), "too late") {
		t.Errorf("read ended %v\n%s", ended, wr.text())
	}
}

// A child that exits before the read answers: the late lines are discarded, and the next run reads
// again.
func TestRestartDiscardsALateRefusalRead(t *testing.T) {
	wr := newWrapperTest(t, "ready-exit1", "ready-wait")
	var mu sync.Mutex
	calls := 0
	wr.w.refusals = func(ctx context.Context, _ int) []string {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			<-ctx.Done() // the first run ends before this read answers
			return []string{"plugin first: refused: late"}
		}
		return []string{"plugin second: refused: now"}
	}
	done := wr.runAsync()
	waitText(t, wr, "restart: plugin second: refused: now")
	wr.w.sigs <- syscall.SIGTERM
	if code := waitCode(t, done); code != 0 {
		t.Fatalf("exit %d\n%s", code, wr.text())
	}
	if strings.Contains(wr.text(), "plugin first") {
		t.Errorf("a late read was printed:\n%s", wr.text())
	}
}

func waitText(t *testing.T, wr *wrapperRun, want string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(wr.text(), want); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q:\n%s", want, wr.text())
		}
	}
}
