package main

import (
	"image/color"
	"strconv"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	tea "charm.land/bubbletea/v2"
)

func testModel() *model {
	m := newModel(&viewClient{ViewID: "v1", Plugin: "messaging", Route: "/thread/42", Requests: make(chan mutation, 16)})
	m.ready = true
	m.apply(testSnapshot("saved", "1"))
	m.setFocus("edit:composer")
	return m
}
func testSnapshot(text, rev string) Snapshot {
	return Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/thread/42", Revision: "1", Tree: Tree{Title: "Thread", Nodes: []Node{{ID: "composer", Type: "composer", Text: text, DraftRevision: rev, SaveAction: &Action{Name: "save", Enabled: true}, SendAction: &Action{Name: "send", Enabled: true}}}}}
}
func request(t *testing.T, m *model, cmd tea.Cmd) mutation {
	t.Helper()
	if cmd == nil {
		t.Fatal("No request command")
	}
	cmd()
	select {
	case req := <-m.client.Requests:
		return req
	default:
		t.Fatal("No request was queued")
		return mutation{}
	}
}

func TestSnapshotDoesNotOverwriteDirtyTextOrCursor(t *testing.T) {
	m := testModel()
	e := m.editors["composer"]
	e.setText("local text")
	e.Area.SetCursorColumn(3)
	m.apply(testSnapshot("remote text", "2"))
	if e.text() != "local text" || e.Revision != "1" || e.Area.Column() != 3 {
		t.Fatalf("Local state changed: %#v", e)
	}
	if e.RemoteText != "remote text" || e.RemoteRevision != "2" {
		t.Fatal("Missing conflict evidence")
	}
}

func TestSaveCoalescesEditsAndPreservesNewerText(t *testing.T) {
	m := testModel()
	e := m.editors["composer"]
	e.setText("first")
	a := request(t, m, m.save("composer"))
	e.setText("second")
	if m.save("composer") != nil {
		t.Fatal("A second save ran while the first was pending")
	}
	cmd := m.finish(completed{a, operationResult{Status: "succeeded", Result: map[string]any{"draft_revision": "2", "text": "first"}}})
	b := request(t, m, cmd)
	if b.Args["base_revision"] != "2" || b.Args["text"] != "second" || e.text() != "second" {
		t.Fatal("The newer edit was lost or saved against the wrong revision")
	}
}

func TestSendKeepsTextTypedBeforeReply(t *testing.T) {
	m := testModel()
	e := m.editors["composer"]
	e.WantSend = true
	e.SendText = e.text()
	send := request(t, m, m.save("composer"))
	if send.Method != "hub.view.send" {
		t.Fatal("Expected send")
	}
	e.setText("next message")
	m.apply(testSnapshot("", "2"))
	if e.text() != "next message" {
		t.Fatal("The send snapshot erased newer text")
	}
	cmd := m.finish(completed{send, operationResult{Status: "succeeded", Result: map[string]any{"draft_revision": "2", "cleared_draft_revision": "1", "message_id": "m1"}}})
	save := request(t, m, cmd)
	if save.Args["text"] != "next message" || save.Args["base_revision"] != "2" {
		t.Fatal("The newer draft did not survive send completion")
	}
}

func TestConflictNeedsExplicitResolution(t *testing.T) {
	m := testModel()
	e := m.editors["composer"]
	e.setText("mine")
	req := request(t, m, m.save("composer"))
	m.apply(testSnapshot("theirs", "2"))
	m.finish(completed{req, operationResult{Status: "failed", Error: &wireError{Code: "draft_conflict", Message: "Draft changed"}}})
	if m.save("composer") != nil || e.text() != "mine" {
		t.Fatal("The conflict overwrote text or retried automatically")
	}
	next := request(t, m, m.activate(control{Node: "composer", Kind: "keep_local"}))
	if next.Args["base_revision"] != "2" || next.Args["text"] != "mine" {
		t.Fatal("Explicit resolution did not use the remote revision")
	}
}

func TestEnterFreezesTextWhileDraftSaveIsPending(t *testing.T) {
	m := testModel()
	e := m.editors["composer"]
	e.setText("send this")
	save := request(t, m, m.save("composer"))
	m.activate(control{Node: "composer", Kind: "send"})
	e.setText("this is later text")
	cmd := m.finish(completed{save, operationResult{Status: "succeeded", Result: map[string]any{"draft_revision": "2", "text": "send this"}}})
	send := request(t, m, cmd)
	if send.Method != "hub.view.send" || send.Args["text"] != "send this" {
		t.Fatal("Typing during save changed the submitted message")
	}
	if m.activate(control{Node: "composer", Kind: "send"}) != nil {
		t.Fatal("Repeated Enter queued another send")
	}
}

func TestOldSnapshotCannotRestoreSentDraft(t *testing.T) {
	m := testModel()
	send := request(t, m, m.activate(control{Node: "composer", Kind: "send"}))
	m.finish(completed{send, operationResult{Status: "succeeded", Result: map[string]any{"draft_revision": "2", "message_id": "m1"}}})
	m.apply(testSnapshot("saved", "1"))
	e := m.editors["composer"]
	if e.text() != "" || e.Revision != "2" {
		t.Fatal("A stale snapshot restored a sent draft")
	}
}

func TestShiftEnterEditsAndEnterSends(t *testing.T) {
	m := testModel()
	e := m.editors["composer"]
	e.Area.CursorEnd()
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	if !strings.Contains(e.text(), "\n") {
		t.Fatal("Shift+Enter did not insert a newline")
	}
	// Bubble Tea executes the edit command asynchronously. The pending save already owns its key.
	if cmd == nil || e.Pending == "" {
		t.Fatal("The newline did not schedule a draft save")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !e.WantSend || e.SendText != e.text() {
		t.Fatal("Enter did not freeze the current text for sending")
	}
}

func TestMouseSendUsesVisibleButton(t *testing.T) {
	m := testModel()
	index := -1
	for i, r := range m.screenRows() {
		if r.Control != nil && r.Control.Kind == "send" {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("No visible Send control")
	}
	c := m.screenRows()[index].Control
	x := m.leftMargin() + c.Left + 1
	m.Update(tea.MouseClickMsg{X: x, Y: index, Button: tea.MouseLeft})
	_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: index, Button: tea.MouseLeft})
	if cmd == nil || !m.editors["composer"].Sending || m.editors["composer"].Pending == "" {
		t.Fatal("Clicking Send did not schedule a send")
	}
}

func TestLoadingSnapshotPreservesComposerDuringReconnect(t *testing.T) {
	m := testModel()
	e := m.editors["composer"]
	e.setText("pending text")
	s := testSnapshot("", "0")
	s.Tree.Nodes = []Node{{ID: "loading", Type: "loading", Text: "Loading"}}
	m.apply(s)
	if m.editors["composer"] != e || !strings.Contains(m.View().Content, "pending text") || len(m.recovery) != 0 {
		t.Fatal("Loading displaced the active draft")
	}
}

func TestSaveReplayWithDifferentTextCannotAdvanceRevision(t *testing.T) {
	m := testModel()
	e := m.editors["composer"]
	e.setText("mine")
	req := request(t, m, m.save("composer"))
	m.finish(completed{req, operationResult{Status: "succeeded", Result: map[string]any{"draft_revision": "9", "text": "someone else's newer draft"}}})
	if !e.Conflict || e.Revision != "1" || e.text() != "mine" {
		t.Fatal("The malformed replay advanced the draft revision")
	}
}

func TestRemovedDirtyComposerRetainsRecoveryText(t *testing.T) {
	m := testModel()
	m.editors["composer"].setText("do not lose")
	s := testSnapshot("", "2")
	s.Tree.Nodes = nil
	m.apply(s)
	if m.recovery["composer"] != "do not lose" || !strings.Contains(m.View().Content, "do not lose") {
		t.Fatal("The removed draft disappeared")
	}
}

func TestUnknownSchemaAndUnboundedHistoryAreRejected(t *testing.T) {
	s := testSnapshot("", "0")
	s.SchemaVersion = 2
	if s.Validate("v1", "/thread/42") == nil {
		t.Fatal("Accepted an unknown schema")
	}
	s.SchemaVersion = 1
	s.Tree.Nodes = []Node{{ID: "history", Type: "history", PageSize: 1, Messages: []Message{{ID: "1"}, {ID: "2"}}}}
	if s.Validate("v1", "/thread/42") == nil {
		t.Fatal("Accepted an oversized page")
	}
}

func TestInputControlsCannotEscapeIntoTerminal(t *testing.T) {
	s := plain("hello\x1b]52;c;c2VjcmV0\a\u009b31m")
	if strings.ContainsAny(s, "\x1b\a\u009b") {
		t.Fatal("Terminal control survived sanitization")
	}
}

func TestAttachRetryClassification(t *testing.T) {
	if !attachNotReady(&wireError{Code: "refused", Message: "not_ready: pane pending"}) {
		t.Fatal("Missing startup retry")
	}
	if attachNotReady(&wireError{Code: "refused", Message: "wrong pane"}) {
		t.Fatal("An identity refusal was treated as a startup race")
	}
}

// A view that starts detached gets no OSC 11 answer. It asks again when the app shows it.
func TestThemeIsCheckedAgain(t *testing.T) {
	m := newModel(nil)
	for _, msg := range []tea.Msg{tea.WindowSizeMsg{Width: 90, Height: 30}} {
		if _, cmd := m.update(msg); cmd == nil {
			t.Fatalf("%T did not ask for the background color", msg)
		}
	}
	if m.dark {
		t.Fatal("the default is light until the terminal answers")
	}
	m.update(tea.BackgroundColorMsg{Color: color.Black})
	if !m.dark {
		t.Fatal("a dark answer did not switch to the dark palette")
	}
	m.update(uv.LightColorSchemeEvent{})
	if m.dark {
		t.Fatal("a light scheme report (DEC 2031) did not switch to the light palette")
	}
	m.update(uv.DarkColorSchemeEvent{})
	if !m.dark {
		t.Fatal("a dark scheme report did not switch to the dark palette")
	}
}

func TestPopulatedSnapshotClearsOnlyLoadingStatus(t *testing.T) {
	m := testModel()
	loading := testSnapshot("", "2")
	loading.Tree.Nodes = []Node{{ID: "loading", Type: "loading", Text: "Loading"}}
	m.apply(loading)
	if m.status != loadingViewStatus || m.snapshot.Tree.Nodes[0].Type == "loading" {
		t.Fatal("Loading snapshot did not retain the tree and set its loading status")
	}
	m.apply(testSnapshot("updated", "3"))
	if m.status != "" || m.editors["composer"].text() != "updated" {
		t.Fatal("Populated snapshot did not clear the loading status")
	}
	m.status = "Could not save"
	m.apply(testSnapshot("updated", "4"))
	if m.status != "Could not save" {
		t.Fatal("Populated snapshot cleared an unrelated status")
	}
}

// The hub pairs and unpairs a view in place (Snapshot.Layout): the history rows go and come back,
// and the unsaved draft stays.
func TestSnapshotLayoutTogglesHistoryInPlace(t *testing.T) {
	m := testModel()
	m.editors["composer"].setText("unsaved draft")
	snap := func(layout string) Snapshot {
		s := testSnapshot("", "1")
		s.Revision = "2"
		s.Layout = layout
		history := Node{ID: "history", Type: "history", PageSize: 10, Messages: []Message{{ID: "1", Author: "codex", Text: "message from the agent", At: "2026-10-18T00:00:00Z"}}}
		s.Tree.Nodes = append([]Node{history}, s.Tree.Nodes...)
		return s
	}
	for _, step := range []struct {
		layout  string
		history bool
	}{{"transcript", false}, {"", true}, {"transcript", false}} {
		m.apply(snap(step.layout))
		out := ansi.Strip(m.View().Content)
		if strings.Contains(out, "message from the agent") != step.history {
			t.Fatalf("layout %q: history shown = %v, want %v", step.layout, !step.history, step.history)
		}
		if m.editors["composer"].text() != "unsaved draft" {
			t.Fatalf("layout %q: the draft changed to %q", step.layout, m.editors["composer"].text())
		}
	}
}

// A tall tree that comes after loading trees starts at its top. The second loading tree shows the
// "Loading the updated view…" notice; the tall tree removes it, so the body gets one row higher in
// the same update.
func TestTreeAfterLoadingStartsAtTheTop(t *testing.T) {
	m := newModel(&viewClient{ViewID: "v1", Plugin: "sp", Route: "/panels/big", Requests: make(chan mutation, 16)})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m.Update(connectionMsg{Ready: true, Text: "Connected"})
	loading := Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/panels/big", Revision: "1", Tree: Tree{Title: "Big", Nodes: []Node{{ID: "l", Type: "loading", Text: "Loading"}}}}
	m.Update(loading)
	m.Update(loading)
	if m.status != loadingViewStatus {
		t.Fatalf("status %q, want %q", m.status, loadingViewStatus)
	}
	nodes := []Node{{ID: "first", Type: "widget", Text: "first"}}
	for i := range 60 {
		nodes = append(nodes, Node{ID: "n" + strconv.Itoa(i), Type: "widget", Text: "filler"})
	}
	m.Update(Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/panels/big", Revision: "2", Tree: Tree{Title: "Big", Nodes: nodes}})
	if len(m.bodyRows()) <= m.bodyHeight() {
		t.Fatalf("the tree has %d rows, want more than %d", len(m.bodyRows()), m.bodyHeight())
	}
	if m.scroll != 0 {
		t.Fatalf("the tree starts at row %d of %d, want 0", m.scroll, len(m.bodyRows()))
	}
}
