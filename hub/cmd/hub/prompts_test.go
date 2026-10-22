package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// promptHub is a hub with one legacy agent bound to this test process (a live binding).
func promptHub(t *testing.T) (*Hub, *Agent) {
	t.Helper()
	h, _ := appModeHub(t)
	pid := os.Getpid()
	a := &Agent{ID: "alice", Name: "Alice", Session: "session:s", BlockID: "block:b", ChildPID: pid, ChildStart: startTime(pid), delivered: map[string]int64{}}
	h.agents = map[string]*Agent{"alice": a}
	h.agentOrder = []string{"alice"}
	h.generation = 7
	return h, a
}

// status sends a program status event for the agent's block, as Rex does.
func status(h *Hub, block string, record map[string]any) {
	name, payload := "program_status_changed", map[string]any{"id": "", "record": record, "reason": "report"}
	if record == nil {
		name, payload = "program_status_removed", map[string]any{"ids": []string{""}, "reason": "clear"}
	}
	b, _ := json.Marshal(payload)
	h.blockEvent(context.Background(), block, name, b)
}

func openPrompt(h *Hub, agent string) *prompt {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prompts.open[agent]
}

func lastClose(h *Hub, agent string) closedPrompt {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prompts.last[agent]
}

// The prompt state machine (docs/hub.md "Agent prompts"): open, the same report, replaced, a
// state change, removed, rebind and process exit.
func TestPromptStateMachine(t *testing.T) {
	h, a := promptHub(t)
	ask := map[string]any{"state": "blocked", "kind": "permission", "app": "claude-code", "msg": "Allow Bash?"}

	status(h, "block:b", map[string]any{"state": "working", "app": "claude-code"})
	if openPrompt(h, "alice") != nil {
		t.Fatal("working opened a prompt")
	}
	status(h, "block:b", ask)
	p1 := openPrompt(h, "alice")
	if p1 == nil || p1.Gen != 1 || p1.Kind != "permission" || p1.Msg != "Allow Bash?" || p1.Block != "block:b" || p1.Session != "session:s" || len(p1.ID) != 16 {
		t.Fatalf("blocked: %+v", p1)
	}
	status(h, "block:b", ask)
	if openPrompt(h, "alice") != p1 {
		t.Fatal("the same report replaced the prompt")
	}
	status(h, "block:b", map[string]any{"state": "blocked", "kind": "permission", "app": "claude-code", "msg": "Allow Edit?"})
	p2 := openPrompt(h, "alice")
	if p2 == nil || p2 == p1 || p2.Gen != 2 || lastClose(h, "alice") != (closedPrompt{p1.ID, "replaced"}) {
		t.Fatalf("a new msg: %+v, last close %+v", p2, lastClose(h, "alice"))
	}
	status(h, "block:b", map[string]any{"state": "blocked", "kind": "permission", "app": "codex", "msg": "Allow Edit?"})
	p3 := openPrompt(h, "alice")
	if p3 == nil || p3.Gen != 3 || lastClose(h, "alice").Reason != "replaced" {
		t.Fatalf("a new app replaces: %+v", p3)
	}
	status(h, "block:b", map[string]any{"state": "working"})
	if openPrompt(h, "alice") != nil || lastClose(h, "alice") != (closedPrompt{p3.ID, "state"}) {
		t.Fatalf("working closes: %+v", lastClose(h, "alice"))
	}

	// Codex: a blocked record without kind and msg is a prompt.
	status(h, "block:b", map[string]any{"state": "blocked", "app": "codex"})
	p4 := openPrompt(h, "alice")
	if p4 == nil || p4.Kind != "" || p4.Msg != "" || p4.Gen != 4 {
		t.Fatalf("blocked without kind and msg: %+v", p4)
	}
	status(h, "block:b", nil)
	if openPrompt(h, "alice") != nil || lastClose(h, "alice").Reason != "removed" {
		t.Fatalf("removed: %+v", lastClose(h, "alice"))
	}

	// Another block's report does not count.
	status(h, "block:other", ask)
	if openPrompt(h, "alice") != nil {
		t.Fatal("another block opened alice's prompt")
	}

	// A rebind to another block closes the prompt, though the record stays blocked.
	status(h, "block:b", ask)
	p5 := openPrompt(h, "alice")
	h.mu.Lock()
	a.BlockID = "block:c"
	h.syncPrompts()
	h.mu.Unlock()
	if openPrompt(h, "alice") == p5 || lastClose(h, "alice") != (closedPrompt{p5.ID, "gone"}) {
		t.Fatalf("rebind: %+v", lastClose(h, "alice"))
	}

	// A process that exited (another start time) closes it at the next check.
	p6 := openPrompt(h, "alice") // the blocked record now counts for block:c
	if p6 == nil || p6.Block != "block:c" {
		t.Fatalf("the record of the new binding: %+v", p6)
	}
	h.mu.Lock()
	a.ChildStart = "gone"
	h.syncPrompts()
	h.mu.Unlock()
	if openPrompt(h, "alice") != nil || lastClose(h, "alice") != (closedPrompt{p6.ID, "gone"}) {
		t.Fatalf("exit: %+v", lastClose(h, "alice"))
	}
}

// Generations increase per agent; rev increases at every open and close.
func TestPromptRevisions(t *testing.T) {
	h, _ := promptHub(t)
	status(h, "block:b", map[string]any{"state": "blocked", "msg": "a"})
	status(h, "block:b", map[string]any{"state": "blocked", "msg": "b"})
	status(h, "block:b", map[string]any{"state": "idle"})
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.prompts.rev != 4 || h.prompts.agentRev["alice"] != 4 || h.prompts.gen["alice"] != 2 {
		t.Fatalf("rev %d, agent rev %d, gen %d", h.prompts.rev, h.prompts.agentRev["alice"], h.prompts.gen["alice"])
	}
}

// msg is cut to 8 KiB on a UTF-8 boundary; a change after the cut still replaces the prompt.
func TestPromptMsgCut(t *testing.T) {
	h, _ := promptHub(t)
	long := strings.Repeat("a", promptMsgMax-1) + "é" + "tail one"
	status(h, "block:b", map[string]any{"state": "blocked", "msg": long})
	p := openPrompt(h, "alice")
	if !p.Truncated || len(p.Msg) != promptMsgMax-1 || !strings.HasSuffix(p.Msg, "a") {
		t.Fatalf("cut: truncated %v, len %d", p.Truncated, len(p.Msg))
	}
	status(h, "block:b", map[string]any{"state": "blocked", "msg": strings.Replace(long, "one", "two", 1)})
	if q := openPrompt(h, "alice"); q == p || q.Msg != p.Msg {
		t.Fatal("a change after the cut must replace the prompt")
	}
	if s, cut := cutUTF8("short", promptMsgMax); s != "short" || cut {
		t.Fatal("a short msg is not cut")
	}
}

// hub.prompts: granted plugins only; the list and its rev; hub_generation; a closed prompt is
// absent.
func TestPromptsList(t *testing.T) {
	h, _ := promptHub(t)
	if _, err := h.promptsList(&Plugin{Name: "other"}); err == nil || !strings.Contains(err.Error(), "no prompts grant") {
		t.Fatalf("no grant: %v", err)
	}
	tg := &Plugin{Name: "telegram", entry: &configEntry{index: 1}, run: pluginRun{Prompts: true}}
	status(h, "block:b", map[string]any{"state": "blocked", "kind": "question", "msg": "Pick one"})
	out, err := h.promptsList(tg)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	list := m["prompts"].([]any)
	p := openPrompt(h, "alice")
	if m["hub_generation"] != "7" || m["rev"] != "1" || len(list) != 1 || list[0].(map[string]any)["prompt_id"] != p.ID || list[0].(map[string]any)["block_id"] != "block:b" {
		t.Fatalf("list: %+v", m)
	}
	status(h, "block:b", map[string]any{"state": "idle"})
	out, _ = h.promptsList(tg)
	if m := out.(map[string]any); m["rev"] != "2" || len(m["prompts"].([]any)) != 0 {
		t.Fatalf("after close: %+v", m)
	}
}

// hub.agents gives block_id for a live agent.
func TestAgentsListBlockID(t *testing.T) {
	h, _ := promptHub(t)
	list := h.agentsList(map[string]any{}).(map[string]any)["agents"].([]any)
	if e := list[0].(map[string]any); e["block_id"] != "block:b" {
		t.Fatalf("agents: %+v", e)
	}
}

// A kept record (its config entry was removed) has no prompts grant, also while its old instance
// with the grant in its run is registered.
func TestPromptsGrantNeedsEntry(t *testing.T) {
	h, _ := promptHub(t)
	kept := &Plugin{Name: "telegram", run: pluginRun{Prompts: true}}
	if _, err := h.promptsList(kept); err == nil || !strings.Contains(err.Error(), "no prompts grant") {
		t.Fatalf("kept record: %v", err)
	}
	withEntry := &Plugin{Name: "telegram", entry: &configEntry{index: 1}}
	if _, err := h.promptsList(withEntry); err == nil {
		t.Fatal("an entry without the grant in the run")
	}
}

// openPrompts lists every open prompt.
func openPrompts(h *Hub) []*prompt {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*prompt
	for _, p := range h.prompts.open {
		out = append(out, p)
	}
	return out
}

// One prompt for each pane: a legacy agent that a current identity in the same pane took over
// (an alias) has no prompt of its own. At the identity's first report the legacy prompt closes
// with the reason "alias", and the identity opens the same prompt (Telegram's review).
func TestPromptAliasedLegacyPane(t *testing.T) {
	h, a := promptHub(t)
	ask := map[string]any{"state": "blocked", "kind": "permission", "app": "claude-code", "msg": "Allow Bash?"}
	status(h, "block:b", ask)
	legacy := openPrompt(h, "alice")
	if legacy == nil {
		t.Fatal("the legacy pane has no prompt")
	}

	// The agent's hook reports the conversation: the identity takes over alice in the same pane.
	id := &Agent{ID: "claude:c1", Name: "Alice", Kind: "claude", IdState: "current", Session: a.Session, BlockID: a.BlockID,
		ChildPID: a.ChildPID, ChildStart: a.ChildStart, delivered: map[string]int64{}}
	h.mu.Lock()
	id.setRecord(a.Record)
	h.idents = map[string]*Agent{id.ID: id}
	h.aliases = map[string]string{"alice": id.ID}
	revBefore := h.prompts.rev
	h.syncPrompts()
	rev := h.prompts.rev
	h.mu.Unlock()
	open := openPrompts(h)
	if len(open) != 1 || open[0].Agent != id.ID {
		t.Fatalf("one prompt for the pane, the identity's: %+v", open)
	}
	p := open[0]
	if lastClose(h, "alice") != (closedPrompt{legacy.ID, "alias"}) {
		t.Fatalf("the legacy prompt closes with alias: %+v", lastClose(h, "alice"))
	}
	if p.ID == legacy.ID || p.Kind != legacy.Kind || p.Msg != legacy.Msg || p.App != legacy.App || rev != revBefore+2 {
		t.Fatalf("the identity opens the same prompt after the close: %+v (rev %d -> %d)", p, revBefore, rev)
	}
	h.mu.Lock()
	gone := h.promptOpen(legacy.ID)
	h.mu.Unlock()
	if gone != nil {
		t.Fatal("the old legacy prompt still takes an answer")
	}

	// A resync (as an identity resync, a restart or a reconcile does) brings no duplicate.
	h.mu.Lock()
	h.syncPrompts()
	h.mu.Unlock()
	if open := openPrompts(h); len(open) != 1 || open[0] != p {
		t.Fatalf("a resync changed the prompts: %+v", open)
	}

	// Each later block of the pane opens one prompt.
	status(h, "block:b", map[string]any{"state": "working", "app": "claude-code"})
	if open := openPrompts(h); len(open) != 0 {
		t.Fatalf("working left prompts: %+v", open)
	}
	status(h, "block:b", map[string]any{"state": "blocked", "kind": "permission", "app": "claude-code", "msg": "Allow Edit?"})
	if open := openPrompts(h); len(open) != 1 || open[0].Agent != id.ID {
		t.Fatalf("a later block opens one prompt, the identity's: %+v", open)
	}
	if c := lastClose(h, "alice"); c.ID != legacy.ID {
		t.Fatalf("alice got a prompt again: %+v", c)
	}
}

// Only a current identity owns a prompt. A superseded or detached identity that memory still binds
// to the pane (a stale binding) has none: the rule reads IdState, not only the binding.
func TestPromptOnlyCurrentIdentity(t *testing.T) {
	h, a := promptHub(t)
	ask := map[string]any{"state": "blocked", "kind": "permission", "app": "claude-code", "msg": "Allow Bash?"}
	bound := func(id, state string) *Agent {
		return &Agent{ID: id, Kind: "claude", IdState: state, Session: a.Session, BlockID: a.BlockID,
			ChildPID: a.ChildPID, ChildStart: a.ChildStart, delivered: map[string]int64{}}
	}
	c1, c2, c3 := bound("claude:c1", "superseded"), bound("claude:c2", "current"), bound("claude:c3", "detached")
	h.mu.Lock()
	h.idents = map[string]*Agent{c1.ID: c1, c2.ID: c2, c3.ID: c3}
	h.aliases = map[string]string{"alice": c1.ID}
	h.mu.Unlock()
	status(h, "block:b", ask)
	if open := openPrompts(h); len(open) != 1 || open[0].Agent != c2.ID {
		agents := []string{}
		for _, p := range open {
			agents = append(agents, p.Agent)
		}
		t.Fatalf("one prompt for the pane, the current identity's: %v", agents)
	}
}

// promptEvents lists the prompt events in order: "opened <agent>" or "closed <agent> <reason>".
func promptEvents(t *testing.T, h *Hub) []string {
	t.Helper()
	b, err := os.ReadFile(h.ev.f.Name())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e["event"] {
		case "prompt_opened":
			out = append(out, "opened "+e["agent"].(string))
		case "prompt_closed":
			out = append(out, "closed "+e["agent"].(string)+" "+e["reason"].(string))
		}
	}
	return out
}

// The real owner transition through the store (identitySync, as a report, a restart and a
// reconcile run it): legacy pane alice -> the first report (claude:c1 takes alice over) -> /clear
// (claude:c2 in the same pane, claude:c1 superseded). At each step the pane has one prompt; each
// close comes before the next open. Late answers to alice's and c1's prompts write nothing, and a
// same-key replay returns the same refusal.
func TestPromptOwnerTransitionStore(t *testing.T) {
	h, a, pl, f := answerHub(t)
	ctx := context.Background()
	if _, err := h.db.Exec(identitySchema); err != nil {
		t.Fatal(err)
	}
	h.rex = replyRex(t, func(method string) (any, string) {
		if method == "com.superlogical.terminal.program_status" {
			return map[string]any{"records": []any{map[string]any{"id": "", "state": "blocked", "kind": "question", "msg": "Pick a color"}}}, ""
		}
		return map[string]any{}, ""
	})
	pa := openPrompt(h, "alice")
	if pa == nil {
		t.Fatal("the legacy pane has no prompt")
	}
	sync := func() {
		t.Helper()
		if err := h.identitySync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	one := func(owner string) *prompt {
		t.Helper()
		open := openPrompts(h)
		if len(open) != 1 || open[0].Agent != owner {
			agents := []string{}
			for _, p := range open {
				agents = append(agents, p.Agent)
			}
			t.Fatalf("the pane's prompts %v, want one of %s", agents, owner)
		}
		return open[0]
	}

	// The first report: claude:c1 takes alice over.
	h.db.Exec(`INSERT INTO identity_panes VALUES (?, ?, ?, ?, 'claude', 'claude:c1', '')`, a.BlockID, a.Session, a.ChildPID, a.ChildStart)
	h.db.Exec(`INSERT INTO identities VALUES ('claude:c1', 'claude', 'c1', 'current', ?, '', 'Alice', 'x', 'x')`, a.BlockID)
	h.db.Exec(`INSERT INTO identity_aliases VALUES ('alice', 'claude:c1', 'x')`)
	sync()
	p1 := one("claude:c1")

	// /clear: claude:c2 in the same pane; claude:c1 is superseded.
	h.db.Exec(`UPDATE identity_panes SET current = 'claude:c2'`)
	h.db.Exec(`UPDATE identities SET state = 'superseded' WHERE identity = 'claude:c1'`)
	h.db.Exec(`INSERT INTO identities VALUES ('claude:c2', 'claude', 'c2', 'current', ?, 'claude:c1', 'Alice', 'x', 'x')`, a.BlockID)
	sync()
	p2 := one("claude:c2")

	// A resync (a restart or a reconcile) keeps the one prompt.
	sync()
	if one("claude:c2") != p2 {
		t.Fatal("a resync replaced the prompt")
	}
	want := []string{"opened alice", "closed alice alias", "opened claude:c1", "closed claude:c1 gone", "opened claude:c2"}
	if got := promptEvents(t, h); strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("prompt events:\n got %q\nwant %q", got, want)
	}

	// Late answers to the closed prompts: refused, zero bytes; a same-key replay is the same.
	for _, c := range []struct{ id, key string }{{pa.ID, "late-alice"}, {p1.ID, "late-c1"}} {
		for range 2 {
			out, err := answer(t, h, pl, c.id, c.key, map[string]any{"keys": "1"})
			if phaseOf(t, out, err) != "refused" || reasonOf(out) != "prompt_closed" {
				t.Fatalf("%s: %+v", c.key, out)
			}
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("late answers wrote %q", w)
	}
	// The current prompt still takes its answer.
	if ph := answerPhase(t, h, pl, p2.ID, "now", map[string]any{"keys": "1"}); ph != "done" || len(f.writes()) != 1 {
		t.Fatalf("the current prompt: %s, writes %q", ph, f.writes())
	}
}
