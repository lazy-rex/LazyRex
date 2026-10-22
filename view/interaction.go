package main

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m *model) controlStyle(id string, style lipgloss.Style) lipgloss.Style {
	if m.hover == id {
		style = style.Background(m.palette().hover)
	}
	if m.pressed == id && m.hover == id {
		style = style.Background(m.palette().press)
	}
	return style
}

// hitControl checks the floating list before the visible form rows.
func (m *model) hitControl(x, y int) (control, bool) {
	if overlay := m.selectOverlay(); overlay != nil {
		return overlay.hit(x, y)
	}
	return m.hitRows(m.screenRows(), x, y)
}
func (m *model) hitRows(rows []row, x, y int) (control, bool) {
	var layers []*lipgloss.Layer
	controls := map[string]control{}
	for i, r := range rows {
		for _, c := range r.rowControls() {
			width := c.HitWidth
			if width == 0 {
				width = m.contentWidth() - c.Left
			}
			width = min(width, m.contentWidth()-c.Left)
			if width < 1 {
				continue
			}
			id := fmt.Sprintf("%d/%s", i, c.ID)
			layers = append(layers, lipgloss.NewLayer(strings.Repeat(" ", width)).ID(id).X(m.leftMargin()+c.Left).Y(i))
			controls[id] = c
		}
	}
	hit := lipgloss.NewCompositor(layers...).Hit(x, y)
	if hit.Empty() {
		return control{}, false
	}
	c, ok := controls[hit.ID()]
	return c, ok
}
func (m *model) refreshHover() {
	m.hover = ""
	if m.selectText || !m.pointerKnown {
		return
	}
	if c, ok := m.hitControl(m.pointerX, m.pointerY); ok {
		m.hover = c.ID
	}
	if target := m.scrollbarTarget(m.pointerX, m.pointerY); target != "" {
		m.hover = target
	}
}

func (m *model) editorHeight(e *editor) int {
	if e.Kind == "text" {
		return 1
	}
	return min(e.Area.Height(), max(1, min(10, (m.height-8)/2)))
}
func (m *model) editorOffset(e *editor) int {
	height := m.editorHeight(e)
	offset := e.Scroll
	if !e.Reviewing {
		// A value copy exposes the library's cursor coordinates without changing the editor.
		area := e.Area
		area.SetVirtualCursor(false)
		area.Focus()
		if cursor := area.Cursor(); cursor != nil {
			if cursor.Position.Y < offset {
				offset = cursor.Position.Y
			}
			if cursor.Position.Y >= offset+height {
				offset = cursor.Position.Y - height + 1
			}
		}
	}
	return max(0, min(offset, e.Area.Height()-height))
}
func (m *model) editorView(e *editor) string {
	v := viewport.New(viewport.WithWidth(e.Area.Width()), viewport.WithHeight(m.editorHeight(e)))
	v.SetContent(e.Area.View())
	v.SetYOffset(m.editorOffset(e))
	return v.View()
}
func (m *model) wheel(msg tea.MouseWheelMsg) tea.Cmd {
	if m.selectText {
		return nil
	}
	delta := 0
	switch msg.Button {
	case tea.MouseWheelUp:
		delta = -3
	case tea.MouseWheelDown:
		delta = 3
	default:
		return nil
	}
	if c, ok := m.hitControl(msg.X, msg.Y); ok && c.Kind == "edit" {
		// A one-line text field cannot scroll. The wheel scrolls the form under it.
		if e := m.editors[editorID(c.Node, c.Field)]; e != nil && e.Kind != "text" {
			e.Scroll = max(0, min(m.editorOffset(e)+delta, e.Area.Height()-m.editorHeight(e)))
			e.Reviewing = true
			return nil
		}
	}
	if msg.X >= m.leftMargin() && msg.X < m.leftMargin()+m.contentWidth() && msg.Y >= headerRows && msg.Y < headerRows+m.bodyHeight() {
		m.scroll = max(0, min(m.scroll+delta, len(m.bodyRows())-m.bodyHeight()))
	}
	return nil
}
func (m *model) notice() string {
	switch m.status {
	case "Connected", "Draft saved", "Sent", "Done", "":
		return ""
	}
	return m.status
}
func (m *model) footerHeight() int {
	if m.notice() != "" {
		return 2
	}
	return 1
}

// scrollbar returns the track height, thumb height, thumb start, and maximum offset.
func (m *model) scrollbar() (int, int, int, int) {
	h := m.bodyHeight()
	total := len(m.bodyRows())
	limit := max(0, total-h)
	if limit == 0 {
		return h, h, 0, 0
	}
	thumb := max(1, h*h/total)
	start := min(m.scroll, limit) * (h - thumb) / limit
	return h, thumb, start, limit
}
func (m *model) pressScrollbar(x, y int) bool {
	h, thumb, start, limit := m.scrollbar()
	if m.selectText || limit == 0 || (x < m.leftMargin()+m.contentWidth()+1 || x > m.leftMargin()+m.contentWidth()+2) || y < headerRows || y >= headerRows+h {
		return false
	}
	m.scrollDragging = true
	m.pressed = ""
	m.scrollGrab = y - headerRows - start
	if m.scrollGrab < 0 || m.scrollGrab >= thumb {
		m.scrollGrab = thumb / 2
		m.dragScrollbar(y)
	}
	return true
}
func (m *model) dragScrollbar(y int) {
	h, thumb, _, limit := m.scrollbar()
	travel := h - thumb
	if travel <= 0 {
		m.scroll = 0
		return
	}
	position := max(0, min(travel, y-headerRows-m.scrollGrab))
	m.scroll = (position*limit + travel/2) / travel
}

// Hover uses geometry from the last frame, so motion does not lay out history.
func (m *model) scrollbarTarget(x, y int) string {
	h, thumb, start, limit := m.visibleScrollbar[0], m.visibleScrollbar[1], m.visibleScrollbar[2], m.visibleScrollbar[3]
	left := m.leftMargin() + m.contentWidth() + 1
	if m.selectText || limit == 0 || x < left || x > left+1 || y < headerRows || y >= headerRows+h {
		return ""
	}
	if y-headerRows >= start && y-headerRows < start+thumb {
		return "$scrollbar-thumb"
	}
	return "$scrollbar-track"
}

// primaryStyle is the focused button of huh ThemeCharm: cream on fuchsia.
func (m *model) primaryStyle() lipgloss.Style {
	p := m.palette()
	return lipgloss.NewStyle().Foreground(p.onHighlight).Background(p.highlight).Bold(true)
}
