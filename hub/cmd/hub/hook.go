package main

// lazyrex-hub hook claude|codex: the agent conversation hook (hooks/README.md). It reports the
// agent's conversation to the hub with hub.identity.report through the Rex CLI.
//
// Two processes, as in the hook that it replaces:
//   - The hook process starts the reporter first, before anything reads the hook input, and gives
//     it the input. It waits at most 3 s, then kills the reporter. It always exits 0 and writes
//     nothing (a hook's output can become model context).
//   - The reporter reads the input, finds the hub, and replaces itself with `rex` (exec). Rex
//     attests the caller pid, so the hub sees the reporter's pid and start time. The hub orders
//     hook reports by that start time (identity.go), so the reporter must start before the input
//     is read: an older hook whose input comes late is then still older. Its parent is the hook
//     process, so the pid chain leads to the agent's pane.
//
// The hook does nothing outside a Rex pane. It does not start Rex or retry a missing hub.

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// hookDeadline bounds a whole hook run; the agent's hook timeout is 5 s.
const hookDeadline = 3 * time.Second

// hookInputLimit bounds the hook input: no unbounded read in a process that every agent start runs
// (the coordinator, 2026-10-21; the same reason as the manifest bound). A longer input gets no
// report. It is never parsed as a prefix.
const hookInputLimit = 1 << 20

var hookSources = map[string]bool{"startup": true, "resume": true, "clear": true, "compact": true, "fork": true, "prompt": true}

func hookMain(args []string) int {
	self, err := os.Executable()
	if err != nil {
		return 0
	}
	return runHook(args, []string{self, "hook"})
}

// runHook runs the hook (args: kind) or the reporter (args: -reporter kind). self is the command
// that runs this function again (the binary and its subcommand).
func runHook(args, self []string) int {
	if !inRexPane() {
		return 0
	}
	if len(args) == 2 && args[0] == "-reporter" {
		hookReport(args[1]) // returns only when it reports nothing
		// No report. A command that the reporter ran (rex client ls) can have left a child in the
		// reporter's group. -reporter is internal: the hook starts it as the leader of a new group, so
		// the group holds only the reporter and what it started; end that group (this process too).
		// A reporter that does not lead its group kills nothing.
		if syscall.Getpgrp() == os.Getpid() {
			syscall.Kill(0, syscall.SIGKILL)
		}
		return 0
	}
	if len(args) != 1 || (args[0] != "claude" && args[0] != "codex") {
		return 0
	}
	cmd := exec.Command(self[0], append(append([]string(nil), self[1:]...), "-reporter", args[0])...)
	cmd.Stdin = os.Stdin // an *os.File: the reporter gets the descriptor; this process never reads it
	// Its own process group, so the deadline kill also reaches what the reporter (rex) started.
	// The parent stays this process, so the pid chain to the pane does not change.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Start() != nil {
		return 0
	}
	// Poll and reap here, not with cmd.Wait: until this process reaps the reporter, its pid (and so
	// its group ID) cannot be reused, so the deadline kill cannot reach another process.
	pid := cmd.Process.Pid
	var status syscall.WaitStatus
	for deadline := time.Now().Add(hookDeadline); ; time.Sleep(20 * time.Millisecond) {
		if wpid, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); wpid == pid || err != nil {
			return 0
		}
		if time.Now().After(deadline) {
			syscall.Kill(-pid, syscall.SIGKILL)
			syscall.Wait4(pid, &status, 0, nil)
			return 0
		}
	}
}

// hookReport is the reporter. On success it does not return: it becomes `rex ... do
// hub.identity.report`.
func hookReport(kind string) {
	if kind != "claude" && kind != "codex" {
		return
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, hookInputLimit+1))
	if err != nil || len(data) > hookInputLimit {
		return
	}
	var event map[string]any
	if json.Unmarshal(data, &event) != nil || event == nil {
		return
	}
	var source string
	switch event["hook_event_name"] {
	case "SessionStart":
		source, _ = event["source"].(string)
	case "UserPromptSubmit":
		if kind != "claude" {
			return
		}
		source = "prompt"
	default:
		return
	}
	conversation, _ := event["session_id"].(string)
	if conversation == "" || strings.ContainsRune(conversation, 0) || !hookSources[source] {
		return
	}
	bin := os.Getenv("REX_BIN")
	if bin == "" {
		bin = "rex"
	}
	rex, err := exec.LookPath(bin)
	if err != nil {
		return
	}
	server := os.Getenv("REX_SERVER")
	// Rex keeps lost clients during reconnection. Select one connected hub explicitly.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	ls := exec.CommandContext(ctx, rex, "-S", server, "--autostart=false", "--timeout", "1s", "client", "ls", "--json")
	ls.WaitDelay = 100 * time.Millisecond // a child that keeps the output pipe open does not hold the reporter
	out, err := ls.Output()
	if err != nil {
		return
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
	if json.Unmarshal(out, &listing) != nil {
		return
	}
	var hubs []string
	for _, c := range listing.Clients {
		if c.Info.Label == "lazyrex-hub" && c.ConnectionState == "connected" {
			hubs = append(hubs, c.ClientID)
		}
	}
	if len(hubs) != 1 {
		return
	}
	report, err := json.Marshal(map[string]string{"kind": kind, "conversation_id": conversation, "source": source,
		"block": os.Getenv("REX_BLOCK")})
	if err != nil {
		return
	}
	argv := []string{rex, "-S", server, "--autostart=false", "--timeout", "2s", "-C", hubs[0], "do", "hub.identity.report",
		"--args", string(report)}
	// Exec keeps the reporter's pid and start time across the Rex call. Rex gets no input and its
	// output goes nowhere.
	quiet, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return
	}
	for _, fd := range []int{0, 1, 2} {
		if unix.Dup2(int(quiet.Fd()), fd) != nil {
			return
		}
	}
	syscall.Exec(rex, argv, os.Environ())
}

// inRexPane gates both the hook and the reporter (which the CLI can also run): never let Rex
// choose the user's default server for a hook outside a Rex pane.
func inRexPane() bool {
	for _, k := range []string{"REX_SERVER", "REX_SESSION", "REX_BLOCK"} {
		if os.Getenv(k) == "" {
			return false
		}
	}
	return true
}
