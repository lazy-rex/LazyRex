package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// A real renderer pane in a 38-column pane on a lab server, through the hub's restart form (a view
// that the hub owns, so no plugin is needed): the hub stops, the pane shows the no-hub text with
// its first sentence whole; the hub starts again, the pane attaches and the text goes. Lab only:
// HUBUNAVAIL_LAB=1 (the test starts its own lab server and hub).
func TestNoHubTextLab(t *testing.T) {
	if os.Getenv("HUBUNAVAIL_LAB") != "1" {
		t.Skip("set HUBUNAVAIL_LAB=1 to run the no-hub lab test (it starts its own lab server)")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	lab, err := os.MkdirTemp("/tmp", "rexlab-hubunavail-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	bin := filepath.Join(lab, "bin")
	view, hubBin := filepath.Join(bin, "lazyrex-view"), filepath.Join(bin, "lazyrex-hub")
	for dir, out := range map[string]string{".": view, filepath.Join(root, "hub", "cmd", "hub"): hubBin} {
		build := exec.Command("go", "build", "-o", out, ".")
		build.Dir = dir
		if b, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", dir, err, b)
		}
	}
	socket := "unix://" + lab + "/rex.sock"
	rexcfg := filepath.Join(lab, "rexcfg")
	os.MkdirAll(rexcfg, 0o755)
	initLua := filepath.Join(rexcfg, "init.lua")
	os.WriteFile(initLua, []byte(`pcall(require, "lazyrex_palette")`+"\n"), 0o644)
	cfg := filepath.Join(lab, "hub.json")
	c, _ := json.Marshal(map[string]any{"server": socket, "state": filepath.Join(lab, "state-hub"), "view": view,
		"palette_lua": filepath.Join(rexcfg, "lazyrex_palette.lua"), "agents": []any{map[string]string{"id": "user", "name": "User"}}})
	os.WriteFile(cfg, c, 0o600)

	server := exec.Command(filepath.Join(root, "lab", "server.sh"), lab)
	server.Env = append(os.Environ(), "LAB_CONFIG="+initLua)
	serverLog, _ := os.Create(filepath.Join(lab, "server.log"))
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Process.Signal(syscall.SIGTERM); server.Wait(); serverLog.Close() })

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
	waitFor := func(what string, within time.Duration, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(within); !ok(); time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
		}
	}
	waitFor("the lab server", 10*time.Second, func() bool { _, err := cli("ls"); return err == nil })

	var hub *exec.Cmd
	startHub := func() {
		t.Helper()
		hub = exec.Command(hubBin, "-config", cfg)
		hub.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + lab, "REX_SERVER=" + socket}
		logf, _ := os.OpenFile(filepath.Join(lab, "hub.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		hub.Stdout, hub.Stderr = logf, logf
		if err := hub.Start(); err != nil {
			t.Fatal(err)
		}
		waitFor("the hub's palette entries", 20*time.Second, func() bool {
			out, _ := cli("actions")
			return strings.Contains(out, "lazyrexhub_restart_codex")
		})
	}
	stopHub := func() {
		t.Helper()
		hub.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { hub.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			hub.Process.Kill()
			<-done
			t.Fatal("the hub did not stop on SIGTERM")
		}
	}
	startHub()
	t.Cleanup(func() {
		if hub.ProcessState == nil {
			stopHub()
		}
	})

	var s struct {
		ID string `json:"session_id"`
	}
	json.Unmarshal([]byte(must("api", "call", "session.create", `{"label":"hubunavail-lab","initial_windows":[{"window_label":"w","layout":{"block":{"flavor":"com.superlogical.terminal.shell","options":{"command":["/bin/sleep","600"],"login":false}}}}]}`)), &s)
	blocks := func() map[string]bool {
		var l struct {
			Blocks []struct {
				ID string `json:"block_id"`
			} `json:"blocks"`
		}
		json.Unmarshal([]byte(must("api", "call", "session.list_blocks", `{"session_id":"`+s.ID+`"}`)), &l)
		ids := map[string]bool{}
		for _, b := range l.Blocks {
			ids[b.ID] = true
		}
		return ids
	}
	before := blocks()
	// The entry opens the form, then queues session.select on the app that ran it. A lab has no app,
	// so rex exits 1 after the form opened.
	if out, _ := cli("-s", s.ID, "do", "lazyrexhub_restart_codex"); !strings.Contains(out, `"status":"opened"`) {
		t.Fatalf("the restart form did not open:\n%s", out)
	}
	var pane string
	waitFor("the form's renderer pane", 20*time.Second, func() bool {
		for id := range blocks() {
			if !before[id] {
				pane = id
			}
		}
		return pane != ""
	})
	// Rex applies a size from the pane's owner only, and a client owns a pane while it is connected.
	// So the test holds a control connection that takes the pane, as an app with a narrow pane does.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	owner, err := dialWire(ctx, lab+"/rex.sock", func(json.RawMessage) {})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.conn.CloseNow()
	if err := owner.call(ctx, "session.attach", map[string]any{"session_id": s.ID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := owner.call(ctx, "session.own_block", map[string]any{"session_id": s.ID, "block_ids": []string{pane}, "mode": "take"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := owner.call(ctx, "block.method", map[string]any{"session_id": s.ID, "block_id": pane, "method": "resize", "args": map[string]int{"columns": 38, "rows": 14}}, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("size: %s", must("block", "call", "-s", s.ID, "-b", pane, "size"))
	screen := func() string { out, _ := cli("block", "capture", "-s", s.ID, "-b", pane); return out }
	const first = "No hub: run LazyRex: Start Hub." // the action, whole in the one notice row
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the pane at the failure:\n%s", screen())
		}
	})
	waitFor("the attached form", 20*time.Second, func() bool {
		sc := screen()
		return strings.Contains(sc, "No agent can be restarted.")
	})
	t.Logf("attached:\n%s", screen())

	stopHub()
	waitFor("the no-hub text", 20*time.Second, func() bool { return strings.Contains(screen(), first) })
	sc := screen()
	t.Logf("without a hub:\n%s", sc)
	for _, line := range strings.Split(sc, "\n") {
		if w := ansi.StringWidth(line); w > 38 {
			t.Fatalf("a row of %d columns in a 38-column pane: %q", w, line)
		}
	}

	startHub()
	waitFor("the attach after the start", 30*time.Second, func() bool {
		sc := screen()
		return !strings.Contains(sc, "No hub") && strings.Contains(sc, "No agent can be restarted.")
	})
	t.Logf("after the start:\n%s", screen())
	must("api", "call", "session.destroy", `{"session_id":"`+s.ID+`"}`)
}
