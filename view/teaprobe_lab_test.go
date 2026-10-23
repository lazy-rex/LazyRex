package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// teaLab runs testdata/teaprobe in a pane of its own lab server, for the thread-view facts
// (docs/rex-protocol.md). The tests record what they see: each step keeps the raw captures (text,
// text --unwrap, vt) in <lab>/captures and logs a summary. They fail only when the lab fails.
// Lab only: TEAPROBE_LAB=1. The lab is kept.
type teaLab struct {
	t       *testing.T
	lab     string
	socket  string
	session string
	pane    string
	owner   *wire
	ctx     context.Context
	step    int
}

func newTeaLab(t *testing.T, cols, rows int, args ...string) *teaLab {
	if os.Getenv("TEAPROBE_LAB") != "1" {
		t.Skip("set TEAPROBE_LAB=1 to run the teaprobe lab (it starts its own lab server)")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	lab, err := os.MkdirTemp("/tmp", "rexlab-teaprobe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("lab: %s", lab)
	l := &teaLab{t: t, lab: lab, socket: "unix://" + lab + "/rex.sock"}
	os.MkdirAll(filepath.Join(lab, "captures"), 0o755)
	bin := filepath.Join(lab, "bin", "teaprobe")
	if b, err := exec.Command("go", "build", "-o", bin, "./testdata/teaprobe").CombinedOutput(); err != nil {
		t.Fatalf("build teaprobe: %v\n%s", err, b)
	}
	server := exec.Command(filepath.Join(root, "lab", "server.sh"), lab)
	serverLog, _ := os.Create(filepath.Join(lab, "server.log"))
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Process.Signal(syscall.SIGTERM); server.Wait(); serverLog.Close() })
	waitUntil(t, "the lab server", 10*time.Second, func() bool { _, err := l.cli("ls"); return err == nil })
	l.note("rex: %s", strings.TrimSpace(strings.SplitN(l.must("version"), "\n", 2)[0]))
	if b, err := exec.Command("go", "version").Output(); err == nil {
		l.note("go: %s", strings.TrimSpace(string(b)))
	}
	if b, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
		l.note("macOS: %s", strings.TrimSpace(string(b)))
	}
	l.note("teaprobe args: %v", args)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	l.ctx = ctx
	// Rex applies a size from the block's owner only: this connection takes the pane, as an app does.
	owner, err := dialWire(ctx, lab+"/rex.sock", func(json.RawMessage) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.conn.CloseNow() })
	l.owner = owner
	// The shell keeps the pane after teaprobe exits, so a capture after q shows what stays.
	command, _ := json.Marshal(append([]string{"/bin/sh", "-c", `"$0" "$@"; echo "EXITED $?"; exec sleep 600`, bin}, args...))
	var s struct {
		ID string `json:"session_id"`
	}
	if err := owner.call(ctx, "session.create", json.RawMessage(`{"label":"teaprobe","initial_windows":[{"window_label":"w","layout":{"block":{"flavor":"com.superlogical.terminal.shell","options":{"command":`+string(command)+`,"login":false}}}}]}`), &s); err != nil {
		t.Fatal(err)
	}
	l.session = s.ID
	if err := owner.call(ctx, "session.attach", map[string]any{"session_id": s.ID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	var blocks struct {
		Blocks []struct {
			ID string `json:"block_id"`
		} `json:"blocks"`
	}
	if err := owner.call(ctx, "session.list_blocks", map[string]any{"session_id": s.ID}, &blocks); err != nil || len(blocks.Blocks) != 1 {
		t.Fatalf("the pane: %v %+v", err, blocks)
	}
	l.pane = blocks.Blocks[0].ID
	if err := owner.call(ctx, "session.own_block", map[string]any{"session_id": s.ID, "block_ids": []string{l.pane}, "mode": "take"}, nil); err != nil {
		t.Fatal(err)
	}
	l.resize(cols, rows)
	return l
}

func waitUntil(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); ; time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("timed out: %s", what)
		}
	}
}

func (l *teaLab) cli(args ...string) (string, error) {
	out, err := exec.Command("rex", append([]string{"-S", l.socket, "--autostart=false", "--timeout", "10s"}, args...)...).CombinedOutput()
	return string(out), err
}

func (l *teaLab) must(args ...string) string {
	l.t.Helper()
	out, err := l.cli(args...)
	if err != nil {
		l.t.Fatalf("rex %v: %v\n%s", args, err, out)
	}
	return out
}

// note logs a line and appends it to <lab>/notes.txt.
func (l *teaLab) note(format string, a ...any) {
	l.t.Helper()
	s := fmt.Sprintf(format, a...)
	l.t.Log(s)
	f, _ := os.OpenFile(filepath.Join(l.lab, "notes.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprintln(f, s)
	f.Close()
}

// resize asks for a grid size from the owner and records the size that the block reports.
func (l *teaLab) resize(cols, rows int) {
	l.t.Helper()
	if err := l.owner.call(l.ctx, "block.method", map[string]any{"session_id": l.session, "block_id": l.pane, "method": "resize", "args": map[string]int{"columns": cols, "rows": rows}}, nil); err != nil {
		l.t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	var size json.RawMessage
	if err := l.owner.call(l.ctx, "block.method", map[string]any{"session_id": l.session, "block_id": l.pane, "method": "size", "args": map[string]any{}}, &size); err != nil {
		l.t.Fatalf("the size after a resize to %dx%d: %v", cols, rows, err)
	}
	var got struct{ Columns, Rows int }
	if err := json.Unmarshal(size, &got); err != nil || got.Columns == 0 {
		l.t.Fatalf("the size after a resize to %dx%d: %v %s", cols, rows, err, size)
	}
	l.note("resize asked %dx%d; size reports %dx%d: %s", cols, rows, got.Columns, got.Rows, size)
}

func (l *teaLab) screen() string {
	out, _ := l.cli("block", "capture", "-s", l.session, "-b", l.pane)
	return out
}

func (l *teaLab) waitScreen(what string, ok func(string) bool) {
	l.t.Helper()
	waitUntil(l.t, what, 10*time.Second, func() bool { return ok(l.screen()) })
	time.Sleep(300 * time.Millisecond) // the renderer finishes its frame
}

func (l *teaLab) keys(keys ...string) {
	l.t.Helper()
	l.must(append([]string{"block", "send-key", "-s", l.session, "-b", l.pane}, keys...)...)
}

var probeLine = regexp.MustCompile(`L(\d\d)\|`)

// capture keeps the three raw captures of one step and notes a summary of the text --unwrap form:
// each printed line number with its count, and the lines whose end marker |E is missing. Each
// marker in want must be in the capture, or the step fails and records nothing: a summary of a
// wrong screen would be a wrong fact.
func (l *teaLab) capture(name string, want ...string) string {
	l.t.Helper()
	l.step++
	base := filepath.Join(l.lab, "captures", fmt.Sprintf("%02d-%s", l.step, name))
	forms := map[string][]string{".txt": nil, ".unwrap.txt": {"--unwrap"}, ".vt": {"--format=vt"}}
	var unwrapped string
	for ext, extra := range forms {
		out := l.must(append([]string{"block", "capture", "-s", l.session, "-b", l.pane}, extra...)...)
		os.WriteFile(base+ext, []byte(out), 0o600)
		if ext == ".unwrap.txt" {
			unwrapped = out
		}
	}
	for _, w := range want {
		if !strings.Contains(unwrapped, w) {
			l.t.Fatalf("step %02d %s: the capture has no %q (raw captures: %s.*)", l.step, name, w, base)
		}
	}
	counts := map[string]int{}
	var cut, seq []string
	for _, row := range strings.Split(unwrapped, "\n") {
		for _, m := range probeLine.FindAllStringSubmatch(row, -1) {
			counts[m[1]]++
			seq = append(seq, m[1])
		}
		if m := probeLine.FindStringSubmatch(row); m != nil && !strings.Contains(row, "|E") {
			cut = append(cut, m[1])
		}
	}
	var nums []string
	for n := range counts {
		nums = append(nums, n)
	}
	sort.Strings(nums)
	var twice []string
	for _, n := range nums {
		if counts[n] > 1 {
			twice = append(twice, fmt.Sprintf("%s×%d", n, counts[n]))
		}
	}
	rows := strings.Split(strings.TrimRight(unwrapped, "\n"), "\n")
	first, last := "", ""
	if len(nums) > 0 {
		first, last = nums[0], nums[len(nums)-1]
	}
	// In order: the numbers in capture order are exactly 01, 02, ... with no gap and no repeat.
	inOrder := true
	for i, n := range seq {
		if n != fmt.Sprintf("%02d", i+1) {
			inOrder = false
		}
	}
	l.note("step %02d %s: %d captured rows (unwrapped); printed lines %d distinct (first %s, last %s); exactly 01..%02d in capture order %v; more than once %v; without |E %v; Connecting row %v; LIVE rows %d",
		l.step, name, len(rows), len(nums), first, last, len(seq), inOrder, twice, cut, strings.Contains(unwrapped, "Connecting"), strings.Count(unwrapped, "LIVE "))
	return unwrapped
}

func printedCount(n int) func(string) bool {
	return func(s string) bool { return strings.Contains(s, fmt.Sprintf("LIVE p=%d |", n)) }
}

// H1: a thread chooses inline after a delayed first tree, then prints its history.
func TestTeaProbeH1ThreadInline(t *testing.T) {
	l := newTeaLab(t, 80, 24, "tea", "-delay", "2s", "-mode", "inline")
	l.waitScreen("Connecting", func(s string) bool { return strings.Contains(s, "Connecting") })
	l.capture("connecting", "| Connecting")
	l.waitScreen("the first tree", func(s string) bool { return strings.Contains(s, "| tree alt=false") })
	l.capture("first-tree", "| tree alt=false")
	l.keys("m")
	l.waitScreen("30 printed", printedCount(30))
	l.capture("history-30", "LIVE p=30 |")
	l.keys("p")
	l.waitScreen("31 printed", printedCount(31))
	l.capture("one-more", "LIVE p=31 |")
}

// H2: an ordinary view chooses the alt screen after a delayed first tree.
func TestTeaProbeH2OrdinaryAlt(t *testing.T) {
	l := newTeaLab(t, 80, 24, "tea", "-delay", "2s", "-mode", "alt")
	l.waitScreen("Connecting", func(s string) bool { return strings.Contains(s, "Connecting") })
	l.capture("connecting", "| Connecting")
	l.waitScreen("the first tree", func(s string) bool { return strings.Contains(s, "alt=true") })
	l.capture("first-tree-alt", "| tree alt=true")
	l.keys("q")
	l.waitScreen("the exit", func(s string) bool { return strings.Contains(s, "EXITED") })
	l.capture("after-quit", "EXITED 0")
}

// H3 (a separate fact): history inline, then alt screen on and off, then one more line.
func TestTeaProbeH3RoundTrip(t *testing.T) {
	l := newTeaLab(t, 80, 24, "tea", "-delay", "500ms", "-mode", "inline")
	l.waitScreen("the first tree", func(s string) bool { return strings.Contains(s, "| tree alt=false") })
	l.keys("m")
	l.waitScreen("30 printed", printedCount(30))
	l.capture("history-30", "LIVE p=30 |")
	l.keys("a")
	l.waitScreen("alt on", func(s string) bool { return strings.Contains(s, "alt=true") })
	l.capture("alt-on", "alt=true")
	l.keys("a")
	l.waitScreen("alt off", func(s string) bool { return strings.Contains(s, "alt=false") })
	l.capture("alt-off", "LIVE p=30 |")
	l.keys("p")
	l.waitScreen("31 printed", printedCount(31))
	l.capture("one-more", "LIVE p=31 |")
}

// A2: the printed lines at a width change, narrow to wide and wide to narrow.
func TestTeaProbeA2Width(t *testing.T) {
	for _, widths := range [][]int{{80, 50, 110}, {110, 50, 80}} {
		t.Run(fmt.Sprint(widths), func(t *testing.T) {
			l := newTeaLab(t, widths[0], 24, "tea", "-delay", "500ms", "-mode", "inline")
			l.waitScreen("the first tree", func(s string) bool { return strings.Contains(s, "| tree alt=false") })
			l.keys("m")
			l.waitScreen("30 printed", printedCount(30))
			l.capture(fmt.Sprintf("at-%d", widths[0]), fmt.Sprintf(" %dx24 ", widths[0]), "LIVE p=30 |")
			for _, w := range widths[1:] {
				l.resize(w, 24)
				l.waitScreen(fmt.Sprintf("the live region at %d", w), func(s string) bool { return strings.Contains(s, fmt.Sprintf(" %dx24 ", w)) })
				l.capture(fmt.Sprintf("at-%d", w), fmt.Sprintf(" %dx24 ", w), "LIVE p=30 |")
			}
			l.keys("p")
			l.waitScreen("31 printed", printedCount(31))
			l.capture("one-more", "LIVE p=31 |")
		})
	}
}

// A3: which heights a resize from the owner applies.
func TestTeaProbeA3Height(t *testing.T) {
	l := newTeaLab(t, 80, 24, "tea", "-delay", "500ms", "-mode", "inline")
	l.waitScreen("the first tree", func(s string) bool { return strings.Contains(s, "| tree alt=false") })
	for _, rows := range []int{20, 60, 200} {
		l.resize(80, rows)
		geometry := fmt.Sprintf(" 80x%d ", rows)
		l.waitScreen("the live geometry"+geometry, func(s string) bool { return strings.Contains(s, geometry) })
		l.capture(fmt.Sprintf("rows-%d", rows), geometry)
	}
}

// The -bytes tee on Rex (the Mac check of the B2 tool, not an iOS fact): the log has the bytes of
// each key before the parser, and the live region has the key message.
func TestTeaProbeBytesTee(t *testing.T) {
	log := filepath.Join(os.TempDir(), fmt.Sprintf("teaprobe-bytes-%d.log", time.Now().UnixNano()))
	l := newTeaLab(t, 80, 24, "tea", "-delay", "500ms", "-mode", "inline", "-bytes", log)
	l.waitScreen("the first tree", func(s string) bool { return strings.Contains(s, "| tree alt=false") })
	for _, k := range []string{"x", "enter", "shift+enter"} {
		l.keys(k)
		time.Sleep(500 * time.Millisecond)
	}
	b, err := os.ReadFile(log)
	if err != nil || len(b) == 0 {
		t.Fatalf("no input bytes in %s: %v", log, err)
	}
	os.WriteFile(filepath.Join(l.lab, "captures", "input-bytes.log"), b, 0o600)
	os.Remove(log)
	// Raw: the bytes of each key, each in a read of its own after the start replies.
	var reads []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if _, hex, ok := strings.Cut(line, " "); ok {
			reads = append(reads, hex)
		}
	}
	for _, want := range []string{"78", "0d", "1b 5b 31 33 3b 32 75"} {
		if !slices.Contains(reads, want) {
			t.Fatalf("the input log has no read %q (x, enter, shift+enter):\n%s", want, b)
		}
	}
	l.note("raw input reads (x, enter, shift+enter by rex send-key):\n%s", b)
	// Parsed: the key message of each key, from the live region.
	l.capture("keys", `key "x" code=120 mod=0`, `key "enter" code=13 mod=0`, `key "shift+enter" code=13 mod=1`)
}
