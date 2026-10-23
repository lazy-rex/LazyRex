package main

import (
	"fmt"
	"hash/fnv"
	"image/color"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// palette holds the semantic color roles. Every control color comes from a role, and the literal
// colors of the roles are only here. The control roles follow the shapes of huh's ThemeCharm
// (huh v2.0.3, theme.go), with colors chosen by the ui.sh guidance: highlight is fuchsia (the
// selector and the primary button), and its label contrast is at least 4.5:1 in every state and
// theme, where Charm's cream on fuchsia is about 2:1. Secondary buttons and borders use zinc
// neutrals. Dark mode uses one light title color. selected is emerald and danger is red.
type palette struct {
	text, muted, accent, border, surface color.Color
	// Form controls.
	title, highlight, onHighlight, selected, danger color.Color
	control                                         color.Color // the border of an idle control
	button                                          color.Color // the background of a secondary button
	hover, press                                    color.Color // a secondary control under the pointer
	highlightHover, highlightPress                  color.Color // a primary button under the pointer
}

func (m *model) palette() palette {
	if m.dark {
		return palette{
			text: lipgloss.Color("#E2E8F0"), muted: lipgloss.Color("#94A3B8"), accent: lipgloss.Color("#A5B4FC"), border: lipgloss.Color("#475569"), surface: lipgloss.Color("#1E293B"),
			title: lipgloss.Color("#F4F4F5"), highlight: lipgloss.Color("#E879F9"), onHighlight: lipgloss.Color("#09090B"), selected: lipgloss.Color("#34D399"), danger: lipgloss.Color("#F87171"),
			control: lipgloss.Color("#A1A1AA"), button: lipgloss.Color("#27272A"), hover: lipgloss.Color("#3F3F46"), press: lipgloss.Color("#52525B"),
			highlightHover: lipgloss.Color("#F0ABFC"), highlightPress: lipgloss.Color("#D946EF"),
		}
	}
	return palette{
		text: lipgloss.Color("#1E293B"), muted: lipgloss.Color("#526176"), accent: lipgloss.Color("#4338CA"), border: lipgloss.Color("#CBD5E1"), surface: lipgloss.Color("#EEF2FF"),
		title: lipgloss.Color("#4338CA"), highlight: lipgloss.Color("#C026D3"), onHighlight: lipgloss.Color("#FFFFFF"), selected: lipgloss.Color("#047857"), danger: lipgloss.Color("#DC2626"),
		control: lipgloss.Color("#71717A"), button: lipgloss.Color("#E4E4E7"), hover: lipgloss.Color("#D4D4D8"), press: lipgloss.Color("#A1A1AA"),
		highlightHover: lipgloss.Color("#A21CAF"), highlightPress: lipgloss.Color("#86198F"),
	}
}
func (m *model) contentWidth() int { return max(8, m.width-6) }
func (m *model) leftMargin() int   { return max(0, (m.width-m.contentWidth())/2) }
func (m *model) controlWidth() int { return min(72, m.contentWidth()) }
func (m *model) rows() []row       { return append(m.bodyRows(), m.composerRows()...) }
func (m *model) authorColor(author string) color.Color {
	light := []string{"#4338CA", "#0F766E", "#A23B16", "#9D174D", "#0369A1"}
	dark := []string{"#A5B4FC", "#5EEAD4", "#FDBA74", "#F9A8D4", "#7DD3FC"}
	h := fnv.New32a()
	_, _ = h.Write([]byte(author))
	i := int(h.Sum32()) % len(light)
	if m.dark {
		return lipgloss.Color(dark[i])
	}
	return lipgloss.Color(light[i])
}
func singleLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(plain(s), "\n", " "), "\t", " ")
}

func shortTime(value string) string {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return plain(value)
	}
	if t.Local().Format("2006-01-02") == time.Now().Format("2006-01-02") {
		return t.Local().Format("15:04")
	}
	return t.Local().Format("Jan 2 · 15:04")
}
func (m *model) messageBody(text string) string {
	width := max(4, min(100, m.contentWidth()*7/10)-4)
	key := fmt.Sprintf("%t/%d/%s", m.dark, width, text)
	if v, ok := m.markdown[key]; ok {
		return v
	}
	style := styles.LightStyleConfig
	if m.dark {
		style = styles.DarkStyleConfig
	}
	codeColor, codeBG := "#4338CA", "#EEF2FF"
	if m.dark {
		codeColor, codeBG = "#C7D2FE", "#1E293B"
	}
	textColor := "#1E293B"
	if m.dark {
		textColor = "#E2E8F0"
	}
	style.Document.Color = &textColor
	style.Code.Color = &codeColor
	style.Code.BackgroundColor = &codeBG
	zero := uint(0)
	style.Document.Margin = &zero
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	renderer, err := glamour.NewTermRenderer(glamour.WithStyles(style), glamour.WithWordWrap(width))
	output := plain(text)
	if err == nil {
		if result, e := renderer.Render(output); e == nil {
			output = strings.Trim(result, "\n")
		}
	}
	output = ansi.Hardwrap(output, width, true)
	if m.markdown == nil || len(m.markdown) > 256 {
		m.markdown = map[string]string{}
	}
	m.markdown[key] = output
	return output
}

func (m *model) bodyRows() []row {
	var rows []row
	p := m.palette()
	width := m.contentWidth()
	add := func(text, anchor string, c *control, extra ...control) {
		rendered := ansi.Hardwrap(text, width, true)
		for i, line := range strings.Split(rendered, "\n") {
			rows = append(rows, row{Text: line, Anchor: fmt.Sprintf("%s:%d", anchor, i), Control: c, Extra: extra})
		}
	}
	label := func(text, anchor string) {
		add(lipgloss.NewStyle().Foreground(p.muted).Render(plain(text)), anchor, nil)
	}
	button := func(text, anchor string, c control) {
		c.HitWidth = min(width, buttonWidth(plain(text)))
		add(m.buttonView(c.ID, text, c.HitWidth, m.primaryButton(c), true), anchor, &c)
	}
	// buttonRow puts buttons side by side, one cell apart (huh ThemeBase: MarginRight(1)). They
	// stack when they do not fit, with a blank row between them.
	buttonRow := func(texts []string, anchor string, cs []control) {
		const gap = 1
		total := 0
		for i := range cs {
			cs[i].HitWidth = buttonWidth(plain(texts[i]))
			cs[i].Left = total
			total += cs[i].HitWidth + gap
		}
		if total-gap > width {
			for i := range cs {
				if i > 0 {
					add("", fmt.Sprintf("%s:%d:gap", anchor, i), nil)
				}
				cs[i].Left = 0
				button(texts[i], fmt.Sprintf("%s:%d", anchor, i), cs[i])
			}
			return
		}
		line := ""
		for i := range cs {
			if i > 0 {
				line += strings.Repeat(" ", gap)
			}
			line += m.buttonView(cs[i].ID, texts[i], cs[i].HitWidth, m.primaryButton(cs[i]), true)
		}
		add(line, anchor, &cs[0], cs[1:]...)
	}

	if m.effectFailure != nil {
		label("The change was saved. "+m.effectFailure.Outcome.EffectsError, "effect-failure")
		button("Retry", "effect-retry", control{ID: "$effect-retry", Kind: "retry_effect"})
	}
	if b := m.snapshot.Tree.Back; b != nil && b.Enabled {
		button(b.Label, "back", control{ID: "back", Node: "$back", Action: b})
		add("", "back:space", nil)
	}
	nodes := m.snapshot.Tree.Nodes
	skip := map[int]bool{}
	for ni, n := range nodes {
		if skip[ni] {
			continue
		}
		if n.Type == "history" && m.mode != viewAlt {
			continue // a thread pane prints its history above the live region
		}
		if n.Type == "composer" {
			if m.dockedComposer() != n.ID {
				rows = append(rows, m.composerRowsFor([]Node{n})...)
			}
			continue
		}
		if n.Label != "" && n.Type == "list" && compactList(n) {
			add(lipgloss.NewStyle().Foreground(p.muted).Bold(true).Render(strings.ToUpper(plain(n.Label))), n.ID+":label", nil)
		} else if n.Label != "" && n.Type != "menu" {
			add(lipgloss.NewStyle().Foreground(p.text).Bold(true).Render(plain(n.Label)), n.ID+":label", nil)
			add("", n.ID+":space", nil)
		}
		switch n.Type {
		case "menu":
			// No button: Ctrl+O opens the menu in a side pane (the footer says so).
		case "history":
			// No Older messages button: a plugin offers older history from its menu.
			if m.composerOnly {
				continue // the transcript pane above prints the messages
			}
			if len(n.Messages) == 0 {
				add(lipgloss.NewStyle().Foreground(p.muted).Render("No messages yet. Start the conversation below."), n.ID+":empty", nil)
			}
			var previous *Message
			for i := range n.Messages {
				msg := n.Messages[i]
				if i > 0 {
					add("", n.ID+":"+msg.ID+":space", nil)
				}
				rows = append(rows, m.historyMessageRows(n.ID, i, msg, previous)...)
				previous = &n.Messages[i]
			}

		case "list":
			listWidth := m.controlWidth()
			if compactList(n) {
				rows = append(rows, m.compactListRows(n, listWidth)...)
				break
			}
			for _, item := range n.Items {
				var c *control
				if item.Action != nil && item.Action.Enabled {
					c = &control{HitWidth: listWidth, ID: n.ID + ":" + item.ID, Node: n.ID, Action: item.Action, Input: map[string]any{"item_id": item.ID}}
				}
				style := lipgloss.NewStyle().Foreground(p.text).Bold(true)
				if c == nil {
					style = style.Foreground(p.muted)
				}
				prefix := "  "
				if c != nil && c.ID == m.focus {
					prefix = lipgloss.NewStyle().Foreground(p.accent).Render("▏") + " "
				}
				if c != nil {
					style = m.controlStyle(c.ID, style)
				}
				add(style.Width(listWidth).Render(""), n.ID+":"+item.ID+":top", c)
				add(style.Width(listWidth).Render(ansi.Truncate(prefix+singleLine(item.Label), listWidth, "…")), n.ID+":"+item.ID, c)
				detailStyle := lipgloss.NewStyle().Foreground(p.muted).Width(listWidth)
				if c != nil {
					detailStyle = m.controlStyle(c.ID, detailStyle)
				}
				add(detailStyle.Render(ansi.Truncate(prefix+singleLine(item.Detail), listWidth, "…")), n.ID+":"+item.ID+":detail", c)
				if c != nil {
					add(style.Width(listWidth).Render(""), n.ID+":"+item.ID+":bottom", c)
				}
			}
		case "form":
			var visible []Field
			for _, f := range n.Fields {
				if m.fieldVisible(n.ID, f) {
					visible = append(visible, f)
				}
			}
			for _, f := range visible {
				id := editorID(n.ID, f.ID)
				e := m.editors[id]
				if e == nil {
					continue
				}
				fieldLabel := f.Label
				if f.Required {
					fieldLabel += " *"
				}
				// The field title of huh ThemeCharm: the title role, bold.
				add(lipgloss.NewStyle().Foreground(p.title).Bold(true).Render(plain(fieldLabel)), id+":label", nil)
				if f.Kind == "boolean" {
					// Yes and No are ordinary buttons. The chosen one has the primary style.
					buttonRow([]string{"Yes", "No"}, id, []control{
						{ID: "yes:" + id, Node: n.ID, Field: f.ID, Kind: "boolean", Input: map[string]any{"value": true}},
						{ID: "no:" + id, Node: n.ID, Field: f.ID, Kind: "boolean", Input: map[string]any{"value": false}},
					})
				} else if f.Kind == "select" {
					value := fmt.Sprint(e.Choice)
					for _, o := range f.Options {
						if o.Value == e.Choice {
							value = o.Label
						}
					}
					choiceControl := control{ID: "choice:" + id, Node: n.ID, Field: f.ID, Kind: "choice", HitWidth: m.controlWidth()}
					button(value+" ▾", id, choiceControl)
				} else if f.Kind == "text" {
					// huh's Input shape: one row with a bar and a prompt. Browse… sits beside it, or
					// under it in a narrow pane.
					c := &control{ID: "edit:" + id, Node: n.ID, Field: f.ID, Kind: "edit", HitWidth: m.controlWidth()}
					bw := browseWidth(f, m.controlWidth())
					if bw > 0 && browseBeside(m.controlWidth()) {
						c.HitWidth = m.controlWidth() - bw - 1
					}
					line := m.inputView(c.ID, e, c.HitWidth)
					if bw > 0 {
						bc := control{ID: "browse:" + id, Node: n.ID, Field: f.ID, Kind: "browse", HitWidth: bw}
						b := m.buttonView(bc.ID, "Browse…", bw, false, m.ready && len(m.operations) == 0)
						if browseBeside(m.controlWidth()) {
							bc.Left = c.HitWidth + 1
							add(line+" "+b, id, c, bc)
						} else {
							add(line, id, c)
							add(b, id+":browse", &bc)
						}
					} else {
						add(line, id, c)
					}
				} else {
					c := &control{ID: "edit:" + id, Node: n.ID, Field: f.ID, Kind: "edit", HitWidth: m.controlWidth()}
					border := p.control
					if m.focus == c.ID || m.hover == c.ID {
						border = p.accent
					}
					box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(0, 1).Render(m.editorView(e))
					if bw := browseWidth(f, m.controlWidth()); bw > 0 {
						// Browse… sits beside its text box, or under it in a narrow pane.
						bc := control{ID: "browse:" + id, Node: n.ID, Field: f.ID, Kind: "browse", HitWidth: bw}
						b := m.buttonView(bc.ID, "Browse…", bw, false, m.ready && len(m.operations) == 0)
						if browseBeside(m.controlWidth()) {
							// The button is on the first text row of the box. Only that row has its target.
							c.HitWidth = lipgloss.Width(box)
							bc.Left = c.HitWidth + 1
							for i, line := range strings.Split(box, "\n") {
								if i == 1 {
									add(line+" "+b, fmt.Sprintf("%s:%d", id, i), c, bc)
								} else {
									add(line, fmt.Sprintf("%s:%d", id, i), c)
								}
							}
						} else {
							add(box, id, c)
							add(b, id+":browse", &bc)
						}
					} else {
						add(box, id, c)
					}
				}
				// One blank line after each field.
				add("", id+":space", nil)
			}
			if n.SubmitAction != nil && n.SubmitAction.Enabled {
				// Plain button nodes after the form (for example Cancel) share the submit row.
				texts := []string{n.SubmitAction.Label}
				cs := []control{{ID: n.ID + ":submit", Node: n.ID, Kind: "submit", Action: n.SubmitAction}}
				for j := ni + 1; j < len(nodes) && bareButton(nodes[j]); j++ {
					texts = append(texts, nodes[j].Action.Label)
					cs = append(cs, control{ID: nodes[j].ID + ":action", Node: nodes[j].ID, Action: nodes[j].Action})
					skip[j] = true
				}
				buttonRow(texts, n.ID+":submit", cs)
			}
		case "widget":
			if strings.TrimSpace(n.Text) != "" {
				add(lipgloss.NewStyle().Foreground(p.text).Render(ansi.Wrap(plain(n.Text), width, "")), n.ID+":text", nil)
			}
			if n.Action != nil && n.Action.Enabled {
				button(n.Action.Label, n.ID+":action", control{ID: n.ID + ":action", Node: n.ID, Action: n.Action})
			}
		case "loading":
			add(lipgloss.NewStyle().Foreground(p.muted).Render("◌ "+plain(n.Text)), n.ID+":loading", nil)
		case "error":
			add(lipgloss.NewStyle().Foreground(p.text).Bold(true).Render(plain(n.Text)), n.ID+":error", nil)
			if n.RetryAction != nil && n.RetryAction.Enabled {
				button(n.RetryAction.Label, n.ID+":retry", control{ID: n.ID + ":retry", Node: n.ID, Action: n.RetryAction})
			}
		default:
			add("Unsupported node type: "+plain(n.Type), n.ID+":unsupported", nil)
		}
		add("", n.ID+":end", nil)
	}
	for _, n := range m.snapshot.Tree.Nodes {
		if n.Type == "composer" {
			if e := m.editors[n.ID]; e != nil && e.Conflict {
				add("Saved draft (full text):\n"+plain(e.RemoteText), n.ID+":remote:full", nil)
			}
		}
	}
	for id, text := range m.recovery {
		add("Unsaved values from removed control "+plain(id)+" (select to copy):\n"+plain(text), "recovery:"+id, nil)
	}
	return rows
}

// A single composer stays at the bottom when the pane has sufficient height.
// Multiple composers remain in the scrolling body, so every control stays reachable.
func (m *model) dockedComposer() string {
	if m.height < 18 && !m.inline() {
		return ""
	}
	id := ""
	for _, n := range m.snapshot.Tree.Nodes {
		if n.Type == "composer" {
			if id != "" {
				return ""
			}
			id = n.ID
		}
	}
	return id
}
func (m *model) composerRows() []row {
	id := m.dockedComposer()
	for _, n := range m.snapshot.Tree.Nodes {
		if n.ID == id {
			return m.composerRowsFor([]Node{n})
		}
	}
	return nil
}
func (m *model) composerRowsFor(nodes []Node) []row {
	var rows []row
	p := m.palette()
	width := m.contentWidth()
	add := func(text, anchor string, c *control) {
		for i, line := range strings.Split(ansi.Hardwrap(text, width, true), "\n") {
			rows = append(rows, row{Text: line, Anchor: fmt.Sprintf("%s:%d", anchor, i), Control: c})
		}
	}
	for _, n := range nodes {
		if n.Type != "composer" {
			continue
		}
		e := m.editors[n.ID]
		if e == nil {
			continue
		}
		add("", n.ID+":space", nil)
		c := &control{ID: "edit:" + n.ID, Node: n.ID, Kind: "edit"}
		border := p.border
		if m.focus == c.ID || m.hover == c.ID {
			border = p.accent
		}
		area := m.editorView(e)
		hint, second := composerHint(width)
		sendWidth := min(10, width-4)
		var sendControl *control
		if n.SendAction != nil && n.SendAction.Enabled && e.Area.Value() != "" && !e.Conflict && !e.Sending && !e.WantSend {
			sendControl = &control{ID: n.ID + ":send", Node: n.ID, Kind: "send", Left: width - 2 - sendWidth, HitWidth: sendWidth}
		}
		innerWidth := width - 4
		hintWidth := max(0, innerWidth-sendWidth-1)
		hint = ansi.Truncate(hint, hintWidth, "")
		// The footer is the hint and the one-row Send button. A narrow pane adds a second hint row.
		muted := lipgloss.NewStyle().Foreground(p.muted)
		footer := muted.Render(hint) + strings.Repeat(" ", max(0, innerWidth-sendWidth-lipgloss.Width(hint))) + m.buttonView(n.ID+":send", "Send ↑", sendWidth, true, sendControl != nil)
		footerRows := 1
		if second != "" {
			footer += "\n" + muted.Render(ansi.Truncate(second, hintWidth, ""))
			footerRows = 2
		}
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(0, 1).Render(area + "\n" + footer)
		lines := strings.Split(box, "\n")
		sendRow := len(lines) - 1 - footerRows
		for i, line := range lines {
			ctl := c
			if i == sendRow {
				ctl = sendControl
			}
			if i > sendRow { // the narrow second hint row and the bottom border are decorative
				ctl = nil
			}
			add(line, fmt.Sprintf("%s:edit:%d", n.ID, i), ctl)
		}

		if e.Conflict {
			add(lipgloss.NewStyle().Foreground(p.accent).Bold(true).Render("Draft conflict · choose which text to keep"), n.ID+":conflict", nil)
			// Keep remote text in the scrolling body when it is too large for this panel.
			add("Saved: "+ansi.Truncate(strings.ReplaceAll(plain(e.RemoteText), "\n", " ↵ "), max(1, width-7), "…"), n.ID+":remote", nil)
			for _, choice := range []struct{ kind, text string }{{"keep_local", "Keep my text"}, {"use_saved", "Use saved text"}} {
				ctl := &control{ID: n.ID + ":" + choice.kind, Node: n.ID, Kind: choice.kind}
				ctl.HitWidth = min(width, buttonWidth(choice.text))
				add(m.buttonView(ctl.ID, choice.text, ctl.HitWidth, false, true), n.ID+":"+choice.kind, ctl)
				add("", n.ID+":"+choice.kind+":gap", nil)
			}
		}
	}
	return rows
}

// headerRows is the number of rows above the body.
const headerRows = 1

func (m *model) screenRows() []row {
	p := m.palette()
	width := m.contentWidth()
	// No title header: the Rex tab already shows the view's title. One blank row is top padding.
	// The connection state appears in the footer only when the view is not connected.
	rows := make([]row, headerRows)
	body := m.bodyRows()
	h := m.bodyHeight()
	offset := min(m.scroll, max(0, len(body)-h))
	// The viewport owns clipping and height. Row metadata follows the same offset.
	v := viewport.New(viewport.WithWidth(width), viewport.WithHeight(h))
	v.FillHeight = true
	visible := body[offset:min(len(body), offset+h)]
	lines := make([]string, len(visible))
	for i, r := range visible {
		lines[i] = r.Text
	}
	v.SetContentLines(lines)
	// Rows already have terminal-width wrapping. Give the viewport only visible rows.
	for i, line := range strings.Split(v.View(), "\n") {
		r := row{Text: line}
		if i+offset < len(body) {
			r.Anchor = body[i+offset].Anchor
			r.Control = body[i+offset].Control
			r.Extra = body[i+offset].Extra
		}
		rows = append(rows, r)
	}
	rows = append(rows, m.composerRows()...)
	status := m.notice()

	help := "Tab next · Ctrl+T select · Ctrl+Q quit"
	for _, n := range m.snapshot.Tree.Nodes {
		if n.Type == "history" {
			help = "Tab next · PgUp/PgDn history · Ctrl+T select · Ctrl+Q quit"
		}
	}
	if width < 65 {
		help = "Tab · Ctrl+T select · Ctrl+Q quit"
	}
	if m.composerOnly {
		help = "Tab next · Ctrl+T select · Ctrl+Q quit" // the transcript pane above holds the history
	}
	if m.isPanel() {
		help = "Tab next · Esc close · Ctrl+Q close"
	}
	if m.isForm() && m.snapshot.Tree.Menu {
		help = "Ctrl+O close · " + help
	}
	for _, n := range m.snapshot.Tree.Nodes {
		if n.Type == "menu" {
			help = "Ctrl+O " + n.Label + " · " + help
			break
		}
	}
	if m.selectText {
		help = "Text selection on · Ctrl+T restores controls"
	}
	for _, n := range m.snapshot.Tree.Nodes {
		if n.Type == "form" {
			help = "Ctrl+N form · " + help
			break
		}
	}
	if status != "" {
		rows = append(rows, row{Text: lipgloss.NewStyle().Foreground(p.muted).Render(singleLine(status))})
	}
	rows = append(rows, row{Text: lipgloss.NewStyle().Foreground(p.muted).Render(fitHints(help, width))})
	return rows
}

// fitHints drops whole hints from the end of a " · " list until it fits the width. A single hint
// that does not fit ends in an ellipsis.
func fitHints(help string, width int) string {
	hints := strings.Split(help, " · ")
	for len(hints) > 1 && ansi.StringWidth(strings.Join(hints, " · ")) > width {
		hints = hints[:len(hints)-1]
	}
	return ansi.Truncate(strings.Join(hints, " · "), width, "…")
}
func (m *model) View() tea.View {
	if m.quitting {
		// The last frame is empty, on the screen that is active, so no row stays after the exit.
		m.visibleRows, m.visibleSelect = nil, nil
		v := tea.NewView("")
		v.AltScreen = !m.inline()
		return v
	}
	if m.replacement != nil {
		return m.replacementView()
	}
	if m.reuseView {
		return m.lastView
	}
	if m.isForm() && !m.themeReady {
		// A form draws nothing before the theme reply. Before the mode is decided the empty frame is inline.
		v := tea.NewView("")
		v.AltScreen, v.ReportFocus = !m.inline(), true
		m.visibleRows = nil
		m.visibleSelect = nil
		m.lastView, m.haveView = v, true
		return v
	}
	if m.inline() {
		m.visibleSelect = nil
		return m.inlineView()
	}
	var lines []string
	h, thumb, start, limit := m.scrollbar()
	m.visibleScrollbar = [4]int{h, thumb, start, limit}
	barHover := m.scrollbarTarget(m.pointerX, m.pointerY)
	barActive := m.pointerKnown && barHover != "" || m.scrollDragging
	m.visibleRows = m.screenRows()
	for i, r := range m.visibleRows {
		line := strings.Repeat(" ", m.leftMargin()) + ansi.Truncate(r.Text, m.contentWidth(), "")
		if limit > 0 && i >= headerRows && i < headerRows+h {
			glyph := "│"
			color := m.palette().border
			if i-headerRows >= start && i-headerRows < start+thumb {
				glyph = "┃"
				color = m.palette().accent
				if barHover == "$scrollbar-thumb" || m.scrollDragging {
					glyph = "█"
					color = m.palette().text
				}
			}
			if barActive {
				glyph = strings.Repeat(glyph, 2)
			}
			line += strings.Repeat(" ", max(0, m.leftMargin()+m.contentWidth()+1-lipgloss.Width(line))) + lipgloss.NewStyle().Foreground(color).Render(glyph)
		}
		lines = append(lines, line)
	}
	content := strings.Join(lines, "\n")
	m.visibleSelect = m.selectOverlay()
	if overlay := m.visibleSelect; overlay != nil {
		canvas := lipgloss.NewCanvas(m.width, m.height)
		canvas.Compose(lipgloss.NewCompositor(lipgloss.NewLayer(content), overlay.layer().Z(1)))
		content = canvas.Render()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.ReportFocus = true
	v.WindowTitle = singleLine(m.snapshot.Tree.Title)
	if !m.selectText {
		v.MouseMode = tea.MouseModeAllMotion
	}
	m.lastView, m.haveView = v, true
	return v
}

// bareButton reports a node that is only a button: a widget with no text (for example Cancel).
func bareButton(n Node) bool {
	return n.Type == "widget" && strings.TrimSpace(n.Text) == "" && n.Label == "" && n.Action != nil && n.Action.Enabled
}

// browseWidth is the width of a field's Browse… button, or 0 when the field has none.
func browseWidth(f Field, controlWidth int) int {
	if f.Kind != "text" || f.Browse != "native" {
		return 0
	}
	return min(12, controlWidth)
}

// browseBeside reports whether Browse… fits beside a text box at least 20 cells wide.
func browseBeside(controlWidth int) bool { return controlWidth-12-1 >= 20 }

// compactList reports a list with no item details. It shows one row per item.
func compactList(n Node) bool {
	if len(n.Items) == 0 {
		return false
	}
	for _, item := range n.Items {
		if item.Detail != "" {
			return false
		}
	}
	return true
}

// compactListRows shows each item on one row: an icon, the label, and the status at the right.
// An enabled item ends in "›" and gets the hover and focus highlight.
func (m *model) compactListRows(n Node, width int) []row {
	p := m.palette()
	var rows []row
	for _, item := range n.Items {
		var c *control
		if item.Action != nil && item.Action.Enabled {
			c = &control{HitWidth: width, ID: n.ID + ":" + item.ID, Node: n.ID, Action: item.Action, Input: map[string]any{"item_id": item.ID}}
		}
		style := lipgloss.NewStyle().Foreground(p.text)
		if c == nil {
			style = style.Foreground(p.muted)
		} else {
			style = m.controlStyle(c.ID, style)
		}
		bar := " "
		if c != nil && c.ID == m.focus {
			bar = lipgloss.NewStyle().Foreground(p.accent).Inherit(style).Render("▏")
		}
		icon := "  "
		if item.Icon != "" {
			icon = ansi.Truncate(singleLine(item.Icon), 1, "") + " "
		}
		right := singleLine(item.Status)
		if c != nil {
			right = strings.TrimSpace(right + " ›")
		}
		if right != "" {
			right += " "
		}
		labelWidth := max(1, width-2-lipgloss.Width(icon)-lipgloss.Width(right)-1)
		text := ansi.Truncate(singleLine(item.Label), labelWidth, "…")
		pad := max(1, width-1-1-lipgloss.Width(icon)-lipgloss.Width(text)-lipgloss.Width(right))
		muted := lipgloss.NewStyle().Foreground(p.muted).Inherit(style)
		line := bar + style.Render(" "+icon+text+strings.Repeat(" ", pad)) + muted.Render(right)
		rows = append(rows, row{Text: line, Anchor: fmt.Sprintf("%s:%s:0", n.ID, item.ID), Control: c})
	}
	return rows
}
