package main

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"image/color"
	"strings"
	"testing"
)

func selectModel() *model {
	m := testModel()
	s := selectSnapshot("old", "Alpha", "beta", "Gamma", "Delta", "Epsilon", "Zeta", "Eta", "Theta", "Iota", "Kappa")
	s.Route = "/forms/start"
	m.apply(s)
	m.setFocus("choice:stop/worker")
	return m
}
func selectKey(m *model, code rune) { m.Update(tea.KeyPressMsg{Code: code}) }

func TestSelectOpenFilterPickAndEscape(t *testing.T) {
	m := selectModel()
	e := m.editors["stop/worker"]
	selectKey(m, tea.KeyEnter)
	if m.selectList == nil || e.Choice != "old" {
		t.Fatal("Opening changed the value")
	}
	selectKey(m, tea.KeyRight)
	if e.Choice != "old" {
		t.Fatal("Right cycled an open select")
	}
	m.Update(tea.KeyPressMsg{Code: 'a', Text: "ALP"})
	if opts := m.selectOptions(); len(opts) != 1 || opts[0].Value != "Alpha" {
		t.Fatalf("Case-insensitive filter: %v", opts)
	}
	selectKey(m, tea.KeyEscape)
	if m.selectList == nil || m.selectList.Filter != "" {
		t.Fatal("First Escape did not clear the filter")
	}
	selectKey(m, tea.KeyDown)
	selectKey(m, tea.KeyEscape)
	if m.selectList != nil || e.Choice != "old" || m.closePending {
		t.Fatal("Escape changed the field or closed the form")
	}
	selectKey(m, ' ')
	m.Update(tea.KeyPressMsg{Code: 'b', Text: "beta"})
	selectKey(m, tea.KeyBackspace)
	if m.selectList.Filter != "bet" {
		t.Fatal("Backspace did not edit the filter")
	}
	selectKey(m, tea.KeyEnter)
	if m.selectList != nil || e.Choice != "beta" {
		t.Fatal("Enter did not pick the filtered option")
	}
	selectKey(m, tea.KeyLeft)
	if e.Choice != "Alpha" {
		t.Fatal("Left did not cycle backwards")
	}
	selectKey(m, tea.KeyRight)
	if e.Choice != "beta" {
		t.Fatal("Right did not cycle forwards")
	}
}

func TestSelectWindowWheelAndPointerPick(t *testing.T) {
	m := selectModel()
	m.height = 30
	selectKey(m, tea.KeyEnter)
	selectKey(m, tea.KeyEnd)
	if m.selectList.Highlight != 10 || m.selectList.Offset != 3 {
		t.Fatal("End did not reveal the last option")
	}
	count := 0
	for _, r := range m.selectOverlay().Rows {
		if r.Control != nil && r.Control.Kind == "select_option" {
			count++
		}
	}
	if count != 8 {
		t.Fatalf("Window has %d options", count)
	}
	var y, x int
	for i, r := range m.selectOverlay().Rows {
		if r.Control != nil && r.Control.Kind == "select_option" {
			y = m.selectOverlay().Y + i
			x = m.leftMargin() + 1
			break
		}
	}
	m.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelUp})
	if m.selectList.Highlight != 9 {
		t.Fatal("Wheel did not move the highlight")
	}
	selectKey(m, tea.KeyHome)
	for i, r := range m.selectOverlay().Rows {
		if r.Control == nil || r.Control.Kind != "select_option" || r.Control.Input["value"] != "Gamma" {
			continue
		}
		x = m.leftMargin() + 1
		y = m.selectOverlay().Y + i
		m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
		if m.selectList != nil || m.editors["stop/worker"].Choice != "Gamma" {
			t.Fatal("Pointer pick did not commit")
		}
		return
	}
	t.Fatal("Gamma is not visible")
}

func TestSelectRefreshFocusAndEmpty(t *testing.T) {
	m := selectModel()
	selectKey(m, tea.KeyEnter)
	selectKey(m, tea.KeyEnd)
	m.apply(selectSnapshot("old", "Kappa", "next"))
	if m.selectList == nil || m.selectList.Highlight != 1 {
		t.Fatal("Refresh lost the highlighted value")
	}
	m.apply(selectSnapshot("old", "next"))
	if m.selectList == nil || m.selectList.Highlight >= len(m.selectOptions()) {
		t.Fatal("Removed option left an invalid highlight")
	}
	s := selectSnapshot("old", "next")
	s.Tree.Nodes[0].Fields[0].Value = "next"
	m.apply(s)
	if m.editors["stop/worker"].Choice != "next" {
		t.Fatal("New tree value was lost")
	}
	selectKey(m, tea.KeyTab)
	if m.selectList != nil || m.editors["stop/worker"].Choice != "next" {
		t.Fatal("Focus change committed a highlighted value")
	}
	m.apply(selectSnapshot())
	m.setFocus("choice:stop/worker")
	selectKey(m, tea.KeyEnter)
	if m.selectList != nil {
		t.Fatal("Empty select opened")
	}
}

func TestSelectNoMatchesAndNarrowLabels(t *testing.T) {
	m := selectModel()
	m.width = 20
	m.resizeEditors()
	selectKey(m, tea.KeyEnter)
	m.Update(tea.PasteMsg{Content: "no match"})
	selectKey(m, tea.KeyEnter)
	if m.selectList == nil || m.editors["stop/worker"].Choice != "old" {
		t.Fatal("No-match Enter changed the field")
	}
	selectKey(m, tea.KeyEscape)
	s := selectSnapshot("old", strings.Repeat("long label ", 10))
	m.apply(s)
	selectKey(m, tea.KeyEnd)
	for _, r := range m.selectOverlay().Rows {
		if strings.HasPrefix(r.Anchor, "option:") && ansi.StringWidth(r.Text) > m.selectOverlay().Width {
			t.Fatalf("Wrapped option: %q", r.Text)
		}
	}
	if !strings.Contains(ansi.Strip(fmt.Sprint(m.selectRows("stop", s.Tree.Nodes[0].Fields[0]))), "…") {
		t.Fatal("Long label lacks an ellipsis")
	}
}

func selectSnapshot(options ...string) Snapshot {
	s := testSnapshot("", "1")
	opts := []Option{}
	for _, v := range options {
		opts = append(opts, Option{Value: v, Label: v})
	}
	s.Tree.Nodes = []Node{{ID: "stop", Type: "form", Fields: []Field{{ID: "worker", Kind: "select", Value: "old", Options: opts}}, SubmitAction: &Action{Name: "stop", Enabled: true}}}
	return s
}

func TestSelectRemovesStaleChoice(t *testing.T) {
	m := testModel()
	m.apply(selectSnapshot("old", "next", "last"))
	e := m.editors["stop/worker"]
	e.Choice = "last"
	m.apply(selectSnapshot("next", "last"))
	if e.Choice != "last" {
		t.Fatal("valid user choice changed")
	}
	m.apply(selectSnapshot("next"))
	if e.Choice != "next" || e.SavedChoice != "next" || e.dirty() {
		t.Fatal("removed choice did not reset cleanly")
	}
	m.apply(selectSnapshot())
	if e.Choice != "" || e.SavedChoice != "" {
		t.Fatal("empty options retained a choice")
	}
	m.apply(selectSnapshot("new"))
	if e.Choice != "new" {
		t.Fatal("new options did not select their first value")
	}
}

func TestSelectRefreshPreservesPendingRequest(t *testing.T) {
	m := testModel()
	m.apply(selectSnapshot("old", "next"))
	var submit control
	for _, c := range m.controls() {
		if c.ID == "stop:submit" {
			submit = c
		}
	}
	req := request(t, m, m.activate(submit))
	m.apply(selectSnapshot("next"))
	values := req.Args["input"].(map[string]any)["values"].(map[string]any)
	if values["worker"] != "old" {
		t.Fatal("refresh changed the submitted request")
	}
	m.finish(completed{Request: req, Outcome: operationResult{Status: "applied"}})
	e := m.editors["stop/worker"]
	if e.Choice != "next" || e.dirty() {
		t.Fatal("late completion restored a stale choice")
	}
}

func TestSelectOverlayKeepsFormLayout(t *testing.T) {
	m := selectModel()
	m.height = 30
	m.themeReady = true
	m.revealFocus()
	beforeRows, beforeScroll := m.bodyRows(), m.scroll
	before := strings.Split(ansi.Strip(m.View().Content), "\n")
	selectKey(m, tea.KeyEnter)
	overlay := m.selectOverlay()
	after := strings.Split(ansi.Strip(m.View().Content), "\n")
	if len(beforeRows) != len(m.bodyRows()) || m.scroll != beforeScroll {
		t.Fatal("Opening changed form layout")
	}
	for i, r := range m.bodyRows() {
		if r.Anchor != beforeRows[i].Anchor || r.Text != beforeRows[i].Text {
			t.Fatalf("Form row %d changed", i)
		}
	}
	for i, line := range before {
		if i >= overlay.Y && i < overlay.Y+len(overlay.Rows) {
			continue
		}
		if i >= len(after) || strings.TrimRight(line, " ") != strings.TrimRight(after[i], " ") {
			t.Fatalf("Outside row %d changed: %q => %q", i, line, after)
		}
	}
	m.Update(tea.PasteMsg{Content: "beta"})
	if len(m.bodyRows()) != len(beforeRows) || m.scroll != beforeScroll {
		t.Fatal("Filtering moved the form")
	}
}

func TestSelectOverlayPlacement(t *testing.T) {
	for _, tc := range []struct {
		name            string
		height, padding int
		above           bool
		capacity        int
	}{
		{"below", 30, 0, false, 8}, {"above", 20, 11, true, 8}, {"shrink", 18, 6, false, 4}, {"minimum", 17, 6, false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := selectModel()
			m.height = tc.height
			if tc.padding > 0 {
				m.snapshot.Tree.Nodes = append([]Node{{ID: "padding", Type: "widget", Text: strings.Repeat("line\n", tc.padding-2) + "line"}}, m.snapshot.Tree.Nodes...)
			}
			m.revealFocus()
			selectKey(m, tea.KeyEnter)
			o := m.selectOverlay()
			fieldTop, fieldBottom := -1, -1
			for i, r := range m.screenRows() {
				if r.Control != nil && r.Control.ID == m.focus {
					if fieldTop < 0 {
						fieldTop = i
					}
					fieldBottom = i
				}
			}
			if tc.above && o.Y+len(o.Rows) > fieldTop {
				t.Fatalf("Did not flip above: %+v, field %d", o, fieldTop)
			}
			if !tc.above && o.Y != fieldBottom+1 {
				t.Fatalf("Did not anchor below: y=%d field bottom=%d", o.Y, fieldBottom)
			}
			if m.selectCapacity() != tc.capacity {
				t.Fatalf("Capacity %d, want %d", m.selectCapacity(), tc.capacity)
			}
			if o.Y < 0 || o.Y+len(o.Rows) > m.height {
				t.Fatal("Overlay exceeds pane")
			}
			selectKey(m, tea.KeyEnd)
			found := false
			for _, r := range m.selectOverlay().Rows {
				if r.Control != nil && r.Control.Input["value"] == "Kappa" {
					found = true
				}
			}
			if !found {
				t.Fatal("Last option is outside the reduced window")
			}
		})
	}
}

func TestSelectOverlayMousePriorityAndOutsideClick(t *testing.T) {
	m := selectModel()
	m.height = 30
	// A text field after the select puts the one-row submit button under the option rows.
	s := m.snapshot
	for i := range s.Tree.Nodes {
		if s.Tree.Nodes[i].Type == "form" {
			s.Tree.Nodes[i].Fields = append(s.Tree.Nodes[i].Fields, Field{ID: "note", Kind: "text", Label: "Note"})
		}
	}
	m.apply(s)
	selectKey(m, tea.KeyEnter)
	o := m.selectOverlay()
	// The option covers the submit button. The click must not submit the form.
	targetY := -1
	for i, r := range m.screenRows() {
		if r.Control != nil && r.Control.Kind == "submit" && i >= o.Y+2 && i < o.Y+len(o.Rows)-2 {
			targetY = i
			break
		}
	}
	if targetY < 0 {
		t.Fatal("Fixture has no covered submit button")
	}
	value := o.Rows[targetY-o.Y].Control.Input["value"]
	m.Update(tea.MouseClickMsg{X: o.X + 2, Y: targetY, Button: tea.MouseLeft})
	_, cmd := m.Update(tea.MouseReleaseMsg{X: o.X + 2, Y: targetY, Button: tea.MouseLeft})
	if cmd != nil || len(m.operations) != 0 || m.editors["stop/worker"].Choice != value {
		t.Fatal("Click reached the covered submit button")
	}
	selectKey(m, tea.KeyEnter)
	selectKey(m, tea.KeyDown)
	m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	if m.selectList != nil || m.editors["stop/worker"].Choice != value {
		t.Fatal("Outside click changed the value")
	}
	// The first click on the field closes the list. It must not reopen it on release.
	selectKey(m, tea.KeyEnter)
	y := 0
	for i, r := range m.screenRows() {
		if r.Control != nil && r.Control.ID == m.focus {
			y = i
			break
		}
	}
	m.Update(tea.MouseClickMsg{X: m.leftMargin() + 2, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: m.leftMargin() + 2, Y: y, Button: tea.MouseLeft})
	if m.selectList != nil {
		t.Fatal("Outside click passed through to the field")
	}
	for _, msg := range []tea.Msg{tea.MouseWheelMsg{X: 0, Y: 0, Button: tea.MouseWheelDown}, tea.KeyPressMsg{Code: tea.KeyPgDown}} {
		selectKey(m, tea.KeyEnter)
		m.Update(msg)
		if m.selectList != nil {
			t.Fatal("Form scrolling left the list open")
		}
	}
}

func TestSelectResizeBeforeOpeningDoesNotScrollOnOpen(t *testing.T) {
	m := selectModel()
	m.snapshot.Tree.Nodes = append(m.snapshot.Tree.Nodes, Node{ID: "padding", Type: "widget", Text: strings.Repeat("line\n", 20)})
	m.height = 40
	m.revealFocus()
	m.Update(tea.WindowSizeMsg{Width: 32, Height: 20})
	before := m.scroll
	selectKey(m, tea.KeyEnter)
	if m.selectOverlay() == nil || m.scroll != before {
		t.Fatal("Opening after resize scrolled the form")
	}
	selectKey(m, tea.KeyEnd)
	selectKey(m, tea.KeyEscape)
	if m.scroll != before {
		t.Fatal("Moving or closing scrolled the form")
	}
}

func TestSelectHintsOnlyDescribeHiddenOptions(t *testing.T) {
	m := selectModel()
	m.height = 30
	selectKey(m, tea.KeyEnter)
	text := func() string { return ansi.Strip(m.selectOverlay().layer().GetContent()) }
	if !strings.Contains(text(), "↓ 3") || strings.Contains(text(), "more above") {
		t.Fatal(text())
	}
	m.positionSelect(8)
	if !strings.Contains(text(), "↑ 1 · ↓ 2") {
		t.Fatal(text())
	}
	selectKey(m, tea.KeyEnd)
	if !strings.Contains(text(), "↑ 3") || strings.Contains(text(), "more below") {
		t.Fatal(text())
	}
	m.Update(tea.PasteMsg{Content: "beta"})
	if strings.Contains(text(), "more") || len(m.selectOverlay().Rows) != 5 {
		t.Fatal("Single option kept a count row: " + text())
	}
	m.Update(tea.PasteMsg{Content: "no match"})
	if strings.Contains(text(), "more") || len(m.selectOverlay().Rows) != 5 {
		t.Fatal("Empty result kept a count row: " + text())
	}
	m.apply(selectSnapshot("old", "next"))
	m.selectList.Filter = ""
	m.positionSelect(0)
	if len(m.selectOverlay().Rows) != 6 {
		t.Fatal("Two options need six total rows")
	}
	// Six rows fit below this field. An unused count row would force an upward flip.
	m.height = 10
	m.snapshot.Tree.Nodes = append([]Node{{ID: "padding", Type: "widget", Text: "line"}}, m.snapshot.Tree.Nodes...)
	m.revealFocus()
	o := m.selectOverlay()
	last := -1
	for i, r := range m.screenRows() {
		if r.Control != nil && r.Control.ID == m.focus {
			last = i
		}
	}
	// Use a pane with exactly six rows below the field.
	m.height = last + 1 + 6
	o = m.selectOverlay()
	if o.Y != last+1 || o.Y+len(o.Rows) != m.height {
		t.Fatalf("The absent hint still uses space: y=%d rows=%d height=%d", o.Y, len(o.Rows), m.height)
	}
}

func TestSelectPaddingAndContentWidth(t *testing.T) {
	m := selectModel()
	m.height = 30
	m.width = 160
	selectKey(m, tea.KeyEnter)
	o := m.selectOverlay()
	fieldWidth := 0
	for _, r := range m.screenRows() {
		if r.Control != nil && r.Control.ID == m.focus {
			fieldWidth = r.Control.HitWidth
		}
	}
	widest := 0
	for _, option := range m.selectField("stop", "worker").Options {
		widest = max(widest, ansi.StringWidth(option.Label)+2)
	}
	if o.Width != max(fieldWidth, widest)+6 || o.X != m.leftMargin() {
		t.Fatalf("Wrong content width or anchor: %+v", o)
	}
	if o.Width >= m.controlWidth() {
		t.Fatal("Short labels still fill the form width")
	}
	if strings.Contains(ansi.Strip(o.layer().GetContent()), "Filter:") {
		t.Fatal("An empty filter has a label")
	}
	if strings.Trim(ansi.Strip(o.Rows[1].Text), "│ ") != "" {
		t.Fatal("The top padding row is not blank")
	}
	lastOption := 0
	for i, r := range o.Rows {
		if r.Control == nil {
			continue
		}
		lastOption = i
		plain := ansi.Strip(r.Text)
		if !strings.HasPrefix(plain, "│  ") || !strings.HasSuffix(plain, "  │") {
			t.Fatalf("Missing horizontal padding: %q", plain)
		}
	}
	if strings.Trim(ansi.Strip(o.Rows[lastOption+1].Text), "│ ") != "" {
		t.Fatal("The bottom padding row is not blank")
	}
	canvas := lipgloss.NewCanvas(o.Width, len(o.Rows))
	canvas.Compose(lipgloss.NewLayer(o.layer().GetContent()))
	sameColor := func(a, b color.Color) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		ar, ag, ab, aa := a.RGBA()
		br, bg, bb, ba := b.RGBA()
		return ar == br && ag == bg && ab == bb && aa == ba
	}
	for x := 1; x < o.Width-1; x++ {
		if !sameColor(canvas.CellAt(x, 2).Style.Bg, m.palette().accent) {
			t.Fatalf("Highlight misses inner cell %d", x)
		}
	}
	for _, x := range []int{0, o.Width - 1} {
		if sameColor(canvas.CellAt(x, 2).Style.Bg, m.palette().accent) {
			t.Fatal("Highlight paints the border")
		}
	}
	m.apply(selectSnapshot("old", strings.Repeat("界", 50)))
	m.width = 24
	m.revealFocus()
	o = m.selectOverlay()
	if o.Width != m.width-o.X {
		t.Fatal("Long labels do not cap at the pane edge")
	}
	if !strings.Contains(ansi.Strip(o.layer().GetContent()), "…") {
		t.Fatal("Long label is not truncated")
	}
	for _, r := range o.Rows {
		if ansi.StringWidth(r.Text) != o.Width {
			t.Fatalf("Option wrapped: %q", r.Text)
		}
	}
}

func TestSelectFilterUsesPaddingWithoutMovingOrigin(t *testing.T) {
	for _, above := range []bool{false, true} {
		m := selectModel()
		m.height = 30
		if above {
			m.height = 20
			m.snapshot.Tree.Nodes = append([]Node{{ID: "pad", Type: "widget", Text: strings.Repeat("line\n", 10)}}, m.snapshot.Tree.Nodes...)
		}
		m.revealFocus()
		selectKey(m, tea.KeyEnter)
		initial := m.selectOverlay()
		m.Update(tea.PasteMsg{Content: "beta"})
		filtered := m.selectOverlay()
		if initial.X != filtered.X || initial.Y != filtered.Y || initial.Width != filtered.Width {
			t.Fatal("Typing moved or resized the filter origin")
		}
		if !strings.Contains(ansi.Strip(filtered.Rows[1].Text), "Filter:") {
			t.Fatal("Filter did not replace top padding")
		}
		selectKey(m, tea.KeyEscape)
		cleared := m.selectOverlay()
		if initial.Y != cleared.Y || strings.Contains(ansi.Strip(cleared.layer().GetContent()), "Filter:") {
			t.Fatal("Clearing moved the list or left the filter label")
		}
	}
}

func TestSelectHoverMovesCandidateAndReusesUnchangedFrame(t *testing.T) {
	m := selectModel()
	m.themeReady = true
	m.height = 30
	selectKey(m, tea.KeyEnter)
	m.View()
	o := m.visibleSelect
	y := o.Y + 3 // Alpha, after the border, padding row, and current option.
	original := m.editors["stop/worker"].Choice
	m.Update(tea.MouseMotionMsg{X: o.X + 2, Y: y})
	if m.selectList.Value != "Alpha" || m.editors["stop/worker"].Choice != original || m.reuseView {
		t.Fatal("Hover did not move only the candidate")
	}
	frame := m.View()
	for _, point := range [][2]int{{o.X + 3, y}, {o.X + 4, y}, {0, 0}, {o.X, o.Y}} {
		m.Update(tea.MouseMotionMsg{X: point[0], Y: point[1]})
		if !m.reuseView || m.selectList.Value != "Alpha" || m.View().Content != frame.Content {
			t.Fatal("Unchanged candidate rendered a new frame")
		}
	}
	selectKey(m, tea.KeyDown)
	if m.selectList.Value != "beta" {
		t.Fatal("Keyboard did not continue from the hover candidate")
	}
	// A motion event before the next paint must not restore a stale frame.
	m.Update(tea.MouseMotionMsg{X: 0, Y: 0})
	if m.reuseView {
		t.Fatal("Motion reused the frame from before the key")
	}
	m.View()
	// Filtering before another paint must not use an old row index.
	m.Update(tea.PasteMsg{Content: "Gamma"})
	m.Update(tea.MouseMotionMsg{X: o.X + 2, Y: o.Y + 7})
	selectKey(m, tea.KeyEnter)
	if m.editors["stop/worker"].Choice != "Gamma" {
		t.Fatal("Pointer used a stale option index")
	}
}

// The hover candidate stays after the pointer leaves the list, and Enter picks it (the old
// select-lab.mjs: hover Agent 04, move outside, Enter selects Agent 04).
func TestSelectEnterPicksHoverCandidateAfterPointerLeaves(t *testing.T) {
	m := selectModel()
	m.themeReady = true
	m.height = 30
	selectKey(m, tea.KeyEnter)
	m.View()
	o := m.visibleSelect
	original := m.editors["stop/worker"].Choice
	m.Update(tea.MouseMotionMsg{X: o.X + 2, Y: o.Y + 3}) // Alpha
	m.View()
	m.Update(tea.MouseMotionMsg{X: 0, Y: 0}) // outside the overlay
	m.View()
	if original == "Alpha" || m.editors["stop/worker"].Choice != original {
		t.Fatalf("hover changed the value before Enter: %q", m.editors["stop/worker"].Choice)
	}
	selectKey(m, tea.KeyEnter)
	if got := m.editors["stop/worker"].Choice; got != "Alpha" {
		t.Fatalf("Enter after the pointer left picked %q, want Alpha", got)
	}
}

func BenchmarkSelectUnchangedMotionFrame(b *testing.B) {
	m := selectModel()
	m.themeReady = true
	m.height = 30
	selectKey(m, tea.KeyEnter)
	m.View()
	o := m.visibleSelect
	m.Update(tea.MouseMotionMsg{X: o.X + 2, Y: o.Y + 3})
	m.View()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Update(tea.MouseMotionMsg{X: o.X + 2 + i%3, Y: o.Y + 3})
		m.View()
	}
}

func TestSelectOpeningRevealsCurrentValue(t *testing.T) {
	m := selectModel()
	m.height = 18
	m.editors["stop/worker"].Choice = "Kappa"
	selectKey(m, tea.KeyEnter)
	if m.selectList.Value != "Kappa" {
		t.Fatal("Opening did not highlight the current value")
	}
	found := false
	for _, r := range m.selectOverlay().Rows {
		if r.Control != nil && r.Control.Input["value"] == "Kappa" {
			found = strings.Contains(ansi.Strip(r.Text), "✓")
		}
	}
	if !found {
		t.Fatal("Opening hid the current value or its check mark")
	}
}
