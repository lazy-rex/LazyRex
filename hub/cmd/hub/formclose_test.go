package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/lazy-rex/LazyRex/hub/rex"
)

// formCloseHub is a test hub with n open forms (form-<i> in block-<i>) and a fake Rex that
// answers every call with {}, or with the error code fail if it is set.
func formCloseHub(t *testing.T, n int, fail string) *Hub {
	t.Helper()
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(uiSchema); err != nil {
		t.Fatal(err)
	}
	h.ui = &uiState{kick: make(chan struct{}, 1), sess: map[string]string{}}
	h.vs = newViewSvc() // closeViewRow forgets the view's render state
	h.selfClosed = map[string]bool{}
	h.rex = answerRex(t, fail)
	for i := range n {
		if _, err := h.db.Exec(`INSERT INTO views (view_id,plugin,tab,route,status,session_id,block_id) VALUES (?,'p',?,'/forms/f','open','s',?)`,
			fmt.Sprintf("form-%d", i), formTab, fmt.Sprintf("block-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// answerRex is a fake Rex control socket: hello gets welcome, and each request gets {} (or the
// error code fail).
func answerRex(t *testing.T, fail string) *rex.Rex {
	t.Helper()
	return replyRex(t, func(string) (any, string) { return map[string]any{}, fail })
}

// replyRex is a fake Rex control socket that answers each request with reply(method): a payload,
// or an error code.
func replyRex(t *testing.T, reply func(method string) (any, string)) *rex.Rex {
	t.Helper()
	return payloadRex(t, func(method string, _ json.RawMessage) (any, string) { return reply(method) })
}

// payloadRex is replyRex with the request payload: reply(method, payload).
func payloadRex(t *testing.T, reply func(method string, payload json.RawMessage) (any, string)) *rex.Rex {
	t.Helper()
	return limitRex(t, 0, reply)
}

// limitRex is payloadRex with a welcome that states max_message_bytes max (0: no limits).
func limitRex(t *testing.T, max int, reply func(method string, payload json.RawMessage) (any, string)) *rex.Rex {
	t.Helper()
	dir, err := os.MkdirTemp("", "fr") // a unix socket path must be short
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
			var f struct {
				Type, ID, Method string
				Payload          json.RawMessage
			}
			json.Unmarshal(data, &f)
			frame := map[string]any{"type": "welcome"}
			if max > 0 {
				frame["limits"] = map[string]any{"max_message_bytes": max}
			}
			if f.Type == "request" {
				payload, fail := reply(f.Method, f.Payload)
				frame = map[string]any{"type": "response", "id": f.ID, "payload": payload}
				if fail != "" {
					frame = map[string]any{"type": "response", "id": f.ID, "error": map[string]any{"code": fail, "message": "test"}}
				}
			}
			b, _ := json.Marshal(frame)
			if c.Write(ctx, websocket.MessageText, b) != nil {
				return
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
	return r
}

// formClosedCounts counts the form_closed events of each view.
func formClosedCounts(t *testing.T, h *Hub) map[string]int {
	t.Helper()
	b, err := os.ReadFile(h.ev.f.Name())
	if err != nil {
		t.Fatal(err)
	}
	closed := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["event"] == "form_closed" {
			closed[e["view_id"].(string)]++
		}
	}
	return closed
}

// The renderer's exit and the pane's close arrive together for one Esc (lab, ui_slot "layer"),
// and the hub can close the same form at that time. Each close can find the form open; only the
// one that closes the row reports the close: one form_closed, and at most one formEnded (the
// panel return).
func TestFormClosesOnce(t *testing.T) {
	const n = 100
	h := formCloseHub(t, n, "")
	ctx := context.Background()
	for i := range n {
		id, block := fmt.Sprintf("form-%d", i), fmt.Sprintf("block-%d", i)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); <-start; h.uiChildExited(ctx, block) }()
		go func() { defer wg.Done(); <-start; h.uiBlockClosed(ctx, block, false) }()
		go func() {
			defer wg.Done()
			<-start
			if err := h.closeForm(ctx, id, "test"); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
	}
	closed := formClosedCounts(t, h)
	for i := range n {
		if id := fmt.Sprintf("form-%d", i); closed[id] != 1 {
			t.Errorf("%s: form_closed %d times, want 1", id, closed[id])
		}
	}
	// A repeat of each close finds the form closed and reports nothing.
	h.uiChildExited(ctx, "block-0")
	h.uiBlockClosed(ctx, "block-0", false)
	if err := h.closeForm(ctx, "form-0", "test"); err != nil {
		t.Fatal(err)
	}
	if got := formClosedCounts(t, h)["form-0"]; got != 1 {
		t.Fatalf("form_closed %d times after repeats, want 1", got)
	}
}

// A failed block.close leaves the form open, so the hub can try again; nothing is reported.
func TestCloseFormRefusedKeepsRow(t *testing.T) {
	h := formCloseHub(t, 1, "busy")
	if err := h.closeForm(context.Background(), "form-0", "test"); err == nil {
		t.Fatal("a refused block.close was not reported")
	}
	var status string
	h.db.QueryRow(`SELECT status FROM views WHERE view_id = 'form-0'`).Scan(&status)
	if status != "open" || formClosedCounts(t, h)["form-0"] != 0 {
		t.Fatalf("status %q, form_closed %d; want open and none", status, formClosedCounts(t, h)["form-0"])
	}
}

// A database error of the close is returned, and the close is not reported.
func TestCloseFormDatabaseError(t *testing.T) {
	h := formCloseHub(t, 1, "")
	if _, err := h.db.Exec(`CREATE TRIGGER no_close BEFORE UPDATE OF status ON views BEGIN SELECT RAISE(FAIL, 'test close failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := h.closeForm(context.Background(), "form-0", "test"); err == nil || !strings.Contains(err.Error(), "test close failure") {
		t.Fatalf("closeForm: %v, want the database error", err)
	}
	h.uiChildExited(context.Background(), "block-0")
	h.uiBlockClosed(context.Background(), "block-0", false)
	if got := formClosedCounts(t, h)["form-0"]; got != 0 {
		t.Fatalf("form_closed %d times after failed closes, want 0", got)
	}
}
