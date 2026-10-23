package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// main reads and unsets the wrapper's variables, so no process that the hub starts inherits them. A
// ready fd that is not a number above 2 is an error, and so is an exec count that is not a number
// of 0 or more, a wrapper's child without the paths, or a path variable without a wrapper.
func TestTakeRestartEnv(t *testing.T) {
	t.Setenv("LAZYREX_READY_FD", "7")
	t.Setenv("LAZYREX_EXPECT_STATE", "/s")
	t.Setenv("LAZYREX_EXEC", "3")
	t.Setenv("LAZYREX_SDK", "/v1/sdk/lua")
	t.Setenv("LAZYREX_VIEW", "") // none
	e, err := takeRestartEnv()
	if err != nil || e.readyFD != 7 || e.expectState != "/s" || e.exec != 3 || e.sdk != "/v1/sdk/lua" || e.view != "" {
		t.Fatalf("%+v, %v", e, err)
	}
	for _, v := range []string{"LAZYREX_READY_FD", "LAZYREX_EXPECT_STATE", "LAZYREX_EXEC", "LAZYREX_SDK", "LAZYREX_VIEW"} {
		if _, ok := os.LookupEnv(v); ok {
			t.Errorf("%s is still set", v)
		}
	}
	if e, err := takeRestartEnv(); err != nil || e.readyFD != -1 || e.expectState != "" || e.exec != 0 {
		t.Errorf("without the variables: %+v, %v", e, err)
	}
	for _, bad := range []string{"x", "2", "-1"} {
		t.Setenv("LAZYREX_READY_FD", bad)
		if _, err := takeRestartEnv(); err == nil {
			t.Errorf("LAZYREX_READY_FD=%s: no error", bad)
		}
	}
	for _, bad := range []string{"x", "-1", "1.5", ""} {
		t.Setenv("LAZYREX_EXEC", bad)
		if _, err := takeRestartEnv(); err == nil {
			t.Errorf("LAZYREX_EXEC=%q: no error", bad)
		}
	}
	// The paths: a wrapper's child without one of them, or one of them without a wrapper, fails.
	t.Setenv("LAZYREX_EXPECT_STATE", "/s")
	t.Setenv("LAZYREX_SDK", "")
	if _, err := takeRestartEnv(); err == nil || !strings.Contains(err.Error(), "needs LAZYREX_SDK and LAZYREX_VIEW") {
		t.Errorf("a wrapper's child without LAZYREX_VIEW: %v", err)
	}
	t.Setenv("LAZYREX_VIEW", "/v1/view")
	if _, err := takeRestartEnv(); err == nil || !strings.Contains(err.Error(), "only from a restart wrapper") {
		t.Errorf("LAZYREX_VIEW without a wrapper: %v", err)
	}
	if _, ok := os.LookupEnv("LAZYREX_VIEW"); ok {
		t.Error("a refused LAZYREX_VIEW is still set")
	}
}

// The wrapper's state and the config state are compared in their canonical form. Another state, or
// a state that does not exist, fails, and the check makes no directory.
func TestCheckExpectedState(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "state")
	os.Mkdir(real, 0o700)
	link := filepath.Join(dir, "link")
	os.Symlink(real, link)
	canon, err := canonicalState(real)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := checkExpectedState(link, canon); err != nil || got != canon {
		t.Errorf("a symlink to the owned state: %q, %v", got, err)
	}
	other := filepath.Join(dir, "other")
	os.Mkdir(other, 0o700)
	if _, err := checkExpectedState(other, canon); err == nil || !strings.Contains(err.Error(), "start a new wrapper") {
		t.Errorf("another state: %v", err)
	}
	missing := filepath.Join(dir, "missing")
	if _, err := checkExpectedState(missing, canon); err == nil || !strings.Contains(err.Error(), "cannot be resolved") {
		t.Errorf("a missing state: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the check made the missing state: %v", err)
	}
}

// The ready line carries the canonical state; the fd is closed after it, so the reader gets EOF.
// No fd: nothing happens.
func TestSignalReady(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	fd, err := syscall.Dup(int(w.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := signalReady(fd, "/private/state"); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "ready /private/state\n" {
		t.Fatalf("%q, %v", b, err)
	}
	signalReady(-1, "/x") // must not panic
}

// hub.info: the canonical state, the pid, the generation, the wrapper's exec count, the build
// commit, and the SDK and view paths that the hub resolved at its start, for any client; an
// argument is refused.
func TestHubInfo(t *testing.T) {
	h, _ := workerTestHub(t)
	h.exec = 2
	h.cfg.SDK, h.cfg.View = "/v1/sdk/lua", "/v1/lazyrex-view"
	call := func(args string) (any, error) {
		var p perform
		json.Unmarshal([]byte(fmt.Sprintf(`{"action":"hub.info","args":%s,"requester":{"client_id":"c9","principal":{"pid":40}}}`, args)), &p)
		return h.dispatch(context.Background(), p)
	}
	commit, modified := buildCommit()
	want := map[string]any{"state": h.cfg.State, "pid": os.Getpid(), "generation": revString(h.generation),
		"exec": 2, "commit": commit, "modified": modified, "sdk": "/v1/sdk/lua", "view": "/v1/lazyrex-view"}
	if out, err := call(`{}`); err != nil || !reflect.DeepEqual(out, want) {
		t.Fatalf("%v, %v; want %v", out, err, want)
	}
	if _, err := call(`{"x":1}`); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("argument: %v", err)
	}
}

// A hub whose config state is not the wrapper's state exits 3 before any side effect of start: no
// hub.lock, hub.db or events.jsonl in either state, and a missing state is not made. The hub
// exits before it connects to Rex, so the test needs no server. The state check comes before the
// check of the pinned paths: with an SDK that is gone, the exit is still 3 (the wrapper stops), not
// 1 (the wrapper would restart in backoff without end).
func TestHubExpectStateMismatchExits3(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	owned := filepath.Join(tmp, "owned")
	os.Mkdir(owned, 0o700)
	canon, _ := canonicalState(owned)
	other := filepath.Join(tmp, "other")
	os.Mkdir(other, 0o700)
	gone := filepath.Join(tmp, "gone", "sdk", "lua")
	for _, c := range []struct{ state, sdk string }{{other, ""}, {filepath.Join(tmp, "missing"), ""}, {other, gone}} {
		state := c.state
		cfg := filepath.Join(tmp, "hub.json")
		b, _ := json.Marshal(map[string]any{"server": "unix:///nonexistent/rex.sock", "state": state,
			"agents": []map[string]any{{"id": "user", "name": "User", "status_record": ""}}})
		os.WriteFile(cfg, b, 0o600)
		cmd := exec.Command(bin, "-config", cfg)
		cmd.Env = append(os.Environ(), "LAZYREX_EXPECT_STATE="+canon, "LAZYREX_SDK="+c.sdk, "LAZYREX_VIEW=")
		out, _ := cmd.CombinedOutput()
		if code := cmd.ProcessState.ExitCode(); code != exitStateMismatch {
			t.Errorf("state %s, sdk %q: exit %d, want %d\n%s", state, c.sdk, code, exitStateMismatch, out)
		}
		if !strings.Contains(string(out), "start a new wrapper") {
			t.Errorf("state %s: text %q", state, out)
		}
		for _, dir := range []string{state, owned} {
			for _, f := range []string{"hub.lock", "hub.db", "events.jsonl"} {
				if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
					t.Errorf("state %s: %s exists in %s", state, f, dir)
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "missing")); !os.IsNotExist(err) {
		t.Errorf("the hub made the missing state: %v", err)
	}
}

// A failed ready write is reported (main logs it once): a pipe without a reader. The fd is closed
// after the try.
func TestSignalReadyReportsAFailedWrite(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Dup(int(w.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	r.Close()
	if err := signalReady(fd, "/s"); err == nil {
		t.Error("a pipe without a reader: no error")
	}
	if err := signalReady(-1, "/s"); err != nil {
		t.Errorf("no fd: %v", err)
	}
}

// A hub that no wrapper started refuses to start while a restart wrapper owns the state (in the
// wrapper's backoff wait or exec gap it would take the state). It makes no hub.lock. A wrapper's
// own child (LAZYREX_EXPECT_STATE) is not refused, and with no wrapper the start goes on (here it
// fails later, at the missing Rex server).
func TestHubRefusesToStartBesideAWrapper(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	state, _ := canonicalState(t.TempDir())
	cfg := filepath.Join(tmp, "hub.json")
	b, _ := json.Marshal(map[string]any{"server": "unix:///nonexistent/rex.sock", "state": state,
		"agents": []map[string]any{{"id": "user", "name": "User", "status_record": ""}}})
	os.WriteFile(cfg, b, 0o600)
	run := func(env ...string) (string, int) {
		cmd := exec.Command(bin, "-config", cfg)
		cmd.Env = append(os.Environ(), env...)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	if out, _ := run(); strings.Contains(out, "restart wrapper owns") {
		t.Errorf("no wrapper: refused\n%s", out)
	}
	os.Remove(filepath.Join(state, "hub.lock"))
	holder := exec.Command(os.Args[0], "-test.run=^TestFakeHub$")
	holder.Env = append(os.Environ(), "FAKE_HUB_PLAN=lock", "FAKE_LOCK_STATE="+state)
	pipe, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	line := make([]byte, 7)
	if _, err := pipe.Read(line); err != nil || string(line) != "locked\n" {
		t.Fatalf("holder: %q, %v", line, err)
	}
	out, code := run()
	want := fmt.Sprintf("a restart wrapper owns %s (pid %d); use lazyrex-hub restart or stop", state, holder.Process.Pid)
	if code != 1 || !strings.Contains(out, want) {
		t.Errorf("beside a wrapper: exit %d, want 1 and %q\n%s", code, want, out)
	}
	if _, err := os.Stat(filepath.Join(state, "hub.lock")); !os.IsNotExist(err) {
		t.Errorf("the refused hub made hub.lock: %v", err)
	}
	if out, _ := run("LAZYREX_EXPECT_STATE="+state, "LAZYREX_SDK=", "LAZYREX_VIEW="); strings.Contains(out, "restart wrapper owns") {
		t.Errorf("the wrapper's own child: refused\n%s", out)
	}
}
