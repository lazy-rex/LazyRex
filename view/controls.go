package main

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"strings"
)

func (m *model) checkboxView(id, label string, checked bool) string {
	p := m.palette()
	indicatorColor := p.control
	if checked || m.focus == id || m.hover == id {
		indicatorColor = p.accent
	}
	style := m.controlStyle(id, lipgloss.NewStyle())
	edge := style.Foreground(indicatorColor)
	center := edge
	if checked {
		center = center.Background(p.accent)
	}
	indicator := center.Render("▏▕")
	topEdge := edge.Render("▁▁")
	bottomEdge := edge.Render("▔▔")
	label = ansi.Truncate(singleLine(label), max(1, m.controlWidth()-6), "…")
	textStyle := style.Foreground(p.text)
	if m.focus == id {
		textStyle = textStyle.Bold(true).Foreground(p.accent)
	}
	padding := func(width int) string { return style.Render(strings.Repeat(" ", max(0, width))) }
	line := padding(2) + indicator + padding(2) + textStyle.Render(label) + padding(m.controlWidth()-6-ansi.StringWidth(label))
	top := padding(2) + topEdge + padding(m.controlWidth()-4)
	bottom := padding(2) + bottomEdge + padding(m.controlWidth()-4)
	return top + "\n" + line + "\n" + bottom
}
