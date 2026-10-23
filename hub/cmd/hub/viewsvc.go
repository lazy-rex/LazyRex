package main

// View services for renderers (../../README.md, "Renderer contract"): snapshots, subscriptions,
// durable operations, and view.changed callbacks.
//
// Rex delivers requests to the hub one at a time, so no handler here waits for a plugin.
// Handlers read snapshots or admit operations. One worker per plugin runs operations and
// renders views by calling the plugin's view.* actions.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const viewOpsSchema = `
-- status: pending, succeeded, failed. Keys are scoped to the plugin and the user.
CREATE TABLE IF NOT EXISTS view_ops (plugin TEXT NOT NULL, user TEXT NOT NULL, op_key TEXT NOT NULL, view_id TEXT NOT NULL,
  kind TEXT NOT NULL, fingerprint TEXT NOT NULL, args TEXT NOT NULL, status TEXT NOT NULL, result TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (plugin, user, op_key));
`

// The renderer's user. Spike 3 has one user.
const viewUser = "user"

type snapshot struct {
	rev    int64
	tree   json.RawMessage
	status string // the tab status, JSON (tabstatus.go); "" = none
	layout string // the effective layout (effectiveLayout): "transcript" = the composer leaves the history to the transcript
}

type viewSvc struct {
	mu      sync.Mutex
	rev     int64                            // view revision counter for this hub generation
	snaps   map[string]*snapshot             // view_id -> current snapshot
	dirty   map[string]bool                  // plugin -> a render pass is needed
	views   map[string]map[string]*viewState // plugin -> view_id -> render state (renderdeps.go)
	all     map[string]renderCause           // plugin -> causes for every open view
	loose   map[string]renderCause           // plugin -> commit and changed causes for a view that has no state yet
	epoch   map[string]int64                 // plugin -> registrations; a result of an older epoch installs nothing
	notify  map[string]bool                  // view_id -> send view.changed after the next render, even if unchanged
	workers map[string]chan struct{}         // plugin -> kick channel
	seen    map[string]map[string]string     // plugin -> route -> reason the user looked at it, not yet sent
	history map[string]*historyFetch         // view_id + "\x00" + before -> a page fetch (viewHistory)
	renders map[string]*openRender           // render ID -> a view.render call that has not returned (renderreads.go)
	senders map[notifyKey]*viewNotifier      // the view.changed deliveries with a request outstanding (viewnotify.go)
	ctx     context.Context                  // the hub run context: its end stops every delivery
}

// historyFetch is one view.history call that runs outside the hub's request queue.
type historyFetch struct {
	started time.Time
	done    bool
	out     map[string]any
	err     error
}

func (h *Hub) viewsStart(ctx context.Context) error {
	h.vs = &viewSvc{snaps: map[string]*snapshot{}, dirty: map[string]bool{}, notify: map[string]bool{}, workers: map[string]chan struct{}{}, seen: map[string]map[string]string{},
		history: map[string]*historyFetch{}, views: map[string]map[string]*viewState{}, all: map[string]renderCause{}, loose: map[string]renderCause{},
		epoch: map[string]int64{}, senders: map[notifyKey]*viewNotifier{}, ctx: ctx}
	if _, err := h.db.ExecContext(ctx, viewOpsSchema); err != nil {
		return err
	}
	for _, m := range []string{
		// The UI effects of a succeeded view action (applyEffects): '' none, pending, done, failed.
		`ALTER TABLE view_ops ADD COLUMN effect_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE view_ops ADD COLUMN effect_error TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE view_ops ADD COLUMN effect_nav INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := h.db.ExecContext(ctx, m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	// Operations admitted before a restart still run, and so do their unfinished effects.
	rows, err := h.db.QueryContext(ctx, `SELECT DISTINCT plugin FROM view_ops WHERE status = 'pending' OR effect_state = 'pending'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		rows.Scan(&p)
		h.kickViews(p, 0)
	}
	return rows.Err()
}

// kickViews wakes the plugin's view worker. A cause renders every open view of the plugin again,
// whatever its declaration; 0 only wakes the worker (operations and effects). markView,
// markCommit and markChanged (renderdeps.go) mark fewer views.
func (h *Hub) kickViews(plugin string, cause renderCause) {
	h.vs.mu.Lock()
	if cause != 0 {
		h.vs.initLocked()
		h.vs.all[plugin] |= cause
		h.vs.dirty[plugin] = true
	}
	ch, ok := h.vs.workers[plugin]
	if !ok {
		ch = make(chan struct{}, 1)
		h.vs.workers[plugin] = ch
		go h.viewWorker(context.Background(), plugin, ch)
	}
	h.vs.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (h *Hub) viewWorker(ctx context.Context, plugin string, kick chan struct{}) {
	for range kick {
		retry := h.runOps(ctx, plugin)
		h.sendSeen(ctx, plugin)
		h.vs.mu.Lock()
		dirty := h.vs.dirty[plugin]
		h.vs.dirty[plugin] = false
		h.vs.mu.Unlock()
		if dirty {
			h.renderViews(ctx, plugin)
		}
		if retry {
			time.Sleep(500 * time.Millisecond)
			select {
			case kick <- struct{}{}:
			default:
			}
		}
	}
}

// sendSeen tells the plugin which routes the user looked at (view.seen). A lost call is
// harmless: the renderer reports again on the next focus or the next snapshot.
func (h *Hub) sendSeen(ctx context.Context, plugin string) {
	h.vs.mu.Lock()
	routes := h.vs.seen[plugin]
	delete(h.vs.seen, plugin)
	h.vs.mu.Unlock()
	for route, reason := range routes {
		var ignored json.RawMessage
		if err := h.callPlugin(ctx, plugin, "view.seen", map[string]any{"route": route, "user": viewUser, "reason": reason}, &ignored); err != nil {
			log.Printf("views %s: seen %s: %v", plugin, route, err)
		}
	}
}

// viewSeen records that the user looks at a view: its pane has focus in an app. The renderer
// calls it on focus, and on a new snapshot while it has focus.
func (h *Hub) viewSeen(ctx context.Context, p perform, args map[string]any) (any, error) {
	v, err := h.readCaller(ctx, p, args) // the transcript pane reports focus too
	if err != nil {
		return nil, err
	}
	reason, _ := args["reason"].(string)
	switch reason {
	case "focus", "content", "connect":
	default:
		reason = "" // an older renderer sends no reason
	}
	h.markSeen(v.Plugin, v.Route, reason)
	if reason == "focus" {
		h.checkProfiles() // the user looks at LazyRex: show a profile edit made meanwhile
	}
	return map[string]any{"status": "accepted"}, nil
}

// markSeen queues view.seen for a route; the plugin's view worker sends it. reason is "focus"
// (the pane got focus), "content" (new content while focused), "connect" or "" (unknown). Queued
// reports coalesce; "focus" wins, so a plugin that refreshes on focus never misses one.
func (h *Hub) markSeen(plugin, route, reason string) {
	h.vs.mu.Lock()
	if h.vs.seen[plugin] == nil {
		h.vs.seen[plugin] = map[string]string{}
	}
	if h.vs.seen[plugin][route] != "focus" {
		h.vs.seen[plugin][route] = reason
	}
	h.vs.mu.Unlock()
	h.kickViews(plugin, 0)
}

// --- Handler side: fast, no plugin calls ---

type boundView struct {
	ID, Plugin, Route string
	Context           string // a form's stored context (JSON); empty for a tab
}

// boundCaller checks that the caller is the renderer attached to the view.
func (h *Hub) boundCaller(ctx context.Context, p perform, args map[string]any) (*boundView, error) {
	id, _ := args["view_id"].(string)
	att, _ := args["attachment"].(string)
	var v boundView
	var client, attachment, status string
	err := h.db.QueryRowContext(ctx, `SELECT plugin, route, client_id, attachment, status, context FROM views WHERE view_id = ?`, id).
		Scan(&v.Plugin, &v.Route, &client, &attachment, &status, &v.Context)
	if err != nil || status != "open" {
		return nil, bad("stale_view: view %q is not open", id)
	}
	if att == "" || att != attachment || client != p.Requester.ClientID {
		return nil, bad("stale_attachment: attach again")
	}
	v.ID = id
	return &v, nil
}

func loadingTree() json.RawMessage {
	return json.RawMessage(`{"title":"","back":null,"nodes":[{"id":"$loading","type":"loading","text":"Loading"}]}`)
}

func (h *Hub) snapshotOf(v *boundView) map[string]any {
	h.vs.mu.Lock()
	s := h.vs.snaps[v.ID]
	h.vs.mu.Unlock()
	if s == nil {
		h.markView(v.Plugin, v.ID, causeSnapshot)
		s = &snapshot{rev: 0, tree: loadingTree()}
	}
	var status any // null: no status (the renderer clears it)
	if s.status != "" {
		status = json.RawMessage(s.status)
	}
	return map[string]any{"schema_version": 1, "view_id": v.ID, "route": v.Route, "revision": revString(s.rev), "tree": s.tree, "status": status,
		"layout": s.layout}
}

func (h *Hub) viewSubscribe(ctx context.Context, p perform, args map[string]any) (any, error) {
	v, err := h.readCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	// Subscription is the attachment itself: view.changed goes to the attached client.
	return h.snapshotOf(v), nil
}

func (h *Hub) viewGet(ctx context.Context, p perform, args map[string]any) (any, error) {
	v, err := h.readCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	return h.snapshotOf(v), nil
}

// viewHistory handles hub.view.history {view_id, attachment, before}: a page of messages older than
// the message ID before, from the plugin (view.history). Only the view's renderers call it: the
// transcript pane fetches the history that a view tree cannot hold (rex-protocol.md: 64 KiB).
//
// A hub handler must never wait for a plugin: Rex sends the hub its next request only after the
// hub answers this one, and the plugin's own calls to the hub wait in that queue (rex-protocol.md,
// "The hub has one request queue"). So the first call starts the fetch and answers
// {status: "pending"}; the renderer asks again until the page is there.
func (h *Hub) viewHistory(ctx context.Context, p perform, args map[string]any) (any, error) {
	v, err := h.readCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	before, _ := args["before"].(string)
	if before == "" {
		return nil, bad("invalid_input: before is required")
	}
	key := v.ID + "\x00" + before
	h.vs.mu.Lock()
	defer h.vs.mu.Unlock()
	if f := h.vs.history[key]; f != nil {
		if !f.done {
			return map[string]any{"status": "pending"}, nil
		}
		delete(h.vs.history, key)
		if f.err != nil {
			return nil, f.err
		}
		return f.out, nil
	}
	for k, f := range h.vs.history {
		if time.Since(f.started) > time.Minute {
			delete(h.vs.history, k) // a renderer that stopped asking
		}
	}
	f := &historyFetch{started: time.Now()}
	h.vs.history[key] = f
	work := viewArgs(v.ID, v.Route, v.Context)
	work["before"] = before
	go func() {
		var out map[string]any
		err := h.callPlugin(context.Background(), v.Plugin, "view.history", work, &out)
		if err == nil {
			normalizeTree(out) // Rex Lua encodes an empty message list as {}
			out["status"] = "ready"
			more, _ := out["has_more"].(bool)
			log.Printf("views %s: history page for %s: %d messages, more %v", v.Plugin, v.Route, len(asSlice(out["messages"])), more)
		}
		h.vs.mu.Lock()
		f.done, f.out, f.err = true, out, err
		h.vs.mu.Unlock()
	}()
	return map[string]any{"status": "pending"}, nil
}

// viewAdmit records an operation (action, draft save or send) and returns at once.
func (h *Hub) viewAdmit(ctx context.Context, kind string, p perform, args map[string]any) (any, error) {
	v, err := h.boundCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	key, _ := args["operation_key"].(string)
	node, _ := args["node_id"].(string)
	if key == "" || node == "" {
		return nil, bad("invalid_input: operation_key and node_id are required")
	}
	// The plugin gets the same arguments on every attempt. The attachment is not part of them.
	// The hub's restart form reads its stored context when the operation runs (restartform.go), so
	// the context is not part of its arguments: a Refresh can change it while an operation waits.
	vctx := v.Context
	if v.Plugin == restartFormOwner {
		vctx = ""
	}
	work := viewArgs(v.ID, v.Route, vctx)
	work["node_id"], work["operation_key"] = node, key
	switch kind {
	case "action":
		action, _ := args["action"].(string)
		if action == "" {
			return nil, bad("invalid_input: action is required")
		}
		input, _ := args["input"].(map[string]any)
		if input == nil {
			input = map[string]any{}
		}
		work["action"], work["input"], work["revision"] = action, input, args["revision"]
	case "draft":
		work["base_revision"], work["text"] = args["base_revision"], args["text"]
	case "send":
		work["draft_revision"], work["text"] = args["draft_revision"], args["text"]
	}
	if _, ok := work["text"]; ok {
		if _, isText := work["text"].(string); !isText {
			return nil, bad("invalid_input: text must be a string")
		}
	}
	b, _ := json.Marshal(work)
	fingerprint := kind + "/" + string(b) // json.Marshal sorts map keys
	var existing string
	err = h.db.QueryRowContext(ctx, `SELECT fingerprint FROM view_ops WHERE plugin = ? AND user = ? AND op_key = ?`, v.Plugin, viewUser, key).Scan(&existing)
	switch {
	case err == nil && existing != fingerprint:
		return nil, bad("operation_conflict: key %q has different arguments", key)
	case err == nil:
		// A retry: the first admission stands. Its current state answers, as hub.view.operation
		// does, so a renderer that lost the first reply cannot finish early.
		return h.opState(ctx, v.Plugin, key)
	case errors.Is(err, sql.ErrNoRows):
		if _, err := h.db.ExecContext(ctx, `INSERT INTO view_ops (plugin, user, op_key, view_id, kind, fingerprint, args, status) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending')`,
			v.Plugin, viewUser, key, v.ID, kind, fingerprint, string(b)); err != nil {
			return nil, err
		}
		h.kickViews(v.Plugin, 0)
	default:
		return nil, err
	}
	return map[string]any{"status": "accepted", "operation_key": key}, nil
}

func (h *Hub) viewOperation(ctx context.Context, p perform, args map[string]any) (any, error) {
	v, err := h.boundCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	key, _ := args["operation_key"].(string)
	return h.opState(ctx, v.Plugin, key)
}

// opState reports an operation: status, its result or error, and the state of its UI effects
// (navigate, form, close_form): pending, done or failed, with the error.
func (h *Hub) opState(ctx context.Context, plugin, key string) (any, error) {
	var status, result, effect, effectErr string
	err := h.db.QueryRowContext(ctx, `SELECT status, result, effect_state, effect_error FROM view_ops WHERE plugin = ? AND user = ? AND op_key = ?`, plugin, viewUser, key).
		Scan(&status, &result, &effect, &effectErr)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{"status": "not_found", "operation_key": key}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{"status": status, "operation_key": key}
	if effect != "" {
		out["effects"] = effect
		if effectErr != "" {
			out["effects_error"] = effectErr
		}
	}
	switch status {
	case "succeeded":
		out["result"] = json.RawMessage(result)
	case "failed":
		out["error"] = json.RawMessage(result)
	}
	return out, nil
}

// viewEffectRetry handles hub.view.effect.retry {view_id, attachment, operation_key}: the
// renderer's Retry after failed effects. Only failed effects of an operation of the same view go
// back to pending. The plugin mutation never runs again.
func (h *Hub) viewEffectRetry(ctx context.Context, p perform, args map[string]any) (any, error) {
	v, err := h.boundCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	key, _ := args["operation_key"].(string)
	res, err := h.db.ExecContext(ctx, `UPDATE view_ops SET effect_state = 'pending', effect_error = '' WHERE plugin = ? AND user = ? AND op_key = ?
		AND view_id = ? AND status = 'succeeded' AND effect_state = 'failed'`, v.Plugin, viewUser, key, v.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, bad("invalid_state: operation %q of this view has no failed effects", key)
	}
	h.kickViews(v.Plugin, 0)
	return h.opState(ctx, v.Plugin, key)
}

// --- Worker side: plugin calls ---

func (h *Hub) pluginClient(plugin string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pl := h.plugins[plugin]; pl != nil {
		return pl.ClientID
	}
	return ""
}

// callPlugin calls a plugin action and waits as long as ctx allows: the view worker's calls have no
// deadline, by design (docs/hub.md "Hub calls to clients"). The worker is serial, and runOps
// retries an unknown outcome, so a deadline would only add queued calls to a plugin that does not
// take its queue.
func (h *Hub) callPlugin(ctx context.Context, plugin, action string, args any, out any) error {
	if plugin == restartFormOwner {
		return h.restartFormCall(ctx, action, args, out) // the hub's own form (restartform.go)
	}
	client := h.pluginClient(plugin)
	if client == "" {
		return &rex.RexError{Code: "client_unavailable", Message: "plugin " + plugin + " is not registered"}
	}
	raw, err := h.performPlugin(ctx, client, action, args, 10000)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// definiteFailure reports a plugin error answer, as opposed to an unknown outcome.
func definiteFailure(err error) (*rex.RexError, bool) {
	var re *rex.RexError
	if !errors.As(err, &re) {
		return nil, false
	}
	switch strings.ToLower(re.Code) {
	case "disconnected", "client_unavailable", "timeout", "deadline_exceeded", "unavailable", "outcome_unknown":
		return nil, false
	}
	return re, true
}

// runOps runs pending operations in admission order. An unknown outcome leaves the operation
// pending: the plugin deduplicates by operation key, so a retry is safe. A view.action result
// with status "running" also stays pending. It returns true to retry with the stored arguments.
func (h *Hub) runOps(ctx context.Context, plugin string) bool {
	// Unfinished effects first, in admission order: a retry, or a restart after a crash.
	rows, err := h.db.QueryContext(ctx, `SELECT op_key, view_id, result FROM view_ops WHERE plugin = ? AND status = 'succeeded' AND effect_state = 'pending' ORDER BY rowid`, plugin)
	if err != nil {
		log.Printf("views %s: %v", plugin, err)
		return true // keep the order: no new operation runs before older effects finish
	}
	{
		type pend struct{ key, view, result string }
		var ps []pend
		for rows.Next() {
			var p pend
			if err = rows.Scan(&p.key, &p.view, &p.result); err != nil {
				break
			}
			ps = append(ps, p)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			log.Printf("views %s: pending effects: %v", plugin, err)
			return true // an unread older effect must not be overtaken
		}
		for _, p := range ps {
			if h.applyEffects(ctx, plugin, p.key, p.view, json.RawMessage(p.result)) {
				return true
			}
		}
	}
	rows, err = h.db.QueryContext(ctx, `SELECT op_key, view_id, kind, args FROM view_ops WHERE plugin = ? AND status = 'pending' ORDER BY rowid`, plugin)
	if err != nil {
		log.Printf("views %s: %v", plugin, err)
		return true
	}
	type op struct{ key, view, kind, args string }
	var ops []op
	for rows.Next() {
		var o op
		if err = rows.Scan(&o.key, &o.view, &o.kind, &o.args); err != nil {
			break
		}
		ops = append(ops, o)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		log.Printf("views %s: pending operations: %v", plugin, err)
		return true // run none rather than skip one out of order
	}
	for _, o := range ops {
		action := map[string]string{"action": "view.action", "draft": "view.draft.save", "send": "view.send"}[o.kind]
		var result json.RawMessage
		err := h.callPlugin(ctx, plugin, action, json.RawMessage(o.args), &result)
		status, stored := "succeeded", string(result)
		if err != nil {
			re, definite := definiteFailure(err)
			if !definite {
				log.Printf("views %s: %s %s: outcome unknown: %v", plugin, o.kind, o.key, err)
				return true
			}
			b, _ := json.Marshal(map[string]string{"code": re.Code, "message": re.Message})
			status, stored = "failed", string(b)
		}
		if err == nil && o.kind == "action" {
			var progress struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(result, &progress) == nil && progress.Status == "running" {
				// The plugin owns the mutation result. Do not commit success, run effects, or
				// let a later operation pass it before that result is final.
				return true
			}
		}
		if len(stored) == 0 {
			stored = "{}"
		}
		// The effects are recorded with the result, in one write: a crash after it runs them at the
		// next start (applyEffects).
		// A malformed effect is pending too: applyEffects then records it as failed.
		effect := ""
		if status == "succeeded" && o.kind == "action" {
			if eff, err := decodeEffects(result); err != nil || eff.any() {
				effect = "pending"
			}
		}
		if _, err := h.db.ExecContext(ctx, `UPDATE view_ops SET status = ?, result = ?, effect_state = ? WHERE plugin = ? AND user = ? AND op_key = ?`,
			status, stored, effect, plugin, viewUser, o.key); err != nil {
			// Not recorded: the operation stays pending and runs again (the plugin deduplicates
			// by operation key). No effect runs before its record exists.
			log.Printf("views %s: record %s: %v", plugin, o.key, err)
			return true
		}
		h.ev.emit("view_op", map[string]any{"plugin": plugin, "view_id": o.view, "kind": o.kind, "op_key": o.key, "status": status})
		// The renderer learns about completion through view.changed, even when the tree is unchanged.
		h.vs.mu.Lock()
		h.vs.notify[o.view] = true
		h.vs.stateLocked(plugin, o.view).dirty |= causeOperation
		h.vs.dirty[plugin] = true
		h.vs.mu.Unlock()
		if effect != "" && h.applyEffects(ctx, plugin, o.key, o.view, result) {
			return true
		}
	}
	return false
}

// viewArgs are the arguments that every view.render, view.action, view.draft.save and view.send
// call starts with. view_id lets a plugin keep state for one view, for example a form's draft.
func viewArgs(viewID, route, context string) map[string]any {
	args := map[string]any{"view_id": viewID, "route": route, "user": viewUser}
	if context != "" {
		args["context"] = json.RawMessage(context) // a form's stored context
	}
	return args
}

// viewRenderHook replaces the plugin's view.render call (tests only).
var viewRenderHook func(h *Hub, ctx context.Context, plugin string, args map[string]any, out any) error

// renderViews renders every open, attached view of the plugin and sends view.changed when a
// tree changed or an operation of that view completed.
// renderPruneHook runs between the target read and the prune of a pass (tests only).
var renderPruneHook func()

func (h *Hub) renderViews(ctx context.Context, plugin string) {
	rows, err := h.db.QueryContext(ctx, `SELECT v.view_id, v.route, v.client_id, v.attachment, v.context, COALESCE(t.status, ''), `+effectiveLayout+`
		FROM views v LEFT JOIN ui_tabs t ON t.plugin = v.plugin AND t.tab = v.tab LEFT JOIN view_transcripts x ON x.view_id = v.view_id
		WHERE v.plugin = ? AND v.status = 'open' AND v.attachment != ''`, plugin)
	if err != nil {
		return
	}
	type target struct{ id, route, client, attachment, context, status, layout string }
	var targets []target
	for rows.Next() {
		var t target
		rows.Scan(&t.id, &t.route, &t.client, &t.attachment, &t.context, &t.status, &t.layout)
		targets = append(targets, t)
	}
	rows.Close()
	printers := h.transcriptTargets(ctx, plugin)
	composers := map[string]string{}
	for _, t := range targets {
		composers[t.id] = t.attachment
	}
	h.vs.pruneNotifiers(plugin, composers, printers)
	if renderPruneHook != nil {
		renderPruneHook()
	}
	// The causes for every view, and for a view that has no state yet, are taken once for the pass.
	// The state of a view that is not open and attached now is deleted (renderdeps.go), but only
	// with no causes: a view that attached after the target read (or asked for a snapshot) keeps
	// its causes for the next pass, which its mark already asked for (Codex review of 1ddb557). A
	// close calls forgetView, so a kept state does not outlive its view.
	h.vs.mu.Lock()
	all, loose := h.vs.all[plugin], h.vs.loose[plugin]
	delete(h.vs.all, plugin)
	delete(h.vs.loose, plugin)
	open := map[string]bool{}
	for _, t := range targets {
		open[t.id] = true
	}
	for id, st := range h.vs.views[plugin] {
		if !open[id] && !st.inFlight && st.dirty == 0 {
			delete(h.vs.views[plugin], id)
		}
	}
	h.vs.mu.Unlock()
	for _, t := range targets {
		h.vs.mu.Lock()
		_, known := h.vs.views[plugin][t.id]
		st := h.vs.stateLocked(plugin, t.id)
		causes := all | st.dirty
		if !known {
			causes |= loose
		}
		if causes == 0 {
			h.vs.mu.Unlock()
			continue // nothing that this view depends on changed
		}
		st.dirty, st.inFlight = 0, true
		epoch := h.vs.epoch[plugin]
		h.vs.mu.Unlock()
		var out struct {
			Tree    map[string]any  `json:"tree"`
			Deps    json.RawMessage `json:"deps"`
			Refresh json.RawMessage `json:"refresh"` // absent: this render is tracked; "all": broad (renderreads.go)
		}
		gen := h.testRenderStart(t.id) // test mode: the requested render that this one covers
		h.testRenderAttempt(plugin, t.id, t.route, causes)
		args := viewArgs(t.id, t.route, t.context)
		rid := h.openRenderID(plugin, h.pluginClient(plugin), t.id, epoch)
		args["render"] = rid
		var err error
		t0 := time.Now()
		if viewRenderHook != nil {
			err = viewRenderHook(h, ctx, plugin, args, &out)
		} else {
			err = h.callPlugin(ctx, plugin, "view.render", args, &out)
		}
		rec := h.closeRenderID(rid)
		h.testRenderCallTime(t.id, time.Since(t0))
		h.testRenderHold(plugin, t.route) // test mode: a view-render fault holds the result here
		if _, definite := definiteFailure(err); err != nil && !definite {
			// The plugin is not there yet or did not answer. Keep the current tree; plugin
			// registration renders again.
			log.Printf("views %s: render %s: %v (transient)", plugin, t.route, err)
			h.renderFinished(plugin, t.id, t.route, st, epoch, nil, false, "", nil)
			h.testRenderOutcome(t.id, "transient")
			h.testRenderDone(t.id, gen, false, "transient: "+err.Error()+" (the hub kept the previous tree)", 0, nil)
			continue
		}
		renderErr := ""
		if err != nil {
			renderErr = err.Error()
		} else if out.Tree == nil {
			renderErr = "the plugin returned no tree"
		}
		if err != nil || out.Tree == nil {
			tree := map[string]any{"title": "Error", "back": nil, "nodes": []any{map[string]any{"id": "$error", "type": "error", "text": "The plugin could not render this view.", "retry_action": nil}}}
			if err != nil {
				log.Printf("views %s: render %s: %v", plugin, t.route, err)
			}
			out.Tree = tree
		}
		status := t.status // declared; a rendered tree.status overrides it
		if st := liftTreeStatus(out.Tree); st != "" {
			status = st
		}
		normalizeTree(out.Tree)
		// Lua drops nil fields. The contract requires these keys, with null.
		if _, ok := out.Tree["back"]; !ok {
			out.Tree["back"] = nil
		}
		for _, n := range asSlice(out.Tree["nodes"]) {
			if m, ok := n.(map[string]any); ok && m["type"] == "history" {
				if _, ok := m["before"]; !ok {
					m["before"] = nil
				}
			}
		}
		if renderErr == "" {
			h.parityLog(plugin, t.route, out.Tree)
			h.testRecordTree(plugin, t.route, out.Tree)
			// The refresh value is checked for every good render, before the untracked and
			// empty-read cases, so an invalid value always gives the same error.
			tracked, rerr := parseRefresh(out.Refresh)
			deps, derr := parseDeps(out.Deps)
			var errs []string
			for _, err := range []error{derr, rerr} {
				if err != nil {
					errs = append(errs, err.Error())
				}
			}
			h.renderFinished(plugin, t.id, t.route, st, epoch, deps, derr == nil, strings.Join(errs, "; "), renderTables(rec, tracked))
			h.testRenderOutcome(t.id, "ok")
		} else {
			h.renderFinished(plugin, t.id, t.route, st, epoch, nil, false, "", nil)
			h.testRenderOutcome(t.id, "failed")
		}
		tree, _ := json.Marshal(out.Tree)
		h.vs.mu.Lock()
		s := h.vs.snaps[t.id]
		changed := s == nil || string(s.tree) != string(tree) || s.status != status || s.layout != t.layout
		if changed {
			h.vs.rev++
			s = &snapshot{rev: h.vs.rev, tree: tree, status: status, layout: t.layout}
			h.vs.snaps[t.id] = s
		}
		send := changed || h.vs.notify[t.id]
		delete(h.vs.notify, t.id)
		rev := s.rev
		h.vs.mu.Unlock()
		h.testRenderDone(t.id, gen, renderErr == "", renderErr, rev, tree)
		if send {
			h.tellViewChanged(plugin, roleComposer, t.client, t.id, t.attachment, rev)
			if pr, ok := printers[t.id]; ok {
				h.tellViewChanged(plugin, rolePrinter, pr[0], t.id, pr[1], rev)
			}
		}
	}
}

// composerAttachment is the view's current attachment, for the stale check of view.changed.
func (h *Hub) composerAttachment(ctx context.Context, viewID string) string {
	var att string
	h.db.QueryRowContext(ctx, `SELECT attachment FROM views WHERE view_id = ? AND status = 'open'`, viewID).Scan(&att)
	return att
}

// listFields are the view-tree fields that must be JSON arrays. Rex Lua encodes an empty table
// as {} (rex-protocol.md), so the hub turns an empty object in these fields into [].
var listFields = map[string]bool{"nodes": true, "items": true, "messages": true, "fields": true, "options": true}

func normalizeTree(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if m, ok := child.(map[string]any); ok && listFields[k] && len(m) == 0 {
				t[k] = []any{}
				continue
			}
			normalizeTree(child)
		}
	case []any:
		for _, child := range t {
			normalizeTree(child)
		}
	}
}

func asSlice(v any) []any {
	l, _ := v.([]any)
	return l
}
