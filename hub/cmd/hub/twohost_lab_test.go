package main

import (
	"context"
	"encoding/json"
	"fmt"
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
// With TWOHOST_SYNC=1 on both sides, the check also covers plugin sync (docs/hub.md, "Plugin
// sync"): b syncs a managed Profiles entry to a's pin (syncSetup, syncCheck). a then keeps its lab
// server up 6 minutes.
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
	sync := os.Getenv("TWOHOST_SYNC") != ""
	if sync {
		s.syncSetup(role)
	}
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
	hold := 60 * time.Second
	if sync {
		hold = 6 * time.Minute // b's sync check reads a's pins
		if role == "b" {
			s.syncCheck(peer)
			hold = 60 * time.Second
		}
	}
	t.Logf("hub.peer.status: %v", s.must("hub.peer.status", map[string]any{}))
	// Keep this lab server up for the other side's last pulls and its own final check.
	time.Sleep(hold)
}

// syncProfiles is the plugin of the sync check: a has the newer tag, b the older one. Both tags
// use SDK "0" and no hub aliases, and v0.3.0 opens a v0.2.0 database.
const syncProfiles = "github:lazy-rex/Profiles@"

// syncSetup prepares the plugin sync part of TestTwoHostLab (TWOHOST_SYNC=1): a managed entry
// "prof" of Profiles (a: v0.3.0, b: v0.2.0), installed from GitHub with plugin sync before the
// hub starts. b also gets plugins_from on its peer a. This needs network access to github.com.
func (s *startLab) syncSetup(role string) {
	s.t.Helper()
	ref := "v0.3.0"
	if role == "b" {
		ref = "v0.2.0"
		var c map[string]any
		b, _ := os.ReadFile(s.cfg)
		json.Unmarshal(b, &c)
		c["peers"].([]any)[0].(map[string]any)["plugins_from"] = true
		b, _ = json.MarshalIndent(c, "", " ")
		if err := os.WriteFile(s.cfg, b, 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
	s.writeManagedFile(`{"plugins":[{"id":"prof","source":"` + syncProfiles + ref + `"}]}`)
	if err := pluginSync(context.Background(), s.cfg, pluginCmdOpts{}); err != nil {
		s.t.Fatalf("plugin sync of %s%s: %v", syncProfiles, ref, err)
	}
}

// syncCheck is b's part of the plugin sync check: b's managed entry prof runs the commit of a's
// Profiles pin (read from a with hub.peer.plugins), and the last sync has no error and no op.
func (s *startLab) syncCheck(peer string) {
	s.t.Helper()
	first := s.managedEntry("prof")
	s.t.Logf("b's prof before the sync: %v", first)
	var want string
	longWait(s.t, "a's pins have Profiles", 5*time.Minute, func() bool {
		out, err := exec.Command("rex", "-S", peer, "--autostart=false", "--timeout", "10s", "-C", "lazyrex-hub", "do", "hub.peer.plugins", "--args", `{"hub":"b"}`).Output()
		if err != nil {
			return false
		}
		var r struct {
			Pins pinDoc `json:"pins"`
		}
		if json.Unmarshal(out, &r) != nil {
			return false
		}
		for _, e := range r.Pins.Entries {
			if e.Repo == "Profiles" {
				s.t.Logf("a's pin: %+v", e)
				want = e.Commit
			}
		}
		return want != ""
	})
	ops := map[string]bool{}
	longWait(s.t, "b runs a's Profiles pin", 5*time.Minute, func() bool {
		st := s.must("hub.peer.status", map[string]any{})
		ps, _ := st["peers"].([]any)[0].(map[string]any)["plugin_sync"].(map[string]any)
		up, _ := ps["updating"].([]any)
		for _, u := range up {
			if u := u.(map[string]any); !ops[fmt.Sprint(u["op_id"], u["phase"])] {
				ops[fmt.Sprint(u["op_id"], u["phase"])] = true
				s.t.Logf("b's sync op: %v", u)
			}
		}
		e := s.managedEntry("prof")
		return ps != nil && ps["last_ok"] != nil && ps["last_error"] == "" && len(up) == 0 &&
			e != nil && e["commit"] == want && e["state"] == "running"
	})
	if first["commit"] == want {
		s.t.Fatalf("b started at a's pin %s: the check proves nothing", want)
	}
	if len(ops) == 0 {
		s.t.Fatalf("b reached a's pin %s with no peer op", want)
	}
	s.t.Logf("b's prof after the sync: %v", s.managedEntry("prof"))
}
