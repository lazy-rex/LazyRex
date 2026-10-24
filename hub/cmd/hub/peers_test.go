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
	if receive == nil {
		return peerHubTo(t, name, peer, nil)
	}
	return peerHubTo(t, name, peer, func(_ string, args map[string]any) (any, string) { return receive(args) })
}

// peerHubTo is peerHub with a receive that also gets the client of the perform.
func peerHubTo(t *testing.T, name, peer string, receive func(client string, args map[string]any) (any, string)) *Hub {
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
			ClientID string         `json:"client_id"`
			Args     map[string]any `json:"args"`
		}
		json.Unmarshal(payload, &p)
		if method != "client.perform" || receive == nil {
			return map[string]any{}, ""
		}
		res, code := receive(p.ClientID, p.Args)
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
	return peerSendArgs(h, client, map[string]any{"hub": peer, "key": key, "body": body})
}

func peerSendArgs(h *Hub, client string, args map[string]any) (map[string]any, error) {
	raw, _ := json.Marshal(args)
	p := perform{Action: "hub.peer.send", Args: raw}
	p.Requester.ClientID = client
	res, err := h.dispatch(context.Background(), p)
	m, _ := res.(map[string]any)
	return m, err
}

// testOutboxID is the outbox identity of a fake peer's pull replies.
const testOutboxID = "0123456789abcdef0123456789abcdef"

// ownOutbox is h's outbox identity, which a pull names to ack.
func ownOutbox(t *testing.T, h *Hub) string {
	t.Helper()
	id, err := h.peerOutboxID(context.Background())
	if err != nil || !outboxID.MatchString(id) {
		t.Fatalf("outbox_id %q: %v", id, err)
	}
	return id
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
// deleted the body; another body is operation_conflict, also after the ack. A seq never repeats after
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
	if _, err := peerSendAs(a, "c-a", "b", "k1", "two"); errCode(err) != "operation_conflict" {
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
	if _, err := peerSendAs(a, "c-a", "b", "k1", "two"); errCode(err) != "operation_conflict" {
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
		return []byte(`{"envelopes":[{"seq":2,"plugin":"messaging","key":"k2","body":"x"},{"seq":5,"plugin":"messaging","key":"k5","body":"y"}],"more":false,"outbox_id":"` + testOutboxID + `"}`), nil
	}
	ctx := context.Background()
	if _, err := b.db.Exec(`INSERT INTO peer_inbox (peer, after, outbox_id) VALUES ('a', 3, ?)`, testOutboxID); err != nil {
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
			return []byte(`{"envelopes":[{"seq":6,"plugin":"messaging","key":"k6","body":"x"}],"more":false,"outbox_id":"` + testOutboxID + `"}`), nil
		}
		return []byte(`{"envelopes":[{"seq":6,"plugin":"messaging","key":"k6","body":"x"},{"seq":7,"plugin":"messaging","key":"k7","body":"y"}],"more":false,"outbox_id":"` + testOutboxID + `"}`), nil
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
		res, err := a.peerPullServe(context.Background(), map[string]any{"hub": "b", "after": float64(after), "outbox_id": ownOutbox(t, a)})
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
		res, err := a.peerPullServe(context.Background(), map[string]any{"hub": "b", "after": float64(after), "outbox_id": ownOutbox(t, a)})
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

// TestPeerPullPluginWait (lead-fed-r2): an envelope for a desired-stopped plugin is skipped at once,
// and the next envelope, for another plugin, is delivered. An envelope for a desired-running plugin
// that does not run blocks the peer, shown in hub.peer.status, for at most peerBlockMax; then it is
// skipped.
func TestPeerPullPluginWait(t *testing.T) {
	var mu sync.Mutex
	var got []string
	a := peerHub(t, "a", "b", nil)
	b := peerHub(t, "b", "a", func(args map[string]any) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, args["key"].(string))
		return map[string]any{}, ""
	})
	for _, h := range []*Hub{a, b} {
		h.mu.Lock()
		h.plugins["other"] = &Plugin{Name: "other", ClientID: "o-" + h.cfg.HubName, entry: &configEntry{}}
		h.mu.Unlock()
	}
	linkHubs(a, b, func() bool { return false })
	ctx, l := context.Background(), b.peers.links["a"]
	send := func(client, key string) {
		t.Helper()
		if _, err := peerSendAs(a, client, "b", key, "x"); err != nil {
			t.Fatal(err)
		}
	}
	setMessaging := func(desired, client string) {
		b.mu.Lock()
		b.plugins["messaging"].Desired, b.plugins["messaging"].ClientID = desired, client
		b.mu.Unlock()
	}

	setMessaging("stopped", "")
	send("c-a", "m1")
	send("o-a", "o1")
	if _, err := b.peerPull(ctx, l); err != nil {
		t.Fatal(err)
	}
	if after, _ := b.peerAfter(ctx, "a"); after != 2 || strings.Join(got, ",") != "o1" {
		t.Fatalf("a stopped plugin: cursor %d, delivered %v; want 2 and o1", after, got)
	}

	setMessaging("running", "") // starting or crashing
	send("c-a", "m2")
	send("o-a", "o2")
	if _, err := b.peerPull(ctx, l); err == nil || !strings.Contains(err.Error(), "does not run here") {
		t.Fatalf("a waiting plugin: %v", err)
	}
	st, err := b.peerStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	row := st.(map[string]any)["peers"].([]any)[0].(map[string]any)
	if after, _ := b.peerAfter(ctx, "a"); after != 2 || row["blocked_plugin"] != "messaging" || row["blocked_since"] == nil {
		t.Fatalf("a waiting plugin: cursor %d, status %v", after, row)
	}
	l.mu.Lock()
	l.blockSince = l.blockSince.Add(-peerBlockMax) // the wait ran out
	l.mu.Unlock()
	if _, err := b.peerPull(ctx, l); err != nil {
		t.Fatal(err)
	}
	st, _ = b.peerStatus(ctx)
	row = st.(map[string]any)["peers"].([]any)[0].(map[string]any)
	if after, _ := b.peerAfter(ctx, "a"); after != 4 || strings.Join(got, ",") != "o1,o2" || row["blocked_plugin"] != nil {
		t.Fatalf("after the wait: cursor %d, delivered %v, status %v; want 4, o1,o2 and no block", after, got, row)
	}
}

// TestPeerPullErrorBound (lead-fed-sdk3): an error reply of peer.receive (the listener raised; its
// transaction rolled back) is retried, for at most peerBlockMax, then skipped; a refusal is final at
// once. Both later envelopes are delivered.
func TestPeerPullErrorBound(t *testing.T) {
	var mu sync.Mutex
	var got []string
	a := peerHub(t, "a", "b", nil)
	b := peerHub(t, "b", "a", func(args map[string]any) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		key := args["key"].(string)
		got = append(got, key)
		switch key {
		case "bad":
			return nil, "failed" // the error reply
		case "drop":
			return map[string]any{"refused": "unknown_recipient", "message": "no such agent"}, ""
		}
		return map[string]any{}, ""
	})
	linkHubs(a, b, func() bool { return false })
	for _, k := range []string{"drop", "ok1", "bad", "ok2"} {
		if _, err := peerSendAs(a, "c-a", "b", k, "x"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, l := context.Background(), b.peers.links["a"]
	cursor := func() int64 { n, _ := b.peerAfter(ctx, "a"); return n }
	for range 2 { // the error reply is retried
		if _, err := b.peerPull(ctx, l); err == nil || !strings.Contains(err.Error(), "envelope 3") || cursor() != 2 {
			t.Fatalf("an error reply: %v, cursor %d; want envelope 3 and cursor 2", err, cursor())
		}
	}
	l.mu.Lock()
	l.blockSince = l.blockSince.Add(-peerBlockMax) // the bound ran out
	l.mu.Unlock()
	if _, err := b.peerPull(ctx, l); err != nil || cursor() != 4 {
		t.Fatalf("after the bound: %v, cursor %d; want 4", err, cursor())
	}
	if want := "drop,ok1,bad,bad,bad,ok2"; strings.Join(got, ",") != want {
		t.Fatalf("delivered %v, want %s", got, want)
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

// TestPeerSendSizeMeasure: the body limit is Go's json.Marshal of the string, quotes included, at
// most 65536 bytes (docs/hub.md, "Peer hubs", Size): & is 6 bytes, " and \ are 2, \n, \r and \t are 2,
// another control character is 6, DEL is 1, U+2028 is 6.
func TestPeerSendSizeMeasure(t *testing.T) {
	a := peerHub(t, "a", "b", nil)
	base := strings.Repeat("&", 10921) // 2 quotes + 65526
	for i, c := range []struct {
		body string
		ok   bool
	}{
		{base + "\"\\" + "\n\r", true},            // 65536
		{base + "\"\\" + "\n\r" + "a", false},     // 65537
		{base + "\"\\" + "\x01", false},           // 65538
		{base + "\u2028", true},                   // 65534
		{base + "\u2028" + "\t", true},            // 65536
		{base + "\u2028" + "\x7f\x7f", true},      // 65536: DEL is 1
		{base + "\u2028" + "\x7f\x7f\x7f", false}, // 65537
	} {
		_, err := peerSendAs(a, "c-a", "b", fmt.Sprintf("m%d", i), c.body)
		if c.ok && err != nil || !c.ok && errCode(err) != "too_large" {
			t.Errorf("case %d (ok %v): %v", i, c.ok, err)
		}
	}
}

// TestPeerSendConcurrent: 32 sends of one key at the same time all get the first seq; with another
// body, each send that does not insert gets operation_conflict.
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
			} else if err != nil && errCode(err) != "operation_conflict" {
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
	a.idents = map[string]*Agent{"claude:x1": {ID: "claude:x1", Kind: "claude", IdState: "current"}}
	res, err := a.resolveAgent(context.Background(), map[string]any{"to": "claude:x1@a"})
	if m, _ := res.(map[string]any); err != nil || m["id"] != "claude:x1" {
		t.Fatalf("own hub: %v %v", res, err)
	}
	res, err = a.resolveAgent(context.Background(), map[string]any{"to": "claude:zz@b"})
	if m, _ := res.(map[string]any); err != nil || m["id"] != "claude:zz@b" || m["via"] != "remote" || m["hub"] != "b" {
		t.Fatalf("a peer: %v %v", res, err)
	}
	if _, err := a.resolveAgent(context.Background(), map[string]any{"to": "claude:zz@c"}); errCode(err) != "unknown_hub" {
		t.Fatalf("another hub: %v", err)
	}
	if _, err := a.resolveAgent(context.Background(), map[string]any{"to": "@b"}); err == nil {
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

// TestPeerStatusAgentFace: hub.peer.status is on the agent face (docs/hub.md, "Peer hubs"), and an
// agent call reaches it through agentDispatch.
func TestPeerStatusAgentFace(t *testing.T) {
	n := 0
	for _, a := range agentHubFace() {
		if a["name"] == "hub.peer.status" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the agent face has %d hub.peer.status descriptors, want 1", n)
	}
	h := peerHub(t, "a", "b", nil)
	p := perform{Action: "hub.peer.status"}
	p.Requester.Principal.PID = 7
	res, err := h.agentDispatch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	st := res.(map[string]any)
	if st["hub_name"] != "a" || len(st["peers"].([]any)) != 1 {
		t.Fatalf("hub.peer.status through the agent face: %v", st)
	}
}

// replaceSender is a new hub a on a fresh hub.db (its outbox seqs start again at 1), linked to b in
// place of the old a.
func replaceSender(t *testing.T, b *Hub) *Hub {
	t.Helper()
	a := peerHub(t, "a", "b", nil)
	linkHubs(a, b, func() bool { return false })
	return a
}

// TestPeerOutboxReset (lead-fedreset): b took 3 envelopes from a (cursor 3); then a's hub.db is
// replaced and the new a queues 2 envelopes, seq 1 and 2. b's first pull still names the old outbox
// and cursor 3: the new a acks nothing (both bodies stay), serves from zero, and b resets its cursor
// to 0 with the new identity and delivers both. The next pull acks them. Before the identity, that
// first pull acked seq 1 and 2 and served nothing: a silent loss.
func TestPeerOutboxReset(t *testing.T) {
	var mu sync.Mutex
	var got []string
	a := peerHub(t, "a", "b", nil)
	b := peerHub(t, "b", "a", func(args map[string]any) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, args["key"].(string))
		return map[string]any{}, ""
	})
	linkHubs(a, b, func() bool { return false })
	ctx, l := context.Background(), b.peers.links["a"]
	for _, k := range []string{"o1", "o2", "o3"} {
		if _, err := peerSendAs(a, "c-a", "b", k, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.peerPull(ctx, l); err != nil {
		t.Fatal(err)
	}
	old := ownOutbox(t, a)
	if after, id, _ := b.peerCursor(ctx, "a"); after != 3 || id != old {
		t.Fatalf("cursor %d of outbox %q, want 3 of %q", after, id, old)
	}
	a2 := replaceSender(t, b)
	if ownOutbox(t, a2) == old {
		t.Fatal("a new hub.db has the old outbox identity")
	}
	for _, k := range []string{"n1", "n2"} {
		if _, err := peerSendAs(a2, "c-a", "b", k, "y"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.peerPull(ctx, l); err != nil {
		t.Fatal(err)
	}
	var bodies int
	if err := a2.db.QueryRow(`SELECT count(*) FROM peer_outbox WHERE peer = 'b' AND acked = 0 AND body IS NOT NULL`).Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	if bodies != 2 {
		t.Fatalf("the stale first pull acked the new outbox: %d of 2 bodies left", bodies)
	}
	if after, id, _ := b.peerCursor(ctx, "a"); after != 2 || id != ownOutbox(t, a2) || strings.Join(got, ",") != "o1,o2,o3,n1,n2" {
		t.Fatalf("cursor %d of outbox %q, delivered %v; want 2 of the new outbox and n1, n2", after, id, got)
	}
	st, _ := b.peerStatus(ctx)
	if row := st.(map[string]any)["peers"].([]any)[0].(map[string]any); row["outbox_id"] != ownOutbox(t, a2) || row["after"] != int64(2) {
		t.Fatalf("hub.peer.status row: %v", row)
	}
	if _, err := b.peerPull(ctx, l); err != nil || outboxOpen(t, a2, "b") != 0 || len(got) != 5 {
		t.Fatalf("the ack pull: %v, %d open, %d delivered", err, outboxOpen(t, a2, "b"), len(got))
	}
}

// TestPeerPullAckNeedsOutbox: a pull acks only when it names this hub's outbox; without it, or with
// another identity, it acks nothing and serves from zero.
func TestPeerPullAckNeedsOutbox(t *testing.T) {
	a := peerHub(t, "a", "b", nil)
	for _, k := range []string{"k1", "k2"} {
		if _, err := peerSendAs(a, "c-a", "b", k, "x"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []any{nil, "", testOutboxID} {
		args := map[string]any{"hub": "b", "after": float64(2)}
		if id != nil {
			args["outbox_id"] = id
		}
		res, err := a.peerPullServe(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		m := res.(map[string]any)
		if n := len(m["envelopes"].([]any)); n != 2 || outboxOpen(t, a, "b") != 2 || m["outbox_id"] != ownOutbox(t, a) {
			t.Fatalf("outbox_id %v: %d served, %d open, reply id %v; want 2, 2 and %s", id, n, outboxOpen(t, a, "b"), m["outbox_id"], ownOutbox(t, a))
		}
	}
	if _, err := a.peerPullServe(context.Background(), map[string]any{"hub": "b", "after": float64(2), "outbox_id": ownOutbox(t, a)}); err != nil || outboxOpen(t, a, "b") != 0 {
		t.Fatalf("with its own identity: %v, %d open", err, outboxOpen(t, a, "b"))
	}
}

// TestPeerOutboxResetClearsBlock (lead-fedreset4 item 1): the retry bound of a blocked envelope is
// keyed by seq. After a reset, the new outbox's envelope with the same seq and one transient error
// is retried, not skipped by the old, expired bound.
func TestPeerOutboxResetClearsBlock(t *testing.T) {
	var mu sync.Mutex
	var got []string
	fail := map[string]bool{"old": true, "new": true}
	a := peerHub(t, "a", "b", nil)
	b := peerHub(t, "b", "a", func(args map[string]any) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		key := args["key"].(string)
		got = append(got, key)
		if fail[key] {
			if key == "new" {
				fail[key] = false // one transient error
			}
			return nil, "failed"
		}
		return map[string]any{}, ""
	})
	linkHubs(a, b, func() bool { return false })
	ctx, l := context.Background(), b.peers.links["a"]
	if _, err := peerSendAs(a, "c-a", "b", "old", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.peerPull(ctx, l); err == nil {
		t.Fatal("the old envelope's error reply did not stop the pull")
	}
	l.mu.Lock()
	l.blockSince = l.blockSince.Add(-peerBlockMax) // seq 1's bound ran out
	l.mu.Unlock()
	a2 := replaceSender(t, b)
	if _, err := peerSendAs(a2, "c-a", "b", "new", "y"); err != nil { // seq 1 again
		t.Fatal(err)
	}
	if _, err := b.peerPull(ctx, l); err == nil || !strings.Contains(err.Error(), "envelope 1") {
		t.Fatalf("the new seq 1's first error: %v; want a retry (envelope 1)", err)
	}
	if after, _ := b.peerAfter(ctx, "a"); after != 0 {
		t.Fatalf("cursor %d after the new envelope's error, want 0", after)
	}
	if _, err := b.peerPull(ctx, l); err != nil {
		t.Fatal(err)
	}
	if after, _ := b.peerAfter(ctx, "a"); after != 1 || strings.Join(got, ",") != "old,new,new" {
		t.Fatalf("cursor %d, delivered %v; want 1 and old,new,new", after, got)
	}
}

// TestPeerPullReplyNeedsOutbox (lead-fedreset4 item 2): a reply without a valid outbox identity is
// an error: no delivery, and the cursor and its identity do not change.
func TestPeerPullReplyNeedsOutbox(t *testing.T) {
	var n atomic.Int32
	b := peerHub(t, "b", "a", func(map[string]any) (any, string) { n.Add(1); return map[string]any{}, "" })
	ctx := context.Background()
	if _, err := b.db.Exec(`INSERT INTO peer_inbox (peer, after, outbox_id) VALUES ('a', 3, ?)`, testOutboxID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{``, `,"outbox_id":""`, `,"outbox_id":"ABC"`, `,"outbox_id":"0123456789ABCDEF0123456789ABCDEF"`} {
		b.peers.call = func(context.Context, PeerConfig, string, any) ([]byte, error) {
			return []byte(`{"envelopes":[{"seq":4,"plugin":"messaging","key":"k4","body":"x"}],"more":false` + id + `}`), nil
		}
		if _, err := b.peerPull(ctx, b.peers.links["a"]); err == nil || !strings.Contains(err.Error(), "no valid outbox identity") {
			t.Fatalf("reply %q: %v", id, err)
		}
		if after, got, _ := b.peerCursor(ctx, "a"); after != 3 || got != testOutboxID || n.Load() != 0 {
			t.Fatalf("reply %q: cursor %d of %q, %d deliveries", id, after, got, n.Load())
		}
	}
}

// TestPeerSendTarget (lead-fedxp): a send names a target plugin on the peer ("plugin"), and the
// receiver gives the envelope to that plugin's peer.receive with the sender's name (from_plugin).
// Absent or the sender's own name is the same plugin: the envelope has no "target", and it goes to
// the plugin of the sender's name. The key scope is (peer, sender, key): the same key with another
// target is operation_conflict; two senders may use one key for one target, and both arrive. A
// target that is not a plugin name is refused at the call.
func TestPeerSendTarget(t *testing.T) {
	var mu sync.Mutex
	var got []string
	a := peerHub(t, "a", "b", nil)
	b := peerHubTo(t, "b", "a", func(client string, args map[string]any) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf("%s<-%v/%v:%v", client, args["from_plugin"], args["from_hub"], args["key"]))
		return map[string]any{}, ""
	})
	for _, h := range []*Hub{a, b} {
		h.mu.Lock()
		h.plugins["other"] = &Plugin{Name: "other", ClientID: "o-" + h.cfg.HubName, entry: &configEntry{}}
		h.mu.Unlock()
	}
	linkHubs(a, b, func() bool { return false })
	send := func(client string, args map[string]any) (map[string]any, error) {
		args["hub"], args["body"] = "b", "x"
		return peerSendArgs(a, client, args)
	}
	s1, err := send("c-a", map[string]any{"key": "k1", "plugin": "other"})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := send("c-a", map[string]any{"key": "k1", "plugin": "other"}); err != nil || s["seq"] != s1["seq"] {
		t.Fatalf("a repeat: %v, %v; want seq %v", s, err, s1["seq"])
	}
	for _, other := range []map[string]any{{"key": "k1"}, {"key": "k1", "plugin": "messaging"}} {
		if _, err := send("c-a", other); errCode(err) != "operation_conflict" || !strings.Contains(err.Error(), "for another plugin") {
			t.Fatalf("k1 for %v: %v, want operation_conflict for another plugin", other["plugin"], err)
		}
	}
	s2, err := send("c-a", map[string]any{"key": "k2", "plugin": "messaging"}) // the own name
	if err != nil {
		t.Fatal(err)
	}
	if s, err := send("c-a", map[string]any{"key": "k2"}); err != nil || s["seq"] != s2["seq"] {
		t.Fatalf("k2 without plugin: %v, %v; want seq %v (the own name is the default)", s, err, s2["seq"])
	}
	if _, err := send("o-a", map[string]any{"key": "k1", "plugin": "other"}); err != nil {
		t.Fatalf("k1 from another sender: %v", err)
	}
	for _, bad := range []any{"", "Other", "a/b", 7} {
		if _, err := send("c-a", map[string]any{"key": "k9", "plugin": bad}); errCode(err) != "refused" || !strings.Contains(err.Error(), "invalid: plugin must be a plugin name") {
			t.Fatalf("plugin %v: %v, want invalid: plugin must be a plugin name", bad, err)
		}
	}
	// The wire: "target" only for another plugin.
	res, err := a.peerPullServe(context.Background(), map[string]any{"hub": "b", "after": float64(0)})
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, e := range res.(map[string]any)["envelopes"].([]any) {
		e := e.(map[string]any)
		target, has := e["target"]
		targets = append(targets, fmt.Sprintf("%v>%v(%v)", e["plugin"], target, has))
	}
	if want := "messaging>other(true),messaging><nil>(false),other><nil>(false)"; strings.Join(targets, ",") != want {
		t.Fatalf("envelopes %v, want %s", targets, want)
	}
	if _, err := b.peerPull(context.Background(), b.peers.links["a"]); err != nil {
		t.Fatal(err)
	}
	if want := "o-b<-messaging/a:k1,c-b<-messaging/a:k2,o-b<-other/a:k1"; strings.Join(got, ",") != want {
		t.Fatalf("delivered %v, want %s", got, want)
	}
}

// TestPeerPullDrops (lead-fedxp item 5): an envelope whose target cannot take it is dropped at once,
// the cursor moves on, and hub.peer.status counts it (dropped: count and the last one): a target
// that is not installed, one that is desired stopped, and one that does not offer peer.receive (Rex
// refuses the perform with not_supported). A desired-running target with no client now (starting,
// restarting) is not dropped: it holds the cursor, for at most peerBlockMax (TestPeerPullPluginWait).
func TestPeerPullDrops(t *testing.T) {
	var mu sync.Mutex
	var got []string
	a := peerHub(t, "a", "b", nil)
	b := peerHubTo(t, "b", "a", func(client string, args map[string]any) (any, string) {
		if client == "n-b" {
			return nil, "not_supported"
		}
		mu.Lock()
		defer mu.Unlock()
		got = append(got, args["key"].(string))
		return map[string]any{}, ""
	})
	b.mu.Lock()
	b.plugins["stopped"] = &Plugin{Name: "stopped", ClientID: "", Desired: "stopped", entry: &configEntry{}}
	b.plugins["nolisten"] = &Plugin{Name: "nolisten", ClientID: "n-b", entry: &configEntry{}}
	b.plugins["later"] = &Plugin{Name: "later", ClientID: "", entry: &configEntry{}}
	b.mu.Unlock()
	linkHubs(a, b, func() bool { return false })
	ctx, l := context.Background(), b.peers.links["a"]
	for _, s := range [][2]string{{"k1", "absent"}, {"k2", "stopped"}, {"k3", "nolisten"}, {"k4", "messaging"}} {
		if _, err := peerSendArgs(a, "c-a", map[string]any{"hub": "b", "key": s[0], "body": "x", "plugin": s[1]}); err != nil {
			t.Fatal(err)
		}
	}
	row := func() map[string]any {
		st, err := b.peerStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st.(map[string]any)["peers"].([]any)[0].(map[string]any)
	}
	if d := row()["dropped"].(map[string]any); d["count"] != 0 || d["last"] != (*peerDrop)(nil) {
		t.Fatalf("dropped before a pull: %v", d)
	}
	if _, err := b.peerPull(ctx, l); err != nil {
		t.Fatal(err)
	}
	d := row()["dropped"].(map[string]any)
	last := d["last"].(*peerDrop)
	if after, _ := b.peerAfter(ctx, "a"); after != 4 || strings.Join(got, ",") != "k4" || d["count"] != 3 ||
		last.Plugin != "nolisten" || last.FromPlugin != "messaging" || last.Key != "k3" || last.Reason != "no_listener" || last.At == "" {
		t.Fatalf("cursor %d, delivered %v, dropped %v %+v; want 4, k4, 3 drops, the last k3 no_listener", after, got, d, last)
	}
	if _, err := peerSendArgs(a, "c-a", map[string]any{"hub": "b", "key": "k5", "body": "x", "plugin": "later"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.peerPull(ctx, l); err == nil || !strings.Contains(err.Error(), "does not run here") {
		t.Fatalf("a target that does not run now: %v, want a block", err)
	}
	if r := row(); r["blocked_plugin"] != "later" || r["dropped"].(map[string]any)["count"] != 3 {
		t.Fatalf("a target that does not run now: %v", r)
	}
}
