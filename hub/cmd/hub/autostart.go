package main

// lazyrex-hub autostart -config <cfg> [-real-server] -session <label> -pane <label> -- <argv>: the
// autostart that lazyrex_boot.lua runs at each load of the Rex config, which is the server start
// and each config reload (docs/hub.md, "Autostart"). It starts argv in a new pane only when both
// hold (lead-cwd item 2, the coordinator's b4-answer):
//   - (a) no connected lazyrex-hub client;
//   - (b) no pane labeled <pane> or "LazyRex hub" in this server instance. Panes do not survive a
//     server restart, and the hub pane stays open after its command ends, so a deliberate stop is
//     not undone by the next config reload.
// It does not take a lock: the restart wrapper's restart.lock and the hub's state lock keep one
// hub for each state.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

var autostartWait = 30 * time.Second // the server must answer within this time; a test shortens it

// startHubPane is the pane label of Start Hub (setupBootLua). That pane is a hub pane for (b) too.
const startHubPane = "LazyRex hub"

func autostartMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("autostart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "hub configuration JSON (required)")
	realServer := fs.Bool("real-server", false, "the config names the user's real Rex server")
	session := fs.String("session", "", "the label of the session for the hub pane (required)")
	pane := fs.String("pane", "", "the label of the hub pane (required)")
	if err := fs.Parse(args); err != nil || *cfgPath == "" || *session == "" || *pane == "" || fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: lazyrex-hub autostart -config <cfg> [-real-server] -session <label> -pane <label> -- <argv>")
		return ctlUsage
	}
	cfg, err := loadConfig(*cfgPath)
	if err == nil {
		err = checkServer(context.Background(), cfg.Server, *realServer, false)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ctlUsage
	}
	v := &ctlView{cfg: cfg}
	if v.rex, err = findRex(cfg.Rex); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	text, err := v.autostart(*session, *pane, fs.Args())
	if err != nil {
		text = "not started: " + err.Error()
	}
	// The boot file cannot read the output of a detached command, so the last result is also in
	// <state>/autostart.log. Each run replaces it: the hub reloads the config often.
	if cfg.State != "" {
		os.WriteFile(filepath.Join(cfg.State, "autostart.log"), []byte(time.Now().UTC().Format(time.RFC3339)+" "+text+"\n"), 0o600)
	}
	fmt.Fprintln(stdout, text)
	if err != nil {
		return 1
	}
	return 0
}

// autostart checks (a) and (b), and starts argv in a new pane labeled pane when both hold: in a new
// window of the first session labeled session, or in a new session with that label.
func (v *ctlView) autostart(session, pane string, argv []string) (string, error) {
	deadline := time.Now().Add(autostartWait)
	var sessions struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			Label     string `json:"label"`
		} `json:"sessions"`
	}
	for {
		out, err := v.rexCmd(deadline, "ls", "--json")
		if err == nil {
			if err := json.Unmarshal(out, &sessions); err != nil {
				return "", fmt.Errorf("rex ls: %v", err)
			}
			break
		}
		if time.Now().Add(200 * time.Millisecond).After(deadline) {
			return "", fmt.Errorf("the Rex server did not answer within %s: %v", autostartWait, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	ids, err := v.hubIDs(deadline)
	if err != nil {
		return "", err
	}
	if len(ids) > 0 {
		return "not started: a hub runs (" + ids[0] + ")", nil
	}
	target := ""
	for _, s := range sessions.Sessions {
		out, err := v.rexCmd(deadline, "block", "ls", "-s", s.SessionID, "--json")
		if err != nil {
			return "", err
		}
		var blocks struct {
			Blocks []struct {
				Label string `json:"label"`
			} `json:"blocks"`
		}
		if err := json.Unmarshal(out, &blocks); err != nil {
			return "", fmt.Errorf("rex block ls: %v", err)
		}
		for _, b := range blocks.Blocks {
			if b.Label == pane || b.Label == startHubPane {
				return fmt.Sprintf("not started: a pane labeled %q is in session %q", b.Label, s.Label), nil
			}
		}
		if target == "" && s.Label == session {
			target = s.SessionID
		}
	}
	layout := map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "label": pane,
		"options": map[string]any{"command": argv, "login": false, "exit": map[string]any{"on_completion": false}}}}
	method, req := "session.create", map[string]any{"label": session, "initial_windows": []any{map[string]any{"layout": layout}}}
	if target != "" {
		method, req = "session.new_window", map[string]any{"session_id": target, "layout": layout}
	}
	b, _ := json.Marshal(req)
	if _, err := v.rexCmd(deadline, "api", "call", method, string(b)); err != nil {
		return "", err
	}
	return fmt.Sprintf("started: pane %q in session %q", pane, session), nil
}
