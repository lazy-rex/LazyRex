package main

import (
	tea "charm.land/bubbletea/v2"
	"strings"
	"testing"
)

func TestRetireWaitsForSaveAndDoesNotReload(t *testing.T) {
	m := testModel()
	m.editors["composer"].Area.SetValue("keep this")
	req := request(t, m, m.save("composer"))
	m.Update(reloadRequested{})
	m.Update(retireRequested{})
	if m.retireReady || m.reloadReady {
		t.Fatal("Exited before save result")
	}
	_, cmd := m.Update(completed{req, operationResult{Status: "succeeded", Result: map[string]any{"text": "keep this", "draft_revision": "2"}}})
	if !m.retireReady || m.reloadReady || cmd == nil {
		t.Fatal("Retirement did not take priority over reload")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("Retirement did not quit cleanly")
	}
}

func TestRetirePreservesUnsafeState(t *testing.T) {
	cases := map[string]func(*model){
		"conflict": func(m *model) { m.editors["composer"].Conflict = true },
		"recovery": func(m *model) { m.recovery["removed"] = "keep" },
		"form": func(m *model) {
			e := newEditor("original", "", "text")
			e.Area.SetValue("changed")
			m.editors["form/text"] = e
		},
		"operation":    func(m *model) { m.operations["unknown"] = true },
		"disconnected": func(m *model) { m.ready = false },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			m := testModel()
			setup(m)
			m.Update(retireRequested{})
			if m.retireReady || m.reloadReady {
				t.Fatal("Unsafe retirement")
			}
			if !strings.HasPrefix(m.status, "Retirement waits") {
				t.Fatalf("Wrong status: %s", m.status)
			}
		})
	}
}

func TestRetireWaitsForActionCompletion(t *testing.T) {
	m := testModel()
	req := request(t, m, m.enqueue("hub.view.action", "widget", map[string]any{}))
	m.Update(retireRequested{})
	if m.retireReady {
		t.Fatal("Retirement raced action dispatch")
	}
	m.Update(completed{req, operationResult{Status: "succeeded"}})
	if !m.retireReady || m.reloadReady {
		t.Fatal("Retirement did not resume")
	}
}
