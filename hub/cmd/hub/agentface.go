package main

// The agent face: a second Rex client, labeled "lazyrex", that carries only the actions agents may
// call. Agents discover them natively (`rex -C lazyrex actions`, `rex client inspect lazyrex`) and
// call them with `rex -C lazyrex do <action> key=value…`. Rex checks the args against the declared
// schema and attests the caller's pid (observed on Rex 1203, lab). A plugin action is forwarded to
// the plugin as hub.forward {action, args, requester_pid}; the plugin accepts it only from the hub.
//
// The client never attaches a session. Rex forgets an unattached client at once when its connection
// dies (observed on Rex 1203, lab), so a restarted hub never shares the label with a lost client and
// `-C lazyrex` never becomes ambiguous. No reconnect token is needed.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const agentLabel = "lazyrex"

// agentHubActions are the hub's own actions on the agent face. Their descriptors come from
// hubToolActions and agentListActions.
var agentHubActions = map[string]bool{"hub.profiles": true, "hub.spawn": true, "hub.workers": true, "hub.stop": true,
	"hub.agents": true, "hub.agents.resolve": true}

var agentListActions = []map[string]any{
	{"name": "hub.agents", "kind": "client", "title": "List agents",
		"description": "List the agents you can message: \"user\" (the human) and each conversation (id \"<kind>:<conversation id>\", name = its tab label, state current, detached or superseded). Set all=true to include superseded conversations and the legacy names they replaced (alias_of). Each entry has session_id, the Rex session of its pane (empty when unknown); set session_id to list only that session's agents.",
		"args": map[string]any{"type": "object", "properties": map[string]any{"all": map[string]any{"type": "boolean"},
			"session_id": map[string]any{"type": "string"}}, "additionalProperties": false}},
	{"name": "hub.agents.resolve", "kind": "client", "title": "Resolve an agent",
		"description": "Turn a name into an agent ID: an exact ID, a legacy name, a unique ID prefix (\"claude:7a18\"), or a unique tab name. An ambiguous name lists the candidates.",
		"args": map[string]any{"type": "object", "properties": map[string]any{"to": map[string]any{"type": "string", "minLength": 1}},
			"required": []string{"to"}, "additionalProperties": false}},
}

// agentFaceStart connects the agent face. The label is set after the first keymap, so a caller
// that finds the label also finds the actions.
func (h *Hub) agentFaceStart(ctx context.Context, socket string) error {
	r, err := rex.Dial(ctx, socket)
	if err != nil {
		return err
	}
	h.agentRex = r
	h.agentKick = make(chan struct{}, 1)
	go func() {
		for raw := range r.Events {
			var env struct {
				Payload struct {
					Type    string          `json:"type"`
					Payload json.RawMessage `json:"payload"`
				} `json:"payload"`
			}
			if json.Unmarshal(raw, &env) != nil || env.Payload.Type != "client_perform_requested" {
				continue
			}
			var p perform
			if json.Unmarshal(env.Payload.Payload, &p) == nil {
				go h.agentAnswer(ctx, p)
			}
		}
		log.Print("agent face: rex connection closed")
	}()
	if err := h.agentKeymap(ctx); err != nil {
		return err
	}
	if err := r.Call(ctx, "client.set_label", map[string]any{"label": agentLabel}, nil); err != nil {
		return err
	}
	go h.agentKeymapLoop(ctx)
	return nil
}

// kickAgentKeymap declares the agent face's actions again: a plugin registered or went away.
func (h *Hub) kickAgentKeymap() {
	if h.agentKick == nil {
		return
	}
	select {
	case h.agentKick <- struct{}{}:
	default:
	}
}

func (h *Hub) agentKeymapLoop(ctx context.Context) {
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.agentKick:
		case <-retry:
		}
		retry = nil
		if err := h.agentKeymap(ctx); err != nil {
			log.Printf("agent face keymap: %v", err)
			retry = time.After(time.Second)
		}
	}
}

// agentKeymap declares the hub's agent actions and the registered exports of every plugin that
// declares hub.forward, with the description and args schema that the plugin declared to Rex (read
// back with client.inspect).
func (h *Hub) agentKeymap(ctx context.Context) error {
	var actions []map[string]any
	for _, a := range hubToolActions {
		if agentHubActions[a["name"].(string)] {
			actions = append(actions, agentFaceAction(a))
		}
	}
	actions = append(actions, agentListActions...)
	actions = append(actions, pluginOpActions...)
	forward := map[string]string{} // plugin action -> the plugin's client, for agentDispatch
	type owner struct {
		client  string
		exports map[string]bool
	}
	var owners []owner
	names := h.pluginNames()
	h.mu.Lock()
	for _, name := range names {
		p := h.plugins[name]
		if p == nil || p.ClientID == "" {
			continue
		}
		o := owner{client: p.ClientID, exports: map[string]bool{}}
		for _, e := range p.Exports {
			o.exports[e] = true
		}
		owners = append(owners, o)
	}
	h.mu.Unlock()
	for _, o := range owners {
		var out struct {
			Actions []map[string]any `json:"actions"`
		}
		if err := h.rex.Call(ctx, "client.inspect", map[string]any{"client_id": o.client}, &out); err != nil {
			var re *rex.RexError
			if errors.As(err, &re) && re.Code == "not_found" {
				continue // the plugin is restarting; its registration kicks again
			}
			return err
		}
		// Only a plugin that declares hub.forward can take a forwarded call: list nothing else.
		forwards := false
		for _, a := range out.Actions {
			forwards = forwards || a["name"] == "hub.forward"
		}
		if !forwards {
			continue
		}
		for _, a := range out.Actions {
			if name, _ := a["name"].(string); o.exports[name] {
				forward[name] = o.client
				d := map[string]any{"name": name, "kind": "client"}
				for _, k := range []string{"title", "description", "args"} {
					if v, ok := a[k]; ok {
						d[k] = v
					}
				}
				actions = append(actions, d)
			}
		}
	}
	if err := h.agentRex.Call(ctx, "client.keymap", map[string]any{"actions": actions}, nil); err != nil {
		return err
	}
	h.mu.Lock()
	h.agentForward = forward
	h.mu.Unlock()
	return nil
}

// agentAnswer runs one agent call and replies on the agent face.
func (h *Hub) agentAnswer(ctx context.Context, p perform) {
	result, err := h.agentDispatch(ctx, p)
	reply := map[string]any{"call_id": p.CallID}
	if err != nil {
		var se *storeError
		var re *rex.RexError
		switch {
		case errors.As(err, &se):
			reply["error"] = &rex.RexError{Code: "refused", Message: err.Error()}
		case errors.As(err, &re):
			reply["error"] = re // a plugin's own error, as the plugin gave it
		default:
			reply["error"] = &rex.RexError{Code: "failed", Message: err.Error()}
		}
	} else {
		reply["result"] = result
	}
	if err := sendReply(ctx, h.agentRex, p.Action, reply); err != nil { // replyguard.go
		log.Printf("agent reply %s %s: %v", p.Action, p.CallID, err)
	}
	log.Printf("agent %s from pid %d -> %v", p.Action, p.Requester.Principal.PID, err)
}

func (h *Hub) agentDispatch(ctx context.Context, p perform) (any, error) {
	if p.Action == "hub.plugins.op" || p.Action == "hub.plugins.ops" {
		return h.pluginOpAgent(ctx, p) // the agent face only (pluginops.go)
	}
	if agentHubActions[p.Action] {
		return h.dispatch(ctx, p)
	}
	// Dispatch uses the same set that the face declared: registered exports of plugins that declare
	// hub.forward. A plugin that went away or restarted is checked against its current client.
	h.mu.Lock()
	client := h.agentForward[p.Action]
	current := false
	for _, pl := range h.plugins {
		current = current || (client != "" && pl.ClientID == client && contains(pl.Exports, p.Action))
	}
	h.mu.Unlock()
	if !current {
		client = ""
	}
	if client == "" {
		return nil, bad("unknown action %s (run `rex -C %s actions`)", p.Action, agentLabel)
	}
	args := p.Args
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	var res struct {
		Result json.RawMessage `json:"result"`
	}
	h.beginForward(client, p.Requester.Principal.PID)
	defer h.endForward(client, p.Requester.Principal.PID)
	err := h.rex.Call(ctx, "client.perform", map[string]any{"client_id": client, "action": "hub.forward",
		"args": map[string]any{"action": p.Action, "args": args, "requester_pid": p.Requester.Principal.PID}, "timeout_ms": 10000}, &res)
	if err != nil {
		return nil, err
	}
	return res.Result, nil
}

// A plugin that handles a forwarded agent call can act for that agent with on_behalf = the
// forward's requester_pid (the coordinator, 2026-10-19). The hub accepts it only while that
// forward to that plugin client is in flight, so a plugin cannot choose an agent by itself. This
// serves synchronous work only: work that runs after the forward returns needs another proof.
func (h *Hub) beginForward(client string, pid int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.forwards == nil {
		h.forwards = map[string]map[int]int{}
	}
	if h.forwards[client] == nil {
		h.forwards[client] = map[int]int{}
	}
	h.forwards[client][pid]++
}

func (h *Hub) endForward(client string, pid int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.forwards[client][pid]--; h.forwards[client][pid] <= 0 {
		delete(h.forwards[client], pid)
	}
	if len(h.forwards[client]) == 0 {
		delete(h.forwards, client)
	}
}

func (h *Hub) forwarding(client string, pid int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return client != "" && h.forwards[client][pid] > 0
}

// agentFaceAction is a hub tool as agents see it: without on_behalf, which only a plugin that
// handles a forward can use.
func agentFaceAction(a map[string]any) map[string]any {
	args, _ := a["args"].(map[string]any)
	props, _ := args["properties"].(map[string]any)
	if _, ok := props["on_behalf"]; !ok {
		return a
	}
	out, outArgs, outProps := map[string]any{}, map[string]any{}, map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range args {
		outArgs[k] = v
	}
	for k, v := range props {
		if k != "on_behalf" {
			outProps[k] = v
		}
	}
	outArgs["properties"] = outProps
	out["args"] = outArgs
	return out
}
