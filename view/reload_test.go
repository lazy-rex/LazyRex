package main

import (
	tea "charm.land/bubbletea/v2"
	"testing"
)

func TestReloadWaitsForSaveResult(t *testing.T) {
	m := testModel()
	m.editors["composer"].Area.SetValue("keep this")
	req := request(t, m, m.save("composer"))
	m.Update(reloadRequested{})
	if m.reloadReady {
		t.Fatal("Reload lost an outstanding save")
	}
	_, cmd := m.Update(completed{req, operationResult{Status: "succeeded", Result: map[string]any{"text": "keep this", "draft_revision": "2"}}})
	if !m.reloadReady || cmd == nil {
		t.Fatal("Reload did not resume after save")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("Reload must restore the terminal before exec")
	}
}

func TestReloadPreservesUnsafeState(t *testing.T) {
	cases := map[string]func(*model){
		"conflict": func(m *model) { m.editors["composer"].Conflict = true },
		"removed":  func(m *model) { m.recovery["removed"] = "keep" },
		"form text": func(m *model) {
			e := newEditor("original", "", "text")
			e.Area.SetValue("changed")
			m.editors["form/text"] = e
		},
		"form choice": func(m *model) {
			e := newEditor("", "", "boolean")
			e.Choice = true
			e.SavedChoice = false
			m.editors["form/choice"] = e
		},
		"operation":    func(m *model) { m.operations["unknown"] = true },
		"disconnected": func(m *model) { m.ready = false },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			m := testModel()
			setup(m)
			m.Update(reloadRequested{})
			if m.reloadReady {
				t.Fatal("Unsafe reload")
			}
		})
	}
}

func TestReloadTracksGenericActionBeforeDispatch(t *testing.T) {
	m := testModel()
	req := request(t, m, m.enqueue("hub.view.action", "widget", map[string]any{}))
	m.Update(reloadRequested{})
	if m.reloadReady {
		t.Fatal("Reload raced action dispatch")
	}
	m.Update(completed{req, operationResult{Status: "succeeded"}})
	if !m.reloadReady {
		t.Fatal("Completed action blocked reload")
	}
}
