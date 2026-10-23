package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// peerHub is a hub named name with one peer and an installed, running plugin "messaging" (client
// c-<name>). Its Rex has the Unix socket's 1 MiB frame limit and answers client.perform with
// receive(args of the perform).
func peerHub(t *testing.T, name, peer string, receive func(args map[string]any) (any, string)) *Hub {
	t.Helper()
	db, err := openDB(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := Config{HubName: name, Peers: []PeerConfig{{Name: peer, Server: "lab-" + peer}}}
	h := &Hub{db: db, cfg: cfg, plugins: map[string]*Plugin{"messaging": {Name: "messaging", ClientID: "c-" + name, entry: &configEntry{}}}, vs: newViewSvc()}
	h.peers = newPeerSet(cfg)
	h.rex = limitRex(t, 1<<20, func(method string, payload json.RawMessage) (any, string) {
		var p struct {
			Args map[string]any `json:"args"`
		}
		json.Unmarshal(payload, &p)
		if method != "client.perform" || receive == nil {
			return map[string]any{}, ""
		}
		res, code := receive(p.Args)
		return map[string]any{"result": res}, code
	})
	return h
}

// linkHubs connects a and b directly: a peer call runs the action in the other hub's dispatch. A
// call fails while down() is true.
func linkHubs(a, b *Hub, down func() bool) {
	call := func(to *Hub) func(context.Context, PeerConfig, string, any) ([]byte, error) {
		return func(ctx context.Context, _ PeerConfig, action string, args any) ([]byte, error) {
			if down() {
				return nil, errors.New("exit status 1: dial: connection refused")
			}
			raw, _ := json.Marshal(args)
			res, err := to.dispatch(ctx, perform{Action: action, Args: raw})
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		}
	}
	a.peers.call, b.peers.call = call(b), call(a)
}

func peerSendAs(h *Hub, client, peer, key, body string) (map[string]any, error) {
	raw, _ := json.Marshal(map[string]any{"hub": peer, "key": key, "body": body})
	p := perform{Action: "hub.peer.send", Args: raw}
	p.Requester.ClientID = client
	res, err := h.dispatch(context.Background(), p)
	m, _ := res.(map[string]any)
	return m, err
}

func errCode(err error) string {
	var se *storeError
	if errors.As(err, &se) {
		return storeErrorCode(se)
	}
	return ""
}

func outboxOpen(t *testing.T, h *Hub, peer string) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(`SELECT count(*) FROM peer_outbox WHERE peer = ? AND acked = 0`, peer).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPeerSendKey: a send gives a seq; the same key and body gives the same seq, also after the ack
// deleted the body; another body is key_conflict, also after the ack. A seq never repeats after
// the outbox is empty. Only a plugin can send, only to a peer, and a body has at most 64 KiB.
func TestPeerSendKey(t *testing.T) {
	a := peerHub(t, "a", "b", nil)
	b := peerHub(t, "b", "a", func(map[string]any) (any, string) { return map[string]any{}, "" })
	linkHubs(a, b, func() bool { return false })
	first, err := peerSendAs(a, "c-a", "b", "k1", "one")
	if err != nil || first["seq"] != int64(1) {
		t.Fatalf("first send: %v %v", first, err)
	}
	if again, err := peerSendAs(a, "c-a", "b", "k1", "one"); err != nil || again["seq"] != int64(1) {
		t.Fatalf("a retry: %v %v", again, err)
	}
	if _, err := peerSendAs(a, "c-a", "b", "k1", "two"); errCode(err) != "key_conflict" {
		t.Fatalf("another body: %v", err)
	}
	if _, err := b.peerPull(context.Background(), b.peers.links["a"]); err != nil {
		t.Fatal(err)
	}
	if _, err := b.peerPull(context.Background(), b.peers.links["a"]); err != nil { // acks seq 1
		t.Fatal(err)
	}
	if n := outboxOpen(t, a, "b"); n != 0 {
		t.Fatalf("%d open envelopes after the ack", n)
	}
	if again, err := peerSendAs(a, "c-a", "b", "k1", "one"); err != nil || again["seq"] != int64(1) {
		t.Fatalf("a retry after the ack: %v %v", again, err)
	}
	if _, err := peerSendAs(a, "c-a", "b", "k1", "two"); errCode(err) != "key_conflict" {
		t.Fatalf("another body after the ack: %v", err)
	}
	if next, err := peerSendAs(a, "c-a", "b", "k2", "x"); err != nil || next["seq"] != int64(2) {
		t.Fatalf("the next seq after an empty outbox: %v %v", next, err)
	}
	// A seq is never used again, also after its row is deleted.
	if _, err := a.db.Exec(`DELETE FROM peer_outbox WHERE seq = 2`); err != nil {
		t.Fatal(err)
	}
	if next, err := peerSendAs(a, "c-a", "b", "k3", "x"); err != nil || next["seq"] != int64(3) {
		t.Fatalf("the seq after a deleted row: %v %v", next, err)
	}
	for name, tc := range map[string]struct{ client, peer, body, code string }{
		"not a plugin": {"c-agent", "b", "x", "not_plugin"},
		"unknown hub":  {"c-a", "zz", "x", "unknown_hub"},
		"own name":     {"c-a", "a", "x", "unknown_hub"},
		"too large":    {"c-a", "b", strings.Repeat("x", peerBodyMax+1), "too_large"},
	} {
		if _, err := peerSendAs(a, tc.client, tc.peer, "k-"+name, tc.body); errCode(err) != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}
}

// TestPeerPullDelivery: B gives A's envelopes to its plugin in seq order and moves its cursor only
// after an answer. A failed peer call, a failed perform (a lost result) and a plugin that does not
// run stop the pull with the cursor where it was; the next pull gives the envelope again. A
// refused envelope ({"refused": code}) moves the cursor.
func TestPeerPullDelivery(t *testing.T) {
	var mu sync.Mutex
	var got []string
	fail := map[string]string{} // key -> error code of its next delivery
	a := peerHub(t, "a", "b", nil)
	b := peerHub(t, "b", "a", func(args map[string]any) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		key, _ := args["key"].(string)
		if code := fail[key]; code != "" {
			delete(fail, key)
			return nil, code
		}
		got = append(got, fmt.Sprintf("%v/%v/%v", args["from_hub"], key, args["body"]))
		if key == "k3" {
			return map[string]any{"refused": "unknown_recipient", "message": "no such agent"}, ""
		}
		return map[string]any{}, ""
	})
	var down atomic.Bool // read by the nudge goroutines of peerSend
	linkHubs(a, b, down.Load)
	for i := 1; i <= 4; i++ {
		if _, err := peerSendAs(a, "c-a", "b", fmt.Sprintf("k%d", i), fmt.Sprintf("body %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	l := b.peers.links["a"]
	ctx := context.Background()
	cursor := func() int64 {
		n, err := b.peerAfter(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	down.Store(true)
	if _, err := b.peerPull(ctx, l); err == nil || cursor() != 0 {
		t.Fatalf("a failed peer call: %v, cursor %d", err, cursor())
	}
	down.Store(false)
	fail["k2"] = "timeout" // the perform of k2 fails: its result is lost
	if _, err := b.peerPull(ctx, l); err == nil || !strings.Contains(err.Error(), "envelope 2") || cursor() != 1 {
		t.Fatalf("a lost result: %v, cursor %d", err, cursor())
	}
	b.plugins["messaging"].ClientID = "" // the plugin stopped
	if _, err := b.peerPull(ctx, l); err == nil || !strings.Contains(err.Error(), "does not run here") || cursor() != 1 {
		t.Fatalf("a stopped plugin: %v, cursor %d", err, cursor())
	}
	b.plugins["messaging"].ClientID = "c-b"
	if _, err := b.peerPull(ctx, l); err != nil || cursor() != 4 {
		t.Fatalf("the pull after the failures: %v, cursor %d", err, cursor())
	}
	want := []string{"a/k1/body 1", "a/k2/body 2", "a/k3/body 3", "a/k4/body 4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("delivered %q, want %q", got, want)
	}
	if _, err := b.peerPull(ctx, l); err != nil || outboxOpen(t, a, "b") != 0 || len(got) != 4 {
		t.Fatalf("the ack pull: %v, %d open, %d delivered", err, outboxOpen(t, a, "b"), len(got))
	}
}

// TestPeerCursorNeverBack: a reply to an older cursor does not deliver again, and the stored
// cursor never moves back.
func TestPeerCursorNeverBack(t *testing.T) {
	var n atomic.Int32
	b := peerHub(t, "b", "a", func(map[string]any) (any, string) { n.Add(1); return map[string]any{}, "" })
	b.peers.call = func(context.Context, PeerConfig, string, any) ([]byte, error) {
		return []byte(`{"envelopes":[{"seq":2,"plugin":"messaging","key":"k2","body":"x"},{"seq":5,"plugin":"messaging","key":"k5","body":"y"}],"more":false}`), nil
	}
	ctx := context.Background()
	if _, err := b.db.Exec(`INSERT INTO peer_inbox (peer, after) VALUES ('a', 3)`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.peerPull(ctx, b.peers.links["a"]); err != nil {
		t.Fatal(err)
	}
	if after, _ := b.peerAfter(ctx, "a"); after != 5 || n.Load() != 1 {
		t.Fatalf("cursor %d after %d deliveries, want 5 after 1", after, n.Load())
	}
	// A late reply to an older pull: the old pull gets seq 6, the new pull gets 6 and 7 and ends
	// first. The old pull's end does not lower the cursor to 6.
	var calls atomic.Int32
	old, release := make(chan struct{}), make(chan struct{})
	b.peers.call = func(context.Context, PeerConfig, string, any) ([]byte, error) {
		if calls.Add(1) == 1 { // the old pull
			close(old)
			<-release
			return []byte(`{"envelopes":[{"seq":6,"plugin":"messaging","key":"k6","body":"x"}],"more":false}`), nil
		}
		return []byte(`{"envelopes":[{"seq":6,"plugin":"messaging","key":"k6","body":"x"},{"seq":7,"plugin":"messaging","key":"k7","body":"y"}],"more":false}`), nil
	}
	done := make(chan error)
	go func() { _, err := b.peerPull(ctx, b.peers.links["a"]); done <- err }()
	<-old
	if _, err := b.peerPull(ctx, b.peers.links["a"]); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if after, _ := b.peerAfter(ctx, "a"); after != 7 {
		t.Fatalf("the cursor moved back to %d", after)
	}
}

// TestPeerPullLimits: a pull gives at most 100 envelopes, or 1 MiB of bodies (at least one), and
// says more.
func TestPeerPullLimits(t *testing.T) {
	a := peerHub(t, "a", "b", nil)
	pull := func(after int) ([]any, bool) {
		res, err := a.peerPullServe(context.Background(), map[string]any{"hub": "b", "after": float64(after)})
		if err != nil {
			t.Fatal(err)
		}
		m := res.(map[string]any)
		return m["envelopes"].([]any), m["more"].(bool)
	}
	for i := 0; i < 101; i++ {
		if _, err := peerSendAs(a, "c-a", "b", fmt.Sprint(i), "x"); err != nil {
			t.Fatal(err)
		}
	}
	if envs, more := pull(0); len(envs) != 100 || !more {
		t.Fatalf("101 envelopes: %d, more %v", len(envs), more)
	}
	if envs, more := pull(101); len(envs) != 0 || more {
		t.Fatalf("after 101: %d, more %v", len(envs), more)
	}
	// NUL encodes to 6 bytes: the reply is measured encoded. 20 envelopes of 10 KiB NUL (60 KiB
	// encoded each) take more than one reply; each reply fits the hub's reply frame.
	big := strings.Repeat("\x00", 10<<10)
	for i := 0; i < 20; i++ {
		if _, err := peerSendAs(a, "c-a", "b", fmt.Sprint("big", i), big); err != nil {
			t.Fatal(err)
		}
	}
	after, got := 101, 0
	for {
		res, err := a.peerPullServe(context.Background(), map[string]any{"hub": "b", "after": float64(after)})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := guardReply("hub.peer.pull", map[string]any{"call_id": strings.Repeat("c", 64), "result": res}, replyLimit(a.rex)); !ok {
			t.Fatalf("a pull reply after %d is over the reply frame limit", after)
		}
		m := res.(map[string]any)
		envs := m["envelopes"].([]any)
		if len(envs) == 0 {
			break
		}
		got += len(envs)
		after = int(envs[len(envs)-1].(map[string]any)["seq"].(int64))
		if got < 20 && !m["more"].(bool) {
			t.Fatalf("more is false after %d of 20 envelopes", got)
		}
	}
	if got != 20 {
		t.Fatalf("pulled %d big envelopes, want 20", got)
	}
	if _, err := a.peerPullServe(context.Background(), map[string]any{"hub": "zz", "after": float64(0)}); errCode(err) != "unknown_hub" {
		t.Fatalf("an unknown puller: %v", err)
	}
}

// TestPeerPullNotInstalled: an envelope for a plugin that is not installed on the receiver (no
// record, or a kept record without a config entry) is logged and skipped: the cursor moves, and
// nothing is delivered.
func TestPeerPullNotInstalled(t *testing.T) {
	var n atomic.Int32
	a := peerHub(t, "a", "b", nil)
	b := peerHub(t, "b", "a", func(map[string]any) (any, string) { n.Add(1); return map[string]any{}, "" })
	linkHubs(a, b, func() bool { return false })
	for _, k := range []string{"k1", "k2"} {
		if _, err := peerSendAs(a, "c-a", "b", k, "x"); err != nil {
			t.Fatal(err)
		}
	}
	b.mu.Lock()
	b.plugins["messaging"].entry = nil // removed from the config; the record is kept
	b.mu.Unlock()
	if _, err := b.peerPull(context.Background(), b.peers.links["a"]); err != nil {
		t.Fatal(err)
	}
	if after, _ := b.peerAfter(context.Background(), "a"); after != 2 || n.Load() != 0 {
		t.Fatalf("cursor %d after %d deliveries, want 2 after 0", after, n.Load())
	}
	if _, err := peerSendAs(a, "c-a", "b", "k3", "x"); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	delete(b.plugins, "messaging") // no record
	b.mu.Unlock()
	if _, err := b.peerPull(context.Background(), b.peers.links["a"]); err != nil {
		t.Fatal(err)
	}
	if after, _ := b.peerAfter(context.Background(), "a"); after != 3 || n.Load() != 0 {
		t.Fatalf("cursor %d after %d deliveries, want 3 after 0", after, n.Load())
	}
}

// TestPeerSendSizes: the body limit counts encoded bytes, and each accepted envelope fits alone in
// one pull reply, also on a Rex connection with the 64 KiB reference limit.
func TestPeerSendSizes(t *testing.T) {
	a := peerHub(t, "a", "b", nil)
	if _, err := peerSendAs(a, "c-a", "b", "nul", strings.Repeat("\x00", 11<<10)); errCode(err) != "too_large" { // 66 KiB encoded
		t.Fatalf("a body over 64 KiB encoded: %v", err)
	}
	a.rex = payloadRex(t, func(string, json.RawMessage) (any, string) { return map[string]any{}, "" }) // no limits: 64 KiB
	if _, err := peerSendAs(a, "c-a", "b", "near", strings.Repeat("x", 63<<10)); errCode(err) != "too_large" {
		t.Fatalf("an envelope over one pull reply: %v", err)
	}
	if _, err := peerSendAs(a, "c-a", "b", "fits", strings.Repeat("x", 60<<10)); err != nil {
		t.Fatal(err)
	}
	res, err := a.peerPullServe(context.Background(), map[string]any{"hub": "b", "after": float64(0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := guardReply("hub.peer.pull", map[string]any{"call_id": strings.Repeat("c", 64), "result": res}, replyLimit(a.rex)); !ok {
		t.Fatal("the reply with the largest accepted envelope is over the reply frame limit")
	}
}

// TestPeerSendConcurrent: 32 sends of one key at the same time all get the first seq; with another
// body, each send that does not insert gets key_conflict.
func TestPeerSendConcurrent(t *testing.T) {
	a := peerHub(t, "a", "b", nil)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	seqs := make(chan any, 64)
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, body := "same", "one"
			if i%2 == 1 {
				key, body = "mixed", fmt.Sprint("body ", i%3)
			}
			res, err := peerSendAs(a, "c-a", "b", key, body)
			if key == "same" {
				if err != nil {
					errs <- err
					return
				}
				seqs <- res["seq"]
			} else if err != nil && errCode(err) != "key_conflict" {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(seqs)
	for err := range errs {
		t.Errorf("a concurrent send: %v", err)
	}
	var first any
	for seq := range seqs {
		if first == nil {
			first = seq
		}
		if seq != first {
			t.Fatalf("seqs %v and %v for one key and body", first, seq)
		}
	}
	var rows int
	a.db.QueryRow(`SELECT count(*) FROM peer_outbox`).Scan(&rows)
	if rows != 2 {
		t.Fatalf("%d outbox rows, want 2", rows)
	}
}

// TestPeerResolve: "<x>@<this hub>" resolves locally; "<x>@<peer>" is remote with no lookup; any
// other hub is unknown_hub.
func TestPeerResolve(t *testing.T) {
	a := peerHub(t, "a", "b", nil)
	a.agents, a.idents, a.aliases = map[string]*Agent{}, map[string]*Agent{"claude:x1": {ID: "claude:x1", Kind: "claude", IdState: "current"}}, map[string]string{}
	res, err := a.resolveAgent(map[string]any{"to": "claude:x1@a"})
	if m, _ := res.(map[string]any); err != nil || m["id"] != "claude:x1" {
		t.Fatalf("own hub: %v %v", res, err)
	}
	res, err = a.resolveAgent(map[string]any{"to": "claude:zz@b"})
	if m, _ := res.(map[string]any); err != nil || m["id"] != "claude:zz@b" || m["via"] != "remote" || m["hub"] != "b" {
		t.Fatalf("a peer: %v %v", res, err)
	}
	if _, err := a.resolveAgent(map[string]any{"to": "claude:zz@c"}); errCode(err) != "unknown_hub" {
		t.Fatalf("another hub: %v", err)
	}
	if _, err := a.resolveAgent(map[string]any{"to": "@b"}); err == nil {
		t.Fatal("no agent before @")
	}
}

func TestCheckPeers(t *testing.T) {
	p := func(name, server string, from bool) PeerConfig {
		return PeerConfig{Name: name, Server: server, PluginsFrom: from}
	}
	for name, tc := range map[string]struct {
		c    Config
		want string
	}{
		"none":         {Config{}, ""},
		"good":         {Config{HubName: "mac", Peers: []PeerConfig{p("linux", "linux", true), p("box-2", "unix:///s", false)}}, ""},
		"no hub name":  {Config{Peers: []PeerConfig{p("linux", "linux", false)}}, "peers need hub_name"},
		"bad hub name": {Config{HubName: "Mac"}, "hub_name"},
		"bad peer":     {Config{HubName: "mac", Peers: []PeerConfig{p("a_b", "x", false)}}, "must match"},
		"own name":     {Config{HubName: "mac", Peers: []PeerConfig{p("mac", "x", false)}}, "this hub's name"},
		"twice":        {Config{HubName: "mac", Peers: []PeerConfig{p("l", "x", false), p("l", "y", false)}}, "twice"},
		"no server":    {Config{HubName: "mac", Peers: []PeerConfig{p("l", "", false)}}, "no server"},
		"two sources":  {Config{HubName: "mac", Peers: []PeerConfig{p("l", "x", true), p("m", "y", true)}}, "at most one"},
	} {
		err := checkPeers(tc.c)
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
