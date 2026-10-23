package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The coordinator ruling c49-kit-option-a and its review (codex-26): an until item repeats the same
// call in the same pane until its expects pass, in one window of 60 s for all attempts; the lab
// adds an agent profile that runs only "true".

func untilItem(action string) relSetItem {
	return relSetItem{Kind: "action", Action: action, Args: json.RawMessage(`{"key":"k-1"}`),
		Until: []relSetExpect{{Pointer: "/status", Value: json.RawMessage(`"done"`)}}}
}

func TestReleaseSetUntilDecide(t *testing.T) {
	it := untilItem("x.poll")
	start := time.Unix(1000, 0)
	if v, err := untilDecide(it, 1, 0, []byte(`{"status":"done"}`), nil, start, start); err != nil || v != "next" {
		t.Fatalf("met: %q %v", v, err)
	}
	if v, err := untilDecide(it, 1, 0, []byte(`{"status":"running"}`), nil, start, start.Add(10*time.Second)); err != nil || v != "again 49" {
		t.Fatalf("not met at 10 s: %q %v", v, err)
	}
	// The window covers all attempts: no time for another call after the pause.
	_, err := untilDecide(it, 7, 0, []byte(`{"status":"running"}`), nil, start, start.Add(58500*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "after 7 attempts") || !strings.Contains(err.Error(), `(exit 0): {"status":"running"}`) {
		t.Fatalf("timeout: %v", err)
	}
	// A matching reply after the window does not count (codex-26, review of 7fca215).
	if _, err := untilDecide(it, 1, 0, []byte(`{"status":"done"}`), nil, start, start.Add(61*time.Second)); err == nil || !strings.Contains(err.Error(), "after the window") {
		t.Fatalf("a met reply after 61 s: %v", err)
	}
	// An error reply is not met: it repeats while the window has time, then fails with its exit code.
	if v, err := untilDecide(it, 2, 1, []byte("Error: unavailable"), nil, start, start); err != nil || v != "again 59" {
		t.Fatalf("nonzero exit: %q %v", v, err)
	}
	if _, err := untilDecide(it, 9, 1, []byte("Error: unavailable"), nil, start, start.Add(59*time.Second)); err == nil || !strings.Contains(err.Error(), "(exit 1): Error: unavailable") {
		t.Fatalf("nonzero exit at the end: %v", err)
	}
	// Placeholders in until values are filled.
	it.Until = []relSetExpect{{Pointer: "/by", Value: json.RawMessage(`"<SELF>"`)}}
	if v, err := untilDecide(it, 1, 0, []byte(`{"by":"relseed"}`), map[string]string{"<SELF>": "relseed"}, start, start); err != nil || v != "next" {
		t.Fatalf("placeholder: %q %v", v, err)
	}
}

// untilFakeRex writes a rex stand-in into bin: hub.join answers {}; x.poll answers running until its
// third call; x.never always answers running; x.fail exits 1; x.after answers {"ok":true}. Each
// call appends "<action> <timeout> <args>" to calls.log in the working directory.
func untilFakeRex(t *testing.T, bin string) {
	t.Helper()
	script := `#!/bin/sh
act=""; timeout=""; args=""
while [ $# -gt 0 ]; do
  case "$1" in
    --timeout) timeout=$2; shift ;;
    --args) args=$2; shift ;;
    do) act=$2; shift ;;
  esac
  shift
done
echo "$act $timeout $args" >> calls.log
case "$act" in
  hub.join) echo '{}' ;;
  x.poll) n=$(cat poll.n 2>/dev/null || echo 0); n=$((n+1)); echo $n > poll.n
    if [ $n -ge 3 ]; then echo '{"status":"done"}'; else echo '{"status":"running"}'; fi ;;
  x.never) echo '{"status":"running"}' ;;
  x.slow) sleep 5; echo '{"status":"done"}' ;;
  x.slow2) n=$(cat slow2.n 2>/dev/null || echo 0); n=$((n+1)); echo $n > slow2.n
    if [ $n -ge 2 ]; then sleep 5; echo '{"status":"done"}'; else echo '{"status":"running"}'; fi ;;
  x.fail) echo 'Error: conflict'; exit 1 ;;
  x.flaky) n=$(cat flaky.n 2>/dev/null || echo 0); n=$((n+1)); echo $n > flaky.n
    if [ $n -ge 2 ]; then echo '{"status":"done"}'; else echo 'Error: unavailable'; exit 1; fi ;;
  x.after) echo '{"ok":true}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "rex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// runBatch runs the batch script of calls with the fake rex, as a pane would, and serves it.
func runBatch(t *testing.T, calls []relSetItem) (string, []relCallOut, error, []string) {
	t.Helper()
	dir, bin := t.TempDir(), t.TempDir()
	untilFakeRex(t, bin)
	for i := range calls {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("args-%d.json", i)), calls[i].Args, 0o600)
	}
	script := filepath.Join(dir, "batch.sh")
	os.WriteFile(script, []byte(batchScript(dir, "unix:///lab/rex.sock", "client:hub", calls)), 0o700)
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":/bin:/usr/bin")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "go"), nil, 0o600)
	var said []string
	outs, err := serveBatch(dir, calls, nil, func(f string, a ...any) { said = append(said, f) }, time.Now().Add(30*time.Second))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the batch script did not end")
	}
	return dir, outs, err, said
}

func TestReleaseSetUntilBatch(t *testing.T) {
	old := relUntilWindow
	defer func() { relUntilWindow = old }()

	// Pass after repeats: three attempts in the same script with the same args file; the next item
	// runs only after the until item is met.
	relUntilWindow = 20 * time.Second
	after := relSetItem{Kind: "action", Action: "x.after", Args: json.RawMessage(`{}`)}
	dir, outs, err, said := runBatch(t, []relSetItem{untilItem("x.poll"), after})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 2 || outs[0].Code != 0 || strings.TrimSpace(string(outs[0].Out)) != `{"status":"done"}` || strings.TrimSpace(string(outs[1].Out)) != `{"ok":true}` {
		t.Fatalf("outs %+v", outs)
	}
	if len(said) != 3 {
		t.Fatalf("%d attempts logged, want 3", len(said))
	}
	log := strings.Split(strings.TrimSpace(string(mustReadFile(filepath.Join(dir, "calls.log")))), "\n")
	if len(log) != 5 || log[0] != "hub.join 60s {\"agent\":\"relseed\"}" || log[4] != "x.after 60s @args-1.json" {
		t.Fatalf("calls %q", log)
	}
	// Each attempt: the same args file; the first has the whole window, later ones what is left.
	prev := 21
	for _, l := range log[1:4] {
		f := strings.Fields(l)
		var secs int
		if len(f) != 3 || f[0] != "x.poll" || f[2] != "@args-0.json" || !strings.HasSuffix(f[1], "s") {
			t.Fatalf("attempt %q", l)
		}
		if _, err := fmt.Sscan(strings.TrimSuffix(f[1], "s"), &secs); err != nil || secs >= prev || secs < 1 {
			t.Fatalf("attempt %q after a timeout of %d", l, prev)
		}
		prev = secs
	}

	// Timeout: the window ends; the error has the last reply and exit code; the next item never runs.
	relUntilWindow = 3 * time.Second
	start := time.Now()
	dir, _, err, said = runBatch(t, []relSetItem{untilItem("x.never"), after})
	if err == nil || !strings.Contains(err.Error(), "not met after") || !strings.Contains(err.Error(), `(exit 0): {"status":"running"}`) {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > 8*time.Second || len(said) < 2 {
		t.Fatalf("timeout after %s and %d attempts", time.Since(start), len(said))
	}
	if strings.Contains(string(mustReadFile(filepath.Join(dir, "calls.log"))), "x.after") {
		t.Fatal("the item after a failed until item ran")
	}

	// An error reply, then a met reply (a plugin that answers unavailable while it starts).
	relUntilWindow = 20 * time.Second
	_, outs, err, said = runBatch(t, []relSetItem{untilItem("x.flaky")})
	if err != nil || len(said) != 2 || strings.TrimSpace(string(outs[0].Out)) != `{"status":"done"}` || outs[0].Code != 0 {
		t.Fatalf("flaky: %v, %d attempts, %+v", err, len(said), outs)
	}
	// Error replies only: the window ends with the last exit code and reply.
	relUntilWindow = 3 * time.Second
	_, _, err, said = runBatch(t, []relSetItem{untilItem("x.fail")})
	if err == nil || !strings.Contains(err.Error(), "(exit 1): Error: conflict") || len(said) < 2 {
		t.Fatalf("error replies: %v, %d attempts", err, len(said))
	}
}

func TestReleaseSetUntilParse(t *testing.T) {
	act := untilItem("x.poll")
	if err := act.check(); err != nil {
		t.Fatal(err)
	}
	view := relSetItem{Kind: "view", PaneDir: "<LAB>/src", Command: "p.c", Route: "/r", NodeID: "n", Action: "a", OperationKey: "k", Until: act.Until}
	if err := view.check(); err == nil {
		t.Error("a view item with until: want an error")
	}
	rows := [][]json.RawMessage{}
	sql := relSetItem{Kind: "sql", SQL: "SELECT 1", Rows: &rows, Until: act.Until}
	if err := sql.check(); err == nil {
		t.Error("a sql item with until: want an error")
	}
	act.Until = []relSetExpect{{Pointer: "status", Value: json.RawMessage(`1`)}}
	if err := act.check(); err == nil {
		t.Error("an until pointer without /: want an error")
	}
}

// TestReleaseSetLabProfiles: the set lab adds lab_inert (accessory) and lab_agent (agent), both with
// the command ["true"] only, to the lab profiles; it refuses a config or profiles file outside the
// lab, and a live profile with either id.
func TestReleaseSetLabProfiles(t *testing.T) {
	setup := func(t *testing.T, profilesAt string, live []any) (*rehearsal, string, string, string) {
		lab := t.TempDir()
		p := filepath.Join(lab, "home", "profiles.json")
		if profilesAt != "" {
			p = profilesAt
		}
		os.MkdirAll(filepath.Dir(p), 0o700)
		b, _ := json.Marshal(map[string]any{"profiles": live})
		os.WriteFile(p, b, 0o600)
		cfg := filepath.Join(lab, "hub.json")
		cb, _ := json.Marshal(map[string]any{"profiles": p})
		os.WriteFile(cfg, cb, 0o600)
		h := &rehearsal{set: &relSetState{file: &relSetFile{}}, env: os.Environ()}
		return h, lab, cfg, p
	}
	h, lab, cfg, p := setup(t, "", []any{map[string]any{"id": "claude", "kind": "agent", "command": []any{"claude"}}})
	if _, err := h.setLabFiles(lab, cfg); err != nil {
		t.Fatal(err)
	}
	var pf struct {
		Profiles []struct {
			ID      string   `json:"id"`
			Kind    string   `json:"kind"`
			Command []string `json:"command"`
		} `json:"profiles"`
	}
	json.Unmarshal(mustReadFile(p), &pf)
	got := map[string]string{}
	for _, q := range pf.Profiles {
		if q.ID == relLabInert || q.ID == relLabAgent {
			if len(q.Command) != 1 || q.Command[0] != "true" {
				t.Errorf("%s runs %q", q.ID, q.Command)
			}
			got[q.ID] = q.Kind
		}
	}
	if got[relLabInert] != "accessory" || got[relLabAgent] != "agent" || len(pf.Profiles) != 3 {
		t.Fatalf("lab profiles %+v", pf.Profiles)
	}

	// A profiles file outside the lab (a live file) is refused and keeps its bytes.
	outside := filepath.Join(t.TempDir(), "profiles.json")
	h, lab, cfg, _ = setup(t, outside, []any{})
	before := mustReadFile(outside)
	_, err := h.setLabFiles(lab, cfg)
	if string(mustReadFile(outside)) != string(before) {
		t.Error("the outside profiles file changed")
	}
	if err == nil || !strings.Contains(err.Error(), "the set lab profiles") {
		t.Errorf("outside profiles: %v", err)
	}
	// A config outside the lab is refused.
	h, lab, _, _ = setup(t, "", []any{})
	if _, err := h.setLabFiles(lab, filepath.Join(t.TempDir(), "hub.json")); err == nil || !strings.Contains(err.Error(), "the set lab config") {
		t.Fatalf("outside config: %v", err)
	}
	// A live profile named lab_agent is refused.
	h, lab, cfg, _ = setup(t, "", []any{map[string]any{"id": relLabAgent, "kind": "agent", "command": []any{"x"}}})
	if _, err := h.setLabFiles(lab, cfg); err == nil || !strings.Contains(err.Error(), "lab_agent") {
		t.Fatalf("live lab_agent: %v", err)
	}
}

// TestReleaseSetLabFileParses: the set file of the lab test parses, and it has an until item.
func TestReleaseSetLabFileParses(t *testing.T) {
	set, err := parseSet(setLabFile(t))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range set.Plugins {
		for _, it := range append(append([]relSetItem{}, e.Seed...), e.Probe...) {
			if len(it.Until) > 0 {
				n++
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d until items, want 2 (seed and probe)", n)
	}
}

// TestReleaseSetUntilWindow: the window starts at the start marker of the script, and it ends a
// pending attempt; the error names the last completed reply (codex-26, review of 7fca215).
func TestReleaseSetUntilWindow(t *testing.T) {
	old := relUntilWindow
	defer func() { relUntilWindow = old }()
	relUntilWindow = 60 * time.Second

	// A start marker 61 s old and a matching first reply: the reply is after the window.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "start-0"), nil, 0o600)
	past := time.Now().Add(-61 * time.Second)
	os.Chtimes(filepath.Join(dir, "start-0"), past, past)
	os.WriteFile(filepath.Join(dir, "out-0-1"), []byte(`{"status":"done"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "code-0-1"), []byte("0\n"), 0o600)
	_, err := serveBatch(dir, []relSetItem{untilItem("x.poll")}, nil, func(string, ...any) {}, time.Now().Add(10*time.Second))
	if err == nil || !strings.Contains(err.Error(), "after the window") {
		t.Fatalf("an old start marker: %v", err)
	}
	if v := strings.TrimSpace(string(mustReadFile(filepath.Join(dir, "go-0-1")))); v != "stop" {
		t.Fatalf("the answer is %q, not stop", v)
	}

	// A pending first attempt: the error comes at the end of the window, before the call ends,
	// and says that no attempt completed.
	relUntilWindow = 2 * time.Second
	start := time.Now()
	_, _, err, _ = runBatch(t, []relSetItem{untilItem("x.slow")})
	if err == nil || !strings.Contains(err.Error(), "ended while attempt 1 ran; the last reply: no attempt completed") {
		t.Fatalf("a pending first attempt: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the batch took %s", d)
	}

	// A pending second attempt: the error names the first reply.
	relUntilWindow = 3 * time.Second
	_, _, err, _ = runBatch(t, []relSetItem{untilItem("x.slow2")})
	if err == nil || !strings.Contains(err.Error(), `ended while attempt 2 ran; the last reply: attempt 1 (exit 0): {"status":"running"}`) {
		t.Fatalf("a pending second attempt: %v", err)
	}
}
