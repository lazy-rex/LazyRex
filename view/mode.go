package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The renderer decides its mode once per process (thread view v1.2, item 1):
//   - neutral: before the first tree with content. Loading and error nodes do not decide. The
//     renderer draws inline and prints nothing;
//   - thread: the first tree with a history node, no list or form node, and a layout other than
//     "transcript". The primary screen for the life of the process;
//   - alt: the first tree with any other content, a history node beside a list or form, or a
//     paired composer (layout "transcript"). The alt screen, as before v1.2.
type viewMode int

const (
	viewNeutral viewMode = iota
	viewThread
	viewAlt
)

// altClearDelay is the wait between the empty inline frame and the alt screen. Bubble Tea v2.1.0
// enters the alt screen without erasing the inline rows, and it has no frame-written signal, so
// this is a timing hack and best effort: on a slow flush the neutral rows stay on the primary
// screen after the exit. They are cosmetic and never hold a printed message. Do not use this
// pattern for anything that holds a message (view/README.md, "Known limits").
const altClearDelay = 50 * time.Millisecond

// The fixed status notes of the two defect cases (v1.2 items 1c2 and 1g). Each shows once per
// process.
const (
	noteHistoryBesideListForm = "view defect: history beside a list or form"
	noteHistoryInAlt          = "view defect: history in alt mode"
)

// threadLiveRows is the bound of the composer and the other nodes beside a history node.
const threadLiveRows = 8

type altClear struct{}

// decideViewMode reads the complete tree. It returns viewNeutral when the tree does not decide, and
// the defect note when the tree is a history node beside a list or form.
func decideViewMode(s Snapshot) (viewMode, string) {
	history, listOrForm, content := false, false, false
	for _, n := range s.Tree.Nodes {
		switch n.Type {
		case "loading", "error":
		case "history":
			history = true
		case "list", "form":
			listOrForm, content = true, true
		default:
			content = true
		}
	}
	switch {
	case history && listOrForm:
		return viewAlt, noteHistoryBesideListForm
	case history && s.Layout == "transcript":
		return viewAlt, ""
	case history:
		return viewThread, ""
	case content:
		return viewAlt, ""
	}
	return viewNeutral, ""
}

func treeHasHistory(s Snapshot) bool {
	for _, n := range s.Tree.Nodes {
		if n.Type == "history" {
			return true
		}
	}
	return false
}

// decide sets the mode from the current tree while the renderer is neutral, and shows a defect
// note once per process. When an inline frame was drawn, the switch to alt waits for one empty
// inline frame (altPending; Update starts the altClearDelay timer).
func (m *model) decide() {
	note := ""
	if m.mode == viewNeutral {
		mode, decided := decideViewMode(m.snapshot)
		m.mode, note = mode, decided
		m.altPending = mode == viewAlt && m.haveView && !m.lastView.AltScreen
		if mode != viewNeutral {
			m.client.decideSeen(mode == viewThread) // seen stays closed until the decision
		}
	} else if m.mode == viewAlt && treeHasHistory(m.snapshot) && m.snapshot.Layout != "transcript" {
		note = noteHistoryInAlt
	}
	if note != "" && !m.defectNoted {
		m.defectNoted = true
		m.status = note
	}
}

// clearBeforeAlt starts the wait before alt-on when apply has just set altPending.
func (m *model) clearBeforeAlt(wasPending bool) tea.Cmd {
	if !m.altPending || wasPending {
		return nil
	}
	return tea.Tick(altClearDelay, func(time.Time) tea.Msg { return altClear{} })
}

// quit ends the program after one empty frame, so no live row stays on the primary screen.
func (m *model) quit() tea.Cmd {
	m.quitting = true
	return tea.Quit
}

// inline reports a frame on the primary screen: neutral, thread, or the wait before alt-on.
func (m *model) inline() bool { return m.mode != viewAlt || m.altPending }

// liveRows returns the live region of an inline frame and the status line. The composer and the
// other nodes get the pane height minus 2 rows, and at most threadLiveRows beside a history node,
// so the whole frame is at most the pane height minus 1 (v1.2 item 1f). Text is cut first; when
// the controls alone do not fit, the rows become a window that holds the focused control.
func (m *model) liveRows() []row {
	bound := max(3, m.height-2)
	if treeHasHistory(m.snapshot) {
		bound = min(threadLiveRows, bound)
	}
	m.liveEditor = 1
	body := m.bodyRows()
	var composer []row
	if id := m.dockedComposer(); id != "" {
		chrome := 4 // the space row, the borders and the footer
		if m.contentWidth() < 60 {
			chrome = 5
		}
		m.liveEditor = max(1, bound-len(body)-chrome)
		composer = m.composerRows()
	}
	body = trimBlank(body)
	rows := append(cutText(body, max(0, bound-len(composer))), composer...)
	rows = m.focusWindow(rows, bound)
	p := m.palette()
	status := m.notice()
	if status == "" {
		status = m.inlineHelp()
	}
	return append(rows, row{Text: lipgloss.NewStyle().Foreground(p.muted).Render(fitHints(singleLine(status), m.contentWidth()))})
}

// trimBlank drops blank rows without a control at the end.
func trimBlank(rows []row) []row {
	for len(rows) > 0 && strings.TrimSpace(ansi.Strip(rows[len(rows)-1].Text)) == "" && len(rows[len(rows)-1].rowControls()) == 0 {
		rows = rows[:len(rows)-1]
	}
	return rows
}

func moreRow(text string) row {
	return row{Text: text, Anchor: "$more:" + text}
}

// cutText shortens blocks of text rows (rows without a control) until rows fit in bound, from the
// last block back. A cut block keeps its first rows and ends in "… <n> more lines". A row with a
// control is never removed.
func cutText(rows []row, bound int) []row {
	need := len(rows) - bound
	if need <= 0 {
		return rows
	}
	type block struct{ start, end int }
	var blocks []block
	for i := 0; i < len(rows); {
		if len(rows[i].rowControls()) > 0 {
			i++
			continue
		}
		j := i
		for j < len(rows) && len(rows[j].rowControls()) == 0 {
			j++
		}
		blocks = append(blocks, block{i, j})
		i = j
	}
	keep := map[int]int{} // block start -> rows kept before the marker
	for b := len(blocks) - 1; b >= 0 && need > 0; b-- {
		size := blocks[b].end - blocks[b].start
		gain := min(need, size-1) // the marker takes one row
		if gain <= 0 {
			continue
		}
		keep[blocks[b].start] = size - 1 - gain
		need -= gain
	}
	var out []row
	for i := 0; i < len(rows); {
		k, cut := keep[i]
		if !cut {
			out = append(out, rows[i])
			i++
			continue
		}
		end := i
		for end < len(rows) && len(rows[end].rowControls()) == 0 {
			end++
		}
		out = append(out, rows[i:i+k]...)
		out = append(out, moreRow(fmt.Sprintf("… %d more lines", end-i-k)))
		i = end
	}
	return out
}

// focusWindow returns rows unchanged when they fit. Otherwise it returns bound rows: a window that
// holds the focused control's rows, with "… <n> more above" and "… <n> more below" edge rows.
func (m *model) focusWindow(rows []row, bound int) []row {
	if len(rows) <= bound {
		m.liveOffset = 0
		return rows
	}
	h := bound - 2
	// A composer's editor and Send are one span: the window holds them together.
	composer := func(c control) bool { return c.Field == "" && (c.Kind == "edit" || c.Kind == "send") }
	focusNode := ""
	for _, r := range rows {
		for _, c := range r.rowControls() {
			if c.ID == m.focus && composer(c) {
				focusNode = c.Node
			}
		}
	}
	first, last := -1, -1
	for i, r := range rows {
		for _, c := range r.rowControls() {
			if c.ID == m.focus || focusNode != "" && c.Node == focusNode && composer(c) {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
	}
	if first >= 0 {
		if first < m.liveOffset {
			m.liveOffset = first
		}
		if last >= m.liveOffset+h {
			m.liveOffset = max(first, last-h+1)
		}
	}
	m.liveOffset = max(0, min(m.liveOffset, len(rows)-h))
	above, below := m.liveOffset, len(rows)-m.liveOffset-h
	out := []row{{}}
	if above > 0 {
		out[0] = moreRow(fmt.Sprintf("… %d more above", above))
	}
	out = append(out, rows[m.liveOffset:m.liveOffset+h]...)
	if below > 0 {
		out = append(out, moreRow(fmt.Sprintf("… %d more below", below)))
	} else {
		out = append(out, row{})
	}
	return out
}

// inlineHelp is the key hint line of an inline frame. The terminal owns the scrollback, so there
// is no scroll, mouse or text-selection hint.
func (m *model) inlineHelp() string {
	help := "Tab next · Ctrl+Q quit"
	for _, n := range m.snapshot.Tree.Nodes {
		if n.Type == "menu" {
			help = "Ctrl+O " + n.Label + " · " + help
			break
		}
	}
	return help
}

// inlineView draws the live region on the primary screen, without mouse reporting.
func (m *model) inlineView() tea.View {
	if m.altPending {
		m.visibleRows = nil
		v := tea.NewView("")
		// The wait before alt-on keeps focus reporting and the title, so they do not flicker.
		v.ReportFocus = true
		v.WindowTitle = singleLine(m.snapshot.Tree.Title)
		return v
	}
	m.visibleRows = m.liveRows()
	lines := make([]string, len(m.visibleRows))
	for i, r := range m.visibleRows {
		lines[i] = strings.Repeat(" ", m.leftMargin()) + ansi.Truncate(r.Text, m.contentWidth(), "")
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.ReportFocus = true
	v.WindowTitle = singleLine(m.snapshot.Tree.Title)
	m.lastView, m.haveView = v, true
	return v
}
