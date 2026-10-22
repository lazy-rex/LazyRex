package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every lab server start path sets REX_SERVER to the lab socket, so that lab panes inherit it: a
// hook without REX_SERVER uses Rex's default endpoint, the user's real server (the hub lead,
// 2026-10-22). labserver.go checks its environment; lab/server.sh sets the variable itself.
func TestLabServerEnv(t *testing.T) {
	const sock = "unix:///tmp/lrt-1/rex.sock"
	for _, c := range []struct {
		name string
		env  []string
		ok   bool
	}{
		{"labEnv", labEnv("/tmp/lrt-1", sock, nil), true},
		{"no REX_SERVER", []string{"PATH=/bin", "HOME=/tmp/lrt-1/home"}, false},
		{"empty REX_SERVER", []string{"REX_SERVER="}, false},
		{"another server", []string{"REX_SERVER=unix:///tmp/lrt-2/rex.sock"}, false},
		{"twice", []string{"REX_SERVER=" + sock, "REX_SERVER=" + sock}, false},
		{"a pane's REX_SESSION", []string{"REX_SERVER=" + sock, "REX_SESSION=session:x"}, false},
		{"a pane's REX_BLOCK", []string{"REX_SERVER=" + sock, "REX_BLOCK=block:x"}, false},
	} {
		if err := labServerEnv(c.env, sock); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// startServer refuses before it starts anything.
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	rexBin := filepath.Join(dir, "rex")
	os.WriteFile(rexBin, []byte("#!/bin/sh\ntouch '"+ran+"'\n"), 0o755)
	l := &lab{dir: dir, server: "unix://" + dir + "/rex.sock", env: []string{"PATH=/bin"}}
	if err := l.startServer(t.Context(), os.Args[0], rexBin); err == nil || !strings.Contains(err.Error(), "REX_SERVER") {
		t.Fatalf("startServer without REX_SERVER: %v", err)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Fatal("startServer ran rex")
	}
}

// lab/server.sh starts rex with REX_SERVER = the lab socket and without the starting pane's
// REX_SESSION and REX_BLOCK, also when it runs in a pane of another server. A fake rex records its
// environment; no server starts.
func TestLabServerShSetsRexServer(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "env")
	os.WriteFile(filepath.Join(dir, "rex"), []byte("#!/bin/sh\nenv | grep '^REX_' | sort > '"+out+"'\n"), 0o755)
	lab := filepath.Join(dir, "lab")
	cmd := exec.Command(filepath.Join(root, "lab", "server.sh"), lab)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "REX_SERVER=unix:///elsewhere/server.sock", "REX_SESSION=session:x", "REX_BLOCK=block:x"}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("server.sh: %v\n%s", err, b)
	}
	if got, want := readFile(out), "REX_SERVER=unix://"+lab+"/rex.sock\n"; got != want {
		t.Fatalf("the server's REX_ variables: %q, want %q", got, want)
	}
}
