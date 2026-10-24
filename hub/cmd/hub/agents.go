package main

// Agent enrollment, readiness and notices (README sections 2 and 5).

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"
)

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
	// lastExit is the block's last child exit, in the form of its child_exited payload; null or
	// empty while no child has exited (lab, Rex 0.0.1792593189-g3f4a0a).
	lastExit json.RawMessage
}

// childProcIn reads a block's child process.
func (h *Hub) childProcIn(ctx context.Context, sess, blockID string) (paneChild, error) {
	var out struct {
		Child *struct {
			PID     int    `json:"pid"`
			Path    string `json:"path"`
			Invoked string `json:"invoked_path"`
		} `json:"child"`
		LastExit json.RawMessage `json:"last_exit"`
	}
	if err := h.rex.BlockCall(ctx, "com.superlogical.terminal.process", sess, blockID, nil, &out); err != nil {
		return paneChild{}, err
	}
	if out.Child == nil {
		return paneChild{lastExit: out.LastExit}, nil
	}
	return paneChild{out.Child.PID, out.Child.Path, out.Child.Invoked, out.LastExit}, nil
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

// recordState is readStatus for an identity sync: a failed read is logged and gives
// the unknown state "". noticeLoop reads the state again when a notice waits (noticeStatus).
func (h *Hub) recordState(ctx context.Context, sess, blockID, record string) statusRecord {
	s, err := h.readStatus(ctx, sess, blockID, record)
	if err != nil {
		log.Printf("status of block %s (record %q): %v", blockID, record, err)
	}
	return s
}

// kickNotices wakes noticeLoop. Call it when a notice can become due: a store commit with a pending
// notice, an agent status change (Rex program_status events), an identity change, or a plugin
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
// noticeRenudgeAfter: a written nudge with no read and no new revision for this time is written
// once more (the coordinator, 2026-10-22).
const noticeRenudgeAfter = 30 * time.Minute

func (h *Hub) noticeLoop(ctx context.Context) {
	var retry <-chan time.Time
	// tries: agents whose status read gave no state, with the time of the next read (backoff from
	// 5 s to 1 min). A kick before that time does not read again.
	type try struct {
		next time.Time
		wait time.Duration
	}
	tries := map[string]*try{}
	// deliveries: this hub run's record of each notice row's last delivery (ns, agent), the same as
	// its stored row. It stands when a store write of a delivery failed.
	deliveries := map[[2]string]noticeDelivery{}
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
		renudge := time.Duration(h.renudgeAfter.Load())
		if renudge == 0 {
			renudge = noticeRenudgeAfter
		}
		for _, n := range pending {
			key := [2]string{n.ns, n.agent}
			d, ok := deliveries[key]
			n = n.withDelivery(d, ok)
			h.mu.Lock()
			a, pl := h.noticeTarget(n.agent), h.plugins[n.ns]
			// wants reports whether a nudge is due now (one repeat after renudge), and arms the wake
			// of a later repeat. A kept record gets no notices.
			repeat := false
			wants := func() bool {
				if a == nil || pl == nil || pl.entry == nil || pl.run.Notice == "" {
					return false
				}
				due, rep, wake := noticeDue(n, noticeConv{a.BlockID, a.ChildPID, a.ChildStart}, a.delivered[n.ns], time.Now(), renudge)
				if !wake.IsZero() {
					arm(time.Until(wake))
				}
				repeat = rep
				return due
			}
			waiting := wants()
			if a != nil && a.State != "" {
				delete(tries, n.agent) // a known state ends the backoff
			}
			if waiting && a.live() && a.State == "" {
				// The state is unknown: a read at start may have failed, and no status event came
				// since. Read it now. Unknown is never ready: a notice ends with CR, and a blocked
				// prompt could take it as an answer.
				snap := statusSnap{a: a, sess: a.Session, block: a.BlockID, pid: a.ChildPID, record: a.StatusRecord, gen: a.stateGen}
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
				waiting = wants()
			}
			due := waiting && a.ready()
			sess, block, conn, notice := "", "", "", ""
			var conv noticeConv
			if due {
				sess, block, conn, notice = a.Session, a.BlockID, a.ClientID, pl.run.Notice // run changes at a spawn
				conv = noticeConv{a.BlockID, a.ChildPID, a.ChildStart}
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
			h.mu.Lock()
			// Only the revision captured before the write counts, and only for the same connection.
			if a.ClientID == conn && n.intentRev > a.delivered[n.ns] {
				a.delivered[n.ns] = n.intentRev
			}
			h.mu.Unlock()
			// The stored delivery outlives a hub restart and a reconnect; it names the conversation
			// captured before the write. A failed store write leaves only the memory record: the
			// nudge can come once more after a restart, never less often.
			now := time.Now()
			if repeat {
				d := deliveries[key]
				d.rev, d.at, d.conv, d.repeated = n.intentRev, n.delivAt, n.delivConv, true
				deliveries[key] = d
			} else {
				deliveries[key] = noticeDelivery{rev: n.intentRev, at: now.UnixMilli(), conv: conv}
			}
			if err := h.db.recordNotice(ctx, n, conv, now, repeat); err != nil {
				log.Printf("notice to %s: record: %v", n.agent, err) // the memory record stands
			}
			if !repeat {
				arm(renudge) // the wake of the one repeat; no other event may come (Codex review of 8169791)
			}
			fields := map[string]any{"agent": n.agent, "plugin": n.ns, "intent_rev": revString(n.intentRev)}
			if repeat {
				fields["repeat"] = true
			}
			h.ev.emit("notice_written", fields)
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
