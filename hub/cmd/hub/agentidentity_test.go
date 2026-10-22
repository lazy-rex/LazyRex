package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The identity field of hub.workers: for an open spawn with an agent_id, its pane's current
// conversation identity; no field for any other worker. The rows below are the states that
// identity.go writes: a report sets current; an exit or a resume in another pane clears it.
func TestWorkerIdentity(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	if _, err := h.db.Exec(identitySchema); err != nil {
		t.Fatal(err)
	}
	pane := func(block, current string) {
		t.Helper()
		if _, err := h.db.Exec(`INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current, reporter_start)
			VALUES (?, 'session:1', 1, '1.0', 'claude', ?, '')
			ON CONFLICT (block_id) DO UPDATE SET current = excluded.current`, block, current); err != nil {
			t.Fatal(err)
		}
	}
	open := func(block string) *spawnRow { return &spawnRow{Status: "open", Agent: "agent-1", Block: block} }
	check := func(name string, s *spawnRow, want string) {
		t.Helper()
		if got, err := h.workerIdentity(ctx, s); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", name, got, err, want)
		}
	}
	check("no report yet (no row)", open("block:a"), "none")
	pane("block:a", "claude:c1")
	check("a report", open("block:a"), "current")
	// The conversation resumes in another pane: identity.go clears it here and sets it there.
	pane("block:a", "")
	pane("block:b", "claude:c1")
	check("resumed elsewhere: the old pane", open("block:a"), "none")
	check("resumed elsewhere: the new pane", open("block:b"), "current")
	pane("block:a", "claude:c2")
	check("a new report in the same pane", open("block:a"), "current")
	pane("block:b", "") // the pane's process exited (identity.go: pane process replaced)
	check("the pane process exited", open("block:b"), "none")

	// An agent spawn is the spawn row's agent_id. The profile is not read: a removed profile or a
	// kind changed to accessory after the spawn gives the same answer.
	s := open("block:a")
	s.Profile = "removed-profile"
	check("the profile is gone", s, "current")
	s.Profile = "shell" // the built-in accessory
	check("the profile is an accessory now", s, "current")

	// No field for any other worker.
	check("an accessory spawn (no agent_id)", &spawnRow{Status: "open", Block: "block:a"}, "")
	for _, st := range []string{"prepared", "sending", "failed", "closed"} {
		check("an agent spawn "+st, &spawnRow{Status: st, Agent: "agent-1", Block: "block:a"}, "")
	}

	// A failed read is an error, never "none".
	if _, err := h.db.Exec(`DROP TABLE identity_panes`); err != nil {
		t.Fatal(err)
	}
	if got, err := h.workerIdentity(ctx, open("block:a")); err == nil {
		t.Fatalf("a failed read gave %q", got)
	}
}

// Hub Status: one advisory line for each running agent worker with identity "none". Unknown, never
// silence, for a failed read, a reply without the list, an unknown value, and a running or exited
// agent worker without the field (a malformed hub.workers answer).
func TestAgentIdentityLines(t *testing.T) {
	line := "agent Coder (pane block:1): no current conversation identity yet."
	for _, c := range []struct {
		name  string
		raw   string
		err   error
		want  []string // substrings, one per line
		lines int
	}{
		{"none, running", `{"workers":[{"title":"Coder","agent_id":"a","block_id":"block:1","state":"running","identity":"none"}]}`, nil, []string{line}, 1},
		{"current", `{"workers":[{"title":"Coder","agent_id":"a","block_id":"block:1","state":"running","identity":"current"}]}`, nil, nil, 0},
		{"none, exited: no hook problem", `{"workers":[{"title":"Coder","agent_id":"a","block_id":"block:1","state":"exited","identity":"none"}]}`, nil, nil, 0},
		{"an accessory", `{"workers":[{"title":"Shell","block_id":"block:2","state":"running"}]}`, nil, nil, 0},
		{"starting and failed agents without the field", `{"workers":[{"title":"A","agent_id":"a","state":"starting"},{"title":"B","agent_id":"b","state":"failed"}]}`, nil, nil, 0},
		{"no workers", `{"workers":[]}`, nil, nil, 0},
		{"two, one none", `{"workers":[{"title":"Coder","agent_id":"a","block_id":"block:1","state":"running","identity":"none"},{"title":"Other","agent_id":"b","block_id":"block:3","state":"running","identity":"current"}]}`, nil, []string{line}, 1},
		{"no field, running", `{"workers":[{"title":"Coder","agent_id":"a","block_id":"block:1","state":"running"}]}`, nil, []string{"agents: unknown: worker Coder (running): hub.workers gives no identity"}, 1},
		{"no field, exited", `{"workers":[{"title":"Coder","agent_id":"a","block_id":"block:1","state":"exited"}]}`, nil, []string{"agents: unknown: worker Coder (exited): hub.workers gives no identity"}, 1},
		{"an unknown value", `{"workers":[{"title":"Coder","agent_id":"a","state":"running","identity":"maybe"}]}`, nil, []string{`agents: unknown: worker Coder: identity "maybe"`}, 1},
		{"no list", `{}`, nil, []string{"agents: unknown: hub.workers has no workers list"}, 1},
		{"bad JSON", `{`, nil, []string{"agents: unknown: hub.workers:"}, 1},
		{"read error", ``, errors.New("exit status 1: refused"), []string{"agents: unknown: exit status 1: refused"}, 1},
		{"pane-safe", "{\"workers\":[{\"title\":\"Co\\u001b]0;x\\u0007der\",\"agent_id\":\"a\",\"block_id\":\"block:1\",\"state\":\"running\",\"identity\":\"none\"}]}", nil, []string{"agent Co?]0;x?der (pane block:1)"}, 1},
	} {
		got := agentIdentityLines([]byte(c.raw), c.err)
		if len(got) != c.lines {
			t.Errorf("%s: %d lines: %q", c.name, len(got), got)
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: line %d %q has no %q", c.name, i, got[i], w)
			}
		}
	}
	if got := agentIdentityLines([]byte(`{"workers":[{"title":"C","agent_id":"a","block_id":"b","state":"running","identity":"none"}]}`), nil); !strings.Contains(got[0], "Codex can report only at its first prompt") || !strings.Contains(got[0], "/hooks") {
		t.Errorf("the advice: %q", got)
	}
}
