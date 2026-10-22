package main

import (
	tea "charm.land/bubbletea/v2"
	"errors"
	"strings"
	"testing"
)

func TestCloseWaitsForSavedDraft(t *testing.T) {
	m := testModel()
	m.editors["composer"].setText("keep this draft")
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl})
	runMenuCmd(cmd)
	if !m.closePending || m.closeRunning {
		t.Fatal("close bypassed draft save")
	}
	req := <-m.client.Requests
	if req.Method != "hub.view.draft.save" {
		t.Fatalf("wrong request: %#v", req)
	}
	_, cmd = m.Update(completed{Request: req, Outcome: operationResult{Status: "succeeded", Result: map[string]any{"text": "keep this draft", "draft_revision": "2"}}})
	if !m.closeRunning || m.closePending || cmd == nil {
		t.Fatal("close did not start after the save")
	}
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.editors["composer"].text() != "keep this draft" {
		t.Fatal("input changed a draft after close admission")
	}
	// A failed close keeps the view. A later Ctrl+Q retries it.
	m.Update(closeFinished{err: errors.New("test refusal")})
	if m.closeReady || m.closeRunning || !strings.Contains(m.status, "Ctrl+Q to retry") {
		t.Fatal("failed close was not recoverable")
	}
	m.Update(tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl})
	if !m.closeRunning {
		t.Fatal("Ctrl+Q did not retry close")
	}
	_, cmd = m.Update(closeFinished{})
	if !m.closeReady || cmd == nil {
		t.Fatal("successful close did not quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("successful close returned no quit")
	}
}

func TestCloseWaitsForOperationAndConflict(t *testing.T) {
	m := testModel()
	m.operations["pending"] = true
	m.Update(tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl})
	if m.closeRunning {
		t.Fatal("close bypassed unresolved operation")
	}
	delete(m.operations, "pending")
	m.editors["composer"].Conflict = true
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.closeRunning || !strings.Contains(m.status, "conflict") {
		t.Fatal("close bypassed draft conflict")
	}
}

func TestCloseUsesCurrentAttachment(t *testing.T) {
	f := &protocolFixture{}
	c, msgs := startFixture(t, f)
	waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	if err := c.closeView(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.attachment = "old"
	c.mu.Unlock()
	if err := c.closeView(); err == nil {
		t.Fatal("stale close succeeded")
	}
}
