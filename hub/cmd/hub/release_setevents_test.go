package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/lazy-rex/LazyRex/hub/rex"
)

// LAB16: the tool's lab connection gets the events of each attached session and reads none. Here
// a fake Rex sends 300 events (over the client buffer of 256) before each response. With
// drainEvents the call gets its response.
func TestReleaseSetDrainEvents(t *testing.T) {
	dir, err := os.MkdirTemp("", "de") // a unix socket path must be short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "rex.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"rex.control.v1"}})
		if err != nil {
			return
		}
		ctx := context.Background()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var f struct{ Type, ID string }
			json.Unmarshal(data, &f)
			frames := []map[string]any{{"type": "welcome"}}
			if f.Type == "request" {
				frames = nil
				for i := 0; i < 300; i++ {
					frames = append(frames, map[string]any{"type": "event", "event": "terminal.output", "seq": i})
				}
				frames = append(frames, map[string]any{"type": "response", "id": f.ID, "payload": map[string]any{"ok": true}})
			}
			for _, fr := range frames {
				b, _ := json.Marshal(fr)
				if c.Write(ctx, websocket.MessageText, b) != nil {
					return
				}
			}
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	r, err := rex.Dial(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	drainEvents(r)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var out struct{ OK bool }
		err := r.Call(ctx, "session.attach", map[string]any{}, &out)
		cancel()
		if err != nil || !out.OK {
			t.Fatalf("call %d after 300 events: %+v, %v", i+1, out, err)
		}
	}
}
