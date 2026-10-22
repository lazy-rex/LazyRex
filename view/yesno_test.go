package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// yesNoModel is a form with a boolean field and a text field after it, at a width.
func yesNoModel(width int, value any) *model {
	m := testModel()
	m.width = width
	s := testSnapshot("saved", "1")
	s.Route = "/forms/f"
	s.Tree.Nodes = []Node{{ID: "f", Type: "form", Fields: []Field{
		{ID: "on", Label: "Enabled", Kind: "boolean", Value: value},
		{ID: "name", Label: "Name", Kind: "text"},
	}, SubmitAction: &Action{Name: "submit", Label: "Go", Enabled: true}}}
	m.apply(s)
	m.resizeEditors()
	return m
}

func choice(m *model) any { return m.editors["f/on"].Choice }

// Yes and No have distinct one-row targets. The gap and the title have none.
func TestYesNoTargets(t *testing.T) {
	m := yesNoModel(60, false)
	ys := controlRows(m, "yes:f/on")
	if len(ys) != 1 || len(controlRows(m, "no:f/on")) != 1 || controlRows(m, "no:f/on")[0] != ys[0] {
		t.Fatalf("Yes rows %v, No rows %v", ys, controlRows(m, "no:f/on"))
	}
	y := ys[0]
	yes, no := controlAt(m, y, "yes:f/on"), controlAt(m, y, "no:f/on")
	if yes.Left != 0 || yes.HitWidth != buttonWidth("Yes") || no.Left != yes.HitWidth+1 || no.HitWidth != buttonWidth("No") {
		t.Fatalf("Yes %+v, No %+v", yes, no)
	}
	if !strings.HasPrefix(ansi.Strip(m.screenRows()[y].Text), "  Yes     No  ") {
		t.Fatalf("Row %q", ansi.Strip(m.screenRows()[y].Text))
	}
	if !strings.Contains(ansi.Strip(m.screenRows()[y-1].Text), "Enabled") {
		t.Fatal("The title is not above the buttons")
	}
	x := m.leftMargin()
	for _, cell := range []struct {
		x, y int
		want string
	}{
		{x, y, "yes:f/on"}, {x + yes.HitWidth - 1, y, "yes:f/on"}, {x + yes.HitWidth, y, ""},
		{x + no.Left, y, "no:f/on"}, {x + no.Left + no.HitWidth - 1, y, "no:f/on"}, {x + no.Left + no.HitWidth, y, ""},
		{x, y - 1, ""}, {x, y + 1, ""},
	} {
		c, ok := m.hitControl(cell.x, cell.y)
		if (cell.want == "" && ok) || (cell.want != "" && c.ID != cell.want) {
			t.Fatalf("Cell %d,%d hits %q, want %q", cell.x, cell.y, c.ID, cell.want)
		}
	}
	// A click on each button sets its value.
	for _, c := range []control{no, yes, no} {
		m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x + c.Left, Y: y})
		m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: x + c.Left, Y: y})
		if choice(m) != c.Input["value"] {
			t.Fatalf("A click on %s gave %v", c.ID, choice(m))
		}
	}
}

// A press on one button and a release on the other or outside changes nothing.
func TestYesNoCanceledPress(t *testing.T) {
	m := yesNoModel(60, false)
	y := controlRows(m, "yes:f/on")[0]
	x := m.leftMargin()
	no := controlAt(m, y, "no:f/on")
	for _, release := range [][2]int{{x + no.Left, y}, {x, y + 1}, {x + no.Left - 1, y}} {
		m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y})
		m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: release[0], Y: release[1]})
		if choice(m) != false || m.pressed != "" {
			t.Fatalf("A release at %v chose %v", release, choice(m))
		}
	}
}

// The chosen button has the primary style, the other the secondary. Hover, press and focus change
// each button in both themes.
func TestYesNoStyles(t *testing.T) {
	for _, dark := range []bool{false, true} {
		m := yesNoModel(60, true)
		m.setDark(dark)
		y := controlRows(m, "yes:f/on")[0]
		row := func() string { return m.screenRows()[y].Text }
		yes, no := controlAt(m, y, "yes:f/on"), controlAt(m, y, "no:f/on")
		if !strings.Contains(row(), m.buttonView(yes.ID, "Yes", yes.HitWidth, true, true)) ||
			!strings.Contains(row(), m.buttonView(no.ID, "No", no.HitWidth, false, true)) {
			t.Fatalf("dark %v: Yes is not primary and No secondary", dark)
		}
		idle := row()
		m.hover = no.ID
		hover := row()
		m.pressed = no.ID
		press := row()
		m.hover, m.pressed = "", ""
		m.setFocus(no.ID)
		focus := row()
		if idle == hover || hover == press || idle == focus {
			t.Fatalf("dark %v: a state does not change No", dark)
		}
		m.setFocus("")
		m.activate(no)
		if !strings.Contains(row(), m.buttonView(no.ID, "No", no.HitWidth, true, true)) {
			t.Fatalf("dark %v: the chosen No is not primary", dark)
		}
	}
}

// In a narrow pane Yes and No stack with a blank row between them. Nothing exceeds the content
// width (at least 8 cells).
func TestYesNoNarrow(t *testing.T) {
	for width := 6; width <= 24; width++ {
		m := yesNoModel(width, false)
		yes, no := controlRows(m, "yes:f/on"), controlRows(m, "no:f/on")
		if len(yes) != 1 || len(no) != 1 {
			t.Fatalf("width %d: Yes rows %v, No rows %v", width, yes, no)
		}
		stacked := buttonWidth("Yes")+1+buttonWidth("No") > m.contentWidth()
		if stacked && no[0] != yes[0]+2 || !stacked && no[0] != yes[0] {
			t.Fatalf("width %d: Yes row %d, No row %d", width, yes[0], no[0])
		}
		if stacked {
			if _, ok := m.hitControl(m.leftMargin(), yes[0]+1); ok {
				t.Fatalf("width %d: the blank row has a target", width)
			}
		}
		for i, r := range m.bodyRows() {
			if w := ansi.StringWidth(r.Text); w > m.contentWidth() {
				t.Fatalf("width %d: row %d has width %d", width, i, w)
			}
		}
	}
}

// Keys: Tab stops once at Yes and once at No. Enter and space choose the focused button.
func TestYesNoKeys(t *testing.T) {
	m := yesNoModel(60, false)
	var ids []string
	for _, c := range m.controls() {
		ids = append(ids, c.ID)
	}
	if got := strings.Join(ids, " "); got != "yes:f/on no:f/on edit:f/name f:submit" {
		t.Fatalf("Focus order %q", got)
	}
	m.setFocus("yes:f/on")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if choice(m) != true || m.focus != "yes:f/on" {
		t.Fatalf("Enter on Yes gave %v", choice(m))
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if m.focus != "no:f/on" || choice(m) != false {
		t.Fatalf("Space on No gave %v at %q", choice(m), m.focus)
	}
	if len(m.client.Requests) != 0 || len(m.operations) != 0 {
		t.Fatal("A choice sent a request")
	}
}

// No submits false. Yes and back to the saved value follow the dirty rule, and a removed dirty
// field keeps recovery text.
func TestYesNoSubmitDirtyAndRecovery(t *testing.T) {
	m := yesNoModel(60, false)
	e := m.editors["f/on"]
	submit := control{Kind: "submit", Node: "f", Action: m.node("f").SubmitAction}
	req := request(t, m, m.activate(submit))
	if v := req.Args["input"].(map[string]any)["values"].(map[string]any)["on"]; v != false {
		t.Fatalf("No submitted %#v", v)
	}
	m = yesNoModel(60, false)
	e = m.editors["f/on"]
	m.activate(control{Kind: "boolean", Node: "f", Field: "on", Input: map[string]any{"value": true}})
	if !e.dirty() {
		t.Fatal("Yes is not an unsaved change")
	}
	m.activate(control{Kind: "boolean", Node: "f", Field: "on", Input: map[string]any{"value": false}})
	if e.dirty() {
		t.Fatal("Back to the saved value is still dirty")
	}
	m.activate(control{Kind: "boolean", Node: "f", Field: "on", Input: map[string]any{"value": true}})
	s := testSnapshot("saved", "1")
	s.Route = "/forms/f"
	s.Revision = "2"
	s.Tree.Nodes = []Node{{ID: "f", Type: "form", Fields: []Field{{ID: "name", Label: "Name", Kind: "text"}}}}
	m.apply(s)
	if m.recovery["f/on"] != "true" {
		t.Fatalf("Recovery %q", m.recovery["f/on"])
	}
}
