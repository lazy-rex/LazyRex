package main

// Agent enrollment, readiness and notices (README sections 2 and 5).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sessionBlocks returns the block IDs in the hub's session.
func (h *Hub) sessionBlocks(ctx context.Context) (map[string]bool, error) {
	return h.sessionBlocksIn(ctx, h.workSession())
}

// sessionLive reports whether a session exists. A session_not_found answer is false.
func (h *Hub) sessionLive(ctx context.Context, sess string) (bool, error) {
	if sess == "" {
		return false, nil
	}
	_, err := h.sessionBlocksIn(ctx, sess)
	if sessionGone(err) {
		return false, nil
	}
	return err == nil, err
}

// sessionIDs lists every session on the server.
func (h *Hub) sessionIDs(ctx context.Context) ([]string, error) {
	var out struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := h.rex.Call(ctx, "session.list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	ids := make([]string, len(out.Sessions))
	for i, s := range out.Sessions {
		ids[i] = s.SessionID
	}
	return ids, nil
}

// findBlockSession returns the session that has the block, or "" if no session has it.
func (h *Hub) findBlockSession(ctx context.Context, block string) (string, error) {
	ids, err := h.sessionIDs(ctx)
	if err != nil {
		return "", err
	}
	for _, s := range ids {
		blocks, err := h.sessionBlocksIn(ctx, s)
		if sessionGone(err) {
			continue
		} else if err != nil {
			return "", err
		}
		if blocks[block] {
			return s, nil
		}
	}
	return "", nil
}

// sessionBlocksIn lists the blocks of any session. Rex refuses list_blocks, terminal.process and
// block.close on a session the connection has not attached ("stream is not attached", observed on
// Rex 1203, lab), so another session is attached first. A gone session answers session_not_found.
func (h *Hub) sessionBlocksIn(ctx context.Context, sess string) (map[string]bool, error) {
	if sess != h.workSession() {
		if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
			return nil, err
		}
	}
	var out struct {
		Blocks []struct {
			BlockID string `json:"block_id"`
		} `json:"blocks"`
	}
	if err := h.rex.Call(ctx, "session.list_blocks", map[string]any{"session_id": sess}, &out); err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, b := range out.Blocks {
		m[b.BlockID] = true
	}
	return m, nil
}

// childPID returns the pid of a block's child process, or 0 if it has none (exited).
func (h *Hub) childPID(ctx context.Context, blockID string) (int, error) {
	return h.childPIDIn(ctx, h.workSession(), blockID)
}

func (h *Hub) childPIDIn(ctx context.Context, sess, blockID string) (int, error) {
	c, err := h.childProcIn(ctx, sess, blockID)
	return c.pid, err
}

// paneChild is a block's child process as one Rex terminal.process answer reports it.
type paneChild struct {
	pid  int    // 0: no child (it exited)
	path string // the executable
	// invoked is the path given to exec (not argv0). Rex reports it only while it still resolves to
	// path: after a symlink moves (a CLI update) it is "" (lab, 2026-10-22).
	invoked string
}

// childProcIn reads a block's child process.
func (h *Hub) childProcIn(ctx context.Context, sess, blockID string) (paneChild, error) {
	var out struct {
		Child *struct {
			PID     int    `json:"pid"`
			Path    string `json:"path"`
			Invoked string `json:"invoked_path"`
		} `json:"child"`
	}
	if err := h.rex.BlockCall(ctx, "com.superlogical.terminal.process", sess, blockID, nil, &out); err != nil {
		return paneChild{}, err
	}
	if out.Child == nil {
		return paneChild{}, nil
	}
	return paneChild{out.Child.PID, out.Child.Path, out.Child.Invoked}, nil
}

// paneOf finds the pane whose child process is an ancestor of pid. Agents may run in any
// session: the work session is searched first, then every other session (attached first).
func (h *Hub) paneOf(ctx context.Context, pid int) (sess, blockID string, child int, err error) {
	sess, blockID, c, err := h.paneChildOf(ctx, pid)
	return sess, blockID, c.pid, err
}

// paneChildOf is paneOf with the child process as the same Rex answer reports it.
func (h *Hub) paneChildOf(ctx context.Context, pid int) (sess, blockID string, child paneChild, err error) {
	ids, err := h.sessionIDs(ctx)
	if err != nil {
		return "", "", paneChild{}, err
	}
	// The work session is "" while it is being created again: search the other sessions then.
	work := h.workSession()
	var order []string
	if work != "" {
		order = append(order, work)
	}
	for _, s := range ids {
		if s != work {
			order = append(order, s)
		}
	}
	for _, s := range order {
		blocks, err := h.sessionBlocksIn(ctx, s)
		if sessionGone(err) {
			continue
		} else if err != nil {
			return "", "", paneChild{}, err
		}
		for b := range blocks {
			c, err := h.childProcIn(ctx, s, b)
			if err == nil && c.pid > 0 && descendsFrom(pid, c.pid) {
				return s, b, c, nil
			}
		}
	}
	return "", "", paneChild{}, nil
}

// bindingSession is the session of an agent's pane. A binding from before sessions were recorded
// ran in the work session (the hub accepted joins only there).
func (h *Hub) bindingSession(a *Agent) string {
	if a.Session != "" {
		return a.Session
	}
	return h.workSession()
}

// errNoRecord: the pane has no program status record with the configured ID.
var errNoRecord = errors.New("no program status record")

// readStatus reads the state of a pane's program status record. Rex answers a session's method
// only on a connection attached to that session (observed on Rex 0.0.1792158932-g88fd41, lab:
// "conflict: stream is not attached to session"), so it attaches a session other than the work
// session first. The attach also brings that session's program_status events.
func (h *Hub) readStatus(ctx context.Context, sess, blockID, record string) (statusRecord, error) {
	if sess != h.workSession() {
		if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
			return statusRecord{}, err
		}
	}
	if f := h.statusReadFault(sess, "fail"); f != nil {
		return statusRecord{}, errors.New("injected fault: the status read failed")
	}
	var status struct {
		Records []struct {
			ID string `json:"id"`
			statusRecord
		} `json:"records"`
	}
	if err := h.rex.BlockCall(ctx, "com.superlogical.terminal.program_status", sess, blockID, nil, &status); err != nil {
		return statusRecord{}, err
	}
	for _, r := range status.Records {
		if r.ID == record {
			return r.statusRecord, nil
		}
	}
	return statusRecord{}, errNoRecord
}

// recordState is readStatus for a join, a rebind or a restore: a failed read is logged and gives
// the unknown state "". noticeLoop reads the state again when a notice waits (noticeStatus).
func (h *Hub) recordState(ctx context.Context, sess, blockID, record string) statusRecord {
	s, err := h.readStatus(ctx, sess, blockID, record)
	if err != nil {
		log.Printf("status of block %s (record %q): %v", blockID, record, err)
	}
	return s
}

func (h *Hub) saveBinding(ctx context.Context, agent, sess, block string, child int, start string) error {
	_, err := h.db.ExecContext(ctx, `INSERT INTO bindings (agent, session_id, block_id, child_pid, child_start) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (agent) DO UPDATE SET session_id = excluded.session_id, block_id = excluded.block_id,
		child_pid = excluded.child_pid, child_start = excluded.child_start`, agent, sess, block, child, start)
	return err
}

// loadBindings restores persisted agent bindings. A binding whose process is gone stays bound but not live.
func (h *Hub) loadBindings(ctx context.Context) error {
	rows, err := h.db.QueryContext(ctx, `SELECT agent, session_id, block_id, child_pid, child_start FROM bindings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, sess, block, start string
		var pid int
		if err := rows.Scan(&id, &sess, &block, &pid, &start); err != nil {
			return err
		}
		a := h.agents[id]
		if a == nil {
			log.Printf("binding for agent %q not in the roster; ignored", id)
			continue
		}
		a.Session, a.BlockID, a.ChildPID, a.ChildStart = sess, block, pid, start
		if a.live() {
			// Attach the agent's session: its status and close events arrive only then.
			if s := h.bindingSession(a); s != h.workSession() {
				h.sessionBlocksIn(ctx, s)
			}
			a.setRecord(h.recordState(ctx, h.bindingSession(a), block, a.StatusRecord))
		}
		log.Printf("agent %s: bound to %s pid %d live=%v state %q", id, block, pid, a.live(), a.State)
	}
	h.mu.Lock()
	h.syncPrompts()
	h.mu.Unlock()
	return rows.Err()
}

// join binds a roster agent to the caller's pane (README section 2). Attribution, not authentication.
func (h *Hub) join(ctx context.Context, p perform, args map[string]any) (any, error) {
	id, _ := args["agent"].(string)
	h.mu.Lock()
	a := h.agents[id]
	h.mu.Unlock()
	if a == nil {
		return nil, bad("agent %q is not in the roster", id)
	}
	// A dynamic agent (one a spawn created) may join only from its spawn's pane. The spawn row
	// decides: sending -> not ready (or adopt, below), open -> the recorded block only, else retired.
	var spawn *spawnRow
	if a.Profile != "" {
		s, err := scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE agent_id = ?`, id))
		if err != nil {
			return nil, fmt.Errorf("agent %s: spawn: %w", id, err)
		}
		if s.Status != "prepared" && s.Status != "sending" && s.Status != "open" {
			return nil, bad("agent %s is retired (its spawn is %s)", id, s.Status)
		}
		spawn = s
	}
	if v, ok := args["status_record"]; ok && v != a.StatusRecord {
		return nil, bad("the readiness policy of %s is fixed by hub configuration", id)
	}
	sess, block, child, err := h.paneOf(ctx, p.Requester.Principal.PID)
	if err != nil {
		return nil, err
	}
	if block == "" {
		return nil, bad("caller pid %d is not in a Rex pane", p.Requester.Principal.PID)
	}
	if spawn != nil && spawn.Status == "prepared" {
		// The create request is not sent yet, so no pane of this spawn exists.
		return nil, bad("not_ready: agent %s is still starting; retry", id)
	}
	if spawn != nil && spawn.Status == "sending" {
		// The spawn's outcome is unknown (the hub restarted during new_window). A join from the
		// one pane with the spawn's label establishes the block.
		st, err := h.adoptByJoin(ctx, spawn, block)
		if err != nil {
			return nil, err
		}
		if st != "open" {
			return nil, bad("not_ready: agent %s is still starting; retry", id)
		}
		spawn.Block = block
	}
	if spawn != nil && block != spawn.Block {
		return nil, bad("agent %s belongs to block %s", id, spawn.Block)
	}
	start := startTime(child)

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, other := range h.agents {
		if other != a && other.BlockID == block {
			return nil, bad("block %s is bound to agent %s", block, other.ID)
		}
	}
	if a.BlockID != "" && a.BlockID != block {
		// A dead pane does not free the name. Rebinding is an explicit admin action.
		return nil, bad("agent %s is bound to block %s; use hub.admin.rebind", id, a.BlockID)
	}
	if a.Session != sess || a.BlockID != block || a.ChildPID != child || a.ChildStart != start {
		if err := h.saveBinding(ctx, id, sess, block, child, start); err != nil {
			return nil, err
		}
		a.Session, a.BlockID, a.ChildPID, a.ChildStart = sess, block, child, start
	}
	if a.ClientID != p.Requester.ClientID {
		// A new shim connection may receive the current pending notice again.
		a.ClientID = p.Requester.ClientID
		a.delivered = map[string]int64{}
		h.mu.Unlock()
		rec := h.recordState(ctx, sess, block, a.StatusRecord)
		h.mu.Lock()
		a.setRecord(rec)
		h.ev.emit("agent_joined", map[string]any{"agent": id, "block_id": block})
		h.kickNotices()
		h.kickSpawns()
	}
	h.syncPrompts()
	return map[string]any{"agent": id, "block_id": block, "generation": revString(h.generation)}, nil
}

// kickNotices wakes noticeLoop. Call it when a notice can become due: a store commit with a pending
// notice, an agent status change (Rex program_status events), a join, a rebind, or a plugin
// registration. Repeated kicks coalesce.
func (h *Hub) kickNotices() {
	if h.noticeKick == nil {
		return
	}
	select {
	case h.noticeKick <- struct{}{}:
	default:
	}
}

// noticeLoop writes one notice per pending intent revision when its agent is ready.
// Rex has no atomic "write if idle", so the agent can become busy between check and write.
// It runs on kicks, not on a timer. A timer is armed only to retry: after a failed write, and after
// a status read that gave an unknown state (a backoff per agent, 5 s to 1 min; noticeStatus).
func (h *Hub) noticeLoop(ctx context.Context) {
	var retry <-chan time.Time
	// tries: agents whose status read gave no state, with the time of the next read (backoff from
	// 5 s to 1 min). A kick before that time does not read again.
	type try struct {
		next time.Time
		wait time.Duration
	}
	tries := map[string]*try{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.noticeKick:
		case <-retry:
		}
		retry = nil
		var next time.Time // the earliest retry that this pass asks for
		arm := func(d time.Duration) {
			if t := time.Now().Add(d); next.IsZero() || t.Before(next) {
				next = t
			}
		}
		pending, err := h.db.pendingNotices(ctx)
		if err != nil {
			log.Printf("notices: %v", err)
			retry = time.After(time.Second)
			continue
		}
		for _, n := range pending {
			h.mu.Lock()
			a, pl := h.noticeTarget(n.agent), h.plugins[n.ns]
			waiting := a != nil && pl != nil && pl.entry != nil && pl.run.Notice != "" && n.intentRev > a.delivered[n.ns] // a kept record gets no notices
			if a != nil && a.State != "" {
				delete(tries, n.agent) // a known state ends the backoff
			}
			if waiting && a.live() && a.State == "" {
				// The state is unknown: a read at start may have failed, and no status event came
				// since. Read it now. Unknown is never ready: a notice ends with CR, and a blocked
				// prompt could take it as an answer.
				snap := statusSnap{a: a, sess: h.bindingSession(a), block: a.BlockID, pid: a.ChildPID, record: a.StatusRecord, gen: a.stateGen}
				h.mu.Unlock()
				if t := tries[n.agent]; t != nil && time.Now().Before(t.next) {
					arm(time.Until(t.next))
				} else if h.noticeStatus(ctx, n.agent, snap) {
					delete(tries, n.agent)
				} else {
					if t == nil {
						t = &try{}
						tries[n.agent] = t
					}
					t.wait = min(max(2*t.wait, 5*time.Second), time.Minute)
					t.next = time.Now().Add(t.wait)
					arm(t.wait)
				}
				// Check the agent and the plugin again: a stop during the read clears the plugin's
				// run, and with it the notice. The notice then waits for the next run.
				h.mu.Lock()
				a, pl = h.noticeTarget(n.agent), h.plugins[n.ns]
				waiting = a != nil && pl != nil && pl.entry != nil && pl.run.Notice != "" && n.intentRev > a.delivered[n.ns]
			}
			due := waiting && a.ready()
			sess, block, conn, notice := "", "", "", ""
			if due {
				sess, block, conn, notice = h.bindingSession(a), a.BlockID, a.ClientID, pl.run.Notice // run changes at a spawn
			}
			h.mu.Unlock()
			if !due {
				continue
			}
			if err := h.writeNotice(ctx, sess, block, notice); err != nil {
				log.Printf("notice to %s: %v", n.agent, err) // not marked delivered
				arm(time.Second)
				continue
			}
			if f := h.faults.take("notice-written", "gate", ""); f != nil {
				// Lab gate: hold between the terminal write and the suppression update, until the test
				// creates <state>/gate.release. A test can commit a newer send meanwhile.
				h.ev.emit("fault", map[string]any{"point": f.point, "mode": f.mode, "agent": n.agent, "intent_rev": revString(n.intentRev)})
				release := filepath.Join(h.cfg.State, "gate.release")
				for {
					if _, err := os.Stat(release); err == nil {
						os.Remove(release)
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
			h.mu.Lock()
			// Only the revision captured before the write counts, and only for the same connection.
			if a.ClientID == conn && n.intentRev > a.delivered[n.ns] {
				a.delivered[n.ns] = n.intentRev
			}
			h.mu.Unlock()
			h.ev.emit("notice_written", map[string]any{"agent": n.agent, "plugin": n.ns, "intent_rev": revString(n.intentRev)})
		}
		if !next.IsZero() {
			retry = time.After(time.Until(next))
		}
	}
}

// writeNotice writes a bracketed paste, then a separate CR (docs/agent-wake.md).
func (h *Hub) writeNotice(ctx context.Context, sess, blockID, text string) error {
	write := func(data string) error {
		return h.rex.BlockCall(ctx, "com.superlogical.terminal.write", sess, blockID, map[string]any{"data": data}, nil)
	}
	if err := write("\x1b[200~" + strings.ReplaceAll(text, "\n", " ") + "\x1b[201~"); err != nil {
		return err
	}
	return write("\r")
}

// statusSnap is what noticeLoop knew about an agent before a status read.
type statusSnap struct {
	a                   *Agent
	sess, block, record string
	pid, gen            int
}

// noticeStatus reads the program status of an agent whose state is unknown while a notice waits
// for it. The read runs outside h.mu. Its result applies only if, after the read, the identity
// still names the same Agent with the same pane, process, status record and state generation: a
// status event or a rebind during the read wins over the older result. It reports whether the
// agent now has a known state; false makes noticeLoop retry with backoff (a failed read, a missing
// record, or a state that changed during the read and is still unknown).
func (h *Hub) noticeStatus(ctx context.Context, id string, s statusSnap) bool {
	rec, err := h.readStatus(ctx, s.sess, s.block, s.record)
	if f := h.statusReadFault(s.sess, "hold"); f != nil {
		h.waitRelease(f)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.noticeTarget(id)
	if cur != s.a || cur.BlockID != s.block || cur.ChildPID != s.pid || cur.StatusRecord != s.record || cur.stateGen != s.gen {
		h.ev.emit("agent_status_read", map[string]any{"agent": id, "applied": false})
		return cur != nil && cur.State != ""
	}
	if err != nil {
		log.Printf("notice for %s: status of block %s (record %q): %v; retrying", id, s.block, s.record, err)
		h.ev.emit("agent_status_read", map[string]any{"agent": id, "applied": false, "error": err.Error()})
		return false
	}
	cur.setRecord(rec)
	h.syncPrompts()
	h.ev.emit("agent_status_read", map[string]any{"agent": id, "applied": true, "state": rec.State})
	if rec.State == "" {
		return false
	}
	// The home shows agent readiness. Only a known state wakes noticeLoop again, so an unknown one
	// cannot make it read in a loop. go: h.mu is held.
	go h.changed("agents")
	return true
}
