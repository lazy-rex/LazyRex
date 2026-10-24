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
// entry reaches its admin action on the hub (docs/hub.md, "Agent resume"). Lab only: LAZYREX_LAB=1.
func TestHubPaletteLab(t *testing.T) {
	needLab(t)
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
	b, _ := json.Marshal(map[string]any{"server": socket, "state": state, "palette_lua": filepath.Join(lab, "lazyrex_palette.lua")})
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

	// The stop (docs/hub.md, "Stop and exit status"): the file is the header alone, the server
	// reloads it, and the hub's entry is gone from the palette.
	began := time.Now()
	hub.Process.Signal(syscall.SIGTERM)
	if c := waitHubExit(t, hub, exit); c != 0 {
		t.Errorf("the hub exited %d", c)
	}
	t.Logf("SIGTERM to exit: %v", time.Since(began))
	if got := readFile(filepath.Join(lab, "lazyrex_palette.lua")); got != paletteHeader {
		t.Errorf("the palette file after the stop:\n%s", got)
	}
	if ev := readFile(filepath.Join(state, "events.jsonl")); !strings.Contains(ev, `"reload_ok":true,"stop":true`) {
		t.Errorf("no stop write with a good reload:\n%s", ev)
	}
	out, err = run("lazyrexhub_resume_detached")
	t.Logf("the entry after the stop: %v: %s", err, strings.TrimSpace(out))
	if err == nil || !strings.Contains(out, `no action named "lazyrexhub_resume_detached"`) {
		t.Errorf("the entry still runs after the stop: %v\n%s", err, out)
	}
}

// A start that fails after the first palette write (the lab fault palette-start) runs the stop
// write too: the file is the header alone, and the hub exits 1.
func TestHubPaletteStartFailsLab(t *testing.T) {
	needLab(t)
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
	b, _ := json.Marshal(map[string]any{"server": socket, "state": state, "palette_lua": filepath.Join(lab, "lazyrex_palette.lua")})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-config", cfg, "-lab-fault", "point=palette-start,mode=fail")
	done := make(chan error, 1)
	var out []byte
	go func() { var err error; out, err = cmd.CombinedOutput(); done <- err }()
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the hub did not exit within 30 s")
	}
	if exitCodeOf(err) != 1 || !strings.Contains(string(out), "lab fault palette-start") {
		t.Errorf("exit %d:\n%s", exitCodeOf(err), out)
	}
	ev := readFile(filepath.Join(state, "events.jsonl"))
	if got := readFile(filepath.Join(lab, "lazyrex_palette.lua")); got != paletteHeader ||
		strings.Count(ev, `"event":"palette_written"`) != 2 || !strings.Contains(ev, `"reload_ok":true,"stop":true`) {
		t.Errorf("after the failed start:\n%s\nevents:\n%s", got, ev)
	}
}
