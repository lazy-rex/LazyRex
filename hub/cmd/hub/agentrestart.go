package main

// Agent restart (docs/hub.md, "Agent restart"): the hub stops a current agent's program and resumes
// its conversation in a new pane. The stop is the start of an agent operation (resume.go); after the
// stop the target is an admitted resume.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// The waits after the exit keys, after SIGTERM and after SIGKILL. Implementation choices.
	restartKeysWait = 5 * time.Second
	restartTermWait = 5 * time.Second
	restartKillWait = 5 * time.Second
	// claudeExitGap is the gap between the two Ctrl-C that end Claude (S1, 2026-10-22: 300 ms
	// exits, 1.5 s does not).
	claudeExitGap = 300 * time.Millisecond
)

// adminRestart is hub.admin.restart {key, agent} | {key, kind?, session_id?}. The admin guard runs
// before it (main.go). A target's reservation is released with hub.admin.resume {key, release}.
func (h *Hub) adminRestart(ctx context.Context, args map[string]any) (any, error) {
	return h.adminAgentOp(ctx, "restart", args)
}

// agentProgram is the program that the spawn of a pane executed, resolved as spawnArgv resolves it:
// the profile's command, or its resume template for a resume spawn.
func agentProgram(prof Profile, resumed bool) string {
	cmd := prof.Command
	if resumed {
		cmd = prof.Resume
	}
	if len(cmd) == 0 {
		return ""
	}
	p := cmd[0]
	if !filepath.IsAbs(p) {
		if found, err := exec.LookPath(p); err == nil {
			p = found
		}
	}
	return p
}

// paneRecord is what identity_panes holds for a pane's process generation: its pid and start time,
// and the executable and exec path that the Rex answer of an accepted identity report gave.
type paneRecord struct {
	pid                  int
	start, path, invoked string
}

// restartCheck reports why the pane's program is not the agent process to stop, or "". p is the
// live Rex answer, liveStart the start time of its child. The process is the pane's child (never an
// ancestor, never a descendant), of the recorded generation, and started as the configured program:
//   - The recorded exec path must equal program. Rex reports an exec path only while it still
//     resolves to the executable, so the record (from the report, before any update) is the proof,
//     not argv0 and not the live answer.
//   - The live executable must equal the recorded one: an exec to another path in the same pid is
//     refused. This detects a changed path; it does not prove that no exec of the same path ran.
//   - A row from before the record (no values): the live exec path must equal program; a null one
//     refuses.
func restartCheck(p paneState, rec paneRecord, liveStart, program string) string {
	switch {
	case p.child == 0:
		return "the pane has no program"
	case p.child != rec.pid:
		return fmt.Sprintf("the pane's program is pid %d, not the agent's pid %d", p.child, rec.pid)
	case liveStart != rec.start:
		return fmt.Sprintf("pid %d is another process now", rec.pid)
	case p.path == "":
		return "Rex reports no executable for the pane's program: the running executable was removed (macOS); restart the agent by hand"
	case rec.invoked == "" && rec.path != "", rec.invoked != "" && rec.path == "":
		return "the recorded program of the pane is incomplete"
	case rec.invoked != "" && rec.invoked != program:
		return fmt.Sprintf("the agent was started as %s, not the configured program %s", rec.invoked, program)
	case rec.invoked != "" && p.path != rec.path:
		return fmt.Sprintf("the program's executable changed from %s to %s", rec.path, p.path)
	case rec.invoked == "" && p.invoked == "":
		return "the exec path cannot be verified: none was recorded at the agent's report, and Rex reports none now (an update since its start?)"
	case rec.invoked == "" && p.invoked != program:
		return fmt.Sprintf("the pane runs %s, not the configured program %s", p.invoked, program)
	}
	return ""
}

// restartLook reads the pane's recorded generation and its live program, and checks them.
// why is "" when the target's process may be sent keys or a signal now.
func (h *Hub) restartLook(ctx context.Context, t *opTarget) (p paneState, why string, err error) {
	var rec paneRecord
	err = h.db.QueryRowContext(ctx, `SELECT child_pid, child_start, child_path, child_invoked FROM identity_panes WHERE block_id = ? AND current = ?`,
		t.stopBlock, t.identity).Scan(&rec.pid, &rec.start, &rec.path, &rec.invoked)
	if errors.Is(err, sql.ErrNoRows) {
		return p, "the agent is no longer bound to its pane", nil
	} else if err != nil {
		return p, "", err
	}
	if rec.pid != t.childPID || rec.start != t.childStart {
		return p, "the pane's recorded program changed", nil
	}
	if p, err = h.paneProgram(ctx, t.stopSession, t.stopBlock); err != nil {
		return p, "", err
	}
	return p, restartCheck(p, rec, startTime(p.child), t.program), nil
}

// restartTargets resolves the request once, at admission. A target is admitted only if its process
// passes restartCheck now; the check runs again before each key write and each signal.
func (h *Hub) restartTargets(ctx context.Context, req map[string]any) ([]opTarget, error) {
	var ids []string
	if a, ok := req["agent"].(string); ok {
		id := a
		h.db.QueryRowContext(ctx, `SELECT identity FROM identity_aliases WHERE legacy = ?`, a).Scan(&id)
		var n int
		if h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identities WHERE identity = ?`, id).Scan(&n); n == 0 {
			return nil, bad("unknown: no agent identity %q", a)
		}
		ids = []string{id}
	} else {
		kind, _ := req["kind"].(string)
		rows, err := h.db.QueryContext(ctx, `SELECT identity FROM identities WHERE state = 'current' AND (? = '' OR kind = ?) ORDER BY identity`, kind, kind)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	byID, _, err := h.loadProfiles()
	if err != nil {
		return nil, err
	}
	session, _ := req["session_id"].(string)
	var out []opTarget
	for _, id := range ids {
		t := opTarget{identity: id, phase: "refused"}
		var state, block, sess string
		h.db.QueryRowContext(ctx, `SELECT state, block_id, updated FROM identities WHERE identity = ?`, id).Scan(&state, &block, &t.admitted)
		paneErr := h.db.QueryRowContext(ctx, `SELECT session_id, child_pid, child_start FROM identity_panes WHERE block_id = ? AND current = ?`, block, id).
			Scan(&sess, &t.childPID, &t.childStart)
		if session != "" && sess != session {
			continue // the batch selects this session only
		}
		src, err := scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE block_id = ? ORDER BY created DESC LIMIT 1`, block))
		var resumeOf string
		if err == nil {
			h.db.QueryRowContext(ctx, `SELECT resume_of FROM spawns WHERE spawn_id = ?`, src.ID).Scan(&resumeOf)
		}
		switch {
		case state != "current":
			t.detail = "the agent is " + state + ", not current"
		case paneErr != nil || t.childPID == 0:
			t.detail = "the agent's pane has no recorded program"
		case errors.Is(err, sql.ErrNoRows):
			t.detail = "no spawn record for its pane: an agent started by hand cannot be restarted"
		case err != nil:
			return nil, err
		case byID[src.Profile].Resume == nil:
			t.detail = fmt.Sprintf("the profile %q has no resume template", src.Profile)
		default:
			t.stopBlock, t.stopSession, t.program = block, sess, agentProgram(byID[src.Profile], resumeOf != "")
			if _, why, err := h.restartLook(ctx, &t); err != nil {
				t.detail = "the pane could not be read: " + err.Error()
			} else if why != "" {
				t.detail = why
			} else {
				t.phase, t.sourceSpawn = "restart_admitted", src.ID
			}
			if t.phase == "refused" {
				t.stopBlock, t.stopSession, t.program = "", "", ""
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// restartAdvance takes one step of a restart's stop. Each step looks again, and writes or signals
// only after restartCheck passes. The phase is stored before the action: after a hub stop the next
// step waits, then escalates.
func (h *Hub) restartAdvance(ctx context.Context, t *opTarget, since time.Time) error {
	if !alive(t.childPID, t.childStart) {
		return h.restartStopped(ctx, t)
	}
	if state, block := h.identityAt(ctx, t.identity); state != "current" || block != t.stopBlock {
		h.setPhase(ctx, t, t.phase, "failed", fmt.Sprintf("the agent changed during its stop (now %s); nothing more was sent", state), nil)
		return nil
	}
	p, why, err := h.restartLook(ctx, t)
	if err != nil {
		if time.Since(since) > restartTermWait {
			h.setPhase(ctx, t, t.phase, "failed", "the pane could not be read: "+err.Error(), nil)
		}
		return nil
	}
	if why != "" {
		h.setPhase(ctx, t, t.phase, "failed", why+"; nothing more was sent", nil)
		return nil
	}
	signal := func(to string, sig syscall.Signal, detail string) {
		if h.setPhase(ctx, t, t.phase, to, detail, nil) {
			syscall.Kill(t.childPID, sig)
		}
	}
	switch t.phase {
	case "restart_admitted":
		// Exit keys go only to the agent itself: another foreground process would read them.
		if p.foreground != t.childPID {
			signal("terminating", syscall.SIGTERM, fmt.Sprintf("the agent (pid %d) is not in the foreground: no exit keys; SIGTERM sent", t.childPID))
			return nil
		}
		if !h.setPhase(ctx, t, "restart_admitted", "stopping", "exit keys sent", nil) {
			return nil
		}
		if err := h.exitKeys(ctx, t); err != nil {
			h.setPhase(ctx, t, "stopping", "stopping", "the exit keys failed: "+err.Error(), nil)
		}
	case "stopping":
		if time.Since(since) > restartKeysWait {
			signal("terminating", syscall.SIGTERM, fmt.Sprintf("no exit within %s of the exit keys; SIGTERM sent", restartKeysWait))
		}
	case "terminating":
		if time.Since(since) > restartTermWait {
			signal("killing", syscall.SIGKILL, fmt.Sprintf("no exit within %s of SIGTERM; SIGKILL sent", restartTermWait))
		}
	case "killing":
		if time.Since(since) > restartKillWait {
			h.setPhase(ctx, t, "killing", "failed", fmt.Sprintf("pid %d did not exit within %s of SIGKILL", t.childPID, restartKillWait), nil)
		}
	}
	return nil
}

// exitKeys writes the exit keys of the agent's kind: one Ctrl-C for Codex, two for Claude.
func (h *Hub) exitKeys(ctx context.Context, t *opTarget) error {
	var kind string
	h.db.QueryRowContext(ctx, `SELECT kind FROM identities WHERE identity = ?`, t.identity).Scan(&kind)
	write := func() error {
		return h.rex.BlockCall(ctx, "com.superlogical.terminal.write", t.stopSession, t.stopBlock, map[string]any{"data": "\x03"}, nil)
	}
	if err := write(); err != nil || kind != "claude" {
		return err
	}
	time.Sleep(claudeExitGap)
	// The second key goes only to the same process: it may have exited after the first.
	if p, why, err := h.restartLook(ctx, t); err != nil || why != "" || p.foreground != t.childPID {
		return nil
	}
	return write()
}

// restartStopped ends the stop when the verified process is gone. The identity is detached as its
// pane's exit event would detach it (the event may come later); then the target is an admitted
// resume of the same generation.
func (h *Hub) restartStopped(ctx context.Context, t *opTarget) error {
	h.identityChildExited(ctx, t.stopBlock)
	var state, block, updated string
	h.db.QueryRowContext(ctx, `SELECT state, block_id, updated FROM identities WHERE identity = ?`, t.identity).Scan(&state, &block, &updated)
	if state != "detached" || block != t.stopBlock {
		h.setPhase(ctx, t, t.phase, "failed", fmt.Sprintf("the program stopped, but the agent is %s, not detached from its pane", state), nil)
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := h.db.ExecContext(ctx, `UPDATE agent_op_targets SET phase = 'admitted', admitted = ?, child_pid = 0, child_start = '', detail = ?, updated = ?
		WHERE op_id = ? AND identity = ? AND phase = ?`, updated, fmt.Sprintf("pid %d stopped", t.childPID), now, t.op, t.identity, t.phase)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		h.ev.emit("agent_op_phase", map[string]any{"op_id": t.op, "agent": t.identity, "phase": "admitted", "detail": "stopped"})
		h.kickResume()
	}
	return nil
}
