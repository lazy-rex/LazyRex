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
