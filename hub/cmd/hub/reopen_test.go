package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// reopenRex is the Rex state of a reopen unit test: one session "s" with one window "w".
type reopenRex struct {
	mu       sync.Mutex
	blocks   []rexBlock
	focused  string
	children map[string]int
	splits   []map[string]any
	closed   []string
	split    string // "ok" (the default), "lost" (the pane appears, the answer is lost), "none" (lost, no pane)
	listErr  string // an error code for session.list_blocks: an incomplete inventory
	release  string // the answer to view.release: released (the default), confirm, or an error code
	child    int    // the pid of a new pane's child
}

func (x *reopenRex) answer(method string, payload json.RawMessage) (any, string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	var p struct {
		Block  string         `json:"block_id"`
		Layout map[string]any `json:"layout"`
	}
	json.Unmarshal(payload, &p)
	switch method {
	case "session.view":
		var bs []map[string]any
		for _, b := range x.blocks {
			bs = append(bs, map[string]any{"block_id": b.BlockID})
		}
		return map[string]any{"active_window_id": "w", "windows": []any{map[string]any{"window_id": "w", "focused_block_id": x.focused,
			"layers": []any{map[string]any{"kind": "tiled", "blocks": bs}}}}}, ""
	case "session.list_blocks":
		if x.listErr != "" {
			return nil, x.listErr
		}
		return map[string]any{"blocks": x.blocks}, ""
	case "com.superlogical.terminal.process":
		if pid := x.children[p.Block]; pid > 0 {
			return map[string]any{"child": map[string]any{"pid": pid}}, ""
		}
		return map[string]any{"child": nil}, ""
	case "session.new_split":
		var args map[string]any
		json.Unmarshal(payload, &args)
		x.splits = append(x.splits, args)
		label, _ := args["layout"].(map[string]any)["block"].(map[string]any)["label"].(string)
		switch x.split {
		case "none":
			return nil, "disconnected"
		case "lost":
			x.blocks = append(x.blocks, rexBlock{BlockID: "b-new", Label: label, WindowID: "w"})
			x.children["b-new"] = x.child
			return nil, "disconnected"
		}
		x.blocks = append(x.blocks, rexBlock{BlockID: "b-new", Label: label, WindowID: "w"})
		x.children["b-new"] = x.child
		return map[string]any{"block_ids": []string{"b-new"}}, ""
	case "block.close":
		x.closed = append(x.closed, p.Block)
		for i, b := range x.blocks {
			if b.BlockID == p.Block {
				x.blocks = append(x.blocks[:i], x.blocks[i+1:]...)
				break
			}
		}
	}
	return map[string]any{}, ""
}

func (x *reopenRex) splitCount() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.splits)
}

// reopenTestHub has a tab view "old" (plugin p, tab main, block b-old, attached as client c) and a
// form "form" whose source is old. The new pane's child carries the new view's ID in its argv.
func reopenTestHub(t *testing.T) (*Hub, *reopenRex, *performRex) {
	t.Helper()
	h := releaseTestHub(t)
	x := &reopenRex{focused: "b-old", children: map[string]int{}, blocks: []rexBlock{{BlockID: "b-old", Label: "lazyrex-view:old", WindowID: "w"}}}
	fr := newPerformRex(t, func(c performCall) (any, string) {
		x.mu.Lock()
		rel := x.release
		x.mu.Unlock()
		if c.Action != "view.release" {
			return map[string]any{}, ""
		}
		switch rel {
		case "", "released":
			return map[string]any{"status": "released"}, ""
		case "confirm":
			return map[string]any{"status": "confirm"}, ""
		}
		return nil, rel
	})
	fr.mu.Lock()
	fr.other = x.answer
	fr.mu.Unlock()
	h.rex = fr.dial(t)
	h.selfClosed = map[string]bool{}
	h.cfg.View = "view"
	h.ui = &uiState{kick: make(chan struct{}, 1), sess: map[string]string{"s": uiSessionKey}}
	h.vs = &viewSvc{workers: map[string]chan struct{}{"p": make(chan struct{}, 1)}} // a kick is recorded, and no worker runs
	child := exec.Command("/bin/sh", "-c", "sleep 60; true", reopenViewID(reopenID("p", "k")))
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	x.child = child.Process.Pid
	mustExec(t, h, `INSERT INTO ui_tabs (plugin, tab, title, route, pos) VALUES ('p', 'main', 'Main', '/main', 0)`)
	mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, session_id, window_id, block_id, client_id, attachment, attach_state)
		VALUES ('old', 'p', 'main', '/main', 'open', 's', 'w', 'b-old', 'c', 'att', 'attached')`)
	fc, _ := json.Marshal(formContext{SessionID: "s", WindowID: "w", SourceViewID: "old", SourceRoute: "/main"})
	mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, context, session_id, attach_state) VALUES ('form', 'p', ?, '/forms/a', 'open', ?, 's', 'attached')`, formTab, string(fc))
	return h, x, fr
}

func mustExec(t *testing.T, h *Hub, q string, args ...any) {
	t.Helper()
	if _, err := h.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func viewStatus(h *Hub, id string) (status, by, open string) {
	h.db.QueryRow(`SELECT status, closed_by, open_args FROM views WHERE view_id = ?`, id).Scan(&status, &by, &open)
	return
}

func reopenState(h *Hub, id string) string {
	var s string
	h.db.QueryRow(`SELECT state FROM ui_replacements WHERE repl_id = ?`, id).Scan(&s)
	return s
}

func fullOpen() reopenEffect { return reopenEffect{set: true, open: `{"full":true}`} }

// The reopen effect decodes as the renderer decodes open args (view.OpenArgs).
func TestReopenEffectDecode(t *testing.T) {
	for _, c := range []struct {
		in, open string
		set, ok  bool
	}{
		{`{"reopen": null}`, "", false, true},
		{`{"reopen": {}}`, "", true, true},
		{`{"reopen": {"args": null}}`, "", true, true},
		{`{"reopen": {"args": {}}}`, "", true, true},
		{`{"reopen": {"args": {"full": false}}}`, "", true, true},
		{`{"reopen": {"args": {"full": true}}}`, `{"full":true}`, true, true},
		{`{"reopen": {"args": {"full": null}}}`, "", true, false},
		{`{"reopen": {"args": {"full": "true"}}}`, "", true, false},
		{`{"reopen": {"args": {"full": 1}}}`, "", true, false},
		{`{"reopen": {"args": {"full": true, "x": 1}}}`, "", true, false},
		{`{"reopen": {"args": []}}`, "", true, false},
		{`{"reopen": {"args": "full"}}`, "", true, false},
		{`{"reopen": {"route": "/x"}}`, "", true, false},
		{`{"reopen": true}`, "", true, false},
		{`{"reopen": []}`, "", true, false},
	} {
		var e viewEffects
		err := json.Unmarshal([]byte(c.in), &e)
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v, want ok %v", c.in, err, c.ok)
			continue
		}
		if err == nil && (e.Reopen.set != c.set || e.Reopen.open != c.open || e.any() != c.set) {
			t.Errorf("%s: %+v", c.in, e.Reopen)
		}
	}
}

// Only a form may reopen: a tab view's own action or a panel's gets invalid_input, before any row
// or release, and the next reopen from a form works.
func TestReopenFormOnly(t *testing.T) {
	h, x, fr := reopenTestHub(t)
	ctx := context.Background()
	fc, _ := json.Marshal(formContext{SessionID: "s", WindowID: "w", SourceViewID: "old", Panel: true})
	mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, context, session_id) VALUES ('panel', 'p', ?, '/panels/a', 'open', ?, 's')`, formTab, string(fc))
	for _, acting := range []string{"old", "panel"} {
		if _, err := h.reopenStep(ctx, "p", "k-"+acting, acting, fullOpen()); err == nil || !strings.Contains(err.Error(), "invalid_input") || !strings.Contains(err.Error(), "only from a form") {
			t.Fatalf("%s: %v", acting, err)
		}
	}
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM ui_replacements`).Scan(&n)
	if n != 0 || len(fr.actions()) != 0 || x.splitCount() != 0 {
		t.Fatalf("a refused reopen left %d rows, actions %v, %d splits", n, fr.actions(), x.splitCount())
	}
	if done, err := h.reopenStep(ctx, "p", "k", "form", fullOpen()); !done || err != nil {
		t.Fatalf("the form's reopen: %v %v", done, err)
	}
}

// The source must be an open tab view of the plugin.
func TestReopenSourceChecks(t *testing.T) {
	h, x, _ := reopenTestHub(t)
	ctx := context.Background()
	form := func(id, src string) {
		fc, _ := json.Marshal(formContext{SessionID: "s", WindowID: "w", SourceViewID: src})
		mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, context, session_id) VALUES (?, 'p', ?, '/forms/a', 'open', ?, 's')`, id, formTab, string(fc))
	}
	mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, session_id, block_id) VALUES ('closed', 'p', 'main', '/main', 'closed', 's', 'b-x')`)
	mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, session_id, block_id) VALUES ('other', 'q', 'main', '/main', 'open', 's', 'b-y')`)
	form("f-none", "")
	form("f-closed", "closed")
	form("f-missing", "nope")
	form("f-other", "other")
	form("f-form", "form")
	for id, want := range map[string]string{"f-none": "the source view is closed", "f-closed": "the source view is closed", "f-missing": "the source view is closed",
		"f-other": "invalid_input", "f-form": "invalid_input"} {
		if _, err := h.reopenStep(ctx, "p", "k-"+id, id, fullOpen()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", id, err, want)
		}
	}
	if x.splitCount() != 0 {
		t.Fatal("a refused source split a pane")
	}
}

// A released reopen: one split anchored at the old pane, a new view with the args, the focus
// copied, then the old pane and row close. A retry of the same operation answers from the row.
func TestReopenReplacesThePane(t *testing.T) {
	h, x, fr := reopenTestHub(t)
	ctx := context.Background()
	done, err := h.reopenStep(ctx, "p", "k", "form", fullOpen())
	if !done || err != nil {
		t.Fatalf("reopen: %v %v", done, err)
	}
	id := reopenID("p", "k")
	nv := reopenViewID(id)
	if nv == "old" || !strings.HasPrefix(nv, "view-") {
		t.Fatalf("the new view ID %q", nv)
	}
	if s := reopenState(h, id); s != "done" {
		t.Fatalf("row %s", s)
	}
	if got := fr.actions(); len(got) != 1 || got[0] != "view.release" {
		t.Fatalf("actions %v", got)
	}
	if x.splitCount() != 1 {
		t.Fatalf("%d splits", x.splitCount())
	}
	sp := x.splits[0]
	label := sp["layout"].(map[string]any)["block"].(map[string]any)["label"]
	if sp["anchor_block_id"] != "b-old" || sp["focus"] != true || label != "lazyrex-view:"+nv {
		t.Fatalf("the split: %v", sp)
	}
	if st, _, open := viewStatus(h, nv); st != "open" || open != `{"full":true}` {
		t.Fatalf("the new view: %s %q", st, open)
	}
	if st, by, _ := viewStatus(h, "old"); st != "closed" || by != "hub" {
		t.Fatalf("the old view: %s %s", st, by)
	}
	var tabKey, route string
	h.db.QueryRow(`SELECT tab, route FROM views WHERE view_id = ?`, nv).Scan(&tabKey, &route)
	if tabKey != "main" || route != "/main" || len(x.closed) != 1 || x.closed[0] != "b-old" {
		t.Fatalf("tab %s route %s closed %v", tabKey, route, x.closed)
	}
	if done, err := h.reopenStep(ctx, "p", "k", "form", fullOpen()); !done || err != nil || x.splitCount() != 1 || len(fr.actions()) != 1 {
		t.Fatalf("the retry: %v %v, %d splits, %v", done, err, x.splitCount(), fr.actions())
	}
	// A reopen without args stores none.
	h2, _, _ := reopenTestHub(t)
	h2.reopenStep(ctx, "p", "k", "form", reopenEffect{set: true})
	if _, _, open := viewStatus(h2, nv); open != "" {
		t.Fatalf("open_args %q without args", open)
	}
}

// The focus moves to the new pane only if the old pane had it.
func TestReopenFocusCopy(t *testing.T) {
	h, x, _ := reopenTestHub(t)
	x.focused = "b-other"
	h.reopenStep(context.Background(), "p", "k", "form", fullOpen())
	if x.splitCount() != 1 || x.splits[0]["focus"] != false {
		t.Fatalf("the split: %v", x.splits)
	}
}

// A lost split answer: the pane is found by its label, and no second split is sent.
func TestReopenLostSplitAnswer(t *testing.T) {
	h, x, _ := reopenTestHub(t)
	ctx := context.Background()
	x.split = "lost"
	done, err := h.reopenStep(ctx, "p", "k", "form", fullOpen())
	if done || err != nil {
		t.Fatalf("an unknown outcome waits: %v %v", done, err)
	}
	id := reopenID("p", "k")
	if s := reopenState(h, id); s != "opening" {
		t.Fatalf("row %s", s)
	}
	h.slotSweep(ctx)
	done, err = h.reopenStep(ctx, "p", "k", "form", fullOpen())
	if !done || err != nil || x.splitCount() != 1 {
		t.Fatalf("after the sweep: %v %v, %d splits", done, err, x.splitCount())
	}
	if st, _, _ := viewStatus(h, reopenViewID(id)); st != "open" {
		t.Fatalf("the new view: %s", st)
	}
}

// No pane within the bound of the durable pane request: an incomplete inventory proves nothing; a
// complete one expires the row, the old view resumes, and a late pane is closed.
func TestReopenExpiry(t *testing.T) {
	h, x, fr := reopenTestHub(t)
	ctx := context.Background()
	x.split = "none"
	h.reopenStep(ctx, "p", "k", "form", fullOpen())
	id := reopenID("p", "k")
	nv := reopenViewID(id)
	mustExec(t, h, `UPDATE ui_replacements SET split_at = ? WHERE repl_id = ?`, time.Now().Add(-reopenBound-time.Second).UTC().Format(time.RFC3339Nano), id)
	x.listErr = "disconnected"
	h.slotSweep(ctx)
	if s := reopenState(h, id); s != "opening" {
		t.Fatalf("an incomplete inventory: row %s", s)
	}
	x.listErr = ""
	h.slotSweep(ctx)
	if s := reopenState(h, id); s != "expired" {
		t.Fatalf("a complete inventory: row %s", s)
	}
	if st, by, _ := viewStatus(h, nv); st != "closed" || by != "abandoned" {
		t.Fatalf("the new view: %s %s", st, by)
	}
	if st, _, _ := viewStatus(h, "old"); st != "open" {
		t.Fatalf("the old view: %s", st)
	}
	waitFor(t, "view.resume", 5*time.Second, func() bool { return slices.Contains(fr.actions(), "view.resume") })
	if _, err := h.reopenStep(ctx, "p", "k", "form", fullOpen()); err == nil || !strings.Contains(err.Error(), "the view did not reopen; try again") {
		t.Fatalf("the effect: %v", err)
	}
	// A renderer that reattaches sees cancelled.
	r, _ := h.slotRow(ctx, h.db, id)
	if rendererState(r.State) != "cancelled" {
		t.Fatalf("renderer state %s", rendererState(r.State))
	}
	// The late pane.
	x.mu.Lock()
	x.blocks = append(x.blocks, rexBlock{BlockID: "b-late", Label: "lazyrex-view:" + nv, WindowID: "w"})
	x.mu.Unlock()
	h.slotSweep(ctx)
	if s := reopenState(h, id); s != "late_closed" || !slices.Contains(x.closed, "b-late") {
		t.Fatalf("the late pane: row %s, closed %v", s, x.closed)
	}
}

// The bound starts at the pane request, not at the row: a confirmation older than the bound, then
// discard, does not expire at once.
func TestReopenBoundStartsAtTheRequest(t *testing.T) {
	h, x, fr := reopenTestHub(t)
	ctx := context.Background()
	x.release = "confirm"
	x.split = "none"
	h.reopenStep(ctx, "p", "k", "form", fullOpen())
	id := reopenID("p", "k")
	mustExec(t, h, `UPDATE ui_replacements SET created = ? WHERE repl_id = ?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), id)
	var p perform
	p.Requester.ClientID = "c"
	if _, err := h.releaseConfirm(ctx, p, map[string]any{"view_id": "old", "attachment": "att", "replacement_id": id, "choice": "discard"}); err != nil {
		t.Fatal(err)
	}
	h.slotSweep(ctx)
	if s := reopenState(h, id); s != "opening" {
		t.Fatalf("row %s right after the request", s)
	}
	_ = fr
}

// A never-attached source is claimed; after an expiry it can attach as new, and a second reopen
// works.
func TestReopenClaimedSourceAfterExpiry(t *testing.T) {
	h, x, fr := reopenTestHub(t)
	ctx := context.Background()
	mustExec(t, h, `UPDATE views SET client_id = '', attachment = '', attach_state = 'never' WHERE view_id = 'old'`)
	x.split = "none"
	h.reopenStep(ctx, "p", "k", "form", fullOpen())
	id := reopenID("p", "k")
	var as string
	h.db.QueryRow(`SELECT attach_state FROM views WHERE view_id = 'old'`).Scan(&as)
	if as != "claimed" || len(fr.actions()) != 0 {
		t.Fatalf("the claim: %s, actions %v", as, fr.actions())
	}
	mustExec(t, h, `UPDATE ui_replacements SET split_at = ? WHERE repl_id = ?`, time.Now().Add(-reopenBound-time.Second).UTC().Format(time.RFC3339Nano), id)
	h.slotSweep(ctx)
	h.db.QueryRow(`SELECT attach_state FROM views WHERE view_id = 'old'`).Scan(&as)
	if s := reopenState(h, id); s != "expired" || as != "never" {
		t.Fatalf("row %s, attach_state %s", s, as)
	}
	x.split = "ok"
	if done, err := h.reopenStep(ctx, "p", "k2", "form", fullOpen()); !done || err != nil {
		t.Fatalf("a second reopen: %v %v", done, err)
	}
}

// The user decides: keep cancels with the unsaved-input answer; a second reopen meanwhile is a
// conflict; draft-save sends no split before released.
func TestReopenConfirm(t *testing.T) {
	h, x, _ := reopenTestHub(t)
	ctx := context.Background()
	x.release = "confirm"
	done, err := h.reopenStep(ctx, "p", "k", "form", fullOpen())
	id := reopenID("p", "k")
	if done || err != nil || reopenState(h, id) != "confirming" || x.splitCount() != 0 {
		t.Fatalf("confirming: %v %v %s, %d splits", done, err, reopenState(h, id), x.splitCount())
	}
	if _, err := h.reopenStep(ctx, "p", "k2", "form", fullOpen()); err == nil || !strings.Contains(err.Error(), "the view is reopening") {
		t.Fatalf("a second reopen: %v", err)
	}
	if done, err := h.reopenStep(ctx, "p", "k", "form", fullOpen()); done || err != nil {
		t.Fatalf("a waiting retry: %v %v", done, err)
	}
	var p perform
	p.Requester.ClientID = "c"
	h.releaseConfirm(ctx, p, map[string]any{"view_id": "old", "attachment": "att", "replacement_id": id, "choice": "keep"})
	if _, err := h.reopenStep(ctx, "p", "k", "form", fullOpen()); err == nil || !strings.Contains(err.Error(), "the view has unsaved input; it was not reopened") {
		t.Fatalf("kept: %v", err)
	}
	if x.splitCount() != 0 {
		t.Fatal("a kept reopen split")
	}
}

// A row left in releasing (the release step did not finish) asks once more on the retry.
func TestReopenReleasingRetry(t *testing.T) {
	h, x, fr := reopenTestHub(t)
	ctx := context.Background()
	id := reopenID("p", "k")
	b, _ := json.Marshal(tabRowContext{Tab: "main"})
	mustExec(t, h, `INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, focus, state, next_title, created, kind)
		VALUES (?, 's', 'w', 'old', ?, 'p', '/main', ?, 1, 'releasing', 'Reopening', '', 'tab')`, id, reopenViewID(id), string(b))
	if done, err := h.reopenStep(ctx, "p", "k", "form", fullOpen()); !done || err != nil {
		t.Fatalf("the retry: %v %v", done, err)
	}
	if got := fr.actions(); len(got) != 1 || got[0] != "view.release" || x.splitCount() != 1 {
		t.Fatalf("actions %v, %d splits", got, x.splitCount())
	}
}

// A user close of the old pane, or Ctrl+Q in it, while the row waits ends the row: the tab is
// closed, and the effect fails.
func TestReopenUserCloseWhileConfirming(t *testing.T) {
	for _, how := range []string{"pane", "ctrl-q"} {
		h, x, _ := reopenTestHub(t)
		ctx := context.Background()
		x.release = "confirm"
		h.reopenStep(ctx, "p", "k", "form", fullOpen())
		id := reopenID("p", "k")
		if how == "pane" {
			h.uiBlockClosed(ctx, "b-old", false)
		} else {
			var p perform
			p.Requester.ClientID = "c"
			if _, err := h.viewClose(ctx, p, map[string]any{"view_id": "old", "attachment": "att"}); err != nil {
				t.Fatal(err)
			}
		}
		var sup int
		h.db.QueryRow(`SELECT suppressed FROM ui_tabs WHERE plugin = 'p' AND tab = 'main'`).Scan(&sup)
		if s := reopenState(h, id); s != "user_closed" || sup != 1 {
			t.Fatalf("%s: row %s, suppressed %d", how, s, sup)
		}
		if _, err := h.reopenStep(ctx, "p", "k", "form", fullOpen()); err == nil || !strings.Contains(err.Error(), "the source view is closed") {
			t.Fatalf("%s: %v", how, err)
		}
	}
}

// A user close of the new pane in closing ends the row as user_closed too.
func TestReopenUserCloseOfTheNewPane(t *testing.T) {
	h, _, _ := reopenTestHub(t)
	ctx := context.Background()
	id := reopenID("p", "k")
	nv := reopenViewID(id)
	b, _ := json.Marshal(tabRowContext{Tab: "main"})
	mustExec(t, h, `INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, focus, state, next_title, created, kind)
		VALUES (?, 's', 'w', 'old', ?, 'p', '/main', ?, 1, 'closing', 'Reopening', '', 'tab')`, id, nv, string(b))
	mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, session_id, window_id, block_id) VALUES (?, 'p', 'main', '/main', 'open', 's', 'w', 'b-new')`, nv)
	h.uiBlockClosed(ctx, "b-new", false)
	if s := reopenState(h, id); s != "user_closed" {
		t.Fatalf("row %s", s)
	}
}

// The old renderer exits: while the row waits, the row is cancelled; the crash path counts once.
func TestReopenOldExitWhileConfirming(t *testing.T) {
	h, x, _ := reopenTestHub(t)
	ctx := context.Background()
	x.release = "confirm"
	h.reopenStep(ctx, "p", "k", "form", fullOpen())
	id := reopenID("p", "k")
	h.uiChildExited(ctx, "b-old")
	var crashes int
	h.db.QueryRow(`SELECT crashes FROM ui_tabs WHERE plugin = 'p' AND tab = 'main'`).Scan(&crashes)
	if s := reopenState(h, id); s != "cancelled" || crashes != 1 {
		t.Fatalf("row %s, crashes %d", s, crashes)
	}
	if _, err := h.reopenStep(ctx, "p", "k", "form", fullOpen()); err == nil || !strings.Contains(err.Error(), "the source view is closed") {
		t.Fatalf("the effect: %v", err)
	}
}

// The old renderer exits after the pane request, at the crash limit: the tab is suppressed, and
// the row goes on and closes the old pane.
func TestReopenOldExitAtTheCrashLimit(t *testing.T) {
	h, x, _ := reopenTestHub(t)
	ctx := context.Background()
	mustExec(t, h, `UPDATE ui_tabs SET crashes = 2`)
	x.split = "none"
	h.reopenStep(ctx, "p", "k", "form", fullOpen())
	id := reopenID("p", "k")
	h.uiChildExited(ctx, "b-old")
	var crashes, sup int
	h.db.QueryRow(`SELECT crashes, suppressed FROM ui_tabs WHERE plugin = 'p' AND tab = 'main'`).Scan(&crashes, &sup)
	if s := reopenState(h, id); s != "opening" || crashes != 3 || sup != 1 {
		t.Fatalf("row %s, crashes %d, suppressed %d", s, crashes, sup)
	}
	x.mu.Lock()
	x.blocks = append(x.blocks, rexBlock{BlockID: "b-new", Label: "lazyrex-view:" + reopenViewID(id), WindowID: "w"})
	x.children["b-new"] = x.child
	x.mu.Unlock()
	h.slotSweep(ctx)
	if s := reopenState(h, id); s != "done" || !slices.Contains(x.closed, "b-old") {
		t.Fatalf("row %s, closed %v", s, x.closed)
	}
}

// A restart in closing closes the old pane once.
func TestReopenRestartInClosing(t *testing.T) {
	h, x, _ := reopenTestHub(t)
	ctx := context.Background()
	id := reopenID("p", "k")
	nv := reopenViewID(id)
	b, _ := json.Marshal(tabRowContext{Tab: "main"})
	mustExec(t, h, `INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, focus, state, next_title, created, kind)
		VALUES (?, 's', 'w', 'old', ?, 'p', '/main', ?, 1, 'closing', 'Reopening', '', 'tab')`, id, nv, string(b))
	mustExec(t, h, `INSERT INTO views (view_id, plugin, tab, route, status, session_id, window_id, block_id) VALUES (?, 'p', 'main', '/main', 'open', 's', 'w', 'b-new')`, nv)
	h.slotSweep(ctx)
	h.slotSweep(ctx)
	if s := reopenState(h, id); s != "done" || len(x.closed) != 1 {
		t.Fatalf("row %s, closed %v", s, x.closed)
	}
	if done, err := h.reopenStep(ctx, "p", "k", "form", fullOpen()); !done || err != nil {
		t.Fatalf("the effect after the restart: %v %v", done, err)
	}
}

// A failed block.close keeps the row in closing.
func TestReopenFailedCloseKeepsTheRow(t *testing.T) {
	h, x, fr := reopenTestHub(t)
	ctx := context.Background()
	fr.mu.Lock()
	base := fr.other
	fr.other = func(m string, p json.RawMessage) (any, string) {
		if m == "block.close" {
			return nil, "internal"
		}
		return base(m, p)
	}
	fr.mu.Unlock()
	done, _ := h.reopenStep(ctx, "p", "k", "form", fullOpen())
	id := reopenID("p", "k")
	if done || reopenState(h, id) != "closing" {
		t.Fatalf("done %v, row %s", done, reopenState(h, id))
	}
	if st, _, _ := viewStatus(h, "old"); st != "open" {
		t.Fatalf("the old view: %s", st)
	}
	_ = x
}

// A side-slot open in the same window and a tab row do not supersede each other.
func TestReopenAndSlotRowsAreIndependent(t *testing.T) {
	h, x, _ := reopenTestHub(t)
	ctx := context.Background()
	x.release = "confirm"
	h.reopenStep(ctx, "p", "k", "form", fullOpen())
	id := reopenID("p", "k")
	mustExec(t, h, `INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, state, next_title, created)
		VALUES ('slot', 's', 'w', '', 'n', 'p', '/panels/a', '{}', 'opening', '', '')`)
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM ui_replacements WHERE kind = 'slot' AND window_id = 'w' AND state IN `+activeStates).Scan(&n)
	if n != 1 || reopenState(h, id) != "confirming" {
		t.Fatalf("slot rows %d, the tab row %s", n, reopenState(h, id))
	}
}

// The new view gets its open args in render args and in its snapshot; a view without has neither.
func TestReopenOpenArgs(t *testing.T) {
	a := viewArgs("v", "/main", "", `{"full":true}`)
	if string(a["open"].(json.RawMessage)) != `{"full":true}` {
		t.Fatalf("args %v", a)
	}
	if _, ok := viewArgs("v", "/main", "", "")["open"]; ok {
		t.Fatal("open without open_args")
	}
}

