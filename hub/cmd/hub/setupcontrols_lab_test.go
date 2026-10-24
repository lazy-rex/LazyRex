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

// The boot file's hub controls on a lab server: the server loads the boot file that setupBootLua
// writes, and the test runs each entry as the palette does (rex do <action> in a session). Each
// entry opens a pane; the test reads the pane's screen and the exit code of its command (Rex shows
// the output in the kept pane, not the exit status: block process last_exit). Lab only:
// LAZYREX_LAB=1. The hub.json names the lab server, so no command gets -real-server.
func TestHubControlsLab(t *testing.T) {
	hubControlsLab(t, false)
}

// The same, with a plugin read that fails: the hub.json "rex" is a script in the lab directory that
// fails hub.plugins and runs the real rex for every other call. Both panes say "unknown", and the
// hub runs on.
func TestHubControlsLabUnknownPlugins(t *testing.T) {
	hubControlsLab(t, true)
}

func hubControlsLab(t *testing.T, failPlugins bool) {
	needLab(t)
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	lab, err := os.MkdirTemp("/tmp", "rexlab-hubctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	bin := filepath.Join(lab, "bin", "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	socket := "unix://" + lab + "/rex.sock"
	// A plugin that is not installed (the lab state has no plugin cache): the hub refuses it and runs.
	const labRefused = "github:lazy-rex/Messaging@v0.1.2"
	cfg := filepath.Join(lab, "hub.json")
	rexField := ""
	if failPlugins {
		realRex, err := exec.LookPath("rex")
		if err != nil {
			t.Fatal(err)
		}
		shim := filepath.Join(lab, "rex-fail-plugins")
		script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = hub.plugins ]; then echo injected-plugin-read-failure >&2; exit 1; fi\ndone\nexec " + shellWord(realRex) + " \"$@\"\n"
		if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		rexField = `"rex":"` + shim + `",`
	}
	os.WriteFile(cfg, []byte(`{`+rexField+`"server":"`+socket+`","state":"`+filepath.Join(lab, "state")+`","agents":[{"id":"user","name":"User"}],"plugins":[{"source":"`+labRefused+`"}]}`), 0o600)
	rexcfg := filepath.Join(lab, "rexcfg")
	os.MkdirAll(rexcfg, 0o755)
	os.WriteFile(filepath.Join(rexcfg, "lazyrex_boot.lua"), []byte(setupBootLua(bin, lab, socket, hubControls{config: cfg, server: socket})), 0o644)
	initLua := filepath.Join(rexcfg, "init.lua")
	os.WriteFile(initLua, []byte(`pcall(require, "lazyrex_boot")`+"\n"), 0o644)
	t.Setenv("LAB_CONFIG", initLua)
	startLabServer(t, root, lab)

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
	acts := must("actions")
	for _, a := range []string{"lazyrexboot_setup", "lazyrexboot_start", "lazyrexboot_stop", "lazyrexboot_status"} {
		if !strings.Contains(acts, a) {
			t.Fatalf("no %s after the load:\n%s", a, acts)
		}
	}
	var s struct {
		ID string `json:"session_id"`
	}
	json.Unmarshal([]byte(must("api", "call", "session.create", `{"label":"hubctl-lab"}`)), &s)
	// run runs a boot entry from the session, as the palette does, and returns its pane.
	run := func(action string) string {
		t.Helper()
		res := must("-s", s.ID, "do", action)
		var opened struct {
			Blocks []string `json:"block_ids"`
		}
		if i := strings.Index(res, "{"); i < 0 || json.Unmarshal([]byte(res[i:]), &opened) != nil || len(opened.Blocks) != 1 {
			t.Fatalf("%s: %s", action, res)
		}
		return opened.Blocks[0]
	}
	// exited waits until the pane's command ends, and returns its exit code and screen.
	exited := func(block string, within time.Duration) (int, string) {
		t.Helper()
		for deadline := time.Now().Add(within); ; time.Sleep(200 * time.Millisecond) {
			var p struct {
				Last *struct {
					Code int `json:"exit_code"`
				} `json:"last_exit"`
			}
			json.Unmarshal([]byte(must("block", "call", "-s", s.ID, "-b", block, "process")), &p)
			if p.Last != nil {
				return p.Last.Code, must("block", "capture", "-s", s.ID, "-b", block)
			}
			if time.Now().After(deadline) {
				t.Fatalf("the pane's command did not end:\n%s", must("block", "capture", "-s", s.ID, "-b", block))
			}
		}
	}
	// hubClient is the connected hub, or "". Rex keeps a lost client in the list for a while.
	hubClient := func() string {
		out, _ := cli("client", "ls", "--json")
		var l struct {
			Clients []struct {
				Info struct {
					Label string `json:"label"`
				} `json:"info"`
				ID    string `json:"client_id"`
				State string `json:"connection_state"`
			} `json:"clients"`
		}
		json.Unmarshal([]byte(out), &l)
		for _, c := range l.Clients {
			if c.Info.Label == "lazyrex-hub" && c.State == "connected" {
				return c.ID
			}
		}
		return ""
	}
	hubListed := func() bool { return hubClient() != "" }
	waitHub := func(want bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); hubListed() != want; time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("a hub listed: %v, want %v", !want, want)
			}
		}
	}
	expect := func(what string, code int, screen string, wantCode int, wantText string) {
		t.Helper()
		if code != wantCode || !strings.Contains(screen, wantText) {
			t.Fatalf("%s: exit %d, want %d with %q:\n%s", what, code, wantCode, wantText, screen)
		}
	}

	code, screen := exited(run("lazyrexboot_status"), 20*time.Second)
	expect("status before a start", code, screen, 1, "not running")

	start := run("lazyrexboot_start")
	waitHub(true)
	if out := must("-C", hubClient(), "do", "hub.info"); !strings.Contains(out, lab) {
		t.Fatalf("hub.info: %s", out)
	}
	// The refused plugin (or, with failPlugins, the unknown line): the Start pane shows it after the
	// ready, Status shows it after the running line, and the hub runs on.
	refusal := "plugin " + labRefused + ": refused: plugin " + labRefused + " is not installed: run lazyrex-hub plugin sync"
	if failPlugins {
		refusal = "plugins: unknown: exit status 1: injected-plugin-read-failure"
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		pane := must("block", "capture", "--unwrap", "-s", s.ID, "-b", start)
		if strings.Contains(pane, "restart: "+refusal) {
			t.Logf("the Start pane:\n%s", pane)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Start pane shows no refusal:\n%s", pane)
		}
	}
	status := run("lazyrexboot_status")
	code, _ = exited(status, 20*time.Second)
	screen = must("block", "capture", "--unwrap", "-s", s.ID, "-b", status)
	t.Logf("Hub Status: exit %d\n%s", code, screen)
	expect("status while it runs", code, screen, 0, "running")
	if i := strings.Index(screen, "running: hub pid"); i < 0 || !strings.Contains(screen[i:], "\n"+refusal) {
		t.Fatalf("status shows no refusal after the running line:\n%s", screen)
	}
	if !hubListed() {
		t.Fatal("the hub stopped after the refusal")
	}

	// A second Start while the wrapper runs: the wrapper's lock refuses it.
	code, screen = exited(run("lazyrexboot_start"), 20*time.Second)
	expect("a second start", code, screen, 1, "another restart wrapper owns")

	code, screen = exited(run("lazyrexboot_stop"), 30*time.Second)
	expect("stop", code, screen, 0, "stopped")
	waitHub(false)
	// The Start pane owned the wrapper: it ended with the hub, and nothing restarts it.
	code, screen = exited(start, 10*time.Second)
	expect("the start pane after the stop", code, screen, 0, "")
	time.Sleep(3 * time.Second)
	if hubListed() {
		t.Fatal("a hub runs again after the stop")
	}
	code, screen = exited(run("lazyrexboot_stop"), 20*time.Second)
	expect("stop when not running", code, screen, 0, "not running")

	// A hub without the wrapper (as dev.sh runs one): Start refuses, and that hub still runs.
	direct := exec.Command(bin, "-config", cfg)
	direct.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + lab, "REX_SERVER=" + socket}
	logf, _ := os.Create(filepath.Join(lab, "direct.log"))
	direct.Stdout, direct.Stderr = logf, logf
	if err := direct.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { direct.Process.Signal(syscall.SIGTERM); direct.Wait(); logf.Close() })
	waitHub(true)
	code, screen = exited(run("lazyrexboot_start"), 30*time.Second)
	t.Logf("start beside a hub without the wrapper: exit %d\n%s", code, screen)
	expect("a start beside a hub without the wrapper", code, screen, 1, "restart: hub: ")
	if !strings.Contains(screen, "another hub owns") {
		t.Fatalf("the pane does not say why:\n%s", screen)
	}
	if out := must("-C", hubClient(), "do", "hub.info"); !strings.Contains(out, lab) {
		t.Fatalf("the first hub after the refused start: %s", out)
	}
	must("api", "call", "session.destroy", `{"session_id":"`+s.ID+`"}`)
}
