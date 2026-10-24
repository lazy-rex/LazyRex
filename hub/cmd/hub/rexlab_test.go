package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// needLab skips a lab test unless LAZYREX_LAB=1 (docs/testing.md, "Go lab tests"). Every lab test of the
// hub calls it; a gate for a documented exception (a known limit, a diagnostic) is separate.
func needLab(t *testing.T) {
	t.Helper()
	if os.Getenv("LAZYREX_LAB") != "1" {
		t.Skip("set LAZYREX_LAB=1 to run the lab tests (each starts its own lab server)")
	}
}

// rexLab starts a lab with only its Rex server (o.startLab, no hub) for a Go lab test that runs an
// in-process Hub against a real server. Lab only: LAZYREX_LAB=1 and the rex CLI. The test binary
// is the keeper (TestMain, labKeeperEnv), so nothing is built. The cleanup stops the server and
// ends the lab dir with endGoLab: a passed test removes it, a failed one keeps it.
//
// The test's REX_SERVER is the lab server, so no code in the test process can reach the user's
// real server through the env (hook.go reads REX_SERVER). Each test also sets its Hub's
// cfg.Server to l.server.
func rexLab(t *testing.T) *lab {
	t.Helper()
	needLab(t)
	rexBin, err := exec.LookPath("rex")
	if err != nil {
		t.Skip("no rex CLI")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "tests", "harness")
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Test.Args, m.Test.WatchRoots, m.Test.Env = nil, nil, nil
	o := &testOpts{dir: dir, lazyrex: root, self: self, rexBin: rexBin, m: m}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l, err := o.startLab(ctx)
	if l != nil {
		t.Cleanup(func() {
			for _, p := range l.stop() {
				t.Error(p)
			}
			endGoLab(t, l.dir, l.unproved)
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l.socket, l.dir+"/") || l.server != "unix://"+l.socket {
		t.Fatalf("the lab socket %s is not inside the lab %s", l.socket, l.dir)
	}
	t.Setenv("REX_SERVER", l.server)
	return l
}

// fdTable is the fd table of the test process: each open fd and its target (readlink). The read's
// own directory fd is in it too, the same in each read.
func fdTable(t *testing.T) map[string]string {
	t.Helper()
	e, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, x := range e {
		l, _ := os.Readlink("/dev/fd/" + x.Name())
		m[x.Name()] = l
	}
	return m
}

// A lab's start and stop leave no fd of the lab in the test process: no file in the lab dir and
// no pidfd of a keeper (Linux). GC is off, so no finalizer closes a leaked fd (the hub lead,
// lead-fdleak4). The subtest ends the lab: its cleanups run before the check.
func TestLabStopLeavesNoFdLab(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	fd0 := fdTable(t)
	var dir string
	t.Run("lab", func(t *testing.T) { dir = rexLab(t).dir })
	if dir == "" {
		t.Skip("no lab")
	}
	for fd, target := range fdTable(t) {
		if fd0[fd] == target {
			continue
		}
		if strings.HasPrefix(target, dir+"/") || target == "anon_inode:[pidfd]" {
			t.Errorf("fd %s of the lab stays open after its stop: %s", fd, target)
		}
	}
}
