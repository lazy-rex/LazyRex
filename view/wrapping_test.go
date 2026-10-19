package main

import (
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
)

func TestBubblePreservesMarkdownLineWrapping(t *testing.T) {
	text := "I will add scrollbar hover feedback now: a wider track on hover, a highlighted thumb under the pointer, and a highlighted thumb during dragging. I will retain the fast unchanged-hover path and run the pointer regression test. For the next improvement, I recommend a New messages button that appears while you read older messages and jumps to the latest message when selected."
	for _, width := range []int{38, 39, 40, 100, 101, 102, 140, 141, 142, 200} {
		m := testModel()
		m.width = width
		want := strings.Split(ansi.Strip(m.messageBody(text)), "\n")
		lines := m.messageLines(Message{Author: "Codex", Text: text}, nil)
		got := lines[2 : len(lines)-1]
		if len(got) != len(want) {
			t.Fatalf("Width%d: bubble has%d text rows, Markdown has%d", width, len(got), len(want))
		}
		for i, line := range got {
			line = strings.TrimSpace(ansi.Strip(line))
			line = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "│"), "│"))
			if line != strings.TrimSpace(want[i]) {
				t.Fatalf("Width%d row%d: got%q want%q", width, i, line, want[i])
			}
		}
	}
}

func TestWidgetWrapsAtWords(t *testing.T) {
	m := testModel()
	m.width = 38
	s := testSnapshot("", "1")
	s.Tree.Nodes = []Node{{ID: "intro", Type: "widget", Text: "Every control here runs the real renderer: hover, click, scroll and type."}}
	m.apply(s)
	for _, r := range m.bodyRows() {
		text := strings.TrimSpace(ansi.Strip(r.Text))
		for _, word := range strings.Fields(text) {
			if word == "cl" || word == "ick," {
				t.Fatalf("Widget split a word: %q", text)
			}
		}
	}
}
