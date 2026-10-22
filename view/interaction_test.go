package main

import (
	tea "charm.land/bubbletea/v2"
	"strings"
	"testing"
)

func TestScrollbarDragAndTrack(t *testing.T) {
	m := testModel()
	m.apply(conversationSnapshot(20))
	m.scroll = 0
	h, thumb, _, limit := m.scrollbar()
	x := m.leftMargin() + m.contentWidth() + 1
	if !m.pressScrollbar(x, headerRows) {
		t.Fatal("Thumb did not accept press")
	}
	m.Update(tea.MouseMotionMsg{X: x, Y: headerRows + h - thumb})
	if m.scroll != limit {
		t.Fatalf("Drag offset %d, want %d", m.scroll, limit)
	}
	m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: x, Y: headerRows + h - thumb})
	if m.scrollDragging {
		t.Fatal("Release kept drag active")
	}
	if !m.pressScrollbar(x, headerRows) || m.scroll != 0 {
		t.Fatal("Track click did not reach top")
	}
}
func TestEditorWheelPreservesTextAndCursor(t *testing.T) {
	m := testModel()
	m.apply(conversationSnapshot(20))
	e := m.editors["composer"]
	text := strings.Repeat("draft line\n", 30) + "last"
	e.setText(text)
	e.Area.CursorEnd()
	if e.Area.Height() <= m.editorHeight(e) {
		t.Fatal("Long draft did not grow")
	}
	before := e.Area.Line()
	history := m.scroll
	y := -1
	for i, r := range m.screenRows() {
		if r.Control != nil && r.Control.Kind == "edit" {
			y = i
			break
		}
	}
	if y < 0 {
		t.Fatal("No editor hit region")
	}
	offset := m.editorOffset(e)
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: m.leftMargin() + 2, Y: y})
	if e.Scroll >= offset || !e.Reviewing {
		t.Fatal("Wheel did not scroll editor")
	}
	if e.text() != text || e.Area.Line() != before || m.scroll != history {
		t.Fatal("Editor wheel changed text, cursor, or history")
	}
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: m.leftMargin() + 2, Y: 4})
	if m.scroll >= history {
		t.Fatal("History wheel did not scroll history")
	}
}
func TestHoverDoesNotChangeFocusAndDragOutCancels(t *testing.T) {
	m := testModel()
	m.apply(conversationSnapshot(1))
	m.editors["composer"].setText("draft")
	var target control
	y := -1
	for i, r := range m.screenRows() {
		if r.Control != nil && r.Control.Kind == "send" {
			target = *r.Control
			y = i
			break
		}
	}
	x := m.leftMargin() + target.Left
	focus := m.focus
	m.Update(tea.MouseMotionMsg{X: x, Y: y})
	if m.hover != target.ID || m.focus != focus {
		t.Fatal("Hover changed focus or missed button")
	}
	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y})
	m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: 0, Y: 0})
	if m.editors["composer"].Sending || m.editors["composer"].WantSend {
		t.Fatal("Drag out sent draft")
	}
	if _, ok := m.hitControl(x-1, y); ok {
		t.Fatal("Hint outside Send is active")
	}
}
func TestRoutineStatusDoesNotChangeLayout(t *testing.T) {
	m := testModel()
	m.apply(conversationSnapshot(2))
	m.status = "Connected"
	h := m.bodyHeight()
	for _, status := range []string{"Draft saved", "Sent", "Done"} {
		m.status = status
		if m.notice() != "" || m.bodyHeight() != h {
			t.Fatal("Routine status changed layout")
		}
	}
}

func TestMessageAlignmentAndGrouping(t *testing.T) {
	m := testModel()
	other := Message{Author: "Claude", At: "2026-10-16T21:00:00Z", Text: "Hello"}
	own := other
	own.Own = true
	left := m.messageLines(other, nil)
	right := m.messageLines(own, nil)
	if strings.HasPrefix(left[0], " ") || !strings.HasPrefix(right[0], " ") {
		t.Fatal("Message alignment does not follow ownership")
	}
	next := other
	next.At = "2026-10-16T21:01:00Z"
	if !grouped(&other, next) {
		t.Fatal("Adjacent author messages should group")
	}
	next.At = "2026-10-16T21:03:00Z"
	if grouped(&other, next) || grouped(&other, own) {
		t.Fatal("Grouping crossed time or ownership boundary")
	}
}

func TestScrollbarHoverUsesCachedFrame(t *testing.T) {
	m := testModel()
	m.apply(conversationSnapshot(20))
	m.scroll = 0
	normal := m.View().Content
	x := m.leftMargin() + m.contentWidth() + 1
	m.Update(tea.MouseMotionMsg{X: x, Y: headerRows})
	if m.hover != "$scrollbar-thumb" || m.reuseView {
		t.Fatal("Thumb entry did not invalidate frame")
	}
	hovered := m.View().Content
	if hovered == normal || !strings.Contains(hovered, "██") {
		t.Fatal("Thumb did not highlight and widen")
	}
	for i := 0; i < 300; i++ {
		m.Update(tea.MouseMotionMsg{X: x + i%2, Y: headerRows})
		if !m.reuseView {
			t.Fatal("Motion within thumb rebuilt frame")
		}
		m.View()
	}
	m.Update(tea.MouseMotionMsg{X: x, Y: headerRows + m.visibleScrollbar[1]})
	if m.hover != "$scrollbar-track" || m.reuseView {
		t.Fatal("Track entry did not change hover")
	}
	m.View()
	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x + 1, Y: headerRows})
	if !m.scrollDragging {
		t.Fatal("Expanded track did not accept press")
	}
	m.Update(tea.MouseMotionMsg{X: 0, Y: 5})
	if !strings.Contains(m.View().Content, "██") {
		t.Fatal("Drag lost thumb highlight outside track")
	}
	m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: 0, Y: 5})
	m.Update(tea.MouseMotionMsg{X: 0, Y: 0})
	if m.hover != "" {
		t.Fatal("Scrollbar hover remained after pointer left")
	}
	if strings.Contains(m.View().Content, "██") {
		t.Fatal("Thumb remained highlighted after release")
	}
}

func TestPaddedSendTargetsMatchVisibleRows(t *testing.T) {
	for _, width := range []int{38, 80, 200} {
		m := testModel()
		m.width = width
		m.apply(conversationSnapshot(2))
		m.editors["composer"].setText("draft")
		count := 0
		for y, r := range m.screenRows() {
			if r.Control == nil || r.Control.Kind != "send" {
				continue
			}
			count++
			c := *r.Control
			if c.HitWidth < 10 {
				t.Fatal("Send target is too narrow")
			}
			for x := 0; x < c.HitWidth; x++ {
				hit, ok := m.hitControl(m.leftMargin()+c.Left+x, y)
				if !ok || hit.ID != c.ID {
					t.Fatal("Visible Send padding is not clickable")
				}
			}
			if _, ok := m.hitControl(m.leftMargin()+c.Left-1, y); ok {
				t.Fatal("Send target includes adjacent hint")
			}
		}
		if count != 1 {
			t.Fatalf("Send target has %d rows, want 1", count)
		}
	}
}

func TestPaddedListAndChoices(t *testing.T) {
	m := testModel()
	s := testSnapshot("", "1")
	s.Tree.Nodes = []Node{{ID: "list", Type: "list", Items: []Item{{ID: "a", Label: "Agent", Detail: "Working", Action: &Action{Name: "open", Enabled: true}}, {ID: "b", Label: "Second agent", Detail: "Ready", Action: &Action{Name: "open", Enabled: true}}}}, {ID: "form", Type: "form", Fields: []Field{{ID: "member", Kind: "boolean", Label: "Include agent"}}, SubmitAction: &Action{Name: "create", Label: "Create thread", Enabled: true}}}
	m.apply(s)
	counts := map[string]int{}
	last := ""
	seen := map[string]bool{}
	for _, r := range m.bodyRows() {
		if r.Control == nil {
			continue
		}
		id := r.Control.ID
		if id != last {
			if seen[id] {
				t.Fatal("Target rows are not contiguous")
			}
			seen[id] = true
		}
		counts[id]++
		last = id
	}
	for _, id := range []string{"list:a", "list:b", "choice:form/member", "form:submit"} {
		want := 3
		if id == "form:submit" {
			want = 1 // a one-row Charm button
		}
		if strings.HasPrefix(id, "list:") {
			want = 4
		}
		if counts[id] != want {
			t.Fatalf("Target %s has only%d rows", id, counts[id])
		}
	}
}

func TestAdjacentListTargetsHaveDistinctOwnership(t *testing.T) {
	m := testModel()
	s := testSnapshot("", "1")
	s.Tree.Nodes = []Node{{ID: "list", Type: "list", Items: []Item{{ID: "a", Label: "First", Detail: "one", Action: &Action{Name: "open", Enabled: true}}, {ID: "b", Label: "Second", Detail: "two", Action: &Action{Name: "open", Enabled: true}}}}}
	m.apply(s)
	for offset := 0; offset < 8; offset++ {
		want := "list:a"
		if offset >= 4 {
			want = "list:b"
		}
		c, ok := m.hitControl(m.leftMargin()+3, headerRows+offset)
		if !ok || c.ID != want {
			t.Fatalf("Row%d target%q, want%q", offset, c.ID, want)
		}
	}
}

func TestFormShortcutFromComposer(t *testing.T) {
	m := testModel()
	s := conversationSnapshot(8)
	s.Tree.Nodes = append([]Node{{ID: "form", Type: "form", Label: "Form", Fields: []Field{{ID: "name", Kind: "text", Label: "Name"}}}}, s.Tree.Nodes...)
	m.apply(s)
	m.setFocus("edit:composer")
	m.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	if m.focus != "edit:form/name" {
		t.Fatalf("Ctrl+N focus=%q", m.focus)
	}
	m.Update(tea.KeyPressMsg{Code: 'A', Text: "Ada"})
	if m.editors["form/name"].text() != "Ada" || m.editors["composer"].text() != "" {
		t.Fatal("Typing did not stay in form field")
	}
}

func TestButtonHoverChangesTheButton(t *testing.T) {
	for _, dark := range []bool{false, true} {
		m := testModel()
		m.dark = dark
		m.apply(conversationSnapshot(2))
		m.editors["composer"].setText("draft")
		m.View()
		x, y := -1, -1
		for i, r := range m.visibleRows {
			if r.Control != nil && r.Control.Kind == "send" {
				x = m.leftMargin() + r.Control.Left
				y = i
				break
			}
		}
		if x < 0 {
			t.Fatal("Missing Send target")
		}
		before := m.buttonView("composer:send", "Send ↑", 10, true, true)
		m.Update(tea.MouseMotionMsg{X: x, Y: y})
		if m.hover != "composer:send" || m.reuseView {
			t.Fatal("Hover did not invalidate the Send frame")
		}
		after := m.buttonView("composer:send", "Send ↑", 10, true, true)
		if before == after {
			t.Fatal("The button did not change on hover")
		}
	}
}

func TestDisabledButtonIgnoresHoverAndPress(t *testing.T) {
	for _, dark := range []bool{false, true} {
		m := testModel()
		m.dark = dark
		before := m.buttonView("button", "Send", 14, true, false)
		m.hover, m.pressed, m.focus = "button", "button", "button"
		after := m.buttonView("button", "Send", 14, true, false)
		if before != after {
			t.Fatal("Disabled button changed on hover, press, or focus")
		}
		if !strings.Contains(before, "╱") || !strings.Contains(before, " Send ") {
			t.Fatal("Disabled button needs stripes and a clear label")
		}
	}
}
