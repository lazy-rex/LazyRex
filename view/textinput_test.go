package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// flowModel is the start form with a ready hub, at a width.
func flowModel(width int) *model {
	m := testModel()
	m.width = width
	m.apply(flowSnapshot())
	m.resizeEditors()
	return m
}

// inputRow is the screen row of a text input.
func inputRow(t *testing.T, m *model, id string) (int, row) {
	t.Helper()
	ys := controlRows(m, "edit:"+id)
	if len(ys) != 1 {
		t.Fatalf("Text input %s has rows %v", id, ys)
	}
	return ys[0], m.screenRows()[ys[0]]
}

// A text input is one row of the control width at every pane width: long CJK and emoji text, the
// cursor at the end, Browse… beside it or under it. Its target is the rendered input.
func TestTextInputRowWidthAndTarget(t *testing.T) {
	for width := 6; width <= 60; width++ {
		for _, focused := range []bool{false, true} {
			m := flowModel(width)
			e := m.editors["start/dir"]
			e.setText("/very/long/path/界界界/😀😀/" + strings.Repeat("x", 80) + "/end")
			e.Input.CursorEnd()
			if focused {
				m.setFocus("edit:start/dir")
			}
			y, r := inputRow(t, m, "start/dir")
			c := controlAt(m, y, "edit:start/dir")
			text := r.Text
			if b := r.Extra; len(b) > 0 {
				if !browseBeside(m.controlWidth()) || b[0].Left != c.HitWidth+1 {
					t.Fatalf("width %d: Browse… beside a narrow input at %d", width, b[0].Left)
				}
				text = ansi.Cut(text, 0, c.HitWidth)
			} else if browseBeside(m.controlWidth()) || len(controlRows(m, "browse:start/dir")) != 1 {
				t.Fatalf("width %d: Browse… is not under the input", width)
			}
			if w := ansi.StringWidth(text); w != c.HitWidth || c.Left != 0 {
				t.Fatalf("width %d: input width %d, target %d at %d", width, w, c.HitWidth, c.Left)
			}
			if e.Input.Width() < 1 {
				t.Fatalf("width %d: input text width %d", width, e.Input.Width())
			}
			if visible := ansi.Strip(text); focused && width >= 12 && !strings.Contains(visible, "end") {
				t.Fatalf("width %d: the cursor end is not visible: %q", width, visible)
			}
		}
	}
}

// huh's Input shape: a bar while the input has focus or the pointer, the prompt in the highlight
// role while it has focus.
func TestTextInputBarAndPrompt(t *testing.T) {
	for _, dark := range []bool{false, true} {
		m := flowModel(60)
		m.setDark(dark)
		p := m.palette()
		_, r := inputRow(t, m, "start/title")
		if !strings.HasPrefix(ansi.Strip(r.Text), "  > ") {
			t.Fatalf("dark %v: blurred input %q", dark, ansi.Strip(r.Text))
		}
		m.hover = "edit:start/title"
		if _, r = inputRow(t, m, "start/title"); !strings.HasPrefix(ansi.Strip(r.Text), "┃ > ") {
			t.Fatalf("dark %v: hovered input has no bar: %q", dark, ansi.Strip(r.Text))
		}
		m.hover = ""
		m.setFocus("edit:start/title")
		_, r = inputRow(t, m, "start/title")
		if !strings.HasPrefix(ansi.Strip(r.Text), "┃ > ") || !strings.Contains(r.Text, lipgloss.NewStyle().Foreground(p.highlight).Render("> ")) {
			t.Fatalf("dark %v: focused input %q", dark, r.Text)
		}
	}
}

// A click on the input row focuses it. The wheel over it scrolls the form.
func TestTextInputClickAndWheel(t *testing.T) {
	m := flowModel(60)
	m.height = 10
	// Scroll the form so that the input is the second body row.
	for i, r := range m.bodyRows() {
		if r.Control != nil && r.Control.ID == "edit:start/title" {
			m.scroll = i - 1
		}
	}
	y, _ := inputRow(t, m, "start/title")
	x := m.leftMargin() + 4
	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y})
	m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: x, Y: y})
	if m.focus != "edit:start/title" || !m.editors["start/title"].Input.Focused() {
		t.Fatalf("A click focused %q", m.focus)
	}
	before := m.scroll
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: x, Y: y})
	if before == 0 || m.scroll != before-3 {
		t.Fatal("The wheel over a text input did not scroll the form")
	}
}

// Enter moves the focus to the next control and reveals it. It never submits or saves.
func TestTextInputEnterMovesFocus(t *testing.T) {
	m := flowModel(60)
	m.setFocus("edit:start/dir")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.focus != "browse:start/dir" {
		t.Fatalf("Enter in Directory moved to %q, not Browse…", m.focus)
	}
	m.setFocus("edit:start/title")
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.focus != "start:submit" {
		t.Fatalf("Enter in the last field moved to %q, not Submit", m.focus)
	}
	if len(m.client.Requests) != 0 || len(m.operations) != 0 {
		t.Fatal("Enter in a text field sent a request")
	}
	if m.editors["start/title"].text() != "x" {
		t.Fatal("Enter changed the text")
	}
}

// Enter keeps the picker wait when it moves to Browse…, as Tab does. A move elsewhere cancels it.
func TestTextInputEnterKeepsPickerRules(t *testing.T) {
	m := flowModel(60)
	m.startBrowse(browseControl())
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.focus != "browse:start/dir" || m.browse == nil {
		t.Fatal("Enter to Browse… cancelled the picker wait")
	}
	m.setFocus("edit:start/dir")
	m.moveFocus(-1)
	if m.browse != nil {
		t.Fatal("A move away from the field kept the picker wait")
	}
}

// Ctrl+U deletes the text before the cursor, as in the text area before.
func TestTextInputCtrlUDeletesBeforeCursor(t *testing.T) {
	m := flowModel(60)
	m.setFocus("edit:start/dir")
	e := m.editors["start/dir"]
	e.setText("/repo/sub")
	e.Input.SetCursor(5)
	m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	if e.text() != "/sub" || e.Input.Position() != 0 {
		t.Fatalf("Ctrl+U in the middle left %q at %d", e.text(), e.Input.Position())
	}
}

// A text input has no length limit.
func TestTextInputLongText(t *testing.T) {
	m := flowModel(30)
	m.setFocus("edit:start/title")
	long := strings.Repeat("abcdefghij", 60)
	m.Update(tea.PasteMsg{Content: long})
	m.Update(tea.KeyPressMsg{Code: 'z', Text: "z"})
	if got := m.editors["start/title"].text(); got != long+"z" {
		t.Fatalf("Long text has %d characters", len(got))
	}
}

// Newlines and tabs become spaces. A value with them is clean and submits normalized.
func TestTextInputNormalizesNewlinesAndTabs(t *testing.T) {
	m := testModel()
	s := flowSnapshot()
	s.Tree.Nodes[0].Fields[2].Value = "a\nb\tc"
	m.apply(s)
	e := m.editors["start/title"]
	if e.text() != "a b c" || e.dirty() {
		t.Fatalf("Snapshot value %q, dirty %v", e.text(), e.dirty())
	}
	m.setFocus("edit:start/title")
	m.Update(tea.PasteMsg{Content: "\nd"})
	if e.text() != "a b c d" {
		t.Fatalf("Pasted newline gave %q", e.text())
	}
	req := request(t, m, m.activate(control{Kind: "submit", Node: "start", Action: s.Tree.Nodes[0].SubmitAction}))
	values := req.Args["input"].(map[string]any)["values"].(map[string]any)
	if values["title"] != "a b c d" {
		t.Fatalf("Submitted %q", values["title"])
	}
}

// A saved submission is clean, and the next snapshot of the same value keeps text and cursor.
func TestTextInputSavedSubmitThenSnapshot(t *testing.T) {
	m := testModel()
	s := flowSnapshot()
	m.apply(s)
	e := m.editors["start/title"]
	m.setFocus("edit:start/title")
	m.Update(tea.PasteMsg{Content: "Review"})
	e.Input.SetCursor(2)
	req := request(t, m, m.activate(control{Kind: "submit", Node: "start", Action: s.Tree.Nodes[0].SubmitAction}))
	m.finish(completed{Request: req, Outcome: operationResult{Status: "succeeded", Effects: "done"}})
	if e.dirty() {
		t.Fatal("A saved submission is dirty")
	}
	next := flowSnapshot()
	next.Revision = "2"
	next.Tree.Nodes[0].Fields[2].Value = "Review"
	m.apply(next)
	if e.text() != "Review" || e.Input.Position() != 2 || e.dirty() {
		t.Fatalf("Snapshot after save: %q at %d", e.text(), e.Input.Position())
	}
}

// Ordinary snapshots and a theme change keep the text and the cursor.
func TestTextInputSnapshotAndThemeKeepCursor(t *testing.T) {
	m := flowModel(60)
	m.setFocus("edit:start/title")
	e := m.editors["start/title"]
	m.Update(tea.PasteMsg{Content: "local"})
	e.Input.SetCursor(2)
	s := flowSnapshot()
	s.Revision = "2"
	m.apply(s)
	m.setDark(!m.dark)
	if e.text() != "local" || e.Input.Position() != 2 {
		t.Fatalf("Snapshot and theme gave %q at %d", e.text(), e.Input.Position())
	}
}

// A new field revision replaces a dirty value, normalized and clean.
func TestTextInputRevisionReplacesDirtyValue(t *testing.T) {
	m := testModel()
	s := flowSnapshot()
	rev := int64(1)
	s.Tree.Nodes[0].Fields[2].Rev = &rev
	m.apply(s)
	e := m.editors["start/title"]
	e.setText("typed")
	next := flowSnapshot()
	next.Revision = "2"
	rev2 := int64(2)
	next.Tree.Nodes[0].Fields[2].Rev = &rev2
	next.Tree.Nodes[0].Fields[2].Value = "x\ty"
	m.apply(next)
	if e.text() != "x y" || e.dirty() {
		t.Fatalf("Revision replacement gave %q, dirty %v", e.text(), e.dirty())
	}
}

// A hidden field keeps its text. A removed or changed-kind dirty field keeps recovery text.
func TestTextInputHiddenAndRemovedFields(t *testing.T) {
	form := func(rev string, custom bool, kind string, withName bool) Snapshot {
		fields := []Field{{ID: "custom", Kind: "boolean", Value: custom}}
		if withName {
			fields = append(fields, Field{ID: "name", Label: "Name", Kind: kind, VisibleWhen: &FieldCondition{Field: "custom", Value: true}})
		}
		return Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/forms/f", Revision: rev, Tree: Tree{Nodes: []Node{
			{ID: "f", Type: "form", Fields: fields, SubmitAction: &Action{Name: "submit", Label: "Go", Enabled: true}}}}}
	}
	m := testModel()
	m.apply(form("1", true, "text", true))
	m.editors["f/name"].setText("mine")
	m.apply(form("2", false, "text", true))
	if len(controlRows(m, "edit:f/name")) != 0 || m.editors["f/name"].text() != "mine" {
		t.Fatal("A hidden field lost its text")
	}
	m.apply(form("3", true, "text", true))
	if m.editors["f/name"].text() != "mine" {
		t.Fatal("A shown field lost its text")
	}
	m.apply(form("4", true, "multiline", true))
	if m.recovery["f/name"] != "mine" {
		t.Fatalf("A changed kind kept recovery %q", m.recovery["f/name"])
	}
	delete(m.recovery, "f/name")
	m.apply(form("5", true, "text", true))
	m.editors["f/name"].setText("again")
	m.apply(form("6", true, "text", false))
	if m.recovery["f/name"] != "again" {
		t.Fatalf("A removed field kept recovery %q", m.recovery["f/name"])
	}
}
