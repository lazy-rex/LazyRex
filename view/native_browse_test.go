package main

import (
	tea "charm.land/bubbletea/v2"
	"encoding/json"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
)

func TestFieldRevisionReplacesDirtyValueAndCancelsPick(t *testing.T) {
	m := browseModel()
	e := m.editors[editorID("start", "dir")]
	e.setText("/typed")
	m.startBrowse(browseControl())
	s := browseSnapshot("/selected")
	zero := int64(0)
	s.Tree.Nodes[0].Fields[0].Rev = &zero
	m.apply(s)
	if e.text() != "/selected" || e.dirty() || m.browse != nil {
		t.Fatal("Explicit choice did not replace dirty input and cancel the picker wait")
	}
	e.setText("/new-typing")
	s = browseSnapshot("/other")
	s.Tree.Nodes[0].Fields[0].Rev = &zero
	m.apply(s)
	if e.text() != "/new-typing" {
		t.Fatal("The same revision overwrote new typing")
	}
	one := int64(1)
	s = browseSnapshot("/other")
	s.Tree.Nodes[0].Fields[0].Rev = &one
	m.apply(s)
	if e.text() != "/other" || e.dirty() {
		t.Fatal("Changed revision did not replace the value")
	}
	e.setText("/keep")
	m.apply(browseSnapshot("/omitted"))
	if e.text() != "/keep" {
		t.Fatal("Omitting the revision overwrote dirty input")
	}
}

func TestFieldRevisionDecode(t *testing.T) {
	var f Field
	if err := json.Unmarshal([]byte(`{"id":"dir","kind":"text","rev":0}`), &f); err != nil || f.Rev == nil || *f.Rev != 0 {
		t.Fatal("Zero revision was lost")
	}
	if err := json.Unmarshal([]byte(`{"rev":0.5}`), &f); err == nil {
		t.Fatal("Fractional revision was accepted")
	}
}

func TestNativeBrowseKeyboardPointerAndNarrowWidth(t *testing.T) {
	for _, width := range []int{18, 80} {
		m := browseModel()
		m.width = width
		m.resizeEditors()
		rows := m.bodyRows()
		label := false
		var button control
		for _, r := range rows {
			if strings.Contains(ansi.Strip(r.Text), "Directory") {
				label = true
			}
			if ansi.StringWidth(r.Text) > m.contentWidth() {
				t.Fatal("Browse exceeds the content width")
			}
			for _, c := range r.rowControls() {
				if c.Kind == "browse" {
					button = c
				}
			}
		}
		if !label || button.ID == "" {
			t.Fatal("Missing field label or Browse control")
		}
		m.setFocus(button.ID)
		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd == nil || m.browse == nil || m.focus != "edit:"+editorID("start", "dir") {
			t.Fatal("Keyboard Browse did not focus the editor")
		}
		m.endBrowse()
		for y, r := range m.screenRows() {
			var b *control
			for _, c := range r.rowControls() {
				if c.Kind == "browse" {
					b = &c
				}
			}
			if b == nil {
				continue
			}
			x := m.leftMargin() + b.Left
			m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y})
			_, cmd = m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: x, Y: y})
			if cmd == nil || m.browse == nil {
				t.Fatal("Pointer Browse did not start")
			}
			break
		}
	}
}

func browseSnapshot(value string) Snapshot {
	return Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/forms/start", Revision: "1", Tree: Tree{Nodes: []Node{{ID: "start", Type: "form", Fields: []Field{{ID: "dir", Label: "Directory", Kind: "text", Browse: "native", Value: value}}}}}}
}
func browseModel() *model { m := testModel(); m.apply(browseSnapshot("/old")); return m }
func browseControl() control {
	return control{ID: "browse:start/dir", Node: "start", Field: "dir", Kind: "browse"}
}

func TestNativeBrowseInputAndCancel(t *testing.T) {
	m := browseModel()
	id := editorID("start", "dir")
	e := m.editors[id]
	if m.startBrowse(browseControl()) == nil || m.focus != "edit:"+id || e.text() != "/old" {
		t.Fatal("Browse did not preserve and focus the editor")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.browse != nil || e.text() != "/old" || m.closePending {
		t.Fatal("Escape lost text or closed the form")
	}
	m.startBrowse(browseControl())
	e.Input.CursorEnd()
	m.Update(tea.KeyPressMsg{Text: "x", Code: 'x'})
	if e.text() != "/oldx" || m.browse != nil {
		t.Fatal("Typing after picker cancellation replaced the value")
	}
	m.startBrowse(browseControl())
	m.Update(tea.PasteMsg{Content: "~/My\\ Folder/"})
	if e.text() != "~/My\\ Folder/" {
		t.Fatalf("Path changed: %q", e.text())
	}
	if m.browse != nil {
		t.Fatal("Paste did not end the wait")
	}
	m.startBrowse(browseControl())
	m.Update(tea.PasteMsg{Content: "/second/"})
	if e.text() != "/second/" {
		t.Fatal("Second pick appended to the first")
	}
	m.endBrowse()
	m.startBrowse(browseControl())
	m.setFocus("browse:" + id)
	if m.startBrowse(browseControl()) != nil || m.browse != nil || e.text() != "/second/" {
		t.Fatal("Second Browse did not cancel the wait")
	}
	m.startBrowse(browseControl())
	m.setFocus("other")
	if m.browse != nil || e.text() != "/second/" {
		t.Fatal("Focus change lost text")
	}
}

func TestNativeBrowseKeepsBracketedPasteEnabled(t *testing.T) {
	m := browseModel()
	m.themeReady = true
	if m.View().DisableBracketedPasteMode {
		t.Fatal("The renderer disabled bracketed paste")
	}
	m.startBrowse(browseControl())
	if m.View().DisableBracketedPasteMode {
		t.Fatal("Browse disabled bracketed paste")
	}
}

func TestNativeBrowseWireAndRefusal(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(map[bool]string{false: "opened", true: "refused"}[refusal], func(t *testing.T) {
			f := &protocolFixture{}
			if refusal {
				f.appError = &wireError{Code: "no_app", Message: "No app is connected"}
			}
			c, msgs := startFixture(t, f)
			waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
			m := newModel(c)
			m.ready = true
			m.apply(browseSnapshot("/keep"))
			cmd := m.startBrowse(browseControl())
			id := editorID("start", "dir")
			if m.focus != "edit:"+id || !m.editors[id].Input.Focused() {
				t.Fatal("The request preceded editor focus")
			}
			m.Update(cmd())
			f.mu.Lock()
			calls := f.appCalls
			f.mu.Unlock()
			if calls != 1 || m.editors[id].text() != "/keep" {
				t.Fatal("Wrong call count or lost value")
			}
			if refusal && (m.browse != nil || !strings.Contains(m.status, "No app is connected")) {
				t.Fatal("Refusal was not shown")
			}
			if !refusal && m.browse == nil {
				t.Fatal("Opened ended the wait before input arrived")
			}
		})
	}
}

func TestNativeBrowseAvailabilityAndHit(t *testing.T) {
	m := browseModel()
	for _, r := range m.bodyRows() {
		for _, c := range r.rowControls() {
			if c.Kind != "browse" {
				continue
			}
			if hit, ok := m.hitRows([]row{r}, m.leftMargin()+c.Left, 0); !ok || hit.Kind != "browse" {
				t.Fatal("Browse has no hit area")
			}
			if hit, ok := m.hitRows([]row{r}, m.leftMargin()+c.Left-1, 0); ok && hit.Kind == "browse" {
				t.Fatal("The text box activates Browse")
			}
			m.ready = false
			if m.activate(c) != nil || m.browse != nil {
				t.Fatal("Disconnected Browse ran")
			}
			m.ready = true
			m.operations["pending"] = true
			if m.activate(c) != nil {
				t.Fatal("Browse ran during a pending mutation")
			}
			return
		}
	}
	t.Fatal("Missing Browse control")
}

func TestCleanFieldTakesChangedTreeValue(t *testing.T) {
	m := browseModel()
	e := m.editors[editorID("start", "dir")]
	m.apply(browseSnapshot("/chosen"))
	if e.text() != "/chosen" || e.dirty() {
		t.Fatal("Clean field did not take the chosen value")
	}
	e.setText("/typed")
	m.apply(browseSnapshot("/remote"))
	if e.text() != "/typed" {
		t.Fatal("Tree overwrote user input")
	}
	e.Saved = "/typed"
	m.apply(browseSnapshot("/remote"))
	if e.text() != "/typed" {
		t.Fatal("Unchanged tree reset a saved edit")
	}
	m.startBrowse(browseControl())
	m.apply(browseSnapshot("/new"))
	if e.text() != "/typed" {
		t.Fatal("Tree changed the value while the picker waited")
	}
}
