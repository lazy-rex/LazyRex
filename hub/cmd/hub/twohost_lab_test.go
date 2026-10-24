package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// LAZYREX_LAB=1 and the four variables below are set. Run one side on each host, b first:
//
//	LAZYREX_LAB=1 TWOHOST_ROLE=b TWOHOST_LISTEN=http://<this host's tailnet IP>:<port> \
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

// The plugin of the sync check: a has the newer commit of Profiles main, b the commit before it.
// Both run on the current hub and have the same schema (base step 2).
const (
	syncProfiles = "github:lazy-rex/Profiles@"
	syncA        = "d484e72a1e44255961e9408ca8e02f2b0e7ee10c"
	syncB        = "de5c0ada9ce50fac40f32611e18e9e5f8a6e06ae"
)

// syncMirror makes the lab's git mirror of lazy-rex/Profiles (coordinator decision
// coord-rel-cred-mirrors: no GitHub credential in a lab process). The test process clones with
// its own environment; a lab gitconfig then maps the GitHub URLs of lazy-rex to the mirror, and
// every other URL to a mirror that does not exist. The hub uses that gitconfig, so the hub's own
// fetch runs, with no network.
func (s *startLab) syncMirror() {
	s.t.Helper()
	m := filepath.Join(s.l.dir, "mirrors", "lazy-rex")
	if err := os.MkdirAll(m, 0o700); err != nil {
		s.t.Fatal(err)
	}
	git := func(args ...string) {
		s.t.Helper()
		c := exec.Command("git", args...)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := c.CombinedOutput(); err != nil {
			s.t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("clone", "-q", "--mirror", "https://github.com/lazy-rex/Profiles", filepath.Join(m, "Profiles.git"))
	if err := os.Symlink("Profiles.git", filepath.Join(m, "Profiles")); err != nil {
		s.t.Fatal(err)
	}
	for _, c := range []string{syncA, syncB} {
		git("-C", filepath.Join(m, "Profiles.git"), "cat-file", "-e", c+"^{commit}")
	}
	cfg := filepath.Join(s.l.dir, "gitconfig")
	none := "file://" + filepath.Join(s.l.dir, "mirrors", "none") + "/"
	body := fmt.Sprintf("[url %q]\n\tinsteadOf = https://github.com/lazy-rex/\n\tinsteadOf = git@github.com:lazy-rex/\n[url %q]\n\tinsteadOf = https://\n\tinsteadOf = git@\n\tinsteadOf = ssh://\n",
		"file://"+m+"/", none)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		s.t.Fatal(err)
	}
	env := []string{"GIT_CONFIG_GLOBAL=" + cfg, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file"}
	s.l.env = append(s.l.env, env...)
}

// syncSetup prepares the plugin sync part of TestTwoHostLab (TWOHOST_SYNC=1): a managed entry
// "prof" of Profiles (a: syncA, b: syncB), installed from the lab mirror by the hub at its
// start. b also gets plugins_from on its peer a. The mirror clone needs access to
// github.com with the test process's own git credentials.
func (s *startLab) syncSetup(role string) {
	s.t.Helper()
	s.syncMirror()
	ref := syncA
	if role == "b" {
		ref = syncB
		var c map[string]any
		b, _ := os.ReadFile(s.cfg)
		json.Unmarshal(b, &c)
		c["peers"].([]any)[0].(map[string]any)["plugins_from"] = true
		b, _ = json.MarshalIndent(c, "", " ")
		if err := os.WriteFile(s.cfg, b, 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
	// The hub installs it from the lab mirror at its start (the install op; the lab env has the
	// mirror git config).
	s.writeManagedFile(`{"plugins":[{"id":"prof","source":"` + syncProfiles + ref + `"}]}`)
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
