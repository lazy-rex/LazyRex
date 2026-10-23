package main

// Agent identity: an agent is its conversation. The hub records which conversation runs in which
// pane, and an append-only lifecycle log. Messages address identities; an agent spawn's address
// "spawn:<spawn_id>" reaches the first identity that its pane reported (spawns.identity).
//
// An identity is "<kind>:<conversation id>". Reports come from agent hooks (SessionStart,
// UserPromptSubmit; sources hookSources). Ordering: reports are ordered by the start time of the
// reporting process: each hook run is a new process. A report from a process that started before
// the reporter of the pane's current identity is stale. This is hook launch order; it equals event
// order only if the agent CLI awaits each hook before the next event (unverified for Claude
// 2.1.302 and Codex 0.163.0). All reports run one at a time (identMu).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const identitySchema = `
-- One row per pane with an agent conversation. child_pid/child_start is the pane's process
-- generation. kind is the kind of the current conversation. reporter_start orders hook reports.
-- child_path/child_invoked (added in workers.go): the generation's executable and exec path, the first
-- complete values that the Rex answer of an accepted report gave (agentrestart.go); cleared with the
-- generation. cli_pid/cli_start (added in workers.go): the agent CLI that the current conversation
-- follows when it is not the pane child (agentProcess); 0 and '': the pane child.
CREATE TABLE IF NOT EXISTS identity_panes (block_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, child_pid INTEGER NOT NULL,
  child_start TEXT NOT NULL, kind TEXT NOT NULL DEFAULT '', current TEXT NOT NULL DEFAULT '', reporter_start TEXT NOT NULL DEFAULT '');
-- state: current (runs in block_id), superseded (replaced in its pane by another conversation),
-- detached (its process exited or its pane closed).
CREATE TABLE IF NOT EXISTS identities (identity TEXT PRIMARY KEY, kind TEXT NOT NULL, conversation_id TEXT NOT NULL,
  state TEXT NOT NULL, block_id TEXT NOT NULL, predecessor TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
  created TEXT NOT NULL, updated TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS identity_log (seq INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, identity TEXT NOT NULL,
  block_id TEXT NOT NULL, event TEXT NOT NULL, detail TEXT NOT NULL);
-- hub.workers reads identity_seen of each agent worker from the log rows of its block (paneSeen).
CREATE INDEX IF NOT EXISTS identity_log_block ON identity_log (block_id);
`

var conversationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// agentEnvUnset are conversation, nesting and per-process variables that an agent pane must not
// inherit from another agent (a pane inherits the Rex server's environment; a server started from
// an agent's shell carries that agent's variables, observed in the lab on Rex 1203). With
// CLAUDE_CODE_CHILD_SESSION set, Claude ignores --session-id (lab, 2.1.302). Deliberate
// configuration (for example CLAUDE_EFFORT, CODEX_HOME) stays.
var agentEnvUnset = []string{"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT",
	"CLAUDE_PID", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
	"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_TOOL_CALL_ID"}

// startBefore compares two process start times ("sec.usec", see procInfo).
func startBefore(a, b string) bool {
	as, au, _ := strings.Cut(a, ".")
	bs, bu, _ := strings.Cut(b, ".")
	ai, _ := strconv.ParseInt(as, 10, 64)
	bi, _ := strconv.ParseInt(bs, 10, 64)
	if ai != bi {
		return ai < bi
	}
	ax, _ := strconv.ParseInt(au, 10, 64)
	bx, _ := strconv.ParseInt(bu, 10, 64)
	return ax < bx
}

type identityPane struct {
	block, session, childStart, kind, current, reporterStart string
	childPath, childInvoked, cliStart                        string
	childPID, cliPID                                         int
}

func (h *Hub) identityLog(ctx context.Context, q execer, id, block, event string, detail map[string]any) error {
	d, _ := json.Marshal(detail)
	_, err := q.ExecContext(ctx, `INSERT INTO identity_log (at, identity, block_id, event, detail) VALUES (?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano), id, block, event, string(d))
	if err == nil {
		h.ev.emit("identity_"+event, map[string]any{"identity": id, "block_id": block})
	}
	return err
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// identityReport handles hub.identity.report {kind, conversation_id, source, block?}.
func (h *Hub) identityReport(ctx context.Context, p perform, args map[string]any) (any, error) {
	kind, _ := args["kind"].(string)
	conv, _ := args["conversation_id"].(string)
	source, _ := args["source"].(string)
	hint, _ := args["block"].(string)
	if kind != "claude" && kind != "codex" {
		return nil, bad("invalid: kind must be claude or codex")
	}
	if !conversationID.MatchString(conv) {
		return nil, bad("invalid: conversation_id must be 1-128 characters: letters, digits, '.', '_', '-'")
	}
	if !hookSources[source] {
		return nil, bad("invalid: source must be one of startup, resume, clear, compact, fork, prompt")
	}
	reporter := p.Requester.Principal.PID
	rstart := startTime(reporter)
	if rstart == "" {
		return nil, bad("not_in_pane: caller pid %d is gone", reporter)
	}
	sess, block, proc, err := h.paneOfHint(ctx, reporter, hint)
	child := proc.pid
	if err != nil {
		return nil, err
	}
	if block == "" {
		return nil, bad("not_in_pane: caller pid %d is not in a Rex pane", reporter)
	}
	cstart := startTime(child)
	cli, cliStart := agentProcess(h.procReader(), reporter, child)
	// Before identMu and the transaction: the adoption calls Rex.
	spawnAgent := h.adoptReported(ctx, sess, block)
	name, nameErr := h.paneName(ctx, sess, block)
	id := kind + ":" + conv

	h.identMu.Lock()
	defer h.identMu.Unlock()
	// The address of the agent spawn that is open in this pane ("spawn:<id>"), or "".
	agentID := spawnAgent
	result := func(state, pred string) map[string]any {
		r := map[string]any{"identity": id, "state": state, "predecessor": nil, "name": name,
			"pane": map[string]any{"session_id": sess, "block_id": block}, "agent_id": nil}
		if pred != "" {
			r["predecessor"] = pred
		}
		if agentID != "" {
			r["agent_id"] = agentID
		}
		return r
	}
	detail := map[string]any{"source": source, "session_id": sess, "agent_id": agentID}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var row identityPane
	err = tx.QueryRowContext(ctx, `SELECT block_id, session_id, child_pid, child_start, kind, current, reporter_start, child_path, child_invoked, cli_pid, cli_start FROM identity_panes WHERE block_id = ?`, block).
		Scan(&row.block, &row.session, &row.childPID, &row.childStart, &row.kind, &row.current, &row.reporterStart, &row.childPath, &row.childInvoked, &row.cliPID, &row.cliStart)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		row = identityPane{block: block}
	case err != nil:
		return nil, err
	case row.childPID != 0 && (row.childPID != child || row.childStart != cstart):
		// The pane runs a new process: the previous conversation left with the old one.
		if row.current != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE identities SET state = 'detached', updated = ? WHERE identity = ? AND state = 'current' AND block_id = ?`,
				time.Now().UTC().Format(time.RFC3339Nano), row.current, block); err != nil {
				return nil, err
			}
			if err := h.identityLog(ctx, tx, row.current, block, "exited", map[string]any{"reason": "pane process replaced"}); err != nil {
				return nil, err
			}
		}
		row.current, row.reporterStart, row.kind = "", "", ""
	}
	stale := func(why string) (any, error) {
		tx.Rollback()
		h.identityLog(ctx, h.db, id, block, "stale_report", map[string]any{"source": source, "reason": why, "current": row.current})
		return nil, bad("stale: %s", why)
	}
	if row.reporterStart != "" && startBefore(rstart, row.reporterStart) {
		return stale("an older hook process than the one that reported " + row.current)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rs := rstart
	// The generation's exec facts come only from this report's Rex answer (the same child pid). The
	// first complete values stay for the generation: a later answer has a null exec path after an update.
	path, invoked := "", ""
	if row.childPID == child && row.childStart == cstart {
		path, invoked = row.childPath, row.childInvoked
	}
	if (path == "" || invoked == "") && cstart != "" && proc.path != "" && proc.invoked != "" {
		path, invoked = proc.path, proc.invoked
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current, reporter_start, child_path, child_invoked, cli_pid, cli_start) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (block_id) DO UPDATE SET session_id = excluded.session_id, child_pid = excluded.child_pid, child_start = excluded.child_start,
		kind = excluded.kind, current = excluded.current, reporter_start = excluded.reporter_start, child_path = excluded.child_path, child_invoked = excluded.child_invoked,
		cli_pid = excluded.cli_pid, cli_start = excluded.cli_start`,
		block, sess, child, cstart, kind, id, rs, path, invoked, cli, cliStart); err != nil {
		return nil, err
	}
	var state, pred, oldName string
	err = tx.QueryRowContext(ctx, `SELECT state, block_id, predecessor, name FROM identities WHERE identity = ?`, id).Scan(&state, new(string), &pred, &oldName)
	known := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if nameErr != nil && known {
		name = oldName // the name is unknown now: keep the recorded one
	}
	if id == row.current {
		if _, err := tx.ExecContext(ctx, `UPDATE identities SET name = ?, updated = ? WHERE identity = ?`, name, now, id); err != nil {
			return nil, err
		}
		if source == "compact" || source == "resume" {
			if err := h.identityLog(ctx, tx, id, block, map[string]string{"compact": "compacted", "resume": "resumed"}[source], detail); err != nil {
				return nil, err
			}
		}
		if known && oldName != name {
			if err := h.identityLog(ctx, tx, id, block, "renamed", map[string]any{"from": oldName, "to": name}); err != nil {
				return nil, err
			}
		}
		// A spawn opened after the pane's first report (an adoption on a later report) binds now.
		bound, err := h.bindSpawn(ctx, tx, agentID, id, block)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		// A new CLI process for the same conversation (a resume in the same shell) changes what the
		// conversation follows.
		if bound || known && oldName != name || row.cliPID != cli || row.cliStart != cliStart {
			h.identityChanged(ctx)
		}
		return result("unchanged", pred), nil
	}
	// A transition: id becomes the pane's current conversation.
	if row.current != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE identities SET state = 'superseded', updated = ? WHERE identity = ?`, now, row.current); err != nil {
			return nil, err
		}
	}
	if known && state == "current" {
		// Resumed in another pane: the old pane no longer runs it.
		if _, err := tx.ExecContext(ctx, `UPDATE identity_panes SET current = '' WHERE current = ? AND block_id <> ?`, id, block); err != nil {
			return nil, err
		}
	}
	event := "resumed"
	if !known {
		pred = row.current
		event = map[string]string{"clear": "cleared", "fork": "forked"}[source]
		if event == "" {
			event = "started"
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO identities VALUES (?, ?, ?, 'current', ?, ?, ?, ?, ?)
		ON CONFLICT (identity) DO UPDATE SET state = 'current', block_id = excluded.block_id, name = excluded.name, updated = excluded.updated`,
		id, kind, conv, block, pred, name, now, now); err != nil {
		return nil, err
	}
	detail["previous"] = row.current
	if err := h.identityLog(ctx, tx, id, block, event, detail); err != nil {
		return nil, err
	}
	if _, err := h.bindSpawn(ctx, tx, agentID, id, block); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	h.identityChanged(ctx)
	log.Printf("identity %s %s in %s (source %s, previous %q)", id, event, block, source, row.current)
	return result("current", pred), nil
}

// agentProcess is the agent CLI that a pane's conversation follows when it is not the pane child
// (lead-irgap4): the leader of the pane terminal's foreground process group, when the reporter runs
// under it and it runs under the pane child (a CLI that an interactive shell started). 0 and "": the
// conversation follows the pane child. A CLI under a shell that stays sends no Rex exit event, so
// identityLoop checks that process. A CLI under a non-interactive wrapper (sh -c without exec) is
// in the wrapper's group: it follows the pane child.
func agentProcess(read func(int) (procRecord, error), reporter, child int) (int, string) {
	r, err := read(child)
	if err != nil || r.tpgid <= 0 || r.tpgid == child {
		return 0, ""
	}
	if up, _ := ancestryOf(read, reporter); !slices.Contains(up, r.tpgid) {
		return 0, ""
	}
	if up, _ := ancestryOf(read, r.tpgid); !slices.Contains(up, child) {
		return 0, ""
	}
	lead, err := read(r.tpgid)
	if err != nil || lead.zombie {
		return 0, ""
	}
	return r.tpgid, lead.start
}

// procReader is h.procRead, or procRead when no test set one.
func (h *Hub) procReader() func(int) (procRecord, error) {
	if h.procRead != nil {
		return h.procRead
	}
	return procRead
}

// spawnIdentity is the identity bound to the spawn address id, or id itself: another ID, or a
// spawn that has not reported yet.
func (h *Hub) spawnIdentity(ctx context.Context, id string) string {
	if spawnID, ok := strings.CutPrefix(id, "spawn:"); ok {
		var bound string
		if h.db.QueryRowContext(ctx, `SELECT identity FROM spawns WHERE spawn_id = ? AND agent_id = ?`, spawnID, id).Scan(&bound) == nil && bound != "" {
			return bound
		}
	}
	return id
}

// spawnAddress is the address of an agent spawn: a message or notice to it goes to the first
// identity that its pane reported.
func spawnAddress(spawnID string) string { return "spawn:" + spawnID }

// bindSpawn binds the agent spawn addr ("spawn:<id>", open in block) to id if it has no identity
// yet: the first identity that the spawn's pane reports stays its identity, also after a /clear and
// after the spawn closes (lead-d-r1, lead-d-r2). It reports whether it bound.
func (h *Hub) bindSpawn(ctx context.Context, tx *sql.Tx, addr, id, block string) (bool, error) {
	spawnID, ok := strings.CutPrefix(addr, "spawn:")
	if !ok {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE spawns SET identity = ? WHERE spawn_id = ? AND agent_id = ? AND identity = ''`, id, spawnID, addr)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	return true, h.identityLog(ctx, tx, id, block, "bound", map[string]any{"spawn_id": spawnID})
}

// paneOfHint finds the caller's pane, the hinted block first. The hint is not trusted: the pid
// ancestry decides.
func (h *Hub) paneOfHint(ctx context.Context, pid int, hint string) (sess, block string, child paneChild, err error) {
	if hint != "" {
		if s, err := h.findBlockSession(ctx, hint); err == nil && s != "" {
			if c, err := h.childProcIn(ctx, s, hint); err == nil && c.pid > 0 && descendsFrom(pid, c.pid) {
				return s, hint, c, nil
			}
		}
	}
	return h.paneChildOf(ctx, pid)
}

// paneName is the agent's display name: the tab (window) label, or the pane label when the tab
// has several panes and the pane has a label. A tab the user never named has Rex's generated
// label, "Window " + the last 8 characters of the window ID (observed on Rex 1203). The app shows
// the terminal title in such a tab, so the name is that title (the maintainer, 2026-10-17). Without a
// title the name is "", and readers show the identity. An error means the name is unknown: the
// callers keep the previous name, so a timeout never renames an agent to its ID (Codex review).
func (h *Hub) paneName(ctx context.Context, sess, block string) (string, error) {
	var sv struct {
		Windows []struct {
			ID     string `json:"window_id"`
			Label  string `json:"label"`
			Layers []struct {
				Blocks []struct {
					BlockID string `json:"block_id"`
				} `json:"blocks"`
			} `json:"layers"`
		} `json:"windows"`
	}
	if err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": sess}, &sv); err != nil {
		return "", err
	}
	for _, w := range sv.Windows {
		n, found := 0, false
		for _, l := range w.Layers {
			for _, b := range l.Blocks {
				n++
				found = found || b.BlockID == block
			}
		}
		if !found {
			continue
		}
		if n > 1 {
			blocks, err := h.blocksIn(ctx, sess)
			if err != nil {
				return "", err
			}
			if blocks[block].Label != "" {
				return blocks[block].Label, nil
			}
		}
		if len(w.ID) >= 8 && w.Label == "Window "+w.ID[len(w.ID)-8:] {
			return h.paneTitle(ctx, sess, block)
		}
		return w.Label, nil
	}
	return "", fmt.Errorf("block %s is not shown in session %s", block, sess)
}

// paneTitle returns the terminal title of block without leading symbols: agents such as Claude
// Code put an animated status glyph before the title, and the name must not change with it.
func (h *Hub) paneTitle(ctx context.Context, sess, block string) (string, error) {
	var out struct {
		Title string `json:"title"`
	}
	if err := h.rex.BlockCall(ctx, "com.superlogical.terminal.title", sess, block, nil, &out); err != nil {
		return "", err
	}
	return cleanTitle(out.Title), nil
}

// cleanTitle removes leading status glyphs and spaces: arrows, symbols, dingbats, Braille
// spinners (U+2190 to U+2BFF) and emoji (U+1F300 to U+1FAFF). Other characters stay, so a title
// such as ".env" or "~/project" keeps its first character.
func cleanTitle(t string) string {
	return strings.TrimSpace(strings.TrimLeftFunc(t, func(r rune) bool {
		return unicode.IsSpace(r) || (r >= 0x2190 && r <= 0x2BFF) || (r >= 0x1F300 && r <= 0x1FAFF)
	}))
}

// identityRenamesSoon runs identityRenames for sess 1 s after a title change: a status glyph can
// change the title several times a second. One refresh per session is scheduled or running at a
// time. A change during a run marks the session dirty, and one more run follows (Codex review).
func (h *Hub) identityRenamesSoon(ctx context.Context, sess string) {
	h.mu.Lock()
	if h.titleRenames == nil {
		h.titleRenames = map[string]bool{}
	}
	if _, busy := h.titleRenames[sess]; busy {
		h.titleRenames[sess] = true // dirty: run once more after the current run
		h.mu.Unlock()
		return
	}
	h.titleRenames[sess] = false
	h.mu.Unlock()
	go func() {
		for {
			time.Sleep(time.Second)
			h.mu.Lock()
			h.titleRenames[sess] = false
			h.mu.Unlock()
			h.identityRenames(ctx, sess)
			h.mu.Lock()
			if !h.titleRenames[sess] {
				delete(h.titleRenames, sess)
				h.mu.Unlock()
				return
			}
			h.mu.Unlock()
		}
	}()
}

// identityRenames updates the names of the current identities in a session after a label change.
func (h *Hub) identityRenames(ctx context.Context, sess string) {
	h.identMu.Lock()
	defer h.identMu.Unlock()
	rows, err := h.db.QueryContext(ctx, `SELECT i.identity, i.block_id, i.name FROM identities i JOIN identity_panes p ON p.current = i.identity WHERE p.session_id = ?`, sess)
	if err != nil {
		log.Printf("identity renames: %v", err)
		return
	}
	type cur struct{ id, block, name string }
	var list []cur
	for rows.Next() {
		var c cur
		if rows.Scan(&c.id, &c.block, &c.name) == nil {
			list = append(list, c)
		}
	}
	rows.Close()
	for _, c := range list {
		name, err := h.paneName(ctx, sess, c.block)
		if err != nil {
			log.Printf("identity name %s: %v", c.id, err) // unknown: keep the name
			continue
		}
		if name == c.name {
			continue
		}
		h.db.ExecContext(ctx, `UPDATE identities SET name = ?, updated = ? WHERE identity = ?`, name, time.Now().UTC().Format(time.RFC3339Nano), c.id)
		h.identityLog(ctx, h.db, c.id, c.block, "renamed", map[string]any{"from": c.name, "to": name})
		h.identityChanged(ctx)
	}
}

// identityChildExited ends the pane's current identity when the pane's process exits (a kept-open
// pane). The event is checked against the recorded generation: an event for an older process does
// not end the identity of a newer one.
func (h *Hub) identityChildExited(ctx context.Context, block string) {
	h.identMu.Lock()
	defer h.identMu.Unlock()
	var cur, start string
	var pid int
	if h.db.QueryRowContext(ctx, `SELECT current, child_pid, child_start FROM identity_panes WHERE block_id = ?`, block).Scan(&cur, &pid, &start) != nil {
		return
	}
	if pid != 0 && alive(pid, start) {
		return // the recorded process still runs: the event is for another one
	}
	h.identityEndLocked(ctx, block, cur, "pane process exited")
}

// identityEndLocked ends the pane's process generation: it clears the pane's current identity and
// detaches it (log event exited, with reason). The caller holds identMu and has decided that the
// process ended.
func (h *Hub) identityEndLocked(ctx context.Context, block, cur, reason string) {
	h.db.ExecContext(ctx, `UPDATE identity_panes SET current = '', child_pid = 0, child_start = '', reporter_start = '', child_path = '', child_invoked = '',
		cli_pid = 0, cli_start = '' WHERE block_id = ?`, block)
	if cur != "" {
		h.db.ExecContext(ctx, `UPDATE identities SET state = 'detached', updated = ? WHERE identity = ? AND block_id = ?`,
			time.Now().UTC().Format(time.RFC3339Nano), cur, block)
		h.identityLog(ctx, h.db, cur, block, "exited", map[string]any{"reason": reason})
	}
	h.identityChanged(ctx)
}

// identityLoopEvery is the period of identityLoop's check.
const identityLoopEvery = 2 * time.Second

// identityLoop checks, every identityLoopEvery, each current conversation that follows a process
// other than its pane child (agentProcess): its exit sends no Rex event, because the pane child
// (the shell) stays. A read (hub.workers, hub.agents) never writes; this loop does (lead-irgap4).
func (h *Hub) identityLoop(ctx context.Context) {
	t := time.NewTicker(identityLoopEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := h.identityCheck(ctx, false); err != nil {
			log.Printf("identity check: %v", err)
		}
	}
}

// identityRecover ends the current identity of each pane whose recorded process is gone. An exit
// while the hub was down sent the hub no event. Called at hub start, after identitySync.
func (h *Hub) identityRecover(ctx context.Context) error { return h.identityCheck(ctx, true) }

// identityCheck ends the current identity of each pane whose followed process is gone: the agent
// CLI (cli_pid, agentProcess), or the pane child when the row has no CLI. all: every current pane
// (the start); else only the panes that follow a CLI (identityLoop; a pane child's exit has its Rex
// event). Only a verified end clears: the system has no such pid, the pid has another start time,
// or the process has exited and is not reaped (a zombie). A failed process read, or a pane without
// a recorded process, keeps the identity; the start logs the reason (lead-c43rec). The pids are
// local: a report comes only from a pane on this host (paneOfHint, descendsFrom).
func (h *Hub) identityCheck(ctx context.Context, all bool) error {
	q := `SELECT block_id, cli_pid, cli_start, child_pid, child_start FROM identity_panes WHERE current != ''`
	if !all {
		q += ` AND cli_pid > 0`
	}
	rows, err := h.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	type pane struct {
		block, start string
		pid          int
		cli          bool
	}
	var panes []pane
	for rows.Next() {
		var p pane
		var cliStart, childStart string
		var cliPID, childPID int
		if err := rows.Scan(&p.block, &cliPID, &cliStart, &childPID, &childStart); err != nil {
			rows.Close()
			return err
		}
		p.pid, p.start, p.cli = childPID, childStart, cliPID > 0
		if p.cli {
			p.pid, p.start = cliPID, cliStart
		}
		panes = append(panes, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	read := h.procReader()
	for _, p := range panes {
		if p.pid <= 0 || p.start == "" {
			log.Printf("identity recover: pane %s: no recorded process", p.block)
			continue
		}
		r, err := read(p.pid)
		switch {
		case errors.Is(err, errProcGone):
		case err != nil:
			if all {
				log.Printf("identity recover: pane %s: %v", p.block, err)
			}
			continue
		case r.start == p.start && !r.zombie:
			continue // the recorded process still runs
		}
		reason, col := "pane process exited", "child"
		if p.cli {
			reason, col = "agent process exited", "cli"
		}
		h.identMu.Lock()
		var cur, start string
		var pid int
		// The pane's row can change between the list and here (a report): end only the checked process.
		if h.db.QueryRowContext(ctx, `SELECT current, `+col+`_pid, `+col+`_start FROM identity_panes WHERE block_id = ?`, p.block).Scan(&cur, &pid, &start) == nil &&
			cur != "" && pid == p.pid && start == p.start {
			h.identityEndLocked(ctx, p.block, cur, reason)
		}
		h.identMu.Unlock()
	}
	return nil
}

// identityPaneClosed detaches the pane's current identity. Its record stays for a later resume.
func (h *Hub) identityPaneClosed(ctx context.Context, block string) {
	h.identMu.Lock()
	defer h.identMu.Unlock()
	var cur string
	if h.db.QueryRowContext(ctx, `SELECT current FROM identity_panes WHERE block_id = ?`, block).Scan(&cur) != nil {
		return
	}
	h.db.ExecContext(ctx, `DELETE FROM identity_panes WHERE block_id = ?`, block)
	if cur != "" {
		h.db.ExecContext(ctx, `UPDATE identities SET state = 'detached', updated = ? WHERE identity = ? AND block_id = ?`,
			time.Now().UTC().Format(time.RFC3339Nano), cur, block)
		h.identityLog(ctx, h.db, cur, block, "pane_closed", map[string]any{})
	}
	h.identityChanged(ctx)
}

// identityList handles hub.identity.list {log?: n}: every identity, and the last n log entries.
func (h *Hub) identityList(ctx context.Context, args map[string]any) (any, error) {
	// The address of the agent spawn open in each pane. Read first: the store has one connection.
	agentOf := map[string]string{}
	srows, err := h.db.QueryContext(ctx, `SELECT block_id, agent_id FROM spawns WHERE status = 'open' AND agent_id LIKE 'spawn:%'`)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var block, addr string
		if err := srows.Scan(&block, &addr); err != nil {
			srows.Close()
			return nil, err
		}
		agentOf[block] = addr
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}
	rows, err := h.db.QueryContext(ctx, `SELECT identity, kind, conversation_id, state, block_id, predecessor, name, created, updated FROM identities ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []any{}
	for rows.Next() {
		var id, kind, conv, state, block, pred, name, created, updated string
		if err := rows.Scan(&id, &kind, &conv, &state, &block, &pred, &name, &created, &updated); err != nil {
			return nil, err
		}
		e := map[string]any{"identity": id, "kind": kind, "conversation_id": conv, "state": state, "block_id": block,
			"name": name, "created": created, "updated": updated}
		if pred != "" {
			e["predecessor"] = pred
		}
		if state == "current" && agentOf[block] != "" {
			e["agent_id"] = agentOf[block]
		}
		list = append(list, e)
	}
	out := map[string]any{"identities": list}
	if n, _ := args["log"].(float64); n > 0 {
		lrows, err := h.db.QueryContext(ctx, `SELECT seq, at, identity, block_id, event, detail FROM identity_log ORDER BY seq DESC LIMIT ?`, int(n))
		if err != nil {
			return nil, err
		}
		defer lrows.Close()
		entries := []any{}
		for lrows.Next() {
			var seq int64
			var at, id, block, event, detail string
			if err := lrows.Scan(&seq, &at, &id, &block, &event, &detail); err != nil {
				return nil, err
			}
			entries = append(entries, map[string]any{"seq": seq, "at": at, "identity": id, "block_id": block, "event": event, "detail": json.RawMessage(detail)})
		}
		out["log"] = entries
	}
	return out, nil
}

// identityChanged mirrors the store into memory, then wakes the notices (a new identity can have a
// pending notice) and the views that list agents or workers (a worker shows its pane's identity).
// Call it after a commit, never inside a transaction.
func (h *Hub) identityChanged(ctx context.Context) {
	if err := h.identitySync(ctx); err != nil {
		log.Printf("identity sync: %v", err)
	}
	h.kickNotices()
	h.changed("agents", "workers")
}

// identitySyncGap runs in identitySync while h.mu is free between the copy and the install. A
// test replaces it.
var identitySyncGap = func() {}

// identitySync rebuilds h.idents and h.spawnAddrs from the store. A current identity is bound to
// its pane's process generation; its program status is read once when its pane changes.
func (h *Hub) identitySync(ctx context.Context) error {
	rows, err := h.db.QueryContext(ctx, `SELECT i.identity, i.kind, i.state, i.name, COALESCE(p.block_id, ''), COALESCE(p.session_id, ''),
		CASE WHEN p.cli_pid > 0 THEN p.cli_pid ELSE COALESCE(p.child_pid, 0) END, CASE WHEN p.cli_pid > 0 THEN p.cli_start ELSE COALESCE(p.child_start, '') END
		FROM identities i LEFT JOIN identity_panes p ON p.current = i.identity`)
	if err != nil {
		return err
	}
	next := map[string]*Agent{}
	for rows.Next() {
		a := &Agent{delivered: map[string]int64{}}
		if err := rows.Scan(&a.ID, &a.Kind, &a.IdState, &a.Name, &a.BlockID, &a.Session, &a.ChildPID, &a.ChildStart); err != nil {
			rows.Close()
			return err
		}
		next[a.ID] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := h.spawnAddrsLoad(ctx); err != nil {
		return err
	}
	// Keep what memory knows for an unchanged binding: status, delivery suppression, connection.
	var fresh []*Agent
	h.mu.Lock()
	kept := map[string]*Agent{}
	for id, a := range next {
		if old := h.idents[id]; old != nil && old.BlockID == a.BlockID && old.ChildPID == a.ChildPID {
			a.delivered, a.ClientID = old.delivered, old.ClientID
			kept[id] = old
			continue
		}
		if a.live() {
			fresh = append(fresh, a)
		}
	}
	h.mu.Unlock()
	identitySyncGap()
	for _, a := range fresh {
		a.setRecord(h.recordState(ctx, a.Session, a.BlockID, a.StatusRecord))
	}
	h.mu.Lock()
	// The status of a kept binding is copied under this lock: a program status event can write
	// the old record while h.mu is free for the reads above.
	for id, old := range kept {
		next[id].State, next[id].Record = old.State, old.Record
	}
	h.idents = next
	h.syncPrompts()
	h.mu.Unlock()
	return nil
}

// spawnAddr is an agent spawn's address "spawn:<spawn_id>" as the hub knows it: the spawn's
// title, the first identity that its pane reported (or ""), and whether the spawn ended (closed or
// failed). An address that never bound and ended takes no message or notice (lead-d-r1).
type spawnAddr struct {
	spawnID, name, identity string
	ended                   bool
}

// spawnAddrsLoadGap runs in spawnAddrsLoad between the store read and the install. A test
// replaces it.
var spawnAddrsLoadGap = func() {}

// spawnAddrsLoad reads every agent spawn address from the store. It logs each address that ended
// before its first report since the last load, and then hints "agents": a plugin that holds items
// for that address ends them.
//
// The read runs without h.mu. A change of h.spawnAddrs after the read began (an admission, or
// another load) makes the read stale: the load reads again, so an older read never replaces a
// newer state (codex-24's review).
func (h *Hub) spawnAddrsLoad(ctx context.Context) error {
	var gone []string
	for {
		h.mu.Lock()
		rev := h.spawnAddrRev
		h.mu.Unlock()
		next, err := h.spawnAddrsRead(ctx)
		if err != nil {
			return err
		}
		spawnAddrsLoadGap()
		h.mu.Lock()
		if h.spawnAddrRev != rev {
			h.mu.Unlock()
			continue
		}
		for addr, sa := range next {
			if old := h.spawnAddrs[addr]; sa.identity == "" && sa.ended && old != nil && !old.ended {
				gone = append(gone, addr)
			}
		}
		h.spawnAddrs = next
		h.spawnAddrRev++
		h.mu.Unlock()
		break
	}
	for _, addr := range gone {
		log.Printf("agent spawn %s ended before its agent reported: its messages and notices are not delivered", addr)
	}
	if len(gone) > 0 {
		h.changed("agents")
	}
	return nil
}

// spawnAddrsRead reads the agent spawn addresses from the store.
func (h *Hub) spawnAddrsRead(ctx context.Context) (map[string]*spawnAddr, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT spawn_id, title, identity, status FROM spawns WHERE agent_id LIKE 'spawn:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	next := map[string]*spawnAddr{}
	for rows.Next() {
		var sa spawnAddr
		var status string
		if err := rows.Scan(&sa.spawnID, &sa.name, &sa.identity, &status); err != nil {
			return nil, err
		}
		sa.ended = status == "closed" || status == "failed"
		next[spawnAddress(sa.spawnID)] = &sa
	}
	return next, rows.Err()
}

// spawnEnded loads the spawn addresses again after an agent spawn closed or failed.
func (h *Hub) spawnEnded(ctx context.Context) {
	if err := h.spawnAddrsLoad(ctx); err != nil {
		log.Printf("spawn addresses: %v", err)
	}
}

// noticeTarget is the agent record that receives a notice for id: an identity, or for a spawn
// address the identity that it bound. A notice to a spawn waits until its first report, and goes
// only while that identity is current: after a /clear the address still names the first
// conversation. h.mu must be held.
func (h *Hub) noticeTarget(id string) *Agent {
	if sa := h.spawnAddrs[id]; sa != nil {
		if a := h.idents[sa.identity]; a != nil && a.IdState == "current" {
			return a
		}
		return nil
	}
	return h.idents[id]
}

// identsOf returns the values of an agent map in a stable order. h.mu must be held.
func identsOf(m map[string]*Agent) []*Agent {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*Agent, len(keys))
	for i, k := range keys {
		out[i] = m[k]
	}
	return out
}

// agentsList handles hub.agents {all?}: "user", then every current or detached identity. all:
// also the superseded identities, and each agent spawn address that waits for its first report
// (state "starting"), so that a message to it can wait (lead-d-r1).
func (h *Hub) agentsList(args map[string]any) any {
	all, _ := args["all"].(bool)
	only, _ := args["session_id"].(string) // optional: only agents whose pane is in this session
	h.mu.Lock()
	defer h.mu.Unlock()
	list := []any{}
	if only == "" {
		list = append(list, map[string]any{"id": "user", "name": "User", "joined": false, "ready": false, "session_id": ""})
	}
	for _, a := range identsOf(h.idents) {
		if a.IdState == "superseded" && !all {
			continue
		}
		if only != "" && a.Session != only {
			continue
		}
		name := a.Name
		if name == "" {
			name = a.ID // an unnamed tab: readers need a nonempty name (messaging tab titles)
		}
		e := map[string]any{"id": a.ID, "name": name, "kind": a.Kind, "state": a.IdState,
			"joined": a.IdState == "current" && a.live(), "ready": a.IdState == "current" && a.ready(), "session_id": a.Session}
		if a.IdState == "current" && a.live() {
			e["activity"] = a.State // the agent's program status: idle, working, done, …; "" when unknown
			e["block_id"] = a.BlockID
		}
		list = append(list, e)
	}
	if all && only == "" {
		addrs := make([]string, 0, len(h.spawnAddrs))
		for addr, sa := range h.spawnAddrs {
			if sa.identity == "" && !sa.ended {
				addrs = append(addrs, addr)
			}
		}
		sort.Strings(addrs)
		for _, addr := range addrs {
			sa := h.spawnAddrs[addr]
			list = append(list, map[string]any{"id": addr, "name": sa.name, "state": "starting", "spawn_id": sa.spawnID, "joined": false, "ready": false, "session_id": ""})
		}
	}
	return map[string]any{"agents": list}
}

// resolveAgent handles hub.agents.resolve {to}: an exact ID, a spawn address, a unique identity
// prefix (at least 4 characters after the colon), or a unique name among current identities. A
// spawn address gives the first identity that its pane reported; before that report, the address
// itself (bound false) while the spawn has not ended (lead-d-r1).
func (h *Hub) resolveAgent(ctx context.Context, args map[string]any) (any, error) {
	to, _ := args["to"].(string)
	to = strings.TrimSpace(to)
	if to == "" {
		return nil, bad("invalid: to is required")
	}
	if local, remote, ok, err := h.resolveQualified(to); err != nil || remote != nil {
		return remote, err
	} else if ok {
		to = local
	}
	if spawnID, ok := strings.CutPrefix(to, "spawn:"); ok {
		var agent, identity, status string
		err := h.db.QueryRowContext(ctx, `SELECT agent_id, identity, status FROM spawns WHERE spawn_id = ?`, spawnID).Scan(&agent, &identity, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, bad("unknown: no spawn %s", spawnID)
		case err != nil:
			return nil, err
		case agent != to:
			return nil, refuse("invalid_target", "spawn %s is not an agent spawn: it never reports an identity", spawnID)
		case identity != "":
			return map[string]any{"id": identity, "via": "spawn"}, nil
		case status == "closed" || status == "failed":
			return nil, refuse("not_delivered", "the spawn ended before its agent reported")
		}
		return map[string]any{"id": to, "via": "spawn", "bound": false}, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if to == "user" || h.idents[to] != nil {
		return map[string]any{"id": to, "via": "id"}, nil
	}
	candidates := func(match func(a *Agent) bool) []*Agent {
		var out []*Agent
		for _, a := range identsOf(h.idents) {
			if match(a) {
				out = append(out, a)
			}
		}
		return out
	}
	pick := func(found []*Agent, via string) (any, error) {
		if len(found) == 1 {
			return map[string]any{"id": found[0].ID, "via": via}, nil
		}
		var names []string
		for _, a := range found {
			names = append(names, a.ID+" "+a.Name)
		}
		return nil, bad("ambiguous: %q matches several agents (candidates: %s)", to, strings.Join(names, ", "))
	}
	if kind, rest, ok := strings.Cut(to, ":"); ok && len(rest) >= 4 {
		if found := candidates(func(a *Agent) bool { return a.Kind == kind && strings.HasPrefix(a.ID, to) }); len(found) > 0 {
			return pick(found, "prefix")
		}
	}
	if found := candidates(func(a *Agent) bool { return a.IdState == "current" && strings.EqualFold(a.Name, to) }); len(found) > 0 {
		return pick(found, "name")
	}
	return nil, bad("unknown: no agent matches %q", to)
}

// agentIDs is every ID of one agent: its identity and each spawn address bound to that identity
// (spawns.identity, read now). A grant or a key can name either, so replay, stop and grants accept
// any ID of the set.
func (h *Hub) agentIDs(ctx context.Context, id string) ([]string, error) {
	return agentIDsIn(ctx, h.db, id)
}

// dbQuerier is *sql.DB or *sql.Tx.
type dbQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func agentIDsIn(ctx context.Context, q dbQuerier, id string) ([]string, error) {
	if id == "" || id == "user" {
		return []string{id}, nil
	}
	root := id
	if spawnID, ok := strings.CutPrefix(id, "spawn:"); ok {
		var bound string
		switch err := q.QueryRowContext(ctx, `SELECT identity FROM spawns WHERE spawn_id = ? AND agent_id = ?`, spawnID, id).Scan(&bound); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, err
		case bound != "":
			root = bound
		}
		if root == id {
			return []string{id}, nil // not bound yet: the address alone
		}
	}
	set := []string{root}
	rows, err := q.QueryContext(ctx, `SELECT agent_id FROM spawns WHERE identity = ? AND agent_id LIKE 'spawn:%' ORDER BY agent_id`, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		set = append(set, a)
	}
	return set, rows.Err()
}

// sameAgent reports whether a and b are IDs of one agent.
func (h *Hub) sameAgent(ctx context.Context, a, b string) (bool, error) {
	if a == b {
		return true, nil
	}
	set, err := h.agentIDs(ctx, a)
	if err != nil {
		return false, err
	}
	for _, id := range set {
		if id == b {
			return true, nil
		}
	}
	return false, nil
}

// callerKeyRow finds the row of key made by any ID of caller's agent: the query selects the
// columns in cols from table. Two rows (two IDs of one agent used the key before they became
// one) are refused: a replay cannot tell which request it repeats.
func (h *Hub) callerKeyRow(ctx context.Context, table, cols, caller, key string) (*sql.Row, error) {
	set, err := h.agentIDs(ctx, caller)
	if err != nil {
		return nil, err
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(set)), ",")
	params := []any{key}
	for _, id := range set {
		params = append(params, id)
	}
	var owners []string
	rows, err := h.db.QueryContext(ctx, `SELECT caller FROM `+table+` WHERE key = ? AND caller IN (`+in+`) ORDER BY caller`, params...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return nil, err
		}
		owners = append(owners, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(owners) > 1 {
		return nil, refuse("conflict", "key %q was used by %s before they became one identity; use a new key", key, strings.Join(owners, " and by "))
	}
	return h.db.QueryRowContext(ctx, `SELECT `+cols+` FROM `+table+` WHERE key = ? AND caller IN (`+in+`)`, params...), nil
}

// errKeyTaken: another ID of the caller's agent admitted the key after this call's lookup.
var errKeyTaken = refuse("conflict", "another ID of this agent admitted the same key at the same time; retry with the same key")

// keyTakenIn checks, inside the admission transaction, that no ID of caller's agent has a row
// for key. The hub's database has one connection, so the check and the insert are atomic with
// respect to every other admission and every alias write.
func keyTakenIn(ctx context.Context, tx *sql.Tx, table, caller, key string) error {
	set, err := agentIDsIn(ctx, tx, caller)
	if err != nil {
		return err
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(set)), ",")
	params := []any{key}
	for _, id := range set {
		params = append(params, id)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE key = ? AND caller IN (`+in+`)`, params...).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errKeyTaken
	}
	return nil
}
