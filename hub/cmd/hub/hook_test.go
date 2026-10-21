package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The hook tests run the hook as a child process of this test binary (TestHookHelperProcess) with a
// fake rex: a shell script that answers `client ls --json` and records the report call. They need
// no Rex server and change no user configuration.

const fakeHookRex = `#!/bin/sh
case "$HOOK_MODE" in
nohub) echo 'hub not found' >&2; exit 4 ;;
slow) exec sleep 10 ;;
esac
case "$*" in
*"client ls --json") printf '%s' "$HOOK_CLIENTS"; exit 0 ;;
esac
[ "$HOOK_MODE" = hang ] && exec sleep 10
printf '%s\0' "$$" "$PPID" "$@" > "$HOOK_CAPTURE.tmp" && mv "$HOOK_CAPTURE.tmp" "$HOOK_CAPTURE"
echo 'This output must not become model context.'
[ -n "$HOOK_LINGER" ] && sleep 1
exit 0
`

func TestHookHelperProcess(t *testing.T) {
	if os.Getenv("LAZYREX_HOOK_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.Exit(runHook(args, helperSelf()))
}

func helperSelf() []string { return []string{os.Args[0], "-test.run=^TestHookHelperProcess$", "--"} }

type hookLab struct {
	t       *testing.T
	dir     string
	capture string
	env     map[string]string
}

func newHookLab(t *testing.T) *hookLab {
	dir := t.TempDir()
	rex := filepath.Join(dir, "rex with spaces")
	if err := os.WriteFile(rex, []byte(fakeHookRex), 0o755); err != nil {
		t.Fatal(err)
	}
	clients, _ := json.Marshal(map[string]any{"clients": []any{
		map[string]any{"client_id": "client:hub", "info": map[string]any{"label": "lazyrex-hub"}, "connection_state": "connected"}}})
	return &hookLab{t: t, dir: dir, capture: filepath.Join(dir, "args"), env: map[string]string{
		"LAZYREX_HOOK_HELPER": "1", "REX_SERVER": "unix:///tmp/hook-test/rex.sock", "REX_SESSION": "session:lab",
		"REX_BLOCK": "block:lab", "REX_BIN": rex, "HOOK_CLIENTS": string(clients), "PATH": os.Getenv("PATH"),
	}}
}

func (l *hookLab) command(kind string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], append(helperSelf(), kind)[1:]...)
	for k, v := range l.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "HOOK_CAPTURE="+l.capture)
	return cmd
}

// run runs one hook with the event as its input. It returns the report call's arguments (after the
// pid and the parent pid), or nil when the hook reported nothing.
func (l *hookLab) run(event any, kind string) []string {
	l.t.Helper()
	os.Remove(l.capture)
	input, _ := json.Marshal(event)
	cmd := l.command(kind)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		l.t.Fatalf("hook: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		l.t.Fatalf("hook wrote output: %q %q", stdout.String(), stderr.String())
	}
	got := l.read()
	if got == nil {
		return nil
	}
	return got[2:]
}

// read returns the whole capture (pid, parent pid, arguments), or nil.
func (l *hookLab) read() []string {
	data, err := os.ReadFile(l.capture)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

func reportArgs(t *testing.T, args []string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(args[10]), &m); err != nil {
		t.Fatalf("report args %q: %v", args[10], err)
	}
	return m
}

func TestHookPreservesJSONAndUsesOnlyTheExplicitServer(t *testing.T) {
	l := newHookLab(t)
	conversation := `quote " space 雪; $(touch never)`
	for _, kind := range []string{"codex", "claude"} {
		args := l.run(map[string]any{"hook_event_name": "SessionStart", "session_id": conversation, "source": "clear"}, kind)
		if args == nil {
			t.Fatalf("%s: no report", kind)
		}
		want := []string{"-S", l.env["REX_SERVER"], "--autostart=false", "--timeout", "2s", "-C", "client:hub", "do", "hub.identity.report", "--args"}
		if strings.Join(args[:10], "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("%s: args %q", kind, args[:10])
		}
		got := reportArgs(t, args)
		wantReport := map[string]any{"kind": kind, "conversation_id": conversation, "source": "clear", "block": "block:lab"}
		if len(got) != len(wantReport) {
			t.Fatalf("%s: report %v", kind, got)
		}
		for k, v := range wantReport {
			if got[k] != v {
				t.Fatalf("%s: report %v", kind, got)
			}
		}
	}
}

func TestHookClaudePromptRepairsAMissingStartReport(t *testing.T) {
	l := newHookLab(t)
	event := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "new-id"}
	if args := l.run(event, "codex"); args != nil {
		t.Fatalf("codex prompt reported: %q", args)
	}
	args := l.run(event, "claude")
	if args == nil {
		t.Fatal("claude prompt: no report")
	}
	if m := reportArgs(t, args); m["source"] != "prompt" || m["conversation_id"] != "new-id" || m["kind"] != "claude" {
		t.Fatalf("report %v", m)
	}
}

func TestHookDiscoveryIgnoresLostClientsAndRefusesMultipleHubs(t *testing.T) {
	l := newHookLab(t)
	event := map[string]any{"hook_event_name": "SessionStart", "session_id": "id", "source": "startup"}
	hub := map[string]any{"client_id": "client:hub", "info": map[string]any{"label": "lazyrex-hub"}, "connection_state": "connected"}
	old := map[string]any{"client_id": "client:old", "info": map[string]any{"label": "lazyrex-hub"}, "connection_state": "lost"}
	set := func(clients ...any) {
		b, _ := json.Marshal(map[string]any{"clients": clients})
		l.env["HOOK_CLIENTS"] = string(b)
	}
	set(hub, old)
	if args := l.run(event, "codex"); args == nil || args[6] != "client:hub" {
		t.Fatalf("args %q", args)
	}
	old["connection_state"] = "connected"
	set(hub, old)
	if args := l.run(event, "codex"); args != nil {
		t.Fatalf("two hubs reported: %q", args)
	}
}

func TestHookOutsideRexDoesNotWaitForInput(t *testing.T) {
	l := newHookLab(t)
	delete(l.env, "REX_BLOCK")
	cmd := l.command("codex")
	stdin, err := cmd.StdinPipe() // never written or closed before the hook ends
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	started := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(started); d > time.Second {
		t.Fatalf("the hook waited %v", d)
	}
	if l.read() != nil {
		t.Fatal("reported outside Rex")
	}
}

func TestHookIgnoresIrrelevantOrInvalidInput(t *testing.T) {
	l := newHookLab(t)
	for _, event := range []any{nil, []any{}, map[string]any{}, map[string]any{"hook_event_name": "Stop"},
		map[string]any{"hook_event_name": "SessionStart", "session_id": 9, "source": "startup"},
		map[string]any{"hook_event_name": "SessionStart", "session_id": "id", "source": "unknown"},
		map[string]any{"hook_event_name": "SessionStart", "session_id": "", "source": "startup"},
		map[string]any{"hook_event_name": "SessionStart", "session_id": "a\x00b", "source": "startup"}} {
		if args := l.run(event, "codex"); args != nil {
			t.Fatalf("%v reported: %q", event, args)
		}
	}
	// An unknown kind reports nothing either.
	if args := l.run(map[string]any{"hook_event_name": "SessionStart", "session_id": "id", "source": "startup"}, "gemini"); args != nil {
		t.Fatalf("unknown kind reported: %q", args)
	}
}

func TestHookMissingRexIsAdvisory(t *testing.T) {
	l := newHookLab(t)
	l.env["REX_BIN"] = filepath.Join(l.dir, "missing")
	if args := l.run(map[string]any{"hook_event_name": "SessionStart", "session_id": "id", "source": "startup"}, "codex"); args != nil {
		t.Fatalf("reported: %q", args)
	}
}

func TestHookNoHubIsAdvisory(t *testing.T) {
	l := newHookLab(t)
	l.env["HOOK_MODE"] = "nohub"
	if args := l.run(map[string]any{"hook_event_name": "SessionStart", "session_id": "id", "source": "resume"}, "codex"); args != nil {
		t.Fatalf("reported: %q", args)
	}
}

func TestHookUnresponsiveRexHasADeadline(t *testing.T) {
	for _, mode := range []string{"slow", "hang"} { // slow: client ls hangs; hang: the report call hangs
		l := newHookLab(t)
		l.env["HOOK_MODE"] = mode
		started := time.Now()
		if args := l.run(map[string]any{"hook_event_name": "SessionStart", "session_id": "id", "source": "compact"}, "codex"); args != nil {
			t.Fatalf("%s: reported: %q", mode, args)
		}
		if d := time.Since(started); d > 4*time.Second {
			t.Fatalf("%s: the hook took %v", mode, d)
		}
	}
}

// The reporter starts before the input is read, and it is the process that calls Rex: an older hook
// whose input comes late still has the older reporter. Its parent is the hook process.
func TestHookReporterStartsBeforeInputAndDescendsFromTheHook(t *testing.T) {
	older, newer := newHookLab(t), newHookLab(t)
	older.env["HOOK_LINGER"], newer.env["HOOK_LINGER"] = "1", "1"
	event := func(id string) []byte {
		b, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": id, "source": "startup"})
		return b
	}
	a := older.command("claude")
	stdin, err := a.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the older reporter has started and waits for its input
	b := newer.command("claude")
	b.Stdin = bytes.NewReader(event("newer"))
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	newerCapture := waitCapture(t, newer)
	newerStart := startTime(atoi(t, newerCapture[0]))
	stdin.Write(event("older"))
	stdin.Close()
	olderCapture := waitCapture(t, older)
	olderStart := startTime(atoi(t, olderCapture[0]))
	if olderStart == "" || newerStart == "" {
		t.Fatalf("a reporter is gone before its check: %q %q", olderStart, newerStart)
	}
	if !startBefore(olderStart, newerStart) {
		t.Fatalf("the older hook's reporter started at %s, not before the newer one at %s", olderStart, newerStart)
	}
	if atoi(t, olderCapture[1]) != a.Process.Pid || atoi(t, newerCapture[1]) != b.Process.Pid {
		t.Fatalf("reporter parents %s %s, hooks %d %d", olderCapture[1], newerCapture[1], a.Process.Pid, b.Process.Pid)
	}
	if err := a.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}
}

func waitCapture(t *testing.T, l *hookLab) []string {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if c := l.read(); c != nil {
			return c
		}
	}
	t.Fatal("no report")
	return nil
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The reporter mode is reachable through the CLI (`lazyrex-hub hook -reporter codex`). Without the
// Rex variables it must not call Rex either: Rex would choose a server.
func TestHookReporterModeKeepsTheServerGate(t *testing.T) {
	for _, missing := range []string{"REX_SERVER", "REX_SESSION", "REX_BLOCK"} {
		l := newHookLab(t)
		delete(l.env, missing)
		input, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "id", "source": "startup"})
		cmd := exec.Command(os.Args[0], append(helperSelf(), "-reporter", "codex")[1:]...)
		for k, v := range l.env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Env = append(cmd.Env, "HOOK_CAPTURE="+l.capture)
		cmd.Stdin = bytes.NewReader(input)
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		if c := l.read(); c != nil {
			t.Fatalf("without %s the reporter called Rex: %q", missing, c)
		}
	}
}

// The input bound: an input of exactly hookInputLimit bytes is read whole; a longer one gets no
// report, also when its first hookInputLimit bytes are a valid event.
func TestHookInputLimit(t *testing.T) {
	event, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "id", "source": "startup"})
	pad := func(n int) []byte {
		return append(append([]byte(nil), event...), bytes.Repeat([]byte(" "), n-len(event))...)
	}
	cases := []struct {
		name   string
		input  []byte
		report bool
	}{
		{"exact limit", pad(hookInputLimit), true},
		{"valid prefix, then invalid data", append(pad(hookInputLimit), "not-json"...), false},
		{"valid JSON over the limit", pad(hookInputLimit + 1), false},
	}
	for _, c := range cases {
		l := newHookLab(t)
		os.Remove(l.capture)
		cmd := l.command("codex")
		cmd.Stdin = bytes.NewReader(c.input)
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		if got := l.read() != nil; got != c.report {
			t.Fatalf("%s: report %v, want %v", c.name, got, c.report)
		}
	}
}

// At the deadline the hook kills the reporter's whole process group, so a child that the reporter
// (rex) started does not outlive the hook. (Reviewer test; the Python hook killed one process.)
func TestHookDeadlineLeavesNoChild(t *testing.T) {
	l := newHookLab(t)
	pidPath := filepath.Join(l.dir, "sleep.pid")
	script := "#!/bin/sh\ncase \"$*\" in\n*\"client ls --json\") printf '%s' \"$HOOK_CLIENTS\"; exit 0 ;;\nesac\n" +
		"sleep 30 &\necho $! > \"$HOOK_SLEEP_PID\"\nwait\n"
	if err := os.WriteFile(l.env["REX_BIN"], []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	l.env["HOOK_SLEEP_PID"] = pidPath
	input, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "id", "source": "startup"})
	cmd := l.command("codex")
	cmd.Stdin = bytes.NewReader(input)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("the child did not start: %v", err)
	}
	pid := atoi(t, strings.TrimSpace(string(data)))
	start := startTime(pid)
	if start != "" {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the hook returned with the reporter's child %d still alive", pid)
	}
}
