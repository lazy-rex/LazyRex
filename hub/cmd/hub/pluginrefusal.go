package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// The refused plugins of a running hub, for Hub Status and the Start Hub pane. The source is the
// "refused" list of hub.plugins (docs/plugin-sources.md): a refused entry, or the managed file
// once. A refusal never makes the hub "not running", and the wrapper never restarts for one.

// pluginRefusalLines gives one line per refused item. A failed read or a reply without the list is
// unknown, never "no refusals". Text from the hub reaches a terminal: paneSafe.
func pluginRefusalLines(raw []byte, readErr error) []string {
	unknown := func(format string, a ...any) []string {
		return []string{paneSafe("plugins: unknown: " + fmt.Sprintf(format, a...))}
	}
	if readErr != nil {
		return unknown("%v", readErr)
	}
	var reply struct {
		Refused *[]struct {
			Source      *string `json:"source"`
			ManagedFile *string `json:"managed_file"`
			Error       string  `json:"error"`
		} `json:"refused"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return unknown("hub.plugins: %v", err)
	}
	if reply.Refused == nil {
		return unknown("hub.plugins has no refused list")
	}
	var lines []string
	for _, r := range *reply.Refused {
		switch {
		case r.ManagedFile != nil:
			lines = append(lines, paneSafe(fmt.Sprintf("plugins: managed file %s: %s", *r.ManagedFile, r.Error)))
		case r.Source != nil:
			lines = append(lines, paneSafe(fmt.Sprintf("plugin %s: refused: %s", *r.Source, r.Error)))
		default:
			return unknown("a refused item without a source: %s", r.Error)
		}
	}
	return lines
}

// readPlugins reads the refusals of this state's hub for Hub Status. It reads nothing when no
// single hub of this state is connected.
func (v *ctlView) readPlugins() {
	if v.own == nil || v.ownTwice {
		return
	}
	out, err := v.rexCmd(time.Now().Add(5*time.Second), "-C", v.own.ID, "do", "hub.plugins")
	v.plugins = pluginRefusalLines(out, err)
}

// hubRefusals reads the refusals of the hub with this pid and state, for the wrapper. The child's
// pid ties the answer to one run: a hub of an earlier run cannot answer for it.
func hubRefusals(cfg Config, state string) func(ctx context.Context, pid int) []string {
	return func(ctx context.Context, pid int) []string {
		v := &ctlView{cfg: cfg}
		var err error
		if v.rex, err = findRex(cfg.Rex); err != nil {
			return pluginRefusalLines(nil, err)
		}
		ids, err := v.hubIDsCtx(ctx)
		if err != nil {
			return pluginRefusalLines(nil, err)
		}
		for _, id := range ids {
			out, err := v.rexCmdCtx(ctx, "-C", id, "do", "hub.info")
			var info struct {
				State string `json:"state"`
				PID   int    `json:"pid"`
			}
			if err != nil || json.Unmarshal(out, &info) != nil || info.PID != pid || info.State != state {
				continue
			}
			return pluginRefusalLines(v.rexCmdCtx(ctx, "-C", id, "do", "hub.plugins"))
		}
		if ctx.Err() != nil {
			return pluginRefusalLines(nil, ctx.Err())
		}
		return pluginRefusalLines(nil, fmt.Errorf("no connected hub reports pid %d and the state %s", pid, state))
	}
}
