package main

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMenuOpensSidePaneWithNormalAction(t *testing.T) {
	for _, event := range []tea.Msg{tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}, tea.MouseClickMsg{Button: tea.MouseRight, X: 3, Y: 3}} {
		m := testModel()
		s := testSnapshot("saved", "1")
		s.Tree.Nodes = append([]Node{{ID: "thread-menu", Type: "menu", Label: "Thread…", Action: &Action{Name: "open", Enabled: true}}}, s.Tree.Nodes...)
		m.apply(s)
		m.setFocus("edit:composer")
		m.editors["composer"].Area.SetValue("unsaved draft")
		_, cmd := m.Update(event)
		runMenuCmd(cmd)
		select {
		case req := <-m.client.Requests:
			if req.Method != "hub.view.action" || req.Node != "thread-menu" || req.Args["action"] != "open" {
				t.Fatalf("wrong menu action: %#v", req)
			}
		default:
			t.Fatal("menu action missing")
		}
		if m.editors["composer"].Area.Value() != "unsaved draft" {
			t.Fatal("opening the menu changed the draft")
		}
	}
}
func runMenuCmd(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, c := range batch {
			runMenuCmd(c)
		}
	}
}

func TestConditionalFieldsPreserveSelections(t *testing.T) {
	m := testModel()
	s := testSnapshot("saved", "1")
	s.Tree.Nodes = []Node{{ID: "new", Type: "form", Fields: []Field{
		{ID: "others", Kind: "boolean", Value: false},
		{ID: "remote", Kind: "boolean", Value: false, VisibleWhen: &FieldCondition{Field: "others", Value: true}},
	}, SubmitAction: &Action{Name: "create", Enabled: true}}}
	m.apply(s)
	visible := func() bool {
		for _, c := range m.controls() {
			if c.Field == "remote" {
				return true
			}
		}
		return false
	}
	if visible() {
		t.Fatal("remote option visible before toggle")
	}
	m.activate(control{Kind: "choice", Node: "new", Field: "others"})
	if !visible() {
		t.Fatal("remote option stayed hidden")
	}
	m.activate(control{Kind: "choice", Node: "new", Field: "remote"})
	m.activate(control{Kind: "choice", Node: "new", Field: "others"})
	if visible() || m.editors[editorID("new", "remote")].Choice != true {
		t.Fatal("toggle lost selection")
	}
	req := request(t, m, m.activate(control{Kind: "submit", Node: "new", Action: s.Tree.Nodes[0].SubmitAction}))
	values := req.Args["input"].(map[string]any)["values"].(map[string]any)
	if values["remote"] != true {
		t.Fatal("hidden selection lost on submit")
	}
}

func TestFormCancelBeforeFirstSnapshot(t *testing.T) {
	m := newModel(&viewClient{Route: "/forms/new-thread"})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("Escape cannot close a connecting form")
	}
}

func TestFormCancelDiscardsOnlyUnsubmittedValues(t *testing.T) {
	m := testModel()
	m.snapshot.Route = "/forms/new-thread"
	m.editors["composer"].Area.SetValue("local form value")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("Cancel did not exit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("Cancel did not return quit")
	}
	m.operations["pending"] = true
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil || !strings.Contains(m.status, "submitted operation") {
		t.Fatal("Cancel abandoned an unresolved mutation")
	}
	m.snapshot.Route = "/thread/42"
	delete(m.operations, "pending")
	if m.cancelForm() != nil {
		t.Fatal("form cancel closed an ordinary view")
	}
}

func TestSideMenuFitsViewAndTracksPointerAfterRender(t *testing.T) {
	for _, size := range [][2]int{{12, 6}, {38, 24}, {80, 24}} {
		for _, dark := range []bool{false, true} {
			m := testModel()
			s := testSnapshot("", "1")
			s.Route = "/forms/thread-menu"
			s.Tree.Nodes = []Node{{ID: "thread-menu", Type: "list", Items: []Item{
				{ID: "rename", Label: "Rename…", Action: &Action{Name: "rename", Enabled: true}},
				{ID: "members", Label: "Members…", Action: &Action{Name: "members", Enabled: true}},
			}}}
			m.apply(s)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.dark = dark
			m.themeReady = true
			view := m.View()
			lines := strings.Split(view.Content, "\n")
			if len(lines) > size[1] {
				t.Fatalf("menu height %d exceeds %d", len(lines), size[1])
			}
			for _, line := range lines {
				if lipgloss.Width(line) > size[0] {
					t.Fatalf("menu exceeds width %d", size[0])
				}
			}
			if dir := os.Getenv("LAZYREX_SNAPSHOTS"); dir != "" {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("menu-%dx%d-dark-%t.ansi", size[0], size[1], dark)), []byte(view.Content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for y, r := range m.screenRows() {
				if r.Control != nil && r.Control.ID == "thread-menu:members" {
					m.Update(tea.MouseMotionMsg{X: m.leftMargin() + r.Control.Left + 1, Y: y})
					if m.hover != r.Control.ID {
						t.Fatal("cached pointer path bypassed menu")
					}
				}
			}
		}
	}
}

func TestSpaceTogglesChoiceAndRemainsTextInEditor(t *testing.T) {
	m := testModel()
	s := testSnapshot("saved", "1")
	s.Tree.Nodes = append(s.Tree.Nodes, Node{ID: "form", Type: "form", Fields: []Field{{ID: "other", Kind: "boolean", Value: false}, {ID: "remote", Kind: "boolean", Value: false, VisibleWhen: &FieldCondition{Field: "other", Value: true}}}})
	m.apply(s)
	m.setFocus("choice:" + editorID("form", "other"))
	m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if m.editors[editorID("form", "other")].Choice != true {
		t.Fatal("Space did not toggle checkbox")
	}
	visible := false
	for _, c := range m.controls() {
		if c.Field == "remote" {
			visible = true
		}
	}
	if !visible {
		t.Fatal("Space toggle did not show remote field")
	}
	m.setFocus("edit:composer")
	before := m.editors["composer"].Area.Value()
	m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if len(m.editors["composer"].Area.Value()) != len(before)+1 {
		t.Fatal("Space was consumed in composer")
	}
}

func TestFailedEffectKeepsSavedValuesAndRetriesOnlyEffect(t *testing.T) {
	m := testModel()
	s := testSnapshot("saved", "1")
	s.Route = "/forms/new-thread"
	s.Tree.Nodes = []Node{{ID: "new", Type: "form", Fields: []Field{{ID: "title", Kind: "text", Value: ""}}, SubmitAction: &Action{Name: "submit", Enabled: true}}}
	m.apply(s)
	m.editors[editorID("new", "title")].Area.SetValue("Review")
	req := request(t, m, m.activate(control{Kind: "submit", Node: "new", Action: s.Tree.Nodes[0].SubmitAction}))
	m.finish(completed{Request: req, Outcome: operationResult{Status: "succeeded", Effects: "failed", EffectsError: "Target unavailable"}})
	if m.editors[editorID("new", "title")].dirty() {
		t.Fatal("committed form values remain dirty")
	}
	if !strings.Contains(m.status, "change was saved") || m.effectFailure == nil {
		t.Fatal("effect failure was hidden")
	}
	if m.activate(control{Kind: "submit", Node: "new", Action: s.Tree.Nodes[0].SubmitAction}) != nil {
		t.Fatal("saved form was submitted again")
	}
	retry := request(t, m, m.activate(control{Kind: "retry_effect"}))
	if retry.Method != "hub.view.effect.retry" || retry.Key != req.Key {
		t.Fatal("retry reran the mutation")
	}
	if m.cancelForm() != nil {
		t.Fatal("cancel abandoned effect retry")
	}
	m.finish(completed{Request: retry, Outcome: operationResult{Status: "succeeded", Effects: "done"}})
	if m.effectFailure != nil {
		t.Fatal("successful retry kept failure")
	}
}

// The menu and older history are reached from the keyboard (Ctrl+O) or a right click, not buttons.
func TestNoMenuOrOlderMessagesButtons(t *testing.T) {
	m := testModel()
	s := testSnapshot("saved", "1")
	history := Node{ID: "history", Type: "history", HasMore: true, LoadAction: &Action{Name: "load", Label: "Older messages", Enabled: true},
		Messages: []Message{{ID: "1", Author: "Claude", Text: "hello"}}}
	s.Tree.Nodes = append([]Node{{ID: "thread-menu", Type: "menu", Label: "Thread…", Action: &Action{Name: "open", Enabled: true}}, history}, s.Tree.Nodes...)
	m.apply(s)
	out := ansi.Strip(m.View().Content)
	if strings.Contains(out, "Older messages") || strings.Contains(out, "▄") && strings.Contains(out, "  Thread…  ") {
		t.Fatalf("a menu or Older messages button is shown:\n%s", out)
	}
	if !strings.Contains(out, "Ctrl+O Thread…") {
		t.Fatalf("the footer does not name the Ctrl+O menu:\n%s", out)
	}
}
