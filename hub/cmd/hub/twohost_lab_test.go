package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// longWait is startLab.wait with a longer bound: a step of the other host can take minutes.
func longWait(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !ok(); time.Sleep(500 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout after %s: %s", d, what)
		}
	}
}

// TestTwoHostLab is the two-host federation check (lead-2host): one lab hub on each of two hosts,
// each the other's peer, with the real Messaging plugin. Not part of the main lab: it skips unless
// HARNESS_LAB=1 and the four variables below are set. Run one side on each host, b first:
//
//	HARNESS_LAB=1 TWOHOST_ROLE=b TWOHOST_LISTEN=http://<this host's tailnet IP>:<port> \
//	  TWOHOST_PEER=http://<the other host's tailnet IP>:<port> MESSAGING_DIR=<Messaging checkout> \
//	  go test -count=1 -v -run '^TestTwoHostLab$' ./cmd/hub
//
// then the same with TWOHOST_ROLE=a on the other host. Role a (agent x) sends to claude:y@b and
// waits for the reply; role b (agent y) reads it and replies with reply_to. Each side then needs
// an up link and empty outboxes, and keeps its lab server up 60 s for the other side.
//
// Setup notes: the lab server of each side gets an extra listener on TWOHOST_LISTEN with
// --allow-anonymous, so the other host's hub can pull from it. Anonymous TCP gives every caller
// full access: use only a tailnet address (never 0.0.0.0), only on a lab server, never on a real
// Rex server. The listener goes away with the lab server at the end of the test. Before the hub
// starts, the test logs whoami of both lab servers; check that the peer's pid and instance_id are
// the other lab server's, not the real server's (docs/rex-protocol.md, "A lab server reached
// from another host").
func TestTwoHostLab(t *testing.T) {
	role, listen, peer, dir := os.Getenv("TWOHOST_ROLE"), os.Getenv("TWOHOST_LISTEN"), os.Getenv("TWOHOST_PEER"), os.Getenv("MESSAGING_DIR")
	if role == "" || listen == "" || peer == "" || dir == "" {
		t.Skip("set TWOHOST_ROLE, TWOHOST_LISTEN, TWOHOST_PEER and MESSAGING_DIR")
	}
	me, other, agent, them := "a", "b", "x", "y"
	if role == "b" {
		me, other, agent, them = "b", "a", "y", "x"
	}
	_, root := harnessBinary(t)
	s := messagingLab(t, dir, root, "--extra-listen", listen, "--allow-anonymous")
	s.setPeer(me, other, peer)
	own, err := exec.Command("rex", "-S", s.l.server, "--autostart=false", "--timeout", "10s", "whoami", "--json").CombinedOutput()
	t.Logf("this lab %s whoami: %s (err %v)", s.l.server, own, err)
	// The peer must be the other host's LAB server, never a real server (lead-2host).
	longWait(t, "the peer lab server answers", 5*time.Minute, func() bool {
		out, err := exec.Command("rex", "-S", peer, "--autostart=false", "--timeout", "10s", "whoami", "--json").CombinedOutput()
		if err == nil {
			t.Logf("peer %s whoami: %s", peer, out)
		}
		return err == nil
	})
	s.start()
	s.registered("messaging")
	ag := s.opsAgent(agent)
	myID, themID := s.agentID("claude:"+agent), "claude:"+them
	t.Logf("hub %s, agent %s", me, myID)
	read := func(what string) map[string]any {
		var msg map[string]any
		longWait(t, what, 5*time.Minute, func() bool {
			r, err := s.agentDo(ag, "messaging.read", map[string]any{})
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
	if role == "a" {
		longWait(t, "the link to b is up", 5*time.Minute, func() bool {
			st := s.must("hub.peer.status", map[string]any{})
			p := st["peers"].([]any)[0].(map[string]any)
			return p["state"] == "up"
		})
		sent, err := s.agentDo(ag, "messaging.send", map[string]any{"to": themID + "@b", "body": "hello from the Mac hub", "key": "twohost-1"})
		if err != nil {
			t.Fatalf("x sends to %s@b: %v", themID, err)
		}
		first, _ := sent["id"].(string)
		t.Logf("x sent %s at %s", first, time.Now().UTC().Format(time.RFC3339))
		reply := read("x gets the reply from b")
		t.Logf("x on a got at %s: %v", time.Now().UTC().Format(time.RFC3339), reply)
		if reply["from"] != themID+"@b" || reply["body"] != "hello back from the Linux hub" || reply["reply_to"] != first {
			t.Fatalf("x got %v, want from %s@b, the body and reply_to %s", reply, themID, first)
		}
	} else {
		got := read("y gets the message from a")
		t.Logf("y on b got at %s: %v", time.Now().UTC().Format(time.RFC3339), got)
		if got["from"] != themID+"@a" || got["body"] != "hello from the Mac hub" {
			t.Fatalf("y got %v, want from %s@a and the body", got, themID)
		}
		if _, err := s.agentDo(ag, "messaging.send", map[string]any{"to": themID + "@a", "body": "hello back from the Linux hub", "key": "twohost-2", "reply_to": got["id"]}); err != nil {
			t.Fatalf("y replies to %s@a: %v", themID, err)
		}
	}
	longWait(t, "an empty outbox and an up link", 5*time.Minute, func() bool {
		st := s.must("hub.peer.status", map[string]any{})
		p := st["peers"].([]any)[0].(map[string]any)
		return p["state"] == "up" && p["outbox"] == float64(0)
	})
	if n := s.messagingOutbox(); n != "0" {
		t.Fatalf("Messaging peer_outbox has %s rows", n)
	}
	t.Logf("hub.peer.status: %v", s.must("hub.peer.status", map[string]any{}))
	// Keep this lab server up for the other side's last pulls and its own final check.
	time.Sleep(60 * time.Second)
}
