package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rexLab starts a lab with only its Rex server (o.startLab, no hub) for a Go lab test that runs an
// in-process Hub against a real server. Lab only: HARNESS_LAB=1 and the rex CLI. The test binary
// is the keeper (TestMain, labKeeperEnv), so nothing is built. The cleanup stops the server and
// ends the lab dir with endGoLab: a passed test removes it, a failed one keeps it.
//
// The test's REX_SERVER is the lab server, so no code in the test process can reach the user's
// real server through the env (hook.go reads REX_SERVER). Each test also sets its Hub's
// cfg.Server to l.server.
func rexLab(t *testing.T) *lab {
	t.Helper()
	if os.Getenv("HARNESS_LAB") != "1" {
		t.Skip("set HARNESS_LAB=1 to run the lab tests")
	}
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
	m.Test.Agents, m.Test.Args, m.Test.WatchRoots, m.Test.Env = nil, nil, nil, nil
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
