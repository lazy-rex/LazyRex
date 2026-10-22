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
)

// A real renderer pane shows a view whose tree comes in reply parts (docs/hub.md "Reply parts").
// The plugin testdata/holdparts stops a render before its last part: the pane keeps the old tree
// for the whole hold, and shows the new tree after the release. Lab only: CHUNK_LAB=1 (the test
// starts its own lab server and hub, with -test for hub.test.sql).
func TestReplyPartsHoldLab(t *testing.T) {
	if os.Getenv("CHUNK_LAB") != "1" {
		t.Skip("set CHUNK_LAB=1 to run the reply parts lab test (it starts its own lab server)")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	lab, err := os.MkdirTemp("/tmp", "rexlab-chunk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			os.RemoveAll(lab)
		}
	})
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
		"palette_lua": filepath.Join(rexcfg, "lazyrex_palette.lua"), "agents": []any{map[string]string{"id": "user", "name": "User"}},
		"plugins": []any{map[string]string{"source": filepath.Join(root, "view", "testdata", "holdparts")}}, "test_roots": []string{lab}})
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
		for deadline := time.Now().Add(within); !ok(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
		}
	}
	waitFor("the lab server", 10*time.Second, func() bool { _, err := cli("ls"); return err == nil })

	hub := exec.Command(hubBin, "-test", "-config", cfg)
	hub.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + lab, "REX_SERVER=" + socket}
	logf, _ := os.OpenFile(filepath.Join(lab, "hub.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	hub.Stdout, hub.Stderr = logf, logf
	if err := hub.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		hub.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { hub.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			hub.Process.Kill()
			<-done
		}
		logf.Close()
	})
	waitFor("the plugin's palette entry", 30*time.Second, func() bool {
		out, _ := cli("actions")
		return strings.Contains(out, "lazyrex_holdp_parts")
	})

	// sql runs one statement on the plugin's database as another writer (hub.test.sql); a write
	// renders the plugin's views again.
	sql := func(text string, write bool) [][]any {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"plugin": "holdp", "sql": text, "params": []any{}, "write": write})
		f := filepath.Join(lab, "sql.json")
		os.WriteFile(f, args, 0o600)
		out := must("-C", "lazyrex-hub", "do", "hub.test.sql", "--args", "@"+f)
		var r struct {
			Rows [][]any `json:"rows"`
		}
		json.Unmarshal([]byte(out), &r)
		return r.Rows
	}
	sql("INSERT INTO hold VALUES (1, 1)", true)
	sql("INSERT INTO held VALUES (0)", true)

	var s struct {
		ID string `json:"session_id"`
	}
	json.Unmarshal([]byte(must("api", "call", "session.create", `{"label":"chunk-lab","initial_windows":[{"window_label":"w","layout":{"block":{"flavor":"com.superlogical.terminal.shell","options":{"command":["/bin/sleep","600"],"login":false}}}}]}`)), &s)
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
	// so rex can exit 1 after the form opened.
	cli("-s", s.ID, "do", "lazyrex_holdp_parts")
	var pane string
	waitFor("the form's renderer pane", 20*time.Second, func() bool {
		for id := range blocks() {
			if !before[id] {
				pane = id
			}
		}
		return pane != ""
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	if err := owner.call(ctx, "block.method", map[string]any{"session_id": s.ID, "block_id": pane, "method": "resize", "args": map[string]int{"columns": 80, "rows": 20}}, nil); err != nil {
		t.Fatal(err)
	}
	screen := func() string { out, _ := cli("block", "capture", "-s", s.ID, "-b", pane); return out }
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the pane at the failure:\n%s\nlab kept: %s", screen(), lab)
		}
	})
	waitFor("the first tree (gen 1)", 30*time.Second, func() bool { return strings.Contains(screen(), "gen 1") })

	// The next render stops before its last part.
	sql("UPDATE hold SET gen = 2, release = 0", true)
	waitFor("the hold before the last part", 8*time.Second, func() bool {
		rows := sql("SELECT n FROM held", false)
		return len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] == float64(1)
	})
	// For the whole hold, the pane shows the old tree.
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if sc := screen(); !strings.Contains(sc, "gen 1") || strings.Contains(sc, "gen 2") {
			t.Fatalf("during the hold the pane does not show the old tree:\n%s", sc)
		}
	}
	t.Logf("during the hold:\n%s", screen())

	sql("UPDATE hold SET release = 1", true)
	waitFor("the new tree (gen 2)", 30*time.Second, func() bool {
		sc := screen()
		return strings.Contains(sc, "gen 2") && !strings.Contains(sc, "gen 1")
	})
	t.Logf("after the release:\n%s", screen())
	if b, _ := os.ReadFile(filepath.Join(lab, "hub.log")); strings.Contains(string(b), "parts_invalid") {
		t.Fatalf("the hub refused a part set:\n%s", b)
	}
	must("api", "call", "session.destroy", `{"session_id":"`+s.ID+`"}`)
}
