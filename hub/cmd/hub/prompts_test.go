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
