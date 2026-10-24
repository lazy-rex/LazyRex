package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The autostart of the boot file on a lab server (codex-28: a config reload in the running state and
// in the deliberately stopped state). With the Start Hub command:
//  1. the server start starts the hub;
//  2. a config reload while it runs starts nothing;
//  3. after lazyrex-hub stop, a config reload starts nothing: the Start Hub pane stays.
//
// Then the server restarts with a command as live's dev.sh (a hub without the wrapper, in session
// lazyrex-live-keep, pane lazyrex-dev-process), and 1-3 again. Lab only: LAZYREX_LAB=1.
func TestHubAutostartLab(t *testing.T) {
	needLab(t)
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	lab, err := os.MkdirTemp("/tmp", "rexlab-autostart-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	bin := filepath.Join(lab, "bin", "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	socket := "unix://" + lab + "/rex.sock"
	state := filepath.Join(lab, "hubstate")
	os.MkdirAll(state, 0o700) // autostart.log goes into an existing state only
	cfg := filepath.Join(lab, "hub.json")
	os.WriteFile(cfg, []byte(`{"server":"`+socket+`","state":"`+state+`","agents":[{"id":"user","name":"User"}]}`), 0o600)
	rexcfg := filepath.Join(lab, "rexcfg")
	os.MkdirAll(rexcfg, 0o755)
	writeBoot := func(auto *setupAutostart) {
		os.WriteFile(filepath.Join(rexcfg, "lazyrex_boot.lua"), []byte(setupBootLua(bin, lab, socket, hubControls{config: cfg, server: socket}, auto)), 0o644)
	}
	writeBoot(&setupAutostart{session: "lazyrex-hub", pane: startHubPane})
	initLua := filepath.Join(rexcfg, "init.lua")
	os.WriteFile(initLua, []byte(`pcall(require, "lazyrex_boot")`+"\n"), 0o644)
	t.Setenv("LAB_CONFIG", initLua)
	srv := startLabServer(t, root, lab)
	stopHub := func() string {
		out, _ := exec.Command(bin, "stop", "-config", cfg).CombinedOutput()
		return string(out)
	}
	t.Cleanup(func() { stopHub() }) // before the server stops (cleanups run last first)

	cli := func(args ...string) (string, error) {
		out, err := exec.Command("rex", append([]string{"-S", socket, "--autostart=false", "--timeout", "10s"}, args...)...).CombinedOutput()
		return string(out), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := cli(args...)
		if err != nil {
			t.Fatalf("rex %v: %v\n%s", args, err, out)
		}
		return out
	}
	hubClient := func() string {
		out, _ := cli("client", "ls", "--json")
		ids, _ := connectedHubs([]byte(out))
		if len(ids) == 1 {
			return ids[0]
		}
		return strings.Join(ids, ",")
	}
	waitHub := func(want bool) string {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			if id := hubClient(); (id != "") == want {
				return id
			}
			if time.Now().After(deadline) {
				t.Fatalf("a hub listed: %v, want %v", !want, want)
			}
		}
	}
	// panes lists the labeled panes of every session, as "session/label".
	panes := func(label string) []string {
		t.Helper()
		var ss struct {
			Sessions []struct {
				ID    string `json:"session_id"`
				Label string `json:"label"`
			} `json:"sessions"`
		}
		json.Unmarshal([]byte(must("ls", "--json")), &ss)
		var out []string
		for _, s := range ss.Sessions {
			var bs struct {
				Blocks []struct {
					Label string `json:"label"`
				} `json:"blocks"`
			}
			json.Unmarshal([]byte(must("block", "ls", "-s", s.ID, "--json")), &bs)
			for _, b := range bs.Blocks {
				if b.Label == label {
					out = append(out, s.Label+"/"+b.Label)
				}
			}
		}
		return out
	}
	logPath := filepath.Join(state, "autostart.log")
	waitLog := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			b, _ := os.ReadFile(logPath)
			if strings.Contains(string(b), want) {
				t.Logf("autostart.log: %s", strings.TrimSpace(string(b)))
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("autostart.log %q, want %q", b, want)
			}
		}
	}
	// cycle is 1-3 for one autostart command, in one server instance.
	cycle := func(session, pane string) {
		t.Helper()
		waitLog(`started: pane "` + pane + `" in session "` + session + `"`)
		id := waitHub(true)
		if got := panes(pane); len(got) != 1 || got[0] != session+"/"+pane {
			t.Fatalf("hub panes after the start: %v", got)
		}
		os.Remove(logPath)
		must("config", "reload")
		waitLog("not started: a hub runs (" + id + ")")
		if got := hubClient(); got != id || len(panes(pane)) != 1 {
			t.Fatalf("after a reload while it runs: hub %q (was %q), panes %v", got, id, panes(pane))
		}
		if out := stopHub(); !strings.Contains(out, "stopped") {
			t.Fatalf("stop: %s", out)
		}
		waitHub(false)
		os.Remove(logPath)
		must("config", "reload")
		waitLog(`not started: a pane labeled "` + pane + `" is in session "` + session + `"`)
		time.Sleep(3 * time.Second)
		if id := hubClient(); id != "" || len(panes(pane)) != 1 {
			t.Fatalf("after a reload in the stopped state: hub %q, panes %v", id, panes(pane))
		}
	}

	cycle("lazyrex-hub", startHubPane)

	// A server restart, with live's kind of command: a hub without the wrapper.
	writeBoot(&setupAutostart{argv: []string{bin, "-config", cfg}, session: "lazyrex-live-keep", pane: "lazyrex-dev-process"})
	os.Remove(logPath)
	srv.Process.Signal(syscall.SIGTERM)
	for deadline := time.Now().Add(10 * time.Second); exec.Command("rex", "-S", socket, "--autostart=false", "--timeout", "1s", "ls").Run() == nil; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the lab server did not stop")
		}
	}
	startLabServer(t, root, lab)
	cycle("lazyrex-live-keep", "lazyrex-dev-process")
	if got := panes(startHubPane); len(got) != 0 {
		t.Fatalf("panes of the old server instance: %v", got)
	}
}
