package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

func (m *model) fieldVisible(node string, field Field) bool {
	if field.VisibleWhen == nil {
		return true
	}
	e := m.editors[editorID(node, field.VisibleWhen.Field)]
	if e == nil {
		return false
	}
	value, ok := e.Choice.(bool)
	return ok && value == field.VisibleWhen.Value
}

func (m *model) openFirstMenu() tea.Cmd {
	for _, n := range m.snapshot.Tree.Nodes {
		if n.Type == "menu" {
			return m.openMenu(n.ID)
		}
	}
	return nil
}

// The hub opens a menu in a side pane through the normal action path.
func (m *model) openMenu(node string) tea.Cmd {
	n := m.node(node)
	if n == nil || n.Type != "menu" || n.Action == nil || !n.Action.Enabled {
		return nil
	}
	return m.activate(control{Node: n.ID, Action: n.Action})
}

// Cancelling a form discards its local values, but cannot cancel an unresolved mutation.
func (m *model) isForm() bool {
	route := m.snapshot.Route
	if route == "" && m.client != nil {
		route = m.client.Route
	}
	return strings.HasPrefix(route, "/forms/")
}

func (m *model) cancelForm() tea.Cmd {
	if !m.isForm() {
		return nil
	}
	if len(m.operations) > 0 {
		m.status = "Wait for the submitted operation before closing this form."
		return nil
	}
	return m.quit()
}

// Retry only the unfinished UI step. The plugin mutation keeps its original operation key.
func (m *model) retryEffect() tea.Cmd {
	if m.effectFailure == nil || !m.ready {
		return nil
	}
	key := m.effectFailure.Request.Key
	if m.operations[key] {
		return nil
	}
	m.operations[key] = true
	req := mutation{Method: "hub.view.effect.retry", Key: key, Node: m.effectFailure.Request.Node, Args: map[string]any{"operation_key": key}}
	m.status = "Retrying the next step…"
	return func() tea.Msg { m.client.Requests <- req; return nil }
}
