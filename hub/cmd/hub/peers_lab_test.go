package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// peerFixture is a plugin for the two-hub lab: fix.send sends with hub.peer.send and replies with
// {result} or {error, code}; peer.receive records each envelope and refuses the key "refuse";
// fix.got returns the records.
const peerFixture = `local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local got = {}
rex.call('client.keymap', {actions = {
  {name = 'fix.send', kind = 'client', title = 'Send', args = {type = 'object'}},
  {name = 'fix.got', kind = 'client', title = 'Got', args = {type = 'object'}},
  {name = 'peer.receive', kind = 'client', title = 'Receive', args = {type = 'object'}}}})
rex.on('client_perform_requested', function(ctx, ev)
  if ev.action == 'fix.send' then
    local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.peer.send', args = ev.args, timeout_ms = 10000})
    local out = {result = res and res.result}
    if err then out.error = type(err) == 'table' and tostring(err.message) or tostring(err); out.code = type(err) == 'table' and err.code or nil end
    rex.call('client.reply', {call_id = ev.call_id, result = out})
  elseif ev.action == 'fix.got' then
    rex.call('client.reply', {call_id = ev.call_id, result = {got = got}})
  elseif ev.action == 'peer.receive' then
    got[#got + 1] = {from_hub = ev.args.from_hub, key = ev.args.key, body = ev.args.body}
    if ev.args.key == 'refuse' then
      rex.call('client.reply', {call_id = ev.call_id, result = {refused = 'unknown_recipient', message = 'lab refusal'}})
    else
      rex.call('client.reply', {call_id = ev.call_id, result = {}})
    end
  else
    rex.call('client.reply', {call_id = ev.call_id, error = {code = 'not_supported', message = 'no such action'}})
  end
end)
local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.register', args = {plugin = 'peerfix', instance_id = instance_id, exports = {}}, timeout_ms = 10000})
if err then print('register: ' .. tostring(err)) end
rex.call('client.set_label', {label = 'lazyrex-plugin:peerfix'})
`

// peerQuiet is a plugin without a peer.receive listener: its keymap has one other action.
const peerQuiet = `local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
rex.call('client.keymap', {actions = {{name = 'quiet.ping', kind = 'client', title = 'Ping', args = {type = 'object'}}}})
rex.on('client_perform_requested', function(ctx, ev)
  rex.call('client.reply', {call_id = ev.call_id, result = {}})
end)
local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.register', args = {plugin = 'peerquiet', instance_id = instance_id, exports = {}}, timeout_ms = 10000})
if err then print('register: ' .. tostring(err)) end
rex.call('client.set_label', {label = 'lazyrex-plugin:peerquiet'})
`

// peerLab is one lab hub of the two-hub test, with more lab server options (serverArgs). It has the
// plugins peerfix and peerquiet.
func peerLab(t *testing.T, serverArgs ...string) *startLab {
	s := newServerLab(t, serverArgs, "peerfix", "peerquiet")
	for name, main := range map[string]string{"peerfix": peerFixture, "peerquiet": peerQuiet} {
		d := filepath.Join(s.src, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d, "lazyrex.json"), []byte(`{"name":"`+name+`","main":"main.lua"}`), 0o600)
		os.WriteFile(filepath.Join(d, "main.lua"), []byte(main), 0o600)
	}
	return s
}

// setPeer writes hub_name and one peer (the other lab's Rex server) into the lab config.
func (s *startLab) setPeer(name, peer, server string) {
	s.t.Helper()
	var c map[string]any
	b, _ := os.ReadFile(s.cfg)
	json.Unmarshal(b, &c)
	c["hub_name"] = name
	c["peers"] = []any{map[string]any{"name": peer, "server": server}}
	b, _ = json.MarshalIndent(c, "", " ")
	if err := os.WriteFile(s.cfg, b, 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// plugin performs action on the peerfix plugin client.
func (s *startLab) plugin(action string, args any) map[string]any {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := s.c.perform(ctx, s.status("peerfix").ClientID, action, args)
	if err != nil {
		s.t.Fatalf("%s: %v", action, err)
	}
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out
}

func (s *startLab) got() []any {
	g, _ := s.plugin("fix.got", map[string]any{})["got"].([]any)
	return g
}

// TestPeerHubsLab: two lab hubs, each the other's peer at its lab Rex server. A plugin on a sends
// to b and b's plugin gets it with from_hub a; b's plugin replies and a gets it. A JSON-looking
// body stays a string. A refused envelope does not stop the next one. Then both outboxes are
// empty and both links are up, and an agent on a reads hub.peer.status through the agent face.
//
// Targets (lead-fedxp): an envelope for a plugin that b does not have, and one for b's peerquiet,
// which offers no peer.receive (Rex refuses the perform: not_supported), are dropped and counted in
// b's hub.peer.status; neither stops the next envelope. A target that is not a plugin name is
// refused at the send, and a key sent again for another target is operation_conflict. Peer agents
// (lead-g4): b lists a's agent in hub.peer.status peers[].agents.
//
// b's lab server also listens on HTTP at 127.0.0.1, and a reaches b at that address: a's pulls and
// nudges arrive at b's hub over TCP, from a caller with no process ID, as from a remote hub.
func TestPeerHubsLab(t *testing.T) {
	needLab(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp := "http://" + ln.Addr().String()
	ln.Close()
	a, b := peerLab(t), peerLab(t, "--extra-listen", tcp, "--allow-anonymous")
	a.setPeer("a", "b", tcp)
	b.setPeer("b", "a", a.l.server)
	a.start()
	b.start()
	a.registered("peerfix")
	b.registered("peerfix")
	b.registered("peerquiet")

	send := func(s *startLab, peer, key, body string) {
		t.Helper()
		out := s.plugin("fix.send", map[string]any{"hub": peer, "key": key, "body": body})
		if out["error"] != nil {
			t.Fatalf("send %s to %s: %v (%v)", key, peer, out["error"], out["code"])
		}
	}
	wait := func(s *startLab, n int) []any {
		t.Helper()
		var g []any
		s.wait(fmt.Sprintf("%d envelopes", n), func() bool { g = s.got(); return len(g) >= n })
		return g
	}
	t0 := time.Now()
	send(a, "b", "refuse", "no one")
	send(a, "b", "k1", `{"v":1,"text":"hello"}`)
	g := wait(b, 2)
	t.Logf("a -> b: 2 envelopes in %s", time.Since(t0).Round(time.Millisecond))
	if e := g[1].(map[string]any); e["from_hub"] != "a" || e["key"] != "k1" || e["body"] != `{"v":1,"text":"hello"}` {
		t.Fatalf("b got %v", g)
	}
	send(b, "a", "r1", "a reply")
	g = wait(a, 1)
	if e := g[0].(map[string]any); e["from_hub"] != "b" || e["key"] != "r1" || e["body"] != "a reply" {
		t.Fatalf("a got %v", g)
	}
	if out := a.plugin("fix.send", map[string]any{"hub": "b", "key": "k1", "body": "other"}); !strings.Contains(fmt.Sprint(out["error"]), "code=operation_conflict") ||
		!strings.Contains(fmt.Sprint(out["error"]), `operation_conflict: key "k1" was sent to b with another body`) {
		t.Fatalf("another body for k1: %v", out)
	}
	for _, x := range [][2]string{{"x1", "ghost"}, {"x2", "peerquiet"}, {"x3", "peerfix"}} {
		if out := a.plugin("fix.send", map[string]any{"hub": "b", "key": x[0], "body": "t", "plugin": x[1]}); out["error"] != nil {
			t.Fatalf("send %s to %s: %v (%v)", x[0], x[1], out["error"], out["code"])
		}
	}
	if g := wait(b, 3); g[2].(map[string]any)["key"] != "x3" {
		t.Fatalf("b got %v, want x3 third", g)
	}
	b.wait("two drops", func() bool {
		d := b.must("hub.peer.status", map[string]any{})["peers"].([]any)[0].(map[string]any)["dropped"].(map[string]any)
		last, _ := d["last"].(map[string]any)
		return d["count"] == float64(2) && last["plugin"] == "peerquiet" && last["from_plugin"] == "peerfix" && last["key"] == "x2" && last["reason"] == "no_listener"
	})
	if out := a.plugin("fix.send", map[string]any{"hub": "b", "key": "x4", "body": "t", "plugin": "Bad"}); !strings.Contains(fmt.Sprint(out["error"]), "invalid: plugin must be a plugin name") {
		t.Fatalf("a bad target: %v", out)
	}
	if out := a.plugin("fix.send", map[string]any{"hub": "b", "key": "k1", "body": `{"v":1,"text":"hello"}`, "plugin": "peerquiet"}); !strings.Contains(fmt.Sprint(out["error"]), "code=operation_conflict") ||
		!strings.Contains(fmt.Sprint(out["error"]), `operation_conflict: key "k1" was sent to b for another plugin`) {
		t.Fatalf("k1 for another target: %v", out)
	}
	for _, s := range []*startLab{a, b} {
		s.wait("an empty outbox and an up link", func() bool {
			st := s.must("hub.peer.status", map[string]any{})
			p := st["peers"].([]any)[0].(map[string]any)
			return p["state"] == "up" && p["outbox"] == float64(0)
		})
		t.Logf("hub.peer.status: %v", s.must("hub.peer.status", map[string]any{}))
	}
	if g := b.got(); len(g) != 3 {
		t.Fatalf("b got %d envelopes after the acks, want 3 (no redelivery): %v", len(g), g)
	}
	// An agent reads the peer state through the agent face (rex -C lazyrex).
	st, err := a.agentDo(a.opsAgent("x"), "hub.peer.status", map[string]any{})
	if err != nil || st["hub_name"] != "a" || len(st["peers"].([]any)) != 1 {
		t.Fatalf("hub.peer.status through the agent face: %v, %v", st, err)
	}
	// Peer agents (lead-g4): b sees a's agent claude:x in peers[].agents within one read (30 s).
	var seen map[string]any
	b.waitLong("b lists a's agent claude:x", 45*time.Second, func() bool {
		ag, _ := b.must("hub.peer.status", map[string]any{})["peers"].([]any)[0].(map[string]any)["agents"].(map[string]any)
		list, _ := ag["list"].([]any)
		for _, e := range list {
			if e := e.(map[string]any); e["id"] == "claude:x" {
				seen = e
				return ag["at"] != ""
			}
		}
		return false
	})
	if seen["kind"] != "claude" || seen["state"] != "current" || seen["joined"] != true || seen["name"] == "" || len(seen) != 5 {
		t.Fatalf("a's agent as b sees it: %v", seen)
	}
}

// TestPeerOutboxResetLab (lead-fedreset): a's hub.db is replaced while b holds a cursor for a's old
// outbox. The new a's first envelope has seq 1, at or under b's cursor; b still gets it, because
// its first pull names the old outbox, so the new a acks nothing and serves from zero. Before the
// outbox identity, that pull acked the envelope and served nothing: a silent loss.
func TestPeerOutboxResetLab(t *testing.T) {
	needLab(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp := "http://" + ln.Addr().String()
	ln.Close()
	a, b := peerLab(t), peerLab(t, "--extra-listen", tcp, "--allow-anonymous")
	a.setPeer("a", "b", tcp)
	b.setPeer("b", "a", a.l.server)
	a.start()
	b.start()
	a.registered("peerfix")
	b.registered("peerfix")
	send := func(key, body string) {
		t.Helper()
		if out := a.plugin("fix.send", map[string]any{"hub": "b", "key": key, "body": body}); out["error"] != nil {
			t.Fatalf("send %s: %v (%v)", key, out["error"], out["code"])
		}
	}
	row := func() map[string]any {
		return b.must("hub.peer.status", map[string]any{})["peers"].([]any)[0].(map[string]any)
	}
	send("k1", "before")
	b.wait("b gets k1", func() bool { return len(b.got()) == 1 })
	b.wait("b's cursor 1 with a's outbox", func() bool { r := row(); return r["after"] == float64(1) && r["outbox_id"] != "" })
	old := row()["outbox_id"]

	a.stop()
	var cfg struct {
		State string `json:"state"`
	}
	raw, _ := os.ReadFile(a.cfg)
	json.Unmarshal(raw, &cfg)
	for _, f := range []string{"hub.db", "hub.db-wal", "hub.db-shm"} {
		if err := os.Rename(filepath.Join(cfg.State, f), filepath.Join(cfg.State, "old-"+f)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	a.start()
	a.registered("peerfix")
	send("k2", "after") // seq 1 of the new outbox
	// Rex keeps the old hub's lost client with the label lazyrex-hub for about 61 s
	// (docs/rex-protocol.md "Lost clients and reconnect tokens"). b's pull names a's hub by the ID of
	// its one connected client (peerCall), so it reaches the new hub in seconds; the bound fails for
	// the 61 s.
	t0 := time.Now()
	b.waitLong("b gets k2 from the new outbox within 20 s", 20*time.Second, func() bool { return len(b.got()) == 2 })
	t.Logf("k2 reached b in %s", time.Since(t0).Round(time.Millisecond))
	if e := b.got()[1].(map[string]any); e["from_hub"] != "a" || e["key"] != "k2" || e["body"] != "after" {
		t.Fatalf("b got %v", b.got())
	}
	b.wait("b's cursor 1 with the new outbox", func() bool {
		r := row()
		return r["after"] == float64(1) && r["outbox_id"] != old && r["outbox_id"] != ""
	})
	a.wait("a's outbox to b is acked", func() bool {
		r := a.must("hub.peer.status", map[string]any{})["peers"].([]any)[0].(map[string]any)
		return r["outbox"] == float64(0)
	})
	t.Logf("b's row: %v (old outbox_id %v)", row(), old)
}

// TestPeerCallTCPLab (deploy-2bbca-mesh1): peerCall reaches a peer over TCP as an anonymous caller,
// as on the tailnet. `client ls --json` works for that caller, the call names the peer's hub by its
// client ID, and after the peer's hub restarts (Rex keeps the old hub's lost client, so the label is
// ambiguous) the next call reaches the new hub at once.
func TestPeerCallTCPLab(t *testing.T) {
	needLab(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp := "http://" + ln.Addr().String()
	ln.Close()
	b := peerLab(t, "--extra-listen", tcp, "--allow-anonymous")
	b.setPeer("b", "a", "unix:///nowhere")
	b.start()
	cfg := Config{HubName: "a", Peers: []PeerConfig{{Name: "b", Server: tcp}}}
	h := &Hub{cfg: cfg, rexBinary: b.o.rexBin, peers: newPeerSet(cfg)}
	call := func(what string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := h.peerCall(ctx, cfg.Peers[0], "hub.peer.agents", map[string]any{"hub": "a"}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if id := h.peerLink("b").hubID; id != b.hub {
			t.Fatalf("%s: the link keeps %q, the hub is %q", what, id, b.hub)
		}
	}
	call("the first call")
	old := b.hub
	b.stop()
	b.start()
	if b.hub == old {
		t.Fatalf("the restarted hub has the old client ID %s", old)
	}
	t0 := time.Now()
	call("the call after the restart")
	t.Logf("the call after the restart took %s", time.Since(t0).Round(time.Millisecond))
}
