package main

import (
	tea "charm.land/bubbletea/v2"
	"strings"
	"testing"
)

func panelModel() *model {
	c := &viewClient{Route: "/panels/threads"}
	m := newModel(c)
	m.ready = true
	m.snapshot.Route = c.Route
	return m
}
func releasePanel(m *model, id string) string {
	answer := ""
	_, cmd := m.Update(replacementRequested{Action: "view.release", ID: id, Title: "Worktrees", valid: func() bool { return true }, reply: func(status string) tea.Cmd { return func() tea.Msg { answer = status; return nil } }})
	if cmd != nil {
		cmd()
	}
	return answer
}
func TestReleaseFreezesBeforeReply(t *testing.T) {
	m := panelModel()
	m.editors["field"] = newEditor("saved", "1", "text")
	m.focus = "edit:field"
	answer := ""
	_, cmd := m.Update(replacementRequested{Action: "view.release", ID: "r1", Title: "Worktrees", valid: func() bool { return true }, reply: func(status string) tea.Cmd {
		if m.replacement == nil || m.client.replacementID != "r1" {
			t.Fatal("Release answered before input froze")
		}
		answer = status
		return nil
	}})
	if answer != "released" || cmd != nil {
		t.Fatalf("Release: %q", answer)
	}
	m.Update(tea.PasteMsg{Content: "lost input"})
	if m.editors["field"].text() != "saved" {
		t.Fatal("Frozen editor accepted input")
	}
	if releasePanel(m, "r2") != "conflict" {
		t.Fatal("Another replacement displaced an active replacement")
	}
}
func TestReplacementConfirmPreservesInput(t *testing.T) {
	m := panelModel()
	e := newEditor("saved", "1", "text")
	e.setText("unsaved")
	m.editors["field"] = e
	m.focus = "edit:field"
	if releasePanel(m, "r1") != "confirm" {
		t.Fatal("Unsaved input did not require confirmation")
	}
	screen := m.View().Content
	if !strings.Contains(screen, "Keep editing") || !strings.Contains(screen, "Discard and replace") {
		t.Fatal(screen)
	}
	m.Update(replacementRecovered{ID: "other", State: replacementState{State: "cancelled"}})
	if m.replacement == nil {
		t.Fatal("An unrelated result resumed the editor")
	}
	m.Update(replacementRecovered{ID: "r1", State: replacementState{State: "cancelled"}})
	if m.replacement != nil || m.focus != "edit:field" || e.text() != "unsaved" {
		t.Fatal("Keep editing did not preserve the editor")
	}
}
func TestReplacementCannotDiscardUnresolvedWork(t *testing.T) {
	for _, reason := range []string{"operation", "conflict", "pending", "offline"} {
		t.Run(reason, func(t *testing.T) {
			m := panelModel()
			e := newEditor("saved", "1", "composer")
			m.editors["composer"] = e
			switch reason {
			case "operation":
				m.operations["op"] = true
			case "conflict":
				e.Conflict = true
			case "pending":
				e.Pending = "op"
			case "offline":
				m.ready = false
			}
			if releasePanel(m, "r1") != "confirm" {
				t.Fatal("Unsafe release")
			}
			if m.chooseReplacement("discard") != nil || m.replacement.State != "confirming" {
				t.Fatal("Discard bypassed unresolved work")
			}
			if m.chooseReplacement("keep") == nil || m.replacement.State != "deciding" {
				t.Fatal("Keep editing was unavailable")
			}
		})
	}
}
func TestReplacementRecoveryAndResumeMatchIdentity(t *testing.T) {
	m := panelModel()
	releasePanel(m, "r1")
	m.Update(replacementRequested{Action: "view.resume", ID: "old", valid: func() bool { return true }, reply: func(string) tea.Cmd { return nil }})
	if m.replacement == nil {
		t.Fatal("A stale resume unfroze the editor")
	}
	for _, state := range []string{"opening", "closing", "done", "user_closed"} {
		m.Update(replacementRecovered{ID: "r1", State: replacementState{State: state, NextTitle: "Worktrees"}})
		if m.replacement == nil {
			t.Fatalf("Recovery %s unfroze the editor", state)
		}
	}
	m.Update(replacementRequested{Action: "view.resume", ID: "r1", valid: func() bool { return false }, reply: func(string) tea.Cmd { return nil }})
	if m.replacement == nil {
		t.Fatal("An obsolete attachment resumed the editor")
	}
	m.Update(replacementRequested{Action: "view.resume", ID: "r1", valid: func() bool { return true }, reply: func(string) tea.Cmd { return nil }})
	if m.replacement != nil || m.client.replacementID != "" {
		t.Fatal("Matching resume did not restore input")
	}
}
func TestPanelEscapeUsesHubClose(t *testing.T) {
	m := panelModel()
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !m.closeRunning || cmd == nil {
		t.Fatal("Escape did not start an intentional hub close")
	}
}

func TestRestartedRendererRecoversActiveReplacement(t *testing.T) {
	m := panelModel()
	m.Update(replacementRecovered{ID: "r1", Initial: true, State: replacementState{ID: "r1", State: "opening", NextTitle: "Worktrees"}})
	if m.replacement == nil || m.client.replacementID != "r1" {
		t.Fatal("An initial attachment did not freeze the active replacement")
	}
	m.Update(tea.PasteMsg{Content: "ignored"})
	if m.replacement.State != "opening" {
		t.Fatal("Input changed the recovered state")
	}
}
func TestRecoveredReleaseStillChecksUnsavedInput(t *testing.T) {
	m := panelModel()
	e := newEditor("saved", "1", "text")
	e.setText("unsaved")
	m.editors["field"] = e
	m.Update(replacementRecovered{ID: "r1", Initial: true, State: replacementState{ID: "r1", State: "releasing", NextTitle: "Worktrees"}})
	if releasePanel(m, "r1") != "confirm" {
		t.Fatal("A recovered release skipped unsaved input")
	}
}

func TestReplacementAfterSourceActionPreservesLaterInput(t *testing.T) {
	for _, later := range []bool{false, true} {
		m := panelModel()
		e := newEditor("", "1", "text")
		e.setText("submitted")
		if later {
			e.setText("typed after submission")
		}
		m.editors[editorID("form", "title")] = e
		m.operations["submit"] = true
		if releasePanel(m, "r1") != "confirm" {
			t.Fatal("Pending submit bypassed release")
		}
		req := mutation{Method: "hub.view.action", Key: "submit", Node: "form", Args: map[string]any{"input": map[string]any{"values": map[string]any{"title": "submitted"}}}}
		_, cmd := m.Update(completed{Request: req, Outcome: operationResult{Status: "succeeded", Effects: "done"}})
		if later {
			if m.replacement.State != "confirming" || e.text() != "typed after submission" {
				t.Fatal("Completion discarded later input")
			}
		} else if m.replacement.State != "deciding" || cmd == nil {
			t.Fatal("Clean submitted values did not release automatically")
		}
	}
}

func TestPanelCloseDiscardsLocalValuesButWaitsForOperations(t *testing.T) {
	m := panelModel()
	e := newEditor("", "1", "text")
	e.setText("local input")
	m.editors["field"] = e
	m.operations["pending"] = true
	if m.closePanel() != nil || m.closeRunning {
		t.Fatal("Panel closed with an unresolved mutation")
	}
	delete(m.operations, "pending")
	if m.closePanel() == nil || !m.closeRunning {
		t.Fatal("Intentional panel close could not discard local values")
	}
}
