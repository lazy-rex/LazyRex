package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func modeModel(height int) *model {
	m := newModel(&viewClient{ViewID: "v1", Plugin: "messaging", Route: "/thread/42", Requests: make(chan mutation, 16)})
	m.ready = true
	m.Update(tea.WindowSizeMsg{Width: 80, Height: height})
	return m
}

func treeOf(nodes ...Node) Snapshot {
	return Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/thread/42", Revision: "1", Tree: Tree{Title: "Thread", Nodes: nodes}}
}

var (
	tvHistory  = Node{ID: "history", Type: "history", PageSize: 10, Messages: []Message{{ID: "m1", Author: "codex", Text: "hello"}}}
	tvComposer = Node{ID: "composer", Type: "composer", SaveAction: &Action{Name: "save", Enabled: true}, SendAction: &Action{Name: "send", Enabled: true}}
	tvMenu     = Node{ID: "menu", Type: "menu", Label: "Thread Settings…"}
	tvList     = Node{ID: "list", Type: "list", Items: []Item{{ID: "a", Label: "A"}}}
	tvForm     = Node{ID: "form", Type: "form", Fields: []Field{{ID: "f", Kind: "text", Label: "Name"}}}
)

func tvError(id, text string, retry bool) Node {
	n := Node{ID: id, Type: "error", Text: text}
	if retry {
		n.RetryAction = &Action{Name: "retry-" + id, Label: "Retry " + id, Enabled: true}
	}
	return n
}

func tvAction(id string) Node {
	return Node{ID: id, Type: "widget", Action: &Action{Name: "do-" + id, Label: "Action " + id, Enabled: true}}
}

func TestDecideViewMode(t *testing.T) {
	paired := treeOf(tvHistory, tvComposer)
	paired.Layout = "transcript"
	cases := []struct {
		name string
		tree Snapshot
		mode viewMode
		note string
	}{
		{"empty tree", treeOf(), viewNeutral, ""},
		{"loading", treeOf(Node{ID: "l", Type: "loading", Text: "Loading"}), viewNeutral, ""},
		{"error with an action", treeOf(tvError("e", "down", true)), viewNeutral, ""},
		{"thread", treeOf(tvHistory, tvComposer, tvMenu), viewThread, ""},
		{"menu before history", treeOf(tvMenu, tvHistory, tvComposer), viewThread, ""},
		{"widget beside history", treeOf(tvAction("w"), tvHistory, tvError("e", "x", false)), viewThread, ""},
		{"read-only thread", treeOf(Node{ID: "notice", Type: "widget", Text: "Observer"}, tvHistory), viewThread, ""},
		{"paired transcript layout", paired, viewAlt, ""},
		{"Gallery: list, form, history, composer", treeOf(tvList, tvForm, tvHistory, tvComposer), viewAlt, noteHistoryBesideListForm},
		{"form beside history", treeOf(tvHistory, tvForm), viewAlt, noteHistoryBesideListForm},
		{"list", treeOf(tvList), viewAlt, ""},
		{"form", treeOf(tvForm), viewAlt, ""},
		{"menu without history", treeOf(tvMenu), viewAlt, ""},
		{"composer without history", treeOf(tvComposer), viewAlt, ""},
	}
	for _, c := range cases {
		mode, note := decideViewMode(c.tree)
		if mode != c.mode || note != c.note {
			t.Errorf("%s: mode=%d note=%q, want mode=%d note=%q", c.name, mode, note, c.mode, c.note)
		}
	}
}

// TestModeIsDecidedOnce: the first deciding tree sets the mode for the life of the process.
func TestModeIsDecidedOnce(t *testing.T) {
	m := modeModel(30)
	m.apply(treeOf(tvError("e", "The hub is not ready", true)))
	if m.mode != viewNeutral {
		t.Fatalf("an error tree decided mode %d", m.mode)
	}
	m.apply(treeOf(tvHistory, tvComposer))
	m.apply(treeOf(tvError("e", "Lost the thread", true)))
	m.apply(treeOf(tvList))
	if m.mode != viewThread {
		t.Fatalf("mode=%d after error, history, error, list; want thread", m.mode)
	}
	if v := m.View(); v.AltScreen || v.MouseMode != tea.MouseModeNone {
		t.Fatalf("thread frame: alt=%t mouse=%v", v.AltScreen, v.MouseMode)
	}

	m = modeModel(30)
	m.apply(treeOf(tvList))
	m.apply(treeOf(tvHistory, tvComposer))
	if m.mode != viewAlt || m.status != noteHistoryInAlt {
		t.Fatalf("list then history: mode=%d status=%q", m.mode, m.status)
	}
	m.status = "Done"
	m.apply(treeOf(tvHistory, tvComposer))
	if m.status != "Done" {
		t.Fatalf("the defect note came back: %q", m.status)
	}
}

func TestGalleryTreeDecidesAltWithItsNote(t *testing.T) {
	m := modeModel(30)
	gallery := treeOf(tvList, tvForm, tvHistory, tvComposer)
	m.apply(gallery)
	if m.mode != viewAlt || m.status != noteHistoryBesideListForm {
		t.Fatalf("mode=%d status=%q", m.mode, m.status)
	}
	m.status = "Draft saved"
	m.apply(gallery)
	if m.status != "Draft saved" {
		t.Fatalf("the note was shown twice: %q", m.status)
	}
	if v := m.View(); !v.AltScreen || !strings.Contains(v.Content, "hello") {
		t.Fatalf("the Gallery is not drawn as today's alt view: alt=%t", v.AltScreen)
	}
}

func TestPairedTreeDecidesAltWithoutNote(t *testing.T) {
	m := modeModel(30)
	paired := treeOf(tvHistory, tvComposer)
	paired.Layout = "transcript"
	m.apply(paired)
	m.apply(paired)
	if m.mode != viewAlt || strings.HasPrefix(m.status, "view defect") {
		t.Fatalf("mode=%d status=%q", m.mode, m.status)
	}
	if v := m.View(); !v.AltScreen || strings.Contains(v.Content, "hello") {
		t.Fatal("the paired composer is not today's alt composer")
	}
}

func TestFirstFrameIsInline(t *testing.T) {
	m := modeModel(30)
	v := m.View()
	if v.AltScreen || v.MouseMode != tea.MouseModeNone || !strings.Contains(ansi.Strip(v.Content), "Connecting to the hub…") {
		t.Fatalf("first frame: alt=%t mouse=%v content=%q", v.AltScreen, v.MouseMode, ansi.Strip(v.Content))
	}
}

// TestFormRouteIsNeutralUntilItsTree: no route decides the mode (lead-tv-c2rules, A3).
func TestFormRouteIsNeutralUntilItsTree(t *testing.T) {
	m := newModel(&viewClient{ViewID: "v1", Route: "/forms/thread-menu", Requests: make(chan mutation, 16)})
	m.themeReady = true
	m.apply(Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/forms/thread-menu", Revision: "1", Tree: Tree{Nodes: []Node{tvError("e", "No thread", true)}}})
	if v := m.View(); m.mode != viewNeutral || v.AltScreen {
		t.Fatalf("an error-only form tree: mode=%d alt=%t", m.mode, v.AltScreen)
	}
	m.apply(Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/forms/thread-menu", Revision: "2", Tree: Tree{Nodes: []Node{tvForm}}})
	if m.mode != viewAlt {
		t.Fatalf("the form tree did not decide alt: %d", m.mode)
	}
}

// TestAltWaitsForAnEmptyInlineFrame: after an inline frame, the decision to alt draws one empty
// inline frame and starts the timer; altClear then turns the alt screen on.
func TestAltWaitsForAnEmptyInlineFrame(t *testing.T) {
	m := modeModel(30)
	m.View() // "Connecting…" is drawn inline
	_, cmd := m.Update(treeOf(tvList))
	if !m.altPending || cmd == nil {
		t.Fatalf("pending=%t cmd=%v", m.altPending, cmd != nil)
	}
	if v := m.View(); v.AltScreen || v.Content != "" {
		t.Fatalf("the frame before alt-on: alt=%t content=%q", v.AltScreen, v.Content)
	}
	m.Update(altClear{})
	if v := m.View(); !v.AltScreen {
		t.Fatal("altClear did not turn the alt screen on")
	}
}

func TestQuitDrawsAnEmptyFrame(t *testing.T) {
	m := modeModel(30)
	m.apply(treeOf(tvHistory, tvComposer))
	if v := m.View(); v.Content == "" {
		t.Fatal("the thread frame is empty")
	}
	m.quit()
	if v := m.View(); v.Content != "" || v.AltScreen {
		t.Fatalf("the last frame: alt=%t content=%q", v.AltScreen, v.Content)
	}
}

// TestReplacementStaysInline: a thread pane keeps the primary screen while a reopen replaces it.
func TestReplacementStaysInline(t *testing.T) {
	m := modeModel(30)
	m.apply(treeOf(tvHistory, tvComposer))
	m.replacement = &panelReplacement{ID: "r1"}
	if v := m.View(); v.AltScreen || v.MouseMode != tea.MouseModeNone {
		t.Fatalf("replacement frame in thread mode: alt=%t mouse=%v", v.AltScreen, v.MouseMode)
	}
}

// TestThreadLiveRegionIsBounded: beside a history node, the composer and the other nodes get at
// most threadLiveRows rows; the status line comes after them. No history row is drawn.
func TestThreadLiveRegionIsBounded(t *testing.T) {
	m := modeModel(40)
	m.apply(treeOf(Node{ID: "n1", Type: "widget", Text: "First notice\nwith two lines"}, tvHistory, Node{ID: "n2", Type: "widget", Text: "Second notice"}, tvComposer, tvMenu))
	m.status = "Connected" // no notice: the status line shows the key hints
	rows := m.liveRows()
	if len(rows) > threadLiveRows+1 {
		t.Fatalf("%d live rows, want at most %d", len(rows), threadLiveRows+1)
	}
	text := ansi.Strip(m.View().Content)
	if strings.Contains(text, "hello") {
		t.Fatal("a history message is drawn in the live region")
	}
	if !strings.Contains(text, "Ctrl+O Thread Settings…") {
		t.Fatalf("the status line has no menu hint: %q", text)
	}
	send := false
	for _, r := range rows {
		for _, c := range r.rowControls() {
			send = send || c.ID == "edit:composer"
		}
	}
	if !send {
		t.Fatal("the composer is not in the live region")
	}
}

var moreLines = regexp.MustCompile(`… (\d+) more lines`)

// TestCutKeepsEveryAction (v1.2 test iv): in thread mode a long error tree with actions is cut;
// only text rows go, each cut ends in "… <n> more lines", and Tab reaches every action.
func TestCutKeepsEveryAction(t *testing.T) {
	m := modeModel(12)
	m.apply(treeOf(tvHistory, tvComposer))
	long := strings.TrimSuffix(strings.Repeat("The hub lost the thread.\n", 30), "\n")
	m.apply(treeOf(tvError("e1", long, true), tvAction("w1"), tvError("e2", long, true)))
	full := trimBlank(m.bodyRows())
	rows := m.liveRows()
	if len(rows) > m.height-1 {
		t.Fatalf("%d rows in a pane of %d", len(rows), m.height)
	}
	cut, marked := 0, 0
	for _, r := range rows {
		if match := moreLines.FindStringSubmatch(ansi.Strip(r.Text)); match != nil {
			n, _ := strconv.Atoi(match[1])
			cut += n
			marked++
		}
	}
	// The status line is the last row; every other row is a kept row or a marker.
	if marked == 0 || cut+len(rows)-1-marked != len(full) {
		t.Fatalf("cut %d rows with %d markers; kept %d of %d", cut, marked, len(rows)-1-marked, len(full))
	}
	want := m.controls()
	visible := map[string]bool{}
	for _, r := range rows {
		for _, c := range r.rowControls() {
			visible[c.ID] = true
		}
	}
	for _, c := range want {
		if !visible[c.ID] {
			t.Fatalf("action %s is not in the live region", c.ID)
		}
	}
	reached := map[string]bool{}
	for range want {
		m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		reached[m.focus] = true
	}
	if len(reached) != len(want) {
		t.Fatalf("Tab reached %d of %d actions", len(reached), len(want))
	}
}

var moreAbove, moreBelow = regexp.MustCompile(`… (\d+) more above`), regexp.MustCompile(`… (\d+) more below`)

// TestFocusWindowFollowsTheFocus (lead-tv-c2rules, B9): when the controls do not fit, the live
// region is a window that holds the focused control, with edge rows; a resize keeps it there.
func TestFocusWindowFollowsTheFocus(t *testing.T) {
	m := modeModel(10)
	m.apply(treeOf(tvHistory, tvComposer))
	nodes := []Node{tvError("e", "The hub lost the thread.", false)}
	for i := 1; i <= 12; i++ {
		nodes = append(nodes, tvAction(fmt.Sprintf("w%02d", i)))
	}
	m.apply(treeOf(nodes...))
	controls := m.controls()
	if len(controls) != 12 {
		t.Fatalf("%d controls", len(controls))
	}
	check := func(step string) {
		t.Helper()
		v := ansi.Strip(m.View().Content)
		lines := strings.Split(v, "\n")
		if len(lines) > m.height-1 {
			t.Fatalf("%s: %d rows in a pane of %d", step, len(lines), m.height)
		}
		label := ""
		for _, c := range controls {
			if c.ID == m.focus {
				label = c.Action.Label
			}
		}
		if label == "" || !strings.Contains(v, label) {
			t.Fatalf("%s: focused %q is not on screen:\n%s", step, m.focus, v)
		}
		above, below := 0, 0
		if match := moreAbove.FindStringSubmatch(v); match != nil {
			above, _ = strconv.Atoi(match[1])
		}
		if match := moreBelow.FindStringSubmatch(v); match != nil {
			below, _ = strconv.Atoi(match[1])
		}
		total := len(cutText(trimBlank(m.bodyRows()), m.height-2))
		if above != m.liveOffset || above+below+(m.height-2-2) != total {
			t.Fatalf("%s: above=%d below=%d offset=%d rows=%d", step, above, below, m.liveOffset, total)
		}
	}
	m.setFocus(controls[0].ID)
	check("first")
	for i := 1; i < len(controls); i++ {
		m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		if m.focus != controls[i].ID {
			t.Fatalf("Tab %d: focus %q, want %q", i, m.focus, controls[i].ID)
		}
		check(fmt.Sprintf("Tab %d", i))
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 8})
	check("resize with the last control focused")
	for i := len(controls) - 2; i >= 0; i-- {
		m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
		if m.focus != controls[i].ID {
			t.Fatalf("Shift+Tab to %d: focus %q, want %q", i, m.focus, controls[i].ID)
		}
		check(fmt.Sprintf("Shift+Tab %d", i))
	}
}

// TestAltClearBytes runs a real tea.Program (Bubble Tea v2.1.0) on a test output: an error tree is
// drawn inline while neutral, then a list tree decides alt. In this run's bytes the inline rows are
// erased before CSI ?1049h. This proves the order of the tested run only: the clear is best effort
// (altClearDelay, view/README.md "Known limit").
func TestAltClearBytes(t *testing.T) {
	out := newProtoOut(nil)
	m := newModel(&viewClient{ViewID: "v1", Route: "/thread/42", Requests: make(chan mutation, 16)})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(out), tea.WithWindowSize(80, 20), tea.WithoutSignals())
	go func() {
		p.Send(treeOf(tvError("e", "NEUTRAL-ERROR", false)))
		time.Sleep(200 * time.Millisecond)
		p.Send(treeOf(tvList))
		time.Sleep(300 * time.Millisecond)
		p.Quit()
	}()
	if _, err := p.Run(); ctx.Err() != nil {
		t.Fatalf("the program did not end: %v", err)
	}
	raw := out.String()
	t.Logf("raw output %q", raw)
	text, on := strings.LastIndex(raw, "NEUTRAL-ERROR"), strings.Index(raw, "\x1b[?1049h")
	if text < 0 || on < text {
		t.Fatalf("the error text is at %d and the alt screen starts at %d", text, on)
	}
	if between := raw[text:on]; !strings.Contains(between, "\x1b[J") && !strings.Contains(between, "\x1b[2K") && !strings.Contains(between, "\x1b[K") {
		t.Fatalf("no erase between the inline error and alt-on: %q", between)
	}
}

// codex-29's probes for bbdb3152 (c29-bbdb3152-probe-bodies), unchanged except gofmt.
func TestReviewThreadSmallPaneWithControls(t *testing.T) {
	m := modeModel(6)
	m.apply(treeOf(tvHistory, tvAction("w1"), tvAction("w2"), tvComposer))
	v := m.View()
	if n := len(strings.Split(v.Content, "\n")); n > m.height {
		t.Fatalf("thread live frame has %d rows in a %d-row pane: %q", n, m.height, v.Content)
	}
}
func TestReviewReplacementQuitClears(t *testing.T) {
	m := modeModel(20)
	m.apply(treeOf(tvHistory, tvComposer))
	m.replacement = &panelReplacement{ID: "r1", replacementState: replacementState{State: "opening", NextTitle: "Next"}}
	m.Update(closeFinished{})
	if !m.quitting {
		t.Fatal("closeFinished did not request quit")
	}
	if v := m.View(); v.Content != "" {
		t.Fatalf("replacement frame survives successful close: %q", v.Content)
	}
}
func TestReviewInlineCtrlTDoesNothing(t *testing.T) {
	m := modeModel(20)
	m.apply(treeOf(tvHistory, tvComposer))
	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if m.selectText {
		t.Fatal("Ctrl+T changed text selection state in thread mode")
	}
}

// TestQuitFrameKeepsTheScreen: the last frame is empty on the screen that is active. The wait
// before alt-on is still the primary screen.
func TestQuitFrameKeepsTheScreen(t *testing.T) {
	m := modeModel(30)
	m.apply(treeOf(tvList))
	m.View()
	m.quit()
	if v := m.View(); v.Content != "" || !v.AltScreen {
		t.Fatalf("alt mode: the last frame: alt=%t content=%q", v.AltScreen, v.Content)
	}
	m = modeModel(30)
	m.apply(treeOf(Node{ID: "l", Type: "loading", Text: "Loading"}))
	m.View()
	m.apply(treeOf(tvList))
	if !m.altPending {
		t.Fatal("no wait before alt-on")
	}
	m.quit()
	if v := m.View(); v.Content != "" || v.AltScreen {
		t.Fatalf("the wait before alt-on: the last frame: alt=%t content=%q", v.AltScreen, v.Content)
	}
}

// TestShortThreadPaneKeepsEditorAndSend (v1.2 item 1f): in a 6-row pane, a thread tree with two
// actions and a composer with a draft is at most 5 rows. Tab and Shift+Tab reach every control,
// the focused control is on screen, and the editor row and Send stay together. A resize keeps the
// focus.
func TestShortThreadPaneKeepsEditorAndSend(t *testing.T) {
	m := modeModel(30)
	m.apply(treeOf(tvHistory, tvAction("w1"), tvAction("w2"), tvComposer))
	m.editors["composer"].Area.SetValue("DRAFT-TEXT")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 6})
	controls := m.controls()
	ids := make([]string, len(controls))
	for i, c := range controls {
		ids[i] = c.ID
	}
	if strings.Join(ids, " ") != "w1:action w2:action edit:composer composer:send" {
		t.Fatalf("controls %q", ids)
	}
	check := func(step string) {
		t.Helper()
		v := ansi.Strip(m.View().Content)
		if n := len(strings.Split(v, "\n")); n > m.height-1 {
			t.Fatalf("%s: %d rows in a pane of %d:\n%s", step, n, m.height, v)
		}
		want := map[string][]string{"w1:action": {"Action w1"}, "w2:action": {"Action w2"}, "edit:composer": {"DRAFT-TEXT", "Send"}, "composer:send": {"DRAFT-TEXT", "Send"}}[m.focus]
		for _, s := range want {
			if !strings.Contains(v, s) {
				t.Fatalf("%s: focus %q, %q is not on screen:\n%s", step, m.focus, s, v)
			}
		}
	}
	m.setFocus(ids[0])
	check("first")
	for i := 1; i < len(ids); i++ {
		m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		if m.focus != ids[i] {
			t.Fatalf("Tab %d: focus %q, want %q", i, m.focus, ids[i])
		}
		check(fmt.Sprintf("Tab %d", i))
	}
	for i := len(ids) - 2; i >= 0; i-- {
		m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
		if m.focus != ids[i] {
			t.Fatalf("Shift+Tab to %d: focus %q, want %q", i, m.focus, ids[i])
		}
		check(fmt.Sprintf("Shift+Tab %d", i))
	}
	// The focus wraps: the window jumps from the first action to Send, and back.
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.focus != "composer:send" {
		t.Fatalf("Shift+Tab from the first control: focus %q", m.focus)
	}
	check("Shift+Tab wraps to Send")
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.focus != ids[0] {
		t.Fatalf("Tab from Send: focus %q", m.focus)
	}
	check("Tab wraps to the first control")
	m.setFocus("edit:composer")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	check("tall pane")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 6})
	if m.focus != "edit:composer" {
		t.Fatalf("the resize moved the focus to %q", m.focus)
	}
	check("resize from 30 to 6 rows")
}

// TestInlineIgnoresTheMouse: an inline frame has no mouse reporting, so a mouse event changes
// nothing. In alt mode a click still focuses a control and Ctrl+T still turns text selection on.
func TestInlineIgnoresTheMouse(t *testing.T) {
	m := modeModel(30)
	m.apply(treeOf(tvHistory, tvAction("w1"), tvComposer))
	m.View()
	at := func() (int, int) {
		for y, r := range m.visibleRows {
			for _, c := range r.rowControls() {
				if c.ID == "w1:action" {
					return m.leftMargin() + c.Left, y
				}
			}
		}
		t.Fatal("w1 is not on screen")
		return 0, 0
	}
	x, y := at()
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: x, Y: y})
	if m.pressed != "" || m.focus == "w1:action" || m.pointerKnown {
		t.Fatalf("inline: pressed=%q focus=%q pointer=%t", m.pressed, m.focus, m.pointerKnown)
	}

	m = modeModel(30)
	m.apply(treeOf(tvAction("w1"), tvComposer))
	m.View()
	x, y = at()
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if m.pressed != "w1:action" || m.focus != "w1:action" {
		t.Fatalf("alt: pressed=%q focus=%q", m.pressed, m.focus)
	}
	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if !m.selectText {
		t.Fatal("alt: Ctrl+T did not turn text selection on")
	}
}
