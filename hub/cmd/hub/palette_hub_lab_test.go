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

// The hub's own palette entries through Rex: a lab server's config loads the generated file, and each
// entry reaches its admin action on the hub (docs/hub.md, "Agent resume"). Lab only: PALETTE_LAB=1.
func TestHubPaletteLab(t *testing.T) {
	if os.Getenv("PALETTE_LAB") != "1" {
		t.Skip("set PALETTE_LAB=1 to run the hub palette lab test (it starts its own lab server)")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	lab, err := os.MkdirTemp("/tmp", "rexlab-pal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	init := filepath.Join(lab, "init.lua")
	if err := os.WriteFile(init, []byte(`pcall(require, "lazyrex_palette")`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAB_CONFIG", init)
	startLabServer(t, root, lab)
	socket := "unix://" + lab + "/rex.sock"

	state := filepath.Join(tmp, "state")
	cfg := filepath.Join(tmp, "hub.json")
	b, _ := json.Marshal(map[string]any{"server": socket, "state": state, "palette_lua": filepath.Join(lab, "lazyrex_palette.lua"),
		"agents": []map[string]any{{"id": "user", "name": "User", "status_record": ""}}})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	hubLog := filepath.Join(tmp, "hub.log")
	hub, exit := startShutdownHub(t, bin, cfg, hubLog)
	waitHubStarted(t, hubLog)
	// The hub writes the file and reloads the Rex config: a reload fails on an error in the file.
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		ev := readFile(filepath.Join(state, "events.jsonl"))
		if strings.Contains(ev, `"event":"palette_written"`) && strings.Contains(ev, `"reload_ok":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no successful palette reload:\n%s", ev)
		}
	}
	run := func(name string) (string, error) {
		out, err := exec.Command("rex", "-S", socket, "--autostart=false", "--timeout", "15s", "do", name).CombinedOutput()
		return string(out), err
	}
	out, err := run("lazyrexhub_resume_detached")
	// No agent is detached: the operation is admitted with no target and is done.
	if err != nil || !strings.Contains(out, `"op_id"`) || !strings.Contains(out, `"done": true`) && !strings.Contains(out, `"done":true`) {
		t.Errorf("resume: %v\n%s", err, out)
	}
	if ev := readFile(filepath.Join(state, "events.jsonl")); strings.Count(ev, `"event":"agent_op_admitted"`) != 1 {
		t.Errorf("want 1 admitted operation:\n%s", ev)
	}

	hub.Process.Signal(syscall.SIGTERM)
	if c := waitHubExit(t, hub, exit); c != 0 {
		t.Errorf("the hub exited %d", c)
	}
	if out, err := run("lazyrexhub_resume_detached"); err == nil || !strings.Contains(out, "the LazyRex hub is not running") {
		t.Errorf("without a hub: %v\n%s", err, out)
	}
}
