package rex

// Minimal Rex control client (copied from spike 1): WebSocket /api/control, subprotocol rex.control.v1,
// over the server's Unix socket. See docs/rex-protocol.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

type Rex struct {
	conn    *websocket.Conn
	closed  atomic.Bool
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[string]chan frame
	Events  chan json.RawMessage
	Welcome json.RawMessage
}

type frame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *RexError       `json:"error,omitempty"`
}

type RexError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RexError) Error() string { return e.Code + ": " + e.Message }

// SocketPath turns REX_SERVER (unix:///path, percent-encoded) into a filesystem path.
func SocketPath(server string) (string, error) {
	if !strings.HasPrefix(server, "unix://") {
		return "", fmt.Errorf("only unix:// servers are supported, got %q", server)
	}
	return url.PathUnescape(strings.TrimPrefix(server, "unix://"))
}

func Dial(ctx context.Context, socket string) (*Rex, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	conn, _, err := websocket.Dial(ctx, "http://rex/api/control", &websocket.DialOptions{
		HTTPClient:   client,
		Subprotocols: []string{"rex.control.v1"},
	})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", socket, err)
	}
	conn.SetReadLimit(16 << 20) // session.list_blocks and block methods can be large
	r := &Rex{conn: conn, pending: map[string]chan frame{}, Events: make(chan json.RawMessage, 256)}

	if err := r.write(ctx, frame{Type: "hello"}); err != nil {
		return nil, err
	}
	var welcome frame
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("read welcome: %w", err)
	}
	if err := json.Unmarshal(data, &welcome); err != nil || welcome.Type != "welcome" {
		return nil, fmt.Errorf("expected welcome, got %s", data)
	}
	r.Welcome = data
	go r.readLoop()
	return r, nil
}

func (r *Rex) write(ctx context.Context, f frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return r.conn.Write(ctx, websocket.MessageText, data)
}

// readLoop never blocks on handlers: events go to a buffered channel, responses to their waiters.
func (r *Rex) readLoop() {
	defer close(r.Events)
	defer r.closed.Store(true)
	for {
		_, data, err := r.conn.Read(context.Background())
		if err != nil {
			r.mu.Lock()
			for id, ch := range r.pending {
				ch <- frame{Error: &RexError{Code: "disconnected", Message: err.Error()}}
				delete(r.pending, id)
			}
			r.mu.Unlock()
			return
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.Type {
		case "response":
			r.mu.Lock()
			ch := r.pending[f.ID]
			delete(r.pending, f.ID)
			r.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		case "event":
			r.Events <- data
		}
	}
}

// Closed reports whether the connection has ended.
func (r *Rex) Closed() bool { return r.closed.Load() }

// Call sends a request and waits for its response.
func (r *Rex) Call(ctx context.Context, method string, payload any, out any) error {
	id := fmt.Sprintf("h%d", r.nextID.Add(1))
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ch := make(chan frame, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()
	if err := r.write(ctx, frame{Type: "request", ID: id, Method: method, Payload: raw}); err != nil {
		return err
	}
	select {
	case f := <-ch:
		if f.Error != nil {
			return fmt.Errorf("%s: %w", method, f.Error)
		}
		if out != nil && len(f.Payload) > 0 {
			return json.Unmarshal(f.Payload, out)
		}
		return nil
	case <-ctx.Done():
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return ctx.Err()
	}
}

// SendOnly writes a request frame and does not wait for its response.
// Lab fault injection uses it to produce an unknown request outcome.
func (r *Rex) SendOnly(ctx context.Context, method string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return r.write(ctx, frame{Type: "request", ID: fmt.Sprintf("h%d", r.nextID.Add(1)), Method: method, Payload: raw})
}

// BlockCall invokes a block method, such as com.superlogical.terminal.process.
func (r *Rex) BlockCall(ctx context.Context, method, sessionID, blockID string, args any, out any) error {
	if args == nil {
		args = map[string]any{}
	}
	return r.Call(ctx, method, map[string]any{"session_id": sessionID, "block_id": blockID, "args": args}, out)
}
