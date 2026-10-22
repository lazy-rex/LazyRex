package rex

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// peerServer is a control socket whose peer runs script after the upgrade. closed gets the error
// that ends the peer's next read: the peer saw the connection close.
func peerServer(t *testing.T, script func(c *websocket.Conn)) (socket string, closed chan error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rx") // a unix socket path must be short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket = filepath.Join(dir, "rex.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	closed = make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"rex.control.v1"}})
		if err != nil {
			return
		}
		script(c)
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				closed <- err
				return
			}
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return socket, closed
}

func readHello(c *websocket.Conn) { c.Read(context.Background()) }

// Every DialHello failure after the WebSocket dial closes the connection at once, so the peer sees
// it close, and the dial ends with its context.
func TestDialFailureClosesTheConnection(t *testing.T) {
	cases := []struct {
		what   string
		script func(c *websocket.Conn)
		hello  map[string]any
		ctx    time.Duration
	}{
		{"invalid welcome JSON", func(c *websocket.Conn) {
			readHello(c)
			c.Write(context.Background(), websocket.MessageText, []byte("{not json"))
		}, nil, 5 * time.Second},
		{"another frame type", func(c *websocket.Conn) {
			readHello(c)
			c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"event"}`))
		}, nil, 5 * time.Second},
		{"no welcome (stall)", readHello, nil, 300 * time.Millisecond},
		{"a hello that does not encode", func(c *websocket.Conn) {}, map[string]any{"bad": make(chan int)}, 5 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			socket, closed := peerServer(t, c.script)
			ctx, cancel := context.WithTimeout(context.Background(), c.ctx)
			defer cancel()
			start := time.Now()
			r, err := DialHello(ctx, socket, c.hello)
			if err == nil {
				r.Close()
				t.Fatal("the dial succeeded")
			}
			if d := time.Since(start); d > c.ctx+time.Second {
				t.Errorf("the dial took %s", d)
			}
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("the peer did not see the connection close")
			}
		})
	}
}

// CloseNow does not wait for a peer that stops reading after the welcome.
func TestCloseNowDoesNotWaitForASilentPeer(t *testing.T) {
	socket, closed := peerServer(t, func(c *websocket.Conn) {
		readHello(c)
		c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"welcome"}`))
		time.Sleep(time.Hour) // never reads again
	})
	r, err := Dial(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r.CloseNow()
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("CloseNow took %s", d)
	}
	_ = closed // the peer sleeps; its read never runs
}
