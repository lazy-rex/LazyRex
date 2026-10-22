package main

// Remote Control, observed passively (the user, 2026-10-22; contract r2, approved by the hub lead
// and codex-24). The Rex app refuses client actions until the user turns on Remote Control in its
// settings. The hub never asks the app to find out: it records the result of the app actions that
// it runs anyway (appaction.go, and the navigate step in ui.go). hub.app.remote_control reads the
// records for the connected apps, and Hub Status shows them as the last observed result, never as
// the current setting.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// appRefusalPrefix starts the app's refusal message. Source: docs/rex-protocol.md, "Driving the Rex
// app" (observed by the CLI on Rex 1203, 2026-10-16): 403 refused, "Rex.app is not accepting
// actions. Turn on Remote Control in its Settings under Rex Server." The form that the hub gets
// (a RexError with code "refused", wrapped by rex.Call) is untested on a real app.
const appRefusalPrefix = "Rex.app is not accepting actions"

// remoteControlState maps the result of one client.perform to an app: "on" when the app ran the
// action, "off" when it refused it for Remote Control, "" when the result says nothing about the
// setting (a timeout, another refusal, any other error).
func remoteControlState(err error) string {
	if err == nil {
		return "on"
	}
	var re *rex.RexError
	if errors.As(err, &re) && re.Code == "refused" && strings.HasPrefix(re.Message, appRefusalPrefix) {
		return "off"
	}
	return ""
}

type rcObservation struct {
	state, action string
	at            time.Time
	seq           uint64 // remoteControl.next at the record: a read prunes only an observation that it saw
}

// remoteControl holds one observation for each app client ID, in memory only: a hub restart forgets
// them. The mutex guards the map only; no Rex call runs under it.
type remoteControl struct {
	mu   sync.Mutex
	seen map[string]rcObservation
	next uint64
}

// observeApp records the result of one client.perform to the app client. A result that says nothing
// about the setting keeps the earlier observation and its time.
func (h *Hub) observeApp(client, action string, err error) {
	state := remoteControlState(err)
	if state == "" || client == "" {
		return
	}
	h.rc.mu.Lock()
	defer h.rc.mu.Unlock()
	if h.rc.seen == nil {
		h.rc.seen = map[string]rcObservation{}
	}
	h.rc.next++
	h.rc.seen[client] = rcObservation{state: state, action: action, at: time.Now().UTC(), seq: h.rc.next}
}

// remoteControlRead is hub.app.remote_control: every connected app client of client.list, with its
// last observation or state "unknown". An empty list means no connected app. Observations of
// clients that Rex no longer lists are deleted; a client that Rex keeps while it reconnects keeps
// its observation, under the same client ID only. Only a read prunes, and only an observation that
// it saw before its client.list: one recorded during the call stays.
func (h *Hub) remoteControlRead(ctx context.Context) (any, error) {
	h.rc.mu.Lock()
	before := make(map[string]uint64, len(h.rc.seen))
	for id, o := range h.rc.seen {
		before[id] = o.seq
	}
	h.rc.mu.Unlock()
	var cl struct {
		Clients []struct {
			ClientID string          `json:"client_id"`
			Info     json.RawMessage `json:"info"`
			State    string          `json:"connection_state"`
		} `json:"clients"`
	}
	if err := appCall(h, ctx, "client.list", map[string]any{}, &cl); err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, c := range cl.Clients {
		listed[c.ClientID] = true
	}
	h.rc.mu.Lock()
	for id, o := range h.rc.seen {
		if seq, ok := before[id]; ok && seq == o.seq && !listed[id] {
			delete(h.rc.seen, id)
		}
	}
	seen := make(map[string]rcObservation, len(h.rc.seen))
	for id, o := range h.rc.seen {
		seen[id] = o
	}
	h.rc.mu.Unlock()
	apps := []any{}
	for _, c := range cl.Clients {
		var info struct {
			Kind  string `json:"kind"`
			Label string `json:"label"`
			Name  string `json:"name"`
		}
		json.Unmarshal(c.Info, &info)
		if info.Kind != "app" || c.State != "connected" {
			continue
		}
		label := info.Label
		if label == "" {
			label = info.Name
		}
		app := map[string]any{"client_id": c.ClientID, "label": label, "state": "unknown"}
		if o, ok := seen[c.ClientID]; ok {
			app["state"], app["action"], app["at"] = o.state, o.action, o.at.Format(time.RFC3339)
		}
		apps = append(apps, app)
	}
	return map[string]any{"apps": apps}, nil
}

// remoteControlLines gives the Hub Status lines: one per connected app, always with its client ID
// (two apps can have the same label). on and off are the last observed result, not the current
// setting. A failed read, a reply without the apps list, or an unknown state is unknown, never
// silence. Text from the hub reaches a terminal: paneSafe.
func remoteControlLines(raw []byte, readErr error) []string {
	unknown := func(format string, a ...any) []string {
		return []string{paneSafe("Remote Control: unknown: " + fmt.Sprintf(format, a...))}
	}
	if readErr != nil {
		return unknown("%v", readErr)
	}
	var reply struct {
		Apps *[]struct {
			ClientID string `json:"client_id"`
			Label    string `json:"label"`
			State    string `json:"state"`
			Action   string `json:"action"`
			At       string `json:"at"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return unknown("hub.app.remote_control: %v", err)
	}
	if reply.Apps == nil {
		return unknown("hub.app.remote_control has no apps list")
	}
	if len(*reply.Apps) == 0 {
		return []string{"Remote Control: no Rex app is connected"}
	}
	var lines []string
	for _, a := range *reply.Apps {
		app := a.ClientID
		if a.Label != "" {
			app = a.Label + " " + a.ClientID
		}
		at := a.At
		if t, err := time.Parse(time.RFC3339, a.At); err == nil {
			at = t.Local().Format("15:04:05")
		}
		var s string
		switch a.State {
		case "on":
			s = fmt.Sprintf("Remote Control (app %s): last observed on at %s (%s)", app, at, a.Action)
		case "off":
			s = fmt.Sprintf("Remote Control (app %s): last observed off at %s (%s refused). "+
				"Turn it on in the Rex app: Settings, Rex Server, Remote Control. "+
				"LazyRex needs it for Go to Session and Insert Path.", app, at, a.Action)
		case "unknown":
			s = fmt.Sprintf("Remote Control (app %s): unknown: no retained app action observation for this app", app)
		default:
			return unknown("app %s: state %q", app, a.State)
		}
		lines = append(lines, paneSafe(s))
	}
	return lines
}

// readRemoteControl reads the observations of this state's hub for Hub Status. It reads nothing
// when no single hub of this state is connected.
func (v *ctlView) readRemoteControl() {
	if v.own == nil || v.ownTwice {
		return
	}
	out, err := v.rexCmd(time.Now().Add(5*time.Second), "-C", v.own.ID, "do", "hub.app.remote_control")
	v.remote = remoteControlLines(out, err)
}
