package main

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func conversationSnapshot(count int) Snapshot {
	s := testSnapshot("", "1")
	s.Tree.Title = "Team"
	history := Node{ID: "history", Type: "history", PageSize: 100}
	for i := 0; i < count; i++ {
		history.Messages = append(history.Messages, Message{ID: fmt.Sprint(i), Author: []string{"Claude", "Codex", "User"}[i%3], Own: i%3 == 2, At: "2026-10-16T21:21:17Z", Text: fmt.Sprintf("Message %d: **Ready for review.**\n\n- Keep the composer within reach\n- Preserve `drafts` across reconnects", i)})
	}
	s.Tree.Nodes = append([]Node{history}, s.Tree.Nodes...)
	return s
}
func TestLayoutFitsAndPinsComposer(t *testing.T) {
	for _, size := range [][2]int{{38, 24}, {80, 24}, {140, 40}} {
		for _, dark := range []bool{false, true} {
			name := fmt.Sprintf("%dx%d-dark-%t", size[0], size[1], dark)
			t.Run(name, func(t *testing.T) {
				m := testModel()
				m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				m.dark = dark
				m.status = "Connected"
				m.apply(conversationSnapshot(8))
				m.editors["composer"].Area.SetValue("A draft ready to send")
				view := m.View()
				lines := strings.Split(view.Content, "\n")
				if len(lines) != size[1] {
					t.Fatalf("Height %d, want %d", len(lines), size[1])
				}
				for _, line := range lines {
					if lipgloss.Width(line) > size[0] {
						t.Fatalf("Line exceeds width: %q", line)
					}
				}
				send := -1
				for i, r := range m.screenRows() {
					if r.Control != nil && r.Control.Kind == "send" {
						send = i
					}
				}
				// Send is on the first footer row of the composer. A narrow pane has a second hint row.
				want := size[1] - 3
				if size[0] < 60 {
					want--
				}
				if send != want {
					t.Fatalf("Send row %d, want %d", send, want)
				}
				if view.WindowTitle != "Team" || !view.ReportFocus {
					t.Fatal("Missing title or focus reporting")
				}
				m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
				after := -1
				for i, r := range m.screenRows() {
					if r.Control != nil && r.Control.Kind == "send" {
						after = i
					}
				}
				if after != send {
					t.Fatal("History scroll moved the composer")
				}
				if dir := os.Getenv("LAZYREX_SNAPSHOTS"); dir != "" {
					if err := os.WriteFile(filepath.Join(dir, name+".ansi"), []byte(view.Content), 0600); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
func TestHistoryFollowsBottomButPreservesReadingPosition(t *testing.T) {
	m := testModel()
	m.apply(conversationSnapshot(12))
	if m.scroll != len(m.bodyRows())-m.bodyHeight() {
		t.Fatal("History did not start at the latest message")
	}
	m.scroll = 4
	anchor := m.bodyRows()[4].Anchor
	m.apply(conversationSnapshot(13))
	if m.bodyRows()[m.scroll].Anchor != anchor {
		t.Fatal("A new message moved the reading position")
	}
	m.scroll = len(m.bodyRows()) - m.bodyHeight()
	m.apply(conversationSnapshot(14))
	if m.scroll != len(m.bodyRows())-m.bodyHeight() {
		t.Fatal("History stopped following the bottom")
	}
}
func TestFormShortcutAndMouseBounds(t *testing.T) {
	m := testModel()
	s := testSnapshot("", "1")
	s.Tree.Nodes = []Node{{ID: "list", Type: "list", Items: []Item{{ID: "thread", Label: "Thread", Action: &Action{Name: "open", Enabled: true}}}}, {ID: "form", Type: "form", Fields: []Field{{ID: "title", Kind: "text", Label: "Title"}}}}
	m.apply(s)
	if m.scroll != 0 {
		t.Fatal("The home view did not start at the top")
	}
	m.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	if m.focus != "edit:form/title" {
		t.Fatal("The form shortcut missed its field")
	}
	m = testModel()
	idx := -1
	for i, r := range m.screenRows() {
		if r.Control != nil && r.Control.Kind == "send" {
			idx = i
		}
	}
	m.Update(tea.MouseClickMsg{X: m.leftMargin(), Y: idx, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: m.leftMargin(), Y: idx, Button: tea.MouseLeft})
	if m.editors["composer"].Pending != "" {
		t.Fatal("The send hint acted as a button")
	}
}
func TestMarkdownAndThemePreserveContent(t *testing.T) {
	m := testModel()
	text := ansi.Strip(m.messageBody("**Bold** and `code`\n\n- One\n- Two"))
	if !strings.Contains(text, "Bold") || !strings.Contains(text, "Two") || strings.Contains(text, "**") {
		t.Fatal(text)
	}
	m.Update(tea.BackgroundColorMsg{Color: color.Black})
	if !m.dark {
		t.Fatal("Dark background was ignored")
	}
	styles := m.editors["composer"].Area.Styles()
	if styles.Focused.CursorLine.GetBackground() != styles.Focused.Text.GetBackground() {
		t.Fatal("Cursor line has a separate background")
	}
}
func TestSeenRequiresFocusAndStopsAfterBlur(t *testing.T) {
	f := &protocolFixture{}
	client, messages := startFixture(t, f)
	m := newModel(client)
	deadline := time.After(2 * time.Second)
	ready := false
	for !ready {
		select {
		case msg := <-messages:
			if c, ok := msg.(connectionMsg); ok && c.Ready {
				ready = true
			}
		case <-deadline:
			t.Fatal("No connection")
		}
	}
	m.Update(testSnapshot("saved", "1"))
	time.Sleep(30 * time.Millisecond)
	f.mu.Lock()
	before := f.seen
	f.mu.Unlock()
	if before != 0 {
		t.Fatal("Background view marked read")
	}
	m.Update(tea.FocusMsg{})
	until := time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		seen := f.seen
		f.mu.Unlock()
		if seen > 0 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("Focused view did not send seen")
		}
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	steady := f.seen
	f.mu.Unlock()
	m.Update(testSnapshot("typed draft", "2"))
	time.Sleep(30 * time.Millisecond)
	f.mu.Lock()
	draftSeen := f.seen
	f.mu.Unlock()
	if draftSeen != steady {
		t.Fatal("A draft-only snapshot sent a read notice")
	}
	m.Update(conversationSnapshot(1))
	until = time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		current := f.seen
		f.mu.Unlock()
		if current > steady {
			break
		}
		if time.Now().After(until) {
			t.Fatal("New history did not send a read notice")
		}
		time.Sleep(time.Millisecond)
	}
	m.Update(tea.BlurMsg{})
	f.mu.Lock()
	before = f.seen
	f.mu.Unlock()
	m.Update(conversationSnapshot(2))
	time.Sleep(30 * time.Millisecond)
	f.mu.Lock()
	after := f.seen
	f.mu.Unlock()
	if after != before {
		t.Fatal("Blurred view marked a new snapshot read")
	}
}

func TestSmallPanesAndMultipleComposersStayScrollable(t *testing.T) {
	m := testModel()
	m.Update(tea.WindowSizeMsg{Width: 38, Height: 12})
	if len(m.composerRows()) != 0 || len(strings.Split(m.View().Content, "\n")) != 12 {
		t.Fatal("Small pane overflowed")
	}
	m = testModel()
	s := testSnapshot("A draft", "1")
	second := s.Tree.Nodes[0]
	second.ID = "second"
	s.Tree.Nodes = append(s.Tree.Nodes, second)
	m.apply(s)
	if len(m.composerRows()) != 0 {
		t.Fatal("Multiple composers must remain scrollable")
	}
	found := map[string]bool{}
	for _, c := range m.controls() {
		if c.Kind == "send" {
			found[c.Node] = true
		}
	}
	if !found["composer"] || !found["second"] {
		t.Fatal("A composer became unreachable")
	}
}
