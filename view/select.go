package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const selectWindow = 8
const selectPadding = 2

// Highlight is an index in the filtered options. Opening and moving never change the field value.
type selectList struct {
	Node, Field, Filter string
	Highlight, Offset   int
	Value               string
}

func (s *selectList) controlID() string    { return "choice:" + editorID(s.Node, s.Field) }
func (s *selectList) optionPrefix() string { return "option:" + editorID(s.Node, s.Field) + ":" }

func (m *model) selectField(node, field string) *Field {
	if n := m.node(node); n != nil && n.Type == "form" {
		for _, f := range n.Fields {
			if f.ID == field && f.Kind == "select" && m.fieldVisible(node, f) {
				return &f
			}
		}
	}
	return nil
}
func filteredOptions(f *Field, query string) []Option {
	var result []Option
	if f == nil {
		return result
	}
	query = strings.ToLower(query)
	for _, o := range f.Options {
		if strings.Contains(strings.ToLower(o.Label), query) {
			result = append(result, o)
		}
	}
	return result
}
func (m *model) selectOptions() []Option {
	s := m.selectList
	if s == nil {
		return nil
	}
	return filteredOptions(m.selectField(s.Node, s.Field), s.Filter)
}
func (m *model) positionSelect(index int) {
	s := m.selectList
	if s == nil {
		return
	}
	opts := m.selectOptions()
	capacity := m.selectCapacity()
	s.Highlight = max(0, min(index, len(opts)-1))
	s.Offset = max(0, min(s.Offset, max(0, len(opts)-capacity)))
	if s.Highlight < s.Offset {
		s.Offset = s.Highlight
	}
	if s.Highlight >= s.Offset+capacity {
		s.Offset = s.Highlight - capacity + 1
	}
	s.Value = ""
	if len(opts) > 0 {
		s.Value = opts[s.Highlight].Value
	}
}
func (m *model) openSelect(c control) {
	if m.selectList != nil && m.selectList.controlID() == c.ID {
		m.selectList = nil
		return
	}
	f := m.selectField(c.Node, c.Field)
	e := m.editors[editorID(c.Node, c.Field)]
	if f == nil || len(f.Options) == 0 || e == nil {
		return
	}
	m.setFocus(c.ID)
	m.selectList = &selectList{Node: c.Node, Field: c.Field}
	index := 0
	for i, o := range f.Options {
		if o.Value == e.Choice {
			index = i
			break
		}
	}
	m.positionSelect(index)
}
func (m *model) closeSelect() {
	if s := m.selectList; s != nil {
		m.selectList = nil
		m.setFocus(s.controlID())
	}
}
func (m *model) pickSelect(c control) {
	s := m.selectList
	if s == nil || c.Node != s.Node || c.Field != s.Field {
		return
	}
	value, ok := c.Input["value"].(string)
	if !ok {
		return
	}
	for _, o := range m.selectOptions() {
		if o.Value == value {
			m.editors[editorID(s.Node, s.Field)].Choice = value
			m.closeSelect()
			return
		}
	}
}
func (m *model) cycleSelect(forward bool) bool {
	for _, c := range m.controls() {
		if c.ID != m.focus || c.Kind != "choice" {
			continue
		}
		f := m.selectField(c.Node, c.Field)
		if f == nil {
			return false
		}
		if len(f.Options) == 0 {
			return true
		}
		e := m.editors[editorID(c.Node, c.Field)]
		idx := 0
		for i, o := range f.Options {
			if o.Value == e.Choice {
				idx = i
				break
			}
		}
		step := 1
		if !forward {
			step = -1
		}
		e.Choice = f.Options[(idx+step+len(f.Options))%len(f.Options)].Value
		return true
	}
	return false
}

// Keep the highlight by option value when a tree changes. A removed option gets a nearby highlight.
func (m *model) reconcileSelect() {
	s := m.selectList
	if s == nil {
		return
	}
	f := m.selectField(s.Node, s.Field)
	if f == nil || len(f.Options) == 0 {
		m.closeSelect()
		return
	}
	index := s.Highlight
	for i, o := range m.selectOptions() {
		if o.Value == s.Value {
			index = i
			break
		}
	}
	m.positionSelect(index)
}

func (m *model) updateSelect(msg tea.Msg) bool {
	s := m.selectList
	if s == nil {
		return false
	}
	queryChanged := false
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		m.pressed = ""
		overlay := m.selectOverlay()
		if overlay == nil || !overlay.contains(msg.X, msg.Y) {
			m.closeSelect()
			return true
		}
		if c, ok := overlay.hit(msg.X, msg.Y); ok && msg.Button == tea.MouseLeft {
			m.pressed = c.ID
		}
		return true
	case tea.MouseReleaseMsg:
		pressed := m.pressed
		m.pressed = ""
		if overlay := m.selectOverlay(); overlay != nil && msg.Button == tea.MouseLeft {
			if c, ok := overlay.hit(msg.X, msg.Y); ok && c.ID == pressed {
				m.pickSelect(c)
			}
		}
		return true
	case tea.MouseWheelMsg:
		overlay := m.selectOverlay()
		if overlay == nil || !overlay.contains(msg.X, msg.Y) {
			m.closeSelect()
			return false // The form handles scrolling after the list closes.
		}
		switch msg.Button {
		case tea.MouseWheelUp:
			m.positionSelect(s.Highlight - 1)
		case tea.MouseWheelDown:
			m.positionSelect(s.Highlight + 1)
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "pgup", "pgdown", "ctrl+n", "ctrl+t":
			m.closeSelect()
			return false
		case "left", "right":
			return true // Cycling applies only to a closed select.
		case "up":
			m.positionSelect(s.Highlight - 1)
		case "down":
			m.positionSelect(s.Highlight + 1)
		case "home":
			m.positionSelect(0)
		case "end":
			m.positionSelect(len(m.selectOptions()) - 1)
		case "enter":
			opts := m.selectOptions()
			if len(opts) > 0 {
				m.pickSelect(control{Node: s.Node, Field: s.Field, Input: map[string]any{"value": opts[s.Highlight].Value}})
			}
			return true
		case "esc":
			if s.Filter == "" {
				m.closeSelect()
				return true
			}
			s.Filter = ""
			queryChanged = true
		case "backspace":
			r := []rune(s.Filter)
			if len(r) > 0 {
				s.Filter = string(r[:len(r)-1])
			}
			queryChanged = true
		case "space", " ":
			s.Filter += " "
			queryChanged = true
		default:
			if msg.Text == "" {
				return false
			}
			s.Filter += msg.Text
			queryChanged = true
		}
	case tea.PasteMsg:
		s.Filter += singleLine(msg.Content)
		queryChanged = true
	default:
		return false
	}
	if queryChanged {
		m.positionSelect(0)
	}
	return true
}

// selectGeometry uses the closed form to keep opening and filtering from moving fields.
func (m *model) selectGeometry() (x, y, width, capacity int) {
	if m.selectList == nil {
		return
	}
	first, last, fieldWidth := -1, -1, 0
	for i, r := range m.screenRows() {
		if r.Control != nil && r.Control.ID == m.selectList.controlID() {
			if first < 0 {
				first = i
			}
			last = i
			fieldWidth = r.Control.HitWidth
			x = m.leftMargin() + r.Control.Left
		}
	}
	if first < 0 {
		return
	}
	field := m.selectField(m.selectList.Node, m.selectList.Field)
	if field == nil {
		return
	}
	width = fieldWidth
	for _, option := range field.Options {
		width = max(width, ansi.StringWidth(singleLine(option.Label))+2)
	}
	width = min(width+2*selectPadding+2, m.width-x)
	// Use all options to keep the origin stable while the filter changes.
	count := len(field.Options)
	capacity = min(selectWindow, max(1, count))
	// The border and two padding rows need four rows. A partial window also needs a count line.
	height := capacity + 4
	if count > capacity {
		height++
	}
	below, above := m.height-last-1, first
	y = last + 1
	if height > below {
		if height <= above {
			y = first - height
		} else {
			capacity = min(capacity, max(3, max(above, below)-5))
			height = capacity + 4
			if count > capacity {
				height++
			}
			if above > below {
				y = first - height
			}
		}
	}
	// Extremely short panes cannot fit three options beside the field. Keep the layer in the pane.
	y = max(0, min(y, m.height-height))
	return
}
func (m *model) selectCapacity() int {
	_, _, _, capacity := m.selectGeometry()
	return max(1, capacity)
}
func (m *model) selectRange() (int, int) {
	s, count, capacity := m.selectList, len(m.selectOptions()), m.selectCapacity()
	offset := max(0, min(s.Offset, max(0, count-capacity)))
	if s.Highlight < offset {
		offset = s.Highlight
	}
	if s.Highlight >= offset+capacity {
		offset = s.Highlight - capacity + 1
	}
	return offset, min(count, offset+capacity)
}

type selectOverlay struct {
	X, Y, Width int
	Rows        []row
}

func (o *selectOverlay) layer() *lipgloss.Layer {
	lines := make([]string, len(o.Rows))
	for i, r := range o.Rows {
		lines[i] = r.Text
	}
	return lipgloss.NewLayer(strings.Join(lines, "\n")).X(o.X).Y(o.Y)
}
func (o *selectOverlay) contains(x, y int) bool {
	return x >= o.X && x < o.X+o.Width && y >= o.Y && y < o.Y+len(o.Rows)
}
func (o *selectOverlay) hit(x, y int) (control, bool) {
	if o.contains(x, y) && x > o.X && x < o.X+o.Width-1 {
		if c := o.Rows[y-o.Y].Control; c != nil {
			return *c, true
		}
	}
	return control{}, false
}
func (m *model) selectOverlay() *selectOverlay {
	s := m.selectList
	if s == nil {
		return nil
	}
	x, y, width, _ := m.selectGeometry()
	if width == 0 {
		return nil
	}
	f := m.selectField(s.Node, s.Field)
	if f == nil {
		return nil
	}
	return &selectOverlay{X: x, Y: y, Width: width, Rows: m.selectRows(s.Node, *f)}
}
func (m *model) selectRows(node string, field Field) []row {
	s := m.selectList
	if s == nil || s.Node != node || s.Field != field.ID {
		return nil
	}
	_, _, outerWidth, _ := m.selectGeometry()
	width := outerWidth - 2
	p := m.palette()
	base := lipgloss.NewStyle().Background(p.surface).Foreground(p.text)
	border := base.Foreground(p.border)
	rows := []row{{Text: border.Render("╭" + strings.Repeat("─", width) + "╮")}}
	add := func(text, anchor string, c *control, style lipgloss.Style) {
		text = ansi.Truncate(singleLine(text), max(1, width-2*selectPadding), "…")
		text = style.Width(width).Render(strings.Repeat(" ", selectPadding) + text + strings.Repeat(" ", selectPadding))
		rows = append(rows, row{Text: border.Render("│") + text + border.Render("│"), Anchor: anchor, Control: c})
	}
	info := func(text string) { add(text, "", nil, base.Foreground(p.muted)) }
	filter := ""
	if s.Filter != "" {
		filter = "Filter: " + s.Filter
	}
	info(filter)
	opts := m.selectOptions()
	start, end := m.selectRange()
	if len(opts) == 0 {
		info("No matching options")
	}
	for i := start; i < end; i++ {
		o := opts[i]
		id := fmt.Sprintf("%s%d", s.optionPrefix(), i)
		c := &control{ID: id, Node: node, Field: field.ID, Kind: "select_option", Input: map[string]any{"value": o.Value}}
		mark := "  "
		if m.editors[editorID(node, field.ID)].Choice == o.Value {
			mark = "✓ "
		}
		style := base
		if i == s.Highlight {
			style = style.Background(p.accent).Foreground(p.surface)
		}
		add(mark+o.Label, id, c, style)
	}
	info("")
	var hints []string
	if start > 0 {
		hints = append(hints, fmt.Sprintf("%d more above", start))
	}
	if end < len(opts) {
		hints = append(hints, fmt.Sprintf("%d more below", len(opts)-end))
	}
	if len(hints) > 0 {
		hint := strings.Join(hints, " · ")
		if ansi.StringWidth(hint) > width-2*selectPadding {
			hints = nil
			if start > 0 {
				hints = append(hints, fmt.Sprintf("↑ %d", start))
			}
			if end < len(opts) {
				hints = append(hints, fmt.Sprintf("↓ %d", len(opts)-end))
			}
			hint = strings.Join(hints, " · ")
		}
		info(hint)
	}
	rows = append(rows, row{Text: border.Render("╰" + strings.Repeat("─", width) + "╯")})
	return rows
}

// Pointer motion changes only the candidate value. A cached overlay avoids form layout work.
func (m *model) moveSelectPointer(overlay *selectOverlay, x, y int) bool {
	m.pointerX, m.pointerY, m.pointerKnown = x, y, true
	if overlay == nil || m.selectList == nil {
		return false
	}
	c, ok := overlay.hit(x, y)
	if !ok {
		return false
	}
	value, ok := c.Input["value"].(string)
	if !ok || value == m.selectList.Value {
		return false
	}
	for index, option := range m.selectOptions() {
		if option.Value == value {
			m.selectList.Highlight, m.selectList.Value = index, value
			return true
		}
	}
	return false
}
