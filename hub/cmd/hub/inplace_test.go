package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

// inPlaceHub is releaseTestHub with a fake Rex whose non-perform answers come from other, and
// whose client.perform answers are {}. The test process stands in for the pane's renderer: its pid
// and start time are the views' child process.
func inPlaceHub(t *testing.T, other func(method string, payload json.RawMessage) (any, string)) (*Hub, *performRex, int, string) {
	t.Helper()
	h := releaseTestHub(t)
	h.selfClosed = map[string]bool{}
	h.vs = newViewSvc()
	h.ui = &uiState{kick: make(chan struct{}, 1), sess: map[string]string{}}
	fr := newPerformRex(t, func(performCall) (any, string) { return map[string]any{}, "" })
	fr.other = other
	h.rex = fr.dial(t)
	pid := os.Getpid()
	return h, fr, pid, startTime(pid)
}

func addView(t *testing.T, h *Hub, id, status, closedBy, block string, pid int, start string) {
	t.Helper()
	if _, err := h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, closed_by, session_id, block_id, child_pid, child_start, client_id, attachment, attach_state, context)
		VALUES (?, 'p', ?, ?, ?, ?, 's', ?, ?, ?, 'c', 'att-'||?, 'attached', '{"window_id":"w"}')`, id, formTab, "/forms/"+id, status, closedBy, block, pid, start, id); err != nil {
		t.Fatal(err)
	}
}

// hub.view.attach of a view that an in-place transfer replaced answers the block's current view:
// the end of the chain A -> B -> C, with its plugin and route, and no attachment. It answers so
// only to a process in that view's pane; a caller outside gets the pane refusal. A view that the
// hub closed for another reason stays an unknown view.
func TestAttachOfReplacedViewNamesTheCurrentView(t *testing.T) {
	h, _, pid, start := inPlaceHub(t, nil)
	ctx := context.Background()
	addView(t, h, "A", "closed", "replaced", "b1", pid, start)
	addView(t, h, "B", "closed", "replaced", "b1", pid, start)
	addView(t, h, "C", "open", "", "b1", pid, start)
	addView(t, h, "X", "closed", "hub", "b2", pid, start)
	attach := func(view string, caller int) (map[string]any, error) {
		var p perform
		p.Requester.ClientID = "renderer"
		p.Requester.Principal.PID = caller
		out, err := h.viewAttach(ctx, p, map[string]any{"view_id": view, "schema_version": float64(1)})
		m, _ := out.(map[string]any)
		return m, err
	}
	for _, x := range []string{"A", "B"} {
		out, err := attach(x, pid)
		if err != nil || out["status"] != "replaced" || out["current_view_id"] != "C" || out["route"] != "/forms/C" || out["plugin"] != "p" {
			t.Fatalf("attach %s from the pane: %v %v, want replaced by C", x, out, err)
		}
		if _, ok := out["attachment"]; ok {
			t.Fatalf("attach %s: an attachment was made: %v", x, out)
		}
	}
	if _, err := attach("A", 1); err == nil || !strings.Contains(err.Error(), "caller is not in the pane") {
		t.Fatalf("attach A from outside the pane: %v, want the pane refusal", err)
	}
	if _, err := attach("X", pid); err == nil || !strings.Contains(err.Error(), "unknown or closed view") {
		t.Fatalf("attach of a view the hub closed: %v, want unknown or closed", err)
	}
	var att string
	h.db.QueryRow(`SELECT attachment FROM views WHERE view_id = 'C'`).Scan(&att)
	if att != "att-C" {
		t.Fatalf("the redirect changed C's attachment: %q", att)
	}
}

// The decision point: in place only when the old view is attached in a live pane and its
// renderer offers view.become. A missing action or a failed client.inspect means the fallback.
func TestInPlaceOKNeedsViewBecome(t *testing.T) {
	var mu sync.Mutex
	inspect := map[string]any{"actions": []any{map[string]any{"name": "view.become"}}}
	code := ""
	h, fr, pid, start := inPlaceHub(t, func(method string, _ json.RawMessage) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		if method == "client.inspect" {
			return inspect, code
		}
		return nil, ""
	})
	ctx := context.Background()
	addView(t, h, "v", "open", "", "b1", pid, start)
	r := &slotRow{ID: "R", Kind: "slot", Old: "v", New: "n"}
	if !h.inPlaceOK(ctx, r) {
		t.Fatal("a renderer with view.become: not in place")
	}
	mu.Lock()
	inspect = map[string]any{"actions": []any{map[string]any{"name": "view.release"}}}
	mu.Unlock()
	if h.inPlaceOK(ctx, r) {
		t.Fatal("a renderer without view.become: in place")
	}
	mu.Lock()
	code = "not_found"
	mu.Unlock()
	if h.inPlaceOK(ctx, r) {
		t.Fatal("a failed client.inspect: in place")
	}
	n := len(fr.methodsSeen())
	h.db.Exec(`UPDATE views SET attach_state = 'never' WHERE view_id = 'v'`)
	if h.inPlaceOK(ctx, r) || len(fr.methodsSeen()) != n {
		t.Fatal("a never-attached old view: in place, or Rex was asked")
	}
	if h.inPlaceOK(ctx, &slotRow{ID: "T", Kind: "tab", Old: "v", New: "n"}) {
		t.Fatal("a tab row: in place")
	}
}

// labelsSet lists the labels of the session.set_block_label requests that fr saw.
type labelLog struct {
	mu     sync.Mutex
	labels []string
}

func (l *labelLog) other(method string, payload json.RawMessage) (any, string) {
	if method == "session.set_block_label" {
		var a struct{ Label string }
		json.Unmarshal(payload, &a)
		l.mu.Lock()
		l.labels = append(l.labels, a.Label)
		l.mu.Unlock()
	}
	return nil, ""
}

func (l *labelLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.labels)
}

// The transfer: the new view takes the old view's block and process in one transaction, the old
// view closes as replaced (no pane close), and the row is becoming. A becoming row whose new view
// was the old one (A -> B still waits) is done, and its worker, run after the commit, renames
// nothing and notifies nobody: it never names B again.
func TestTransferInPlaceTakesThePaneAndRetiresTheOlderRow(t *testing.T) {
	ll := &labelLog{}
	h, fr, pid, start := inPlaceHub(t, ll.other)
	ctx := context.Background()
	addView(t, h, "A", "closed", "replaced", "b1", pid, start)
	addView(t, h, "B", "open", "", "b1", pid, start)
	for _, row := range [][]string{{"R1", "A", "B", "becoming"}, {"R2", "B", "C", "opening"}} {
		if _, err := h.db.Exec(`INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, state, next_title, created)
			VALUES (?, 's', 'w', ?, ?, 'p', ?, '{"window_id":"w"}', ?, '', '')`, row[0], row[1], row[2], "/forms/"+row[2], row[3]); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := h.slotRow(ctx, h.db, "R2")
	if err := h.transferInPlace(ctx, r); err != nil {
		t.Fatalf("transferInPlace: %v", err)
	}
	var status, block, closedBy string
	var cpid int
	h.db.QueryRow(`SELECT status, block_id, child_pid FROM views WHERE view_id = 'C'`).Scan(&status, &block, &cpid)
	if status != "open" || block != "b1" || cpid != pid {
		t.Fatalf("C: %s %s %d, want open in b1 with the same process", status, block, cpid)
	}
	h.db.QueryRow(`SELECT status, closed_by FROM views WHERE view_id = 'B'`).Scan(&status, &closedBy)
	if status != "closed" || closedBy != "replaced" {
		t.Fatalf("B: %s %s, want closed replaced", status, closedBy)
	}
	if m := fr.methodsSeen(); slices.Contains(m, "block.close") {
		t.Fatalf("a pane close: %v", m)
	}
	for id, want := range map[string]string{"R1": "done", "R2": "becoming"} {
		var st string
		h.db.QueryRow(`SELECT state FROM ui_replacements WHERE repl_id = ?`, id).Scan(&st)
		if st != want {
			t.Fatalf("%s is %q, want %q", id, st, want)
		}
	}
	// R1's worker, late: it stops at its first read.
	h.becomeWorker("R1", "w", make(chan struct{}, 1))
	for _, l := range ll.list() {
		if l == "lazyrex-form:B" {
			t.Fatalf("the retired worker renamed the pane to B: %v", ll.list())
		}
	}
	for _, c := range fr.calls {
		if c.Action == "view.become" && strings.Contains(string(c.Args), `"next_view_id":"B"`) {
			t.Fatalf("the retired worker asked for B: %s", c.Args)
		}
	}
	h.db.Exec(`UPDATE ui_replacements SET state = 'done' WHERE repl_id = 'R2'`) // stop R2's worker
}

// A becoming row ends: done once the label is set and the new view attached; late_closed with
// the new view closed when the renderer died and the pane stays (a held pane).
func TestBecomeWorkerEnds(t *testing.T) {
	ll := &labelLog{}
	h, _, pid, start := inPlaceHub(t, ll.other)
	addView(t, h, "A", "closed", "replaced", "b1", pid, start)
	addView(t, h, "B", "open", "", "b1", pid, start)
	addView(t, h, "D", "closed", "replaced", "b2", 999999, "gone")
	addView(t, h, "E", "open", "", "b2", 999999, "gone")
	for _, row := range [][]string{{"R1", "A", "B"}, {"R3", "D", "E"}} {
		h.db.Exec(`INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, state, next_title, created)
			VALUES (?, 's', 'w', ?, ?, 'p', '/forms/x', '{}', 'becoming', '', '')`, row[0], row[1], row[2])
	}
	h.becomeWorker("R1", "w", make(chan struct{}, 1)) // B is attached: the label, then done
	h.becomeWorker("R3", "w", make(chan struct{}, 1)) // E's renderer is gone
	for id, want := range map[string]string{"R1": "done", "R3": "late_closed"} {
		var st string
		h.db.QueryRow(`SELECT state FROM ui_replacements WHERE repl_id = ?`, id).Scan(&st)
		if st != want {
			t.Fatalf("%s is %q, want %q", id, st, want)
		}
	}
	var st string
	h.db.QueryRow(`SELECT status FROM views WHERE view_id = 'E'`).Scan(&st)
	if st != "closed" {
		t.Fatalf("E is %q, want closed", st)
	}
	if got := ll.list(); !slices.Equal(got, []string{"lazyrex-form:B"}) {
		t.Fatalf("labels %v, want only lazyrex-form:B", got)
	}
}

// A pane close for a view row skips a block that another open row owns: after an in-place
// transfer, the pane runs the other view.
func TestPaneCloseSkipsABlockThatAnotherViewOwns(t *testing.T) {
	h, fr, pid, start := inPlaceHub(t, nil)
	ctx := context.Background()
	addView(t, h, "old", "open", "", "b1", pid, start)
	addView(t, h, "cur", "open", "", "b1", pid, start)
	if !h.closeMoved(ctx, &view{ID: "old", Plugin: "p", Session: "s", Block: "b1"}) {
		t.Fatal("closeMoved failed")
	}
	addView(t, h, "old2", "open", "", "b1", pid, start)
	if err := h.closeForm(ctx, "old2", "test"); err != nil {
		t.Fatal(err)
	}
	if m := fr.methodsSeen(); slices.Contains(m, "block.close") {
		t.Fatalf("the shared pane was closed: %v", m)
	}
	var st string
	h.db.QueryRow(`SELECT status FROM views WHERE view_id = 'cur'`).Scan(&st)
	if st != "open" {
		t.Fatalf("cur is %q, want open", st)
	}
	addView(t, h, "alone", "open", "", "b9", pid, start)
	h.closeForm(ctx, "alone", "test")
	if m := fr.methodsSeen(); !slices.Contains(m, "block.close") {
		t.Fatalf("a pane that no other view owns stayed open: %v", m)
	}
}
