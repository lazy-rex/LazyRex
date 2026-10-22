package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/coder/websocket"
)

// resumePanel sends view.resume for id, as the hub does when a replacement ends without the slot.
func resumePanel(m *model, id string) string {
	answer := ""
	_, cmd := m.Update(replacementRequested{Action: "view.resume", ID: id, valid: func() bool { return true }, reply: func(status string) tea.Cmd { return func() tea.Msg { answer = status; return nil } }})
	if cmd != nil {
		cmd()
	}
	return answer
}

// Rex does not order calls from different connections (rex-protocol.md), so the hub's view.resume
// R can reach the renderer before the view.release R that it ends. A release of an ended
// replacement answers cancelled at once and never freezes the renderer (row 7).
func TestReleaseAfterResumeIsCancelled(t *testing.T) {
	m := panelModel()
	resumePanel(m, "x")
	if a := releasePanel(m, "x"); a != "cancelled" || m.replacement != nil || m.client.replacementID != "" {
		t.Fatalf("release after resume: %q, frozen %v", a, m.replacement != nil)
	}
	// Input is not held by a replacement.
	if held, _ := m.updateReplacement(tea.PasteMsg{Content: "!"}); held {
		t.Fatal("a cancelled release still holds input")
	}
}

// A late read or result for an ended replacement cannot freeze the renderer again.
func TestEndedReplacementStaysEnded(t *testing.T) {
	m := panelModel()
	if releasePanel(m, "x") != "released" || m.replacement == nil {
		t.Fatal("no freeze")
	}
	resumePanel(m, "x")
	if m.replacement != nil {
		t.Fatal("the resume did not unfreeze")
	}
	m.Update(replacementRead{ID: "x", Attachment: m.currentAttachment(), State: &replacementState{ID: "x", State: "opening"}})
	m.Update(replacementRecovered{ID: "x", State: replacementState{ID: "x", State: "opening"}})
	m.Update(replacementRecovered{ID: "x", State: replacementState{ID: "x", State: "opening"}, Initial: true})
	if m.replacement != nil {
		t.Fatalf("a late answer froze an ended replacement: %+v", m.replacement)
	}
	// An attach reply (Initial) for an ended replacement on a fresh renderer path.
	m2 := panelModel()
	resumePanel(m2, "y")
	m2.Update(replacementRecovered{ID: "y", State: replacementState{ID: "y", State: "opening"}, Initial: true, FromAttach: true})
	if m2.replacement != nil {
		t.Fatal("an attach reply froze an ended replacement")
	}
}

// The ended set has no eviction: many other ended replacements do not let an old release freeze.
func TestEndedSetHasNoEviction(t *testing.T) {
	m := panelModel()
	resumePanel(m, "x")
	for i := range 20 {
		id := fmt.Sprintf("other-%d", i)
		if releasePanel(m, id) != "released" {
			t.Fatalf("%s: no release", id)
		}
		resumePanel(m, id)
	}
	if a := releasePanel(m, "x"); a != "cancelled" || m.replacement != nil {
		t.Fatalf("release x after 20 other ended replacements: %q", a)
	}
}

// A cancelled or superseded result marks its replacement ended, also when it is not the active
// one, and it never touches the active replacement.
func TestStaleEndDoesNotTouchTheActiveReplacement(t *testing.T) {
	for _, state := range []string{"cancelled", "superseded"} {
		m := panelModel()
		if releasePanel(m, "r2") != "released" {
			t.Fatal("no freeze for r2")
		}
		m.Update(replacementRecovered{ID: "x", State: replacementState{ID: "x", State: state}})
		m.Update(replacementRead{ID: "z", Attachment: m.currentAttachment(), State: &replacementState{ID: "z", State: state}})
		if m.replacement == nil || m.replacement.ID != "r2" {
			t.Fatalf("%s: a result for another replacement changed r2", state)
		}
		// A resume of x while r2 holds the freeze: r2 stays.
		resumePanel(m, "x")
		if m.replacement == nil || m.replacement.ID != "r2" {
			t.Fatalf("%s: a resume of x unfroze r2", state)
		}
		resumePanel(m, "r2")
		for _, id := range []string{"x", "z"} {
			if a := releasePanel(m, id); a != "cancelled" || m.replacement != nil {
				t.Fatalf("%s: release %s after its %s result: %q", state, id, state, a)
			}
		}
	}
}

// Two resumes of one replacement: the second changes nothing, and a later replacement freezes.
func TestSecondResumeIsANoOp(t *testing.T) {
	m := panelModel()
	releasePanel(m, "r")
	if resumePanel(m, "r") != "resumed" || resumePanel(m, "r") != "resumed" || m.replacement != nil {
		t.Fatal("two resumes")
	}
	if releasePanel(m, "r2") != "released" || m.replacement == nil || m.replacement.ID != "r2" {
		t.Fatal("a new replacement after two resumes did not freeze")
	}
	resumePanel(m, "r")
	if m.replacement == nil || m.replacement.ID != "r2" {
		t.Fatal("a third resume of r unfroze r2")
	}
}

// A repeated release of the active replacement answers the same, with one freeze; the answer is
// given in the same model step, with no hub call before it.
func TestRepeatedReleaseOfTheActiveReplacement(t *testing.T) {
	m := panelModel()
	if releasePanel(m, "r") != "released" {
		t.Fatal("no release")
	}
	first := m.replacement
	answer := ""
	_, cmd := m.Update(replacementRequested{Action: "view.release", ID: "r", Title: "Worktrees", valid: func() bool { return true }, reply: func(status string) tea.Cmd { answer = status; return nil }})
	if answer != "released" || cmd != nil || m.replacement != first {
		t.Fatalf("a repeated release: %q, cmd %v, same freeze %v", answer, cmd != nil, m.replacement == first)
	}
}

// h: the real client, both orders of view.resume R and view.release R on the wire. Resume first:
// the release answers cancelled and nothing freezes. Release first: it freezes, and the resume
// unfreezes. Each answer is on the wire after its model step.
func TestResumeAndReleaseInBothOrdersOnTheClient(t *testing.T) {
	for _, resumeFirst := range []bool{true, false} {
		f := &protocolFixture{replies: make(chan map[string]json.RawMessage, 8)}
		c, msgs := startFixture(t, f)
		waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
		m := newModel(c)
		m.ready = true
		call := func(action string) string {
			t.Helper()
			// The request comes on the fixture's connection, as from Rex.
			f.mu.Lock()
			conn, attachment := f.conn, f.attachment
			f.mu.Unlock()
			raw, _ := json.Marshal(map[string]any{"type": "event", "payload": map[string]any{"type": "client_perform_requested", "payload": map[string]any{"call_id": action, "action": action, "requester": map[string]string{"client_id": "hub"}, "args": map[string]string{"view_id": "v1", "attachment": attachment, "replacement_id": "r1", "next_title": "Worktrees"}}}})
			if conn == nil || conn.Write(context.Background(), websocket.MessageText, raw) != nil {
				t.Fatalf("%s: no fixture connection", action)
			}
			var request replacementRequested
			waitMessage(t, msgs, func(msg tea.Msg) bool { var ok bool; request, ok = msg.(replacementRequested); return ok })
			if _, cmd := m.Update(request); cmd != nil {
				cmd()
			}
			select {
			case reply := <-f.replies:
				var result map[string]string
				json.Unmarshal(reply["result"], &result)
				return result["status"]
			case <-time.After(5 * time.Second):
				t.Fatalf("no answer to %s", action)
				return ""
			}
		}
		if resumeFirst {
			if a, b := call("view.resume"), call("view.release"); a != "resumed" || b != "cancelled" || m.replacement != nil {
				t.Fatalf("resume, then release: %q, %q, frozen %v", a, b, m.replacement != nil)
			}
		} else {
			if a := call("view.release"); a != "released" || m.replacement == nil {
				t.Fatalf("release first: %q, frozen %v", a, m.replacement != nil)
			}
			if b := call("view.resume"); b != "resumed" || m.replacement != nil {
				t.Fatalf("then resume: %q, frozen %v", b, m.replacement != nil)
			}
		}
	}
}
