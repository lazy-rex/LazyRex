package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const logStamp = "2026/10/22 10:00:00 "

// The marker is the start of the message, after the time. A quoted marker, a longer name, another
// entry key of the same name, and a line without the time do not count.
func TestLogMessageHas(t *testing.T) {
	m := pluginLogMarker("x", "managed:x")
	for line, want := range map[string]bool{
		logStamp + "plugin x [managed:x]: exited: status 1":                 true,
		logStamp + "plugin y [managed:y]: saw plugin x [managed:x]: exited": false,
		logStamp + "plugin xx [managed:xx]: exited":                         false,
		logStamp + "plugin x [src/x]: exited":                               false,
		logStamp + "plugin x: exited":                                       false,
		"plugin x [managed:x]: exited":                                      false,
		"2026-10-22 10:00:00 plugin x [managed:x]: exited":                  false,
		logStamp + " plugin x [managed:x]: exited":                          false,
		logStamp[:19]: false,
	} {
		if got := logMessageHas(line, m); got != want {
			t.Errorf("%q: %v, want %v", line, got, want)
		}
	}
	for _, c := range [][3]string{{"x", "managed:x", "plugin x [managed:x]: "}, {"x", "", "plugin x: "}, {"", "src/a", "plugin [src/a]: "}} {
		if got := pluginLogMarker(c[0], c[1]); got != c[2] {
			t.Errorf("marker %q %q: %q", c[0], c[1], got)
		}
	}
}

// countingReader counts the bytes that the hub.log read takes from the file.
type countingReader struct {
	r io.Reader
	n *int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

func countHubLogReads(t *testing.T) *int64 {
	t.Helper()
	var n int64
	read := hubLogRead
	t.Cleanup(func() { hubLogRead = read })
	hubLogRead = func(r io.Reader) io.Reader { return countingReader{r, &n} }
	return &n
}

func markedLines(name, key string, from, n int) string {
	var b strings.Builder
	for i := from; i < from+n; i++ {
		fmt.Fprintf(&b, "%s%sline %d\n", logStamp, pluginLogMarker(name, key), i)
	}
	return b.String()
}

// A 3 GiB sparse hub.log: the read takes at most hubLogTail bytes, and gives the marked lines in
// the last 100 KiB.
func TestReadHubLogSparse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const size = 3 << 30
	tail := markedLines("x", "managed:x", 0, 3) + logStamp + "plugin y [managed:y]: other\n"
	if err := f.Truncate(size - 100<<10); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("\n"+tail+strings.Repeat("z", 100<<10-len(tail)-2)+"\n"), size-100<<10); err != nil {
		t.Fatal(err)
	}
	f.Close()
	n := countHubLogReads(t)
	p := readHubLog(path, pluginLogMarker("x", "managed:x"))
	if *n > hubLogTail {
		t.Fatalf("the read took %d bytes", *n)
	}
	want := strings.Split(strings.TrimSuffix(markedLines("x", "managed:x", 0, 3), "\n"), "\n")
	if !p.Available || p.Error != "" || !reflect.DeepEqual(p.Lines, want) || !p.Truncated {
		t.Fatalf("the part: %+v", p)
	}
}

// After a seek past 0, the first line can be partial: it is dropped, also when its rest looks like
// a marked line.
func TestReadHubLogPartialFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.log")
	fake := logStamp + "plugin x [managed:x]: not a line\n"
	last := markedLines("x", "managed:x", 1, 1)
	body := markedLines("x", "managed:x", 0, 1) + strings.Repeat("a", 100) + fake
	body += strings.Repeat("b", hubLogTail-len(fake)-len("\n")-len(last)) + "\n" + last
	// The read starts len(body)-hubLogTail bytes in: at the start of fake.
	if off := len(body) - hubLogTail; body[off:off+len(fake)] != fake {
		t.Fatalf("the cut is at %q", body[off:off+20])
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p := readHubLog(path, pluginLogMarker("x", "managed:x"))
	want := []string{logStamp + "plugin x [managed:x]: line 1"}
	if !reflect.DeepEqual(p.Lines, want) || !p.Truncated {
		t.Fatalf("the part: %+v", p)
	}
}

// A rotation between the Stat and the read keeps the opened file; a truncation gives a shorter
// read, with no error.
func TestReadHubLogRotateTruncate(t *testing.T) {
	marker := pluginLogMarker("x", "managed:x")
	for _, c := range []struct {
		name  string
		hook  func(path string) error
		lines int
	}{
		{"rotate", func(path string) error {
			if err := os.Rename(path, path+".1"); err != nil {
				return err
			}
			return os.WriteFile(path, []byte(markedLines("x", "managed:x", 9, 1)), 0o600)
		}, 2},
		{"truncate", func(path string) error { return os.Truncate(path, 0) }, 0},
	} {
		path := filepath.Join(t.TempDir(), "hub.log")
		if err := os.WriteFile(path, []byte(markedLines("x", "managed:x", 0, 2)), 0o600); err != nil {
			t.Fatal(err)
		}
		before := hubLogBeforeRead
		var hookErr error
		hubLogBeforeRead = func(p string) { hookErr = c.hook(p) }
		p := readHubLog(path, marker)
		hubLogBeforeRead = before
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if p.Error != "" || len(p.Lines) != c.lines || (c.lines > 0 && p.Lines[0] != logStamp+marker+"line 0") {
			t.Errorf("%s: %+v", c.name, p)
		}
	}
}

// Without the restart wrapper there is no hub.log; over hubLogLines lines, the last ones are kept.
func TestReadHubLogNoWrapperAndLines(t *testing.T) {
	if p := readHubLog("", "plugin x [managed:x]: "); p.Available || p.Reason != noHubLog || p.Lines == nil {
		t.Errorf("no wrapper: %+v", p)
	}
	path := filepath.Join(t.TempDir(), "hub.log")
	if err := os.WriteFile(path, []byte(markedLines("x", "managed:x", 0, hubLogLines+5)), 0o600); err != nil {
		t.Fatal(err)
	}
	p := readHubLog(path, pluginLogMarker("x", "managed:x"))
	if len(p.Lines) != hubLogLines || !p.Truncated || p.Lines[0] != logStamp+"plugin x [managed:x]: line 5" {
		t.Fatalf("over the line bound: %d lines, truncated %v, first %q", len(p.Lines), p.Truncated, p.Lines[0])
	}
	if p := readHubLog(filepath.Join(t.TempDir(), "none"), "m"); !p.Available || p.Error == "" {
		t.Errorf("a missing file: %+v", p)
	}
}

func TestTailText(t *testing.T) {
	for _, c := range []struct {
		in, out string
		cut     bool
	}{
		{"abc", "abc", false},
		{"ab\ncdef", "cdef", true},
		{"aé" + "bcd", "bcd", true}, // the cut is inside é: the text starts at the next character
	} {
		if out, cut := tailText(c.in, 4); out != c.out || cut != c.cut {
			t.Errorf("%q: %q %v", c.in, out, cut)
		}
	}
}

// logTestHub is opsTestHub with one managed entry x, its record with a block, a hub.log, and the
// load service. The Rex calls go to call.
func logTestHub(t *testing.T, call rexCall) *Hub {
	t.Helper()
	h, _, _ := opsTestHub(t, "")
	e := &configEntry{Source: "github:acme/x@v1.0.0", name: "x", managed: "x"}
	h.entriesMu.Lock()
	h.entries = append(h.entries, e)
	h.entriesMu.Unlock()
	h.mu.Lock()
	h.plugins["x"] = &Plugin{Name: "x", entry: e, InstanceID: "i1", SessionID: "session:1", BlockID: "block:1"}
	h.mu.Unlock()
	h.cfg.hubLog = filepath.Join(h.cfg.State, "hub.log")
	if err := os.WriteFile(h.cfg.hubLog, []byte(markedLines("x", "managed:x", 0, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); h.loads.wait() }) // no load notice outlives the test fakes
	h.loads.call = call
	h.loadsStart(ctx)
	return h
}

func askLog(t *testing.T, h *Hub, args map[string]any) map[string]any {
	t.Helper()
	v, err := h.pluginLog(rawJSON(args))
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func waitLogLoad(t *testing.T, h *Hub, key string) map[string]any {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		h.loads.mu.Lock()
		e := h.loads.logs.entries[key]
		running := e != nil && e.running
		h.loads.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the load did not end")
		}
	}
	h.loads.mu.Lock()
	r := h.loads.logs.entries[key].reply()
	h.loads.mu.Unlock()
	return map[string]any{"state": r.State, "value": r.Value}
}

// The ready reply has the block text of the record's block (the last blockKeep bytes) and the
// hub.log lines; the call is the terminal format method on the snapshot's block.
func TestPluginLogAction(t *testing.T) {
	var got map[string]any
	h := logTestHub(t, func(ctx context.Context, method string, payload any, out any) error {
		b, _ := json.Marshal(payload)
		json.Unmarshal(b, &got)
		got["method_name"] = method
		_, hasDeadline := ctx.Deadline()
		got["deadline"] = hasDeadline
		return json.Unmarshal(rawJSON(map[string]any{"content": strings.Repeat("o", 10) + "\n" + strings.Repeat("p", blockKeep)}), out)
	})
	if v := askLog(t, h, map[string]any{"entry": "managed:x"}); v["state"] != "loading" {
		t.Fatalf("the first request: %v", v)
	}
	waitLogLoad(t, h, "managed:x")
	v := askLog(t, h, map[string]any{"entry": "managed:x"})
	want := map[string]any{"method_name": "block.method", "session_id": "session:1", "block_id": "block:1", "method": "format",
		"args": map[string]any{"format": "text"}, "deadline": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the call: %v", got)
	}
	b := v["block"].(*logBlock)
	hl := v["hub_log"].(hubLogPart)
	if v["state"] != "ready" || b.Text != strings.Repeat("p", blockKeep) || !b.Truncated || b.Error != "" || b.InstanceID != "i1" ||
		len(hl.Lines) != 1 || !hl.Available || v["read_at"].(int64) == 0 {
		t.Fatalf("ready: %v %+v %+v", v, b, hl)
	}
	for _, args := range []map[string]any{{"entry": ""}, {"entry": "managed:x", "x": 1}} {
		if _, err := h.pluginLog(rawJSON(args)); !isRefusal(err, "invalid_input") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := h.pluginLog(rawJSON(map[string]any{"entry": "managed:nope"})); !isRefusal(err, "not_found") {
		t.Errorf("an unknown entry: %v", err)
	}
}

// Rex refuses a pane text over 1 MiB (too_large): the block part has the error, and the hub_log
// part is in the same reply. A record with no block gives block null.
func TestPluginLogTooLargeAndNoBlock(t *testing.T) {
	h := logTestHub(t, func(ctx context.Context, method string, payload any, out any) error {
		return fmt.Errorf("%s: %w", method, &rex.RexError{Code: "too_large", Message: "control response exceeds 1048576 bytes"})
	})
	askLog(t, h, map[string]any{"entry": "managed:x"})
	waitLogLoad(t, h, "managed:x")
	v := askLog(t, h, map[string]any{"entry": "managed:x"})
	b, hl := v["block"].(*logBlock), v["hub_log"].(hubLogPart)
	if v["state"] != "ready" || b.Error != "the pane text is over Rex's 1 MiB reply limit" || b.Text != "" || len(hl.Lines) != 1 {
		t.Fatalf("too_large: %v %+v %+v", v, b, hl)
	}
	h.mu.Lock()
	h.plugins["x"].SessionID, h.plugins["x"].BlockID = "", ""
	h.mu.Unlock()
	askLog(t, h, map[string]any{"entry": "managed:x", "refresh": true})
	waitLogLoad(t, h, "managed:x")
	if v := askLog(t, h, map[string]any{"entry": "managed:x"}); v["block"] != nil || len(v["hub_log"].(hubLogPart).Lines) != 1 {
		t.Fatalf("no block: %v", v)
	}
}

// A load whose record changes during the load (a new instance or block, or another entry) is
// stale: the entry goes back to idle with no value.
func TestPluginLogStale(t *testing.T) {
	var h *Hub
	h = logTestHub(t, func(ctx context.Context, method string, payload any, out any) error {
		h.mu.Lock()
		h.plugins["x"].InstanceID, h.plugins["x"].BlockID = "i2", "block:2"
		h.mu.Unlock()
		return json.Unmarshal([]byte(`{"content":"old"}`), out)
	})
	askLog(t, h, map[string]any{"entry": "managed:x"})
	if r := waitLogLoad(t, h, "managed:x"); r["state"] != "idle" || r["value"] != nil {
		t.Fatalf("a changed record: %v", r)
	}
	h.loads.call = func(ctx context.Context, method string, payload any, out any) error {
		h.entriesMu.Lock()
		h.entries = nil
		h.entriesMu.Unlock()
		return json.Unmarshal([]byte(`{"content":"old"}`), out)
	}
	askLog(t, h, map[string]any{"entry": "managed:x"})
	if r := waitLogLoad(t, h, "managed:x"); r["state"] != "idle" || r["value"] != nil {
		t.Fatalf("a removed entry: %v", r)
	}
}

// Only the home plugin's registered instance reads the tags and logs on the plugin face; the agent
// face does not offer them.
func TestPluginTagsLogFace(t *testing.T) {
	h := logTestHub(t, func(ctx context.Context, method string, payload any, out any) error {
		return json.Unmarshal([]byte(`{"content":""}`), out)
	})
	for _, a := range []string{"hub.plugins.tags", "hub.plugins.log"} {
		if !contains(hubActionNames, a) {
			t.Errorf("the hub does not declare %s", a)
		}
		for _, d := range pluginOpActions {
			if d["name"] == a {
				t.Errorf("the agent face offers %s", a)
			}
		}
	}
	h.mu.Lock()
	h.plugins["x"].ClientID = "client:x"
	h.mu.Unlock()
	for _, a := range []string{"hub.plugins.tags", "hub.plugins.log"} {
		p := perform{Action: a, Args: json.RawMessage(`{"entry":"managed:x","source":"github:acme/x@v1"}`)}
		p.Requester.ClientID = "client:x"
		_, err := h.dispatch(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), "only the home plugin reads the plugin tags and logs") {
			t.Errorf("%s from x: %v", a, err)
		}
	}
	h.cfg.Home = "x"
	p := perform{Action: "hub.plugins.log", Args: json.RawMessage(`{"entry":"managed:x"}`)}
	p.Requester.ClientID = "client:x"
	if _, err := h.dispatch(context.Background(), p); err != nil {
		t.Errorf("hub.plugins.log from the home plugin: %v", err)
	}
}

// A file that grows between the Stat and the read: the read still takes at most hubLogTail bytes.
func TestReadHubLogGrows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.log")
	if err := os.WriteFile(path, []byte(markedLines("x", "managed:x", 0, 2)), 0o600); err != nil {
		t.Fatal(err)
	}
	n := countHubLogReads(t)
	before := hubLogBeforeRead
	t.Cleanup(func() { hubLogBeforeRead = before })
	hubLogBeforeRead = func(p string) {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		f.Write([]byte(strings.Repeat("g", 2*hubLogTail) + "\n"))
		f.Close()
	}
	p := readHubLog(path, pluginLogMarker("x", "managed:x"))
	if *n > hubLogTail || len(p.Lines) != 2 {
		t.Fatalf("a growing file: the read took %d bytes, %d lines", *n, len(p.Lines))
	}
}

// recordLogf writes the marker of the record's current entry; a kept record (no entry) has no key,
// so its lines are in no entry's log.
func TestRecordLogf(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	var buf strings.Builder
	w, flags := log.Writer(), log.Flags()
	t.Cleanup(func() { log.SetOutput(w); log.SetFlags(flags) })
	log.SetOutput(&buf)
	log.SetFlags(log.LstdFlags)
	h.recordLogf(&Plugin{Name: "x", entry: &configEntry{Source: "src/x", name: "x"}}, "wake: %v", "e1")
	h.recordLogf(&Plugin{Name: "x"}, "reload: %s", "e2")
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 || !logMessageHas(lines[0], "plugin x [src/x]: wake: e1") || !logMessageHas(lines[1], "plugin x: reload: e2") {
		t.Fatalf("the lines: %q", lines)
	}
}

// A start error that starts with "plugin <same name>: " loses that inner prefix once (the hub
// lead, lead-c7code); a prefix of another name, and the same text later in the message, stay.
func TestPluginLogfInnerPrefix(t *testing.T) {
	var buf strings.Builder
	w, flags := log.Writer(), log.Flags()
	t.Cleanup(func() { log.SetOutput(w); log.SetFlags(flags) })
	log.SetOutput(&buf)
	log.SetFlags(0)
	pluginLogf("x", "managed:x", "%v", "plugin x: plugin x: dependency cycle a -> b -> a")
	pluginLogf("x", "managed:x", "%v", "plugin y: dependency cycle")
	pluginLogf("x", "managed:x", "respawn: %v", "plugin x: no run")
	want := "plugin x [managed:x]: plugin x: dependency cycle a -> b -> a\n" +
		"plugin x [managed:x]: plugin y: dependency cycle\n" +
		"plugin x [managed:x]: respawn: plugin x: no run\n"
	if buf.String() != want {
		t.Fatalf("the lines:\n%s", buf.String())
	}
}

// blockTextCall answers the format method with the text "pane of <block_id>", so a reply shows
// which block it read.
func blockTextCall(ctx context.Context, method string, payload any, out any) error {
	block, _ := payload.(map[string]any)["block_id"].(string)
	return json.Unmarshal(rawJSON(map[string]any{"content": "pane of " + block}), out)
}

// changeLogRecord changes the entry managed:x or its record, as a restart, a source change, a block
// change or an entry replacement under the same key does.
func changeLogRecord(h *Hub, mode string) {
	h.entriesMu.Lock()
	defer h.entriesMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.plugins["x"]
	switch mode {
	case "restart":
		p.InstanceID, p.BlockID = "i2", "block:2"
	case "source":
		p.entry.Source = "github:acme/y@v2.0.0"
	case "block":
		p.BlockID = "block:2"
	case "entry_replaced":
		e := &configEntry{Source: "github:acme/y@v2.0.0", name: "x", managed: "x"}
		h.entries[len(h.entries)-1], p.entry = e, e
	}
}

// The block part of a cached log belongs to the record that its load read (codex-27, plugins-c7-
// cache-blocker; the hub lead, lead-c7cache). After a change of the instance, the block, the source
// or the managed entry (the last two keep the instance and the block), a read and a refresh start a
// load at once and do not show the old block: state loading, block null. The hub_log part may stay.
// The new load reads the current block.
func TestPluginLogCacheFollowsRecord(t *testing.T) {
	for _, mode := range []string{"restart", "source", "block", "entry_replaced"} {
		for _, refresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s refresh=%v", mode, refresh), func(t *testing.T) {
				h := logTestHub(t, blockTextCall)
				askLog(t, h, map[string]any{"entry": "managed:x"})
				waitLogLoad(t, h, "managed:x")
				changeLogRecord(h, mode)
				v := askLog(t, h, map[string]any{"entry": "managed:x", "refresh": refresh})
				if v["state"] != "loading" || v["block"] != nil {
					t.Fatalf("after the change: %v", v)
				}
				waitLogLoad(t, h, "managed:x")
				v = askLog(t, h, map[string]any{"entry": "managed:x"})
				want, instance := "pane of block:1", "i1"
				switch mode {
				case "restart":
					want, instance = "pane of block:2", "i2"
				case "block":
					want = "pane of block:2"
				}
				if b, _ := v["block"].(*logBlock); v["state"] != "ready" || b == nil || b.Text != want || b.InstanceID != instance {
					t.Fatalf("the new load: %v %+v", v, b)
				}
			})
		}
	}
}

// A change while a load runs: a request does not get the old value, the running load's result is
// discarded at its end, and the next request starts one new load (one load per key).
func TestPluginLogCacheChangeDuringLoad(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	h := logTestHub(t, func(ctx context.Context, method string, payload any, out any) error {
		if calls.Add(1) == 1 {
			<-release
		}
		return blockTextCall(ctx, method, payload, out)
	})
	askLog(t, h, map[string]any{"entry": "managed:x"})
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	changeLogRecord(h, "restart")
	if v := askLog(t, h, map[string]any{"entry": "managed:x", "refresh": true}); v["state"] != "loading" || v["block"] != nil {
		t.Fatalf("during the load: %v", v)
	}
	close(release)
	if r := waitLogLoad(t, h, "managed:x"); r["state"] != "idle" || r["value"] != nil {
		t.Fatalf("the old load's end: %v", r)
	}
	askLog(t, h, map[string]any{"entry": "managed:x"})
	waitLogLoad(t, h, "managed:x")
	v := askLog(t, h, map[string]any{"entry": "managed:x"})
	if b, _ := v["block"].(*logBlock); b == nil || b.Text != "pane of block:2" || calls.Load() != 2 {
		t.Fatalf("the new load: %v %+v, %d calls", v, b, calls.Load())
	}
}

// A failed load for the new instance keeps no block of the old one: state failed, block null.
func TestPluginLogFailedLoadAfterChange(t *testing.T) {
	var h *Hub
	h = logTestHub(t, blockTextCall)
	askLog(t, h, map[string]any{"entry": "managed:x"})
	waitLogLoad(t, h, "managed:x")
	changeLogRecord(h, "restart")
	h.loads.mu.Lock()
	h.loads.logs.run = func(ctx context.Context, key string) (any, bool, error) {
		return nil, true, errors.New("the load failed")
	}
	h.loads.mu.Unlock()
	askLog(t, h, map[string]any{"entry": "managed:x"})
	waitLogLoad(t, h, "managed:x")
	v := askLog(t, h, map[string]any{"entry": "managed:x"})
	if v["block"] != nil || v["error"] != "the load failed" {
		t.Fatalf("a failed load of i2: %v", v)
	}
	if v["state"] != "loading" && v["state"] != "failed" {
		t.Fatalf("a failed load of i2: state %v", v["state"])
	}
}
