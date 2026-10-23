package main

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type replacementState struct {
	ID        string `json:"replacement_id"`
	State     string `json:"state"`
	NextTitle string `json:"next_title"`
}
type panelReplacement struct {
	ID string
	replacementState
	Discard       bool
	PreviousFocus string
	Error         string
}
type replacementRequested struct {
	Action, ID, Title string
	valid             func() bool
	reply             func(string) tea.Cmd
}
type replacementRecovered struct {
	ID      string
	State   replacementState
	Err     error
	Initial bool
	// FromAttach: the state came in an attach reply. A view.resume can reach the renderer before
	// that reply is applied (and is then lost), so a freeze from it reads the durable state once.
	FromAttach bool
}

// replacementRead is the durable state of one replacement (hub.view.replacement), read with
// the attachment Attachment. State nil: the hub has no such replacement for this view.
type replacementRead struct {
	ID, Attachment string
	State          *replacementState
	Err            error
}

func (m *model) isPanel() bool {
	route := m.snapshot.Route
	if route == "" && m.client != nil {
		route = m.client.Route
	}
	return strings.HasPrefix(route, "/panels/")
}

func (m *model) setReplacementID(id string) {
	m.client.mu.Lock()
	m.client.replacementID = id
	m.client.mu.Unlock()
}

// Replacement waits for the model to freeze input before the client sends its answer.
func (m *model) updateReplacement(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case replacementRequested:
		if msg.ID == "" || !msg.valid() {
			return true, msg.reply("refused")
		}
		if msg.Action == "view.resume" {
			m.endReplacement(msg.ID)
			return true, msg.reply("resumed")
		}
		if m.endedReplacements[msg.ID] {
			// The resume of this replacement came first (Rex does not order calls from different
			// connections): it ended, and it never freezes this renderer again.
			return true, msg.reply("cancelled")
		}
		if m.replacement != nil {
			if m.replacement.ID != msg.ID {
				return true, msg.reply("conflict")
			}
			if m.replacement.State == "releasing" {
				m.replacement.State = "opening"
				if m.replacementUnsafe() {
					m.replacement.State = "confirming"
				}
			}
			if m.replacement.State == "confirming" {
				return true, msg.reply("confirm")
			}
			return true, msg.reply("released")
		}
		if m.closePending || m.closeRunning || m.reloadPending {
			return true, msg.reply("conflict")
		}
		state, answer := "opening", "released"
		if m.replacementUnsafe() {
			state, answer = "confirming", "confirm"
		}
		m.replacement = &panelReplacement{ID: msg.ID, replacementState: replacementState{State: state, NextTitle: msg.Title}, PreviousFocus: m.focus}
		m.pressed, m.hover, m.scrollDragging = "", "", false
		m.setReplacementID(msg.ID)
		return true, msg.reply(answer)
	case replacementRecovered:
		if m.endedReplacements[msg.ID] {
			return true, nil // a late result cannot undo the end
		}
		if msg.Err == nil && (msg.State.State == "cancelled" || msg.State.State == "superseded") {
			m.endReplacement(msg.ID)
			return true, nil
		}
		if m.replacement == nil && msg.Initial && msg.ID != "" {
			m.replacement = &panelReplacement{ID: msg.ID, PreviousFocus: m.focus}
			m.setReplacementID(msg.ID)
		}
		if m.replacement == nil || m.replacement.ID != msg.ID {
			return true, nil
		}
		if msg.Err != nil {
			m.replacement.Error = "The hub has not confirmed the choice. Waiting for reconnection."
			return true, nil
		}
		m.replacement.replacementState = msg.State
		m.replacement.Error = ""
		if msg.FromAttach && frozenState(msg.State.State) {
			return true, m.readReplacement(msg.ID)
		}
		return true, nil
	case replacementRead:
		// An ended state ends the replacement, also one that is not active (row 7).
		if msg.Err == nil && msg.State != nil && (msg.State.State == "cancelled" || msg.State.State == "superseded") {
			m.endReplacement(msg.ID)
			return true, nil
		}
		if m.endedReplacements[msg.ID] {
			return true, nil
		}
		// Only the replacement that froze this renderer, read through the current attachment: a
		// late answer after a resume or a new attachment changes nothing.
		if m.replacement == nil || m.replacement.ID != msg.ID || m.currentAttachment() != msg.Attachment {
			return true, nil
		}
		switch {
		case msg.Err != nil:
			// Recover as after a lost release_confirm answer: attach again, and the attach reply
			// reads the state again (it is FromAttach).
			m.replacement.Error = "The hub has not confirmed the replacement. Reconnecting."
			m.client.mu.Lock()
			m.client.lost = true
			m.client.mu.Unlock()
			m.client.pulse()
		case msg.State == nil:
			m.resumeReplacement()
		default:
			m.replacement.replacementState = *msg.State
			m.replacement.Error = ""
		}
		return true, nil
	}
	if m.replacement == nil {
		return false, nil
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+q":
			return true, m.closePanel()
		case "esc":
			if m.replacement.State == "confirming" {
				return true, m.chooseReplacement("keep")
			}
			return true, m.closePanel()
		case "tab", "shift+tab", "left", "right":
			if m.replacement.State == "confirming" {
				m.replacement.Discard = !m.replacement.Discard
			}
		case "enter":
			if m.replacement.State == "confirming" {
				if m.replacement.Discard {
					return true, m.chooseReplacement("discard")
				}
				return true, m.chooseReplacement("keep")
			}
		}
		return true, nil
	case tea.PasteMsg, tea.MouseMotionMsg, tea.MouseWheelMsg:
		return true, nil
	case tea.MouseClickMsg:
		m.pressed = ""
		if msg.Button == tea.MouseLeft {
			if c, ok := m.hitRows(m.visibleRows, msg.X, msg.Y); ok {
				m.pressed = c.ID
			}
		}
		return true, nil
	case tea.MouseReleaseMsg:
		pressed := m.pressed
		m.pressed = ""
		if msg.Button == tea.MouseLeft {
			if c, ok := m.hitRows(m.visibleRows, msg.X, msg.Y); ok && c.ID == pressed {
				return true, m.chooseReplacement(c.Kind)
			}
		}
		return true, nil
	}
	return false, nil
}

func (m *model) replacementBlocked() bool {
	if !m.ready || len(m.operations) > 0 || m.afterSave != nil {
		return true
	}
	for _, e := range m.editors {
		if e.Conflict || e.Pending != "" || e.WantSend || e.Sending {
			return true
		}
	}
	return false
}
func (m *model) replacementUnsafe() bool {
	if m.replacementBlocked() || len(m.recovery) > 0 {
		return true
	}
	for _, e := range m.editors {
		if e.dirty() {
			return true
		}
	}
	return false
}

// frozenState reports a replacement state in which the renderer waits for the hub.
func frozenState(state string) bool {
	return state == "releasing" || state == "opening" || state == "closing"
}

func (m *model) currentAttachment() string {
	m.client.mu.Lock()
	defer m.client.mu.Unlock()
	return m.client.attachment
}

// readReplacement reads the durable state of replacement id once (hub.view.replacement).
func (m *model) readReplacement(id string) tea.Cmd {
	attachment := m.currentAttachment()
	return func() tea.Msg {
		var out struct {
			Replacement *replacementState `json:"replacement"`
		}
		err := m.client.call(context.Background(), "hub.view.replacement", map[string]any{"replacement_id": id}, &out)
		if err == nil && out.Replacement != nil && out.Replacement.ID != id {
			err = errors.New("the hub answered for another replacement")
		}
		return replacementRead{ID: id, Attachment: attachment, State: out.Replacement, Err: err}
	}
}

// endReplacement records that replacement id ended without the slot (a resume, a cancel or a
// supersede), and unfreezes the renderer if id froze it. Another active replacement stays. The
// set is never evicted: it gets one ID per replacement of this view, and an eviction would let a
// late release of an evicted ID freeze the renderer again.
func (m *model) endReplacement(id string) {
	if id == "" {
		return
	}
	if m.endedReplacements == nil {
		m.endedReplacements = map[string]bool{}
	}
	m.endedReplacements[id] = true
	if m.replacement != nil && m.replacement.ID == id {
		m.resumeReplacement()
	}
}

func (m *model) resumeReplacement() {
	m.focus = m.replacement.PreviousFocus
	m.replacement = nil
	m.setReplacementID("")
	m.status = ""
}
func (m *model) chooseReplacement(choice string) tea.Cmd {
	r := m.replacement
	if r == nil || r.State != "confirming" || (choice == "discard" && m.replacementBlocked()) {
		return nil
	}
	if choice != "keep" && choice != "discard" {
		return nil
	}
	r.State = "deciding"
	id := r.ID
	return func() tea.Msg {
		var out struct {
			Replacement replacementState `json:"replacement"`
		}
		err := m.client.call(context.Background(), "hub.view.release_confirm", map[string]any{"replacement_id": id, "choice": choice}, &out)
		if err != nil {
			// Reattach reads the durable choice after a lost answer. Do not infer cancellation.
			m.client.mu.Lock()
			m.client.lost = true
			m.client.mu.Unlock()
			m.client.pulse()
		}
		return replacementRecovered{ID: id, State: out.Replacement, Err: err}
	}
}
func (m *model) closePanel() tea.Cmd {
	if m.closeRunning {
		return nil
	}
	if m.replacementBlocked() {
		m.status = "Close waits for operation results, draft conflicts, and the hub connection."
		return nil
	}
	m.closeRunning = true
	return func() tea.Msg { return closeFinished{err: m.client.closeView()} }
}

func (m *model) replacementView() tea.View {
	r := m.replacement
	p := m.palette()
	rows := []row{{}, {Text: "Opening " + singleLine(r.NextTitle) + "…"}, {}}
	if r.State == "confirming" {
		rows[1].Text = "Replace this panel with " + singleLine(r.NextTitle) + "?"
		rows = append(rows, row{Text: "This panel has unsaved input or an unfinished operation."}, row{})
		for _, choice := range []struct{ id, title string }{{"keep", "Keep editing"}, {"discard", "Discard and replace"}} {
			selected := (choice.id == "discard") == r.Discard
			label := "  " + choice.title
			if selected {
				label = "> " + choice.title
			}
			style := lipgloss.NewStyle().Foreground(p.text)
			disabled := choice.id == "discard" && m.replacementBlocked()
			if disabled {
				style = style.Foreground(p.muted)
				label += " (unavailable)"
			} else if selected {
				style = style.Foreground(p.accent)
			}
			item := row{Text: style.Render(label)}
			if !disabled {
				item.Control = &control{ID: "$replace-" + choice.id, Kind: choice.id, HitWidth: lipgloss.Width(label)}
			}
			rows = append(rows, item)
		}
		rows = append(rows, row{}, row{Text: "Tab selects · Enter confirms · Esc keeps editing"})
	} else {
		rows = append(rows, row{Text: "Waiting for the hub. Esc or Ctrl+Q closes this panel."})
	}
	if r.Error != "" {
		rows = append(rows, row{Text: r.Error})
	}
	if m.closeRunning {
		rows = append(rows, row{Text: "Closing the panel…"})
	}
	m.visibleRows = nil
	var lines []string
	for _, item := range rows {
		for _, line := range strings.Split(ansi.Wrap(item.Text, m.contentWidth(), ""), "\n") {
			visible := item
			visible.Text = line
			m.visibleRows = append(m.visibleRows, visible)
			lines = append(lines, strings.Repeat(" ", m.leftMargin())+line)
		}
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	// A thread pane stays on the primary screen (the mode is decided once).
	v.ReportFocus = true
	if !m.inline() {
		v.AltScreen, v.MouseMode = true, tea.MouseModeAllMotion
	}
	return v
}
