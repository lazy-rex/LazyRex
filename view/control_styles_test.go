package main

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// controlRows returns the screen rows that have a target for the control.
func controlRows(m *model, id string) []int {
	var ys []int
	for y, r := range m.screenRows() {
		for _, c := range r.rowControls() {
			if c.ID == id {
				ys = append(ys, y)
			}
		}
	}
	return ys
}

// controlAt returns the control on a screen row, by ID.
func controlAt(m *model, y int, id string) control {
	for _, c := range m.screenRows()[y].rowControls() {
		if c.ID == id {
			return c
		}
	}
	return control{}
}

func TestControlRolesInLightAndDark(t *testing.T) {
	m := testModel()
	light := m.palette()
	m.dark = true
	dark := m.palette()
	for _, p := range []palette{light, dark} {
		roles := map[string]color.Color{"title": p.title, "highlight": p.highlight, "onHighlight": p.onHighlight, "selected": p.selected, "danger": p.danger,
			"control": p.control, "button": p.button, "hover": p.hover, "press": p.press, "highlightHover": p.highlightHover, "highlightPress": p.highlightPress}
		for name, c := range roles {
			if c == nil {
				t.Fatalf("Role %s has no color", name)
			}
		}
		if p.hover == p.button || p.press == p.button || p.hover == p.press {
			t.Fatal("A secondary button has no hover or press feedback")
		}
		if p.highlightHover == p.highlight || p.highlightPress == p.highlight || p.highlightHover == p.highlightPress {
			t.Fatal("A primary button has no hover or press feedback")
		}
	}
	for name, pair := range map[string][2]color.Color{"title": {light.title, dark.title}, "selected": {light.selected, dark.selected}, "danger": {light.danger, dark.danger},
		"button": {light.button, dark.button}, "hover": {light.hover, dark.hover}, "press": {light.press, dark.press}, "control": {light.control, dark.control}} {
		if pair[0] == pair[1] {
			t.Fatalf("Role %s is the same in light and dark", name)
		}
	}
}

// A button is one row. Each cell of the label and its padding hits the button; the cells beside
// it and the rows above and below do not.
func TestOneRowButtonBounds(t *testing.T) {
	m := testModel()
	m.apply(flowSnapshot())
	ys := controlRows(m, "start:submit")
	if len(ys) != 1 {
		t.Fatalf("Submit has %d rows, want 1", len(ys))
	}
	y := ys[0]
	submit, cancel := controlAt(m, y, "start:submit"), controlAt(m, y, "cancel:action")
	if submit.HitWidth != buttonWidth("Start") || cancel.HitWidth != buttonWidth("Cancel") {
		t.Fatalf("Button widths %d and %d", submit.HitWidth, cancel.HitWidth)
	}
	if cancel.Left != submit.Left+submit.HitWidth+1 {
		t.Fatalf("Cancel starts at %d, want one cell after Submit", cancel.Left)
	}
	for _, c := range []control{submit, cancel} {
		for x := c.Left; x < c.Left+c.HitWidth; x++ {
			if hit, ok := m.hitControl(m.leftMargin()+x, y); !ok || hit.ID != c.ID {
				t.Fatalf("Cell %d of %s hits %q", x, c.ID, hit.ID)
			}
			for _, other := range []int{y - 1, y + 1} {
				if hit, ok := m.hitControl(m.leftMargin()+x, other); ok && hit.ID == c.ID {
					t.Fatalf("Row %d beside %s hits it", other, c.ID)
				}
			}
		}
	}
	// The gap between the adjacent buttons hits nothing, and so does the cell after Cancel.
	for _, x := range []int{submit.Left + submit.HitWidth, cancel.Left + cancel.HitWidth} {
		if hit, ok := m.hitControl(m.leftMargin()+x, y); ok {
			t.Fatalf("Cell %d beside the buttons hits %q", x, hit.ID)
		}
	}
	line := ansi.Strip(m.screenRows()[y].Text)
	if !strings.HasPrefix(line, "  Start  "+" "+"  Cancel  ") {
		t.Fatalf("Button row %q", line)
	}
}

// A press that ends outside its button, or on another button, runs nothing.
func TestCanceledButtonPress(t *testing.T) {
	m := testModel()
	m.ready = true
	m.apply(flowSnapshot())
	y := controlRows(m, "start:submit")[0]
	submit, cancel := controlAt(m, y, "start:submit"), controlAt(m, y, "cancel:action")
	x := m.leftMargin() + submit.Left
	for _, release := range [][2]int{{x, y + 1}, {x + submit.HitWidth, y}, {m.leftMargin() + cancel.Left, y}} {
		m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y})
		if m.pressed != "start:submit" {
			t.Fatal("The press did not start on Submit")
		}
		_, cmd := m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: release[0], Y: release[1]})
		if cmd != nil || len(m.operations) != 0 || m.pressed != "" {
			t.Fatalf("A release at %v ran an action", release)
		}
	}
	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y})
	if _, cmd := m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: x + submit.HitWidth - 1, Y: y}); cmd == nil || len(m.operations) != 1 {
		t.Fatal("A release on the last cell of Submit did not submit")
	}
}

// Hover and press change a primary and a secondary button. Focus underlines the label.
func TestButtonFeedback(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, primary := range []bool{true, false} {
			m := testModel()
			m.dark = dark
			idle := m.buttonView("b", "Start", 9, primary, true)
			m.hover = "b"
			hover := m.buttonView("b", "Start", 9, primary, true)
			m.pressed = "b"
			press := m.buttonView("b", "Start", 9, primary, true)
			m.hover, m.pressed, m.focus = "", "", "b"
			focus := m.buttonView("b", "Start", 9, primary, true)
			if idle == hover || hover == press || idle == press || idle == focus {
				t.Fatalf("dark=%t primary=%t: a state does not change the button", dark, primary)
			}
			for _, view := range []string{idle, hover, press, focus} {
				if ansi.StringWidth(view) != 9 || strings.Contains(view, "\n") || ansi.Strip(view) != "  Start  " {
					t.Fatalf("Button view %q is not one padded row", ansi.Strip(view))
				}
			}
		}
	}
}

// Buttons that do not fit side by side stack, with a blank row between them. Each keeps its own
// target, and the blank row hits nothing.
func TestNarrowButtonsStack(t *testing.T) {
	m := testModel()
	m.width = 14
	s := flowSnapshot()
	s.Tree.Nodes[0].SubmitAction.Label = "Start the work"
	m.apply(s)
	submit, cancel := controlRows(m, "start:submit"), controlRows(m, "cancel:action")
	if len(submit) != 1 || len(cancel) != 1 || cancel[0] != submit[0]+2 {
		t.Fatalf("Submit rows %v, Cancel rows %v", submit, cancel)
	}
	if hit, ok := m.hitControl(m.leftMargin(), submit[0]+1); ok {
		t.Fatalf("The row between stacked buttons hits %q", hit.ID)
	}
	for _, y := range []int{submit[0], cancel[0]} {
		if ansi.StringWidth(m.screenRows()[y].Text) > m.contentWidth() {
			t.Fatal("A stacked button exceeds the content width")
		}
	}
	if c := controlAt(m, submit[0], "start:submit"); c.HitWidth != m.contentWidth() {
		t.Fatalf("A long label is %d wide, want the content width %d", c.HitWidth, m.contentWidth())
	}
}

// Browse… is on the text row of its box. The border rows beside it hit the text box only.
func TestBrowseTargetIsOneRow(t *testing.T) {
	m := testModel()
	m.ready = true
	m.apply(flowSnapshot())
	browse := controlRows(m, "browse:start/dir")
	edit := controlRows(m, "edit:start/dir")
	if len(browse) != 1 || len(edit) != 3 || browse[0] != edit[1] {
		t.Fatalf("Browse rows %v, text box rows %v", browse, edit)
	}
	c := controlAt(m, browse[0], "browse:start/dir")
	x := m.leftMargin() + c.Left
	if hit, ok := m.hitControl(x, browse[0]); !ok || hit.ID != c.ID {
		t.Fatal("Browse has no target")
	}
	for _, y := range []int{edit[0], edit[2]} {
		if hit, ok := m.hitControl(x, y); ok && hit.ID == c.ID {
			t.Fatalf("Border row %d hits Browse", y)
		}
	}
	if !strings.Contains(ansi.Strip(m.screenRows()[browse[0]].Text), "  Browse…  ") {
		t.Fatal("Browse… is not on the text row")
	}
}

// contrast is the WCAG contrast ratio of two colors.
func contrast(a, b color.Color) float64 {
	lum := func(c color.Color) float64 {
		r, g, b, _ := c.RGBA()
		return relativeLuminance(float64(r)/0xffff, float64(g)/0xffff, float64(b)/0xffff)
	}
	x, y := lum(a)+0.05, lum(b)+0.05
	return max(x, y) / min(x, y)
}

// Button labels keep at least 4.5:1 in every state and theme. Text roles keep 4.5:1 and the
// control border 3:1 on a reference background (white, or zinc-900 in dark mode). The primary
// button stands out more from the background than a secondary button.
func TestControlContrast(t *testing.T) {
	m := testModel()
	for _, dark := range []bool{false, true} {
		m.dark = dark
		p := m.palette()
		background := color.Color(color.White)
		if dark {
			background = lipgloss.Color("#18181B")
		}
		check := func(name string, fg, bg color.Color, minimum float64) {
			ratio := contrast(fg, bg)
			t.Logf("dark=%t %s %.2f:1", dark, name, ratio)
			if ratio < minimum {
				t.Errorf("dark=%t %s is %.2f:1, want %.1f:1", dark, name, ratio, minimum)
			}
		}
		check("primary label idle", p.onHighlight, p.highlight, 4.5)
		check("primary label hover", p.onHighlight, p.highlightHover, 4.5)
		check("primary label press", p.onHighlight, p.highlightPress, 4.5)
		check("secondary label idle", p.text, p.button, 4.5)
		check("secondary label hover", p.text, p.hover, 4.5)
		check("secondary label press", p.text, p.press, 4.5)
		check("title", p.title, background, 4.5)
		check("highlight text", p.highlight, background, 4.5)
		check("selected", p.selected, background, 4.5)
		check("danger", p.danger, background, 4.5)
		check("control border", p.control, background, 3)
		primary, secondary := contrast(p.highlight, background), contrast(p.button, background)
		t.Logf("dark=%t primary fill %.2f:1, secondary fill %.2f:1 against the background", dark, primary, secondary)
		if primary <= secondary {
			t.Errorf("dark=%t the secondary button stands out more than the primary button", dark)
		}
	}
}
