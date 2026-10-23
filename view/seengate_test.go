package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/coder/websocket"
)

// The seen gate (thread view v1.2 item 3, lead-tv-seenneutral) on the real client.run and the
// fixture hub: the model's trees come from the test, after the attach, as a slow first tree would.

func seenCount(f *protocolFixture) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen
}

// seenStays checks that the seen count stays at want for a while.
func seenStays(t *testing.T, f *protocolFixture, want int, step string) {
	t.Helper()
	until := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(until) {
		if n := seenCount(f); n != want {
			t.Fatalf("%s: %d seen, want %d", step, n, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stamp gives a test tree the client's current attachment generation, as the client's fetch does.
func stamp(c *viewClient, s Snapshot) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s.gen = c.gen
	return s
}

// reattach does what connect does to the gate for a new attachment.
func reattach(c *viewClient, attachment string) {
	c.mu.Lock()
	c.seenOK, c.attachment = false, attachment
	c.gen++
	c.mu.Unlock()
}

func seenReaches(t *testing.T, f *protocolFixture, want int, step string) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for seenCount(f) < want {
		if time.Now().After(until) {
			t.Fatalf("%s: %d seen, want %d", step, seenCount(f), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	seenStays(t, f, want, step)
}

// focusedFixtureModel attaches a client to the fixture and gives its model the pane focus. The
// first attach and the focus send no seen: no tree decided the mode yet.
func focusedFixtureModel(t *testing.T) (*model, *protocolFixture, <-chan tea.Msg) {
	t.Helper()
	f := &protocolFixture{}
	client, events := startFixture(t, f)
	waitMessage(t, events, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	m := newModel(client)
	m.ready = true
	m.Update(tea.FocusMsg{})
	seenStays(t, f, 0, "the first attach and the focus, no tree")
	return m, f, events
}

func TestSeenClosedUntilAltDecision(t *testing.T) {
	m, f, _ := focusedFixtureModel(t)
	m.Update(stamp(m.client, treeOf(tvError("e", "The hub lost the thread.", true))))
	seenStays(t, f, 0, "an error-only tree")
	m.Update(stamp(m.client, treeOf(tvList)))
	seenReaches(t, f, 1, "the list tree decides alt")
}

func TestSeenClosedUntilThreadPrint(t *testing.T) {
	m, f, _ := focusedFixtureModel(t)
	m.Update(stamp(m.client, treeOf(Node{ID: "l", Type: "loading", Text: "Loading"})))
	seenStays(t, f, 0, "a loading tree")
	m.Update(stamp(m.client, threadTree(msgs(1, 2))))
	seenStays(t, f, 0, "the thread tree before its print")
	m.Update(printDone{ids: []string{"m1", "m2"}})
	seenReaches(t, f, 1, "the print of the newest ID")
	m.Update(stamp(m.client, threadTree(msgs(1, 2))))
	seenStays(t, f, 1, "the same tree again")
}

func TestSeenAfterReattachWaitsForAThreadFrame(t *testing.T) {
	m, f, events := focusedFixtureModel(t)
	m.Update(stamp(m.client, threadTree(msgs(1))))
	m.Update(printDone{ids: []string{"m1"}})
	seenReaches(t, f, 1, "the first print")
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	event := map[string]any{"type": "event", "payload": map[string]any{"type": "session_event", "payload": map[string]any{"event": map[string]any{"type": "client_disconnected", "payload": map[string]string{"client_id": "hub"}}}}}
	b, _ := json.Marshal(event)
	if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	waitMessage(t, events, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && !s.Ready })
	waitMessage(t, events, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	seenStays(t, f, 1, "the reattach alone")
	m.Update(stamp(m.client, threadTree(msgs(1))))
	seenReaches(t, f, 2, "the first thread frame of the new attachment")
}

// TestSeenNotSentAfterOutputFault: a seen wake that waits when the output fails sends nothing.
// The writer closes the gate before outputFailed reaches the model. Without the writer's hook, the
// latch alone (outFailed, read at the send) holds the seen.
func TestSeenNotSentAfterOutputFault(t *testing.T) {
	for _, hook := range []bool{true, false} {
		t.Run(map[bool]string{true: "hook", false: "latch only"}[hook], func(t *testing.T) { seenAfterFault(t, hook) })
	}
}

func seenAfterFault(t *testing.T, hook bool) {
	m, f, _ := focusedFixtureModel(t)
	out, tt := newTestOut(nil)
	m.client.mu.Lock() // client.run reads it
	m.client.outFailed = out.Failed
	m.client.mu.Unlock()
	if hook {
		out.onFault = m.client.closeSeen
	}
	m.Update(stamp(m.client, threadTree(msgs(1))))
	tt.set(func(t *testTerm) { t.failAt = func([]byte) bool { return true } })
	out.Write([]byte("a frame"))
	m.Update(printDone{ids: []string{"m1"}}) // the model has not heard of the fault yet
	m.client.focus(true, "focus")
	seenStays(t, f, 0, "after the fault")
}

// TestSeenOldReceiptBeforeNewAttachTree (codex-29's probe on bbe841b3): attachment A's tree, a
// reattach on the real client, then the receipt of A's print. The current tree is A's, so the
// receipt sends no seen.
func TestSeenOldReceiptBeforeNewAttachTree(t *testing.T) {
	m, f, events := focusedFixtureModel(t)
	m.Update(stamp(m.client, threadTree(msgs(1))))
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"type": "event", "payload": map[string]any{"type": "session_event", "payload": map[string]any{"event": map[string]any{"type": "client_disconnected", "payload": map[string]string{"client_id": "hub"}}}}})
	if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	waitMessage(t, events, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && !s.Ready })
	waitMessage(t, events, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready })
	m.Update(printDone{ids: []string{"m1"}})
	seenStays(t, f, 0, "the old print receipt before the new attachment's tree")
}
