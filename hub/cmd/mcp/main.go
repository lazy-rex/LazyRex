// Command mcp exposes hub and plugin actions as MCP tools.
// Startup joins the legacy mailbox. Tool calls report conversation identity before dispatch.
// Rex attests this process's PID, and the hub checks its pane and process generation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lazy-rex/LazyRex/hub/internal/rexreal"
	"github.com/lazy-rex/LazyRex/hub/rex"
)

type tool struct {
	Name        string          `json:"name"`
	Action      string          `json:"action"`
	Plugin      string          `json:"plugin"`
	ClientID    string          `json:"client_id"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

const hubLabel = "lazyrex-hub"

type shim struct {
	server, agent, block string

	mu    sync.Mutex
	conn  *rex.Rex
	hubID string
	tools map[string]tool // by MCP tool name
}

// connect joins the legacy mailbox and refreshes tool routes. It does not report a conversation.
func (s *shim) connect(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil || s.conn.Closed() {
		socket, err := rex.SocketPath(s.server)
		if err != nil {
			return err
		}
		if s.conn, err = rex.Dial(ctx, socket); err != nil {
			return err
		}
	}
	hubID, err := s.resolveHub(ctx)
	if err != nil {
		return err
	}
	s.hubID = hubID
	if err := s.performLocked(ctx, s.hubID, "hub.join", map[string]any{"agent": s.agent}, nil); err != nil {
		return fmt.Errorf("join as %s: %w", s.agent, err)
	}
	var out struct {
		Tools []tool `json:"tools"`
	}
	if err := s.performLocked(ctx, s.hubID, "hub.tools", map[string]any{}, &out); err != nil {
		return err
	}
	// Descriptions and argument schemas come from Rex, where each plugin declared them.
	meta := map[string]map[string]tool{}
	s.tools = map[string]tool{}
	for _, t := range out.Tools {
		if meta[t.ClientID] == nil {
			var ins struct {
				Actions []struct {
					Name        string          `json:"name"`
					Description string          `json:"description"`
					Args        json.RawMessage `json:"args"`
				} `json:"actions"`
			}
			if err := s.conn.Call(ctx, "client.inspect", map[string]any{"client_id": t.ClientID}, &ins); err != nil {
				return err
			}
			meta[t.ClientID] = map[string]tool{}
			for _, a := range ins.Actions {
				meta[t.ClientID][a.Name] = tool{Description: a.Description, InputSchema: a.Args}
			}
		}
		m := meta[t.ClientID][t.Action]
		t.Description, t.InputSchema = m.Description, m.InputSchema
		if len(t.InputSchema) == 0 {
			t.InputSchema = json.RawMessage(`{"type":"object"}`)
		}
		s.tools[t.Name] = t
	}
	return nil
}

// resolveHub finds the one connected client labeled lazyrex-hub. Rex keeps a lost client
// (for example a crashed hub) for a reconnect grace period, so target.resolve can report
// "ambiguous" during a restart. Only candidates whose Rex connection_state is "connected"
// count. Two connected hubs are refused.
func (s *shim) resolveHub(ctx context.Context) (string, error) {
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
	if err := s.conn.Call(ctx, "target.resolve", map[string]any{"kinds": []string{"client"}, "queries": []string{hubLabel}}, &res); err != nil {
		return "", err
	}
	if len(res.Results) != 1 {
		return "", fmt.Errorf("hub %q: no result", hubLabel)
	}
	r := res.Results[0]
	candidates := r.Candidates
	switch r.Status {
	case "resolved":
		candidates = []target{r.Target}
	case "ambiguous":
	default:
		return "", fmt.Errorf("hub %q: %s", hubLabel, r.Status)
	}
	var connected []string
	for _, c := range candidates {
		var ins struct {
			Client struct {
				ConnectionState string `json:"connection_state"`
			} `json:"client"`
		}
		if err := s.conn.Call(ctx, "client.inspect", map[string]any{"client_id": c.ClientID}, &ins); err != nil {
			continue // gone since resolve
		}
		if ins.Client.ConnectionState == "connected" {
			connected = append(connected, c.ClientID)
		}
	}
	if len(connected) != 1 {
		return "", fmt.Errorf("hub %q: %d connected candidates of %d", hubLabel, len(connected), len(candidates))
	}
	return connected[0], nil
}

func (s *shim) performLocked(ctx context.Context, clientID, action string, args any, out any) error {
	var res struct {
		Result json.RawMessage `json:"result"`
	}
	err := s.conn.Call(ctx, "client.perform", map[string]any{"client_id": clientID, "action": action, "args": args, "timeout_ms": 10000}, &res)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(res.Result, out)
}

// targetGone reports a failure where Rex did not deliver the request: retrying cannot duplicate it.
func targetGone(err error) bool {
	var re *rex.RexError
	return errors.As(err, &re) && strings.Contains(re.Message, "not found")
}

// definite reports an error reply from the handler itself (for example a validation error or
// a key conflict). Timeouts and lost connections have an unknown outcome.
func definite(err error) bool {
	var re *rex.RexError
	if !errors.As(err, &re) {
		return false
	}
	switch strings.ToLower(re.Code) {
	case "disconnected", "client_unavailable", "timeout", "deadline_exceeded", "unavailable":
		return false
	}
	return !strings.Contains(strings.ToLower(re.Message), "timed out")
}

func (s *shim) call(ctx context.Context, name string, args json.RawMessage, meta map[string]any) (*mcp.CallToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	for attempt := 0; ; attempt++ {
		s.mu.Lock()
		t, ok := s.tools[name]
		var res struct {
			Result json.RawMessage `json:"result"`
		}
		var err error
		if !ok {
			err = fmt.Errorf("tool %s is not available", name)
		} else if s.conn == nil || s.conn.Closed() {
			err = &rex.RexError{Code: "disconnected", Message: "client target not found: no connection"}
		} else {
			// Codex supplies the current thread on each call. Never use inherited process identity.
			if thread, ok := meta["threadId"].(string); ok && thread != "" {
				report := map[string]any{"kind": "codex", "conversation_id": thread, "source": "mcp"}
				if s.block != "" {
					report["block"] = s.block
				}
				if reportErr := s.performLocked(ctx, s.hubID, "hub.identity.report", report, nil); reportErr != nil {
					// Stage 1 records identity failures but keeps legacy mailbox routing.
					log.Printf("conversation identity report: %v", reportErr)
				}
			}
			err = s.conn.Call(ctx, "client.perform", map[string]any{"client_id": t.ClientID, "action": t.Action, "args": args, "timeout_ms": 10000}, &res)
		}
		s.mu.Unlock()
		if err == nil {
			text := string(res.Result)
			if text == "" {
				text = "{}"
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
		}
		// Rediscover and retry only when the request was not delivered. Any other failure
		// has an unknown outcome: the agent retries a mutation itself, with the same key.
		if attempt >= 3 || !targetGone(err) {
			text := "outcome unknown: " + err.Error() + ". Retry a send only with the same key."
			if definite(err) {
				text = "refused: " + err.Error()
			}
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
		if cerr := s.connect(ctx); cerr != nil {
			log.Printf("rediscover: %v", cerr)
		}
	}
}

// resolveServer picks the shim's server: -server, else REX_SERVER, else Rex's default endpoint (a
// Linux pane has no REX_SERVER). The real server needs -real-server (rexreal.IsReal: a known
// default path, or Rex's identity of its default; each uncertain answer is "real").
func resolveServer(flagServer, env string, realServer bool) (string, error) {
	server := flagServer
	if server == "" {
		server = env
	}
	if server == "" {
		def, err := rexreal.DefaultEndpoint()
		if err != nil {
			return "", fmt.Errorf("no -server, no REX_SERVER, and Rex's default server is not known: %w", err)
		}
		server = def
	}
	if rexreal.IsReal(server) != realServer {
		return "", fmt.Errorf("server %s and -real-server=%v disagree; the real Rex server needs -real-server", server, realServer)
	}
	return server, nil
}

func main() {
	serverFlag := flag.String("server", "", "Rex server (default: $REX_SERVER, else Rex's default server)")
	agent := flag.String("agent", "", "logical agent ID from the hub roster (required)")
	realServer := flag.Bool("real-server", false, "connect to the user's real Rex server (approved runs only)")
	flag.Parse()
	// A pane that the hub started for a profile carries its agent ID in LAZYREX_AGENT. That
	// assignment wins over -agent, which comes from a shared MCP config (for example "claude").
	if id := os.Getenv("LAZYREX_AGENT"); id != "" {
		*agent = id
	}
	if *agent == "" {
		log.Fatal("usage: mcp -agent <id> [-server unix:///...] [-real-server]")
	}
	server, err := resolveServer(*serverFlag, os.Getenv("REX_SERVER"), *realServer)
	if err != nil {
		log.Fatal(err)
	}
	s := &shim{server: server, agent: *agent, block: os.Getenv("REX_BLOCK")}
	ctx := context.Background()
	// Join before discovery so notices can arrive before the first tool call.
	for i := 0; i < 30; i++ {
		if err = s.connect(ctx); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Fatalf("cannot discover hub tools: %v", err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "lazyrex", Version: "0.2.0"}, nil)
	// The tool list is fixed for this run (README section 7). Routes refresh on rediscovery.
	for name, t := range s.tools {
		srv.AddTool(&mcp.Tool{Name: name, Description: t.Description, InputSchema: t.InputSchema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return s.call(ctx, name, req.Params.Arguments, req.Params.GetMeta())
			})
	}
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
