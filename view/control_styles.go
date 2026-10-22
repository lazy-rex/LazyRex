package main

import (
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"strings"
)

// buttonPadding is the space on each side of a button label (huh ThemeBase: Padding(0, 2)).
const buttonPadding = 2

// buttonWidth is the natural width of a button: the label and its padding.
func buttonWidth(text string) int {
	return ansi.StringWidth(singleLine(text)) + 2*buttonPadding
}

func (m *model) primaryControlStyle(id string) lipgloss.Style {
	p := m.palette()
	style := m.primaryStyle()
	if m.hover == id {
		style = style.Background(p.highlightHover)
	}
	if m.pressed == id && m.hover == id {
		style = style.Background(p.highlightPress)
	}
	return style
}

// buttonView is a one-row button in the shape of huh's ThemeCharm: a primary button has the
// focused-button colors, and a secondary button has the blurred-button colors. The keyboard focus
// underlines the label. A disabled button has stripes and ignores hover, press and focus.
func (m *model) buttonView(id, text string, width int, primary, enabled bool) string {
	p := m.palette()
	style := lipgloss.NewStyle().Foreground(p.text).Background(p.button)
	if enabled {
		if primary {
			style = m.primaryControlStyle(id)
		} else {
			style = m.controlStyle(id, style)
		}
		if m.focus == id {
			style = style.Underline(true).UnderlineSpaces(false)
		}
	} else {
		style = style.Foreground(p.muted)
	}
	text = ansi.Truncate(singleLine(text), max(1, width-2), "…")
	if !enabled {
		label := " " + text + " "
		remaining := max(0, width-ansi.StringWidth(label))
		left := remaining / 2
		stripe := func(n int) string { return style.Render(strings.Repeat("╱", n)) }
		return stripe(left) + style.Render(label) + stripe(remaining-left)
	}
	return style.Width(width).Align(lipgloss.Center).Render(text)
}

// inputBar is the width of the bar before a text input: the bar and one space.
const inputBar = 2

// inputStyles are the text input styles of huh's ThemeCharm in our roles: the prompt has the
// highlight role while the input has focus.
func (m *model) inputStyles() textinput.Styles {
	p := m.palette()
	s := textinput.DefaultStyles(m.dark)
	s.Focused.Text = lipgloss.NewStyle().Foreground(p.text)
	s.Focused.Placeholder = lipgloss.NewStyle().Foreground(p.muted)
	s.Focused.Prompt = lipgloss.NewStyle().Foreground(p.highlight)
	s.Blurred = s.Focused
	s.Blurred.Prompt = lipgloss.NewStyle().Foreground(p.control)
	s.Cursor.Color = p.accent
	return s
}

// inputView is one row of exactly width cells: huh's thick bar while the input has focus or the
// pointer, then the input with its prompt.
func (m *model) inputView(id string, e *editor, width int) string {
	bar := strings.Repeat(" ", inputBar)
	if m.focus == id || m.hover == id {
		bar = lipgloss.NewStyle().Foreground(m.palette().control).Render("┃") + " "
	}
	line := bar + e.Input.View()
	if w := ansi.StringWidth(line); w > width {
		line = ansi.Truncate(line, width, "")
	} else {
		line += strings.Repeat(" ", width-w)
	}
	return line
}

// primaryButton reports a button with the primary style: Submit, and the chosen Yes or No.
func (m *model) primaryButton(c control) bool {
	if c.Kind == "boolean" {
		e := m.editors[editorID(c.Node, c.Field)]
		if e == nil {
			return false
		}
		chosen, _ := e.Choice.(bool)
		return chosen == c.Input["value"]
	}
	return c.Kind == "submit"
}
