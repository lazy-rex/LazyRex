package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// A hub error keeps its code in a reply; an untyped bad() is "refused".
func TestStoreErrorCode(t *testing.T) {
	var se *storeError
	errors.As(refuse("invalid_input", "x"), &se)
	if c := storeErrorCode(se); c != "invalid_input" {
		t.Errorf("refuse: %q", c)
	}
	errors.As(bad("x"), &se)
	if c := storeErrorCode(se); c != "refused" {
		t.Errorf("bad: %q", c)
	}
}

// The agent face keeps the hub's code, as the plugin face does (review of fcb9e34, codex-28).
func TestAgentReplyError(t *testing.T) {
	plugin := &rex.RexError{Code: "conflict", Message: "the plugin's own"}
	for _, c := range []struct {
		what string
		err  error
		code string
	}{
		{"a typed hub error", refuse("invalid_input", "x"), "invalid_input"},
		{"an unknown outcome", refuse("outcome_unknown", "x"), "outcome_unknown"},
		{"an untyped hub error", bad("x"), "refused"},
		{"a plugin's own error", fmt.Errorf("hub.forward: %w", plugin), "conflict"},
		{"another error", errors.New("x"), "failed"},
	} {
		if got := agentReplyError(c.err); got.Code != c.code {
			t.Errorf("%s: %q, want %q", c.what, got.Code, c.code)
		}
	}
	if agentReplyError(fmt.Errorf("w: %w", plugin)) != plugin {
		t.Error("a plugin's own error is not passed as the plugin gave it")
	}
}

// Through the real client.reply path of the agent face: the hub's own cut of a forward reaches the
// agent as outcome_unknown (it was "refused" on fcb9e34), an unknown action as refused, and a
// plugin's own error with its code.
func TestAgentAnswerKeepsTheCode(t *testing.T) {
	shortPerformWait(t, 200*time.Millisecond)
	h, _ := forwardHub(t)
	face := newPerformRex(t, func(performCall) (any, string) { return nil, "" })
	h.agentRex = face.dial(t)
	for i, c := range []struct{ action, tag, code string }{
		{"x.do", "never", "outcome_unknown"},
		{"nope", "", "refused"},
		{"x.do", "conflict", "conflict"},
	} {
		var p perform
		p.CallID = fmt.Sprintf("call-%d", i)
		p.Action = c.action
		p.Args = json.RawMessage(`{"tag":"` + c.tag + `"}`)
		p.Requester.Principal.PID = 7
		h.agentAnswer(context.Background(), p)
		var got struct {
			CallID string `json:"call_id"`
			Error  struct{ Code string }
		}
		waitFor(t, "the reply of "+p.CallID, 5*time.Second, func() bool {
			face.mu.Lock()
			defer face.mu.Unlock()
			return len(face.replies) == i+1
		})
		face.mu.Lock()
		json.Unmarshal(face.replies[i], &got)
		face.mu.Unlock()
		if got.CallID != p.CallID || got.Error.Code != c.code {
			t.Errorf("%s %s: reply %+v, want code %q", c.action, c.tag, got, c.code)
		}
	}
}
