package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// shortPerformWait makes the hub's wait for a client.perform d, for one test.
func shortPerformWait(t *testing.T, d time.Duration) {
	t.Helper()
	orig := performWait
	performWait = func(int) time.Duration { return d }
	t.Cleanup(func() { performWait = orig })
}

// neverAnswer is an answer of a performRex that waits until the test ends.
func neverAnswer(t *testing.T) func() (any, string) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	return func() (any, string) { <-done; return nil, "test_ended" }
}

// loggedEvents reads the hub's event log: the events named event, in order.
func loggedEvents(t *testing.T, h *Hub, event string) []map[string]any {
	t.Helper()
	f, err := os.Open(h.ev.f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	for s := bufio.NewScanner(f); s.Scan(); {
		var e map[string]any
		if json.Unmarshal(s.Bytes(), &e) == nil && e["event"] == event {
			out = append(out, e)
		}
	}
	return out
}

// sendsTo counts the client.perform calls with action to attachment att.
func sendsTo(fr *performRex, action, att string) int {
	n := 0
	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, c := range fr.calls {
		var a struct{ Attachment string }
		json.Unmarshal(c.Args, &a)
		if c.Action == action && a.Attachment == att {
			n++
		}
	}
	return n
}

// Row 11: a view.reload that the renderer never answers ends at the hub's wait, with its event.
func TestViewReloadHasADeadline(t *testing.T) {
	shortPerformWait(t, 200*time.Millisecond)
	h := releaseTestHub(t)
	h.ui = &uiState{kick: make(chan struct{}, 1), retire: map[string]time.Time{}, retireCut: map[string]bool{}}
	if _, err := h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, client_id, attachment) VALUES ('v', 'p', 't', '/', 'open', 'c', 'a1')`); err != nil {
		t.Fatal(err)
	}
	wait := neverAnswer(t)
	fr := newPerformRex(t, func(performCall) (any, string) { return wait() })
	h.rex = fr.dial(t)
	if _, err := h.uiReload(context.Background(), map[string]any{"plugin": "p"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the view_reload event", 5*time.Second, func() bool { return len(loggedEvents(t, h, "view_reload")) == 1 })
	if e := loggedEvents(t, h, "view_reload")[0]; !strings.Contains(e["error"].(string), "deadline exceeded") {
		t.Errorf("view_reload: %v, want the hub's deadline", e)
	}
}

// Row 11: after the hub's wait cuts a view.retire, the hub sends no other one to that attachment,
// also on later passes after the 10 s throttle: Rex can still deliver the first. A new attachment
// gets one. A failure that Rex reports keeps the retry.
func TestViewRetireAfterACut(t *testing.T) {
	shortPerformWait(t, 200*time.Millisecond)
	h := releaseTestHub(t)
	h.ui = &uiState{kick: make(chan struct{}, 1), retire: map[string]time.Time{}, retireCut: map[string]bool{}}
	wait := neverAnswer(t)
	fr := newPerformRex(t, func(c performCall) (any, string) {
		if strings.Contains(string(c.Args), `"refuses"`) {
			return nil, "refused"
		}
		return wait()
	})
	h.rex = fr.dial(t)
	ctx := context.Background()
	pass := func(att string) {
		h.ui.retire[att] = time.Now().Add(-11 * time.Second) // the throttle has passed
		h.retireLegacy(ctx, &view{ID: "v", Plugin: "p", ClientID: "c", Attachment: att})
	}

	pass("a1")
	waitFor(t, "the cut of the first view.retire", 5*time.Second, func() bool { return len(loggedEvents(t, h, "view_retire")) == 1 })
	for range 3 {
		pass("a1")
	}
	time.Sleep(300 * time.Millisecond)
	if n := sendsTo(fr, "view.retire", "a1"); n != 1 {
		t.Errorf("a1: %d sends after a cut and 3 later passes, want 1", n)
	}

	pass("a2")
	waitFor(t, "a2's send", 5*time.Second, func() bool { return sendsTo(fr, "view.retire", "a2") == 1 })

	pass("refuses")
	waitFor(t, "the refusal", 5*time.Second, func() bool { return len(loggedEvents(t, h, "view_retire")) == 3 })
	pass("refuses")
	waitFor(t, "the retry after a refusal", 5*time.Second, func() bool { return sendsTo(fr, "view.retire", "refuses") == 2 })
}

// hangingApp is a fakeApp whose client.perform never answers: it waits until its context ends.
type hangingApp struct{ fakeApp }

func (f *hangingApp) call(h *Hub, ctx context.Context, method string, args map[string]any, out any) error {
	if method == "client.perform" {
		f.performs = append(f.performs, args)
		<-ctx.Done()
		return ctx.Err()
	}
	return f.fakeApp.call(h, ctx, method, args, out)
}

// Row 13: an app call that the app never answers ends at the hub's wait. The caller's error says
// that the outcome is unknown, the request is sent once, and no Remote Control state is recorded.
func TestAppCallsHaveADeadline(t *testing.T) {
	shortPerformWait(t, 200*time.Millisecond)
	check := func(what string, f *hangingApp, h *Hub, run func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- run() }()
		var err error
		select {
		case err = <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("%s: no answer within 2 s, want the hub's wait (200 ms)", what)
			return
		}
		if !isRefusal(err, "outcome_unknown") || !strings.Contains(err.Error(), "the request may still apply") {
			t.Errorf("%s: %v, want outcome_unknown that may still apply", what, err)
		}
		if len(f.performs) != 1 {
			t.Errorf("%s: %d sends, want 1", what, len(f.performs))
		}
		if _, ok := h.rc.seen["app-1"]; ok {
			t.Errorf("%s: a cut recorded a Remote Control state", what)
		}
	}
	f := &hangingApp{fakeApp{focused: "form-block", clients: oneApp}}
	h, p := appActionHub(t, &f.fakeApp)
	appCall = f.call
	check("pane.insert_path", f, h, func() error {
		_, err := h.viewAppAction(context.Background(), p, map[string]any{"view_id": "view-form-1", "attachment": "att-1", "action": "pane.insert_path"})
		return err
	})
	g := &hangingApp{fakeApp{focused: "form-block", clients: oneApp}}
	hs, ps := selectHub(t, &g.fakeApp)
	appCall = g.call
	check("session.select", g, hs, func() error {
		_, err := hs.appSelectSession(context.Background(), ps, map[string]any{"view_id": "view-start", "session_id": "s2", "window_id": "w-new"})
		return err
	})
}

// Row 12: a view.close_request that the hub's wait cuts is not tried again (Rex can still deliver
// it). A failure that Rex reports is tried again while the close is pending and the attachment is
// current: three tries.
func TestCloseRequestAfterACut(t *testing.T) {
	shortPerformWait(t, 200*time.Millisecond)
	for _, c := range []struct {
		what  string
		code  string // "" = never answers
		sends int
	}{{"a cut", "", 1}, {"a refusal", "refused", 3}} {
		t.Run(c.what, func(t *testing.T) {
			h := releaseTestHub(t)
			if _, err := h.db.Exec(pairSchema); err != nil {
				t.Fatal(err)
			}
			h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, client_id, attachment) VALUES ('v', 'p', 't', '/', 'open', 'c', 'a1')`)
			h.db.Exec(`INSERT INTO view_transcripts (view_id, status, close_pending) VALUES ('v', 'declined', 1)`)
			wait := neverAnswer(t)
			fr := newPerformRex(t, func(performCall) (any, string) {
				if c.code != "" {
					return nil, c.code
				}
				return wait()
			})
			h.rex = fr.dial(t)
			done := make(chan struct{})
			go func() { h.requestClose("p", "v", "c", "a1"); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("requestClose did not end within 5 s")
			}
			if n := sendsTo(fr, "view.close_request", "a1"); n != c.sends {
				t.Errorf("%d sends, want %d", n, c.sends)
			}
			if e := loggedEvents(t, h, "view_close_requested"); len(e) != 1 || e[0]["error"] == "" {
				t.Errorf("view_close_requested: %v, want one with the error", e)
			}
		})
	}
}

// forwardHub is a hub with plugin x (client c) that exports x.do, on a performRex whose hub.forward
// for args.action "x.do" waits until release gets that call's tag (args.args.tag), then fails.
func forwardHub(t *testing.T) (*Hub, map[string]chan struct{}) {
	t.Helper()
	h := releaseTestHub(t)
	h.plugins = map[string]*Plugin{"x": {ClientID: "c", Exports: []string{"x.do"}}}
	h.agentForward = map[string]string{"x.do": "c"}
	release := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
	wait := neverAnswer(t)
	fr := newPerformRex(t, func(c performCall) (any, string) {
		var a struct {
			Args struct{ Tag string }
		}
		json.Unmarshal(c.Args, &a)
		if ch := release[a.Args.Tag]; ch != nil {
			select {
			case <-ch:
				return nil, "failed"
			case <-time.After(time.Minute):
			}
		}
		return wait()
	})
	h.rex = fr.dial(t)
	return h, release
}

// forward runs agentDispatch for x.do from pid 7 with tag, in a goroutine.
func forward(h *Hub, tag string) chan error {
	var p perform
	p.Action = "x.do"
	p.Args = json.RawMessage(`{"tag":"` + tag + `"}`)
	p.Requester.Principal.PID = 7
	done := make(chan error, 1)
	go func() { _, err := h.agentDispatch(context.Background(), p); done <- err }()
	return done
}

func endOf(t *testing.T, what string, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not end within 5 s", what)
		return nil
	}
}

// Row 8: a hub.forward that the plugin never answers ends at the hub's wait with outcome_unknown
// that may still apply. The on_behalf window is for each agent and plugin client: it stays open
// while any forward of that agent to that client is in flight, and closes after the last one.
// The test does not claim that a late delivery is refused.
func TestForwardHasADeadline(t *testing.T) {
	shortPerformWait(t, time.Second)

	// A cut forward: the agent's error, and the window closes.
	h, _ := forwardHub(t)
	err := endOf(t, "the cut forward", forward(h, "never"))
	if !isRefusal(err, "outcome_unknown") || !strings.Contains(err.Error(), "the plugin did not answer within 1 s; the request may still apply") {
		t.Errorf("the cut forward: %v", err)
	}
	if h.forwarding("c", 7) {
		t.Error("the window is still open after the cut forward ended")
	}

	// A is cut, then B starts: B opens the window again, and its end closes it.
	h, release := forwardHub(t)
	endOf(t, "A", forward(h, "never"))
	b := forward(h, "b")
	waitFor(t, "B in flight", 5*time.Second, func() bool { return h.forwarding("c", 7) })
	close(release["b"])
	endOf(t, "B", b)
	if h.forwarding("c", 7) {
		t.Error("the window is still open after B ended")
	}

	// A and B overlap: A's cut leaves the window open for B; B's end closes it.
	h, release = forwardHub(t)
	a := forward(h, "never")
	time.Sleep(200 * time.Millisecond)
	b = forward(h, "b")
	waitFor(t, "two forwards in flight", 5*time.Second, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.forwards["c"][7] == 2
	})
	if err := endOf(t, "A", a); !isRefusal(err, "outcome_unknown") {
		t.Errorf("A: %v", err)
	}
	if !h.forwarding("c", 7) {
		t.Error("A's cut closed the window while B is in flight")
	}
	close(release["b"])
	endOf(t, "B", b)
	if h.forwarding("c", 7) {
		t.Error("the window is still open after the last forward ended")
	}
}

// The error of an unknown outcome names the hub's wait only when the hub's own wait cut the call:
// when the caller's context ended first, the duration would be wrong (review of cbcd96e).
func TestUnknownOutcomeText(t *testing.T) {
	shortPerformWait(t, 7*time.Second)
	if err := unknownOutcome(context.Background(), "the app", 5000); !isRefusal(err, "outcome_unknown") || err.Error() != "outcome_unknown: the app did not answer within 7 s; the request may still apply" {
		t.Errorf("the hub's cut: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := unknownOutcome(ctx, "the app", 5000); !isRefusal(err, "outcome_unknown") || err.Error() != "outcome_unknown: the app did not answer before the caller's deadline; the request may still apply" {
		t.Errorf("the caller's deadline: %v", err)
	}
}
