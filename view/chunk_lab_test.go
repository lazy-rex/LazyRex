package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	hub     *exec.Cmd
	hubArgs []string // the hub binary and its arguments
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
	if os.Getenv("VIEW_TEA_TRACE") == "1" {
		// Each view process logs its renderer writes to <lab>/tea-<pid>.log (Bubble Tea's TEA_TRACE).
		wrapper := filepath.Join(bin, "lazyrex-view-trace")
		script := "#!/bin/sh\nTEA_TRACE=" + lab + "/tea-$$.log exec " + view + " \"$@\"\n"
		if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		view = wrapper
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

	l.hubArgs = []string{hubBin, "-test", "-sdk", filepath.Join(root, "sdk", "lua"), "-config", cfg}
	l.startHub()
	t.Cleanup(func() {
		if l.hub.ProcessState == nil {
			l.stopHub()
		}
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

// startHub starts the lab hub; its output goes to hub.log.
func (l *chunkLab) startHub() {
	l.t.Helper()
	l.hub = exec.Command(l.hubArgs[0], l.hubArgs[1:]...)
	l.hub.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.lab, "REX_SERVER=" + l.socket}
	logf, _ := os.OpenFile(filepath.Join(l.lab, "hub.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	l.hub.Stdout, l.hub.Stderr = logf, logf
	if err := l.hub.Start(); err != nil {
		l.t.Fatal(err)
	}
	logf.Close() // the hub holds its own copy
}

// stopHub stops the lab hub: SIGTERM, then the wait, as the hub's keeper does (labkeeper.go).
func (l *chunkLab) stopHub() {
	l.t.Helper()
	l.hub.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { l.hub.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		l.hub.Process.Kill()
		<-done
		l.t.Logf("the lab hub did not stop on SIGTERM; it was killed")
	}
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

// hubClient returns the client ID of the one connected lab hub, or "". Rex keeps a lost client
// during reconnection (hub hook.go), so after a hub restart "-C lazyrex-hub" can be ambiguous.
func (l *chunkLab) hubClient() string {
	out, err := l.cli("client", "ls", "--json")
	if err != nil {
		return ""
	}
	var listing struct {
		Clients []struct {
			ClientID string `json:"client_id"`
			Info     struct {
				Label string `json:"label"`
			} `json:"info"`
			ConnectionState string `json:"connection_state"`
		} `json:"clients"`
	}
	json.Unmarshal([]byte(out), &listing)
	var hubs []string
	for _, c := range listing.Clients {
		if c.Info.Label == "lazyrex-hub" && c.ConnectionState == "connected" {
			hubs = append(hubs, c.ClientID)
		}
	}
	if len(hubs) != 1 {
		return ""
	}
	return hubs[0]
}

// hubTest calls a hub.test action of the lab hub.
func (l *chunkLab) hubTest(action string, args map[string]any, out any) {
	l.t.Helper()
	args["plugin"] = l.plugin
	b, _ := json.Marshal(args)
	f := filepath.Join(l.lab, "args.json")
	os.WriteFile(f, b, 0o600)
	hub := l.hubClient()
	if hub == "" {
		l.t.Fatalf("%s: no single connected lab hub (lab kept: %s)", action, l.lab)
	}
	if err := json.Unmarshal([]byte(l.must("-C", hub, "do", action, "--args", "@"+f)), out); err != nil {
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

// The plugin testdata/holdparts renders more than 150 KiB in parts and stops a render before its
// last part. Before the hold the pane shows the whole old tree (its first and its last node); for the
// hold it keeps the old tree; after the release it shows the whole new tree.
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
	// The tree fits on one screen (testdata/holdparts puts its bytes in one-row list items), so
	// every capture shows the first and the last node of the tree on the screen.
	whole := func(sc, gen, other string) bool {
		return strings.Contains(sc, "gen "+gen) && strings.Contains(sc, "end "+gen) &&
			!strings.Contains(sc, "gen "+other) && !strings.Contains(sc, "end "+other)
	}
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last screen:\n%s", last)
		}
	})
	l.waitFor("the first tree (gen 1), first and last node", 30*time.Second, func() bool {
		last = l.screen(pane)
		return whole(last, "1", "2")
	})

	// The next render stops before its last part (at most 8 s: the hub's render timeout is 10 s).
	parts := func() int {
		b, _ := os.ReadFile(filepath.Join(l.lab, "hub.log"))
		return strings.Count(string(b), "hub.reply.part from")
	}
	before := parts()
	l.sql("UPDATE hold SET gen = 2, release = 0", true)
	l.waitFor("the hold before the last part", 8*time.Second, func() bool {
		rows := l.sql("SELECT n FROM held", false)
		return len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] == float64(1)
	})
	// For the hold (2 s), every capture shows the whole old tree and no node of the new tree. The
	// plugin holds for at most 8 s, so release must still be 0 after the checks.
	start := time.Now()
	for ; time.Since(start) < 2*time.Second; time.Sleep(100 * time.Millisecond) {
		if last = l.screen(pane); !whole(last, "1", "2") {
			t.Fatalf("during the hold the pane does not show the whole old tree:\n%s", last)
		}
	}
	if rows := l.sql("SELECT release FROM hold", false); len(rows) != 1 || rows[0][0] != float64(0) {
		t.Fatalf("the hold ended before its checks: %v", rows)
	}
	// The held render sent its parts but the last, so the reply has 2 parts or more.
	if n := parts() - before; n < 1 {
		t.Fatalf("the held render sent %d parts before its last part, want 1 or more", n)
	}
	t.Logf("during the hold:\n%s", last)

	l.sql("UPDATE hold SET release = 1", true)
	l.waitFor("the whole new tree (gen 2), first and last node", 30*time.Second, func() bool {
		last = l.screen(pane)
		return whole(last, "2", "1")
	})
	t.Logf("after the release:\n%s", last)
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
	// partTimes returns the hub-side times (dispatch to reply sent, calllog.go) of the
	// hub.reply.part calls that the hub log has after offset, and the new offset.
	partRE := regexp.MustCompile(`hub\.reply\.part from .* in ([0-9.]+)ms`)
	partTimes := func(offset int) ([]float64, int) {
		b, _ := os.ReadFile(filepath.Join(l.lab, "hub.log"))
		var out []float64
		for _, m := range partRE.FindAllStringSubmatch(string(b[offset:]), -1) {
			v, _ := strconv.ParseFloat(m[1], 64)
			out = append(out, v)
		}
		return out, len(b)
	}
	// render is one measured view.render call: its time, and the times of its part calls.
	type render struct {
		call  float64
		parts []float64
	}
	// measure opens one panel (a panel replaces the one before it in its split), then writes to its
	// table 8 times. After each write it waits until the pane shows the new count and reads the
	// time of that view.render call. The first write is a warm-up.
	measure := func(entry, table, route string) []render {
		t.Helper()
		pane := l.open(entry)
		l.waitFor(route+" shows ticks 0", 30*time.Second, func() bool { return strings.Contains(l.screen(pane), "ticks 0") })
		var out []render
		for n := 1; n <= 8; n++ {
			_, offset := partTimes(0)
			l.sql("INSERT INTO "+table+" VALUES (1)", true)
			want := "ticks " + itoa(n)
			l.waitFor(route+" shows "+want, 20*time.Second, func() bool { return strings.Contains(l.screen(pane), want) })
			c := renders()[route]
			if c == nil {
				t.Fatalf("no render count for %s", route)
			}
			parts, _ := partTimes(offset)
			if n > 1 {
				out = append(out, render{call: c["last_call_ms"].(float64), parts: parts})
			}
		}
		return out
	}
	big := measure("lazyrex_sp_big", "big_ticks", "/panels/big")
	small := measure("lazyrex_sp_small", "small_ticks", "/panels/small")
	median := func(rs []render, f func(render) float64) float64 {
		v := make([]float64, len(rs))
		for i, r := range rs {
			v[i] = f(r)
		}
		sort.Float64s(v)
		return v[len(v)/2]
	}
	sum := func(r render) float64 {
		s := 0.0
		for _, p := range r.parts {
			s += p
		}
		return s
	}
	for _, r := range big {
		if len(r.parts) < 2 {
			t.Fatalf("a render of the 150 KiB panel had %d hub.reply.part calls; want 2 or more", len(r.parts))
		}
	}
	for _, r := range small {
		if len(r.parts) != 0 {
			t.Fatalf("a render of the 40 KiB panel had %d hub.reply.part calls; want 0", len(r.parts))
		}
	}
	for i, r := range big {
		t.Logf("150 KiB render %d: call %.1f ms; %d part calls (hub side) %v, sum %.1f ms; residual %.1f ms", i+1, r.call, len(r.parts), r.parts, sum(r), r.call-sum(r))
	}
	call := func(r render) float64 { return r.call }
	residual := func(r render) float64 { return r.call - sum(r) }
	tb, ts := median(big, call), median(small, call)
	// The residual is the call time minus the hub-side time of the part calls: the handler, the
	// encode and the slicing in the SDK, the transport of each part, and the hub's join.
	t.Logf("view.render call ms (median of 7): 150 KiB in parts %.1f, 40 KiB inline %.1f. 150 KiB: part calls (hub side) median sum %.1f ms; residual (handler, encode, slice, transport, join) median %.1f ms", tb, ts, median(big, sum), median(big, residual))
	if tb-ts > 50 {
		t.Fatalf("parts add %.1f ms; the budget is 50 ms", tb-ts)
	}
	l.noRefusedParts()
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
