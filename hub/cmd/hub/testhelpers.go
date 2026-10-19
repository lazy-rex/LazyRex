package main

// The pane helpers of the Lua test harness (docs/testing.md). Both run in lab panes, so the hub sees
// real pane ancestry for their calls:
//
//   - testview is the renderer of a test hub (config "view"). The hub's UI worker starts it in a
//     pane like the real renderer. It attaches, subscribes and answers the hub's callbacks, and
//     carries the test's view calls (testview.*) to the hub with its attachment.
//   - testagent is a test agent. It joins as a roster agent from its pane, reports conversation
//     identities from a new child process each time (like an agent hook), and carries the test's
//     agent calls: through the agent face (agent.call, the MCP path), to the plugin (agent.direct),
//     or to the hub (agent.hub).
//
// Rex delivers requests to one client one at a time. So every handler here answers after one
// short call to the hub, the agent face or a plugin, and none waits for progress that needs a
// callback to itself. The Lua test polls.
//
// A `rex do` script receives at most 64 KiB in one control message (rex-protocol.md, "Rex Lua
// runtime"). Each reply to the test is measured as encoded JSON and refused with too_large above
// testReplyLimit. Nothing is truncated.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// helperConn is a Rex client of a pane helper.
type helperConn struct {
	r     *rex.Rex
	label string
}

func helperDial(ctx context.Context, label string, actions []map[string]any) (*helperConn, error) {
	server := os.Getenv("REX_SERVER")
	if server == "" || isRealServer(server) {
		return nil, fmt.Errorf("%s: REX_SERVER must name a lab server, got %q", label, server)
	}
	socket, err := rex.SocketPath(server)
	if err != nil {
		return nil, err
	}
	r, err := rex.Dial(ctx, socket)
	if err != nil {
		return nil, err
	}
	if err := r.Call(ctx, "client.keymap", map[string]any{"actions": actions}, nil); err != nil {
		return nil, err
	}
	if label != "" {
		if err := r.Call(ctx, "client.set_label", map[string]any{"label": label}, nil); err != nil {
			return nil, err
		}
	}
	return &helperConn{r: r, label: label}, nil
}

// resolveClient finds the one connected client with the label.
func (c *helperConn) resolveClient(ctx context.Context, label string) (string, error) {
	type target struct {
		ClientID string `json:"client_id"`
	}
	var res struct {
		Results []struct {
			Status     string   `json:"status"`
			Target     target   `json:"target"`
			Candidates []target `json:"candidates"`
		} `json:"results"`
	}
	if err := c.r.Call(ctx, "target.resolve", map[string]any{"kinds": []string{"client"}, "queries": []string{label}}, &res); err != nil {
		return "", err
	}
	if len(res.Results) != 1 {
		return "", fmt.Errorf("resolve %s: no result", label)
	}
	candidates := res.Results[0].Candidates
	if res.Results[0].Status == "resolved" {
		candidates = []target{res.Results[0].Target}
	}
	var connected []string
	for _, t := range candidates {
		var ins struct {
			Client struct {
				ConnectionState string `json:"connection_state"`
			} `json:"client"`
		}
		if c.r.Call(ctx, "client.inspect", map[string]any{"client_id": t.ClientID}, &ins) == nil && ins.Client.ConnectionState == "connected" {
			connected = append(connected, t.ClientID)
		}
	}
	if len(connected) != 1 {
		return "", &rex.RexError{Code: "not_found", Message: fmt.Sprintf("%d connected clients labeled %s", len(connected), label)}
	}
	return connected[0], nil
}

// perform calls an action on another client and returns its raw result.
func (c *helperConn) perform(ctx context.Context, client, action string, args any) (json.RawMessage, error) {
	if args == nil {
		args = map[string]any{}
	}
	var res struct {
		Result json.RawMessage `json:"result"`
	}
	err := c.r.Call(ctx, "client.perform", map[string]any{"client_id": client, "action": action, "args": args, "timeout_ms": 10000}, &res)
	return res.Result, err
}

// reply answers one request. An error keeps the code of a Rex error, so the test can match it.
func (c *helperConn) reply(callID string, result any, err error) {
	msg := map[string]any{"call_id": callID}
	if err == nil {
		b, merr := json.Marshal(result)
		switch {
		case merr != nil:
			err = merr
		case len(b) > testReplyLimit:
			err = &rex.RexError{Code: "too_large", Message: fmt.Sprintf("the reply is %d bytes, the limit for a Lua test client is %d", len(b), testReplyLimit)}
		default:
			msg["result"] = json.RawMessage(b)
		}
	}
	if err != nil {
		var re *rex.RexError
		if errors.As(err, &re) {
			msg["error"] = &rex.RexError{Code: re.Code, Message: re.Message}
		} else {
			msg["error"] = &rex.RexError{Code: "failed", Message: err.Error()}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if rerr := c.r.Call(ctx, "client.reply", msg, nil); rerr != nil {
		log.Printf("reply %s: %v", callID, rerr)
	}
}

// serve runs handle for each client_perform_requested event, each in its own goroutine.
func (c *helperConn) serve(handle func(p perform, args map[string]any)) {
	for raw := range c.r.Events {
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
		if json.Unmarshal(env.Payload.Payload, &p) != nil {
			continue
		}
		var args map[string]any
		json.Unmarshal(p.Args, &args)
		if args == nil {
			args = map[string]any{}
		}
		go handle(p, args)
	}
}

func clientAction(name, title string, args map[string]any) map[string]any {
	a := map[string]any{"name": name, "kind": "client", "title": title}
	if args != nil {
		a["args"] = args
	}
	return a
}

// anyArgs is an args schema that Rex accepts with any object.
var anyArgs = map[string]any{"type": "object"}

// --- testview ---

func testViewMain(argv []string) int {
	fs := flag.NewFlagSet("testview", flag.ContinueOnError)
	viewID := fs.String("view-id", "", "view ID (from the hub)")
	fs.Bool("transcript", false, "transcript role (pair.go); the test renderer takes the composer role only")
	real := fs.Bool("real-server", false, "refused")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if *real || *viewID == "" || fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: lazyrex-hub testview -view-id <id> <plugin> <route> (lab only)")
		return 2
	}
	log.SetPrefix("testview " + *viewID + " ")
	ctx := context.Background()
	actions := []map[string]any{}
	for _, n := range []string{"view.changed", "view.reload", "view.retire", "view.close_request", "view.release", "view.resume"} {
		actions = append(actions, clientAction(n, n, nil))
	}
	for _, n := range []string{"testview.status", "testview.get", "testview.action", "testview.draft", "testview.send", "testview.operation",
		"testview.effect_retry", "testview.history", "testview.close"} {
		actions = append(actions, clientAction(n, n, anyArgs))
	}
	c, err := helperDial(ctx, "lazyrex-testview:"+*viewID, actions)
	if err != nil {
		log.Print(err)
		return 1
	}
	hub, err := c.resolveClient(ctx, "lazyrex-hub")
	if err != nil {
		log.Print(err)
		return 1
	}
	var att struct {
		Plugin     string `json:"plugin"`
		Route      string `json:"route"`
		Attachment string `json:"attachment"`
	}
	for i := 0; ; i++ {
		raw, err := c.perform(ctx, hub, "hub.view.attach", map[string]any{"view_id": *viewID, "schema_version": 1})
		if err == nil {
			json.Unmarshal(raw, &att)
			break
		}
		if i == 30 || !strings.Contains(err.Error(), "not_ready") {
			log.Printf("attach: %v", err)
			return 1
		}
		time.Sleep(100 * time.Millisecond)
	}
	if att.Plugin != fs.Arg(0) || att.Route != fs.Arg(1) {
		log.Printf("attach: the hub bound %s %s, the launch named %s %s", att.Plugin, att.Route, fs.Arg(0), fs.Arg(1))
		return 1
	}
	var mu sync.Mutex
	announced := "0"
	if _, err := c.perform(ctx, hub, "hub.view.subscribe", map[string]any{"view_id": *viewID, "attachment": att.Attachment}); err != nil {
		log.Printf("subscribe: %v", err)
		return 1
	}
	log.Printf("attached %s %s", att.Plugin, att.Route)
	exit := make(chan int, 1)
	go c.serve(func(p perform, args map[string]any) {
		if strings.HasPrefix(p.Action, "view.") {
			// Callbacks from the hub: answer at once (the hub must not wait for a renderer).
			if p.Requester.ClientID != hub || args["view_id"] != *viewID || args["attachment"] != att.Attachment {
				c.reply(p.CallID, nil, &rex.RexError{Code: "refused", Message: "The notification does not match the current view attachment"})
				return
			}
			switch p.Action {
			case "view.changed":
				mu.Lock()
				if r, _ := args["revision"].(string); r != "" {
					announced = r
				}
				mu.Unlock()
				c.reply(p.CallID, map[string]any{"received": true}, nil)
			case "view.release":
				c.reply(p.CallID, map[string]any{"status": "released"}, nil) // no unsaved text
			case "view.resume":
				c.reply(p.CallID, map[string]any{"status": "resumed"}, nil)
			case "view.reload":
				c.reply(p.CallID, map[string]any{}, nil)
				self, _ := os.Executable()
				syscall.Exec(self, os.Args, os.Environ()) // like the renderer: the same process, a new attachment
			case "view.retire", "view.close_request":
				c.reply(p.CallID, map[string]any{"status": "accepted"}, nil)
				exit <- 0 // no unsaved text: the renderer leaves; its exit closes the pane
			}
			return
		}
		base := map[string]any{"view_id": *viewID, "attachment": att.Attachment}
		for k, v := range args {
			base[k] = v
		}
		call := func(action string) {
			raw, err := c.perform(ctx, hub, action, base)
			c.reply(p.CallID, raw, err)
		}
		switch p.Action {
		case "testview.status":
			mu.Lock()
			a := announced
			mu.Unlock()
			c.reply(p.CallID, map[string]any{"view_id": *viewID, "plugin": att.Plugin, "route": att.Route, "announced": a, "pid": os.Getpid()}, nil)
		case "testview.get":
			call("hub.view.get")
		case "testview.action":
			call("hub.view.action")
		case "testview.draft":
			call("hub.view.draft.save")
		case "testview.send":
			call("hub.view.send")
		case "testview.operation":
			call("hub.view.operation")
		case "testview.effect_retry":
			call("hub.view.effect.retry")
		case "testview.history":
			call("hub.view.history")
		case "testview.close":
			call("hub.view.close")
		default:
			c.reply(p.CallID, nil, &rex.RexError{Code: "not_found", Message: "no action " + p.Action})
		}
	})
	select {
	case code := <-exit:
		time.Sleep(200 * time.Millisecond) // let the reply leave
		return code
	case <-closed(c.r):
		log.Print("rex connection closed")
		return 1
	}
}

// closed returns a channel that is closed when the connection ends.
func closed(r *rex.Rex) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for !r.Closed() {
			time.Sleep(200 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}

// --- testagent ---

func testAgentMain(argv []string) int {
	if len(argv) > 0 && argv[0] == "report" {
		return testAgentReport(argv[1:])
	}
	fs := flag.NewFlagSet("testagent", flag.ContinueOnError)
	id := fs.String("id", "", "test agent name (its label is lazyrex-testagent:<id>)")
	join := fs.String("join", "", "roster agent to join as (hub.join), from this pane")
	if err := fs.Parse(argv); err != nil || *id == "" {
		fmt.Fprintln(os.Stderr, "usage: lazyrex-hub testagent -id <name> [-join <roster agent>] (lab only)")
		return 2
	}
	log.SetPrefix("testagent " + *id + " ")
	ctx := context.Background()
	var actions []map[string]any
	for _, n := range []string{"agent.status", "agent.call", "agent.direct", "agent.hub", "agent.report"} {
		actions = append(actions, clientAction(n, n, anyArgs))
	}
	c, err := helperDial(ctx, "lazyrex-testagent:"+*id, actions)
	if err != nil {
		log.Print(err)
		return 1
	}
	var mu sync.Mutex
	joined, joinErr := *join == "", ""
	if *join != "" {
		go func() {
			for i := 0; i < 100; i++ {
				hub, err := c.resolveClient(ctx, "lazyrex-hub")
				if err == nil {
					_, err = c.perform(ctx, hub, "hub.join", map[string]any{"agent": *join})
				}
				mu.Lock()
				joined, joinErr = err == nil, errString(err)
				mu.Unlock()
				if err == nil || !strings.Contains(err.Error(), "not_ready") && !strings.Contains(err.Error(), "not_found") {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}
	go c.serve(func(p perform, args map[string]any) {
		action, _ := args["action"].(string)
		inner := args["args"]
		// args_json: the arguments as JSON text, used verbatim. Rex Lua encodes an empty table
		// as {}, so a Lua test cannot send an empty array any other way.
		if text, ok := args["args_json"].(string); ok {
			var v any
			if err := json.Unmarshal([]byte(text), &v); err != nil {
				c.reply(p.CallID, nil, fmt.Errorf("args_json: %w", err))
				return
			}
			inner = v
		}
		target := func(label string) (string, error) { return c.resolveClient(ctx, label) }
		switch p.Action {
		case "agent.status":
			mu.Lock()
			defer mu.Unlock()
			c.reply(p.CallID, map[string]any{"id": *id, "pid": os.Getpid(), "joined": joined, "join_error": joinErr}, nil)
		case "agent.call": // the agent face: the path agents use (MCP shim and `rex -C lazyrex do`)
			client, err := target(agentLabel)
			if err != nil {
				c.reply(p.CallID, nil, err)
				return
			}
			raw, err := c.perform(ctx, client, action, inner)
			c.reply(p.CallID, raw, err)
		case "agent.direct":
			plugin, _ := args["plugin"].(string)
			client, err := target("lazyrex-plugin:" + plugin)
			if err != nil {
				c.reply(p.CallID, nil, err)
				return
			}
			raw, err := c.perform(ctx, client, action, inner)
			c.reply(p.CallID, raw, err)
		case "agent.hub":
			client, err := target("lazyrex-hub")
			if err != nil {
				c.reply(p.CallID, nil, err)
				return
			}
			raw, err := c.perform(ctx, client, action, inner)
			c.reply(p.CallID, raw, err)
		case "agent.report":
			// A new process for each report, as an agent hook runs: the hub orders hook reports by
			// the start time of the reporting process (identity.go).
			b, _ := json.Marshal(inner)
			self, _ := os.Executable()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			out, err := exec.CommandContext(cctx, self, "testagent", "report", string(b)).Output()
			if err != nil {
				msg := err.Error()
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					msg = strings.TrimSpace(string(ee.Stderr))
				}
				var re rex.RexError
				if json.Unmarshal([]byte(msg), &re) == nil && re.Code != "" {
					c.reply(p.CallID, nil, &re)
				} else {
					c.reply(p.CallID, nil, errors.New(msg))
				}
				return
			}
			c.reply(p.CallID, json.RawMessage(out), nil)
		default:
			c.reply(p.CallID, nil, &rex.RexError{Code: "not_found", Message: "no action " + p.Action})
		}
	})
	<-closed(c.r)
	return 1
}

// testAgentReport sends one hub.identity.report from this process and prints the result. On a
// failure it prints the Rex error as JSON on stderr.
func testAgentReport(argv []string) int {
	if len(argv) != 1 {
		fmt.Fprintln(os.Stderr, "usage: lazyrex-hub testagent report '<args json>'")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	fail := func(err error) int {
		var re *rex.RexError
		if !errors.As(err, &re) {
			re = &rex.RexError{Code: "failed", Message: err.Error()}
		}
		b, _ := json.Marshal(re)
		os.Stderr.Write(b)
		return 1
	}
	c, err := helperDial(ctx, "", []map[string]any{})
	if err != nil {
		return fail(err)
	}
	hub, err := c.resolveClient(ctx, "lazyrex-hub")
	if err != nil {
		return fail(err)
	}
	raw, err := c.perform(ctx, hub, "hub.identity.report", json.RawMessage(argv[0]))
	if err != nil {
		return fail(err)
	}
	os.Stdout.Write(raw)
	return 0
}
