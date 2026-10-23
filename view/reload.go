package main

import (
	"fmt"
	"reflect"

	tea "charm.land/bubbletea/v2"
)

type reloadRequested struct{}
type retireRequested struct{}

// closeRequested is the hub's view.close_request: the user closed the transcript of this pair,
// so the composer closes through the same gate as Ctrl+Q.
type closeRequested struct{}

func (e *editor) dirty() bool {
	if e.Kind == "boolean" || e.Kind == "select" {
		return !reflect.DeepEqual(e.Choice, e.SavedChoice)
	}
	return e.text() != e.Saved
}

func (e *editor) recoveryText() string {
	if e.Kind == "boolean" || e.Kind == "select" {
		return fmt.Sprint(e.Choice)
	}
	return e.text()
}

// text is the value of a text editor: the input of a field of kind text, otherwise the area.
func (e *editor) text() string {
	if e.Kind == "text" {
		return e.Input.Value()
	}
	return e.Area.Value()
}

// setText replaces the value. A field of kind text changes newlines and tabs to spaces.
func (e *editor) setText(s string) {
	if e.Kind == "text" {
		e.Input.SetValue(plain(s))
		return
	}
	e.Area.SetValue(plain(s))
}

// Update checks reload safety after each model change.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.reloadReady || m.retireReady || m.closeReady {
		return m, nil
	}
	// An inline frame turns mouse reporting off: the terminal owns the pointer and the scrollback.
	if m.inline() {
		switch msg.(type) {
		case tea.MouseMotionMsg, tea.MouseWheelMsg, tea.MouseClickMsg, tea.MouseReleaseMsg:
			return m, nil
		}
	}
	m.reuseView = false
	if _, motion := msg.(tea.MouseMotionMsg); !motion {
		m.visibleSelect = nil
	}
	if handled, cmd := m.updateReplacement(msg); handled {
		return m, cmd
	}
	if motion, ok := msg.(tea.MouseMotionMsg); ok && !m.scrollDragging && m.selectList != nil {
		overlay := m.visibleSelect
		if overlay == nil {
			overlay = m.selectOverlay()
		}
		cached := m.visibleSelect != nil
		changed := m.moveSelectPointer(overlay, motion.X, motion.Y)
		m.reuseView = m.haveView && cached && !changed
		if changed {
			m.visibleSelect = nil
		}
		return m, nil
	}
	// Pointer motion does not change layout unless it drags the scrollbar.
	if motion, ok := msg.(tea.MouseMotionMsg); ok && !m.scrollDragging && m.haveView && m.selectList == nil {
		m.pointerX, m.pointerY, m.pointerKnown = motion.X, motion.Y, true
		hover := ""
		if !m.selectText {
			if c, hit := m.hitRows(m.visibleRows, motion.X, motion.Y); hit {
				hover = c.ID
			}
		}
		if target := m.scrollbarTarget(motion.X, motion.Y); target != "" {
			hover = target
		}
		m.reuseView = hover == m.hover
		m.hover = hover
		return m, nil
	}
	// A body at its bottom stays at its bottom when its height changes. A snapshot is not part of
	// this rule: it replaces the rows, and apply keeps a history at its bottom itself. (A loading
	// tree fits, so it is at its bottom; a tall tree after it must start at its top.)
	beforeHeight := m.bodyHeight()
	_, snapshot := msg.(Snapshot)
	follow := !snapshot && m.scroll >= max(0, len(m.bodyRows())-beforeHeight)
	_, cmd := m.update(msg)
	// A source action can finish while its replacement waits for safe input.
	// Submitted values become saved; values typed after submission remain dirty.
	if m.replacement != nil && m.replacement.State == "confirming" && !m.replacementUnsafe() {
		return m, tea.Batch(cmd, m.chooseReplacement("discard"))
	}
	if follow && beforeHeight != m.bodyHeight() {
		m.scroll = max(0, len(m.bodyRows())-m.bodyHeight())
	}
	if _, resized := msg.(tea.WindowSizeMsg); resized && fieldChoiceFocus(m.focus) {
		m.revealFocus()
	}
	m.refreshHover()
	if m.replacement != nil || m.closeRunning || m.closeReady || (!m.reloadPending && !m.retirePending && !m.closePending) {
		return m, cmd
	}
	operation := "Reload"
	if m.closePending {
		operation = "Close"
	}
	if m.retirePending {
		operation = "Retirement"
	}
	if len(m.recovery) > 0 {
		m.status = operation + " waits: copy the text from removed controls, then quit and reopen this view."
		return m, cmd
	}
	for _, e := range m.editors {
		if e.Conflict {
			m.status = operation + " waits: resolve the draft conflict."
			return m, cmd
		}
		if e.Kind != "composer" && e.dirty() {
			m.status = operation + " waits: submit the form or restore its original values."
			return m, cmd
		}
	}
	var saves []tea.Cmd
	waiting := len(m.operations) > 0 || !m.ready
	for id, e := range m.editors {
		if e.dirty() || e.Pending != "" || e.WantSend || e.Sending {
			waiting = true
			if e.Kind == "composer" {
				saves = append(saves, m.save(id))
			}
		}
	}
	if waiting {
		m.status = operation + " waits for saved drafts, operation results, and the hub connection."
		return m, tea.Batch(append(saves, cmd)...)
	}
	if m.closePending {
		m.closePending, m.closeRunning = false, true
		m.status = "Closing the tab…"
		return m, func() tea.Msg { return closeFinished{err: m.client.closeView()} }
	}
	if m.retirePending {
		m.retireReady = true
		m.status = "Closing the migrated view…"
	} else {
		m.reloadReady = true
		m.status = "Reloading renderer…"
	}
	return m, m.quit()
}
