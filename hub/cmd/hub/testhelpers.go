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
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
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
	if server == "" {
		return nil, fmt.Errorf("%s: REX_SERVER must name a lab server, got %q", label, server)
	}
	if err := checkLabProof(server); err != nil {
		return nil, fmt.Errorf("%s: REX_SERVER must name a lab server: %v", label, err)
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
	hold := fs.String("hold", "", "a file; while it names this route, the renderer waits before each subscribe (lab tests)")
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
		"testview.effect_retry", "testview.history", "testview.close", "testview.seen"} {
		actions = append(actions, clientAction(n, n, anyArgs))
	}
	c, err := helperDial(ctx, "lazyrex-testview:"+*viewID, actions)
	if err != nil {
		log.Print(err)
		return 1
	}
	// attach binds this renderer to the view through the current hub and subscribes. After a hub
	// restart the hub is a new client and the old attachment is gone: the watcher attaches again,
	// as the real renderer does (view/client.go).
	var mu sync.Mutex
	var hub, attachment string
	announced := "0"
	// subscribed: the current attachment has a subscription. The watcher attaches again while it is
	// false, also on the same hub. held, holdExpired, failures: test evidence (testview.status).
	var subscribed, held bool
	var holdExpired, failures int
	failOnce := true
	attach := func() error {
		h, err := c.resolveClient(ctx, "lazyrex-hub")
		if err != nil {
			return err
		}
		var att struct {
			Plugin     string `json:"plugin"`
			Route      string `json:"route"`
			Attachment string `json:"attachment"`
		}
		for i := 0; ; i++ {
			raw, err := c.perform(ctx, h, "hub.view.attach", map[string]any{"view_id": *viewID, "schema_version": 1})
			if err == nil {
				json.Unmarshal(raw, &att)
				break
			}
			if i == 30 || !strings.Contains(err.Error(), "not_ready") {
				return fmt.Errorf("attach: %w", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if att.Plugin != fs.Arg(0) || att.Route != fs.Arg(1) {
			return fmt.Errorf("attach: the hub bound %s %s, the launch named %s %s", att.Plugin, att.Route, fs.Arg(0), fs.Arg(1))
		}
		mu.Lock()
		hub, attachment, subscribed = h, att.Attachment, false
		mu.Unlock()
		mode := readHold(*hold, fs.Arg(1))
		if mode == "hold" {
			mu.Lock()
			held = true
			mu.Unlock()
			expired := holdBeforeSubscribe(*hold, fs.Arg(1))
			mu.Lock()
			held = false
			if expired {
				holdExpired++
			}
			mu.Unlock()
		}
		mu.Lock()
		fail := mode == "fail-subscribe-once" && failOnce
		if fail {
			failOnce = false
			failures++
		}
		mu.Unlock()
		if fail {
			// Simulated by the test view: the subscribe is not sent. It tests the retry.
			return errors.New("subscribe: simulated refusal (hold file: fail-subscribe-once)")
		}
		if _, err := c.perform(ctx, h, "hub.view.subscribe", map[string]any{"view_id": *viewID, "attachment": att.Attachment}); err != nil {
			mu.Lock()
			failures++
			mu.Unlock()
			return fmt.Errorf("subscribe: %w", err)
		}
		mu.Lock()
		if attachment == att.Attachment {
			subscribed = true
		}
		mu.Unlock()
		log.Printf("attached %s %s through %s", att.Plugin, att.Route, h)
		return nil
	}
	current := func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		return hub, attachment
	}
	exit := make(chan int, 1)
	go c.serve(func(p perform, args map[string]any) {
		hub, attachment := current()
		if strings.HasPrefix(p.Action, "view.") {
			// Callbacks from the hub: answer at once (the hub must not wait for a renderer).
			if p.Requester.ClientID != hub || args["view_id"] != *viewID || args["attachment"] != attachment {
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
		base := map[string]any{"view_id": *viewID, "attachment": attachment}
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
			mu.Lock()
			st := map[string]any{"view_id": *viewID, "plugin": fs.Arg(0), "route": fs.Arg(1), "announced": a, "pid": os.Getpid(),
				"subscribed": subscribed, "held": held, "hold_expired": holdExpired, "subscribe_failures": failures}
			mu.Unlock()
			c.reply(p.CallID, st, nil)
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
		case "testview.seen": // the renderer reports focus or new content (hub.view.seen {reason})
			call("hub.view.seen")
		default:
			c.reply(p.CallID, nil, &rex.RexError{Code: "not_found", Message: "no action " + p.Action})
		}
	})
	// Callbacks are served before the first subscribe: the hub can ask view.release while the
	// subscribe waits in its queue behind that request (rex-protocol.md, "The hub has one request
	// queue").
	if err := attach(); err != nil {
		log.Printf("%v; the watcher tries again", err) // a closed view ends with its pane
	}
	go func() {
		// A new hub client means a restarted hub. While no hub is connected, the resolve fails and
		// the watcher waits. An attach or subscribe that failed is tried again on the same hub.
		for !c.r.Closed() {
			time.Sleep(200 * time.Millisecond)
			h, err := c.resolveClient(ctx, "lazyrex-hub")
			mu.Lock()
			done := subscribed
			mu.Unlock()
			if was, _ := current(); err != nil || h == was && done {
				continue
			}
			if err := attach(); err != nil {
				log.Printf("attach again: %v", err)
			}
		}
	}()
	select {
	case code := <-exit:
		time.Sleep(200 * time.Millisecond) // let the reply leave
		return code
	case <-closed(c.r):
		log.Print("rex connection closed")
		return 1
	}
}

// The hold file (lab tests): the first line is a route, the second a mode, "hold" (the default) or
// "fail-subscribe-once". readHold returns the mode when the file names route, else "".
func readHold(path, route string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if strings.TrimSpace(lines[0]) != route {
		return ""
	}
	if len(lines) > 1 && strings.TrimSpace(lines[1]) != "" {
		return strings.TrimSpace(lines[1])
	}
	return "hold"
}

// holdBeforeSubscribe waits while the hold file holds route, at most 20 s. It reports whether the
// 20 s expired. A test uses it to keep a renderer between its attach and its subscribe.
func holdBeforeSubscribe(path, route string) bool {
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if readHold(path, route) != "hold" {
			return false
		}
	}
	log.Printf("hold: %s still holds %s after 20 s; subscribing", path, route)
	return true
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
	if len(argv) > 0 && argv[0] == "cli" {
		return testCLIMain(argv[1:]) // testcli.go
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
	for _, n := range []string{"agent.status", "agent.call", "agent.direct", "agent.hub", "agent.report", "agent.report_result", "agent.title", "agent.program_status", "agent.input", "agent.exit"} {
		actions = append(actions, clientAction(n, n, anyArgs))
	}
	c, err := helperDial(ctx, "lazyrex-testagent:"+*id, actions)
	if err != nil {
		log.Print(err)
		return 1
	}
	var mu sync.Mutex
	joined, joinErr := *join == "", ""
	reports := map[string]*asyncReport{} // async key -> outcome (nil: running)
	var input *strings.Builder           // agent.input: the captured pane input (nil: not capturing)
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
		case "agent.title": // the terminal title (OSC 2), as an agent CLI sets it: stdout is the pane
			title, _ := args["title"].(string)
			_, err := fmt.Fprintf(os.Stdout, "\x1b]2;%s\x07", title)
			c.reply(p.CallID, map[string]any{}, err)
		case "agent.program_status": // the root program status record (OSC 7501), as an agent CLI reports it
			// Optional kind, app and msg (base64 in the report), as Claude Code reports a prompt.
			report := "state=" + fmt.Sprint(args["state"])
			for _, k := range []string{"kind", "app"} {
				if v, _ := args[k].(string); v != "" {
					report += ":" + k + "=" + v
				}
			}
			if v, _ := args["msg"].(string); v != "" {
				report += ":msg=" + base64.StdEncoding.EncodeToString([]byte(v))
			}
			_, err := fmt.Fprintf(os.Stdout, "\x1b]7501;%s\x1b\\", report)
			c.reply(p.CallID, map[string]any{}, err)
		case "agent.input": // the bytes written to this pane since the capture started (or reset)
			mu.Lock()
			if input == nil {
				input = &strings.Builder{}
				go captureInput(&mu, input)
			}
			data := input.String()
			if reset, _ := args["reset"].(bool); reset {
				input.Reset()
			}
			mu.Unlock()
			c.reply(p.CallID, map[string]any{"data": data}, nil)
		case "agent.exit": // the agent process exits, as an agent CLI does when the user quits it
			c.reply(p.CallID, map[string]any{}, nil)
			go func() { time.Sleep(200 * time.Millisecond); os.Exit(0) }()
		case "agent.report":
			// delay_ms: the report process waits before it reports, so a report started later can
			// finish first. async: answer when the process has started (its start time is then older
			// than the start of any later report); agent.report_result gives the outcome.
			delay, _ := args["delay_ms"].(float64)
			key, _ := args["async"].(string)
			wait, err := startTestReport(ctx, inner, int(delay))
			if err != nil {
				c.reply(p.CallID, nil, err)
				return
			}
			if key == "" {
				out, err := wait()
				c.reply(p.CallID, out, err)
				return
			}
			// A Linux start time has a resolution of one clock tick (procUserHZ): a report that
			// starts in the same tick has an equal start time, not a later one. Two ticks after the
			// start, the boot clock has passed the tick of this process.
			time.Sleep(2 * time.Second / procUserHZ)
			mu.Lock()
			reports[key] = nil
			mu.Unlock()
			go func() {
				out, err := wait()
				mu.Lock()
				reports[key] = &asyncReport{out, err}
				mu.Unlock()
			}()
			c.reply(p.CallID, map[string]any{"started": true}, nil)
		case "agent.report_result":
			key, _ := args["async"].(string)
			mu.Lock()
			r, known := reports[key]
			mu.Unlock()
			switch {
			case !known:
				c.reply(p.CallID, nil, &rex.RexError{Code: "not_found", Message: "no report " + key})
			case r == nil:
				c.reply(p.CallID, map[string]any{"done": false}, nil)
			case r.err != nil:
				var re *rex.RexError
				if !errors.As(r.err, &re) {
					re = &rex.RexError{Code: "failed", Message: r.err.Error()}
				}
				c.reply(p.CallID, map[string]any{"done": true, "error": map[string]any{"code": re.Code, "message": re.Message}}, nil)
			default:
				c.reply(p.CallID, map[string]any{"done": true, "result": r.out}, nil)
			}
		default:
			c.reply(p.CallID, nil, &rex.RexError{Code: "not_found", Message: "no action " + p.Action})
		}
	})
	<-closed(c.r)
	return 1
}

type asyncReport struct {
	out json.RawMessage
	err error
}

// startTestReport starts one report in a new process, as an agent hook runs: the hub orders hook
// reports by the start time of the reporting process (identity.go). The process waits delay ms
// before it reports. wait returns the outcome.
func startTestReport(ctx context.Context, args any, delay int) (wait func() (json.RawMessage, error), err error) {
	b, _ := json.Marshal(args)
	self, _ := os.Executable()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second+time.Duration(delay)*time.Millisecond)
	cmd := exec.CommandContext(cctx, self, "testagent", "report", "-delay", strconv.Itoa(delay), string(b))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	return func() (json.RawMessage, error) {
		defer cancel()
		if err := cmd.Wait(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			var re rex.RexError
			if json.Unmarshal([]byte(msg), &re) == nil && re.Code != "" {
				return nil, &re
			}
			return nil, errors.New(msg)
		}
		return json.RawMessage(stdout.String()), nil
	}, nil
}

// testAgentReport sends one hub.identity.report from this process and prints the result. On a
// failure it prints the Rex error as JSON on stderr.
func testAgentReport(argv []string) int {
	if len(argv) == 3 && argv[0] == "-delay" {
		ms, _ := strconv.Atoi(argv[1])
		time.Sleep(time.Duration(ms) * time.Millisecond)
		argv = argv[2:]
	}
	if len(argv) != 1 {
		fmt.Fprintln(os.Stderr, "usage: lazyrex-hub testagent report [-delay ms] '<args json>'")
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

// captureInput puts the pane's terminal in raw mode without echo, so every byte reaches stdin as
// written (a key without Enter too), and appends stdin to buf.
func captureInput(mu *sync.Mutex, buf *strings.Builder) {
	stty := exec.Command("stty", "raw", "-echo")
	stty.Stdin = os.Stdin
	if err := stty.Run(); err != nil {
		log.Printf("agent.input: stty: %v", err)
	}
	b := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(b)
		mu.Lock()
		buf.Write(b[:n])
		mu.Unlock()
		if err != nil {
			return
		}
	}
}
