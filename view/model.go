package main

import (
	"crypto/rand"
	"encoding/hex"
	"reflect"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

type editor struct {
	Area textarea.Model
	// Input holds the value of a field of kind text. Other kinds use Area.
	Input                                       textinput.Model
	Saved, Revision, RemoteText, RemoteRevision string
	Pending                                     string
	WantSend                                    bool
	SendText                                    string
	Sending                                     bool
	Conflict                                    bool
	Kind                                        string
	Choice                                      any
	SavedChoice                                 any
	Scroll                                      int
	Reviewing                                   bool
}

type control struct {
	ID, Node, Field, Kind string
	Action                *Action
	Input                 map[string]any
	Left, HitWidth        int
}
type row struct {
	Text, Anchor string
	Control      *control
	// Extra are more controls on the same row, each at its own Left (for example Cancel beside
	// Submit, or Browse… beside its text box).
	Extra []control
}

// rowControls returns the row's controls from left to right.
func (r row) rowControls() []control {
	var out []control
	if r.Control != nil {
		out = append(out, *r.Control)
	}
	return append(out, r.Extra...)
}

type model struct {
	composerOnly                           bool // a pair's composer (Snapshot.Layout): the transcript pane shows the messages
	selectList                             *selectList
	browse                                 *nativeBrowse
	browseSequence                         uint64
	client                                 *viewClient
	replacement                            *panelReplacement
	effectFailure                          *completed
	snapshot                               Snapshot
	programStatus                          string   // last OSC 7501 report written; "" = none yet
	afterSave                              *control // a closing action that waits for the drafts to save
	editors                                map[string]*editor
	recovery                               map[string]string
	focus                                  string
	width, height, scroll                  int
	ready                                  bool
	status                                 string
	keys                                   bool
	selectText                             bool
	operations                             map[string]bool
	reloadPending, reloadReady             bool
	retirePending, retireReady             bool
	closePending, closeRunning, closeReady bool
	dark                                   bool
	themeReady                             bool
	markdown                               map[string]string
	messageRows                            map[string]messageRowCache
	visibleRows                            []row
	visibleSelect                          *selectOverlay
	visibleScrollbar                       [4]int
	lastView                               tea.View
	haveView, reuseView                    bool
	hover, pressed                         string
	pointerX, pointerY                     int
	pointerKnown                           bool
	scrollDragging                         bool
	scrollGrab                             int
}

func newModel(client *viewClient) *model {
	return &model{client: client, operations: map[string]bool{}, editors: map[string]*editor{}, recovery: map[string]string{}, width: 80, height: 24, status: "Connecting to the hub…"}
}

// Init subscribes to color scheme changes (DEC mode 2031) and asks for the current scheme (DSR 996).
// Rex answers the query also before any app has themed the pane, and reports each later change of
// scheme from set_theme (observed on Rex 1203, lab). A form waits for the first theme reply,
// with a short fallback timeout. Later changes use reports, not a polling timer.
func (m *model) Init() tea.Cmd {
	commands := []tea.Cmd{tea.Raw("\x1b[?2031h\x1b[?996n"), tea.RequestBackgroundColor}
	if m.isForm() {
		commands = append(commands, tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return formThemeTimeout{} }))
	}
	return tea.Batch(commands...)
}

type formThemeTimeout struct{}

func newEditor(text, rev, kind string) *editor {
	a := textarea.New()
	a.ShowLineNumbers = false
	a.Prompt = ""
	a.Placeholder = "Write a message…"
	a.CharLimit = 0
	a.MaxHeight = 0
	a.MaxWidth = 0
	a.DynamicHeight = kind == "composer" || kind == "multiline"
	a.MinHeight = 1
	a.SetHeight(4)
	a.SetWidth(70)
	a.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "ctrl+j"))
	a.SetValue(plain(text))
	e := &editor{Area: a, Saved: text, Revision: rev, RemoteText: text, RemoteRevision: rev, Kind: kind}
	if kind == "text" {
		e.Input = textinput.New()
		e.setText(text)
		// The input changes newlines and tabs to spaces. A field shows its normalized value clean.
		e.Saved = e.text()
	}
	return e
}

// Keep a valid choice. Otherwise, use the first option or an empty value.
func selectChoice(value any, options []Option) string {
	for _, option := range options {
		if value == option.Value {
			return option.Value
		}
	}
	if len(options) > 0 {
		return options[0].Value
	}
	return ""
}

func editorID(node, field string) string {
	if field == "" {
		return node
	}
	return node + "/" + field
}
func (m *model) node(id string) *Node {
	for i := range m.snapshot.Tree.Nodes {
		if m.snapshot.Tree.Nodes[i].ID == id {
			return &m.snapshot.Tree.Nodes[i]
		}
	}
	return nil
}

const loadingViewStatus = "Loading the updated view…"

func (m *model) apply(s Snapshot) {
	loading := len(s.Tree.Nodes) > 0
	for _, n := range s.Tree.Nodes {
		loading = loading && n.Type == "loading"
	}
	if loading && len(m.snapshot.Tree.Nodes) > 0 {
		m.status = loadingViewStatus
		return
	}
	if len(s.Tree.Nodes) > 0 && !loading && m.status == loadingViewStatus {
		m.status = ""
	}
	rows := m.bodyRows()
	follow := len(rows) == 0 || m.scroll >= max(0, len(rows)-m.bodyHeight())
	anchor := ""
	if m.scroll < len(rows) {
		anchor = rows[m.scroll].Anchor
	}
	present := map[string]bool{}
	for _, n := range s.Tree.Nodes {
		if n.Type == "composer" {
			present[n.ID] = true
			e := m.editors[n.ID]
			if e == nil || e.Kind != "composer" {
				if e != nil && (e.dirty() || e.Pending != "") {
					m.recovery[n.ID] = e.Area.Value()
				}
				e = newEditor(n.Text, n.DraftRevision, "composer")
				m.editors[n.ID] = e
			} else {
				incoming, valid := revision(n.DraftRevision)
				current, currentValid := revision(e.Revision)
				if valid && currentValid && incoming.Cmp(current) < 0 {
					continue
				}
				e.RemoteText = n.Text
				e.RemoteRevision = n.DraftRevision
				// An in-flight result owns its draft revision. Snapshots can precede that result.
				if e.Pending == "" && !e.Conflict && e.Area.Value() == e.Saved {
					if e.Area.Value() != n.Text {
						e.Area.SetValue(plain(n.Text))
					}
					e.Saved = n.Text
					e.Revision = n.DraftRevision
				}
			}
			if text, ok := m.recovery[n.ID]; ok && text == e.Area.Value() {
				delete(m.recovery, n.ID)
			}
		}
		if n.Type == "form" {
			for _, f := range n.Fields {
				id := editorID(n.ID, f.ID)
				present[id] = true
				if old := m.editors[id]; old == nil || old.Kind != f.Kind {
					if old != nil && old.dirty() {
						m.recovery[id] = old.recoveryText()
					}
					s, _ := f.Value.(string)
					e := newEditor(s, "", f.Kind)
					e.Choice = f.Value
					e.SavedChoice = f.Value
					e.Area.Placeholder = ""
					m.editors[id] = e
				} else if m.fieldRevisionChanged(n.ID, f) || (!old.dirty() && (m.browse == nil || m.browse.Editor != id) && m.fieldValueChanged(n.ID, f)) {
					if m.browse != nil && m.browse.Editor == id {
						m.endBrowse()
					}
					if f.Kind == "boolean" || f.Kind == "select" {
						old.Choice, old.SavedChoice = f.Value, f.Value
					} else {
						value, _ := f.Value.(string)
						old.setText(value)
						old.Saved = value
						if f.Kind == "text" {
							old.Saved = old.text()
						}
					}
				}
				if f.Kind == "select" {
					e := m.editors[id]
					e.Choice = selectChoice(e.Choice, f.Options)
					e.SavedChoice = selectChoice(e.SavedChoice, f.Options)
				}
			}
		}
	}
	for id, e := range m.editors {
		if !present[id] {
			if e.dirty() || e.Pending != "" {
				m.recovery[id] = e.recoveryText()
			}
			// Keep pending editors until their results arrive.
			if e.Pending == "" {
				delete(m.editors, id)
			}
		}
	}
	if only := s.Layout == "transcript"; only != m.composerOnly {
		// The hub paired or unpaired the view in place: the history rows come or go.
		m.composerOnly = only
		follow, anchor = true, ""
	}
	m.snapshot = s
	m.reconcileSelect()
	if anchor != "" {
		for i, r := range m.bodyRows() {
			if r.Anchor == anchor {
				m.scroll = i
				break
			}
		}
	}
	m.resizeEditors()
	history := false
	for _, n := range s.Tree.Nodes {
		history = history || n.Type == "history"
	}
	if follow && history {
		m.scroll = max(0, len(m.bodyRows())-m.bodyHeight())
	}
	controls := m.controls()
	found := false
	for _, c := range controls {
		if c.ID == m.focus {
			found = true
		}
	}
	if !found && len(controls) > 0 {
		target := controls[0].ID
		for _, c := range controls {
			if c.Kind == "edit" && m.node(c.Node) != nil && m.node(c.Node).Type == "composer" {
				target = c.ID
				break
			}
		}
		m.setFocus(target)
	}
	if m.selectList != nil {
		m.revealFocus()
	}
}

func (m *model) lastMessageIDs() map[string]string {
	ids := map[string]string{}
	for _, n := range m.snapshot.Tree.Nodes {
		if n.Type == "history" && len(n.Messages) > 0 {
			ids[n.ID] = n.Messages[len(n.Messages)-1].ID
		}
	}
	return ids
}

func (m *model) resizeEditors() {
	browse := map[string]int{}
	for _, n := range m.snapshot.Tree.Nodes {
		for _, f := range n.Fields {
			if bw := browseWidth(f, m.controlWidth()); bw > 0 && browseBeside(m.controlWidth()) {
				browse[editorID(n.ID, f.ID)] = bw + 1
			}
		}
	}
	for id, e := range m.editors {
		width := m.contentWidth()
		if e.Kind != "composer" {
			width = m.controlWidth() - browse[id]
		}
		if e.Kind == "text" {
			// The bar, the prompt and the cell for the cursor after the last character.
			e.Input.SetWidth(max(1, width-inputBar-lipgloss.Width(e.Input.Prompt)-1))
			e.Input.SetStyles(m.inputStyles())
			continue
		}
		e.Area.SetWidth(max(4, width-4))
		e.Scroll = min(e.Scroll, max(0, e.Area.Height()-m.editorHeight(e)))

		styles := textarea.DefaultStyles(m.dark)
		p := m.palette()
		styles.Focused.Text = lipgloss.NewStyle().Foreground(p.text)
		styles.Blurred.Text = styles.Focused.Text
		styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(p.muted)
		styles.Blurred.Placeholder = styles.Focused.Placeholder
		styles.Cursor.Color = p.accent
		styles.Focused.CursorLine = styles.Focused.Text
		styles.Blurred.CursorLine = styles.Blurred.Text
		e.Area.SetStyles(styles)
	}
}
func (m *model) setFocus(id string) tea.Cmd {
	if m.selectList != nil && id != m.selectList.controlID() && !strings.HasPrefix(id, m.selectList.optionPrefix()) {
		m.selectList = nil
	}
	if m.browse != nil && id != "edit:"+m.browse.Editor && id != "browse:"+m.browse.Editor {
		m.endBrowse()
	}
	m.focus = id
	var cmds []tea.Cmd
	for key, e := range m.editors {
		switch {
		case id != "edit:"+key:
			e.Area.Blur()
			e.Input.Blur()
		case e.Kind == "text":
			cmds = append(cmds, e.Input.Focus())
		default:
			cmds = append(cmds, e.Area.Focus())
		}
	}
	return tea.Batch(cmds...)
}

func (m *model) enqueue(method, node string, args map[string]any) tea.Cmd {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		m.status = "Cannot create an operation key: " + err.Error()
		return nil
	}
	k := hex.EncodeToString(b)
	args["node_id"] = node
	args["operation_key"] = k
	if m.operations == nil {
		m.operations = map[string]bool{}
	}
	m.operations[k] = true
	req := mutation{Method: method, Key: k, Node: node, Args: args}
	if method == "hub.view.draft.save" || method == "hub.view.send" {
		m.editors[node].Pending = k
	}
	return func() tea.Msg { m.client.Requests <- req; return nil }
}

func (m *model) save(node string) tea.Cmd {
	e := m.editors[node]
	n := m.node(node)
	if e == nil || n == nil || n.Type != "composer" || !m.ready || e.Pending != "" || e.Conflict || n.SaveAction == nil || !n.SaveAction.Enabled {
		return nil
	}
	text := e.Area.Value()
	if e.WantSend {
		text = e.SendText
	}
	if text != e.Saved {
		return m.enqueue("hub.view.draft.save", node, map[string]any{"base_revision": e.Revision, "text": text})
	}
	if e.WantSend {
		e.WantSend = false
		if text == "" || n.SendAction == nil || !n.SendAction.Enabled {
			return nil
		}
		e.Sending = true
		return m.enqueue("hub.view.send", node, map[string]any{"draft_revision": e.Revision, "text": text})
	}
	return nil
}

func (m *model) finish(c completed) tea.Cmd {
	delete(m.operations, c.Request.Key)
	e := m.editors[c.Request.Node]
	if c.Outcome.Status == "failed" {
		m.status = "The operation failed"
		if c.Outcome.Error != nil {
			m.status = c.Outcome.Error.Message
		}
		if e != nil && e.Pending == c.Request.Key {
			e.Pending = ""
			e.Conflict = true
			e.WantSend = false
			e.Sending = false
		}
		m.afterSave = nil
		return nil
	}
	if c.Request.Method == "hub.view.action" {
		m.revealFocus()
		if input, ok := c.Request.Args["input"].(map[string]any); ok {
			if values, ok := input["values"].(map[string]any); ok {
				for field, value := range values {
					if fieldEditor := m.editors[editorID(c.Request.Node, field)]; fieldEditor != nil {
						if fieldEditor.Kind == "boolean" || fieldEditor.Kind == "select" {
							fieldEditor.SavedChoice = value
							if fieldEditor.Kind == "select" {
								if node := m.node(c.Request.Node); node != nil {
									for _, f := range node.Fields {
										if f.ID == field {
											fieldEditor.SavedChoice = selectChoice(value, f.Options)
											break
										}
									}
								}
							}
						} else if text, ok := value.(string); ok {
							fieldEditor.Saved = text
						}
					}
				}
			}
		}
	}
	if c.Outcome.Effects == "failed" {
		saved := c
		m.effectFailure = &saved
		m.status = "The change was saved, but the next step failed: " + c.Outcome.EffectsError
		return nil
	}
	if m.effectFailure != nil && m.effectFailure.Request.Key == c.Request.Key {
		m.effectFailure = nil
	}
	if e == nil || e.Pending != c.Request.Key {
		m.status = "Done"
		return nil
	}
	result := c.Outcome.Result
	if c.Request.Method == "hub.view.draft.save" {
		text, ok := result["text"].(string)
		if !ok || text != c.Request.Args["text"] {
			m.status = "The saved operation returned different text. Local text is preserved."
			e.Conflict = true
			e.Pending = ""
			e.WantSend = false
			m.afterSave = nil
			return nil
		}
	}
	rev, _ := result["draft_revision"].(string)
	if _, ok := revision(rev); !ok {
		m.status = "Invalid draft result; local text is preserved"
		e.Conflict = true
		e.Pending = ""
		m.afterSave = nil
		return nil
	}
	e.Pending = ""
	e.Revision = rev
	text, _ := c.Request.Args["text"].(string)
	if c.Request.Method == "hub.view.send" {
		e.Sending = false
		if e.Area.Value() == text {
			e.Area.SetValue("")
		}
		e.Saved = ""
		m.status = "Sent"
	} else {
		e.Saved = text
		m.status = "Draft saved"
	}
	if cmd := m.save(c.Request.Node); cmd != nil || m.afterSave == nil || m.draftsBusy() {
		return cmd
	}
	// Every draft is saved: run the closing action that waited for it.
	after := *m.afterSave
	m.afterSave = nil
	return m.activate(after)
}

// draftsBusy reports a composer whose text is not saved yet, or that is saving or sending.
func (m *model) draftsBusy() bool {
	for _, e := range m.editors {
		if e.Kind == "composer" && (e.dirty() || e.Pending != "" || e.WantSend || e.Sending) {
			return true
		}
	}
	return false
}

func (m *model) activate(c control) tea.Cmd {
	if c.Kind == "select_option" {
		m.pickSelect(c)
		return nil
	}
	if c.Kind == "browse" {
		return m.startBrowse(c)
	}
	if c.Kind == "retry_effect" {
		return m.retryEffect()
	}
	if c.Kind == "submit" && m.isForm() && m.effectFailure != nil {
		m.status = "The change is already saved. Use Retry for the next step, or Cancel to close."
		return nil
	}
	if c.Action != nil && c.Action.Name == "$cancel" {
		return m.cancelForm()
	}
	if c.Kind == "menu" {
		return m.openMenu(c.Node)
	}
	if c.Kind == "edit" {
		return m.setFocus(c.ID)
	}
	if c.Kind == "boolean" {
		if e := m.editors[editorID(c.Node, c.Field)]; e != nil {
			e.Choice = c.Input["value"]
		}
		return nil
	}
	if c.Kind == "choice" {
		e := m.editors[editorID(c.Node, c.Field)]
		n := m.node(c.Node)
		if e == nil || n == nil {
			return nil
		}
		for _, f := range n.Fields {
			if f.ID == c.Field {
				if f.Kind == "select" {
					m.openSelect(c)
				}
			}
		}
		return nil
	}
	if c.Kind == "use_saved" || c.Kind == "keep_local" {
		e := m.editors[c.Node]
		if e == nil || e.Pending != "" {
			return nil
		}
		e.Conflict = false
		e.Revision = e.RemoteRevision
		e.Saved = e.RemoteText
		if c.Kind == "use_saved" {
			e.Area.SetValue(plain(e.RemoteText))
		}
		return m.save(c.Node)
	}
	if !m.ready {
		m.status = "The hub is unavailable. Your text remains here."
		return nil
	}
	if c.Kind == "send" {
		if e := m.editors[c.Node]; e != nil {
			if e.Sending || e.WantSend {
				return nil
			}
			e.SendText = e.Area.Value()
			e.WantSend = true
			return m.save(c.Node)
		}
		return nil
	}
	if c.Action == nil || !c.Action.Enabled {
		return nil
	}
	if c.Action.ClosesView {
		// The action can close this view. Save every draft first, then run it.
		for _, e := range m.editors {
			if e.Kind == "composer" && e.Conflict {
				m.status = "Resolve the draft conflict first."
				return nil
			}
		}
		if m.draftsBusy() {
			var saves []tea.Cmd
			for id, e := range m.editors {
				if e.Kind == "composer" && e.dirty() {
					saves = append(saves, m.save(id))
				}
			}
			after := c
			m.afterSave = &after
			m.status = "Saving the draft first…"
			return tea.Batch(saves...)
		}
	}
	input := c.Input
	if c.Kind == "submit" {
		values := map[string]any{}
		for _, f := range m.node(c.Node).Fields {
			e := m.editors[editorID(c.Node, f.ID)]
			if f.Kind == "boolean" || f.Kind == "select" {
				values[f.ID] = e.Choice
			} else {
				values[f.ID] = e.text()
			}
			if m.fieldVisible(c.Node, f) && f.Required && (values[f.ID] == nil || values[f.ID] == "") {
				m.status = "Complete " + f.Label
				return nil
			}
		}
		input = map[string]any{"values": values}
	}
	if input == nil {
		input = map[string]any{}
	}
	return m.enqueue("hub.view.action", c.Node, map[string]any{"action": c.Action.Name, "revision": m.snapshot.Revision, "input": input})
}

func (m *model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.closeRunning {
		switch msg.(type) {
		case tea.KeyPressMsg, tea.PasteMsg, tea.MouseClickMsg, tea.MouseReleaseMsg, tea.MouseWheelMsg:
			return m, nil
		}
	}
	if m.updateSelect(msg) {
		return m, nil
	}
	switch msg := msg.(type) {
	case nativeBrowseResult:
		if m.browse != nil && m.browse.Sequence == msg.Sequence {
			if msg.Err != nil {
				m.endBrowse()
				m.status = "Could not open the folder browser: " + msg.Err.Error()
			}
		}
		return m, nil
	case closeFinished:
		m.closeRunning = false
		if msg.err != nil {
			m.status = "Could not close this view: " + msg.err.Error() + ". Press Ctrl+Q to retry."
			if m.replacement != nil {
				m.replacement.Error = m.status
			}
			return m, nil
		}
		m.closeReady = true
		return m, tea.Quit
	case tea.FocusMsg:
		m.client.focus(true, "focus")
		return m, tea.RequestBackgroundColor
	case uv.DarkColorSchemeEvent:
		m.setDark(true)
		m.themeReady = true
		return m, tea.RequestBackgroundColor
	case uv.LightColorSchemeEvent:
		m.setDark(false)
		m.themeReady = true
		return m, tea.RequestBackgroundColor
	case tea.BlurMsg:
		m.client.focus(false, "")
		return m, nil
	case retireRequested:
		m.retirePending = true
		return m, nil
	case closeRequested:
		if !m.isPanel() && !m.isForm() {
			m.closePending = true
		}
		return m, nil
	case reloadRequested:
		m.reloadPending = true
		return m, nil
	case tea.BackgroundColorMsg:
		m.setDark(msg.IsDark())
		m.themeReady = true
		return m, nil
	case formThemeTimeout:
		m.themeReady = true
		return m, nil
	case Snapshot:
		before := m.lastMessageIDs()
		m.apply(msg)
		m.client.mu.Lock()
		focused := m.client.focused
		m.client.mu.Unlock()
		if focused && !reflect.DeepEqual(before, m.lastMessageIDs()) {
			m.client.focus(true, "content")
		}
		// The first snapshot always reports, so a record left by an earlier renderer is replaced.
		if seq := statusSequence(m.snapshot.Status); seq != m.programStatus {
			m.programStatus = seq
			return m, tea.Raw(seq)
		}
		return m, nil
	case connectionMsg:
		m.ready = msg.Ready
		m.status = msg.Text
		var cmds []tea.Cmd
		if m.ready {
			for _, n := range m.snapshot.Tree.Nodes {
				if n.Type == "composer" {
					cmds = append(cmds, m.save(n.ID))
				}
			}
		}
		return m, tea.Batch(cmds...)
	case completed:
		return m, m.finish(msg)
	case tea.WindowSizeMsg:
		m.width = max(12, msg.Width)
		m.height = max(6, msg.Height)
		m.resizeEditors()
		// The app sizes a pane when it shows it, and sends its theme then.
		return m, tea.RequestBackgroundColor
	case tea.KeyboardEnhancementsMsg:
		m.keys = msg.SupportsKeyDisambiguation()
		return m, nil
	case tea.MouseMotionMsg:
		if m.scrollDragging {
			m.dragScrollbar(msg.Y)
			return m, nil
		}
		m.pointerX, m.pointerY, m.pointerKnown = msg.X, msg.Y, true
		m.refreshHover()
		return m, nil
	case tea.MouseWheelMsg:
		m.pointerX, m.pointerY, m.pointerKnown = msg.X, msg.Y, true
		return m, m.wheel(msg)
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseRight && !m.selectText {
			return m, m.openFirstMenu()
		}
		if msg.Button != tea.MouseLeft {
			return m, nil
		}
		m.pointerX, m.pointerY, m.pointerKnown = msg.X, msg.Y, true
		if m.pressScrollbar(msg.X, msg.Y) {
			return m, nil
		}
		c, ok := m.hitControl(msg.X, msg.Y)
		if !ok {
			m.pressed = ""
			return m, nil
		}
		m.pressed = c.ID
		return m, m.setFocus(c.ID)
	case tea.MouseReleaseMsg:
		if m.scrollDragging {
			m.dragScrollbar(msg.Y)
			m.scrollDragging = false
			return m, nil
		}
		if msg.Button != tea.MouseLeft {
			return m, nil
		}
		c, ok := m.hitControl(msg.X, msg.Y)
		pressed := m.pressed
		m.pressed = ""
		if ok && c.ID == pressed {
			return m, m.activate(c)
		}
		return m, nil
	case tea.KeyPressMsg:
		if msg.String() == "esc" && m.browse != nil {
			m.endBrowse()
			return m, nil
		}
		switch msg.String() {
		case "left", "right":
			if m.cycleSelect(msg.String() == "right") {
				return m, nil
			}
		case "ctrl+o":
			// Ctrl+O toggles: in a menu's form it closes the form, like Esc.
			if m.isForm() && m.snapshot.Tree.Menu {
				return m, m.cancelForm()
			}
			return m, m.openFirstMenu()
		case "ctrl+q":
			if m.isPanel() {
				return m, m.closePanel()
			}
			if m.isForm() {
				return m, m.cancelForm()
			}
			m.closePending = true
			return m, nil
		case "ctrl+n":
			for _, n := range m.snapshot.Tree.Nodes {
				if n.Type == "form" && len(n.Fields) > 0 {
					id := editorID(n.ID, n.Fields[0].ID)
					cmd := m.setFocus(fieldControlID(n.Fields[0], id))
					m.revealFocus()
					for i, r := range m.bodyRows() {
						if strings.HasPrefix(r.Anchor, n.ID+":label:") {
							m.scroll = i
							break
						}
					}
					return m, cmd
				}
			}
			return m, nil
		case "ctrl+t":
			m.selectText = !m.selectText
			return m, nil
		case "tab":
			return m, m.moveFocus(1)
		case "shift+tab":
			return m, m.moveFocus(-1)
		case "pgup":
			m.scroll = max(0, m.scroll-m.bodyHeight())
			return m, nil
		case "pgdown":
			m.scroll = min(max(0, len(m.bodyRows())-m.bodyHeight()), m.scroll+m.bodyHeight())
			return m, nil
		case "esc":
			if m.isPanel() {
				return m, m.closePanel()
			}
			if m.isForm() {
				return m, m.cancelForm()
			}
			if m.snapshot.Tree.Back != nil {
				return m, m.activate(control{Node: "$back", Action: m.snapshot.Tree.Back})
			}
			return m, nil
		case "space", " ":
			for _, c := range m.controls() {
				if c.ID == m.focus && (c.Kind == "choice" || c.Kind == "boolean") {
					return m, m.activate(c)
				}
			}
		case "enter":
			for _, c := range m.controls() {
				if c.ID == m.focus {
					if c.Kind == "edit" {
						if n := m.node(c.Node); n != nil && n.Type == "composer" {
							return m, m.activate(control{Node: c.Node, Kind: "send"})
						}
						// Enter in a one-line text field moves to the next control, as in huh.
						if e := m.editors[editorID(c.Node, c.Field)]; e != nil && e.Kind == "text" {
							return m, m.moveFocus(1)
						}
						break
					}
					return m, m.activate(c)
				}
			}
		}
	}
	// Focus on Browse… keeps the picker wait. The picked path is a paste: it goes to the field.
	if _, paste := msg.(tea.PasteMsg); paste && m.browse != nil && m.focus == "browse:"+m.browse.Editor {
		cmd := m.setFocus("edit:" + m.browse.Editor)
		_, input := m.update(msg)
		return m, tea.Batch(cmd, input)
	}
	if strings.HasPrefix(m.focus, "edit:") {
		id := strings.TrimPrefix(m.focus, "edit:")
		e := m.editors[id]
		if e != nil {
			m.browseInput(id, msg)
			old := e.text()
			var cmd tea.Cmd
			if e.Kind == "text" {
				e.Input, cmd = e.Input.Update(msg)
			} else {
				e.Area, cmd = e.Area.Update(msg)
			}
			switch msg.(type) {
			case tea.KeyPressMsg, tea.PasteMsg:
				e.Reviewing = false
			}
			if e.text() != old && e.Kind == "composer" {
				return m, tea.Batch(cmd, m.save(id))
			}
			return m, cmd
		}
	}
	return m, nil
}

func (m *model) bodyHeight() int {
	return max(1, m.height-m.footerHeight()-headerRows-len(m.composerRows()))
}
func (m *model) controls() []control {
	var controls []control
	seen := map[string]bool{}
	for _, r := range m.rows() {
		for _, c := range r.rowControls() {
			if c.Kind != "select_option" && !seen[c.ID] {
				controls = append(controls, c)
				seen[c.ID] = true
			}
		}
	}
	return controls
}
func (m *model) revealFocus() {
	first, last := -1, -1
	for i, r := range m.bodyRows() {
		for _, c := range r.rowControls() {
			if c.ID == m.focus {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
	}
	if first < 0 {
		return
	}
	if first < m.scroll {
		m.scroll = first
	}
	if last >= m.scroll+m.bodyHeight() {
		m.scroll = max(first, last-m.bodyHeight()+1)
	}
}

// setDark switches the palette. An unchanged scheme does nothing, so repeated reports cost no
// editor resize.
func (m *model) setDark(dark bool) {
	if m.dark == dark {
		return
	}
	m.dark = dark
	m.markdown = nil
	m.resizeEditors()
}

// moveFocus moves the focus by delta controls, wrapping at the ends, and reveals the control.
func (m *model) moveFocus(delta int) tea.Cmd {
	cs := m.controls()
	if len(cs) == 0 {
		return nil
	}
	idx := 0
	for i, c := range cs {
		if c.ID == m.focus {
			idx = i
			break
		}
	}
	cmd := m.setFocus(cs[(idx+delta+len(cs))%len(cs)].ID)
	m.revealFocus()
	return cmd
}

// fieldControlID is the ID of a field's first control: Yes for a boolean, the closed select, or
// the text editor.
func fieldControlID(f Field, editor string) string {
	switch f.Kind {
	case "boolean":
		return "yes:" + editor
	case "select":
		return "choice:" + editor
	}
	return "edit:" + editor
}

// fieldChoiceFocus reports focus on a choice control: a closed select, Yes or No.
func fieldChoiceFocus(id string) bool {
	return strings.HasPrefix(id, "choice:") || strings.HasPrefix(id, "yes:") || strings.HasPrefix(id, "no:")
}
