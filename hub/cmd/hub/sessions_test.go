package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// fakeRex answers session.create and session.attach. lose makes the next create time out after
// Rex created the session (a lost answer). gone lists closed sessions.
type fakeRex struct {
	creates int
	lose    bool
	gone    map[string]bool
}

func (f *fakeRex) call(ctx context.Context, method string, payload any, out any) error {
	switch method {
	case "session.create":
		f.creates++
		if f.lose {
			f.lose = false
			return context.DeadlineExceeded
		}
		b, _ := json.Marshal(map[string]any{"session_id": "session:new-" + string(rune('0'+f.creates))})
		return json.Unmarshal(b, out)
	case "session.attach":
		if f.gone[payload.(map[string]any)["session_id"].(string)] {
			return &rex.RexError{Code: "session_not_found", Message: "gone"}
		}
	}
	return nil
}

func ensureTestHub(t *testing.T) *Hub {
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(ensuredSchema); err != nil {
		t.Fatal(err)
	}
	return h
}

func codeOf(err error) string {
	var se *storeError
	if errors.As(err, &se) {
		return se.code
	}
	return ""
}

func TestEnsureSessionOncePerKey(t *testing.T) {
	h, f, ctx := ensureTestHub(t), &fakeRex{gone: map[string]bool{}}, context.Background()
	args := map[string]any{"key": "k1", "name": "Work"}
	a, err := h.ensureSession(ctx, f.call, "messaging", args)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.ensureSession(ctx, f.call, "messaging", args)
	if err != nil || b.(map[string]any)["session_id"] != a.(map[string]any)["session_id"] || f.creates != 1 {
		t.Fatalf("repeat: %v %v, creates %d", b, err, f.creates)
	}
	if _, err := h.ensureSession(ctx, f.call, "messaging", map[string]any{"key": "k1", "name": "Other"}); codeOf(err) != "operation_conflict" {
		t.Fatalf("other name: %v", err)
	}
	// Another plugin's key is separate.
	if _, err := h.ensureSession(ctx, f.call, "worktrees", args); err != nil || f.creates != 2 {
		t.Fatalf("other plugin: %v, creates %d", err, f.creates)
	}
	// A gone session is an error, never a second create.
	f.gone[a.(map[string]any)["session_id"].(string)] = true
	if _, err := h.ensureSession(ctx, f.call, "messaging", args); codeOf(err) != "session_gone" || f.creates != 2 {
		t.Fatalf("gone: %v, creates %d", err, f.creates)
	}
}

// A lost answer: the session may exist (here it does, with the same label as an unrelated session).
// The hub adopts nothing by label and sends no second create.
func TestEnsureSessionLostAnswerIsUnknown(t *testing.T) {
	h, f, ctx := ensureTestHub(t), &fakeRex{lose: true}, context.Background()
	args := map[string]any{"key": "k2", "name": "Work"}
	if _, err := h.ensureSession(ctx, f.call, "messaging", args); codeOf(err) != "outcome_unknown" {
		t.Fatalf("lost answer: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := h.ensureSession(ctx, f.call, "messaging", args); codeOf(err) != "outcome_unknown" || f.creates != 1 {
			t.Fatalf("repeat %d: %v, creates %d", i, err, f.creates)
		}
	}
}

// A prepared row (the hub stopped before it sent the create) is retried. Any error after the send
// is uncertain, including a dropped connection (RexError code disconnected): no second create.
func TestEnsureSessionRetriesOnlyUnsentCreates(t *testing.T) {
	h, f, ctx := ensureTestHub(t), &fakeRex{}, context.Background()
	if _, err := h.db.Exec(`INSERT INTO ui_ensured (plugin, key, name, state) VALUES ('messaging', 'k3', 'Work', 'prepared')`); err != nil {
		t.Fatal(err)
	}
	if out, err := h.ensureSession(ctx, f.call, "messaging", map[string]any{"key": "k3", "name": "Work"}); err != nil || !strings.HasPrefix(out.(map[string]any)["session_id"].(string), "session:") {
		t.Fatalf("prepared: %v %v", out, err)
	}
	for _, code := range []string{"disconnected", "invalid_request"} {
		failing := func(ctx context.Context, method string, payload any, out any) error {
			return &rex.RexError{Code: code, Message: "no answer"}
		}
		args := map[string]any{"key": "k-" + code, "name": "Work"}
		if _, err := h.ensureSession(ctx, failing, "messaging", args); codeOf(err) != "outcome_unknown" {
			t.Fatalf("%s: %v", code, err)
		}
		before := f.creates
		if _, err := h.ensureSession(ctx, f.call, "messaging", args); codeOf(err) != "outcome_unknown" || f.creates != before {
			t.Fatalf("%s repeat: %v, creates %d", code, err, f.creates-before)
		}
	}
	if _, err := h.db.Exec(`INSERT INTO ui_ensured (plugin, key, name, state) VALUES ('messaging', 'k5', 'Work', 'odd')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ensureSession(ctx, f.call, "messaging", map[string]any{"key": "k5", "name": "Work"}); err == nil || f.creates != 1 {
		t.Fatalf("unknown state: %v, creates %d", err, f.creates)
	}
}

// The created session is gone before the attach: session_gone, and the ID stays recorded.
func TestEnsureSessionGoneAtOnce(t *testing.T) {
	h, f, ctx := ensureTestHub(t), &fakeRex{gone: map[string]bool{"session:new-1": true}}, context.Background()
	args := map[string]any{"key": "k6", "name": "Work"}
	if _, err := h.ensureSession(ctx, f.call, "messaging", args); codeOf(err) != "session_gone" {
		t.Fatalf("gone at once: %v", err)
	}
	if _, err := h.ensureSession(ctx, f.call, "messaging", args); codeOf(err) != "session_gone" || f.creates != 1 {
		t.Fatalf("repeat: %v, creates %d", err, f.creates)
	}
}
