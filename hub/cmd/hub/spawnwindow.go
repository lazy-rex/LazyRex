package main

// Spawn windows (coordinator, 2026-10-17: approved for session templates; the Templates pair
// builds it, the Profiles pair reviews it).
//
// hub.spawn_window {key, session?, title?, layout} starts a whole tab of profiles with one Rex
// request. layout is Rex's own tree: a node is {leaf: {profile, args?, cwd?, title?}} or
// {split: {direction: horizontal|vertical, ratio, before, after}}, and ratio is the before share,
// as in session.view. Observed on Rex 1203 (lab, tests/templates-layout-lab.mjs): one
// session.new_window creates the whole tree and returns block_ids in depth-first order; an
// invalid leaf creates no block and no window.
//
// Each leaf is an ordinary spawn row (the same agents, cwd locks and recovery as hub.spawn), with
// the key "window/<key>/<n>" (n: depth-first leaf index) and the label lazyrex-spawn:<spawn id>.
// One spawn_windows row holds the exact request, so a retry with the same key returns the same
// spawns and a changed request is refused. All rows commit in one transaction before Rex creates
// anything. Then the hub sends one new_window:
//   - Rex refuses: every leaf fails (Rex answered, so nothing was created).
//   - Rex succeeds with one window ID and one distinct block per leaf: every leaf opens (block by
//     position).
//   - No answer, or a success answer of another shape: every leaf stays sending. resolveSpawns treats each leaf as it treats a sending
//     hub.spawn row (an agent leaf opens when its agent joins from its one labeled pane). The hub
//     never sends a second new_window.
// placement {focus: {source_window_id}} (user spawns only, spawnfocus.go): on the first success
// the hub focuses the first leaf (depth-first), only while the source tab is still active in the
// target session. A retry or a late adoption never focuses. It is part of the retry identity.
// Callers: the same policy as hub.spawn (spawnCaller): a registered plugin acting for "user" or
// on_behalf of the agent of a forwarded call in flight, a plugin with a durable grant of this exact
// request (grant_id, grants.go), or an identified agent pane. The action is not in
// hubToolActions, so MCP does not offer it.
// Window status: pending if a leaf is sending, else failed if a leaf failed, else closed if a leaf
// closed, else open.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const spawnWindowSchema = `
-- One row per hub.spawn_window request. spawn_ids lists the leaf spawns in depth-first order.
CREATE TABLE IF NOT EXISTS spawn_windows (caller TEXT NOT NULL, key TEXT NOT NULL, request TEXT NOT NULL,
  session_id TEXT NOT NULL, spawn_ids TEXT NOT NULL, created TEXT NOT NULL, PRIMARY KEY (caller, key));
`

// windowKeyPrefix keeps leaf keys apart from hub.spawn keys of the same caller.
const windowKeyPrefix = "window/"

type windowLeaf struct {
	Profile string            `json:"profile"`
	Args    map[string]string `json:"args,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Title   string            `json:"title,omitempty"`
}

type windowNode struct {
	Leaf  *windowLeaf  `json:"leaf,omitempty"`
	Split *windowSplit `json:"split,omitempty"`
}

type windowSplit struct {
	Direction string      `json:"direction"`
	Ratio     float64     `json:"ratio"`
	Before    *windowNode `json:"before"`
	After     *windowNode `json:"after"`
}

// parseWindowNode checks one node strictly: an unknown field is refused, so a typo never changes
// the layout silently.
func parseWindowNode(raw any, leaves *[]*windowLeaf) (*windowNode, error) {
	m, ok := raw.(map[string]any)
	if !ok || len(m) != 1 {
		return nil, bad("each layout node must be an object with exactly one of leaf or split")
	}
	if v, ok := m["leaf"]; ok {
		lm, ok := v.(map[string]any)
		if !ok {
			return nil, bad("leaf must be an object")
		}
		l := &windowLeaf{}
		for k, v := range lm {
			switch k {
			case "profile", "cwd", "title":
				text, ok := v.(string)
				if !ok {
					return nil, bad("leaf.%s must be text", k)
				}
				switch k {
				case "profile":
					l.Profile = text
				case "cwd":
					l.Cwd = text
				case "title":
					l.Title = text
				}
			case "args":
				am, ok := v.(map[string]any)
				if !ok {
					return nil, bad("leaf.args must be an object of strings")
				}
				l.Args = map[string]string{}
				for name, av := range am {
					str, ok := av.(string)
					if !ok || len(str) > 16000 {
						return nil, bad("arg %q must be text of at most 16000 bytes", name)
					}
					l.Args[name] = str
				}
			default:
				return nil, bad("unknown leaf field %q", k)
			}
		}
		if l.Profile == "" {
			return nil, bad("leaf.profile is required")
		}
		if l.Cwd != "" && !strings.HasPrefix(l.Cwd, "/") {
			return nil, bad("%q must be an absolute directory path", l.Cwd)
		}
		if len([]rune(l.Title)) > 60 {
			return nil, bad("leaf.title is at most 60 characters")
		}
		*leaves = append(*leaves, l)
		return &windowNode{Leaf: l}, nil
	}
	v, ok := m["split"]
	if !ok {
		return nil, bad("each layout node must be an object with exactly one of leaf or split")
	}
	sm, ok := v.(map[string]any)
	if !ok {
		return nil, bad("split must be an object")
	}
	for k := range sm {
		switch k {
		case "direction", "ratio", "before", "after":
		default:
			return nil, bad("unknown split field %q", k)
		}
	}
	sp := &windowSplit{}
	sp.Direction, _ = sm["direction"].(string)
	if sp.Direction != "horizontal" && sp.Direction != "vertical" {
		return nil, bad("split.direction must be horizontal or vertical")
	}
	r, ok := sm["ratio"].(float64)
	if !ok || math.IsNaN(r) || math.IsInf(r, 0) || r <= 0 || r >= 1 {
		return nil, bad("split.ratio must be greater than 0 and less than 1")
	}
	sp.Ratio = r
	var err error
	if sp.Before, err = parseWindowNode(sm["before"], leaves); err != nil {
		return nil, err
	}
	if sp.After, err = parseWindowNode(sm["after"], leaves); err != nil {
		return nil, err
	}
	return &windowNode{Split: sp}, nil
}

// rexLayout turns the tree into a session.new_window layout. blocks are the leaf blocks in
// depth-first order; next counts them.
func (n *windowNode) rexLayout(blocks []map[string]any, next *int) map[string]any {
	if n.Leaf != nil {
		b := blocks[*next]
		*next++
		return map[string]any{"block": b}
	}
	return map[string]any{"split": map[string]any{"direction": n.Split.Direction, "ratio": n.Split.Ratio,
		"before": n.Split.Before.rexLayout(blocks, next), "after": n.Split.After.rexLayout(blocks, next)}}
}

func (h *Hub) spawnWindow(ctx context.Context, p perform, args map[string]any) (any, error) {
	var caller string
	var err error
	if _, ok := args["grant_id"]; ok {
		caller, err = h.grantCaller(ctx, h.db, p, "hub.spawn_window", args) // grants.go
	} else {
		caller, err = h.spawnCaller(ctx, p, args)
	}
	if err != nil {
		return nil, err
	}
	for k := range args {
		switch k {
		case "key", "session", "title", "layout", "for", "on_behalf", "placement", "grant_id":
		default:
			return nil, bad("unknown field %q", k)
		}
	}
	text := func(name string) (string, error) {
		v, ok := args[name]
		if !ok {
			return "", nil
		}
		str, ok := v.(string)
		if !ok || str == "" {
			return "", bad("%s must be nonempty text", name)
		}
		return str, nil
	}
	if _, err := text("for"); err != nil {
		return nil, err
	}
	key, err := text("key")
	if err != nil {
		return nil, err
	}
	if key == "" || len(key) > 100 {
		return nil, bad("key is required (at most 100 characters)")
	}
	sessArg, err := text("session")
	if err != nil {
		return nil, err
	}
	title, err := text("title")
	if err != nil {
		return nil, err
	}
	if len([]rune(title)) > 60 {
		return nil, bad("title is at most 60 characters")
	}
	var focus *spawnFocus
	if raw, ok := args["placement"]; ok {
		m, ok := raw.(map[string]any)
		if !ok || len(m) != 1 || m["focus"] == nil {
			return nil, bad("placement must be an object with only focus")
		}
		if focus, err = parseSpawnFocus(m["focus"]); err != nil {
			return nil, err
		}
		if args["for"] != "user" {
			return nil, bad("placement.focus is only available for user spawns")
		}
	}
	var leaves []*windowLeaf
	root, err := parseWindowNode(args["layout"], &leaves)
	if err != nil {
		return nil, err
	}
	request := map[string]any{"session": sessArg, "title": title, "layout": root}
	if focus != nil {
		request["placement"] = map[string]any{"focus": focus}
	}
	fp, _ := json.Marshal(request)

	var prevReq, prevIDs string
	row, err := h.callerKeyRow(ctx, "spawn_windows", "request, spawn_ids", caller, key)
	if err != nil {
		return nil, err
	}
	switch err := row.Scan(&prevReq, &prevIDs); {
	case err == nil:
		if prevReq != string(fp) {
			return nil, bad("key %q was used for a different request", key)
		}
		return h.windowResult(ctx, key, prevIDs)
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	byID, _, err := h.loadProfiles()
	if err != nil {
		return nil, err
	}
	profs := make([]Profile, len(leaves))
	resolved := make([]map[string]string, len(leaves))
	cwds := make([]string, len(leaves))
	for i, l := range leaves {
		prof, ok := byID[l.Profile]
		if !ok {
			return nil, bad("no profile %q", l.Profile)
		}
		if resolved[i], err = resolveArgs(prof, l.Args); err != nil {
			return nil, err
		}
		if cwds[i], err = h.effectiveCwd(prof, l.Cwd); err != nil {
			return nil, err
		}
		profs[i] = prof
	}
	sess, err := h.resolveWorkerPlacement(ctx, workerPlacement{Session: sessArg, Direction: "right", Ratio: 0.5})
	if err != nil {
		return nil, err
	}

	// Commit the intent: the window row and every leaf spawn, in one transaction.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows := make([]*spawnRow, len(leaves))
	names := make([]string, len(leaves))
	ids := make([]string, len(leaves))
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for i, l := range leaves {
		s := &spawnRow{ID: randomID("spawn-"), Caller: caller, Key: fmt.Sprintf("%s%s/%d", windowKeyPrefix, key, i),
			Profile: profs[i].ID, Parent: caller, Status: "prepared", Created: now, Session: sess, Cwd: cwds[i]}
		// The leaf request has the hub.spawn fields, so hub.spawn.spec reads both kinds of rows.
		lreq := map[string]any{"profile": l.Profile, "title": l.Title, "args": l.Args, "window": key, "leaf": i}
		if lreq["args"] == nil {
			lreq["args"] = map[string]string{}
		}
		if l.Cwd != "" {
			lreq["cwd"] = l.Cwd
		}
		lfp, _ := json.Marshal(lreq)
		if names[i], err = h.admitSpawn(ctx, tx, s, profs[i], l.Title, string(lfp), resolved[i]); err != nil {
			return nil, err
		}
		rows[i], ids[i] = s, s.ID
	}
	if err := keyTakenIn(ctx, tx, "spawn_windows", caller, key); err != nil {
		return nil, err
	}
	if _, ok := args["grant_id"]; ok {
		// The grant again, inside the admission: a revoke that committed since the first check
		// refuses this call (grants.go).
		if _, err := h.grantCaller(ctx, tx, p, "hub.spawn_window", args); err != nil {
			return nil, err
		}
	}
	idsJSON, _ := json.Marshal(ids)
	if _, err := tx.ExecContext(ctx, `INSERT INTO spawn_windows VALUES (?, ?, ?, ?, ?, ?)`, caller, key, string(fp), sess, string(idsJSON), now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	blocks := make([]map[string]any, len(rows))
	for i, s := range rows {
		h.spawnAdmitted(s, profs[i], names[i])
		opts := map[string]any{"command": spawnArgv(s, profs[i], resolved[i]), "exit": map[string]any{"on_completion": false}, "cwd": s.Cwd}
		blocks[i] = map[string]any{"flavor": "com.superlogical.terminal.shell", "label": "lazyrex-spawn:" + s.ID, "options": opts}
	}
	if err := h.markSending(ctx, rows...); err != nil {
		if u := (*spawnUnrecorded)(nil); errors.As(err, &u) {
			return nil, err
		}
		return h.windowResult(ctx, key, string(idsJSON)) // nothing was sent: every leaf failed (not_created)
	}
	next := 0
	label := title
	if label == "" {
		label = rows[0].Title
	}
	req := map[string]any{"session_id": sess, "window_label": label, "focus": false, "layout": root.rexLayout(blocks, &next)}
	var out struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := windowCreate(cctx, h, req, &out); err != nil {
		var re *rex.RexError
		if errors.As(err, &re) && re.Code != "disconnected" {
			for _, s := range rows {
				h.failSpawn(ctx, s, err.Error()) // Rex answered: nothing was created
			}
		} else {
			h.kickSpawns() // outcome unknown: resolveSpawns looks for each leaf by its label
		}
		return h.windowResult(ctx, key, string(idsJSON))
	}
	if !windowAnswerOK(out.WindowID, out.BlockIDs, len(rows)) {
		// Rex said yes, so panes may exist: the outcome is unknown, never a failure.
		log.Printf("spawn window %s: unexpected new_window result %+v", key, out)
		h.kickSpawns()
		return h.windowResult(ctx, key, string(idsJSON))
	}
	if h.test != nil {
		// Test mode only: a window hold stops here, after Rex created the window and before the
		// hub records it (testmode.go, testWindowHold).
		if hold := h.testWindowHold(p, caller, key); hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	for i, s := range rows {
		if err := h.openSpawn(ctx, s, out.WindowID, out.BlockIDs[i]); err != nil {
			return nil, err
		}
	}
	h.focusSpawn(ctx, sess, out.BlockIDs[0], focus)
	return h.windowResult(ctx, key, string(idsJSON))
}

// windowCreate sends the one new_window request. Tests replace it to lose or garble the answer.
var windowCreate = func(ctx context.Context, h *Hub, req map[string]any, out any) error {
	return h.rex.Call(ctx, "session.new_window", req, out)
}

// windowAnswerOK accepts a success answer only with a window and one distinct block per leaf.
func windowAnswerOK(window string, blocks []string, n int) bool {
	if window == "" || len(blocks) != n {
		return false
	}
	seen := map[string]bool{}
	for _, b := range blocks {
		if b == "" || seen[b] {
			return false
		}
		seen[b] = true
	}
	return true
}

// windowResult reads the leaf spawns again, so a retry reports their current state.
func (h *Hub) windowResult(ctx context.Context, key, idsJSON string) (any, error) {
	var ids []string
	if err := json.Unmarshal([]byte(idsJSON), &ids); err != nil {
		return nil, err
	}
	leaves := make([]any, 0, len(ids))
	counts := map[string]int{}
	out := map[string]any{"key": key}
	for _, id := range ids {
		s, err := scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE spawn_id = ?`, id))
		if err != nil {
			return nil, err
		}
		if _, err := h.workerBlocks(ctx, s); err != nil {
			return nil, err
		}
		counts[s.Status]++
		if s.Session != "" {
			out["session_id"] = s.Session
		}
		if s.Window != "" {
			out["window_id"] = s.Window
		}
		leaves = append(leaves, s.result())
	}
	status := "open"
	switch {
	case counts["prepared"]+counts["sending"] > 0:
		status = "pending"
	case counts["failed"] > 0:
		status = "failed"
	case counts["closed"] > 0:
		status = "closed"
	}
	out["status"], out["leaves"] = status, leaves
	return out, nil
}

// spawnSpec returns the launch spec of the open spawn in a block: what a session template saves.
// Only registered plugins may read it, because args can hold task text.
func (h *Hub) spawnSpec(ctx context.Context, p perform, args map[string]any) (any, error) {
	if h.pluginByClient(p.Requester.ClientID) == nil {
		return nil, bad("only a registered plugin may read a spawn spec")
	}
	block, _ := args["block_id"].(string)
	if block == "" {
		return nil, bad("block_id is required")
	}
	var request, effective string
	s, err := scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+`, request, effective_args FROM spawns WHERE block_id = ? AND status = 'open'`, block), &request, &effective)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{"found": false}, nil
	}
	if err != nil {
		return nil, err
	}
	// The args after defaults, so a later change of a profile default does not change the saved
	// launch. A row from before effective_args has only the args as given ("args_source").
	var args0 map[string]string
	source := "effective"
	if effective != "" {
		json.Unmarshal([]byte(effective), &args0)
	} else {
		var req struct {
			Args map[string]string `json:"args"`
		}
		json.Unmarshal([]byte(request), &req)
		args0, source = req.Args, "request"
	}
	if args0 == nil {
		args0 = map[string]string{}
	}
	out := map[string]any{"found": true, "spawn_id": s.ID, "profile": s.Profile, "args": args0, "args_source": source, "cwd": s.Cwd,
		"title": s.Title, "session_id": s.Session, "window_id": s.Window, "agent_id": s.Agent}
	requested, titleSource := requestedTitle(request)
	out["title_source"] = titleSource
	if requested != "" {
		out["requested_title"] = requested
	}
	return out, nil
}

// requestedTitle reads the title that the caller gave, from a spawn's request. title is the
// resolved title (a default when none was given), so a template cannot tell an explicit title
// from it. source is "request" when the request has a title string (also ""), else "unknown":
// a request that is not JSON, or that has no title string, cannot tell.
func requestedTitle(request string) (title, source string) {
	var req map[string]any
	if json.Unmarshal([]byte(request), &req) != nil {
		return "", "unknown"
	}
	t, ok := req["title"].(string)
	if !ok {
		return "", "unknown"
	}
	return t, "request"
}
