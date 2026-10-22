package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/coder/websocket"
)

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *wireError) Error() string { return e.Code + ": " + e.Message }

type frame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wire struct {
	conn    *websocket.Conn
	next    atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan frame
	done    chan struct{}
	event   func(json.RawMessage)
}

// realSocket accepts only the user's real Rex server socket. The hub passes -real-server to
// renderers only when it runs with -real-server itself (an approved run).
func realSocket(server string) (string, error) {
	p, err := url.PathUnescape(strings.TrimPrefix(server, "unix://"))
	if err != nil || !strings.HasPrefix(server, "unix://") {
		return "", fmt.Errorf("A Unix socket is required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if filepath.Clean(p) != filepath.Join(home, "Library/Application Support/rex/server.sock") {
		return "", fmt.Errorf("-real-server is set, but REX_SERVER is not the real Rex server")
	}
	return filepath.Clean(p), nil
}

func labSocket(server string) (string, error) {
	if !strings.HasPrefix(server, "unix://") {
		return "", fmt.Errorf("A lab Unix socket is required")
	}
	p, err := url.PathUnescape(strings.TrimPrefix(server, "unix://"))
	if err != nil {
		return "", err
	}
	p = filepath.Clean(p)
	// The spike accepts only temporary lab sockets, including macOS /tmp aliases.
	if !strings.HasPrefix(p, "/tmp/") && !strings.HasPrefix(p, "/private/tmp/") {
		return "", fmt.Errorf("Refusing a socket outside /tmp; use spikes/lab/server.sh")
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(resolved, "/tmp/") && !strings.HasPrefix(resolved, "/private/tmp/") {
		return "", fmt.Errorf("The lab socket resolves outside /tmp")
	}
	return p, nil
}

func dialWire(ctx context.Context, socket string, event func(json.RawMessage)) (*wire, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	c, _, err := websocket.Dial(ctx, "http://rex/api/control", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}, Subprotocols: []string{"rex.control.v1"}})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(16 << 20)
	w := &wire{conn: c, pending: map[string]chan frame{}, done: make(chan struct{}), event: event}
	if err = w.write(ctx, frame{Type: "hello"}); err != nil {
		c.CloseNow()
		return nil, err
	}
	_, raw, err := c.Read(ctx)
	var welcome frame
	if err != nil {
		c.CloseNow()
		return nil, err
	}
	if json.Unmarshal(raw, &welcome) != nil || welcome.Type != "welcome" {
		c.CloseNow()
		return nil, fmt.Errorf("Rex did not send welcome")
	}
	go w.read()
	return w, nil
}

func (w *wire) write(ctx context.Context, f frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return w.conn.Write(ctx, websocket.MessageText, b)
}

func (w *wire) read() {
	defer close(w.done)
	defer w.conn.CloseNow()
	for {
		_, b, err := w.conn.Read(context.Background())
		if err != nil {
			return
		}
		var f frame
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		if f.Type == "event" {
			w.event(f.Payload)
			continue
		}
		if f.Type != "response" {
			continue
		}
		w.mu.Lock()
		ch := w.pending[f.ID]
		w.mu.Unlock()
		if ch != nil {
			select {
			case ch <- f:
			default:
			}
		}
	}
}

func (w *wire) call(ctx context.Context, method string, args any, out any) error {
	id := fmt.Sprintf("view-%d", w.next.Add(1))
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	ch := make(chan frame, 1)
	w.mu.Lock()
	w.pending[id] = ch
	w.mu.Unlock()
	defer func() { w.mu.Lock(); delete(w.pending, id); w.mu.Unlock() }()
	if err = w.write(ctx, frame{Type: "request", ID: id, Method: method, Payload: raw}); err != nil {
		return err
	}
	select {
	case f := <-ch:
		if f.Error != nil {
			return f.Error
		}
		if out != nil {
			return json.Unmarshal(f.Payload, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return fmt.Errorf("Rex disconnected; the request outcome can be unknown")
	}
}

func (w *wire) perform(ctx context.Context, hub, action string, args any, out any) error {
	var reply struct {
		Result json.RawMessage `json:"result"`
	}
	if err := w.call(ctx, "client.perform", map[string]any{"client_id": hub, "action": action, "args": args, "timeout_ms": 10000}, &reply); err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(reply.Result, out)
	}
	return nil
}

func (w *wire) resolve(ctx context.Context) (string, error) {
	type candidate struct {
		ClientID string `json:"client_id"`
	}
	var result struct {
		Results []struct {
			Status     string      `json:"status"`
			Target     candidate   `json:"target"`
			Candidates []candidate `json:"candidates"`
		} `json:"results"`
	}
	if err := w.call(ctx, "target.resolve", map[string]any{"kinds": []string{"client"}, "queries": []string{"lazyrex-hub"}}, &result); err != nil {
		return "", err
	}
	if len(result.Results) != 1 {
		return "", fmt.Errorf("Unexpected target.resolve reply: %d results", len(result.Results))
	}
	r := result.Results[0]
	var candidates []candidate
	switch r.Status {
	case "resolved":
		if r.Target.ClientID == "" {
			return "", fmt.Errorf("Unexpected target.resolve reply: a target without a client ID")
		}
		candidates = []candidate{r.Target}
	case "ambiguous":
		if len(r.Candidates) == 0 {
			return "", fmt.Errorf("Unexpected target.resolve reply: ambiguous without candidates")
		}
		for _, c := range r.Candidates {
			if c.ClientID == "" {
				return "", fmt.Errorf("Unexpected target.resolve reply: a candidate without a client ID")
			}
		}
		candidates = r.Candidates
	case "not_found": // Rex sends no candidates (lab, 2026-10-22)
		if len(r.Candidates) > 0 {
			return "", fmt.Errorf("Unexpected target.resolve reply: not_found with candidates")
		}
	default:
		return "", fmt.Errorf("Unexpected target.resolve reply: status %q", r.Status)
	}
	var live []string
	for _, c := range candidates {
		var ins struct {
			Client struct {
				State string `json:"connection_state"`
			} `json:"client"`
		}
		if err := w.call(ctx, "client.inspect", map[string]any{"client_id": c.ClientID}, &ins); err != nil {
			return "", fmt.Errorf("Cannot inspect the hub client %s: %w", c.ClientID, err)
		}
		// Any state but connected (for example lost) is a client that is not a hub now. No state
		// is an incomplete answer, not a lost client.
		if ins.Client.State == "" {
			return "", fmt.Errorf("Cannot inspect the hub client %s: the reply has no connection state", c.ClientID)
		}
		if ins.Client.State == "connected" {
			live = append(live, c.ClientID)
		}
	}
	if len(live) != 1 {
		return "", &hubCount{len(live)}
	}
	return live[0], nil
}

// hubCount is a complete discovery that found no connected hub or more than one. resolve returns it
// only after target.resolve and every client.inspect succeeded; any other failure is an ordinary
// error. Its text is the pane's status. The notice is one row cut at the pane's width, so the text starts
// with the action: "No hub: run LazyRex: Start Hub." is 31 columns, whole in a 38-column pane.
type hubCount struct{ n int }

func (e *hubCount) Error() string {
	if e.n == 0 {
		return "No hub: run LazyRex: Start Hub. If the palette has no such entry, run lazyrex-hub setup and the Rex config reload that it prints. This pane retries the connection automatically."
	}
	return fmt.Sprintf("%d LazyRex hubs are connected. This pane needs exactly one.", e.n)
}

// paneSafe is a copy of paneSafe in hub/cmd/hub/restartlog.go (another module): it maps every
// control character except tab to "?" (C0, DEL, C1 and invalid UTF-8), so text from Rex or the hub
// cannot move the cursor or change the terminal.
func paneSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\t' && (r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f || r == utf8.RuneError) {
			return '?'
		}
		return r
	}, s)
}
