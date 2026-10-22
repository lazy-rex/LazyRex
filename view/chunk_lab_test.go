package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// chunkLab is a lab server and a hub (-test) with one plugin, and real renderer panes (docs/hub.md
// "Reply parts"). Lab only: CHUNK_LAB=1.
type chunkLab struct {
	t       *testing.T
	lab     string
	socket  string
	plugin  string
	session string
	owner   *wire
	ctx     context.Context
}

func newChunkLab(t *testing.T, plugin, source string) *chunkLab {
	if os.Getenv("CHUNK_LAB") != "1" {
		t.Skip("set CHUNK_LAB=1 to run the reply parts lab tests (they start their own lab server)")
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
	l := &chunkLab{t: t, lab: lab, socket: "unix://" + lab + "/rex.sock", plugin: plugin}
	bin := filepath.Join(lab, "bin")
	view, hubBin := filepath.Join(bin, "lazyrex-view"), filepath.Join(bin, "lazyrex-hub")
	for dir, out := range map[string]string{".": view, filepath.Join(root, "hub", "cmd", "hub"): hubBin} {
		build := exec.Command("go", "build", "-o", out, ".")
		build.Dir = dir
		if b, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", dir, err, b)
		}
	}
	rexcfg := filepath.Join(lab, "rexcfg")
	os.MkdirAll(rexcfg, 0o755)
	initLua := filepath.Join(rexcfg, "init.lua")
	os.WriteFile(initLua, []byte(`pcall(require, "lazyrex_palette")`+"\n"), 0o644)
	cfg := filepath.Join(lab, "hub.json")
	c, _ := json.Marshal(map[string]any{"server": l.socket, "state": filepath.Join(lab, "state-hub"), "view": view,
		"palette_lua": filepath.Join(rexcfg, "lazyrex_palette.lua"), "agents": []any{map[string]string{"id": "user", "name": "User"}},
		"plugins": []any{map[string]string{"source": filepath.Join(root, source)}}, "test_roots": []string{lab}})
	os.WriteFile(cfg, c, 0o600)

	server := exec.Command(filepath.Join(root, "lab", "server.sh"), lab)
	server.Env = append(os.Environ(), "LAB_CONFIG="+initLua)
	serverLog, _ := os.Create(filepath.Join(lab, "server.log"))
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Process.Signal(syscall.SIGTERM); server.Wait(); serverLog.Close() })
	l.waitFor("the lab server", 10*time.Second, func() bool { _, err := l.cli("ls"); return err == nil })

	hub := exec.Command(hubBin, "-test", "-sdk", filepath.Join(root, "sdk", "lua"), "-config", cfg)
	hub.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + lab, "REX_SERVER=" + l.socket}
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

	var s struct {
		ID string `json:"session_id"`
	}
	json.Unmarshal([]byte(l.must("api", "call", "session.create", `{"label":"chunk-lab","initial_windows":[{"window_label":"w","layout":{"block":{"flavor":"com.superlogical.terminal.shell","options":{"command":["/bin/sleep","600"],"login":false}}}}]}`)), &s)
	l.session = s.ID
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	l.ctx = ctx
	// Rex applies a size from the pane's owner only. The test holds a control connection that takes
	// each renderer pane, as an app does.
	owner, err := dialWire(ctx, lab+"/rex.sock", func(json.RawMessage) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.conn.CloseNow() })
	if err := owner.call(ctx, "session.attach", map[string]any{"session_id": s.ID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	l.owner = owner
	return l
}

func (l *chunkLab) cli(args ...string) (string, error) {
	out, err := exec.Command("rex", append([]string{"-S", l.socket, "--autostart=false", "--timeout", "10s"}, args...)...).CombinedOutput()
	return string(out), err
}

func (l *chunkLab) must(args ...string) string {
	l.t.Helper()
	out, err := l.cli(args...)
	if err != nil {
		l.t.Fatalf("rex %v: %v\n%s", args, err, out)
	}
	return out
}

func (l *chunkLab) waitFor(what string, within time.Duration, ok func() bool) {
	l.t.Helper()
	for deadline := time.Now().Add(within); !ok(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			l.t.Fatalf("timed out: %s (lab kept: %s)", what, l.lab)
		}
	}
}

// hubTest calls a hub.test action of the lab hub.
func (l *chunkLab) hubTest(action string, args map[string]any, out any) {
	l.t.Helper()
	args["plugin"] = l.plugin
	b, _ := json.Marshal(args)
	f := filepath.Join(l.lab, "args.json")
	os.WriteFile(f, b, 0o600)
	if err := json.Unmarshal([]byte(l.must("-C", "lazyrex-hub", "do", action, "--args", "@"+f)), out); err != nil {
		l.t.Fatalf("%s: %v", action, err)
	}
}

// sql runs one statement on the plugin's database as another writer (hub.test.sql); a write
// renders the plugin's views again.
func (l *chunkLab) sql(text string, write bool) [][]any {
	l.t.Helper()
	var r struct {
		Rows [][]any `json:"rows"`
	}
	l.hubTest("hub.test.sql", map[string]any{"sql": text, "params": []any{}, "write": write}, &r)
	return r.Rows
}

// open runs a palette entry and returns its new renderer pane, taken and sized 80x20.
func (l *chunkLab) open(entry string) (pane string) {
	l.t.Helper()
	l.waitFor("the palette entry "+entry, 30*time.Second, func() bool {
		out, _ := l.cli("actions")
		return strings.Contains(out, entry)
	})
	blocks := func() map[string]bool {
		var r struct {
			Blocks []struct {
				ID string `json:"block_id"`
			} `json:"blocks"`
		}
		json.Unmarshal([]byte(l.must("api", "call", "session.list_blocks", `{"session_id":"`+l.session+`"}`)), &r)
		ids := map[string]bool{}
		for _, b := range r.Blocks {
			ids[b.ID] = true
		}
		return ids
	}
	before := blocks()
	// The entry queues session.select on the app that ran it. A lab has no app, so rex can exit 1
	// after the view opened.
	l.cli("-s", l.session, "do", entry)
	l.waitFor("the renderer pane of "+entry, 20*time.Second, func() bool {
		for id := range blocks() {
			if !before[id] {
				pane = id
			}
		}
		return pane != ""
	})
	if err := l.owner.call(l.ctx, "session.own_block", map[string]any{"session_id": l.session, "block_ids": []string{pane}, "mode": "take"}, nil); err != nil {
		l.t.Fatal(err)
	}
	if err := l.owner.call(l.ctx, "block.method", map[string]any{"session_id": l.session, "block_id": pane, "method": "resize", "args": map[string]int{"columns": 80, "rows": 20}}, nil); err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(func() {
		if l.t.Failed() {
			l.t.Logf("pane %s at the failure:\n%s", pane, l.screen(pane))
		}
	})
	return pane
}

func (l *chunkLab) screen(pane string) string {
	out, _ := l.cli("block", "capture", "-s", l.session, "-b", pane)
	return out
}

func (l *chunkLab) noRefusedParts() {
	l.t.Helper()
	if b, _ := os.ReadFile(filepath.Join(l.lab, "hub.log")); strings.Contains(string(b), "parts_invalid") {
		l.t.Fatalf("the hub refused a part set (lab kept: %s)", l.lab)
	}
}

// The plugin testdata/holdparts renders about 150 KiB in parts and stops a render before its last
// part: the pane keeps the old tree for the whole hold, and shows the new tree after the release.
func TestReplyPartsHoldLab(t *testing.T) {
	l := newChunkLab(t, "holdp", "view/testdata/holdparts")
	// The plugin creates its tables after its registration.
	b, _ := json.Marshal(map[string]any{"plugin": "holdp", "sql": "SELECT count(*) FROM held", "params": []any{}})
	f := filepath.Join(l.lab, "tables.json")
	os.WriteFile(f, b, 0o600)
	l.waitFor("the plugin's tables", 30*time.Second, func() bool {
		_, err := l.cli("-C", "lazyrex-hub", "do", "hub.test.sql", "--args", "@"+f)
		return err == nil
	})
	l.sql("INSERT INTO hold VALUES (1, 1)", true)
	l.sql("INSERT INTO held VALUES (0)", true)
	pane := l.open("lazyrex_holdp_parts")
	l.waitFor("the first tree (gen 1)", 30*time.Second, func() bool { return strings.Contains(l.screen(pane), "gen 1") })

	// The next render stops before its last part (at most 8 s: the hub's render timeout is 10 s).
	l.sql("UPDATE hold SET gen = 2, release = 0", true)
	l.waitFor("the hold before the last part", 8*time.Second, func() bool {
		rows := l.sql("SELECT n FROM held", false)
		return len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] == float64(1)
	})
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if sc := l.screen(pane); !strings.Contains(sc, "gen 1") || strings.Contains(sc, "gen 2") {
			t.Fatalf("during the hold the pane does not show the old tree:\n%s", sc)
		}
	}
	t.Logf("during the hold:\n%s", l.screen(pane))

	l.sql("UPDATE hold SET release = 1", true)
	l.waitFor("the new tree (gen 2)", 30*time.Second, func() bool {
		sc := l.screen(pane)
		return strings.Contains(sc, "gen 2") && !strings.Contains(sc, "gen 1")
	})
	t.Logf("after the release:\n%s", l.screen(pane))
	l.noRefusedParts()
}

// The performance budget of reply parts: through the real SDK (tests/sdk-parts) and real renderer
// panes, the view.render call of a 150 KiB panel (in parts) takes at most 50 ms more than the call
// of a 40 KiB panel (inline). Median of 7 renders each, after one warm-up render.
func TestReplyPartsTimingLab(t *testing.T) {
	l := newChunkLab(t, "sp", "tests/sdk-parts")
	renders := func() map[string]map[string]any {
		var r struct {
			Views []map[string]any `json:"views"`
		}
		l.hubTest("hub.test.renders", map[string]any{}, &r)
		out := map[string]map[string]any{}
		for _, c := range r.Views {
			out[c["route"].(string)] = c
		}
		return out
	}
	// measure opens one panel (a panel replaces the one before it in its split), then writes to its
	// table 8 times. After each write it waits until the pane shows the new count and reads the
	// time of that view.render call. The first write is a warm-up.
	measure := func(entry, table, route string) []float64 {
		t.Helper()
		pane := l.open(entry)
		l.waitFor(route+" shows ticks 0", 30*time.Second, func() bool { return strings.Contains(l.screen(pane), "ticks 0") })
		var ms []float64
		for n := 1; n <= 8; n++ {
			l.sql("INSERT INTO "+table+" VALUES (1)", true)
			want := "ticks " + itoa(n)
			l.waitFor(route+" shows "+want, 20*time.Second, func() bool { return strings.Contains(l.screen(pane), want) })
			c := renders()[route]
			if c == nil {
				t.Fatalf("no render count for %s", route)
			}
			if n > 1 {
				ms = append(ms, c["last_call_ms"].(float64))
			}
		}
		return ms
	}
	// parts counts the hub.reply.part calls in the hub log.
	parts := func() int {
		b, _ := os.ReadFile(filepath.Join(l.lab, "hub.log"))
		return strings.Count(string(b), "hub.reply.part from ")
	}
	p0 := parts()
	tb := measure("lazyrex_sp_big", "big_ticks", "/panels/big")
	p1 := parts()
	ts := measure("lazyrex_sp_small", "small_ticks", "/panels/small")
	p2 := parts()
	if p1-p0 < 2*9 || p2 != p1 {
		t.Fatalf("hub.reply.part calls: %d for the 9 renders of the 150 KiB panel (want 2 or more each), %d for the 40 KiB panel (want 0)", p1-p0, p2-p1)
	}
	t.Logf("hub.reply.part calls: %d for 9 renders of the 150 KiB panel, 0 for the 40 KiB panel", p1-p0)
	sort.Float64s(tb)
	sort.Float64s(ts)
	t.Logf("view.render call ms (median of 7): 150 KiB in parts %.1f, 40 KiB inline %.1f; all: %v %v", tb[3], ts[3], tb, ts)
	if tb[3]-ts[3] > 50 {
		t.Fatalf("parts add %.1f ms; the budget is 50 ms", tb[3]-ts[3])
	}
	l.noRefusedParts()
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
