package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// Home-session tabs (hometabs.go) against a real Rex lab. Lab only (rexLab: LAZYREX_LAB=1). The
// hub runs in the test process with no UI worker: each pass feeds the Rex events that arrived, in
// the order they arrived, to handleEvent, then runs one reconcile. So these tests check the rows
// and the panes that Rex reports, not the worker's scheduling. The "renderer" is a shell loop that
// keeps its arguments (the hub's process proof); it never attaches, so the test ends it where a
// real renderer exits (a crash). A second connection acts as the
// user. These replace tests/hometabs-lab.mjs and tests/session-rename-lab.mjs; the hub's restart
// and a rename while it is stopped are TestSessionRenameKeptLab (a real hub process).
type homeLab struct {
	t     *testing.T
	h     *Hub
	u     *rex.Rex // the user's connection
	app   string   // the hub's app session
	mu    sync.Mutex
	queue []json.RawMessage
}

func newHomeLab(t *testing.T) *homeLab {
	t.Helper()
	l := rexLab(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) // first, so it runs after every cleanup that uses r
	u, err := rex.Dial(ctx, l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	go func() {
		for range u.Events {
		}
	}()
	hl := &homeLab{t: t, u: u}
	go func() {
		for ev := range r.Events {
			hl.mu.Lock()
			hl.queue = append(hl.queue, ev)
			hl.mu.Unlock()
		}
	}()
	h, _ := workerTestHub(t)
	h.rex, h.cfg.Server = r, l.server
	h.selfClosed = map[string]bool{}
	h.vs = newViewSvc() // closeViewRow forgets the view's render state
	// An inert worker channel: a kick (navigation focus, layout, status) is recorded, and no view
	// worker runs. This fixture has no view_ops schema, and nothing stops a worker before the DB
	// closes. The cleanup checks that no other worker started.
	h.vs.workers["formprobe"] = make(chan struct{}, 1)
	t.Cleanup(func() {
		h.vs.mu.Lock()
		defer h.vs.mu.Unlock()
		if len(h.vs.workers) != 1 {
			t.Errorf("view workers %v, want only the inert formprobe channel", h.vs.workers)
		}
	})
	view := filepath.Join(t.TempDir(), "view.sh")
	os.WriteFile(view, []byte("#!/bin/sh\nwhile :; do sleep 600; done\n"), 0o755) // no exec: the shell keeps the argv with the view ID
	h.cfg.View = view
	if err := h.uiStart(ctx); err != nil {
		t.Fatal(err)
	}
	hl.h = h
	t.Cleanup(func() {
		var sess string
		h.db.QueryRow(`SELECT session_id FROM ui_sessions WHERE plugin = ?`, uiSessionKey).Scan(&sess)
		if sess != "" {
			u.Call(context.Background(), "session.destroy", map[string]any{"session_id": sess}, nil)
		}
	})
	hl.declare("", "") // the plugin's own tab only: the first passes make the app session
	hl.until("the app session", 20*time.Second, func() bool { return hl.describe().Default != "" })
	hl.app = hl.describe().Default
	return hl
}

// pass feeds the queued events to the hub in their order, then runs one reconcile.
func (l *homeLab) pass() {
	l.t.Helper()
	l.drain()
	if _, err := l.h.reconcile(context.Background()); err != nil {
		l.t.Fatalf("reconcile: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // the pass's own events arrive
	l.drain()
}

func (l *homeLab) drain() {
	l.mu.Lock()
	q := l.queue
	l.queue = nil
	l.mu.Unlock()
	for _, ev := range q {
		l.h.handleEvent(context.Background(), ev)
	}
}

// queued counts the queued events of type typ (a session event) for session sess.
func (l *homeLab) queued(sess, typ string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ev := range l.queue {
		var e struct {
			Payload struct {
				Type    string `json:"type"`
				Payload struct {
					SessionID string `json:"session_id"`
					Event     struct {
						Type string `json:"type"`
					} `json:"event"`
				} `json:"payload"`
			} `json:"payload"`
		}
		if json.Unmarshal(ev, &e) == nil && e.Payload.Payload.SessionID == sess && e.Payload.Payload.Event.Type == typ {
			n++
		}
	}
	return n
}

// childExited reports whether Rex's child_exited event of block is queued.
func (l *homeLab) childExited(block string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ev := range l.queue {
		var e struct {
			Payload struct {
				Payload struct {
					Event struct {
						Type    string `json:"type"`
						Payload struct {
							BlockID string `json:"block_id"`
							Name    string `json:"name"`
						} `json:"payload"`
					} `json:"event"`
				} `json:"payload"`
			} `json:"payload"`
		}
		if json.Unmarshal(ev, &e) == nil && e.Payload.Payload.Event.Type == "block_event" &&
			e.Payload.Payload.Event.Payload.Name == "child_exited" && e.Payload.Payload.Event.Payload.BlockID == block {
			return true
		}
	}
	return false
}

// until runs passes until ok holds.
func (l *homeLab) until(what string, d time.Duration, ok func() bool) {
	l.t.Helper()
	for end := time.Now().Add(d); ; time.Sleep(150 * time.Millisecond) {
		l.pass()
		if ok() {
			return
		}
		if time.Now().After(end) {
			b, _ := os.ReadFile(l.h.ev.f.Name())
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) > 12 {
				lines = lines[len(lines)-12:]
			}
			l.t.Fatalf("timed out: %s (app session %s; describe %+v)\nlast hub events:\n%s", what, l.app, l.describe(), strings.Join(lines, "\n"))
		}
	}
}

// passesFor runs passes for d (a check that something does not happen).
func (l *homeLab) passesFor(d time.Duration) {
	l.t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		l.pass()
	}
}

// declare declares the tabs of plugin "formprobe" (hub.ui.tabs): its own tab, and with a
// non-empty revision a 'homed' tab in home sess ("" = no session_id).
func (l *homeLab) declare(sess, revision string) {
	l.t.Helper()
	tabs := []any{map[string]any{"key": "home", "title": "Form probe", "route": "/"}}
	if revision != "" {
		homed := map[string]any{"key": "homed", "title": "Homed", "route": "/", "revision": revision}
		if sess != "" {
			homed["session_id"] = sess
		}
		tabs = append(tabs, homed)
	}
	if _, err := l.h.uiTabs(context.Background(), "formprobe", map[string]any{"tabs": tabs}); err != nil {
		l.t.Fatal(err)
	}
}

type homeView struct {
	ID      string `json:"view_id"`
	Tab     string `json:"tab"`
	Status  string `json:"status"`
	Session string `json:"session_id"`
	Window  string `json:"window_id"`
	Block   string `json:"block_id"`
}

type homeDescribe struct {
	Default   string            `json:"default_session_id"`
	Views     []homeView        `json:"views"`
	Redirects map[string]string `json:"home_redirects"`
}

func (l *homeLab) describe() homeDescribe {
	l.t.Helper()
	out, err := l.h.uiDescribe(context.Background(), map[string]any{"plugin": "formprobe"})
	if err != nil {
		l.t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var d homeDescribe
	json.Unmarshal(b, &d)
	return d
}

// homed is the open view of the 'homed' tab, or nil.
func (l *homeLab) homed() *homeView {
	for _, v := range l.describe().Views {
		if v.Tab == "homed" && v.Status == "open" {
			return &v
		}
	}
	return nil
}

type homeSessionView struct {
	Active  string `json:"active_window_id"`
	Windows []struct {
		WindowID string `json:"window_id"`
		Label    string `json:"label"`
	} `json:"windows"`
}

func (l *homeLab) view(sess string) homeSessionView {
	var v homeSessionView
	if err := l.u.Call(context.Background(), "session.view", map[string]any{"session_id": sess}, &v); err != nil {
		l.t.Fatalf("session.view %s: %v", sess, err)
	}
	return v
}

func (l *homeLab) labels(sess string) []string {
	var out []string
	for _, w := range l.view(sess).Windows {
		out = append(out, w.Label)
	}
	return out
}

func (l *homeLab) sessionLabel(sess string) string {
	var out sessionList
	if err := l.u.Call(context.Background(), "session.list", map[string]any{}, &out); err != nil {
		l.t.Fatal(err)
	}
	for _, s := range out.Sessions {
		if s.SessionID == sess {
			return s.Label
		}
	}
	return ""
}

// session makes a user session with shell tabs of these labels; the user's connection attaches.
func (l *homeLab) session(label string, tabs ...string) string {
	l.t.Helper()
	var windows []any
	for _, w := range tabs {
		windows = append(windows, map[string]any{"window_label": w, "layout": map[string]any{"block": map[string]any{
			"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"command": []string{"/bin/sleep", "600"}, "login": false}}}})
	}
	var s struct {
		ID string `json:"session_id"`
	}
	if err := l.u.Call(context.Background(), "session.create", map[string]any{"label": label, "initial_windows": windows}, &s); err != nil {
		l.t.Fatal(err)
	}
	l.call("session.attach", map[string]any{"session_id": s.ID, "event_scope": "session"})
	l.t.Cleanup(func() { l.u.Call(context.Background(), "session.destroy", map[string]any{"session_id": s.ID}, nil) })
	return s.ID
}

// call is a user action.
func (l *homeLab) call(method string, args map[string]any) {
	l.t.Helper()
	if err := l.u.Call(context.Background(), method, args, nil); err != nil {
		l.t.Fatalf("%s: %v", method, err)
	}
}

func (l *homeLab) rename(sess, label string) {
	l.t.Helper()
	l.call("session.attach", map[string]any{"session_id": sess, "event_scope": "session"})
	l.call("session.set_label", map[string]any{"session_id": sess, "label": label})
}

// kill ends the renderer of view id (the pid the hub recorded), as a crash or an exit would.
func (l *homeLab) kill(id string) {
	l.t.Helper()
	var pid int
	if err := l.h.db.QueryRow(`SELECT child_pid FROM views WHERE view_id = ?`, id).Scan(&pid); err != nil || pid <= 0 {
		l.t.Fatalf("the renderer pid of %s: %d %v", id, pid, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		l.t.Fatalf("kill the renderer %d of %s: %v", pid, id, err)
	}
}

// events are the hub's events of this name.
func (l *homeLab) events(name string) []map[string]any {
	b, _ := os.ReadFile(l.h.ev.f.Name())
	var out []map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["event"] == name {
			out = append(out, e)
		}
	}
	return out
}

func (l *homeLab) event(name, key, value string) map[string]any {
	for _, e := range l.events(name) {
		if e[key] == value {
			return e
		}
	}
	return nil
}

// TestHomeTabLab: a tab with session_id opens in that session, after the user's tabs, and keeps
// to the revision rules there, in a session that replaced a gone home too.
func TestHomeTabLab(t *testing.T) {
	l := newHomeLab(t)
	slice2 := func(s []string) []string {
		if len(s) > 2 {
			return s[:2]
		}
		return s
	}
	same := func(a, b []string) bool { return strings.Join(a, "\x00") == strings.Join(b, "\x00") }

	// 1. A home tab opens in its home session, after the user's tabs, which keep their order.
	H := l.session("home-tabs", "b-first", "a-second")
	l.declare(H, "r1")
	var v1 *homeView
	l.until("the homed view in H", 20*time.Second, func() bool {
		v1 = l.homed()
		return v1 != nil && v1.Session == H && v1.Window != ""
	})
	l.until("the tab titled Homed", 20*time.Second, func() bool { return strings.Contains(strings.Join(l.labels(H), ","), "Homed") })
	want := []string{"b-first", "a-second"}
	if got := slice2(l.labels(H)); !same(got, want) {
		t.Fatalf("the user's tabs: %v, want %v first", got, want)
	}
	for i := 0; i < 3; i++ {
		l.pass()
	}
	if got := slice2(l.labels(H)); !same(got, want) {
		t.Fatalf("after 3 more passes the user's tabs are %v, want %v first", got, want)
	}

	// 2. The user closes it: it stays closed for the same revision; a new revision reopens it, and
	// the reopen takes no focus.
	l.call("block.close", map[string]any{"session_id": H, "block_id": v1.Block})
	l.until("the homed view closed", 20*time.Second, func() bool { return l.homed() == nil })
	l.declare(H, "r1")
	l.passesFor(2 * time.Second)
	if v := l.homed(); v != nil {
		t.Fatalf("same revision: the tab opened again: %+v", v)
	}
	l.declare(H, "r2")
	var v2 *homeView
	l.until("reopened in H", 20*time.Second, func() bool {
		v2 = l.homed()
		return v2 != nil && v2.Session == H && v2.Window != ""
	})
	if e := l.event("tab_reopened", "revision", "r2"); e == nil || e["tab"] != "homed" {
		t.Fatalf("no tab_reopened event for homed r2: %v", l.events("tab_reopened"))
	}
	if a := l.view(H).Active; a == v2.Window {
		t.Fatal("the reopen took focus")
	}

	// 3. A navigation shows it.
	if _, err := l.h.uiNavigate(context.Background(), map[string]any{"plugin": "formprobe", "tab": "homed"}); err != nil {
		t.Fatal(err)
	}
	l.until("the navigation focuses the home tab", 20*time.Second, func() bool { return l.view(H).Active == v2.Window })

	// 4. Dropping the home moves the tab to the app session: the next pass closes the view in H (a
	// view outside its tab's target session), with no crash, and the tab opens in the app session.
	l.declare("", "r2")
	l.until("the close of the view in H", 20*time.Second, func() bool { return l.event("view_closed", "view_id", v2.ID) != nil })
	if e := l.event("view_closed", "view_id", v2.ID); e["by"] != "hub" || e["why"] != "moved" {
		t.Fatalf("the view outside the tab's target session: %v, want a hub close, why moved", e)
	}
	var crashes int
	l.h.db.QueryRow(`SELECT crashes FROM ui_tabs WHERE plugin = 'formprobe' AND tab = 'homed'`).Scan(&crashes)
	if crashes != 0 {
		t.Fatalf("the move counted %d crashes", crashes)
	}
	l.until("moved to the app session", 20*time.Second, func() bool { v := l.homed(); return v != nil && v.Session == l.app })
	l.until("gone from H", 20*time.Second, func() bool { return !strings.Contains(strings.Join(l.labels(H), ","), "Homed") })

	// 5. A home session that is gone before the declaration: the tab stays in the app session.
	G := l.session("gone-home", "x")
	l.call("session.destroy", map[string]any{"session_id": G})
	l.declare(G, "r3")
	l.passesFor(2 * time.Second)
	if v := l.homed(); v == nil || v.Session != l.app {
		t.Fatalf("a missing home: the tab is %+v, want it open in the app session %s", v, l.app)
	}

	// 6. A home that was alive, then destroyed: the tab closes with it; the same revision creates
	// nothing; a new revision brings back a session with the home's name, and the tab lives there.
	name := "recreate-" + strings.ReplaceAll(t.Name(), "/", "-")
	H2 := l.session(name, "y")
	l.declare(H2, "r4")
	l.until("homed in H2", 20*time.Second, func() bool { v := l.homed(); return v != nil && v.Session == H2 })
	l.call("session.destroy", map[string]any{"session_id": H2})
	l.until("closed with its session", 20*time.Second, func() bool { return l.homed() == nil })
	l.passesFor(1500 * time.Millisecond)
	if v := l.homed(); v != nil {
		t.Fatalf("a closed tab in a gone home created a view: %+v", v)
	}
	l.declare(H2, "r5")
	var v6 *homeView
	l.until("homed in a new session", 20*time.Second, func() bool {
		v6 = l.homed()
		return v6 != nil && v6.Session != H2 && v6.Session != l.app && v6.Block != ""
	})
	t.Cleanup(func() {
		l.u.Call(context.Background(), "session.destroy", map[string]any{"session_id": v6.Session}, nil)
	})
	if got := l.sessionLabel(v6.Session); got != name {
		t.Fatalf("the new session is labeled %q, want the home's name %q", got, name)
	}
	if got := l.describe().Redirects[H2]; got != v6.Session {
		t.Fatalf("describe home_redirects[%s] = %q, want %s", H2, got, v6.Session)
	}

	// 7. In the replacement session the tab is at home: a renderer exit is a crash; a user close is
	// a user close; the revision rules hold there.
	l.call("session.attach", map[string]any{"session_id": v6.Session, "event_scope": "session"})
	// This case exercises event-first crash classification: the test feeds the exit event before
	// any pass. A running hub does not guarantee that order; a pass first closes the row as
	// "renderer exited unseen" and counts no crash (the Hub UX fix lead-unseen-crash).
	l.kill(v6.ID)
	waitFor(t, "Rex's child_exited of the renderer", 10*time.Second, func() bool { return l.childExited(v6.Block) })
	l.drain()
	var exited map[string]any
	waitFor(t, "the renderer exit", 10*time.Second, func() bool { exited = l.event("view_exited", "view_id", v6.ID); return exited != nil })
	if exited["crashes"] != float64(1) {
		t.Fatalf("an exit at home must count one crash: %v", exited)
	}
	var v7 *homeView
	l.until("the tab opened again in the replacement", 20*time.Second, func() bool {
		v7 = l.homed()
		return v7 != nil && v7.ID != v6.ID && v7.Session == v6.Session && v7.Block != ""
	})
	l.call("block.close", map[string]any{"session_id": v7.Session, "block_id": v7.Block})
	var closed map[string]any
	l.until("the user close", 20*time.Second, func() bool { closed = l.event("view_closed", "view_id", v7.ID); return closed != nil })
	if closed["by"] != "user" {
		t.Fatalf("a close at home must be a user close: %v", closed)
	}
	l.declare(H2, "r5")
	l.passesFor(2 * time.Second)
	if v := l.homed(); v != nil {
		t.Fatalf("same revision: the tab opened again in the replacement: %+v", v)
	}
	l.declare(H2, "r6")
	l.until("a new revision reopens it in the replacement", 20*time.Second, func() bool { v := l.homed(); return v != nil && v.Session == v6.Session })
}

// TestHomeSessionRenameLab: a home renamed twice and then destroyed comes back with its last name;
// two quick renames of the app session record the last one as the user's. The hub's restart and a
// rename while it is stopped are TestSessionRenameKeptLab (a real hub process).
func TestHomeSessionRenameLab(t *testing.T) {
	l := newHomeLab(t)
	base := strings.ReplaceAll(t.Name(), "/", "-")

	// 1. Both rename events reach the hub before the destroy, with no pass between them: only the
	// events can tell the hub the last name.
	H := l.session(base+"-0", "x")
	l.declare(H, "r1")
	l.until("homed in H", 20*time.Second, func() bool { v := l.homed(); return v != nil && v.Session == H })
	l.rename(H, base+"-1")
	l.rename(H, base+"-2")
	waitFor(t, "both rename events", 10*time.Second, func() bool { return l.queued(H, "session_label_changed") == 2 })
	l.drain()
	l.call("session.destroy", map[string]any{"session_id": H})
	l.until("closed with its session", 20*time.Second, func() bool { return l.homed() == nil })
	l.declare(H, "r2")
	var v *homeView
	l.until("homed in a new session", 20*time.Second, func() bool { v = l.homed(); return v != nil && v.Session != H && v.Session != l.app })
	t.Cleanup(func() {
		l.u.Call(context.Background(), "session.destroy", map[string]any{"session_id": v.Session}, nil)
	})
	if got := l.sessionLabel(v.Session); got != base+"-2" {
		t.Fatalf("the replacement is labeled %q, want the last name %q", got, base+"-2")
	}

	// 2. The user renames the app session twice in quick succession: the last name is the user's.
	a, b := "My LazyRex A "+base, "My LazyRex B "+base
	l.rename(l.app, a)
	l.rename(l.app, b)
	l.until("the last name recorded as the user's", 10*time.Second, func() bool {
		var user string
		l.h.db.QueryRow(`SELECT user_label FROM ui_session_names WHERE session_id = ?`, l.app).Scan(&user)
		return user == b
	})
	l.passesFor(1500 * time.Millisecond)
	if got := l.sessionLabel(l.app); got != b {
		t.Fatalf("after more passes the app session is labeled %q, want the user's %q", got, b)
	}
}
