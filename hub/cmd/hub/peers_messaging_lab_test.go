package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// messagingLab is one lab hub of TestPeerMessagingLab: a copy of the Messaging plugin in dir (the
// hub refuses a local source with a symlink) and the SDK of this checkout.
func messagingLab(t *testing.T, dir, root string, serverArgs ...string) *startLab {
	s := newServerLab(t, serverArgs, "messaging")
	if err := os.MkdirAll(s.src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(s.src, "messaging"), os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	s.args = append(s.args, "-sdk", filepath.Join(root, "sdk", "lua"))
	return s
}

// addAgent adds an agent to the lab hub's roster (config "agents"), for opsAgent.
func (s *startLab) addAgent(id string) {
	s.t.Helper()
	var c map[string]any
	b, _ := os.ReadFile(s.cfg)
	json.Unmarshal(b, &c)
	agents, _ := c["agents"].([]any)
	c["agents"] = append(agents, map[string]any{"id": id, "name": id})
	b, _ = json.MarshalIndent(c, "", " ")
	if err := os.WriteFile(s.cfg, b, 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// agentID is the agent ID that hub.agents.resolve gives for name on s's hub.
func (s *startLab) agentID(name string) string {
	s.t.Helper()
	r := s.must("hub.agents.resolve", map[string]any{"to": name})
	id, _ := r["id"].(string)
	if id == "" {
		s.t.Fatalf("hub.agents.resolve %s: %v", name, r)
	}
	return id
}

// messagingOutbox counts the rows of Messaging's own peer_outbox table on s's hub.
func (s *startLab) messagingOutbox() string {
	s.t.Helper()
	r := s.must("hub.test.sql", map[string]any{"plugin": "messaging", "sql": "SELECT count(*) FROM peer_outbox", "params": []any{}})
	rows, _ := r["rows"].([]any)
	if len(rows) != 1 {
		s.t.Fatalf("peer_outbox count: %v", r)
	}
	v := rows[0].([]any)[0]
	if m, ok := v.(map[string]any); ok {
		v = m["int"]
	}
	return fmt.Sprint(v)
}

// agentRead is messaging.read as agent; it waits until the agent has a message and returns the
// first one.
func (s *startLab) agentRead(agent, what string) map[string]any {
	s.t.Helper()
	var msg map[string]any
	s.wait(what, func() bool {
		r, err := s.agentDo(agent, "messaging.read", map[string]any{})
		if err != nil {
			return false
		}
		list, _ := r["messages"].([]any)
		if len(list) == 0 {
			return false
		}
		msg, _ = list[0].(map[string]any)
		return true
	})
	return msg
}

// TestPeerMessagingLab: the Messaging plugin on two lab hubs a and b, each the other's peer (b over
// TCP, as in TestPeerHubsLab). Agent x on a sends to y@b; y reads it from x@a and replies; x reads
// the reply from y@b with reply_to the first message. Then both hubs' outboxes and both Messaging
// peer_outbox tables are empty. MESSAGING_DIR names the Messaging plugin checkout (skipped without
// it): the Messaging owner and its reviewer run it against their branch.
func TestPeerMessagingLab(t *testing.T) {
	dir := os.Getenv("MESSAGING_DIR")
	if dir == "" {
		t.Skip("set MESSAGING_DIR to a Messaging plugin checkout")
	}
	_, root := harnessBinary(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp := "http://" + ln.Addr().String()
	ln.Close()
	a, b := messagingLab(t, dir, root), messagingLab(t, dir, root, "--extra-listen", tcp, "--allow-anonymous")
	a.setPeer("a", "b", tcp)
	b.setPeer("b", "a", a.l.server)
	a.addAgent("x")
	b.addAgent("y")
	a.start()
	b.start()
	a.registered("messaging")
	b.registered("messaging")
	x, y := a.opsAgent("x"), b.opsAgent("y")
	xID, yID := a.agentID("x"), b.agentID("y")

	sent, err := a.agentDo(x, "messaging.send", map[string]any{"to": yID + "@b", "body": "hello from a", "key": "fed-1"})
	if err != nil {
		t.Fatalf("x sends to %s@b: %v", yID, err)
	}
	first, _ := sent["id"].(string)
	got := b.agentRead(y, "y gets the message from a")
	if got["from"] != xID+"@a" || got["body"] != "hello from a" {
		t.Fatalf("y got %v, want from %s@a and the body", got, xID)
	}
	t.Logf("y on b got: %v", got)
	if _, err := b.agentDo(y, "messaging.send", map[string]any{"to": xID + "@a", "body": "hello back", "key": "fed-2", "reply_to": got["id"]}); err != nil {
		t.Fatalf("y replies to %s@a: %v", xID, err)
	}
	reply := a.agentRead(x, "x gets the reply from b")
	if reply["from"] != yID+"@b" || reply["body"] != "hello back" || reply["reply_to"] != first {
		t.Fatalf("x got %v, want from %s@b, the body and reply_to %s", reply, yID, first)
	}
	t.Logf("x on a got: %v", reply)
	for _, s := range []*startLab{a, b} {
		s.wait("an empty outbox and an up link", func() bool {
			st := s.must("hub.peer.status", map[string]any{})
			p := st["peers"].([]any)[0].(map[string]any)
			return p["state"] == "up" && p["outbox"] == float64(0)
		})
		if n := s.messagingOutbox(); n != "0" {
			t.Fatalf("Messaging peer_outbox has %s rows", n)
		}
		t.Logf("hub.peer.status: %v", s.must("hub.peer.status", map[string]any{}))
	}
}
