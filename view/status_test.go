package main

import (
	"encoding/base64"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestStatusSequence(t *testing.T) {
	if got := statusSequence(nil); got != statusClear {
		t.Fatalf("nil: %q", got)
	}
	if got := statusSequence(&Status{State: "bogus", Msg: "x"}); got != statusClear {
		t.Fatalf("unknown state: %q", got)
	}
	got := statusSequence(&Status{State: "done", Msg: "3 unread\nfrom Codex"})
	want := "\x1b]7501;state=done:app=rex:msg=" + base64.StdEncoding.EncodeToString([]byte("3 unread from Codex")) + "\x1b\\"
	if got != want {
		t.Fatalf("done: %q, want %q", got, want)
	}
	if got := statusSequence(&Status{State: "done", Kind: "question"}); strings.Contains(got, "kind=") {
		t.Fatalf("kind outside blocked: %q", got)
	}
	if got := statusSequence(&Status{State: "blocked", Kind: "question"}); !strings.Contains(got, ":kind=question") {
		t.Fatalf("blocked kind: %q", got)
	}
}

// The renderer reports on the first snapshot and then only when the status changes.
func TestSnapshotReportsStatusOnlyWhenItChanges(t *testing.T) {
	m := newModel(&viewClient{})
	s := conversationSnapshot(2)
	s.Status = &Status{State: "done", Msg: "2 unread"}
	if _, cmd := m.Update(s); cmd == nil {
		t.Fatal("first snapshot: no report")
	}
	if _, cmd := m.Update(s); cmd != nil {
		t.Fatal("same status: reported again")
	}
	s.Status = nil
	_, cmd := m.Update(s)
	if cmd == nil {
		t.Fatal("cleared status: no report")
	}
	if raw, ok := cmd().(tea.RawMsg); !ok || raw.Msg != statusClear {
		t.Fatalf("cleared status: %#v", cmd())
	}
}

// An action that closes the view waits until every draft is saved, then runs by itself.
func TestClosingActionWaitsForUnsavedDraft(t *testing.T) {
	m := newModel(&viewClient{})
	m.ready = true
	s := conversationSnapshot(2)
	archive := &Action{Name: "archive", Label: "Archive", Enabled: true, ClosesView: true}
	s.Tree.Nodes = append(s.Tree.Nodes, Node{ID: "archive", Type: "widget", Text: "Archive", Action: archive})
	m.apply(s)
	var composer string
	for id, e := range m.editors {
		if e.Kind == "composer" {
			composer = id
		}
	}
	if composer == "" {
		t.Fatal("no composer in the test snapshot")
	}
	m.editors[composer].Area.SetValue("unsaved text")
	m.activate(control{Kind: "action", Node: "archive", Action: archive})
	save := m.editors[composer].Pending
	if len(m.operations) != 1 || !m.operations[save] || m.afterSave == nil {
		t.Fatalf("archive did not wait for the draft: operations %d, save %q, waiting %v", len(m.operations), save, m.afterSave != nil)
	}
	// The save completes; the archive runs without a second click.
	m.finish(completed{
		Request: mutation{Method: "hub.view.draft.save", Key: save, Node: composer, Args: map[string]any{"text": "unsaved text"}},
		Outcome: operationResult{Status: "succeeded", Result: map[string]any{"text": "unsaved text", "draft_revision": "2"}},
	})
	if m.afterSave != nil || len(m.operations) != 1 || m.operations[save] {
		t.Fatalf("archive did not run after the save: operations %d, waiting %v", len(m.operations), m.afterSave != nil)
	}
}

func TestFYIBreaksGroupingAndShowsNote(t *testing.T) {
	a := Message{ID: "1", Author: "Codex", Text: "a", At: "2026-10-16T21:00:00Z"}
	b := Message{ID: "2", Author: "Codex", Text: "b", At: "2026-10-16T21:00:30Z", FYI: true}
	if grouped(&a, b) {
		t.Fatal("FYI message grouped under a normal one")
	}
	m := newModel(&viewClient{})
	if lines := m.messageLines(b, &a); !strings.Contains(ansi.Strip(lines[0]), "no reply needed") {
		t.Fatalf("header: %q", lines[0])
	}
}
