package main

import (
	tea "charm.land/bubbletea/v2"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func largeConversationModel() *model {
	m := testModel()
	m.width = 200
	m.height = 60
	s := conversationSnapshot(30)
	for i := range s.Tree.Nodes[0].Messages {
		s.Tree.Nodes[0].Messages[i].Text = strings.Repeat("## Implementation review\n\nThis is a long message with **bold text**, `code`, and detailed implementation notes.\n\n- Preserve the draft and cursor.\n- Review the resulting state.\n\n", 8)
	}
	if path := os.Getenv("LAZYREX_PERF_FIXTURE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		var messages []struct {
			From string `json:"from"`
			Body string `json:"body"`
		}
		if err = json.Unmarshal(data, &messages); err != nil {
			panic(err)
		}
		s = conversationSnapshot(len(messages))
		for i, msg := range messages {
			s.Tree.Nodes[0].Messages[i].Author = msg.From
			s.Tree.Nodes[0].Messages[i].Text = msg.Body
		}
	}
	m.apply(s)
	m.View()
	return m
}
func BenchmarkLargeConversationView(b *testing.B) {
	m := largeConversationModel()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.View()
	}
}
func BenchmarkLargeConversationMotionFrame(b *testing.B) {
	m := largeConversationModel()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Update(tea.MouseMotionMsg{X: 10 + i%180, Y: 10 + i%40})
		m.View()
	}
}

func TestTypingAfterMotionBurst(t *testing.T) {
	m := largeConversationModel()
	e := m.editors["composer"]
	m.setFocus("edit:composer")
	m.View()
	start := time.Now()
	for i := 0; i < 256; i++ {
		m.Update(tea.MouseMotionMsg{X: 10 + i%180, Y: 10 + i%40})
		m.View()
	}
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	frame := m.View()
	elapsed := time.Since(start)
	if e.Area.Value() != "x" || !strings.Contains(frame.Content, "x") {
		t.Fatal("Key did not reach editor and frame")
	}
	// This budget includes race instrumentation and a burst of 256 pointer events.
	if elapsed > 250*time.Millisecond {
		t.Fatalf("Motion burst plus key frame took %v; budget is 250ms", elapsed)
	}
	t.Logf("256 motion events plus key frame: %v", elapsed)
}
func TestMessageRowCacheInvalidation(t *testing.T) {
	m := testModel()
	msg := Message{ID: "1", Author: "Claude", At: "2026-10-16T21:00:00Z", Text: "first"}
	first := m.historyMessageRows("h", 0, msg, nil)
	msg.Text = "second"
	second := m.historyMessageRows("h", 0, msg, nil)
	if len(first) == 0 || len(second) == 0 || &first[0] == &second[0] {
		t.Fatal("Changed text reused old rows")
	}
	m.width = 140
	wide := m.historyMessageRows("h", 0, msg, nil)
	if &wide[0] == &second[0] {
		t.Fatal("Resize reused old rows")
	}
	m.dark = !m.dark
	dark := m.historyMessageRows("h", 0, msg, nil)
	if &dark[0] == &wide[0] {
		t.Fatal("Theme change reused old rows")
	}
	again := m.historyMessageRows("h", 0, msg, nil)
	if &again[0] != &dark[0] {
		t.Fatal("Unchanged message was rendered again")
	}
}

func BenchmarkLargeConversationKeyFrame(b *testing.B) {
	m := largeConversationModel()
	m.setFocus("edit:composer")
	m.View()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
		m.View()
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		m.View()
	}
}
