package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// testCLIMain is `lazyrex-hub testagent cli <kind> <conversation> <source> <when> [stop]` (lab
// only): a fake agent CLI that is a program of its own, not a shell, for tests/agent_restart.lua.
// The tests run it through a lab symlink named codex or claude.
//
// Like both CLIs (S1, 2026-10-22) it reports program status idle first. It reports its identity
// from a child process (as a hook does) at its start (when "start") or after its first input line
// (when "prompt"). stop says how it ends: "int" (one Ctrl-C), "int2" (two Ctrl-C within a second),
// "term" (it ignores Ctrl-C), "kill" (it ignores Ctrl-C and SIGTERM), "fgchild" (after its report a
// child in its own foreground process group logs each Ctrl-C). Each input line and each SIGTERM is
// appended to $PWD/fake-<conversation>.log. After the first line, "again" reports once more, "exec"
// executes /bin/sleep in this pid, "stale" starts a reporter that reports 3 s later and then reports at
// once (the older reporter is then stale), and "exit" ends it.
func testCLIMain(argv []string) int {
	if len(argv) < 4 {
		fmt.Fprintln(os.Stderr, "usage: lazyrex-hub testagent cli <kind> <conversation> <source> <when> [stop] (lab only)")
		return 2
	}
	kind, conv, source, when, stop := argv[0], argv[1], argv[2], argv[3], "int"
	if len(argv) > 4 {
		stop = argv[4]
	}
	logf, err := os.OpenFile("fake-"+conv+".log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	note := func(s string) { fmt.Fprintln(logf, s) }
	// The real binary, resolved once: a test moves the symlink this program was started through.
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	args, _ := json.Marshal(map[string]any{"kind": kind, "conversation_id": conv, "source": source, "block": os.Getenv("REX_BLOCK")})
	report := func() { exec.Command(self, "testagent", "report", string(args)).Run() }
	status := func(s string) { fmt.Printf("\x1b]7501;state=%s\x1b\\", s) }

	sigs := make(chan os.Signal, 4)
	switch stop {
	case "term":
		signal.Ignore(syscall.SIGINT)
		signal.Notify(sigs, syscall.SIGTERM)
	case "kill":
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
	case "int2":
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	default:
		signal.Notify(sigs, syscall.SIGTERM)
	}
	go func() {
		var last time.Time
		for s := range sigs {
			if s == syscall.SIGTERM {
				note("signal: TERM")
				os.Exit(0)
			}
			if !last.IsZero() && time.Since(last) <= time.Second {
				os.Exit(0)
			}
			last = time.Now()
		}
	}()

	status("idle")
	if when == "start" {
		report()
	}
	if stop == "fgchild" {
		child := exec.Command("/bin/sh", "-c", `trap 'echo "signal: INT" >> "$1"' INT; while :; do sleep 1; done`, "sh", filepath.Join(".", logf.Name()))
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: int(os.Stdin.Fd())}
		child.Run()
		return 0
	}
	in := bufio.NewScanner(os.Stdin)
	if !in.Scan() {
		return 0
	}
	note("prompt: " + in.Text())
	if in.Text() == "exit" {
		return 0
	}
	if when == "prompt" {
		report()
	}
	status("done")
	for in.Scan() {
		switch line := in.Text(); line {
		case "exit":
			return 0
		case "again":
			report()
		case "stale":
			older := exec.Command(self, "testagent", "report", "-delay", "3000", string(args))
			if older.Start() == nil {
				time.Sleep(300 * time.Millisecond)
				report()
				go older.Wait()
			}
		case "exec":
			syscall.Exec("/bin/sleep", []string{"sleep", "600"}, os.Environ())
		default:
			note("input: " + line)
		}
	}
	return 0
}
