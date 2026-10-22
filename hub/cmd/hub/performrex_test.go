package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// performCall is one client.perform that a performRex received.
type performCall struct {
	Client, Action string
	Args           json.RawMessage
}

// performRex is a Rex server for unit tests. It answers each client.perform with answer: a result, or
// an error code. Every other request gets an empty response; methodsSeen lists them all.
type performRex struct {
	Socket  string
	mu      sync.Mutex
	calls   []performCall
	methods []string // every request method, in arrival order
}

func newPerformRex(t *testing.T, answer func(c performCall) (result any, code string)) *performRex {
	t.Helper()
	dir, err := os.MkdirTemp("", "fr") // a unix socket path must be short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	fr := &performRex{Socket: filepath.Join(dir, "rex.sock")}
	ln, err := net.Listen("unix", fr.Socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"rex.control.v1"}})
		if err != nil {
			return
		}
		ctx := context.Background()
		var wmu sync.Mutex
		write := func(frame map[string]any) {
			b, _ := json.Marshal(frame)
			wmu.Lock()
			defer wmu.Unlock()
			c.Write(ctx, websocket.MessageText, b)
		}
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var f struct {
				Type, ID, Method string
				Payload          struct {
					ClientID string          `json:"client_id"`
					Action   string          `json:"action"`
					Args     json.RawMessage `json:"args"`
				}
			}
			json.Unmarshal(data, &f)
			if f.Type != "request" {
				write(map[string]any{"type": "welcome"})
				continue
			}
			fr.mu.Lock()
			fr.methods = append(fr.methods, f.Method)
			fr.mu.Unlock()
			if f.Method != "client.perform" {
				write(map[string]any{"type": "response", "id": f.ID, "payload": map[string]any{}})
				continue
			}
			pc := performCall{f.Payload.ClientID, f.Payload.Action, f.Payload.Args}
			fr.mu.Lock()
			fr.calls = append(fr.calls, pc)
			fr.mu.Unlock()
			// Each answer runs on its own goroutine: an answer may wait without blocking the reads.
			go func(id string) {
				result, code := answer(pc)
				if code != "" {
					write(map[string]any{"type": "response", "id": id, "error": map[string]any{"code": code, "message": "test"}})
					return
				}
				write(map[string]any{"type": "response", "id": id, "payload": map[string]any{"result": result}})
			}(f.ID)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fr
}

// dial connects a hub-side Rex client to fr.
func (fr *performRex) dial(t *testing.T) *rex.Rex {
	t.Helper()
	r, err := rex.Dial(context.Background(), fr.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// methodsSeen lists the method of every request so far, in arrival order.
func (fr *performRex) methodsSeen() []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return slices.Clone(fr.methods)
}

// actions lists the actions of the client.perform calls so far, in arrival order.
func (fr *performRex) actions() []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	var out []string
	for _, c := range fr.calls {
		out = append(out, c.Action)
	}
	return out
}
