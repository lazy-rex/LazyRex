package main

// Agent resume (docs/hub.md, "Agent resume"): a detached agent identity continues its conversation in
// a new pane, from its profile's resume template.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"
)

// validResume checks a profile's resume template. ${conversation_id} must appear; every other
// token is a declared arg. Tokens are whole arguments and never the program, as in the command.
func validResume(p Profile, declared map[string]bool) error {
	if p.Resume == nil {
		return nil
	}
	if p.Kind != "agent" {
		return errors.New("resume is only for agent profiles")
	}
	if len(p.Resume) == 0 || p.Resume[0] == "" {
		return errors.New("resume needs a program")
	}
	conv := false
	for i, a := range p.Resume {
		for _, m := range argToken.FindAllStringSubmatch(a, -1) {
			if a != m[0] || i == 0 {
				return fmt.Errorf("resume: %s must be a whole argument, and not the program", m[0])
			}
			if m[1] == "conversation_id" {
				conv = true
			} else if !declared[m[1]] {
				return fmt.Errorf("resume: %s is not a declared arg", m[0])
			}
		}
	}
	if !conv {
		return errors.New("resume must contain ${conversation_id}")
	}
	return nil
}

// resumeArgv is the spawn argv (the same environment cleanup) with the resume template as the
// command. The spawn's resolved args fill their tokens; the identity's conversation fills
// ${conversation_id}, never an arg.
func resumeArgv(s *spawnRow, prof Profile, resolved map[string]string, conversation string) []string {
	vals := make(map[string]string, len(resolved)+1)
	for k, v := range resolved {
		vals[k] = v
	}
	vals["conversation_id"] = conversation
	prof.Command = prof.Resume
	return spawnArgv(s, prof, vals)
}

// An agent operation (hub.admin.resume) is durable: the request, its frozen target list and each
// target's phase are rows. A retry with the same key reads them; it never resolves targets again.
//
// Target phases, in order: admitted, launching (the spawn row exists; the create request may be in
// flight), launched (the pane is open), prompt_sending (the first prompt may be written), prompt_sent,
// rebound. Terminal: rebound, refused, failed, released. unknown is NOT terminal: the launch or the
// prompt may still take effect, so the target keeps its reservation until a look proves an end
// (rebound, or the pane closed without a rebind) or the user releases it.
const agentOpsSchema = `
CREATE TABLE IF NOT EXISTS agent_ops (op_id TEXT PRIMARY KEY, caller TEXT NOT NULL, key TEXT NOT NULL,
  action TEXT NOT NULL, request TEXT NOT NULL, created TEXT NOT NULL, UNIQUE (caller, key));
CREATE TABLE IF NOT EXISTS agent_op_targets (op_id TEXT NOT NULL, identity TEXT NOT NULL, seq INTEGER NOT NULL,
  phase TEXT NOT NULL, source_spawn TEXT NOT NULL DEFAULT '', spawn_id TEXT NOT NULL DEFAULT '',
  block_id TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', updated TEXT NOT NULL,
  admitted TEXT NOT NULL DEFAULT '', -- identities.updated at admission: the generation that may launch
  child_pid INTEGER NOT NULL DEFAULT 0, child_start TEXT NOT NULL DEFAULT '', -- the new pane's program, at its first look
  PRIMARY KEY (op_id, identity));
-- One active target for each identity, across operations and keys.
CREATE UNIQUE INDEX IF NOT EXISTS agent_op_active ON agent_op_targets (identity)
  WHERE phase NOT IN ('rebound', 'refused', 'failed', 'released');
`

const (
	// resumePrompt is the first prompt of a resumed agent: a resumed Codex runs its hook (and so
	// rebinds) only at its first prompt (S1, 2026-10-22). It is the text of a message notice.
	resumePrompt = "New message. Call messaging_read."
	// resumeInFlight bounds the targets between admitted and rebound: each launch makes the hub
	// tell every plugin that agents changed. An implementation choice, not a measured limit.
	resumeInFlight = 3
	// resumeReadyWait bounds the wait for the new pane's program status (idle or done), and
	// resumeRebindWait the wait for the hook after the prompt. Implementation choices: S1 saw about
	// 4 s and 1 s.
	resumeReadyWait  = 120 * time.Second
	resumeRebindWait = 120 * time.Second
)

var activePhases = []string{"admitted", "launching", "launched", "prompt_sending", "prompt_sent", "unknown"}

type opTarget struct {
	op, identity, phase, sourceSpawn, spawn, block, detail, updated, admitted string
	seq, childPID                                                             int
	childStart                                                                string
}

// adminResume is hub.admin.resume {key, agent} | {key, kind?, session_id?} | {key, release}. The
// admin guard runs before it (main.go). The reply is the operation's current state; the caller
// reads it again with the same key.
func (h *Hub) adminResume(ctx context.Context, args map[string]any) (any, error) {
	key, _ := args["key"].(string)
	if key == "" || len(key) > 128 {
		return nil, bad("key is required (at most 128 characters)")
	}
	req := map[string]any{"action": "resume"}
	for name, v := range args {
		switch name {
		case "key":
		case "agent", "kind", "session_id", "release":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, bad("%s must be a non-empty string", name)
			}
			req[name] = s
		default:
			return nil, bad("unknown argument %q", name)
		}
	}
	if req["release"] != nil && len(req) > 2 || req["agent"] != nil && (req["kind"] != nil || req["session_id"] != nil) {
		return nil, bad("give agent, or kind and session_id, or release")
	}
	fp, _ := json.Marshal(req) // map keys marshal sorted: the normalized request
	var opID, prev string
	err := h.db.QueryRowContext(ctx, `SELECT op_id, request FROM agent_ops WHERE caller = 'admin' AND key = ?`, key).Scan(&opID, &prev)
	if err == nil {
		if prev != string(fp) {
			return nil, bad("key %q was used for a different request", key)
		}
		h.kickResume()
		return h.opReply(ctx, opID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if id, ok := req["release"].(string); ok {
		return h.releaseTarget(ctx, key, string(fp), id)
	}
	targets, err := h.resumeTargets(ctx, req)
	if err != nil {
		return nil, err
	}
	opID = randomID("op-")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_ops VALUES (?, 'admin', ?, 'resume', ?, ?)`, opID, key, string(fp), now); err != nil {
		return nil, err
	}
	for i, t := range targets {
		if t.phase == "admitted" {
			var other string
			// The store has one connection, so this check and the insert see no other admission.
			if tx.QueryRowContext(ctx, `SELECT op_id FROM agent_op_targets WHERE identity = ? AND phase IN `+inList(activePhases), t.identity).Scan(&other) == nil {
				t.phase, t.detail = "refused", "busy: operation "+other+" is active for this agent"
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_op_targets (op_id, identity, seq, phase, source_spawn, detail, updated, admitted) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			opID, t.identity, i, t.phase, t.sourceSpawn, t.detail, now, t.admitted); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	h.ev.emit("agent_op_admitted", map[string]any{"op_id": opID, "action": "resume", "targets": len(targets)})
	h.kickResume()
	return h.opReply(ctx, opID)
}

func inList(vals []string) string {
	return "('" + strings.Join(vals, "', '") + "')"
}

// resumeTargets resolves the request once, at admission. Each identity is admitted or refused with
// its reason.
func (h *Hub) resumeTargets(ctx context.Context, req map[string]any) ([]opTarget, error) {
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
		rows, err := h.db.QueryContext(ctx, `SELECT identity FROM identities WHERE state = 'detached' AND (? = '' OR kind = ?) ORDER BY identity`, kind, kind)
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
		var state, block string
		h.db.QueryRowContext(ctx, `SELECT state, block_id, updated FROM identities WHERE identity = ?`, id).Scan(&state, &block, &t.admitted)
		src, err := h.sourceSpawn(ctx, id, block)
		switch {
		case state != "detached":
			t.detail = "the agent is " + state + ", not detached"
		case errors.Is(err, sql.ErrNoRows):
			t.detail = "no spawn record: an agent started by hand cannot be resumed yet"
		case err != nil:
			return nil, err
		case session != "" && src.Session != session:
			continue // the batch selects this session only
		case byID[src.Profile].Resume == nil:
			t.detail = fmt.Sprintf("the profile %q has no resume template", src.Profile)
		default:
			t.phase, t.sourceSpawn = "admitted", src.ID
		}
		out = append(out, t)
	}
	return out, nil
}

// sourceSpawn is the spawn whose profile, session, cwd, title and args the identity resumes with:
// the spawn of the identity's last pane, else the spawn of an agent ID that became its alias.
func (h *Hub) sourceSpawn(ctx context.Context, identity, block string) (*spawnRow, error) {
	if block != "" {
		s, err := scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE block_id = ? ORDER BY created DESC LIMIT 1`, block))
		if !errors.Is(err, sql.ErrNoRows) {
			return s, err
		}
	}
	return scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE agent_id != '' AND agent_id IN
		(SELECT legacy FROM identity_aliases WHERE identity = ?) ORDER BY created DESC LIMIT 1`, identity))
}

// releaseTarget ends the reservation of an identity's active target (phase released). It is
// stored as its own operation, so a retry of the same key replays it.
func (h *Hub) releaseTarget(ctx context.Context, key, request, identity string) (any, error) {
	h.db.QueryRowContext(ctx, `SELECT identity FROM identity_aliases WHERE legacy = ?`, identity).Scan(&identity)
	opID := randomID("op-")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_ops VALUES (?, 'admin', ?, 'release', ?, ?)`, opID, key, request, now); err != nil {
		return nil, err
	}
	var held, phase string
	detail := "nothing to release: no active operation for this agent"
	if tx.QueryRowContext(ctx, `SELECT op_id, phase FROM agent_op_targets WHERE identity = ? AND phase IN `+inList(activePhases), identity).Scan(&held, &phase) == nil {
		detail = fmt.Sprintf("released operation %s in phase %s", held, phase)
		if _, err := tx.ExecContext(ctx, `UPDATE agent_op_targets SET phase = 'released', detail = ?, updated = ? WHERE op_id = ? AND identity = ?`,
			"released by the user in phase "+phase, now, held, identity); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_op_targets (op_id, identity, seq, phase, detail, updated) VALUES (?, ?, 0, 'released', ?, ?)`, opID, identity, detail, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return h.opReply(ctx, opID)
}

func (h *Hub) opTargets(ctx context.Context, where string, args ...any) ([]opTarget, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT op_id, identity, seq, phase, source_spawn, spawn_id, block_id, detail, updated, admitted, child_pid, child_start
		FROM agent_op_targets WHERE `+where+` ORDER BY updated, seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []opTarget
	for rows.Next() {
		var t opTarget
		if err := rows.Scan(&t.op, &t.identity, &t.seq, &t.phase, &t.sourceSpawn, &t.spawn, &t.block, &t.detail, &t.updated, &t.admitted, &t.childPID, &t.childStart); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (h *Hub) opReply(ctx context.Context, opID string) (any, error) {
	ts, err := h.opTargets(ctx, `op_id = ?`, opID)
	if err != nil {
		return nil, err
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].seq < ts[j].seq })
	list := []any{}
	done := true
	for _, t := range ts {
		e := map[string]any{"agent": t.identity, "phase": t.phase}
		for k, v := range map[string]string{"detail": t.detail, "block_id": t.block, "spawn_id": t.spawn} {
			if v != "" {
				e[k] = v
			}
		}
		done = done && !slices.Contains(activePhases, t.phase)
		list = append(list, e)
	}
	return map[string]any{"op_id": opID, "done": done, "targets": list}, nil
}

// setPhase moves a target from one phase to the next. It changes nothing when the target left
// from (a release, or another step): the caller then stops.
func (h *Hub) setPhase(ctx context.Context, t *opTarget, from, to, detail string, cols map[string]string) bool {
	set, vals := "phase = ?, detail = ?, updated = ?", []any{to, detail, time.Now().UTC().Format(time.RFC3339Nano)}
	for _, c := range []string{"spawn_id", "block_id"} {
		if v, ok := cols[c]; ok {
			set += ", " + c + " = ?"
			vals = append(vals, v)
		}
	}
	res, err := h.db.ExecContext(ctx, `UPDATE agent_op_targets SET `+set+` WHERE op_id = ? AND identity = ? AND phase = ?`,
		append(vals, t.op, t.identity, from)...)
	if err != nil {
		log.Printf("agent op %s %s: %s -> %s: %v", t.op, t.identity, from, to, err)
		return false
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false
	}
	t.phase, t.detail = to, detail
	if v, ok := cols["spawn_id"]; ok {
		t.spawn = v
	}
	if v, ok := cols["block_id"]; ok {
		t.block = v
	}
	h.ev.emit("agent_op_phase", map[string]any{"op_id": t.op, "agent": t.identity, "phase": to, "detail": detail})
	return true
}

// kickResume wakes resumeLoop. Repeated kicks coalesce.
func (h *Hub) kickResume() {
	if h.resumeKick == nil {
		return
	}
	select {
	case h.resumeKick <- struct{}{}:
	default:
	}
}

// resumeLoop drives the active targets. It runs on kicks, and every 2 s while a target is active:
// the waits (ready status, the hook) are looks, not events.
func (h *Hub) resumeLoop(ctx context.Context) {
	// A prompt write that may have run before the hub stopped is never written again.
	h.db.ExecContext(ctx, `UPDATE agent_op_targets SET phase = 'unknown', detail = 'the hub stopped while it wrote the first prompt', updated = ?
		WHERE phase = 'prompt_sending'`, time.Now().UTC().Format(time.RFC3339Nano))
	for {
		active, err := h.resumeStep(ctx)
		if err != nil {
			log.Printf("agent ops: %v", err)
		}
		var tick <-chan time.Time
		if active || err != nil {
			tick = time.After(2 * time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-h.resumeKick:
		case <-tick:
		}
	}
}

// resumeStep advances each active target by one look. It reports whether a target is still active.
func (h *Hub) resumeStep(ctx context.Context) (bool, error) {
	ts, err := h.opTargets(ctx, `phase IN `+inList(activePhases))
	if err != nil {
		return false, err
	}
	inFlight := 0
	for _, t := range ts {
		if t.phase != "admitted" {
			inFlight++
		}
	}
	for i := range ts {
		t := &ts[i]
		if t.phase == "admitted" {
			if inFlight >= resumeInFlight {
				continue
			}
			inFlight++
		}
		if err := h.resumeAdvance(ctx, t); err != nil {
			return true, err
		}
	}
	return len(ts) > 0, nil
}

// identityAt reports the identity's state and block.
func (h *Hub) identityAt(ctx context.Context, identity string) (state, block string) {
	h.db.QueryRowContext(ctx, `SELECT state, block_id FROM identities WHERE identity = ?`, identity).Scan(&state, &block)
	return state, block
}

func (h *Hub) resumeAdvance(ctx context.Context, t *opTarget) error {
	// Rebound: the identity's own hook reported from the target's pane. An earlier pane (an older
	// attempt) has another block, so its report does not complete this target.
	if t.block != "" {
		if state, block := h.identityAt(ctx, t.identity); state == "current" && block == t.block {
			h.setPhase(ctx, t, t.phase, "rebound", "", nil)
			return nil
		}
	}
	since, _ := time.Parse(time.RFC3339Nano, t.updated)
	switch t.phase {
	case "admitted":
		return h.resumeLaunch(ctx, t)
	case "launching", "unknown":
		if t.spawn == "" {
			return nil
		}
		s, err := scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE spawn_id = ?`, t.spawn))
		if err != nil {
			return err
		}
		switch {
		case s.Status == "failed":
			h.setPhase(ctx, t, t.phase, "failed", "the pane was not created: "+s.Error, nil)
		case s.Status == "closed" && (t.phase == "unknown" || t.block == ""):
			// The pane is gone and its hook never rebound: nothing of this attempt can still act.
			h.setPhase(ctx, t, t.phase, "failed", "the pane closed before the agent rebound", nil)
		case t.phase == "launching" && s.Status == "open":
			h.setPhase(ctx, t, "launching", "launched", "", map[string]string{"block_id": s.Block})
		}
	case "launched":
		s, err := scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE spawn_id = ?`, t.spawn))
		if err != nil {
			return err
		}
		if s.Status == "closed" {
			h.setPhase(ctx, t, "launched", "failed", "the pane closed before its first prompt", nil)
			return nil
		}
		p, err := h.paneProgram(ctx, s.Session, s.Block)
		switch {
		case err == nil && p.child == 0:
			h.setPhase(ctx, t, "launched", "failed", "the program exited before its first prompt", nil)
			return nil
		case err == nil && t.childPID == 0:
			// The first look records the launched program: the prompt goes only to it.
			t.childPID, t.childStart = p.child, startTime(p.child)
			h.db.ExecContext(ctx, `UPDATE agent_op_targets SET child_pid = ?, child_start = ? WHERE op_id = ? AND identity = ? AND phase = 'launched'`,
				t.childPID, t.childStart, t.op, t.identity)
		case err == nil && (p.child != t.childPID || startTime(p.child) != t.childStart):
			h.setPhase(ctx, t, "launched", "unknown", fmt.Sprintf("the pane's program changed (pid %d, launched %d)", p.child, t.childPID), nil)
			return nil
		}
		ready := err == nil && (p.state == "idle" || p.state == "done")
		if ready && p.owner != t.childPID {
			// Another process reports the status: it is not proof that the launched program waits.
			h.setPhase(ctx, t, "launched", "unknown", fmt.Sprintf("the status owner (pid %d) is not the launched program (pid %d)", p.owner, t.childPID), nil)
			return nil
		}
		if !ready {
			if time.Since(since) > resumeReadyWait {
				h.setPhase(ctx, t, "launched", "unknown", fmt.Sprintf("no ready program status within %s", resumeReadyWait), nil)
			}
			return nil
		}
		// At most once: the phase is stored before the write, and prompt_sending is never written again.
		if !h.setPhase(ctx, t, "launched", "prompt_sending", "", nil) {
			return nil
		}
		if err := h.writeResumePrompt(ctx, s.Session, s.Block, t.identity); err != nil {
			h.setPhase(ctx, t, "prompt_sending", "unknown", "the first prompt write failed, its outcome is unknown: "+err.Error(), nil)
			return nil
		}
		h.setPhase(ctx, t, "prompt_sending", "prompt_sent", "", nil)
	case "prompt_sent":
		if time.Since(since) > resumeRebindWait {
			h.setPhase(ctx, t, "prompt_sent", "unknown", fmt.Sprintf("no hook report from the new pane within %s", resumeRebindWait), nil)
		}
	}
	return nil
}

// resumeLaunch admits the target's spawn (a row of its own, no new agent: the identity is the
// agent) and sends it through sendSpawn.
func (h *Hub) resumeLaunch(ctx context.Context, t *opTarget) error {
	src, err := scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE spawn_id = ?`, t.sourceSpawn))
	if err != nil {
		return err
	}
	var effRaw string
	h.db.QueryRowContext(ctx, `SELECT effective_args FROM spawns WHERE spawn_id = ?`, src.ID).Scan(&effRaw)
	eff := map[string]string{}
	json.Unmarshal([]byte(effRaw), &eff)
	var kind, conv string
	h.db.QueryRowContext(ctx, `SELECT kind, conversation_id FROM identities WHERE identity = ?`, t.identity).Scan(&kind, &conv)
	byID, _, err := h.loadProfiles()
	if err != nil {
		return err
	}
	prof, ok := byID[src.Profile]
	if !ok || prof.Resume == nil || conv == "" {
		h.setPhase(ctx, t, "admitted", "failed", fmt.Sprintf("the profile %q or its resume template is gone", src.Profile), nil)
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s := &spawnRow{ID: randomID("spawn-"), Caller: "admin", Key: "agent-op:" + t.op + ":" + t.identity, Profile: prof.ID,
		Title: src.Title, Parent: "admin", Status: "prepared", Created: now, Session: src.Session, Cwd: src.Cwd}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	why := ""
	// The identity must still be detached in the generation that was admitted: a manual resume, or
	// any report, since then refuses this target (the frozen list is not resolved again). The store
	// has one connection, so no identity report commits between this check and the spawn row.
	var state, updated string
	tx.QueryRowContext(ctx, `SELECT state, updated FROM identities WHERE identity = ?`, t.identity).Scan(&state, &updated)
	if state != "detached" || updated != t.admitted {
		tx.Rollback()
		h.setPhase(ctx, t, "admitted", "refused", fmt.Sprintf("the agent changed after admission (now %s)", state), nil)
		return nil
	}
	if path, owner, err := lockedPath(ctx, tx, s.Cwd); err != nil {
		return err
	} else if path != "" {
		why = fmt.Sprintf("%s is locked by %s", path, owner)
	} else if again, err := realDir(s.Cwd); err != nil || again != s.Cwd {
		why = fmt.Sprintf("the directory %s changed or was removed", s.Cwd)
	}
	if why != "" {
		tx.Rollback()
		h.setPhase(ctx, t, "admitted", "failed", why, nil)
		return nil
	}
	req, _ := json.Marshal(map[string]any{"resume": t.identity, "op": t.op})
	eff2, _ := json.Marshal(eff)
	if _, err := tx.ExecContext(ctx, `INSERT INTO spawns (`+spawnCols+`, request, effective_args, generation, resume_of) VALUES (?, ?, ?, ?, ?, '', 'admin', 'prepared', '', '', '', '', ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Caller, s.Key, s.Profile, s.Title, s.Created, s.Session, s.Cwd, string(req), string(eff2), h.generation, t.identity); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE agent_op_targets SET phase = 'launching', spawn_id = ?, updated = ? WHERE op_id = ? AND identity = ? AND phase = 'admitted'`,
		s.ID, now, t.op, t.identity)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil // released meanwhile
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	t.phase, t.spawn = "launching", s.ID
	h.ev.emit("agent_op_phase", map[string]any{"op_id": t.op, "agent": t.identity, "phase": "launching", "spawn_id": s.ID})
	if err := h.sendSpawn(ctx, s, workerPlacement{}, resumeArgv(s, prof, eff, conv)); err != nil {
		return err
	}
	switch s.Status {
	case "open":
		h.setPhase(ctx, t, "launching", "launched", "", map[string]string{"block_id": s.Block})
	case "failed":
		h.setPhase(ctx, t, "launching", "failed", "the pane was not created: "+s.Error, nil)
	default:
		// The outcome is unknown. resolveSpawns never adopts a pane by its label, and the hub
		// writes no prompt to a pane it has not seen open; the reservation stays.
		h.setPhase(ctx, t, "launching", "unknown", "the create request had no answer; the pane may exist", nil)
	}
	return nil
}

// paneState is a pane's program and the root program status record, read one after the other.
type paneState struct {
	child, owner int // 0: no child (it exited), no owner
	state        string
}

// paneProgram reads the pane's child process (terminal.process) and its root status record. Rex
// reports as the owner the process in the foreground when the status was written (lab, Rex 1203).
func (h *Hub) paneProgram(ctx context.Context, sess, block string) (paneState, error) {
	var p paneState
	if sess != h.workSession() {
		if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
			return p, err
		}
	}
	var proc struct {
		Child *struct {
			PID int `json:"pid"`
		} `json:"child"`
	}
	if err := h.rex.BlockCall(ctx, "com.superlogical.terminal.process", sess, block, nil, &proc); err != nil {
		return p, err
	}
	if proc.Child != nil {
		p.child = proc.Child.PID
	}
	var status struct {
		Records []struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Owner struct {
				PID int `json:"pid"`
			} `json:"owner"`
		} `json:"records"`
	}
	if err := h.rex.BlockCall(ctx, "com.superlogical.terminal.program_status", sess, block, nil, &status); err != nil {
		return p, err
	}
	for _, r := range status.Records {
		if r.ID == "" {
			p.state, p.owner = r.State, r.Owner.PID
		}
	}
	return p, nil
}

// writeResumePrompt writes the first prompt. In test mode a resume-prompt fault holds before the
// write (hold) or loses the answer after it (lose-answer).
func (h *Hub) writeResumePrompt(ctx context.Context, sess, block, identity string) error {
	match := func(mode string) func(*testFault) bool {
		return func(f *testFault) bool {
			return f.Point == "resume-prompt" && f.Mode == mode && (f.Key == "" || f.Key == identity)
		}
	}
	if f := h.takeFault(match("hold")); f != nil {
		h.waitRelease(f)
	}
	if err := h.writeNotice(ctx, sess, block, resumePrompt); err != nil {
		return err
	}
	if h.takeFault(match("lose-answer")) != nil {
		return errors.New("injected fault: the answer of the prompt write was lost")
	}
	return nil
}
