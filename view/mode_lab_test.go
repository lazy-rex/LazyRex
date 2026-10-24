package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newModeLab is a chunk lab (lab server, hub -test, real renderer panes) with the plugin
// testdata/modelab. Lab only: LAZYREX_LAB=1.
func newModeLab(t *testing.T) *chunkLab {
	needLab(t)
	l := newChunkLab(t, "modelab", "view/testdata/modelab")
	b, _ := json.Marshal(map[string]any{"plugin": "modelab", "sql": "SELECT count(*) FROM clicked", "params": []any{}})
	f := filepath.Join(l.lab, "tables.json")
	os.WriteFile(f, b, 0o600)
	l.waitFor("the plugin's tables", 30*time.Second, func() bool {
		_, err := l.cli("-C", "lazyrex-hub", "do", "hub.test.sql", "--args", "@"+f)
		return err == nil
	})
	l.sql("INSERT INTO stage VALUES (0)", true)
	return l
}

// resize sizes a taken pane.
func (l *chunkLab) resize(pane string, columns, rows int) {
	l.t.Helper()
	if err := l.owner.call(l.ctx, "block.method", map[string]any{"session_id": l.session, "block_id": pane, "method": "resize", "args": map[string]int{"columns": columns, "rows": rows}}, nil); err != nil {
		l.t.Fatal(err)
	}
}

func (l *chunkLab) key(pane, key string) {
	l.t.Helper()
	l.must("send-key", "-s", l.session, "-b", pane, key)
}

// TestFocusWindowLab (lead-tv-c2rules, B9): a real 10-row pane in thread mode gets an error tree
// with 12 actions. Tab and Shift+Tab reach every action, the focused one is on screen with the
// more-above and more-below rows, and Enter on the last action runs it.
func TestFocusWindowLab(t *testing.T) {
	l := newModeLab(t)
	pane := l.open("lazyrex_modelab_mode")
	l.resize(pane, 80, 10)
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last screen:\n%s", last)
		}
	})
	l.waitFor("the thread frame", 30*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "LAB-NOTICE")
	})
	l.sql("UPDATE stage SET n = 1", true)
	label := func(i int) string { return fmt.Sprintf("Action w%02d", i) }
	focused := func(i int, step string) {
		t.Helper()
		l.waitFor(step+": "+label(i)+" on screen", 10*time.Second, func() bool {
			last = l.screen(pane)
			return strings.Contains(last, label(i))
		})
	}
	focused(1, "the error tree")
	if !strings.Contains(last, "more below") || strings.Contains(last, "more above") {
		t.Fatalf("the first window has no more-below row, or has a more-above row:\n%s", last)
	}
	for i := 2; i <= 12; i++ {
		l.key(pane, "tab")
		focused(i, fmt.Sprintf("Tab %d", i-1))
	}
	if !strings.Contains(last, "more above") || strings.Contains(last, "more below") {
		t.Fatalf("the last window has no more-above row, or has a more-below row:\n%s", last)
	}
	t.Logf("the last control focused:\n%s", last)
	for i := 11; i >= 1; i-- {
		l.key(pane, "shift+tab")
		focused(i, fmt.Sprintf("Shift+Tab to %d", i))
	}
	for i := 2; i <= 12; i++ {
		l.key(pane, "tab")
		focused(i, fmt.Sprintf("Tab again %d", i-1))
	}
	l.key(pane, "enter")
	l.waitFor("the last action runs", 20*time.Second, func() bool {
		rows := l.sql("SELECT name FROM clicked", false)
		return len(rows) == 1 && rows[0][0] == "do-w12"
	})
}

// TestShortPaneLab (v1.2 item 1f): a real 6-row pane in thread mode gets a tree with a history
// node, 2 actions and a composer with a draft. The focused control is in the last 5 rows (the
// frame ends in the status line). Tab and Shift+Tab reach every control, and the editor row and
// Send are on screen together, at 80 and at 40 columns. A
// resize from 30 to 6 rows keeps the focus at both widths; at 40 columns, Tab then Enter runs Send. The unit tests count the frame rows: block capture also returns the scrollback, and a
// shrink moves the top rows of the tall frame there (a 6-row pane captured 25 lines, Linux lab
// 2026-10-23).
func TestShortPaneLab(t *testing.T) {
	l := newModeLab(t)
	pane := l.open("lazyrex_modelab_mode")
	var last string
	// bottom returns the last n rows of the screen, without the blank rows below the frame.
	bottom := func(n int) string {
		out, _ := l.cli("block", "capture", "-s", l.session, "-b", pane, "--trim")
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		return strings.Join(lines[max(0, len(lines)-n):], "\n")
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last screen:\n%s", last)
		}
	})
	l.waitFor("the thread frame", 30*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "LAB-NOTICE")
	})
	l.sql("UPDATE stage SET n = 2", true)
	l.waitFor("the composer tree", 10*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "DRAFT-TEXT") && strings.Contains(last, "Action w2")
	})
	l.resize(pane, 80, 6)
	// The renderer focuses the composer's editor first (model.apply).
	steps := []struct{ key, want string }{
		{"", "editor"}, {"tab", "send"}, {"tab", "Action w1"}, {"tab", "Action w2"}, {"tab", "editor"},
		{"shift+tab", "Action w2"}, {"shift+tab", "Action w1"}, {"shift+tab", "send"}, {"shift+tab", "editor"},
	}
	frame := func(step, want string) {
		t.Helper()
		l.waitFor(step+": "+want+" on screen", 10*time.Second, func() bool {
			last = bottom(5)
			if !strings.HasSuffix(strings.TrimSpace(last), "Ctrl+Q quit") {
				return false // the frame ends in the status line
			}
			if want == "editor" || want == "send" {
				return strings.Contains(last, "DRAFT-TEXT") && strings.Contains(last, "Send") && !strings.Contains(last, "Action w")
			}
			return strings.Contains(last, want) && !strings.Contains(last, "DRAFT-TEXT")
		})
	}
	run := func(width int, steps []struct{ key, want string }) {
		t.Helper()
		for i, s := range steps {
			if s.key != "" {
				l.key(pane, s.key)
			}
			frame(fmt.Sprintf("%d columns: step %d %s", width, i, s.key), s.want)
		}
	}
	shrink := func(width int) {
		t.Helper()
		l.resize(pane, width, 30)
		l.waitFor(fmt.Sprintf("%d columns: the tall frame", width), 10*time.Second, func() bool {
			last = bottom(29)
			return strings.Contains(last, "DRAFT-TEXT") && strings.Contains(last, "Action w1")
		})
		l.resize(pane, width, 6)
		frame(fmt.Sprintf("%d columns: resize from 30 to 6 rows", width), "editor")
	}
	run(80, steps)
	shrink(80)
	l.resize(pane, 40, 6)
	run(40, []struct{ key, want string }{
		{"", "editor"}, {"tab", "send"}, {"tab", "Action w1"}, {"shift+tab", "send"}, {"shift+tab", "editor"},
	})
	shrink(40)
	l.key(pane, "tab")
	l.key(pane, "enter")
	l.waitFor("Send runs", 20*time.Second, func() bool {
		rows := l.sql("SELECT name FROM clicked", false)
		return len(rows) == 1 && rows[0][0] == "send"
	})
}

// TestPrintLab (v1.2 item 3): a real pane in thread mode prints its history above the live region.
// It prints 3 messages once each, in order; then a new message from an SQL write; then, after a
// restart of the hub process (the renderer reattaches), only a new message, because the ledger of
// printed IDs belongs to the process. Each check reads the whole capture (with the scrollback).
func TestPrintLab(t *testing.T) {
	l := newModeLab(t)
	insert := func(i int) {
		t.Helper()
		l.sql(fmt.Sprintf("INSERT INTO msgs VALUES ('p%d', 'codex', 'PRINT-%d')", i, i), true)
	}
	for i := 1; i <= 3; i++ {
		insert(i)
	}
	l.sql("UPDATE stage SET n = 3", true)
	pane := l.open("lazyrex_modelab_mode")
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last screen:\n%s", last)
		}
	})
	printed := func(step string, n int) {
		t.Helper()
		newest := fmt.Sprintf("PRINT-%d", n)
		// A print writes at once (Bubble Tea's insertAbove); the frame ticker draws the live region
		// later. So the wait needs both before the whole-capture checks.
		l.waitFor(step+": "+newest+" printed and the live region drawn", 30*time.Second, func() bool {
			last = l.screen(pane)
			return strings.Contains(last, newest) && strings.Contains(last, "LAB-NOTICE")
		})
		// A second copy or a late mark would come in the same print as the newest message or before it.
		at := -1
		for i := 1; i <= n; i++ {
			text := fmt.Sprintf("PRINT-%d", i)
			if c := strings.Count(last, text); c != 1 {
				t.Fatalf("%s: %s is on the pane %d times:\n%s", step, text, c, last)
			}
			j := strings.Index(last, text)
			if j < at {
				t.Fatalf("%s: %s is above the message before it:\n%s", step, text, last)
			}
			at = j
		}
		if strings.Contains(last, "(late)") {
			t.Fatalf("%s: a message is marked late:\n%s", step, last)
		}
		if c := strings.Count(last, "LAB-NOTICE"); c != 1 {
			t.Fatalf("%s: the live region is on the pane %d times:\n%s", step, c, last)
		}
	}
	printed("the first tree", 3)
	insert(4)
	printed("an SQL insert", 4)
	t.Logf("before the hub restart:\n%s", last)
	old := l.hubClient()
	l.stopHub()
	l.startHub()
	// The new hub is a new client; its plugin registers after the hub connects.
	b, _ := json.Marshal(map[string]any{"plugin": "modelab", "sql": "SELECT count(*) FROM msgs", "params": []any{}})
	f := filepath.Join(l.lab, "restarted.json")
	os.WriteFile(f, b, 0o600)
	l.waitFor("the restarted hub", 30*time.Second, func() bool {
		hub := l.hubClient()
		if hub == "" || hub == old {
			return false
		}
		_, err := l.cli("-C", hub, "do", "hub.test.sql", "--args", "@"+f)
		return err == nil
	})
	insert(5)
	printed("an insert after the reattach", 5)
	t.Logf("after the reattach:\n%s", last)
}

// tabPane waits for the plugin's open, attached tab view on route, takes its pane and sizes it.
// It returns the pane's session and block.
func (l *chunkLab) tabPane(route string, columns, rows int) (session, pane string) {
	l.t.Helper()
	l.waitFor("the tab view "+route, 30*time.Second, func() bool {
		var r struct {
			Views []struct {
				Route    string `json:"route"`
				Attached bool   `json:"attached"`
				Session  string `json:"session_id"`
				Block    string `json:"block_id"`
			} `json:"views"`
		}
		l.hubTest("hub.test.views", map[string]any{}, &r)
		for _, v := range r.Views {
			if v.Route == route && v.Attached && v.Block != "" {
				session, pane = v.Session, v.Block
			}
		}
		return pane != ""
	})
	if err := l.owner.call(l.ctx, "session.attach", map[string]any{"session_id": session, "event_scope": "session"}, nil); err != nil {
		l.t.Fatal(err)
	}
	if err := l.owner.call(l.ctx, "session.own_block", map[string]any{"session_id": session, "block_ids": []string{pane}, "mode": "take"}, nil); err != nil {
		l.t.Fatal(err)
	}
	if err := l.owner.call(l.ctx, "block.method", map[string]any{"session_id": session, "block_id": pane, "method": "resize", "args": map[string]int{"columns": columns, "rows": rows}}, nil); err != nil {
		l.t.Fatal(err)
	}
	return session, pane
}

// TestReloadLeavesNoLiveRowsLab (v1.2, C11): a thread tab reloads its renderer (hub.ui.reload;
// the process and the pane stay). The old renderer's last frame is empty, so after the new
// renderer draws, the pane shows the live region once. The new process clears the pane before
// Bubble Tea starts (lead-tv-clear2), so its print of the history is the only copy: LAB-MSG is in
// the whole capture once, also after later frame flushes.
func TestReloadLeavesNoLiveRowsLab(t *testing.T) {
	l := newModeLab(t)
	session, pane := l.tabPane("/thread/lab", 80, 20)
	screen := func() string {
		out, _ := l.cli("block", "capture", "-s", session, "-b", pane)
		return out
	}
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last screen:\n%s", last)
		}
	})
	l.waitFor("the thread frame", 30*time.Second, func() bool {
		last = screen()
		return strings.Count(last, "LAB-NOTICE") == 1
	})
	events := filepath.Join(l.lab, "state-hub", "events.jsonl")
	reloads := func() int { return strings.Count(string(must(os.ReadFile(events))), `"event":"view_reload"`) }
	before := reloads()
	b, _ := json.Marshal(map[string]any{"plugin": "modelab"})
	f := filepath.Join(l.lab, "reload.json")
	os.WriteFile(f, b, 0o600)
	out := l.must("-C", "lazyrex-hub", "do", "hub.ui.reload", "--args", "@"+f)
	t.Logf("hub.ui.reload: %s", strings.TrimSpace(out))
	l.waitFor("the hub's reload request", 10*time.Second, func() bool { return reloads() > before })
	// The new renderer draws "Connecting…" first, then the tree.
	time.Sleep(2 * time.Second)
	l.waitFor("the new renderer's thread frame", 30*time.Second, func() bool {
		last = screen()
		return strings.Contains(last, "LAB-NOTICE") && !strings.Contains(last, "Connecting")
	})
	once := func(step string) {
		t.Helper()
		for _, text := range []string{"LAB-NOTICE", "LAB-MSG"} {
			if n := strings.Count(last, text); n != 1 {
				t.Fatalf("%s: %s is on the pane %d times:\n%s", step, text, n, last)
			}
		}
	}
	once("after the reload")
	t.Logf("after the reload:\n%s", last)
	time.Sleep(2 * time.Second)
	last = screen()
	once("2 s after the reload")
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestOpenWindowLab (v1.2 item 2, lead-tv-excl): the plugin holds 520 messages and its tree holds
// the newest 40, with has_more and total; view.history serves pages of 40. A real pane in thread
// mode fetches the pages back and prints the count line once, first, then PR-0021..PR-0520 once
// each, in order. PR-0001..PR-0020 are not printed. The check reads the whole capture (with the
// scrollback).
func TestOpenWindowLab(t *testing.T) {
	l := newModeLab(t)
	var values []string
	for i := 1; i <= 520; i++ {
		values = append(values, fmt.Sprintf("('p%d', 'codex', 'PR-%04d')", i, i))
	}
	l.sql("INSERT INTO msgs VALUES "+strings.Join(values, ", "), true)
	l.sql("UPDATE stage SET n = 4", true)
	pane := l.open("lazyrex_modelab_mode")
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last screen:\n%s", last)
		}
	})
	l.waitFor("PR-0520 printed and the live region drawn", 60*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "PR-0520") && strings.Contains(last, "LAB-NOTICE")
	})
	t.Logf("the capture holds %d lines", strings.Count(last, "\n")+1)
	head := "20 earlier messages. Thread Settings… > Show full history"
	if c := strings.Count(last, "earlier messages"); c != 1 || !strings.Contains(last, head) {
		t.Fatalf("the count line is on the pane %d times, or its text differs:\n%s", c, last)
	}
	at := strings.Index(last, head)
	for i := 1; i <= 520; i++ {
		text := fmt.Sprintf("PR-%04d", i)
		c := strings.Count(last, text)
		if i <= 20 {
			if c != 0 {
				t.Fatalf("%s is outside the window but is on the pane %d times", text, c)
			}
			continue
		}
		if c != 1 {
			t.Fatalf("%s is on the pane %d times:\n%s", text, c, last)
		}
		j := strings.Index(last, text)
		if j < at {
			t.Fatalf("%s is above the message or the count line before it:\n%s", text, last)
		}
		at = j
	}
	if strings.Contains(last, "(late)") {
		t.Fatalf("a message is marked late:\n%s", last)
	}
}

// TestNewlineKeysLab (thread view plan item 5): a real pane in thread mode with a composer that
// holds DRAFT-TEXT. Rex sends Shift+Enter, a, Alt+Enter, b, Ctrl+J, c, then Enter. Each newline key
// inserts a line; Enter sends once: the plugin's view.send receives "DRAFT-TEXT\na\nb\nc".
func TestNewlineKeysLab(t *testing.T) {
	l := newModeLab(t)
	pane := l.open("lazyrex_modelab_mode")
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last screen:\n%s", last)
		}
	})
	l.waitFor("the thread frame", 30*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "LAB-NOTICE")
	})
	l.sql("UPDATE stage SET n = 2", true)
	l.waitFor("the composer tree", 10*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "DRAFT-TEXT") && strings.Contains(last, "Action w2")
	})
	// The renderer focuses the composer's editor first (model.apply), with the cursor at the end.
	l.must("send-key", "-s", l.session, "-b", pane, "shift+enter", "a", "alt+enter", "b", "ctrl+j", "c")
	// The live region shows the editor row with the cursor: "│ c". The plugin's one send below
	// proves the three newlines (a lab run at 1bf675c1 showed only that row).
	l.waitFor("the typed c", 10*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "│ c ")
	})
	if rows := l.sql("SELECT text FROM sent", false); len(rows) != 0 {
		t.Fatalf("a newline key sent the draft: %v", rows)
	}
	l.key(pane, "enter")
	var rows [][]any
	l.waitFor("the send", 20*time.Second, func() bool {
		rows = l.sql("SELECT text FROM sent", false)
		return len(rows) > 0
	})
	time.Sleep(time.Second) // a second send would come within this time
	rows = l.sql("SELECT text FROM sent", false)
	if len(rows) != 1 || rows[0][0] != "DRAFT-TEXT\na\nb\nc" {
		t.Fatalf("the plugin received %q, want one send of %q", rows, "DRAFT-TEXT\na\nb\nc")
	}
}

// unwrapped is the whole capture with soft-wrapped rows joined (rex block capture --unwrap). A hard
// newline stays a line break: TestResizeLab checks that on a message with one.
func (l *chunkLab) unwrapped(pane string) string {
	out, _ := l.cli("block", "capture", "-s", l.session, "-b", pane, "--unwrap")
	return out
}

// TestResizeLab (thread view plan item 7): a real pane in thread mode prints messages longer than
// 110 cells and one message with a hard newline. The pane resizes 80 -> 50 -> 110 -> 80, and a new
// long message prints after each resize. After every print, the unwrapped capture holds each long
// message's whole text as one contiguous string on one line: the renderer never breaks a line, and
// the terminal's soft wrap joins back. The hard newline stays (HARD-A and HARD-B on two lines), the
// start markers are each on the pane once, in order, and the live region is on the pane once.
func TestResizeLab(t *testing.T) {
	l := newModeLab(t)
	long := func(i int) string {
		return fmt.Sprintf("RZ%02d-START %s RZ%02d-END", i, strings.Repeat("word ", 24), i)
	}
	ids := []string{"k7", "c2", "x9", "b4", "q1"} // opaque: the thread order is the insert order
	insert := func(i int) {
		t.Helper()
		l.sql(fmt.Sprintf("INSERT INTO msgs VALUES ('%s', 'codex', '%s')", ids[i-1], long(i)), true)
	}
	l.sql("INSERT INTO msgs VALUES ('h3', 'codex', 'HARD-A' || char(10) || char(10) || 'HARD-B')", true)
	insert(1)
	insert(2)
	l.sql("UPDATE stage SET n = 3", true)
	pane := l.open("lazyrex_modelab_mode")
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last unwrapped capture:\n%s", last)
		}
	})
	check := func(step string, n int) {
		t.Helper()
		l.waitFor(step+": RZ"+fmt.Sprintf("%02d", n)+" printed and the live region drawn", 30*time.Second, func() bool {
			last = l.unwrapped(pane)
			return strings.Contains(last, fmt.Sprintf("RZ%02d-END", n)) && strings.Contains(last, "LAB-NOTICE")
		})
		lines := strings.Split(last, "\n")
		at := -1
		for i := 1; i <= n; i++ {
			text := long(i)
			whole := 0
			for _, line := range lines {
				whole += strings.Count(line, text)
			}
			if whole != 1 {
				t.Fatalf("%s: the whole text of RZ%02d is on one line %d times, want once:\n%s", step, i, whole, last)
			}
			start := fmt.Sprintf("RZ%02d-START", i)
			if c := strings.Count(last, start); c != 1 {
				t.Fatalf("%s: %s is on the pane %d times:\n%s", step, start, c, last)
			}
			j := strings.Index(last, start)
			if j < at {
				t.Fatalf("%s: %s is above the message before it:\n%s", step, start, last)
			}
			at = j
		}
		for _, line := range lines {
			if strings.Contains(line, "HARD-A") && strings.Contains(line, "HARD-B") {
				t.Fatalf("%s: the hard newline between HARD-A and HARD-B is gone (unwrap joined it):\n%s", step, last)
			}
		}
		if strings.Count(last, "HARD-A") != 1 || strings.Count(last, "HARD-B") != 1 {
			t.Fatalf("%s: HARD-A or HARD-B is not on the pane once:\n%s", step, last)
		}
		if c := strings.Count(last, "LAB-NOTICE"); c != 1 {
			t.Fatalf("%s: the live region is on the pane %d times:\n%s", step, c, last)
		}
	}
	check("the first tree at 80 columns", 2)
	for k, width := range []int{50, 110, 80} {
		l.resize(pane, width, 24)
		insert(3 + k)
		check(fmt.Sprintf("a new message after the resize to %d columns", width), 3+k)
	}
	t.Logf("the final unwrapped capture:\n%s", last)
}

// TestLateMessageLab (thread view plan item 7): the thread order is explicit (msg_sort), and the IDs
// are opaque. Three messages print; then a new message that the thread places between the first
// two arrives. It prints once, after the printed messages, with "(late)" after the author. A later
// message prints once, not late, and nothing prints a second time.
func TestLateMessageLab(t *testing.T) {
	l := newModeLab(t)
	// The sort row goes first: a render between the two writes then does not show the message yet.
	// The other order showed it once at its insert position, before every sort value, and so late
	// (lab run c6-f8f826e).
	add := func(id, text string, sort float64) {
		t.Helper()
		l.sql(fmt.Sprintf("INSERT INTO msg_sort VALUES ('%s', %g)", id, sort), true)
		l.sql(fmt.Sprintf("INSERT INTO msgs VALUES ('%s', 'codex', '%s')", id, text), true)
	}
	add("t5", "LATE-ONE", 10)
	add("a8", "LATE-TWO", 20)
	add("m2", "LATE-THREE", 30)
	l.sql("UPDATE stage SET n = 3", true)
	pane := l.open("lazyrex_modelab_mode")
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last capture:\n%s", last)
		}
	})
	wait := func(step, text string) {
		t.Helper()
		l.waitFor(step, 30*time.Second, func() bool {
			last = l.unwrapped(pane)
			return strings.Contains(last, text) && strings.Contains(last, "LAB-NOTICE")
		})
	}
	wait("the first three", "LATE-THREE")
	add("z1", "LATE-INSIDE", 15)
	wait("the late message", "LATE-INSIDE")
	add("e6", "LATE-AFTER", 40)
	wait("the next message", "LATE-AFTER")
	for _, text := range []string{"LATE-ONE", "LATE-TWO", "LATE-THREE", "LATE-INSIDE", "LATE-AFTER"} {
		if c := strings.Count(last, text); c != 1 {
			t.Fatalf("%s is on the pane %d times:\n%s", text, c, last)
		}
	}
	order := []string{"LATE-ONE", "LATE-TWO", "LATE-THREE", "LATE-INSIDE", "LATE-AFTER"}
	for i := 1; i < len(order); i++ {
		if strings.Index(last, order[i]) < strings.Index(last, order[i-1]) {
			t.Fatalf("%s is above %s:\n%s", order[i], order[i-1], last)
		}
	}
	if c := strings.Count(last, "(late)"); c != 1 {
		t.Fatalf("the pane has %d late marks, want 1:\n%s", c, last)
	}
	lines := strings.Split(last, "\n")
	for i, line := range lines {
		if strings.Contains(line, "LATE-INSIDE") {
			if i == 0 || !strings.Contains(lines[i-1], "codex (late)") {
				t.Fatalf("the line above LATE-INSIDE is not its header with the late mark after the author:\n%s", last)
			}
		}
	}
}

// TestComposerGrowLab (lead-comp2): a real 80x24 pane in thread mode with the stage 2 composer.
// Seven typed lines grow the input, and deleting them shrinks it again. The two stage 2 actions
// share the 10-row bound, so the input grows from 1 to 2 rows here (the frame grows by one row);
// TestComposerGrowsWithItsText covers the 6-row cap. After each change the capture (scrollback and screen) holds each live node and each typed
// line at most once, and after the delete no typed line stays: the live region's height change
// leaves no stale rows.
func TestComposerGrowLab(t *testing.T) {
	l := newModeLab(t)
	l.sql("UPDATE stage SET n = 2", true)
	pane := l.open("lazyrex_modelab_mode")
	l.resize(pane, 80, 24)
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last capture:\n%s", last)
		}
	})
	l.waitFor("the composer frame", 30*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "DRAFT-TEXT") && strings.Contains(last, "Ctrl+Q quit")
	})
	once := func(step string, texts ...string) {
		t.Helper()
		for _, text := range append([]string{"╭", "Action w1", "Action w2"}, texts...) {
			if n := strings.Count(last, text); n > 1 {
				t.Errorf("%s: %d copies of %q on the pane, want at most 1", step, n, text)
			}
		}
	}
	// The renderer focuses the composer's editor first (model.apply), with the cursor at the end.
	keys := []string{"send-key", "-s", l.session, "-b", pane}
	var typed []string
	for i := 2; i <= 8; i++ {
		keys = append(keys, "shift+enter", "l", fmt.Sprint(i)) // rex send-key sends a key name in lowercase
		typed = append(typed, fmt.Sprintf("│ l%d ", i))
	}
	l.must(keys...)
	l.waitFor("the grown composer", 10*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "│ l8 ")
	})
	time.Sleep(time.Second) // the frames for the earlier lines are drawn
	last = l.screen(pane)
	t.Logf("grown capture:\n%s", last)
	once("grown", typed...)
	// The two actions share the 10-row bound, so the input shows 2 to 6 of its 8 lines.
	if n := strings.Count(last, "│ l"); n < 2 || n > 6 {
		t.Errorf("grown: %d typed input rows on the pane, want 2 to 6", n)
	}
	back := []string{"send-key", "-s", l.session, "-b", pane}
	for range 7 * 3 {
		back = append(back, "backspace")
	}
	l.must(back...)
	l.waitFor("the shrunk composer", 10*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "│ DRAFT-TEXT ") && !strings.Contains(last, "│ l8 ")
	})
	time.Sleep(time.Second)
	last = l.screen(pane)
	t.Logf("shrunk capture:\n%s", last)
	once("shrunk", "DRAFT-TEXT")
	if strings.Contains(last, "│ l") {
		t.Errorf("shrunk: a typed line stays on the pane after the delete")
	}
}

// TestLiveRegionResizeLab (coord-tvglitch-1): a real pane in thread mode with a docked composer
// (stage 2). The pane changes size several times, one size at a time. After each change the whole
// capture (scrollback and screen) holds each live node at most once: the composer border,
// DRAFT-TEXT, the two actions and the more-above row. A resize never moves live rows into the
// scrollback legitimately, so a second copy is a stale row of an earlier frame. Every new pane
// holds the old frame, also after Rex rewraps it: a pane shorter than that pushes the old frame's
// top rows into the scrollback, where no frame can erase them (teaprobe A5, a known limit).
func TestLiveRegionResizeLab(t *testing.T) {
	liveRegionResize(t, [][][2]int{{{50, 24}}, {{80, 24}}, {{40, 24}}, {{100, 24}}, {{100, 12}}, {{60, 30}}, {{60, 14}}, {{90, 26}}, {{120, 40}}, {{70, 18}}})
}

// TestLiveRegionResizeBurstLab: the same pane gets two sizes with no wait between them, as an app
// does when it lays out a pane that it shows. It is a known limit (view/README.md, "Known
// limits"): one run at 027b8718 left 2 copies of the actions after 120x40 then 70x18. Lab only:
// RESIZE_BURST_LAB=1 with LAZYREX_LAB=1; VIEW_TEA_TRACE=1 keeps each view's renderer writes.
func TestLiveRegionResizeBurstLab(t *testing.T) {
	if os.Getenv("RESIZE_BURST_LAB") != "1" {
		t.Skip("known defect: resize burst leaves stale rows (lead-tvburst); set RESIZE_BURST_LAB=1 to run it")
	}
	var steps [][][2]int
	for range 3 {
		steps = append(steps, [][2]int{{60, 14}, {90, 26}}, [][2]int{{120, 40}, {70, 18}})
	}
	liveRegionResize(t, steps)
}

// liveRegionResize opens the stage 2 pane at 80x24 and applies each step: the sizes of one step
// come with no wait between them. After each step, each live node is on the pane at most once.
func liveRegionResize(t *testing.T, steps [][][2]int) {
	l := newModeLab(t)
	l.sql("UPDATE stage SET n = 2", true)
	pane := l.open("lazyrex_modelab_mode")
	l.resize(pane, 80, 24)
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last capture:\n%s", last)
		}
	})
	l.waitFor("the composer frame", 30*time.Second, func() bool {
		last = l.screen(pane)
		return strings.Contains(last, "DRAFT-TEXT") && strings.Contains(last, "Ctrl+Q quit")
	})
	t.Logf("80x24 start capture:\n%s", last)
	for _, sizes := range steps {
		step := ""
		for _, size := range sizes {
			step += fmt.Sprintf("%dx%d ", size[0], size[1])
			l.resize(pane, size[0], size[1])
		}
		time.Sleep(1500 * time.Millisecond) // the renderer gets the size and draws again
		l.waitFor(step+": the frame", 10*time.Second, func() bool {
			last = l.screen(pane)
			return strings.Contains(last, "DRAFT-TEXT") && strings.Contains(last, "Ctrl+Q quit")
		})
		t.Logf("%s capture:\n%s", step, last)
		// The tree has one of each, so a second copy is a stale row of an earlier frame.
		for _, text := range []string{"╭", "DRAFT-TEXT", "Action w1", "Action w2", "more above"} {
			if n := strings.Count(last, text); n > 1 {
				t.Errorf("%s: %d copies of %q on the pane, want at most 1", step, n, text)
			}
		}
	}
}
