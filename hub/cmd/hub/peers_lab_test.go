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

// peerLab is one lab hub of the two-hub test, with more lab server options (serverArgs).
func peerLab(t *testing.T, serverArgs ...string) *startLab {
	s := newServerLab(t, serverArgs, "peerfix")
	d := filepath.Join(s.src, "peerfix")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "lazyrex.json"), []byte(`{"name":"peerfix","main":"main.lua"}`), 0o600)
	os.WriteFile(filepath.Join(d, "main.lua"), []byte(peerFixture), 0o600)
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
// b's lab server also listens on HTTP at 127.0.0.1, and a reaches b at that address: a's pulls and
// nudges arrive at b's hub over TCP, from a caller with no process ID, as from a remote hub.
func TestPeerHubsLab(t *testing.T) {
	if os.Getenv("HARNESS_LAB") == "" {
		t.Skip("lab test: set HARNESS_LAB=1")
	}
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
	if out := a.plugin("fix.send", map[string]any{"hub": "b", "key": "k1", "body": "other"}); !strings.Contains(fmt.Sprint(out["error"]), "code=key_conflict") {
		t.Fatalf("another body for k1: %v", out)
	}
	for _, s := range []*startLab{a, b} {
		s.wait("an empty outbox and an up link", func() bool {
			st := s.must("hub.peer.status", map[string]any{})
			p := st["peers"].([]any)[0].(map[string]any)
			return p["state"] == "up" && p["outbox"] == float64(0)
		})
		t.Logf("hub.peer.status: %v", s.must("hub.peer.status", map[string]any{}))
	}
	if g := b.got(); len(g) != 2 {
		t.Fatalf("b got %d envelopes after the acks, want 2 (no redelivery): %v", len(g), g)
	}
	// An agent reads the peer state through the agent face (rex -C lazyrex).
	st, err := a.agentDo(a.opsAgent("x"), "hub.peer.status", map[string]any{})
	if err != nil || st["hub_name"] != "a" || len(st["peers"].([]any)) != 1 {
		t.Fatalf("hub.peer.status through the agent face: %v, %v", st, err)
	}
}
