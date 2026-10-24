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
//   - The exec path's base name is the CLI of the identity's kind (harness.go exe): v1 restarts a
//     direct agent CLI only. A pane shell reports the shell, and a script launcher its interpreter
//     (lab, 2026-10-22). This is a name rule on the exec path, not on argv0; it cannot tell a
//     program deliberately named after the CLI.
//
// Missing evidence refuses: no recorded pid or start time, or a start time that cannot be read now.
func restartCheck(p paneState, rec paneRecord, liveStart, program, kind string) string {
	cli := rec.invoked
	if cli == "" {
		cli = p.invoked
	}
	switch {
	case rec.pid <= 0 || rec.start == "":
		return "the pane's process generation was not recorded"
	case p.child == 0:
		return "the pane has no program"
	case p.child != rec.pid:
		return fmt.Sprintf("the pane's program is pid %d, not the agent's pid %d", p.child, rec.pid)
	case liveStart == "":
		return fmt.Sprintf("the start time of pid %d cannot be read", rec.pid)
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
	case harnessOf(kind) == nil || filepath.Base(cli) != harnessOf(kind).exe:
		return fmt.Sprintf("the pane runs %s, not the %s CLI: a shell or a launcher is not restarted", cli, kind)
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
	if t.binding != 0 {
		if err := bindingCurrent(ctx, h.db, t.identity, t.binding); err != nil {
			return p, "not_current: " + bindingName(t.binding) + " is not current", nil
		}
	}
	var kind string
	h.db.QueryRowContext(ctx, `SELECT kind FROM identities WHERE identity = ?`, t.identity).Scan(&kind)
	if p, err = h.paneProgram(ctx, t.stopSession, t.stopBlock); err != nil {
		return p, "", err
	}
	return p, restartCheck(p, rec, startTime(p.child), t.program, kind), nil
}

// restartTargets resolves the request once, at admission. A target is admitted only if its process
// passes restartCheck now; the check runs again before each key write and each signal.
func (h *Hub) restartTargets(ctx context.Context, req map[string]any) ([]opTarget, error) {
	var ids []string
	if a, ok := req["agent"].(string); ok {
		id := a
		id = h.spawnIdentity(ctx, a)
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
// step waits, then escalates. The stop's outcome is stored apart from the phase (stop_result), so
// the reply still has it after the resume.
func (h *Hub) restartAdvance(ctx context.Context, t *opTarget, since time.Time) error {
	gone, err := procGone(t.childPID, t.childStart)
	if err != nil {
		// No answer is no proof of an exit: look again, then fail.
		if time.Since(since) > restartTermWait {
			h.stopFailed(ctx, t, "the process state cannot be read: "+err.Error())
		}
		return nil
	}
	if gone {
		return h.restartStopped(ctx, t, stopResults[t.phase], since)
	}
	if state, block := h.identityAt(ctx, t.identity); state != "current" || block != t.stopBlock {
		h.stopFailed(ctx, t, fmt.Sprintf("the agent changed during its stop (now %s); nothing more was sent", state))
		return nil
	}
	p, why, err := h.restartLook(ctx, t)
	if err != nil {
		if time.Since(since) > restartTermWait {
			h.stopFailed(ctx, t, "the pane could not be read: "+err.Error())
		}
		return nil
	}
	if why != "" {
		// The process may have exited since the first look: Rex, its parent, reports no child, or the
		// pid is gone. That is the stop's end, not a failed check.
		if gone, _ := procGone(t.childPID, t.childStart); gone || p.read && p.child == 0 {
			return h.restartStopped(ctx, t, stopResults[t.phase], since)
		}
		h.stopFailed(ctx, t, why+"; nothing more was sent")
		return nil
	}
	signal := func(to string, sig syscall.Signal, name, why string) {
		if !h.setPhase(ctx, t, t.phase, to, why+"; "+name+" sent", nil) {
			return
		}
		switch err := syscall.Kill(t.childPID, sig); {
		case errors.Is(err, syscall.ESRCH):
			h.restartStopped(ctx, t, "exited before "+name, since)
		case err != nil:
			h.stopFailed(ctx, t, name+" failed: "+err.Error())
		}
	}
	switch t.phase {
	case "restart_admitted":
		// Exit keys go only to the agent itself: another foreground process would read them.
		if p.foreground != t.childPID {
			signal("terminating", syscall.SIGTERM, "SIGTERM", fmt.Sprintf("the agent (pid %d) is not in the foreground: no exit keys", t.childPID))
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
			signal("terminating", syscall.SIGTERM, "SIGTERM", fmt.Sprintf("no exit within %s of the exit keys", restartKeysWait))
		}
	case "terminating":
		if time.Since(since) > restartTermWait {
			signal("killing", syscall.SIGKILL, "SIGKILL", fmt.Sprintf("no exit within %s of SIGTERM", restartTermWait))
		}
	case "killing":
		if time.Since(since) > restartKillWait {
			h.stopFailed(ctx, t, fmt.Sprintf("pid %d did not exit within %s of SIGKILL", t.childPID, restartKillWait))
		}
	}
	return nil
}

// stopResults is the stop's outcome by the phase in which the process was seen gone.
var stopResults = map[string]string{
	"restart_admitted": "exited before any key or signal",
	"stopping":         "exited after the exit keys",
	"terminating":      "exited after SIGTERM",
	"killing":          "exited after SIGKILL",
}

// procGone reports whether the process lifetime pid/start has ended: the system has no such pid, or
// the pid is another process (another start time). A failed read is no answer: an error.
func procGone(pid int, start string) (bool, error) {
	if pid <= 0 || start == "" {
		return false, errors.New("no recorded process")
	}
	r, err := procRead(pid)
	if errors.Is(err, errProcGone) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return r.start != start, nil
}

// stopFailed ends a restart target whose stop did not finish; the agent was not stopped.
func (h *Hub) stopFailed(ctx context.Context, t *opTarget, why string) {
	h.setPhase(ctx, t, t.phase, "failed", why, map[string]string{"stop_result": "not stopped: " + why})
}

// exitKeys writes the exit keys of the agent's kind (harness.go: exitKeys Ctrl-C, exitGap apart).
func (h *Hub) exitKeys(ctx context.Context, t *opTarget) error {
	var kind string
	h.db.QueryRowContext(ctx, `SELECT kind FROM identities WHERE identity = ?`, t.identity).Scan(&kind)
	write := func() error {
		return h.rex.BlockCall(ctx, "com.superlogical.terminal.write", t.stopSession, t.stopBlock, map[string]any{"data": "\x03"}, nil)
	}
	if err := write(); err != nil {
		return err
	}
	cli := harnessOf(kind)
	for i := 1; cli != nil && i < cli.exitKeys; i++ {
		time.Sleep(cli.exitGap)
		// A later key goes only to the same process: it may have exited after the first.
		if p, why, err := h.restartLook(ctx, t); err != nil || why != "" || p.foreground != t.childPID {
			return nil
		}
		if err := write(); err != nil {
			return err
		}
	}
	return nil
}

// restartStopped ends the stop when the verified process is gone. The identity is detached as its
// pane's exit event would detach it (the event may come later); then the target is an admitted
// resume of the same generation. result is the stop's outcome (stop_result). While the identity is
// still current in its pane (the exit is not complete yet), the next step looks again.
func (h *Hub) restartStopped(ctx context.Context, t *opTarget, result string, since time.Time) error {
	h.identityChildExited(ctx, t.stopBlock)
	var state, block, updated string
	h.db.QueryRowContext(ctx, `SELECT state, block_id, updated FROM identities WHERE identity = ?`, t.identity).Scan(&state, &block, &updated)
	if state == "current" && block == t.stopBlock && time.Since(since) <= restartTermWait {
		return nil
	}
	if state != "detached" || block != t.stopBlock {
		h.setPhase(ctx, t, t.phase, "failed", fmt.Sprintf("the program stopped, but the agent is %s, not detached from its pane", state),
			map[string]string{"stop_result": result})
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := h.db.ExecContext(ctx, `UPDATE agent_op_targets SET phase = 'admitted', admitted = ?, child_pid = 0, child_start = '', detail = ?, stop_result = ?, updated = ?
		WHERE op_id = ? AND identity = ? AND phase = ?`, updated, fmt.Sprintf("pid %d %s", t.childPID, result), result, now, t.op, t.identity, t.phase)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		h.ev.emit("agent_op_phase", map[string]any{"op_id": t.op, "agent": t.identity, "phase": "admitted", "stop_result": result})
		h.kickResume()
	}
	return nil
}
