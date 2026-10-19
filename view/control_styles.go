package main

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"image/color"
	"strings"
)

func (m *model) hoverColor() color.Color {
	if m.dark {
		return lipgloss.Color("#253145")
	}
	return lipgloss.Color("#F1F3F5")
}
func (m *model) pressColor() color.Color {
	if m.dark {
		return lipgloss.Color("#334155")
	}
	return lipgloss.Color("#DCE1E7")
}
func (m *model) controlBorder() color.Color {
	if m.dark {
		return lipgloss.Color("#94A3B8")
	}
	return lipgloss.Color("#64748B")
}
func (m *model) primaryControlStyle(id string) lipgloss.Style {
	style := m.primaryStyle()
	if m.hover == id {
		color := lipgloss.Color("#5B52DE")
		if m.dark {
			color = lipgloss.Color("#C7D2FE")
		}
		style = style.Background(color)
	}
	if m.pressed == id && m.hover == id {
		color := lipgloss.Color("#312E81")
		if m.dark {
			color = lipgloss.Color("#818CF8")
		}
		style = style.Background(color)
	}
	return style
}

// buttonLines paints half a row above and below a one-row label.
func (m *model) buttonLines(id, text string, width int, primary, enabled bool) [3]string {
	p := m.palette()
	style := lipgloss.NewStyle().Foreground(p.text).Background(p.surface)
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
	capStyle := lipgloss.NewStyle().Foreground(style.GetBackground())
	text = ansi.Truncate(singleLine(text), max(1, width-2), "…")
	if !enabled {
		label := " " + text + " "
		remaining := max(0, width-ansi.StringWidth(label))
		left := remaining / 2
		stripe := func(n int) string { return style.Render(strings.Repeat("╱", n)) }
		middle := stripe(left) + style.Render(label) + stripe(remaining-left)
		return [3]string{capStyle.Render(strings.Repeat("▄", width)), middle, capStyle.Render(strings.Repeat("▀", width))}
	}
	return [3]string{capStyle.Render(strings.Repeat("▄", width)), style.Width(width).Align(lipgloss.Center).Render(text), capStyle.Render(strings.Repeat("▀", width))}
}
