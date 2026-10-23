package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/coder/websocket"
)

// A renderer that freezes from an attach reply reads the durable replacement state once
// (hub.view.replacement), so a view.resume that it lost does not freeze it for good. The tests
// use the real client (attachment check, reconnect) and the real model.Update; the fixture hub
// orders each reply and event.

// recovery drives a model with the client's messages. It runs only the commands of replacement
// messages: the others start timers that a test does not need.
type recovery struct {
	t    *testing.T
	m    *model
	msgs <-chan tea.Msg
}

func newRecovery(t *testing.T, f *protocolFixture) *recovery {
	c, msgs := startFixture(t, f)
	return &recovery{t: t, m: newModel(c), msgs: msgs}
}

// next returns the next client message that match accepts. It applies the messages before it
// (snapshots, connection state).
func (r *recovery) next(match func(tea.Msg) bool) tea.Msg {
	r.t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case msg := <-r.msgs:
			if match(msg) {
				return msg
			}
			r.m.Update(msg)
		case <-timer.C:
			r.t.Fatal("Timed out waiting for the client")
			return nil
		}
	}
}

// apply runs Update and the replacement commands that follow it. It returns the read command
// unrun when hold is set.
func (r *recovery) apply(msg tea.Msg, hold bool) tea.Cmd {
	_, cmd := r.m.Update(msg)
	if cmd == nil {
		return nil
	}
	if hold {
		return cmd
	}
	r.run(cmd)
	return nil
}

func (r *recovery) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch out := cmd().(type) {
	case nil:
	case replacementRead, replacementRecovered:
		r.apply(out, false)
	}
}

func isRecovered(msg tea.Msg) bool { _, ok := msg.(replacementRecovered); return ok }
func isSnapshot(msg tea.Msg) bool  { _, ok := msg.(Snapshot); return ok }
func isResume(msg tea.Msg) bool {
	q, ok := msg.(replacementRequested)
	return ok && q.Action == "view.resume"
}

// edits reports whether a paste reaches the composer.
func (r *recovery) edits(text string) bool {
	r.m.focus = "edit:composer"
	before := r.m.editors["composer"].text()
	r.m.Update(tea.PasteMsg{Content: text})
	return r.m.editors["composer"].text() != before
}

func (r *recovery) sendResume(f *protocolFixture, id string) {
	r.t.Helper()
	f.mu.Lock()
	conn, attachment := f.conn, f.attachment
	f.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"type": "event", "payload": map[string]any{"type": "client_perform_requested", "payload": map[string]any{"call_id": "resume-" + id, "action": "view.resume", "requester": map[string]string{"client_id": "hub"}, "args": map[string]string{"view_id": "v1", "attachment": attachment, "replacement_id": id}}}})
	if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		r.t.Fatal(err)
	}
}

func opening(id string) map[string]any {
	return map[string]any{"replacement_id": id, "state": "opening"}
}
func cancelled(id string) map[string]any {
	return map[string]any{"replacement_id": id, "state": "cancelled"}
}

// V1: the hub cancels and sends view.resume before the renderer has its attach reply. The client
// refuses that resume (stale attachment); the attach reply then freezes the renderer, and the read
// resumes it.
func TestResumeBeforeAttachReplyIsRecovered(t *testing.T) {
	f := &protocolFixture{attachReplacement: opening("r1"), resumeBeforeAttach: "r1", readAnswers: []any{cancelled("r1")}, replies: make(chan map[string]json.RawMessage, 8)}
	r := newRecovery(t, f)
	select {
	case reply := <-f.replies:
		var id string
		json.Unmarshal(reply["call_id"], &id)
		if id != "early-resume" || reply["error"] == nil {
			t.Fatalf("The early resume was not refused: %v", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("No answer to the early resume")
	}
	read := r.apply(r.next(isRecovered), true)
	r.apply(r.next(isSnapshot), false)
	if r.m.replacement == nil || r.edits("lost") {
		t.Fatal("The attach reply did not freeze the renderer")
	}
	r.run(read)
	if r.m.replacement != nil || !r.edits("typed") {
		t.Fatal("The renderer stayed frozen after the read")
	}
}

// V2: view.resume is applied after the attachment is set and before the attach reply's state. The
// model answers it with no replacement; the attach reply then freezes, and the read resumes.
func TestResumeBeforeRecoveredStateIsRecovered(t *testing.T) {
	// The subscribe reply waits until the resume is applied, so the Snapshot cannot come first.
	subscribed := make(chan struct{})
	f := &protocolFixture{attachReplacement: opening("r1"), readAnswers: []any{cancelled("r1")}, replies: make(chan map[string]json.RawMessage, 8), holdSubscribe: subscribed}
	r := newRecovery(t, f)
	recovered := r.next(isRecovered)
	r.sendResume(f, "r1")
	r.apply(r.next(isResume), false) // before the recovered state
	read := r.apply(recovered, true)
	close(subscribed)
	r.apply(r.next(isSnapshot), false)
	// The resume ended r1 (the hub sends it after the end is durable): the attach reply for an
	// ended replacement does not freeze the renderer (row 7), and it asks for no read.
	if r.m.replacement != nil || read != nil || !r.edits("typed") {
		t.Fatal("The attach reply froze the renderer for an ended replacement")
	}
	r.run(read)
	if r.m.replacement != nil || !r.edits("typed") {
		t.Fatal("The renderer stayed frozen after the read")
	}
}

// V3: the read finds the replacement still opening; the hub's later view.resume unfreezes.
func TestResumeAfterReadIsApplied(t *testing.T) {
	f := &protocolFixture{attachReplacement: opening("r1"), readAnswers: []any{opening("r1")}}
	r := newRecovery(t, f)
	r.apply(r.next(isRecovered), false)
	r.apply(r.next(isSnapshot), false)
	if r.m.replacement == nil || r.m.replacement.State != "opening" || r.edits("lost") {
		t.Fatal("An opening read did not keep the renderer frozen")
	}
	r.sendResume(f, "r1")
	r.apply(r.next(isResume), false)
	if r.m.replacement != nil || !r.edits("typed") {
		t.Fatal("view.resume after the read did not unfreeze")
	}
}

// V4: one read fails. The renderer attaches again by itself (no disconnect from the fixture), and
// the second read resumes it.
func TestFailedReadRecoversOnTheSameRenderer(t *testing.T) {
	f := &protocolFixture{attachReplacement: opening("r1"), readAnswers: []any{&wireError{Code: "failed", Message: "lab: the read failed"}, cancelled("r1")}}
	r := newRecovery(t, f)
	r.apply(r.next(isRecovered), false)
	if r.m.replacement == nil || r.m.replacement.Error == "" {
		t.Fatal("A failed read did not report the recovery")
	}
	r.apply(r.next(isRecovered), false) // the attach after lost = true
	r.apply(r.next(isSnapshot), false)
	if r.m.replacement != nil || !r.edits("typed") {
		t.Fatal("The second read did not resume the renderer")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads != 2 || f.attaches != 2 {
		t.Fatalf("reads=%d attaches=%d", f.reads, f.attaches)
	}
}

// V5: view.resume arrives while the read is delayed; the late read (opening) does not freeze the
// renderer again.
func TestLateReadAfterResumeChangesNothing(t *testing.T) {
	f := &protocolFixture{attachReplacement: opening("r1"), readAnswers: []any{opening("r1")}}
	r := newRecovery(t, f)
	read := r.apply(r.next(isRecovered), true)
	r.apply(r.next(isSnapshot), false)
	r.sendResume(f, "r1")
	r.apply(r.next(isResume), false)
	if r.m.replacement != nil {
		t.Fatal("view.resume did not unfreeze")
	}
	r.run(read)
	if r.m.replacement != nil || !r.edits("typed") {
		t.Fatal("A late read froze the renderer again")
	}
}

// V6: the hub restarted while this renderer asked the user (panels-lab.mjs case C). A restarted
// renderer attaches again; the attach reply names the replacement as confirming, so the prompt is
// shown again, and Discard reaches the hub with the new attachment. The hub side (the row stays
// confirming over a restart, the old attachment is stale) is TestSlotConfirmSurvivesHubRestartLab.
func TestRestartedRendererPromptsAgainAndDiscards(t *testing.T) {
	f := &protocolFixture{attachReplacement: map[string]any{"replacement_id": "r1", "state": "confirming", "next_title": "Worktrees"}}
	r := newRecovery(t, f)
	r.apply(r.next(isRecovered), false)
	r.apply(r.next(isSnapshot), false)
	// Only the hub restarted: the renderer keeps its unsaved input (a text field, which starts no
	// draft save). Without it, the renderer discards by itself when it is ready (reload.go).
	field := newEditor("saved", "1", "text")
	field.setText("unsaved")
	r.m.editors["field"] = field
	r.apply(r.next(isReady), false) // Discard waits for a ready connection (replacementBlocked)
	if r.m.replacement == nil || r.m.replacement.State != "confirming" || r.edits("lost") {
		t.Fatal("The attach reply did not restore the prompt")
	}
	screen := r.m.View().Content
	if !strings.Contains(screen, "Keep editing") || !strings.Contains(screen, "Discard and replace") {
		t.Fatal(screen)
	}
	r.run(r.m.chooseReplacement("discard"))
	f.mu.Lock()
	confirms, attachment := f.confirms, f.attachment
	f.mu.Unlock()
	if len(confirms) != 1 || confirms[0]["choice"] != "discard" || confirms[0]["replacement_id"] != "r1" || confirms[0]["attachment"] != attachment {
		t.Fatalf("Discard did not reach the hub with the new attachment: %v (attachment %s)", confirms, attachment)
	}
	if r.m.replacement == nil || r.m.replacement.State != "opening" {
		t.Fatal("The renderer did not follow the hub's answer")
	}
}

func isReady(msg tea.Msg) bool { c, ok := msg.(connectionMsg); return ok && c.Ready }
