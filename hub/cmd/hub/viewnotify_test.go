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
	"time"

	"github.com/coder/websocket"
	"github.com/lazy-rex/LazyRex/hub/rex"
)

// heldPerform is one view.changed request that the fake Rex holds until the test answers it.
type heldPerform struct {
	id, client, view, attachment, revision string
}

// holdRex is a fake Rex that holds every view.changed request until the test answers it. Every
// other request gets a null result at once. It counts the requests that have no outcome yet.
type holdRex struct {
	mu      sync.Mutex
	held    []heldPerform // in arrival order, with no outcome yet
	all     []heldPerform // every view.changed request
	answer  func(id string, fail string)
	arrived chan struct{}
}

func newHoldRex(t *testing.T) (*rex.Rex, *holdRex) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hr") // a unix socket path must be short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "rex.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	hr := &holdRex{arrived: make(chan struct{}, 100)}
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
		hr.mu.Lock()
		hr.answer = func(id, fail string) {
			if fail != "" {
				write(map[string]any{"type": "response", "id": id, "error": map[string]any{"code": fail, "message": "test"}})
				return
			}
			write(map[string]any{"type": "response", "id": id, "payload": map[string]any{"received": true}})
		}
		hr.mu.Unlock()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var f struct {
				Type, ID, Method string
				Payload          struct {
					ClientID string `json:"client_id"`
					Action   string `json:"action"`
					Args     struct {
						ViewID     string `json:"view_id"`
						Attachment string `json:"attachment"`
						Revision   string `json:"revision"`
					} `json:"args"`
				}
			}
			json.Unmarshal(data, &f)
			if f.Type != "request" {
				write(map[string]any{"type": "welcome"})
				continue
			}
			if f.Method == "client.perform" && f.Payload.Action == "view.changed" {
				p := heldPerform{f.ID, f.Payload.ClientID, f.Payload.Args.ViewID, f.Payload.Args.Attachment, f.Payload.Args.Revision}
				hr.mu.Lock()
				hr.held = append(hr.held, p)
				hr.all = append(hr.all, p)
				hr.mu.Unlock()
				hr.arrived <- struct{}{}
				continue
			}
			write(map[string]any{"type": "response", "id": f.ID, "payload": nil})
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	r, err := rex.Dial(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, hr
}

// release answers the oldest held request of the attachment. fail is a Rex error code, or "".
func (hr *holdRex) release(t *testing.T, attachment, fail string) heldPerform {
	t.Helper()
	hr.mu.Lock()
	defer hr.mu.Unlock()
	for i, p := range hr.held {
		if p.attachment == attachment {
			hr.held = append(hr.held[:i], hr.held[i+1:]...)
			hr.answer(p.id, fail)
			return p
		}
	}
	t.Fatalf("no request of %s is held", attachment)
	return heldPerform{}
}

// outstanding returns the held requests of the attachment.
func (hr *holdRex) outstanding(attachment string) []heldPerform {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	var out []heldPerform
	for _, p := range hr.held {
		if p.attachment == attachment {
			out = append(out, p)
		}
	}
	return out
}

// sent returns every request of the attachment.
func (hr *holdRex) sent(attachment string) []heldPerform {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	var out []heldPerform
	for _, p := range hr.all {
		if p.attachment == attachment {
			out = append(out, p)
		}
	}
	return out
}

// waitSent waits until the attachment has n requests in total.
func (hr *holdRex) waitSent(t *testing.T, attachment string, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(hr.sent(attachment)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d requests, want %d", attachment, len(hr.sent(attachment)), n)
		}
		select {
		case <-hr.arrived:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// settle gives late requests time to arrive, then checks the totals of each attachment.
func (hr *holdRex) settle(t *testing.T, want map[string]int) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	for att, n := range want {
		if got := len(hr.sent(att)); got != n {
			t.Errorf("%s: %d requests in total, want %d: %+v", att, got, n, hr.sent(att))
		}
	}
}

// notifyHub is a hub with one open view v1 of the plugin p: the composer client "composer" with
// att-c, and the printer client "printer" with att-p. Each render of v1 returns a new tree.
func notifyHub(t *testing.T) (*Hub, *holdRex) {
	t.Helper()
	h := testUIHub(t)
	for _, m := range []string{pairSchema, `ALTER TABLE views ADD COLUMN context TEXT NOT NULL DEFAULT ''`, `ALTER TABLE views ADD COLUMN layout TEXT NOT NULL DEFAULT ''`, `ALTER TABLE ui_tabs ADD COLUMN status TEXT NOT NULL DEFAULT ''`, `ALTER TABLE ui_tabs ADD COLUMN layout TEXT NOT NULL DEFAULT ''`} {
		if _, err := h.db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			t.Fatal(err)
		}
	}
	if _, err := h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, session_id, block_id, client_id, attachment) VALUES
		('v1', 'p', 'home', '/a', 'open', 's1', 'b1', 'composer', 'att-c')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO view_transcripts (view_id, status, client_id, attachment) VALUES ('v1', 'open', 'printer', 'att-p')`); err != nil {
		t.Fatal(err)
	}
	h.vs = depsHub().vs
	h.vs.snaps, h.vs.notify = map[string]*snapshot{}, map[string]bool{}
	h.plugins = map[string]*Plugin{"p": {ClientID: "c1"}}
	r, hr := newHoldRex(t)
	h.rex = r
	n := 0
	viewRenderHook = func(h *Hub, ctx context.Context, plugin string, args map[string]any, out any) error {
		n++
		b, _ := json.Marshal(map[string]any{"tree": map[string]any{"title": fmt.Sprint("render ", n), "back": nil, "nodes": []any{}}})
		return json.Unmarshal(b, out)
	}
	t.Cleanup(func() { viewRenderHook = nil })
	// The senders end before the next test: none reads a test value of viewChangedRetry later.
	t.Cleanup(func() {
		h.vs.mu.Lock()
		var left []*viewNotifier
		for k, n := range h.vs.senders {
			h.vs.retireLocked(k, n)
			left = append(left, n)
		}
		h.vs.mu.Unlock()
		for _, n := range left {
			<-n.done
		}
	})
	return h, hr
}

func (h *Hub) renderOnce(t *testing.T) {
	t.Helper()
	h.markView("p", "v1", causeTest)
	h.renderViews(context.Background(), "p")
}

// A renderer that is busy holds the view.changed request. 20 renders during that time send no
// more requests to it; its answer sends one more, with the latest revision; then none (timeout
// audit, row 10). Before, each render sent its own request: 21 to each renderer.
func TestViewChangedOneOutstanding(t *testing.T) {
	h, hr := notifyHub(t)
	h.renderOnce(t)
	hr.waitSent(t, "att-c", 1)
	hr.waitSent(t, "att-p", 1)
	for i := 0; i < 20; i++ {
		h.renderOnce(t)
	}
	hr.settle(t, map[string]int{"att-c": 1, "att-p": 1})
	for _, att := range []string{"att-c", "att-p"} {
		hr.release(t, att, "")
		hr.waitSent(t, att, 2)
		if got := hr.sent(att)[1].revision; got != revString(h.vs.rev) {
			t.Errorf("%s: the second request tells revision %s, want the latest %d", att, got, h.vs.rev)
		}
		hr.release(t, att, "")
	}
	hr.settle(t, map[string]int{"att-c": 2, "att-p": 2})
}

// The composer and the printer have separate requests: a busy composer does not hold the
// printer's notifications.
func TestViewChangedRolesSeparate(t *testing.T) {
	h, hr := notifyHub(t)
	h.renderOnce(t)
	hr.waitSent(t, "att-c", 1)
	for i := 2; i <= 4; i++ {
		hr.waitSent(t, "att-p", i-1)
		hr.release(t, "att-p", "")
		h.renderOnce(t)
		hr.waitSent(t, "att-p", i)
	}
	if n := len(hr.outstanding("att-c")); n != 1 {
		t.Errorf("composer: %d requests outstanding, want 1", n)
	}
	hr.settle(t, map[string]int{"att-c": 1, "att-p": 4})
}

// A new composer attachment gets its request at once, while the request to the old attachment is
// still held. The old answer, later, sends nothing more.
func TestViewChangedNewAttachment(t *testing.T) {
	h, hr := notifyHub(t)
	h.renderOnce(t)
	hr.waitSent(t, "att-c", 1)
	h.db.Exec(`UPDATE views SET attachment = 'att-c2' WHERE view_id = 'v1'`)
	h.renderOnce(t)
	hr.waitSent(t, "att-c2", 1)
	h.renderOnce(t)
	hr.release(t, "att-c", "")
	hr.settle(t, map[string]int{"att-c": 1, "att-c2": 1})
	hr.release(t, "att-c2", "")
	hr.waitSent(t, "att-c2", 2)
	hr.release(t, "att-c2", "")
	hr.settle(t, map[string]int{"att-c": 1, "att-c2": 2})
}

// notifierOf returns the composer notifier of v1 for the attachment, or nil.
func notifierOf(h *Hub, role, attachment string) *viewNotifier {
	h.vs.mu.Lock()
	defer h.vs.mu.Unlock()
	return h.vs.senders[notifyKey{"v1", role, attachment}]
}

// waitDone waits until the sender has ended.
func waitDone(t *testing.T, n *viewNotifier, what string) {
	t.Helper()
	select {
	case <-n.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: the sender did not end", what)
	}
}

// A failed request is tried again, three attempts in total, one at a time. Rex's not_found (the
// renderer's client is gone) ends the delivery at once. A render after a failure starts a fresh
// count of attempts.
func TestViewChangedFailures(t *testing.T) {
	old := viewChangedRetry
	viewChangedRetry = 10 * time.Millisecond
	defer func() { viewChangedRetry = old }()

	h, hr := notifyHub(t)
	h.db.Exec(`DELETE FROM view_transcripts`)
	h.renderOnce(t)
	n := notifierOf(h, roleComposer, "att-c")
	for i := 1; i <= 3; i++ {
		hr.waitSent(t, "att-c", i)
		if k := len(hr.outstanding("att-c")); k != 1 {
			t.Fatalf("attempt %d: %d requests outstanding, want 1", i, k)
		}
		hr.release(t, "att-c", "refused")
	}
	waitDone(t, n, "three failures")
	hr.settle(t, map[string]int{"att-c": 3})

	h.renderOnce(t)
	n = notifierOf(h, roleComposer, "att-c")
	hr.waitSent(t, "att-c", 4)
	hr.release(t, "att-c", "not_found")
	waitDone(t, n, "not_found")
	hr.settle(t, map[string]int{"att-c": 4})

	// Two failures, then a render: the new revision gets three attempts of its own.
	h.renderOnce(t)
	hr.waitSent(t, "att-c", 5)
	hr.release(t, "att-c", "refused")
	hr.waitSent(t, "att-c", 6)
	h.renderOnce(t)
	hr.release(t, "att-c", "refused")
	for i := 7; i <= 9; i++ {
		hr.waitSent(t, "att-c", i)
		hr.release(t, "att-c", "refused")
	}
	hr.settle(t, map[string]int{"att-c": 9})
	if got, want := hr.sent("att-c")[6].revision, revString(h.vs.rev); got != want {
		t.Errorf("the fresh count tells revision %s, want %s", got, want)
	}
}

// A view close and the hub stop end the sender while its request is still held: the local wait
// ends at once, and a later answer sends nothing.
func TestViewChangedCloseAndStop(t *testing.T) {
	h, hr := notifyHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.vs.ctx = ctx
	h.renderOnce(t)
	hr.waitSent(t, "att-c", 1)
	hr.waitSent(t, "att-p", 1)
	c, p := notifierOf(h, roleComposer, "att-c"), notifierOf(h, rolePrinter, "att-p")
	h.renderOnce(t)
	h.forgetView("v1")
	waitDone(t, c, "view close (composer)")
	waitDone(t, p, "view close (printer)")
	hr.release(t, "att-c", "")
	hr.release(t, "att-p", "")
	hr.settle(t, map[string]int{"att-c": 1, "att-p": 1})

	h.renderOnce(t)
	hr.waitSent(t, "att-c", 2)
	hr.waitSent(t, "att-p", 2)
	c, p = notifierOf(h, roleComposer, "att-c"), notifierOf(h, rolePrinter, "att-p")
	cancel()
	waitDone(t, c, "hub stop (composer)")
	waitDone(t, p, "hub stop (printer)")
	h.vs.mu.Lock()
	left := len(h.vs.senders)
	h.vs.mu.Unlock()
	if left != 0 {
		t.Errorf("%d notifiers after the stop", left)
	}
}

// A render pass that no longer finds the attachment (the view detached) ends its sender.
func TestViewChangedPrune(t *testing.T) {
	h, hr := notifyHub(t)
	h.renderOnce(t)
	hr.waitSent(t, "att-p", 1)
	p := notifierOf(h, rolePrinter, "att-p")
	h.db.Exec(`UPDATE view_transcripts SET attachment = '' WHERE view_id = 'v1'`)
	h.renderOnce(t)
	waitDone(t, p, "printer detached")
}

// Notifications at the same time start one sender.
func TestViewChangedConcurrentStart(t *testing.T) {
	h, hr := notifyHub(t)
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(rev int64) {
			defer wg.Done()
			h.tellViewChanged("p", roleComposer, "composer", "v1", "att-c", rev)
		}(int64(i))
	}
	wg.Wait()
	hr.waitSent(t, "att-c", 1)
	hr.settle(t, map[string]int{"att-c": 1})
}

// A render that comes just as the sender ends is still sent: the end and the removal of the
// sender are one step (Codex review of 8025d6b). Before, the sender decided to stop, released
// the lock, and removed itself later; a render in between raised its revision, and nothing
// sent it.
func TestViewChangedEndHandoff(t *testing.T) {
	h, hr := notifyHub(t)
	h.db.Exec(`DELETE FROM view_transcripts`)
	var once sync.Once
	var second *viewNotifier // the sender that the render at the end starts
	ended := make(chan struct{})
	senderEndHook = func(k notifyKey) {
		once.Do(func() {
			h.tellViewChanged("p", roleComposer, "composer", "v1", "att-c", 99)
			second = notifierOf(h, roleComposer, "att-c")
			close(ended)
		})
	}
	// The hook is reset only after the second sender has ended: its own end reads the hook.
	defer func() {
		if second != nil {
			waitDone(t, second, "the second sender")
		}
		senderEndHook = nil
	}()
	h.renderOnce(t)
	hr.waitSent(t, "att-c", 1)
	hr.release(t, "att-c", "")
	<-ended
	hr.waitSent(t, "att-c", 2)
	if got := hr.sent("att-c")[1].revision; got != "99" {
		t.Errorf("the render at the end tells revision %s, want 99", got)
	}
	if second == nil {
		t.Fatal("the render at the end started no sender")
	}
	hr.release(t, "att-c", "")
	hr.settle(t, map[string]int{"att-c": 2})
}

// A notification with an unchanged revision, while a request is held, sends one more request
// after the answer: an operation completion asks for view.changed with an unchanged tree
// (vs.notify; Codex review of c2e9a78). Before, the same revision did not count, and the answer
// ended the sender.
func TestViewChangedSameRevision(t *testing.T) {
	h, hr := notifyHub(t)
	h.db.Exec(`DELETE FROM view_transcripts`)
	h.tellViewChanged("p", roleComposer, "composer", "v1", "att-c", 1)
	hr.waitSent(t, "att-c", 1)
	h.tellViewChanged("p", roleComposer, "composer", "v1", "att-c", 1)
	hr.release(t, "att-c", "")
	hr.waitSent(t, "att-c", 2)
	if got := hr.sent("att-c")[1].revision; got != "1" {
		t.Errorf("the second request tells revision %s, want 1", got)
	}
	hr.release(t, "att-c", "")
	hr.settle(t, map[string]int{"att-c": 2})
}
