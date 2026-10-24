package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// Hub Status: the running agent panes that have no current conversation identity (the user,
// 2026-10-22; plan reviewed by codex-24 and the hub lead). The source is the identity field of
// hub.workers. The line is advice, shown at once: Codex can report only at its first prompt. It
// never changes the exit code.

// agentIdentityLines gives one line per running agent worker whose identity is "none". A failed
// read, a reply without the workers list, an unknown identity value, or a running or exited agent
// worker without the field (a malformed hub.workers answer) is unknown, never silence. Text from
// the hub reaches a terminal: paneSafe.
func agentIdentityLines(raw []byte, readErr error) []string {
	unknown := func(format string, a ...any) []string {
		return []string{paneSafe("agents: unknown: " + fmt.Sprintf(format, a...))}
	}
	if readErr != nil {
		return unknown("%v", readErr)
	}
	var reply struct {
		Workers *[]struct {
			Title    string  `json:"title"`
			Agent    string  `json:"agent_id"`
			Block    string  `json:"block_id"`
			State    string  `json:"state"`
			Identity *string `json:"identity"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return unknown("hub.workers: %v", err)
	}
	if reply.Workers == nil {
		return unknown("hub.workers has no workers list")
	}
	var lines []string
	for _, w := range *reply.Workers {
		if w.Agent == "" {
			continue
		}
		if w.Identity == nil {
			// The hub gives the field for every open agent spawn: running and exited workers. A
			// starting or failed worker has no pane process to report.
			if w.State == "running" || w.State == "exited" {
				return unknown("worker %s (%s): hub.workers gives no identity", w.Title, w.State)
			}
			continue
		}
		switch *w.Identity {
		case "current":
		case "none":
			// Only a running CLI can still report; an exited one is not a hook problem.
			if w.State == "running" {
				lines = append(lines, paneSafe(fmt.Sprintf("agent %s (pane %s): no current conversation identity yet. %s", w.Title, w.Block, harnessHelp())))
			}
		default:
			return unknown("worker %s: identity %q", w.Title, *w.Identity)
		}
	}
	return lines
}

// readAgents reads the agent identities of this state's hub for Hub Status. It reads nothing when
// no single hub of this state is connected.
func (v *ctlView) readAgents() {
	if v.own == nil || v.ownTwice {
		return
	}
	out, err := v.rexCmd(time.Now().Add(5*time.Second), "-C", v.own.ID, "do", "hub.workers")
	v.agents = agentIdentityLines(out, err)
}
