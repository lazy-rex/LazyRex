package main

import (
	"context"
	"fmt"
	"reflect"
	"time"

	tea "charm.land/bubbletea/v2"
)

type nativeBrowse struct {
	Editor   string
	Sequence uint64
}
type nativeBrowseResult struct {
	Sequence uint64
	Err      error
}

const browseNotice = "Choose a folder in the browser…"

func (m *model) endBrowse() {
	m.browse = nil
	if m.status == browseNotice {
		m.status = ""
	}
}

func (m *model) startBrowse(c control) tea.Cmd {
	if m.browse != nil {
		m.endBrowse()
		return nil
	}
	if !m.ready || m.replacement != nil || m.closePending || m.closeRunning || m.reloadPending || len(m.operations) > 0 {
		m.status = "The folder browser is unavailable while the view waits for the hub."
		return nil
	}
	n := m.node(c.Node)
	if n == nil {
		return nil
	}
	valid := false
	for _, f := range n.Fields {
		if f.ID == c.Field && f.Kind == "text" && f.Browse == "native" && m.fieldVisible(n.ID, f) {
			valid = true
		}
	}
	id := editorID(c.Node, c.Field)
	if !valid || m.editors[id] == nil {
		return nil
	}
	m.setFocus("edit:" + id)
	m.revealFocus()
	m.browseSequence++
	seq := m.browseSequence
	m.browse = &nativeBrowse{Editor: id, Sequence: seq}
	m.status = browseNotice
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var out struct {
			Status string `json:"status"`
		}
		err := m.client.call(ctx, "hub.view.app_action", map[string]any{"action": "pane.insert_path"}, &out)
		if err == nil && out.Status != "opened" {
			err = fmt.Errorf("unexpected browser status %q", out.Status)
		}
		// The app action has no operation key. Never retry an unknown result automatically.
		return nativeBrowseResult{Sequence: seq, Err: err}
	}
}

// A picker result is a bracketed paste. Ordinary typing preserves the existing field value.
func (m *model) browseInput(id string, msg tea.Msg) {
	b := m.browse
	if b == nil || b.Editor != id {
		return
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.Text != "" {
			m.endBrowse()
		}
	case tea.PasteMsg:
		m.editors[id].setText("")
		m.endBrowse()
	}
}

func (m *model) fieldValueChanged(node string, field Field) bool {
	if old := m.node(node); old != nil {
		for _, f := range old.Fields {
			if f.ID == field.ID {
				return !reflect.DeepEqual(f.Value, field.Value)
			}
		}
	}
	return true
}

// A new explicit field revision makes the plugin's replacement authoritative.
func (m *model) fieldRevisionChanged(node string, field Field) bool {
	if field.Rev == nil {
		return false
	}
	if old := m.node(node); old != nil {
		for _, f := range old.Fields {
			if f.ID == field.ID {
				return f.Rev == nil || *f.Rev != *field.Rev
			}
		}
	}
	return true
}
