package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// Setup on a real terminal: a lab Rex pane runs lazyrex-hub setup, the test answers each prompt
// with rex block send, and the lab server then loads the written Rex config and runs the boot
// entry LazyRex: Set Up. Lab only: LAZYREX_LAB=1.
//
// Isolation: the test builds the binary first. Then it clears the whole environment of this test
// process and sets only the variables below (as env -i does), and restores the environment at the
// cleanup. The lab server, every lab pane (also the one that the boot entry opens) and every
// command of the test inherit only these: HOME, the agent and XDG directories under the lab, PATH
// with a fake claude and a fake git that prints the tags, and the lab Rex config. With HOME
// isolated, Rex's default server is a socket under the lab HOME (checked first: no server answers
// there). Setup names the lab server with -server.
func TestSetupLab(t *testing.T) {
	needLab(t)
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	lab, err := os.MkdirTemp("/tmp", "rexlab-setup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	bin := filepath.Join(lab, "bin", "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	rexBin, err := exec.LookPath("rex")
	if err != nil {
		t.Fatal(err)
	}
	tools := filepath.Join(lab, "tools")
	home := filepath.Join(lab, "home")
	rexcfg := filepath.Join(lab, "rexcfg")
	for p, body := range map[string]string{
		filepath.Join(lab, "bin", "lazyrex-view"): "#!/bin/sh\n",
		filepath.Join(tools, "claude"):            "#!/bin/sh\n",
		filepath.Join(tools, "git"):               "#!/bin/sh\nprintf '0123\\trefs/tags/v0.1.0\\n0124\\trefs/tags/v0.2.0\\n'\n",
		filepath.Join(rexcfg, "init.lua"):         "-- the lab config\n",
		filepath.Join(home, ".keep"):              "",
	} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o755)
	}
	initLua := filepath.Join(rexcfg, "init.lua")
	path := tools + ":" + filepath.Dir(rexBin) + ":/usr/bin:/bin"
	saved := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			k, v, _ := strings.Cut(kv, "=")
			os.Setenv(k, v)
		}
	})
	os.Clearenv()
	for k, v := range map[string]string{
		"HOME": home, "PATH": path, "USER": "lab", "LANG": "C.UTF-8",
		"REX_CONFIG": initLua, "LAB_CONFIG": initLua,
		// The agent directories: Claude Code under the lab HOME; no Codex directory (the skip case).
		"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"), "CODEX_HOME": filepath.Join(home, ".codex"),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_DATA_HOME": filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME": filepath.Join(home, ".local", "state"), "XDG_CACHE_HOME": filepath.Join(home, ".cache"),
		"XDG_RUNTIME_DIR": filepath.Join(lab, "run"),
	} {
		os.Setenv(k, v)
	}
	dir := filepath.Join(home, ".lazyrex")

	// The default server in the isolated HOME: no server answers, and setup writes nothing.
	out, err := exec.Command(bin, "setup", "-print").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "no Rex server answered") {
		t.Fatalf("the default server in the lab HOME: %v\n%s", err, out)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 1 {
		t.Fatalf("setup wrote into the lab HOME: %v", ents)
	}

	startLabServer(t, root, lab)
	socket := "unix://" + lab + "/rex.sock"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := rex.Dial(ctx, filepath.Join(lab, "rex.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) // first, so it runs after every cleanup that uses r
	go func() {
		for range r.Events {
		}
	}()
	var s struct {
		ID string `json:"session_id"`
	}
	if err := r.Call(ctx, "session.create", map[string]any{"label": "setup-lab"}, &s); err != nil {
		t.Fatal(err)
	}
	r.Call(ctx, "session.attach", map[string]any{"session_id": s.ID, "event_scope": "session"}, nil)
	// Standard input is the pane's terminal. The output also goes to a file through tee, so the
	// test reads all of it (a screen capture shows only the last rows).
	log := filepath.Join(lab, "setup.out")
	pane := map[string]any{"flavor": "com.superlogical.terminal.shell", "label": "setup",
		"options": map[string]any{"login": false, "exit": map[string]any{"on_completion": false}, "command": []string{"/bin/sh", "-c",
			`"$1" setup -dir "$2" -server "$3" 2>&1 | tee "$4"`, "sh", bin, dir, socket, log}}}
	var w struct {
		Blocks []string `json:"block_ids"`
	}
	if err := r.Call(ctx, "session.new_window", map[string]any{"session_id": s.ID, "focus": true, "layout": map[string]any{"block": pane}}, &w); err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) string {
		out, err := exec.Command(rexBin, append([]string{"-S", socket, "--autostart=false"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("rex %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	waitFor := func(what string, ok func(string) bool) string {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			if s := readFile(log); ok(s) {
				return s
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %s:\n%s", what, readFile(log))
			}
		}
	}
	claudeSkill := filepath.Join(home, ".claude", "skills", "lazyrex", "SKILL.md")
	answers := []struct{ file, answer string }{
		{filepath.Join(dir, "hub.json"), "y"},
		{filepath.Join(dir, "state", "plugins.json"), "y"},
		{filepath.Join(dir, "profiles.json"), "y"},
		{filepath.Join(rexcfg, "lazyrex_boot.lua"), "y"},
		{initLua, "y"},
		{filepath.Join(home, ".claude", "settings.json"), "y"},
		{claudeSkill, "n"},
	}
	for i, a := range answers {
		prompt := "Write " + a.file + "? [y/N] "
		got := waitFor(prompt, func(s string) bool { return strings.Count(s, "[y/N] ") == i+1 && strings.HasSuffix(s, prompt) })
		if i == 0 {
			// Before any answer: the resolved endpoint, and the full path of every proposed file
			// (the file list comes before the first diff).
			want := []string{`"server": "` + socket + `"`, `"palette_lua": "` + filepath.Join(rexcfg, "lazyrex_palette.lua") + `"`,
				"skipped    Codex: no codex on PATH and no " + filepath.Join(home, ".codex") + "."}
			for _, a := range answers {
				state := "new       "
				if a.file == initLua {
					state = "change    " // the existing Rex config
				}
				want = append(want, state+" "+a.file+"\n")
			}
			for _, want := range want {
				if !strings.Contains(got, want) {
					t.Fatalf("before the first answer, no %q:\n%s", want, got)
				}
			}
		}
		cli("block", "send", "-s", s.ID, "-b", w.Blocks[0], a.answer+"\r")
	}
	// No plugin sync: the hub installs the managed file's entries at its first start.
	// Rex's own skill and hooks: the real rex installs them into the lab HOME, for Claude only
	// (setup found no Codex), and its status confirms them.
	skillPrompt := "Install Rex's own skill and hooks for claude now (rex skill setup --agent claude)? [y/N] "
	waitFor(skillPrompt, func(s string) bool { return strings.HasSuffix(s, skillPrompt) })
	cli("block", "send", "-s", s.ID, "-b", w.Blocks[0], "y\r")
	got := waitFor("the report", func(s string) bool {
		return strings.Contains(s, "Load the LazyRex palette entries: rex -S "+shellWord(socket)+" config reload")
	})
	if strings.Contains(got, "plugin sync") {
		t.Fatalf("setup offers the plugin sync:\n%s", got)
	}
	if !strings.Contains(got, "Rex's skill and hooks: installed for claude.") || strings.Contains(got, "Install Rex's own skill and hooks: ") {
		t.Fatalf("the Rex skill:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "rex")); err != nil {
		t.Fatalf("no Rex skill in the lab HOME: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "rex")); !os.IsNotExist(err) {
		t.Fatalf("rex skill setup went beyond Claude: %v", err)
	}
	if strings.Count(got, "Written:") != 6 || !strings.Contains(got, "Declined: "+claudeSkill+"\n") {
		t.Fatalf("report:\n%s", got)
	}
	// hub.json has no plugins; the hub's load reads the six from the managed file at their tag.
	cfg, err := loadConfig(filepath.Join(dir, "hub.json"))
	if err != nil || cfg.Server != socket || cfg.managedErr != nil || len(cfg.entries) != 6 || cfg.entries[1].Source != "github:lazy-rex/Messaging@v0.2.0" {
		t.Fatalf("hub.json: %v %+v", err, cfg)
	}
	if init := readFile(initLua); !strings.HasPrefix(init, "-- the lab config\n") || !strings.Contains(init, `pcall(require, "lazyrex_boot")`) {
		t.Fatalf("init.lua:\n%s", init)
	}
	if _, err := os.Stat(claudeSkill); !os.IsNotExist(err) {
		t.Fatal("the declined skill was written")
	}

	// The lab server loads the written config: the boot entry is an action.
	cli("config", "reload")
	if acts := cli("actions"); !strings.Contains(acts, "lazyrexboot_setup") {
		t.Fatalf("no boot action after the reload:\n%s", acts)
	}
	// The boot entry opens a window that runs the same setup: the same binary, -dir and -server.
	// Only the declined skill is left, so it stops at that prompt.
	res := cli("-s", s.ID, "do", "lazyrexboot_setup")
	var opened struct {
		Blocks []string `json:"block_ids"`
	}
	if json.Unmarshal([]byte(res[strings.Index(res, "{"):]), &opened) != nil || len(opened.Blocks) != 1 {
		t.Fatalf("the boot action: %s", res)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		screen := cli("block", "capture", "-s", s.ID, "-b", opened.Blocks[0])
		if strings.Contains(screen, "Write "+claudeSkill+"? [y/N]") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the boot pane:\n%s", screen)
		}
	}
	var proc struct {
		Child struct {
			Path string `json:"path"`
		} `json:"child"`
	}
	json.Unmarshal([]byte(cli("block", "call", "-s", s.ID, "-b", opened.Blocks[0], "process")), &proc)
	if real, _ := filepath.EvalSymlinks(bin); proc.Child.Path != real {
		t.Fatalf("the boot pane runs %q, not %q", proc.Child.Path, real)
	}
	r.Call(context.Background(), "session.destroy", map[string]any{"session_id": s.ID}, nil)
}
