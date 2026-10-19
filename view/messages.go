package main

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func grouped(previous *Message, current Message) bool {
	// An FYI change starts a new group, so its header (with the note) is shown.
	if previous == nil || previous.Author != current.Author || previous.Own != current.Own || previous.FYI != current.FYI {
		return false
	}
	a, errA := time.Parse(time.RFC3339Nano, previous.At)
	b, errB := time.Parse(time.RFC3339Nano, current.At)
	return errA == nil && errB == nil && !b.Before(a) && b.Sub(a) <= 2*time.Minute
}
func (m *model) messageLines(msg Message, previous *Message) []string {
	p := m.palette()
	width := m.contentWidth()
	maxWidth := max(8, min(100, width*7/10))
	lines := strings.Split(m.messageBody(msg.Text), "\n")
	bodyWidth := 1
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
		bodyWidth = max(bodyWidth, lipgloss.Width(lines[i]))
	}
	name := lipgloss.NewStyle().Foreground(m.authorColor(msg.Author)).Bold(true).Render(singleLine(msg.Author))
	if msg.Own {
		name = lipgloss.NewStyle().Foreground(p.accent).Bold(true).Render("You")
	}
	meta := shortTime(msg.At)
	if msg.FYI {
		meta += " · no reply needed"
	}
	header := name + "  " + lipgloss.NewStyle().Foreground(p.muted).Render(meta)
	bodyWidth = min(maxWidth-4, max(bodyWidth, lipgloss.Width(header)))
	border := p.border
	if m.dark {
		border = lipgloss.Color("#334155")
	}
	if msg.Own {
		border = p.accent
	}
	block := lipgloss.NewStyle().Width(bodyWidth+4).Padding(0, 1).Border(lipgloss.RoundedBorder()).BorderForeground(border).Render(strings.Join(lines, "\n"))
	bubbleWidth := lipgloss.Width(block)
	inset := 0
	if msg.Own {
		inset = max(0, width-bubbleWidth)
	}
	var result []string
	if !grouped(previous, msg) {
		result = append(result, strings.Repeat(" ", inset)+ansi.Truncate(header, bubbleWidth, "…"))
	}
	for _, line := range strings.Split(block, "\n") {
		result = append(result, strings.Repeat(" ", inset)+line)
	}
	return result
}

// Message rows do not depend on pointer position, focus, or draft state.
type messageRowCache struct {
	message       Message
	width         int
	dark, grouped bool
	rows          []row
}

func (m *model) historyMessageRows(node string, index int, msg Message, previous *Message) []row {
	key := fmt.Sprintf("%s/%d/%s", node, index, msg.ID)
	entry, ok := m.messageRows[key]
	sameGroup := grouped(previous, msg)
	if ok && entry.message == msg && entry.width == m.contentWidth() && entry.dark == m.dark && entry.grouped == sameGroup {
		return entry.rows
	}
	var rows []row
	for j, line := range m.messageLines(msg, previous) {
		for k, part := range strings.Split(ansi.Hardwrap(line, m.contentWidth(), true), "\n") {
			rows = append(rows, row{Text: part, Anchor: fmt.Sprintf("%s:%s:line:%d:%d", node, msg.ID, j, k)})
		}
	}
	if m.messageRows == nil || len(m.messageRows) > 2048 {
		m.messageRows = map[string]messageRowCache{}
	}
	m.messageRows[key] = messageRowCache{message: msg, width: m.contentWidth(), dark: m.dark, grouped: sameGroup, rows: rows}
	return rows
}
