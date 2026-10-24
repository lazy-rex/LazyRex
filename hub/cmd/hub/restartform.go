package main

// The restart form (docs/hub.md, "Agent restart"): the palette entries "Restart Codex agents…" and
// "Restart Claude agents…" open a form that lists the agents to restart. Restart stops and resumes
// exactly the listed agents, or none.
//
// The hub owns this form; no plugin does. A plugin cannot run hub.admin.* (the admin guard refuses
// plugin panes), and a plugin must not get that power. The form's view rows have the owner
// restartFormOwner, which no plugin can have, and callPlugin answers the owner's view.render and
// view.action here instead of through Rex. So the form uses the view service unchanged: view_ops
// (operation keys, retries, a lost reply, a hub restart), one worker per owner (Refresh and the first
// admission never run at the same time), and boundCaller (only the bound renderer acts).
//
// The snapshot. hub.admin.restart.form takes it once, after the admin guard, and stores it in the
// form's context with revision 1 and a new operation key. The render only reads: it shows the
// stored snapshot and marks a target that changed since. Restart sends the revision in its action
// name (restart@<revision>); the hub restarts the stored snapshot of that revision, never a list from
// the renderer, through adminAgentOp with the stored key. Every listed target must still match its
// token, or the whole batch is refused before any stop. Refresh is an action (refresh@<revision>):
// it takes a new snapshot (a new revision and key). After the admission the form is frozen: Refresh
// is refused, and the render shows the operation's outcome. A new restart needs a new form.
//
// The stored context is the only state. An operation waits in view_ops until the worker runs it,
// and the form can change in between (a Refresh queued before it), so every call reads the context
// of the view row when it runs, never a copy from the admission. viewAdmit keeps the context out of
// this owner's operation arguments, so a retry of the same key matches after a Refresh too.
//
// The token binds the agent to its pane and to its process generation, as restartCheck sees them.
// A token cannot prove the current executable by itself: restartCheck still runs live at admission
// and before every key and signal.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// restartFormOwner is the plugin field of the form's view rows. A plugin name must match pluginName
// (sources.go, `^[a-z0-9_-]+$`), which has no colon, and register refuses this name too.
const restartFormOwner = "hub:restart"

// restartForm is the form's stored state, in its form context.
type restartForm struct {
	Kind     string         `json:"kind"`
	Revision int            `json:"revision"`
	Key      string         `json:"key"` // the hub.admin.restart key of this revision
	Targets  []restartToken `json:"targets"`
	// RefreshOp is the view operation key of the Refresh that took this revision ("" at open). A
	// retry of that operation (a crash after the context write) changes nothing.
	RefreshOp string `json:"refresh_op,omitempty"`
}

// restartToken is one target of the form's snapshot.
type restartToken struct {
	Identity string `json:"identity"`
	Name     string `json:"name,omitempty"` // shown only
	Session  string `json:"session_id"`
	Block    string `json:"block_id"`
	Spawn    string `json:"spawn_id"`
	PID      int    `json:"pid"`
	Start    string `json:"start"`
	Path     string `json:"path"`    // the executable that the accepted identity report recorded
	Invoked  string `json:"invoked"` // the exec path that the report recorded ("" if Rex gave none)
	Program  string `json:"program"` // the configured program of the pane's spawn
	Refused  string `json:"refused,omitempty"`
}

// adminRestartForm handles hub.admin.restart.form {kind, ctx} from the hub's palette entry. The
// admin guard runs before it (main.go). It opens the form in the window of ctx.block_id.
func (h *Hub) adminRestartForm(ctx context.Context, p perform, args map[string]any) (any, error) {
	kind, _ := args["kind"].(string)
	cli := harnessOf(kind)
	if cli == nil {
		return nil, bad("kind must be %s", harnessKinds())
	}
	title := cli.name
	rc, _ := args["ctx"].(map[string]any)
	// As in paletteRun: a config action's ctx.client_id is the client that ran it.
	if cid, _ := rc["client_id"].(string); cid == "" || cid != p.Requester.ClientID {
		return nil, bad("ctx.client_id is not the calling client")
	}
	sess, _ := rc["session_id"].(string)
	block, _ := rc["block_id"].(string)
	if sess == "" {
		return nil, bad("run this command from the Rex palette or a key binding: it needs a session")
	}
	window, err := h.formWindow(ctx, sess, block)
	if err != nil {
		return nil, err
	}
	targets, err := h.restartSnapshot(ctx, kind)
	if err != nil {
		return nil, err
	}
	fc := formContext{SessionID: sess, WindowID: window, SourceBlock: block,
		Restart: &restartForm{Kind: kind, Revision: 1, Key: randomID("restart-form-"), Targets: targets}}
	out, err := h.openSlot(ctx, slotRequest{Plugin: restartFormOwner, Route: "restart", Ctx: fc, Focus: true, NextTitle: "Restart " + title + " agents"})
	if err != nil {
		return nil, err
	}
	if out["status"] == "opened" {
		go h.refocusForm(sess, window, h.viewBlock(out["view_id"].(string)), block)
	}
	out["select"] = map[string]any{"session_id": sess, "window_id": window}
	return out, nil
}

// restartSnapshot takes the snapshot for a kind. It only reads: the hub's rows and the live pane
// program (restartTargets). A target that fails a check is in the snapshot with its reason, and is
// never restarted.
func (h *Hub) restartSnapshot(ctx context.Context, kind string) ([]restartToken, error) {
	targets, err := h.restartTargets(ctx, map[string]any{"kind": kind})
	if err != nil {
		return nil, err
	}
	out := make([]restartToken, 0, len(targets))
	for _, t := range targets {
		out = append(out, h.tokenOf(ctx, t))
	}
	return out, nil
}

// tokenOf is the token of a target that restartTargets resolved.
func (h *Hub) tokenOf(ctx context.Context, t opTarget) restartToken {
	tok := restartToken{Identity: t.identity, Session: t.stopSession, Block: t.stopBlock, Spawn: t.sourceSpawn,
		PID: t.childPID, Start: t.childStart, Program: t.program}
	h.db.QueryRowContext(ctx, `SELECT name FROM identities WHERE identity = ?`, t.identity).Scan(&tok.Name)
	if t.phase != "restart_admitted" {
		tok.Refused = t.detail
	} else if err := h.db.QueryRowContext(ctx, `SELECT child_path, child_invoked FROM identity_panes WHERE block_id = ? AND current = ?`,
		t.stopBlock, t.identity).Scan(&tok.Path, &tok.Invoked); err != nil {
		tok.Refused = "the pane's record cannot be read: " + err.Error()
	}
	if tok.Refused == "" {
		tok.Refused = tok.incomplete()
	}
	return tok
}

// incomplete reports the evidence that the token lacks, or "". Missing evidence refuses.
func (t restartToken) incomplete() string {
	switch {
	case t.Identity == "" || t.Session == "" || t.Block == "" || t.Spawn == "":
		return "the agent's pane or spawn is not recorded"
	case t.PID == 0 || t.Start == "":
		return "the agent's process generation is not recorded"
	case t.Path == "" || t.Program == "":
		return "the agent's executable is not recorded"
	}
	return ""
}

// changed reports how the target now differs from the shown token, or "". The pane and the process
// generation are compared apart, so the reason says which one changed.
func (t restartToken) changed(now restartToken) string {
	switch {
	case now.Identity != t.Identity:
		return "another agent"
	case now.Refused != "":
		return now.Refused
	case now.Session != t.Session || now.Block != t.Block:
		return fmt.Sprintf("the agent moved to another pane (%s)", now.Block)
	case now.Spawn != t.Spawn:
		return "the pane belongs to another spawn"
	case now.PID != t.PID || now.Start != t.Start:
		return fmt.Sprintf("the agent's process changed (pid %d)", now.PID)
	case now.Path != t.Path || now.Invoked != t.Invoked || now.Program != t.Program:
		return "the agent's executable changed"
	}
	return ""
}

// listed is the targets that a submit restarts: those without a refusal.
func (f *restartForm) listed() []restartToken {
	var out []restartToken
	for _, t := range f.Targets {
		if t.Refused == "" {
			out = append(out, t)
		}
	}
	return out
}

// snapshotTargets resolves a form submit for adminAgentOp: the listed targets, each read again now.
// Any listed target that changed refuses the whole batch, before anything is written.
func (h *Hub) snapshotTargets(ctx context.Context, f *restartForm) ([]opTarget, error) {
	listed := f.listed()
	if len(listed) == 0 {
		return nil, refuse("invalid_state", "the form lists no agent to restart")
	}
	fresh, err := h.restartTargets(ctx, map[string]any{"kind": f.Kind})
	if err != nil {
		return nil, err
	}
	byID := map[string]opTarget{}
	for _, t := range fresh {
		byID[t.identity] = t
	}
	var out []opTarget
	var conflicts []string
	for _, tok := range listed {
		t, ok := byID[tok.Identity]
		if !ok {
			conflicts = append(conflicts, tok.Identity+": the agent is no longer current")
			continue
		}
		if why := tok.changed(h.tokenOf(ctx, t)); why != "" {
			conflicts = append(conflicts, tok.Identity+": "+why)
			continue
		}
		out = append(out, t)
	}
	if len(conflicts) > 0 {
		return nil, refuse("conflict", "the agents changed since the form showed them, nothing was restarted (%s); refresh the form", strings.Join(conflicts, "; "))
	}
	return out, nil
}

// restartFormCall answers view.render and view.action for the form's views (callPlugin). Errors
// are definite (*rex.RexError) when they are an answer, as a plugin's would be.
func (h *Hub) restartFormCall(ctx context.Context, action string, args any, out any) error {
	b, _ := json.Marshal(args)
	var in struct {
		ViewID string `json:"view_id"`
		OpKey  string `json:"operation_key"`
		Action string `json:"action"`
	}
	json.Unmarshal(b, &in)
	// The view row's context, as it is now (see the top of this file).
	var stored string
	err := h.db.QueryRowContext(ctx, `SELECT context FROM views WHERE view_id = ? AND plugin = ?`, in.ViewID, restartFormOwner).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err // not an answer: the view service tries again
	}
	var fc formContext
	if err != nil || json.Unmarshal([]byte(stored), &fc) != nil || fc.Restart == nil {
		return &rex.RexError{Code: "invalid_state", Message: "the restart form has no stored snapshot"}
	}
	var res any
	switch action {
	case "view.render":
		res, err = h.restartFormRender(ctx, fc.Restart)
	case "view.action":
		res, err = h.restartFormAction(ctx, in.ViewID, in.OpKey, stored, fc, in.Action)
	default:
		return &rex.RexError{Code: "unknown_action", Message: action + " is not supported by the restart form"}
	}
	var se *storeError
	if errors.As(err, &se) {
		return &rex.RexError{Code: storeErrorCode(se), Message: se.msg}
	}
	if err != nil {
		return err // not an answer: the view service tries again
	}
	if out == nil {
		return nil
	}
	b, _ = json.Marshal(res)
	return json.Unmarshal(b, out)
}

// restartOp is the operation of the form's key, or "".
func (h *Hub) restartOp(ctx context.Context, key string) (string, error) {
	var op string
	err := h.db.QueryRowContext(ctx, `SELECT op_id FROM agent_ops WHERE caller = 'admin' AND key = ?`, key).Scan(&op)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return op, err
}

// restartFormAction runs one view operation (opKey) against the stored context of the view (stored,
// decoded as fc). An action names the revision that the user saw.
func (h *Hub) restartFormAction(ctx context.Context, viewID, opKey, stored string, fc formContext, action string) (any, error) {
	f := fc.Restart
	name, revText, _ := strings.Cut(action, "@")
	rev, revErr := strconv.Atoi(revText)
	switch name {
	case "close":
		return map[string]any{"close_form": true}, nil
	case "refresh":
		// This operation took the stored revision already: a retry after a crash between the
		// context write and the operation's record. No new snapshot, no new key.
		if opKey != "" && f.RefreshOp == opKey {
			return map[string]any{}, nil
		}
		// After the admission the form is frozen: the outcome is shown, and a new restart needs a
		// new form.
		if op, err := h.restartOp(ctx, f.Key); err != nil {
			return nil, err
		} else if op != "" {
			return nil, refuse("invalid_state", "this form's restart was admitted; open a new form for another restart")
		}
		if revErr != nil || rev != f.Revision {
			return nil, refuse("stale", "the form changed since it was shown; look at it again")
		}
		targets, err := h.restartSnapshot(ctx, f.Kind)
		if err != nil {
			return nil, err
		}
		next := *f
		next.Revision, next.Key, next.Targets, next.RefreshOp = f.Revision+1, randomID("restart-form-"), targets, opKey
		fc.Restart = &next
		b, _ := json.Marshal(fc)
		// Only over the context that this call read. One worker runs the owner's operations, so
		// nothing else writes it; a closed view takes no new snapshot.
		res, err := h.db.ExecContext(ctx, `UPDATE views SET context = ? WHERE view_id = ? AND plugin = ? AND status = 'open' AND context = ?`,
			string(b), viewID, restartFormOwner, stored)
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n == 0 {
			return nil, refuse("stale", "the form changed or closed since it was shown")
		}
		return map[string]any{}, nil
	case "restart":
		// The revision that the user saw, checked against the stored one. A retry of an admitted
		// restart names the same revision (the form is frozen after the admission) and gets the
		// same operation: agentOp looks up the stored key before it reads any target.
		if revErr != nil || rev != f.Revision {
			return nil, refuse("stale", "the form changed since it was shown; look at it again")
		}
		res, err := h.agentOp(ctx, "restart", map[string]any{"key": f.Key, "kind": f.Kind}, f)
		if err != nil {
			return nil, err
		}
		return map[string]any{"operation": res}, nil
	}
	return nil, refuse("unknown_action", "the restart form has no action %q", action)
}

// restartFormRender is the form's tree. It only reads.
func (h *Hub) restartFormRender(ctx context.Context, f *restartForm) (any, error) {
	title := "Restart " + harnessName(f.Kind) + " agents"
	closeAction := map[string]any{"name": "close", "label": "Close", "enabled": true, "user_only": restartUserOnly}
	op, err := h.restartOp(ctx, f.Key)
	if err != nil {
		return nil, err
	}
	if op != "" {
		reply, err := h.opReply(ctx, op)
		if err != nil {
			return nil, err
		}
		r := reply.(map[string]any)
		state := "Restarting…"
		if r["done"] == true {
			state = "Done."
		}
		items := []any{}
		for _, e := range r["targets"].([]any) {
			t := e.(map[string]any)
			detail := fmt.Sprint(t["phase"])
			for _, k := range []string{"stop", "detail"} {
				if v, ok := t[k].(string); ok && v != "" {
					detail += " · " + v
				}
			}
			items = append(items, map[string]any{"id": t["agent"], "label": t["agent"], "detail": detail, "action": nil})
		}
		return map[string]any{"tree": map[string]any{"title": title, "back": closeAction, "nodes": []any{
			map[string]any{"id": "state", "type": "widget", "text": state, "action": nil},
			map[string]any{"id": "outcome", "type": "list", "label": "Outcome", "items": items},
			map[string]any{"id": "close", "type": "widget", "text": "", "action": closeAction},
		}}}, nil
	}
	// Before the restart: the stored snapshot, and what changed since (read now).
	now := map[string]restartToken{}
	if fresh, err := h.restartSnapshot(ctx, f.Kind); err == nil {
		for _, t := range fresh {
			now[t.Identity] = t
		}
	}
	listed, refused := []any{}, []any{}
	changed := false
	for _, t := range f.Targets {
		label := t.Identity
		if t.Name != "" {
			label = t.Name + " (" + t.Identity + ")"
		}
		if t.Refused != "" {
			refused = append(refused, map[string]any{"id": t.Identity, "label": label, "detail": t.Refused, "action": nil})
			continue
		}
		detail := fmt.Sprintf("pane %s · %s · pid %d", t.Block, t.Program, t.PID)
		cur, ok := now[t.Identity]
		why := "the agent is no longer current"
		if ok {
			why = t.changed(cur)
		}
		if why != "" {
			changed = true
			detail += " · changed since the form opened: " + why
		}
		listed = append(listed, map[string]any{"id": t.Identity, "label": label, "detail": detail, "action": nil})
	}
	text := "Restart stops " + agentCount(len(listed)) + " and resumes each one in a new pane."
	switch {
	case len(listed) == 0:
		text = "No agent can be restarted."
	case changed:
		text = "An agent changed since the form opened. Refresh the form before a restart."
	}
	nodes := []any{
		map[string]any{"id": "intro", "type": "widget", "text": text, "action": nil},
		map[string]any{"id": "targets", "type": "list", "label": "To restart", "items": listed},
	}
	if len(refused) > 0 {
		nodes = append(nodes, map[string]any{"id": "refused", "type": "list", "label": "Not restarted", "items": refused})
	}
	nodes = append(nodes,
		map[string]any{"id": "restart", "type": "widget", "text": "", "action": map[string]any{
			"name": "restart@" + strconv.Itoa(f.Revision), "label": "Restart " + agentCount(len(listed)), "enabled": len(listed) > 0 && !changed, "user_only": restartUserOnly}},
		map[string]any{"id": "refresh", "type": "widget", "text": "", "action": map[string]any{"name": "refresh@" + strconv.Itoa(f.Revision), "label": "Refresh", "enabled": true, "user_only": restartUserOnly}},
		map[string]any{"id": "close", "type": "widget", "text": "", "action": closeAction},
	)
	return map[string]any{"tree": map[string]any{"title": title, "back": closeAction, "nodes": nodes}}, nil
}

// kickRestartForms renders the open restart forms again, so a form shows its operation's progress.
func (h *Hub) kickRestartForms() {
	h.kickViews(restartFormOwner, causeChanged) // every form of the owner: no form declares dependencies
}

// restartUserOnly is the agent-parity reason of the form's actions (docs/testing.md, "Agent
// parity"): a restart is an admin action, which the admin guard refuses from agent panes.
const restartUserOnly = "admin: hub.admin.restart refuses agent panes"

// agentCount is "1 agent" or "N agents".
func agentCount(n int) string {
	if n == 1 {
		return "1 agent"
	}
	return strconv.Itoa(n) + " agents"
}
