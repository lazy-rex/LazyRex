package main

import (
	"context"
	"encoding/json"
	"os"
	"sort"
)

// caller is who may call a hub action through h.dispatch (the hub's own client, lazyrex-hub).
type caller int

const (
	callerAny      caller = iota // any client of lazyrex-hub
	callerOperator               // the admin guard: refused from agent and plugin panes, and while a relaunch runs
	callerPlugin                 // a registered plugin instance only; a home check stays in its run
	callerTest                   // hub.test.*: test mode only; testDispatch runs it
)

// hubCall is one call of a hub action: the request, its decoded args (without render for a covered
// read), the calling plugin instance (callerPlugin), and degraded, which hub.workers sets for the
// covered-read outcome.
type hubCall struct {
	p        perform
	args     map[string]any
	pl       *Plugin
	degraded bool
}

// hubAction is one row of the action table: the hub's own actions on both faces.
type hubAction struct {
	name       string
	caller     caller
	plugin     bool                                                       // on the plugin face (the keymap of lazyrex-hub)
	pluginBare bool                                                       // the plugin face declares the name only, although desc is set
	agentOrder int                                                        // the place on the agent face (lazyrex), from 1; 0: not on it
	agentHub   bool                                                       // in agentHubActions, which tool parity reads (testmode.go)
	opRoute    bool                                                       // agentDispatch sends it to pluginOpAgent first (pluginops.go)
	desc       map[string]any                                             // the full descriptor; nil: {name, kind: client, title: name}
	run        func(h *Hub, ctx context.Context, c *hubCall) (any, error) // nil: no h.dispatch case (testDispatch, the agent face only)
}

// hubActions is the action table, in the order of the plugin face. Built in init: a run refers to
// hub methods that reach the table again (agentKeymap).
var hubActions []hubAction

// agentHubActions are the hub's own actions that tool parity finds on the agent face (agentHub).
var agentHubActions map[string]bool

func init() {
	hubActions = []hubAction{
		{name: "hub.register", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.register(ctx, c.p, c.args)
		}},
		{name: "hub.identify", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			target, _ := c.args["pid"].(float64)
			return h.identify(ctx, int(target))
		}},
		{name: "hub.agents", plugin: true, pluginBare: true, agentOrder: 6, agentHub: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.agentsList(c.args), nil
		}, desc: map[string]any{"name": "hub.agents", "kind": "client", "title": "List agents",
			"description": "List the agents you can message: \"user\" (the human) and each conversation (id \"<kind>:<conversation id>\", name = its tab label, state current, detached or superseded). Set all=true to include superseded conversations and the agent spawns that wait for their first report (id \"spawn:<spawn_id>\", state starting). Each entry has session_id, the Rex session of its pane (empty when unknown); set session_id to list only that session's agents.",
			"args": map[string]any{"type": "object", "properties": map[string]any{"all": map[string]any{"type": "boolean"},
				"session_id": map[string]any{"type": "string"}}, "additionalProperties": false}}},
		{name: "hub.agents.resolve", plugin: true, pluginBare: true, agentOrder: 7, agentHub: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.resolveAgent(ctx, c.args)
		}, desc: map[string]any{"name": "hub.agents.resolve", "kind": "client", "title": "Resolve an agent",
			"description": "Turn a name into an agent ID: an exact ID, an agent spawn address (\"spawn:<spawn_id>\"), a unique ID prefix (\"claude:7a18\"), or a unique tab name. An ambiguous name lists the candidates.",
			"args": map[string]any{"type": "object", "properties": map[string]any{"to": map[string]any{"type": "string", "minLength": 1}},
				"required": []string{"to"}, "additionalProperties": false}}},
		{name: "hub.admin.start", caller: callerOperator, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.adminAction(ctx, c.p.Action, c.args)
		}},
		{name: "hub.admin.stop", caller: callerOperator, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.adminAction(ctx, c.p.Action, c.args)
		}},
		{name: "hub.admin.replace", caller: callerOperator, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.adminAction(ctx, c.p.Action, c.args)
		}},
		{name: "hub.admin.shutdown", caller: callerOperator, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			// The stop itself starts in answer, after the reply.
			if len(c.args) > 0 {
				return nil, bad("hub.admin.shutdown takes no arguments")
			}
			return map[string]any{"stopping": true}, nil
		}},
		{name: "hub.admin.relaunch", caller: callerOperator, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.adminRelaunch(c.args)
		}},
		{name: "hub.admin.resume", caller: callerOperator, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.adminResume(ctx, c.args)
		}},
		{name: "hub.admin.restart", caller: callerOperator, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.adminRestart(ctx, c.args)
		}},
		{name: "hub.admin.restart.form", caller: callerOperator, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.adminRestartForm(ctx, c.p, c.args)
		}},
		{name: "hub.info", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			if len(c.args) > 0 {
				return nil, bad("hub.info takes no arguments")
			}
			commit, modified := buildCommit()
			return map[string]any{"state": h.cfg.State, "pid": os.Getpid(), "generation": revString(h.generation),
				"exec": h.exec, "commit": commit, "modified": modified, "sdk": h.cfg.SDK, "view": h.cfg.View}, nil
		}},
		{name: "hub.ui.tabs", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.uiTabs(ctx, c.pl.Name, c.args)
		}},
		{name: "hub.ui.navigate", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.uiNavigate(ctx, c.args)
		}},
		{name: "hub.ui.reload", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.uiReload(ctx, c.args)
		}},
		{name: "hub.ui.describe", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.uiDescribe(ctx, c.args)
		}},
		{name: "hub.view.attach", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewAttach(ctx, c.p, c.args)
		}},
		{name: "hub.view.subscribe", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewSubscribe(ctx, c.p, c.args)
		}},
		{name: "hub.view.get", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewGet(ctx, c.p, c.args)
		}},
		{name: "hub.view.history", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewHistory(ctx, c.p, c.args)
		}},
		{name: "hub.view.action", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewAdmit(ctx, "action", c.p, c.args)
		}},
		{name: "hub.view.draft.save", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewAdmit(ctx, "draft", c.p, c.args)
		}},
		{name: "hub.view.send", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewAdmit(ctx, "send", c.p, c.args)
		}},
		{name: "hub.view.operation", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewOperation(ctx, c.p, c.args)
		}},
		{name: "hub.view.effect.retry", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewEffectRetry(ctx, c.p, c.args)
		}},
		{name: "hub.view.seen", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewSeen(ctx, c.p, c.args)
		}},
		{name: "hub.view.close", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewClose(ctx, c.p, c.args)
		}},
		{name: "hub.session.list", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.sessionList(ctx)
		}},
		{name: "hub.session.ensure", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.sessionEnsure(ctx, c.pl.Name, c.args)
		}},
		{name: "hub.plugins", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.pluginsList(ctx)
		}},
		{name: "hub.plugins.reload", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.reloadAction(ctx, c.p)
		}},
		{name: "hub.plugins.ops", caller: callerPlugin, plugin: true, pluginBare: true, agentOrder: 9, opRoute: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			// The plugins panel of the home plugin reads the plugin manager's ops (pluginops.go).
			if !h.isHome(c.pl) {
				return nil, bad("only the home plugin reads the plugin operations")
			}
			var raw map[string]json.RawMessage
			if len(c.p.Args) > 0 && string(c.p.Args) != "null" {
				if err := json.Unmarshal(c.p.Args, &raw); err != nil {
					return nil, bad("args must be an object")
				}
			}
			return h.pluginOpsList(ctx, raw, c.pl.Name)
		}, desc: map[string]any{"name": "hub.plugins.ops", "kind": "client", "title": "List plugin operations",
			"description": "The plugin operations: one by op_id, or the 50 newest. Each has op, entry, origin, phase (queued, a working phase, then done, failed, conflict or unknown), result and error.",
			"args":        map[string]any{"type": "object", "properties": map[string]any{"op_id": map[string]any{"type": "string", "minLength": 1}}, "additionalProperties": false}}},
		{name: "hub.plugins.tags", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			// The plugin manager views of the home plugin read the background loads (pluginloads.go).
			if !h.isHome(c.pl) {
				return nil, bad("only the home plugin reads the plugin tags and logs")
			}
			return h.pluginTags(c.p.Args)
		}},
		{name: "hub.plugins.log", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			if !h.isHome(c.pl) {
				return nil, bad("only the home plugin reads the plugin tags and logs")
			}
			return h.pluginLog(c.p.Args)
		}},
		{name: "hub.digest", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.digest(c.p.Requester.ClientID, c.args)
		}},
		{name: "hub.navigate.worker", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.navigateWorker(ctx, c.args)
		}},
		{name: "hub.navigate.agent", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.navigateAgent(ctx, c.args)
		}},
		{name: "hub.cwd.check", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.cwdCheck(ctx, c.args)
		}},
		{name: "hub.cwd.lock", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.cwdLock(ctx, c.pl.Name, c.args)
		}},
		{name: "hub.cwd.unlock", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.cwdUnlock(ctx, c.pl.Name, false, c.args)
		}},
		{name: "hub.admin.unlock", caller: callerOperator, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.cwdUnlock(ctx, "", true, c.args)
		}},
		{name: "hub.ui.invalidate", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			// The plugin's own data changed outside the store (for example a scan finished): render its
			// open views again. Fast; the view worker does the render.
			if h.test != nil {
				h.test.mu.Lock()
				h.test.invalidate[c.pl.Name]++
				h.test.mu.Unlock()
			}
			return h.uiInvalidate(ctx, c.pl.Name, c.args)
		}},
		{name: "hub.identity.report", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.identityReport(ctx, c.p, c.args)
		}},
		{name: "hub.identity.list", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.identityList(ctx, c.args)
		}},
		{name: "hub.palette.run", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.paletteRun(ctx, c.p, c.args)
		}},
		{name: "hub.view.release_confirm", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.releaseConfirm(ctx, c.p, c.args)
		}},
		{name: "hub.view.replacement", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewReplacement(ctx, c.p, c.args)
		}},
		{name: "hub.view.app_action", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.viewAppAction(ctx, c.p, c.args)
		}},
		{name: "hub.app.select_session", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.appSelectSession(ctx, c.p, c.args)
		}},
		{name: "hub.app.remote_control", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			if len(c.args) > 0 {
				return nil, bad("hub.app.remote_control takes no arguments")
			}
			return h.remoteControlRead(ctx)
		}},
		{name: "hub.spawn_window", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.spawnWindow(ctx, c.p, c.args)
		}},
		{name: "hub.spawn.spec", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.spawnSpec(ctx, c.p, c.args)
		}},
		{name: "hub.grant.create", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.grantCreate(ctx, c.p, c.pl.Name, c.args)
		}},
		{name: "hub.grant.get", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.grantGet(ctx, c.pl.Name, c.args)
		}},
		{name: "hub.grant.revoke", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.grantRevoke(ctx, c.pl.Name, c.args)
		}},
		{name: "hub.watch.ensure", caller: callerPlugin, plugin: true, run: runWatch},
		{name: "hub.watch.read", caller: callerPlugin, plugin: true, run: runWatch},
		{name: "hub.watch.ack", caller: callerPlugin, plugin: true, run: runWatch},
		{name: "hub.watch.remove", caller: callerPlugin, plugin: true, run: runWatch},
		{name: "hub.watch.list", caller: callerPlugin, plugin: true, run: runWatch},
		{name: "hub.prompts", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.promptsList(c.pl)
		}},
		{name: "hub.prompt.answer", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.promptAnswer(ctx, c.pl, c.args)
		}},
		{name: "hub.sql.begin", caller: callerPlugin, plugin: true, run: runSQL},
		{name: "hub.sql.exec", caller: callerPlugin, plugin: true, run: runSQL},
		{name: "hub.sql.query", caller: callerPlugin, plugin: true, run: runSQL},
		{name: "hub.sql.batch", caller: callerPlugin, plugin: true, run: runSQL},
		{name: "hub.sql.commit", caller: callerPlugin, plugin: true, run: runSQL},
		{name: "hub.sql.rollback", caller: callerPlugin, plugin: true, run: runSQL},
		{name: "hub.sql.replay", caller: callerPlugin, plugin: true, run: runSQL},
		{name: "hub.reply.part", caller: callerPlugin, plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.parts.part(c.p.Requester.ClientID, c.args)
		}},
		{name: "hub.peer.send", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.peerSend(ctx, c.p, c.args)
		}},
		{name: "hub.peer.pull", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.peerPullServe(ctx, c.args)
		}},
		{name: "hub.peer.nudge", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.peerNudge(c.args)
		}},
		{name: "hub.peer.plugins", plugin: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.peerPluginsServe(c.args)
		}},
		{name: "hub.peer.status", plugin: true, agentOrder: 1, agentHub: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.peerStatus(ctx)
		}, desc: map[string]any{"name": "hub.peer.status", "kind": "client", "title": "Peer hubs",
			"description": "List this hub's name and its peer hubs: for each, its server, the link state (up, down or unknown), the last success and error, the envelopes from this hub that it has not acked (outbox, oldest_age_s), the cursor of what this hub took from it (after), the identity of its outbox that the cursor belongs to (outbox_id; a new one resets the cursor) and whether it holds more. A remote agent's address is \"<agent>@<hub>\".",
			"args":        map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}},
		{name: "hub.profiles", plugin: true, agentOrder: 2, agentHub: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			h.checkProfiles() // no loop: a second check finds the same files
			return h.profilesList(ctx)
		}, desc: map[string]any{"name": "hub.profiles", "kind": "client", "title": "List profiles",
			"description": "List the named commands (profiles) that can start in a new tab, with their kind (agent or accessory), their default cwd (empty: the home directory) and how many run now. args lists the named values that hub.spawn accepts for the profile (name, label, default, required).",
			"args":        map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}},
		{name: "hub.spawn", plugin: true, agentOrder: 3, agentHub: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.spawnWorker(ctx, c.p, c.args)
		}, desc: map[string]any{"name": "hub.spawn", "kind": "client", "title": "Start a profile",
			"description": "Start a profile in a new tab, beside a block, or detached, in the chosen session. An agent profile gets its own agent ID, so it can message you. Choose a key; after an unknown outcome, retry with the same key. Returns spawn_id, session_id, status (open, pending, failed or closed) and agent_id.",
			"args": map[string]any{"type": "object", "properties": map[string]any{
				"profile": map[string]any{"type": "string", "minLength": 1}, "key": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
				"title": map[string]any{"type": "string", "maxLength": 60},
				"cwd":   map[string]any{"type": "string", "minLength": 1, "description": "An existing absolute directory to start in, for example a worktree from worktree_create. Replaces the profile's cwd."},
				"placement": map[string]any{"type": "object", "additionalProperties": false, "description": "Default: a new tab in the work session. Choose another session, split beside a placed block, or start detached.", "properties": map[string]any{
					"session":   map[string]any{"type": "string", "minLength": 1, "description": "Target session ID."},
					"beside":    map[string]any{"type": "string", "minLength": 1, "description": "A placed block ID in the target session."},
					"direction": map[string]any{"type": "string", "enum": []string{"right", "left", "above", "below"}, "description": "Requires beside. Default: right."},
					"ratio":     map[string]any{"type": "number", "exclusiveMinimum": 0, "exclusiveMaximum": 1, "description": "Requires beside. Fraction for the new pane. Default: 0.5."},
					"detached":  map[string]any{"type": "boolean", "description": "Start without a tab. Cannot be combined with beside."},
					"focus":     spawnFocusSchema(),
				}},
				"args": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string", "maxLength": 16000},
					"description": "Named values for the profile, for example {\"task\": \"Fix the login test\"}. hub.profiles lists each profile's args."},
				"for": map[string]any{"type": "string"}, "on_behalf": map[string]any{"type": "integer"}},
				"required": []string{"profile", "key"}, "additionalProperties": false}}},
		{name: "hub.workers", plugin: true, agentOrder: 4, agentHub: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			spawnID, _ := c.args["spawn_id"].(string)
			result, degraded, err := h.workersRead(ctx, spawnID)
			c.degraded = degraded
			return result, err
		}, desc: map[string]any{"name": "hub.workers", "kind": "client", "title": "List workers",
			"description": "List started profiles that are not closed: spawn_id, session_id, cwd, profile, title, parent, agent_id, state (starting, running, exited, failed), error and program status. An agent worker also has identity_seen (true once its pane has had a current conversation identity), and, when open, identity: current or none, with identity_id (the current identity's ID) when current. With spawn_id: only that worker; not_found when it is closed or unknown.",
			"args": map[string]any{"type": "object", "properties": map[string]any{
				"spawn_id": map[string]any{"type": "string", "minLength": 1}}, "additionalProperties": false}}},
		{name: "hub.stop", plugin: true, agentOrder: 5, agentHub: true, run: func(h *Hub, ctx context.Context, c *hubCall) (any, error) {
			return h.stop(ctx, c.p, c.args)
		}, desc: map[string]any{"name": "hub.stop", "kind": "client", "title": "Stop a worker",
			"description": "Close a worker's tab, or dismiss a failed start. A pending start with an unknown outcome cannot be stopped. Any agent can stop any worker, also one that another agent or the user started. Safe to retry: stop is idempotent by spawn_id, and key is optional.",
			"args": map[string]any{"type": "object", "properties": map[string]any{
				"spawn_id": map[string]any{"type": "string", "minLength": 1}, "key": map[string]any{"type": "string", "maxLength": 128},
				"for": map[string]any{"type": "string"}, "on_behalf": map[string]any{"type": "integer"}},
				"required": []string{"spawn_id"}, "additionalProperties": false}}},
		{name: "hub.test.status", caller: callerTest, plugin: true},
		{name: "hub.test.sql", caller: callerTest, plugin: true},
		{name: "hub.test.chunk", caller: callerTest, plugin: true},
		{name: "hub.test.notice", caller: callerTest, plugin: true},
		{name: "hub.test.notice_renudge", caller: callerTest, plugin: true},
		{name: "hub.test.stats", caller: callerTest, plugin: true},
		{name: "hub.test.tabs", caller: callerTest, plugin: true},
		{name: "hub.test.views", caller: callerTest, plugin: true},
		{name: "hub.test.operation", caller: callerTest, plugin: true},
		{name: "hub.test.inject", caller: callerTest, plugin: true},
		{name: "hub.test.inject.cancel", caller: callerTest, plugin: true},
		{name: "hub.test.injection", caller: callerTest, plugin: true},
		{name: "hub.test.view.render", caller: callerTest, plugin: true},
		{name: "hub.test.view.result", caller: callerTest, plugin: true},
		{name: "hub.test.parity", caller: callerTest, plugin: true},
		{name: "hub.test.finish", caller: callerTest, plugin: true},
		{name: "hub.test.commands", caller: callerTest, plugin: true},
		{name: "hub.test.events", caller: callerTest, plugin: true},
		{name: "hub.test.app", caller: callerTest, plugin: true},
		{name: "hub.test.app.calls", caller: callerTest, plugin: true},
		{name: "hub.test.calls", caller: callerTest, plugin: true},
		{name: "hub.test.renders", caller: callerTest, plugin: true},
		{name: "hub.test.app.close", caller: callerTest, plugin: true},
		{name: "hub.test.fault", caller: callerTest, plugin: true},
		{name: "hub.test.fault_state", caller: callerTest, plugin: true},
		{name: "hub.test.fault_release", caller: callerTest, plugin: true},
		{name: "hub.test.recover", caller: callerTest, plugin: true},
		{name: "hub.test.peer.deliver", caller: callerTest, plugin: true},
		{name: "hub.test.peer.delivery", caller: callerTest, plugin: true},
		{name: "hub.plugins.op", agentOrder: 8, agentHub: true, opRoute: true, desc: map[string]any{"name": "hub.plugins.op", "kind": "client", "title": "Run a plugin operation",
			"description": "Start, stop, restart, remove, update or change the settings of a plugin entry. restart replaces the running instance with the same run; it is refused (and stops nothing) when the plugin does not run or has a new candidate (use update for new code). entry is \"managed:<id>\" (a managed entry) or the exact source of a config entry; settings and remove change only managed entries. settings takes input {args, watch_roots}; they apply at the next start. settings can also take expect {args, watch_roots}, the values that you read: if the entry has other values now, the hub refuses with conflict. An input equal to the current values ends done with result \"no change\". update (a managed GitHub entry) takes input {to}: \"@\" fetches the entry's own ref again (a branch gives its tip, latest the newest release, a tag the commit the tag names now (no change unless the tag was moved)), \"latest\" moves the entry to the newest release, and a tag name moves it to that tag; an entry already at the target ends done with result \"already at <source> <commit>\". The hub runs the operation in the background: read its phase with hub.plugins.ops op_id=<op_id>. Choose a key; after an unknown outcome, retry with the same key. A start of a GitHub entry runs only the plugin's last run; new GitHub code needs the user's confirm in the plugins panel (user_confirmation_required). The home plugin cannot be stopped or removed.",
			"args": map[string]any{"type": "object", "properties": map[string]any{
				"key":    map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
				"op":     map[string]any{"type": "string", "enum": []string{"start", "stop", "restart", "settings", "remove", "update"}},
				"entry":  map[string]any{"type": "string", "minLength": 1},
				"input":  opInputSchema,
				"expect": settingsSchema,
			}, "required": []string{"key", "op", "entry"}, "additionalProperties": false}}},
	}
	agentHubActions = map[string]bool{}
	for _, a := range hubActions {
		if a.agentHub {
			agentHubActions[a.name] = true
		}
	}
}

// actionNamed returns the table row of name, or nil.
func actionNamed(name string) *hubAction {
	for i := range hubActions {
		if hubActions[i].name == name {
			return &hubActions[i]
		}
	}
	return nil
}

// runSQL is the run of the hub.sql.* actions (sqlsvc.go).
func runSQL(h *Hub, ctx context.Context, c *hubCall) (any, error) {
	return h.sql.dispatch(ctx, c.p.Action, sqlCall{plugin: c.pl.Name, client: c.p.Requester.ClientID, args: c.args})
}

// runWatch is the run of the hub.watch.* actions (watchsvc.go).
func runWatch(h *Hub, ctx context.Context, c *hubCall) (any, error) {
	return h.watchAction(ctx, c.pl.Name, c.p.Action, c.args)
}

// isHome reports whether pl is the home plugin's record.
func (h *Hub) isHome(pl *Plugin) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.homeRecordLocked(pl.Name)
}

// hubClientActions are the actions that the hub declares on its own client (the plugin face): the
// rows with plugin, and the test rows only in test mode. A covered read takes the render ID as well
// (withRenderArg); the agent face declares it without (agentHubFace).
func hubClientActions(test bool) []map[string]any {
	var actions []map[string]any
	for _, a := range hubActions {
		if !a.plugin || (a.caller == callerTest && !test) {
			continue
		}
		switch {
		case a.desc == nil || a.pluginBare:
			actions = append(actions, map[string]any{"name": a.name, "kind": "client", "title": a.name})
		case coveredReads[a.name] != nil:
			actions = append(actions, withRenderArg(a.desc))
		default:
			actions = append(actions, a.desc)
		}
	}
	return actions
}

// agentHubFace is the hub's own part of the agent face, in agentOrder: the shared hub tools that
// agents may call (without on_behalf, agentFaceAction), the list actions, and the plugin manager's
// ops.
func agentHubFace() []map[string]any {
	var rows []hubAction
	for _, a := range hubActions {
		if a.agentOrder > 0 {
			rows = append(rows, a)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].agentOrder < rows[j].agentOrder })
	var actions []map[string]any
	for _, a := range rows {
		actions = append(actions, agentFaceAction(a.desc))
	}
	return actions
}
