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

// main reads and unsets both variables, so no process that the hub starts inherits them. A ready
// fd that is not a number above 2 is an error.
func TestTakeRestartEnv(t *testing.T) {
	t.Setenv("LAZYREX_READY_FD", "7")
	t.Setenv("LAZYREX_EXPECT_STATE", "/s")
	e, err := takeRestartEnv()
	if err != nil || e.readyFD != 7 || e.expectState != "/s" {
		t.Fatalf("%+v, %v", e, err)
	}
	for _, v := range []string{"LAZYREX_READY_FD", "LAZYREX_EXPECT_STATE"} {
		if _, ok := os.LookupEnv(v); ok {
			t.Errorf("%s is still set", v)
		}
	}
	if e, err := takeRestartEnv(); err != nil || e.readyFD != -1 || e.expectState != "" {
		t.Errorf("without the variables: %+v, %v", e, err)
	}
	for _, bad := range []string{"x", "2", "-1"} {
		t.Setenv("LAZYREX_READY_FD", bad)
		if _, err := takeRestartEnv(); err == nil {
			t.Errorf("LAZYREX_READY_FD=%s: no error", bad)
		}
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
	signalReady(fd, "/private/state")
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "ready /private/state\n" {
		t.Fatalf("%q, %v", b, err)
	}
	signalReady(-1, "/x") // must not panic
}

// hub.info: the canonical state, the pid and the generation, for any client; an argument is
// refused.
func TestHubInfo(t *testing.T) {
	h, _ := workerTestHub(t)
	call := func(args string) (any, error) {
		var p perform
		json.Unmarshal([]byte(fmt.Sprintf(`{"action":"hub.info","args":%s,"requester":{"client_id":"c9","principal":{"pid":40}}}`, args)), &p)
		return h.dispatch(context.Background(), p)
	}
	want := map[string]any{"state": h.cfg.State, "pid": os.Getpid(), "generation": revString(h.generation)}
	if out, err := call(`{}`); err != nil || !reflect.DeepEqual(out, want) {
		t.Fatalf("%v, %v; want %v", out, err, want)
	}
	if _, err := call(`{"x":1}`); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("argument: %v", err)
	}
}

// A hub whose config state is not the wrapper's state exits 3 before any side effect of start: no
// hub.lock, hub.db or events.jsonl in either state, and a missing state is not made. The hub
// exits before it connects to Rex, so the test needs no server.
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
	for _, state := range []string{other, filepath.Join(tmp, "missing")} {
		cfg := filepath.Join(tmp, "hub.json")
		b, _ := json.Marshal(map[string]any{"server": "unix:///nonexistent/rex.sock", "state": state,
			"agents": []map[string]any{{"id": "user", "name": "User", "status_record": ""}}})
		os.WriteFile(cfg, b, 0o600)
		cmd := exec.Command(bin, "-config", cfg)
		cmd.Env = append(os.Environ(), "LAZYREX_EXPECT_STATE="+canon)
		out, _ := cmd.CombinedOutput()
		if code := cmd.ProcessState.ExitCode(); code != exitStateMismatch {
			t.Errorf("state %s: exit %d, want %d\n%s", state, code, exitStateMismatch, out)
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
