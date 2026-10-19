package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// Transcript pairs against a real Rex lab (pair.go). Lab only: PAIR_LAB_SOCKET=/tmp/rexlab-*/rex.sock.
// The "renderer" is a shell loop that keeps its arguments (the view ID and, for a
// transcript, -transcript), so the hub's process proofs work. No plugin runs.
func pairLabHub(t *testing.T) *Hub {
	t.Helper()
	socket := os.Getenv("PAIR_LAB_SOCKET")
	if socket == "" {
		t.Skip("set PAIR_LAB_SOCKET to an isolated Rex lab socket")
	}
	if !strings.HasPrefix(socket, "/tmp/rexlab-") || !strings.HasSuffix(socket, "/rex.sock") {
		t.Fatal("refuse a non-lab socket")
	}
	if real, err := filepath.EvalSymlinks(socket); err != nil || !strings.HasPrefix(real, "/private/tmp/rexlab-") {
		t.Fatalf("refuse a socket that resolves outside the lab: %s %v", real, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := workerTestHub(t)
	go func() {
		for ev := range r.Events {
			if f, _ := pairSink.Load().(func(json.RawMessage)); f != nil {
				f(ev)
			}
		}
	}()
	h.rex = r
	view := filepath.Join(t.TempDir(), "view.sh")
	os.WriteFile(view, []byte("#!/bin/sh\nwhile :; do sleep 600; done\n"), 0o755)
	h.cfg.View = view
	h.cfg.Transcript = true
	pairRestart(t, h)
	t.Cleanup(func() {
		var sess string
		h.db.QueryRow(`SELECT session_id FROM ui_sessions WHERE plugin = ?`, uiSessionKey).Scan(&sess)
		if sess != "" {
			r.Call(context.Background(), "session.destroy", map[string]any{"session_id": sess}, nil)
		}
	})
	return h
}

// pairSink receives the lab's Rex events when a test runs the real worker (pairWorker).
var pairSink atomic.Value

// pairWorker runs the hub's own UI worker and event handling, as the hub does, instead of the
// manual passes: it shows that events and retries wake the worker by themselves.
func pairWorker(t *testing.T, h *Hub) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pairSink.Store(func(ev json.RawMessage) { h.handleEvent(ctx, ev) })
	go h.uiWorker(ctx)
	h.kickUI("")
	t.Cleanup(func() {
		pairSink.Store(func(json.RawMessage) {})
		cancel()
	})
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timeout: %s", what)
}

// pairRestart is a hub start on the same state: memory is new, attachments are gone.
func pairRestart(t *testing.T, h *Hub) {
	t.Helper()
	h.selfClosed = map[string]bool{}
	h.generation++
	if err := h.uiStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.viewsStart(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func declarePair(t *testing.T, h *Hub, layout string) {
	t.Helper()
	tabs := []any{map[string]any{"key": "thread/a", "title": "A", "route": "/thread/a", "layout": layout}}
	if _, err := h.uiTabs(context.Background(), "messaging", map[string]any{"tabs": tabs}); err != nil {
		t.Fatal(err)
	}
}

func passes(t *testing.T, h *Hub, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := h.reconcile(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
}

type pairWindow struct {
	Composer, Transcript   string // block IDs that Rex reports for the view
	Composers, Transcripts int    // panes with each label
	Focused                string
	TranscriptRect         []float64
	Active                 bool // the pair's window is the session's active window
}

// pairPanes reads, from Rex, the panes of the view's labels and the composer's window.
func pairPanes(t *testing.T, h *Hub, viewID string) pairWindow {
	t.Helper()
	ctx := context.Background()
	var sess string
	h.db.QueryRow(`SELECT session_id FROM ui_sessions WHERE plugin = ?`, uiSessionKey).Scan(&sess)
	blocks, err := h.blocksIn(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	var w pairWindow
	for id, b := range blocks {
		switch b.Label {
		case "lazyrex-view:" + viewID:
			w.Composer, w.Composers = id, w.Composers+1
		case transcriptLabel + viewID:
			w.Transcript, w.Transcripts = id, w.Transcripts+1
		}
	}
	var sv struct {
		Active  string `json:"active_window_id"`
		Windows []struct {
			WindowID string          `json:"window_id"`
			Focused  string          `json:"focused_block_id"`
			Layers   json.RawMessage `json:"layers"`
		} `json:"windows"`
	}
	if err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": sess}, &sv); err != nil {
		t.Fatal(err)
	}
	for _, win := range sv.Windows {
		if win.WindowID != blocks[w.Composer].WindowID {
			continue
		}
		w.Focused, w.Active = win.Focused, sv.Active == win.WindowID
		var layers []struct {
			Layout struct {
				Split *struct {
					Before struct {
						BlockID string    `json:"block_id"`
						Rect    []float64 `json:"rect"`
					} `json:"before"`
				} `json:"split"`
			} `json:"layout"`
		}
		json.Unmarshal(win.Layers, &layers)
		if len(layers) > 0 && layers[0].Layout.Split != nil && layers[0].Layout.Split.Before.BlockID == w.Transcript {
			w.TranscriptRect = layers[0].Layout.Split.Before.Rect
		}
	}
	return w
}

func onlyView(t *testing.T, h *Hub) *view {
	t.Helper()
	views, err := h.loadViews(context.Background(), "messaging")
	if err != nil {
		t.Fatal(err)
	}
	var open []*view
	for _, v := range views {
		if v.Status == "open" {
			open = append(open, v)
		}
	}
	if len(open) != 1 {
		t.Fatalf("open views: %d", len(open))
	}
	return open[0]
}

func transcriptOf(t *testing.T, h *Hub, id string) *transcriptRole {
	t.Helper()
	tr, err := h.loadTranscript(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// assertPair checks one composer and one transcript, the transcript on top at 76 percent, and
// the composer focused in its window.
func assertPair(t *testing.T, h *Hub, v *view) pairWindow {
	t.Helper()
	w := pairPanes(t, h, v.ID)
	if w.Composers != 1 || w.Transcripts != 1 {
		t.Fatalf("panes: %d composers, %d transcripts", w.Composers, w.Transcripts)
	}
	if len(w.TranscriptRect) != 4 || w.TranscriptRect[1] != 0 || w.TranscriptRect[3] < 0.75 || w.TranscriptRect[3] > 0.77 {
		t.Fatalf("transcript rect %v", w.TranscriptRect)
	}
	if w.Focused != w.Composer {
		t.Fatalf("focused %s, composer %s", w.Focused, w.Composer)
	}
	tr := transcriptOf(t, h, v.ID)
	if tr == nil || tr.Status != "open" || tr.Block != w.Transcript {
		t.Fatalf("transcript row %+v, pane %s", tr, w.Transcript)
	}
	if !argvHas(tr.ChildPID, "-transcript") || argvHas(v.ChildPID, "-transcript") {
		t.Fatal("the transcript pane does not run -transcript, or the composer does")
	}
	if l := snapLayout(t, h, v.ID); l != layoutTranscript {
		t.Fatalf("the composer's snapshot layout is %q", l)
	}
	return w
}

func TestPairCreateLab(t *testing.T) {
	h := pairLabHub(t)
	otherWindow(t, h) // Rex makes a new window active when none is active, even with focus false
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	w := assertPair(t, h, v)
	if w.Active {
		t.Fatal("the hub made the pair's tab active without a navigation")
	}
	// A navigation focuses the composer and makes the tab active.
	if _, err := h.uiNavigate(context.Background(), map[string]any{"plugin": "messaging", "tab": "thread/a"}); err != nil {
		t.Fatal(err)
	}
	passes(t, h, 1)
	if w = pairPanes(t, h, v.ID); !w.Active || w.Focused != w.Composer {
		t.Fatalf("after navigation: %+v", w)
	}
	// More passes change nothing.
	passes(t, h, 2)
	assertPair(t, h, v)
}

func TestPairOffWithoutConfigLab(t *testing.T) {
	h := pairLabHub(t)
	h.cfg.Transcript = false
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	if w := pairPanes(t, h, v.ID); w.Composers != 1 || w.Transcripts != 0 || v.Layout != "" || snapLayout(t, h, v.ID) != "" {
		t.Fatalf("panes %+v layout %q", w, v.Layout)
	}
}

// Step 1's answer is lost: the composer exists, its row says 'sending'. The next pass adopts
// it by label and process proof, then adds the transcript. No second composer.
func TestPairUnknownComposerReplyLab(t *testing.T) {
	h := pairLabHub(t)
	orig := viewNewWindow
	viewNewWindow = func(h *Hub, ctx context.Context, req map[string]any, out any) error {
		viewNewWindow = orig
		orig(h, ctx, req, out)
		return &rex.RexError{Code: "disconnected", Message: "lab: the answer is lost"}
	}
	t.Cleanup(func() { viewNewWindow = orig })
	declarePair(t, h, layoutTranscript)
	h.reconcile(context.Background()) // the lost answer fails this pass
	passes(t, h, 3)
	assertPair(t, h, onlyView(t, h))
}

// Step 2's answer is lost, and the hub restarts before the next pass. The new hub adopts the
// transcript by label and process proof. No second transcript.
func TestPairUnknownTranscriptReplyAndRestartLab(t *testing.T) {
	h := pairLabHub(t)
	orig := transcriptSplit
	transcriptSplit = func(h *Hub, ctx context.Context, args map[string]any, out any) error {
		transcriptSplit = orig
		orig(h, ctx, args, out)
		return &rex.RexError{Code: "disconnected", Message: "lab: the answer is lost"}
	}
	t.Cleanup(func() { transcriptSplit = orig })
	declarePair(t, h, layoutTranscript)
	passes(t, h, 1)
	v := onlyView(t, h)
	if tr := transcriptOf(t, h, v.ID); tr == nil || tr.Status != "sending" {
		t.Fatalf("after the lost answer: %+v", tr)
	}
	pairRestart(t, h)
	passes(t, h, 3)
	assertPair(t, h, v)
	if tr := transcriptOf(t, h, v.ID); tr.Crashes != 0 {
		t.Fatalf("an adoption counted a crash: %d", tr.Crashes)
	}
}

// The hub stops between the two steps: the composer exists, the split request never left.
// The restarted hub finds no transcript pane and opens one.
func TestPairRestartBetweenStepsLab(t *testing.T) {
	h := pairLabHub(t)
	orig := transcriptSplit
	transcriptSplit = func(*Hub, context.Context, map[string]any, any) error {
		return &rex.RexError{Code: "disconnected", Message: "lab: the hub stops before the split"}
	}
	declarePair(t, h, layoutTranscript)
	passes(t, h, 1)
	transcriptSplit = orig
	pairRestart(t, h)
	passes(t, h, 3)
	assertPair(t, h, onlyView(t, h))
}

// Rex refuses the split each time: the composer stays alone, the hub stops after maxCrashes
// attempts, and a navigation to the tab tries again.
func TestPairRefusedSplitLab(t *testing.T) {
	h := pairLabHub(t)
	orig := transcriptSplit
	refusals := 0
	transcriptSplit = func(*Hub, context.Context, map[string]any, any) error {
		refusals++
		return &rex.RexError{Code: "bad_request", Message: "lab: refused"}
	}
	t.Cleanup(func() { transcriptSplit = orig })
	declarePair(t, h, layoutTranscript)
	passes(t, h, maxCrashes+3)
	v := onlyView(t, h)
	if w := pairPanes(t, h, v.ID); w.Composers != 1 || w.Transcripts != 0 || refusals != maxCrashes {
		t.Fatalf("panes %+v after %d refusals", w, refusals)
	}
	transcriptSplit = orig
	if _, err := h.uiNavigate(context.Background(), map[string]any{"plugin": "messaging", "tab": "thread/a"}); err != nil {
		t.Fatal(err)
	}
	passes(t, h, 2)
	assertPair(t, h, v)
}

// A transcript pane of this view that no row records (an earlier hub's pane) closes.
func TestPairOrphanTranscriptLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	if err := transcriptSplit(h, context.Background(), map[string]any{"session_id": v.Session, "anchor_block_id": v.Block,
		"direction": "vertical", "side": "before", "ratio": 0.5, "focus": false,
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "label": transcriptLabel + v.ID,
			"options": map[string]any{"command": h.viewCommand(v.ID, v.Plugin, v.Route, "-transcript"), "login": false}}}}, nil); err != nil {
		t.Fatal(err)
	}
	if w := pairPanes(t, h, v.ID); w.Transcripts != 2 {
		t.Fatalf("setup: %d transcripts", w.Transcripts)
	}
	passes(t, h, 2)
	assertPair(t, h, v)
}

// The user closes the transcript after the composer's renderer process ended (before the hub
// saw it): nothing can be unsaved, so the composer closes too and the tab stays closed until a
// navigation.
func TestPairUserClosesTranscriptComposerGoneLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	tr := transcriptOf(t, h, v.ID)
	// The lab started this process; its arguments prove it before the kill.
	if !argvHas(v.ChildPID, v.ID) || argvHas(v.ChildPID, "-transcript") {
		t.Fatal("not the lab's composer")
	}
	syscall.Kill(v.ChildPID, syscall.SIGTERM)
	time.Sleep(300 * time.Millisecond)
	userClose(t, h, v.Session, tr.Block)
	passes(t, h, 2)
	if w := pairPanes(t, h, v.ID); w.Composers != 0 || w.Transcripts != 0 {
		t.Fatalf("panes after the user's close: %+v", w)
	}
	tabs, _ := h.loadTabs(context.Background(), "messaging")
	if len(tabs) != 1 || !tabs[0].Suppressed {
		t.Fatalf("tab %+v", tabs)
	}
	if transcriptOf(t, h, v.ID) != nil {
		t.Fatal("the transcript row stays")
	}
}

// The user closes the transcript while the composer is attached: the hub asks the composer to
// close through its gate (view.close_request). Here no renderer answers, so the composer stays
// alone, and no new transcript opens until a navigation.
func TestPairUserClosesTranscriptAttachedLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	h.db.Exec(`UPDATE views SET client_id = 'client:lab-none', attachment = 'att-lab' WHERE view_id = ?`, v.ID)
	tr := transcriptOf(t, h, v.ID)
	userClose(t, h, v.Session, tr.Block)
	passes(t, h, 3)
	w := pairPanes(t, h, v.ID)
	if w.Composers != 1 || w.Transcripts != 0 {
		t.Fatalf("panes: %+v", w)
	}
	if tr := transcriptOf(t, h, v.ID); tr == nil || tr.Status != "declined" {
		t.Fatalf("transcript row %+v", tr)
	}
	if !waitPairEvent(t, h, "view_close_requested", v.ID) {
		t.Fatal("no close request to the composer")
	}
	if _, err := h.uiNavigate(context.Background(), map[string]any{"plugin": "messaging", "tab": "thread/a"}); err != nil {
		t.Fatal(err)
	}
	passes(t, h, 2)
	assertPair(t, h, v)
}

// The user closes the composer: the transcript closes with it.
func TestPairUserClosesComposerLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	userClose(t, h, v.Session, v.Block)
	if w := pairPanes(t, h, v.ID); w.Composers != 0 || w.Transcripts != 0 {
		t.Fatalf("panes: %+v", w)
	}
	if transcriptOf(t, h, v.ID) != nil {
		t.Fatal("the transcript row stays")
	}
}

// The printer exits (its pane stays, with no child): the hub replaces the transcript and counts it.
func TestPairTranscriptCrashLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	tr := transcriptOf(t, h, v.ID)
	// The lab started this process; its arguments prove it before the kill.
	if !argvHas(tr.ChildPID, v.ID) || !argvHas(tr.ChildPID, "-transcript") {
		t.Fatal("not the lab's printer")
	}
	syscall.Kill(tr.ChildPID, syscall.SIGTERM)
	time.Sleep(500 * time.Millisecond)
	passes(t, h, 3)
	assertPair(t, h, v)
	if tr2 := transcriptOf(t, h, v.ID); tr2.Block == tr.Block || tr2.Crashes != 1 {
		t.Fatalf("after the crash: %+v", tr2)
	}
}

// The user moves the transcript out of the pair's window: the hub closes it and opens a new one
// above the composer.
func TestPairMovedTranscriptLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	tr := transcriptOf(t, h, v.ID)
	if err := h.rex.Call(context.Background(), "session.detach_block", map[string]any{"session_id": v.Session, "block_id": tr.Block}, nil); err != nil {
		t.Fatal(err)
	}
	passes(t, h, 3)
	assertPair(t, h, v)
}

// Only the printer in the transcript pane can attach as the transcript. The printer can read
// and report focus, and cannot mutate.
func TestPairAttachAuthorityLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	tr := transcriptOf(t, h, v.ID)
	caller := func(pid int, client string) perform {
		var p perform
		p.Requester.ClientID = client
		p.Requester.Principal.PID = pid
		return p
	}
	ctx := context.Background()
	args := map[string]any{"view_id": v.ID, "schema_version": float64(1), "role": "transcript"}
	if _, err := h.viewAttach(ctx, caller(v.ChildPID, "client:composer"), args); err == nil {
		t.Fatal("the composer's process attached as the transcript")
	}
	out, err := h.viewAttach(ctx, caller(tr.ChildPID, "client:printer"), args)
	if err != nil {
		t.Fatal(err)
	}
	att := out.(map[string]any)["attachment"].(string)
	read := map[string]any{"view_id": v.ID, "attachment": att}
	if _, err := h.viewGet(ctx, caller(tr.ChildPID, "client:printer"), read); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := h.viewSeen(ctx, caller(tr.ChildPID, "client:printer"), map[string]any{"view_id": v.ID, "attachment": att, "reason": "focus"}); err != nil {
		t.Fatalf("seen: %v", err)
	}
	h.vs.mu.Lock()
	seen := h.vs.seen["messaging"]["/thread/a"]
	h.vs.mu.Unlock()
	if seen != "focus" {
		t.Fatalf("the printer's focus is not a read notice: %q", seen)
	}
	send := map[string]any{"view_id": v.ID, "attachment": att, "operation_key": "k1", "node_id": "composer", "text": "x"}
	if _, err := h.viewAdmit(ctx, "send", caller(tr.ChildPID, "client:printer"), send); err == nil || !strings.Contains(err.Error(), "stale_") {
		t.Fatalf("the printer could send: %v", err)
	}
	// A hub restart ends the attachment; the printer attaches again.
	pairRestart(t, h)
	if _, err := h.viewGet(ctx, caller(tr.ChildPID, "client:printer"), read); err == nil {
		t.Fatal("an attachment survived the restart")
	}
	passes(t, h, 1)
	assertPair(t, h, v)
}

// userClose closes a pane as the user would, and delivers the close to the hub (the lab hub
// drains Rex events, so the test delivers it).
func userClose(t *testing.T, h *Hub, sess, block string) {
	t.Helper()
	if err := h.rex.Call(context.Background(), "block.close", map[string]any{"session_id": sess, "block_id": block}, nil); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	self := h.selfClosed[block]
	delete(h.selfClosed, block)
	h.mu.Unlock()
	h.uiBlockClosed(context.Background(), block, self)
	time.Sleep(200 * time.Millisecond) // a close request runs in the background
}

func pairEvent(t *testing.T, h *Hub, name, viewID string) bool {
	t.Helper()
	b, _ := os.ReadFile(h.ev.f.Name())
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, `"`+name+`"`) && strings.Contains(line, viewID) {
			return true
		}
	}
	return false
}

// otherWindow opens an active window in the LazyRex session that is not the pair's.
func otherWindow(t *testing.T, h *Hub) {
	t.Helper()
	var sess string
	h.db.QueryRow(`SELECT session_id FROM ui_sessions WHERE plugin = ?`, uiSessionKey).Scan(&sess)
	if err := h.rex.Call(context.Background(), "session.new_window", map[string]any{"session_id": sess, "window_label": "other", "focus": true,
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell",
			"options": map[string]any{"command": []string{"/bin/sleep", "600"}, "login": false}}}}, nil); err != nil {
		t.Fatal(err)
	}
}

// Codex's case: the hub restarts (attachments end) while the composer runs, perhaps with an
// unsaved draft. The user closes the transcript before the composer attaches again. The composer
// must stay until its gate runs: the request waits, goes to the new attachment, and the pair
// closes only when the composer calls hub.view.close.
func TestPairCloseBeforeComposerReattachLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	ctx := context.Background()
	composer := func() perform {
		var p perform
		p.Requester.ClientID = "client:lab-composer"
		p.Requester.Principal.PID = v.ChildPID
		return p
	}
	if _, err := h.viewAttach(ctx, composer(), map[string]any{"view_id": v.ID, "schema_version": float64(1)}); err != nil {
		t.Fatal(err)
	}
	pairRestart(t, h)
	tr := transcriptOf(t, h, v.ID)
	userClose(t, h, v.Session, tr.Block)
	passes(t, h, 2)
	if w := pairPanes(t, h, v.ID); w.Composers != 1 || w.Transcripts != 0 {
		t.Fatalf("the composer closed without its gate: %+v", w)
	}
	var pending bool
	h.db.QueryRow(`SELECT close_pending FROM view_transcripts WHERE view_id = ?`, v.ID).Scan(&pending)
	if !pending || !pairEvent(t, h, "view_close_pending", v.ID) || pairEvent(t, h, "view_close_requested", v.ID) {
		t.Fatalf("pending=%v before the composer attached", pending)
	}
	// The composer attaches again: it gets the request.
	out, err := h.viewAttach(ctx, composer(), map[string]any{"view_id": v.ID, "schema_version": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if !waitPairEvent(t, h, "view_close_requested", v.ID) {
		t.Fatal("the new attachment got no close request")
	}
	// The gate passes: the composer closes the pair.
	att := out.(map[string]any)["attachment"].(string)
	if _, err := h.viewClose(ctx, composer(), map[string]any{"view_id": v.ID, "attachment": att}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond) // the hub closes the pane after the answer
	if w := pairPanes(t, h, v.ID); w.Composers != 0 || w.Transcripts != 0 {
		t.Fatalf("panes after the gate: %+v", w)
	}
	if transcriptOf(t, h, v.ID) != nil {
		t.Fatal("the transcript row stays")
	}
}

// waitPairEvent waits for an event. The lab client never answers a perform, so a request
// event comes after its retries.
func waitPairEvent(t *testing.T, h *Hub, name, viewID string) bool {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if pairEvent(t, h, name, viewID) {
			return true
		}
	}
	return false
}

// Codex's review case 1: a hub restart clears the composer's attachment while its renderer
// runs; a transcript close then must not close the composer before its gate.
func TestReviewPairCloseAfterRestartPreservesComposerLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	h.db.Exec(`UPDATE views SET client_id = 'client:old', attachment = 'old' WHERE view_id = ?`, v.ID)
	pairRestart(t, h)
	tr := transcriptOf(t, h, v.ID)
	userClose(t, h, v.Session, tr.Block)
	passes(t, h, 2)
	if w := pairPanes(t, h, v.ID); w.Composers != 1 {
		t.Fatalf("Live composer closed before it could reattach and run its close gate: %+v", w)
	}
}

// Codex's review case 2: the user closes the composer after new_split created the transcript
// but before the hub adopted it. The late pane must not survive.
func TestReviewPairComposerClosesDuringSplitLab(t *testing.T) {
	h := pairLabHub(t)
	orig := transcriptSplit
	transcriptSplit = func(h *Hub, ctx context.Context, args map[string]any, out any) error {
		err := orig(h, ctx, args, out)
		if err != nil {
			return err
		}
		v := onlyView(t, h)
		userClose(t, h, v.Session, v.Block)
		return nil
	}
	t.Cleanup(func() { transcriptSplit = orig })
	declarePair(t, h, layoutTranscript)
	passes(t, h, 3)
	var sess string
	h.db.QueryRow(`SELECT session_id FROM ui_sessions WHERE plugin = ?`, uiSessionKey).Scan(&sess)
	blocks, err := h.blocksIn(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if strings.HasPrefix(b.Label, transcriptLabel) {
			t.Fatalf("Orphan transcript survived composer close during split: %+v", b)
		}
	}
}

// The view closes while the split's answer is lost: the record stays (retiring), and the sweep
// closes the late pane by label and process proof.
func TestPairComposerClosesDuringLostSplitLab(t *testing.T) {
	h := pairLabHub(t)
	orig := transcriptSplit
	transcriptSplit = func(h *Hub, ctx context.Context, args map[string]any, out any) error {
		orig(h, ctx, args, out)
		return &rex.RexError{Code: "disconnected", Message: "lab: the answer is lost"}
	}
	t.Cleanup(func() { transcriptSplit = orig })
	declarePair(t, h, layoutTranscript)
	passes(t, h, 1)
	v := onlyView(t, h)
	userClose(t, h, v.Session, v.Block)
	if tr := transcriptOf(t, h, v.ID); tr == nil || tr.Status != "retiring" {
		t.Fatalf("the split record was dropped: %+v", tr)
	}
	transcriptSplit = orig
	pairRestart(t, h) // also across a restart
	passes(t, h, 2)
	if w := pairPanes(t, h, v.ID); w.Transcripts != 0 || w.Composers != 0 {
		t.Fatalf("panes: %+v", w)
	}
	if transcriptOf(t, h, v.ID) != nil {
		t.Fatal("the record stays after its pane closed")
	}
}

// A navigation cancels a pending close: a later try of the request does not deliver it.
func TestPairCloseRequestCancelledLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	h.db.Exec(`UPDATE view_transcripts SET status = 'declined', close_pending = 0 WHERE view_id = ?`, v.ID)
	h.requestClose("messaging", v.ID, "client:lab-none", "att-lab")
	if !pairEvent(t, h, "view_close_cancelled", v.ID) || pairEvent(t, h, "view_close_requested", v.ID) {
		t.Fatal("a cancelled close was sent")
	}
}

// A split whose answer is lost and whose pane never appears: after the view closes, its record
// stays (retiring). No timer drops the only record that can find a late pane.
func TestPairUnlandedSplitRecordStaysLab(t *testing.T) {
	h := pairLabHub(t)
	orig := transcriptSplit
	transcriptSplit = func(*Hub, context.Context, map[string]any, any) error {
		return &rex.RexError{Code: "disconnected", Message: "lab: no answer, no pane"}
	}
	t.Cleanup(func() { transcriptSplit = orig })
	declarePair(t, h, layoutTranscript)
	passes(t, h, 1)
	v := onlyView(t, h)
	userClose(t, h, v.Session, v.Block)
	passes(t, h, 3)
	if tr := transcriptOf(t, h, v.ID); tr == nil || tr.Status != "retiring" {
		t.Fatalf("record %+v", tr)
	}
}

// pairLayout reads the pair window's tiled layout from Rex.
func pairLayout(t *testing.T, h *Hub, v *view) (active bool, root layoutNode) {
	t.Helper()
	var sv struct {
		Active  string `json:"active_window_id"`
		Windows []struct {
			WindowID string `json:"window_id"`
			Layers   []struct {
				Layout layoutNode `json:"layout"`
			} `json:"layers"`
		} `json:"windows"`
	}
	if err := h.rex.Call(context.Background(), "session.view", map[string]any{"session_id": v.Session}, &sv); err != nil {
		t.Fatal(err)
	}
	for _, w := range sv.Windows {
		if w.WindowID == v.Window {
			return sv.Active == w.WindowID, w.Layers[0].Layout
		}
	}
	t.Fatal("no pair window")
	return
}

// openPairPanel opens a side panel in the pair's window, as a plugin's panel action would.
func openPairPanel(t *testing.T, h *Hub, v *view, id string) {
	t.Helper()
	ctx := formContext{SessionID: v.Session, WindowID: v.Window, Panel: true}
	if out, err := h.openSlot(context.Background(), slotRequest{ReplID: id, Plugin: "messaging", Route: "/panels/" + id, Ctx: ctx}); err != nil || out["status"] != "opened" {
		t.Fatalf("panel: %v %v", out, err)
	}
}

func countPairEvents(t *testing.T, h *Hub, name, viewID string) int {
	t.Helper()
	b, _ := os.ReadFile(h.ev.f.Name())
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, `"`+name+`"`) && strings.Contains(line, viewID) {
			n++
		}
	}
	return n
}

// assertWrapped checks [(transcript / composer) | panel]: a horizontal root whose before half is
// the pair, the composer directly under the transcript.
func assertWrapped(t *testing.T, h *Hub, v *view) {
	t.Helper()
	tr := transcriptOf(t, h, v.ID)
	_, root := pairLayout(t, h, v)
	if root.Split == nil || root.Split.Direction != "horizontal" || root.Split.After.Split != nil {
		t.Fatalf("root %+v", root)
	}
	if paired, panes := pairShape(root.Split.Before, tr.Block, v.Block); !paired || panes != 2 {
		t.Fatalf("before half %+v", root.Split.Before)
	}
}

// Active pair: the panel opens beside the transcript, the next pass moves the composer under the
// transcript, the composer stays focused, and closing the panel gives the pair the full width.
func TestPairPanelActiveLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	if _, err := h.uiNavigate(context.Background(), map[string]any{"plugin": "messaging", "tab": "thread/a"}); err != nil {
		t.Fatal(err)
	}
	passes(t, h, 1)
	openPairPanel(t, h, v, "repl-active")
	passes(t, h, 2)
	assertWrapped(t, h, v)
	if w := pairPanes(t, h, v.ID); w.Focused != w.Composer || !w.Active {
		t.Fatalf("focus %+v", w)
	}
	if n := countPairEvents(t, h, "pair_moved", v.ID); n != 1 {
		t.Fatalf("moves: %d", n)
	}
	if err := h.closeForm(context.Background(), slotViewID("repl-active"), "test"); err != nil {
		t.Fatal(err)
	}
	passes(t, h, 1)
	w := assertPair(t, h, v)
	if len(w.TranscriptRect) != 4 || w.TranscriptRect[2] != 1 {
		t.Fatalf("the pair does not take the full width: %v", w.TranscriptRect)
	}
}

// Background pair: the panel opens beside the transcript and the move waits, without touching
// the active window. Activation runs it once. Also across a hub restart.
func TestPairPanelBackgroundLab(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "no restart", true: "restart"}[restart], func(t *testing.T) {
			h := pairLabHub(t)
			declarePair(t, h, layoutTranscript)
			passes(t, h, 2)
			v := onlyView(t, h)
			otherWindow(t, h)
			openPairPanel(t, h, v, "repl-bg")
			passes(t, h, 3)
			tr := transcriptOf(t, h, v.ID)
			active, root := pairLayout(t, h, v)
			if paired, _ := pairShape(root, tr.Block, v.Block); paired || active {
				t.Fatalf("a background move ran: active=%v", active)
			}
			if countPairEvents(t, h, "pair_move_waiting", v.ID) == 0 || countPairEvents(t, h, "pair_moved", v.ID) != 0 {
				t.Fatal("no waiting move")
			}
			if restart {
				pairRestart(t, h)
			}
			if err := h.rex.Call(context.Background(), "session.focus_window", map[string]any{"session_id": v.Session, "window_id": v.Window}, nil); err != nil {
				t.Fatal(err)
			}
			passes(t, h, 3)
			assertWrapped(t, h, v)
			if n := countPairEvents(t, h, "pair_moved", v.ID); n != 1 {
				t.Fatalf("moves: %d", n)
			}
		})
	}
}

// The move's answer is lost: the next pass reads the layout and does not move again.
func TestPairPanelLostMoveAnswerLab(t *testing.T) {
	h := pairLabHub(t)
	orig := pairMove
	calls := 0
	pairMove = func(h *Hub, ctx context.Context, args map[string]any) error {
		calls++
		orig(h, ctx, args)
		return &rex.RexError{Code: "disconnected", Message: "lab: the answer is lost"}
	}
	t.Cleanup(func() { pairMove = orig })
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	if _, err := h.uiNavigate(context.Background(), map[string]any{"plugin": "messaging", "tab": "thread/a"}); err != nil {
		t.Fatal(err)
	}
	passes(t, h, 1)
	openPairPanel(t, h, v, "repl-lost")
	passes(t, h, 3)
	assertWrapped(t, h, v)
	if calls != 1 {
		t.Fatalf("move calls: %d", calls)
	}
}

// Worker-driven (no manual passes): Rex refuses the split; the hub's own retries reach the crash
// limit, with no other event.
func TestPairWorkerRefusedSplitRetriesLab(t *testing.T) {
	h := pairLabHub(t)
	orig := transcriptSplit
	var mu sync.Mutex
	refusals := 0
	transcriptSplit = func(*Hub, context.Context, map[string]any, any) error {
		mu.Lock()
		refusals++
		mu.Unlock()
		return &rex.RexError{Code: "bad_request", Message: "lab: refused"}
	}
	t.Cleanup(func() { transcriptSplit = orig })
	declarePair(t, h, layoutTranscript)
	pairWorker(t, h)
	waitFor(t, "refusals up to the crash limit", 30*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return refusals >= maxCrashes
	})
	time.Sleep(6 * time.Second) // one more retry period: no attempt past the limit
	mu.Lock()
	defer mu.Unlock()
	if refusals != maxCrashes {
		t.Fatalf("refusals %d", refusals)
	}
}

// Worker-driven: a background pair with a panel waits; activating its window (a Rex event, not a
// manual pass) runs the move once.
func TestPairWorkerActivationRunsMoveLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	pairWorker(t, h)
	var v *view
	waitFor(t, "the pair", 10*time.Second, func() bool {
		views, _ := h.loadViews(context.Background(), "messaging")
		for _, x := range views {
			if tr, _ := h.loadTranscript(context.Background(), x.ID); x.Status == "open" && tr != nil && tr.Status == "open" {
				v = x
				return true
			}
		}
		return false
	})
	otherWindow(t, h)
	openPairPanel(t, h, v, "repl-worker")
	waitFor(t, "a waiting move", 10*time.Second, func() bool { return countPairEvents(t, h, "pair_move_waiting", v.ID) > 0 })
	if countPairEvents(t, h, "pair_moved", v.ID) != 0 {
		t.Fatal("a background move ran")
	}
	if err := h.rex.Call(context.Background(), "session.focus_window", map[string]any{"session_id": v.Session, "window_id": v.Window}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the move after activation", 10*time.Second, func() bool { return countPairEvents(t, h, "pair_moved", v.ID) == 1 })
	time.Sleep(time.Second)
	if n := countPairEvents(t, h, "pair_moved", v.ID); n != 1 {
		t.Fatalf("moves: %d", n)
	}
	assertWrapped(t, h, v)
}

// snapLayout is the layout that the view's next snapshot reports to its composer.
func snapLayout(t *testing.T, h *Hub, id string) string {
	t.Helper()
	var l string
	if err := h.db.QueryRow(`SELECT `+effectiveLayout+` FROM views v LEFT JOIN view_transcripts x ON x.view_id = v.view_id
		WHERE v.view_id = ?`, id).Scan(&l); err != nil {
		t.Fatal(err)
	}
	return l
}

// sameComposer checks that a layout change kept the composer: the same row, pane and process
// (so its renderer keeps an unsaved draft and its pending operations).
func sameComposer(t *testing.T, h *Hub, before *view) *view {
	t.Helper()
	v := onlyView(t, h)
	if v.ID != before.ID || v.Block != before.Block || v.ChildPID != before.ChildPID || !alive(v.ChildPID, v.ChildStart) {
		t.Fatalf("the composer changed: %+v, before %+v", v, before)
	}
	return v
}

// The config enables pairs while a classic view is open in a background tab: the hub converts
// it in place. The composer process stays, the transcript pane comes, and the user's active tab
// stays active.
func TestPairEnableInPlaceLab(t *testing.T) {
	h := pairLabHub(t)
	h.cfg.Transcript = false
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	otherWindow(t, h)
	h.cfg.Transcript = true // the config edit and a hub restart
	pairRestart(t, h)
	passes(t, h, 3)
	v = sameComposer(t, h, v)
	if w := assertPair(t, h, v); w.Active {
		t.Fatal("the conversion made the background tab active")
	}
	if !pairEvent(t, h, "view_layout_changed", v.ID) || pairEvent(t, h, "view_closed", v.ID) {
		t.Fatal("no in-place layout change, or the composer closed")
	}
}

// Rollback: the config disables pairs. The transcript pane closes, the composer stays and shows
// the history again, and the background tab stays in the background.
func TestPairDisableInPlaceLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	assertPair(t, h, v)
	otherWindow(t, h)
	h.cfg.Transcript = false
	pairRestart(t, h)
	passes(t, h, 3)
	v = sameComposer(t, h, v)
	if w := pairPanes(t, h, v.ID); w.Composers != 1 || w.Transcripts != 0 || w.Active {
		t.Fatalf("after disable: %+v", w)
	}
	if tr := transcriptOf(t, h, v.ID); tr != nil || v.Layout != "" || snapLayout(t, h, v.ID) != "" {
		t.Fatalf("after disable: row %+v, layout %q", tr, v.Layout)
	}
	// Enable again: the same composer gets a new transcript.
	h.cfg.Transcript = true
	passes(t, h, 3)
	assertPair(t, h, sameComposer(t, h, v))
}

// Codex's case: the config disables pairs while a transcript split has an unknown outcome, and
// the pane exists. The row becomes retiring, and the sweep closes the late pane while the
// composer stays open.
func TestPairDisableDuringUnknownSplitLab(t *testing.T) {
	h := pairLabHub(t)
	orig := transcriptSplit
	transcriptSplit = func(h *Hub, ctx context.Context, args map[string]any, out any) error {
		transcriptSplit = orig
		orig(h, ctx, args, out)
		return &rex.RexError{Code: "disconnected", Message: "lab: the answer is lost"}
	}
	t.Cleanup(func() { transcriptSplit = orig })
	declarePair(t, h, layoutTranscript)
	passes(t, h, 1)
	v := onlyView(t, h)
	if tr := transcriptOf(t, h, v.ID); tr == nil || tr.Status != "sending" {
		t.Fatalf("setup: %+v", tr)
	}
	h.cfg.Transcript = false
	passes(t, h, 1)
	if tr := transcriptOf(t, h, v.ID); tr == nil || tr.Status != "retiring" {
		t.Fatalf("after disable: %+v", tr)
	}
	passes(t, h, 2)
	v = sameComposer(t, h, v)
	if w := pairPanes(t, h, v.ID); w.Composers != 1 || w.Transcripts != 0 {
		t.Fatalf("after the sweep: %+v", w)
	}
	if tr := transcriptOf(t, h, v.ID); tr != nil || !pairEvent(t, h, "transcript_late_closed", v.ID) {
		t.Fatalf("the late pane was not retired: %+v", tr)
	}
}

// The split answer is lost and no pane appears; the config disables pairs (retiring), then
// enables them again before the outcome is known. The same row resolves the split once: one new
// transcript opens, and a pane of the lost split that lands later is closed, not adopted.
func TestPairReenableDuringRetirementLab(t *testing.T) {
	h := pairLabHub(t)
	orig := transcriptSplit
	transcriptSplit = func(*Hub, context.Context, map[string]any, any) error {
		transcriptSplit = orig
		return &rex.RexError{Code: "disconnected", Message: "lab: the answer is lost, no pane yet"}
	}
	t.Cleanup(func() { transcriptSplit = orig })
	declarePair(t, h, layoutTranscript)
	passes(t, h, 1)
	v := onlyView(t, h)
	h.cfg.Transcript = false
	passes(t, h, 2)
	if tr := transcriptOf(t, h, v.ID); tr == nil || tr.Status != "retiring" {
		t.Fatalf("while disabled: %+v (the record must wait for an outcome)", tr)
	}
	h.cfg.Transcript = true
	passes(t, h, 3)
	v = sameComposer(t, h, v)
	first := assertPair(t, h, v)
	// The lost split lands now.
	if err := orig(h, context.Background(), map[string]any{"session_id": v.Session, "anchor_block_id": v.Block,
		"direction": "vertical", "side": "before", "ratio": 0.5, "focus": false,
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "label": transcriptLabel + v.ID,
			"options": map[string]any{"command": h.viewCommand(v.ID, v.Plugin, v.Route, "-transcript"), "login": false}}}}, nil); err != nil {
		t.Fatal(err)
	}
	passes(t, h, 2)
	if w := assertPair(t, h, v); w.Transcript != first.Transcript {
		t.Fatalf("the late pane replaced the adopted transcript: %s, first %s", w.Transcript, first.Transcript)
	}
}

// The user closed the transcript, and the composer's close gate has not run yet (not attached
// after a restart). Disabling pairs keeps the pending close: the composer still gets
// view.close_request when it attaches, and its snapshot shows the history meanwhile.
func TestPairDisableKeepsPendingCloseLab(t *testing.T) {
	h := pairLabHub(t)
	declarePair(t, h, layoutTranscript)
	passes(t, h, 2)
	v := onlyView(t, h)
	w := assertPair(t, h, v)
	pairRestart(t, h) // attachments end
	userClose(t, h, v.Session, w.Transcript)
	waitFor(t, "close pending", 5*time.Second, func() bool { return pairEvent(t, h, "view_close_pending", v.ID) })
	h.cfg.Transcript = false
	passes(t, h, 3)
	v = sameComposer(t, h, v)
	if tr := transcriptOf(t, h, v.ID); tr == nil || tr.Status != "declined" || !h.closePending(context.Background(), v.ID) {
		t.Fatalf("the pending close was lost: %+v", tr)
	}
	if snapLayout(t, h, v.ID) != "" {
		t.Fatal("a declined transcript must give the history back to the composer")
	}
}
