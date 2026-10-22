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
	"strconv"
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

// Error is "code: message". A hub refusal already starts its message with "code: " (the hub's
// refuse), so that message is shown alone, not as "no_app: no_app: …".
func (e *wireError) Error() string {
	if strings.HasPrefix(e.Message, e.Code+": ") {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

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

// realSocket accepts only a default Rex server socket (knownDefault). The hub passes -real-server
// to renderers only when it runs with -real-server itself (an approved run). A Linux pane has no
// REX_SERVER: then the renderer takes this user's default socket that exists.
func realSocket(server string) (string, error) {
	home, _ := os.UserHomeDir()
	return realSocketFor(server, home, os.Getenv("XDG_RUNTIME_DIR"), os.Getuid())
}

func realSocketFor(server, home, xdg string, uid int) (string, error) {
	if server == "" {
		for _, c := range defaultSockets(home, xdg, uid) {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
		return "", fmt.Errorf("-real-server is set, REX_SERVER is empty, and no default Rex socket exists")
	}
	if !strings.HasPrefix(server, "unix://") {
		return "", fmt.Errorf("A Unix socket is required")
	}
	if !knownDefault(server, home, xdg, uid) {
		return "", fmt.Errorf("-real-server is set, but REX_SERVER is not the real Rex server")
	}
	p, _ := url.PathUnescape(strings.TrimPrefix(server, "unix://"))
	return filepath.Clean(p), nil
}

// knownDefault and its helpers are a copy of hub/internal/rexreal (another Go module): keep the
// code and TestKnownDefault equal in both. The default sockets of macOS and Linux, of any home or
// uid, count on either host.
func knownDefault(endpoint, home, xdg string, uid int) bool {
	p, ok := socketPath(endpoint)
	if !ok {
		return false
	}
	for _, c := range defaultSockets(home, xdg, uid) {
		if resolveSocket(c) == p {
			return true
		}
	}
	if strings.HasSuffix(p, "/Library/Application Support/rex/server.sock") {
		return true
	}
	if rest, ok := strings.CutPrefix(p, "/run/user/"); ok {
		u, tail, _ := strings.Cut(rest, "/")
		if _, err := strconv.Atoi(u); err == nil && tail == "rex/server.sock" {
			return true
		}
	}
	return false
}

// defaultSockets: macOS ~/Library/Application Support/rex/server.sock; Linux
// $XDG_RUNTIME_DIR/rex/server.sock, then /run/user/<uid>/rex/server.sock (Rex's path without
// XDG_RUNTIME_DIR). A relative home or XDG_RUNTIME_DIR makes no candidate.
func defaultSockets(home, xdg string, uid int) []string {
	var c []string
	if filepath.IsAbs(home) {
		c = append(c, filepath.Join(home, "Library", "Application Support", "rex", "server.sock"))
	}
	if filepath.IsAbs(xdg) {
		c = append(c, filepath.Join(xdg, "rex", "server.sock"))
	}
	return append(c, filepath.Join("/run/user", strconv.Itoa(uid), "rex", "server.sock"))
}

func socketPath(endpoint string) (string, bool) {
	p, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok {
		return "", false
	}
	p, err := url.PathUnescape(p)
	if err != nil || !filepath.IsAbs(p) {
		return "", false
	}
	return resolveSocket(p), true
}

func resolveSocket(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(r, filepath.Base(p))
	}
	return p
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
