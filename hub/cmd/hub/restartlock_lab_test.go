package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A crash restart (hub:restart({crash = true})) starts the new hub only after the old hub has
// released hub.lock. On the Linux lab host the old hub could still hold the lock after its keeper
// was reaped, and the new hub then refused with "another hub owns". Here a helper process holds
// hub.lock across the restart, as a slow exit of the old hub does: the restart must wait for it,
// and the new hub must start once the lock is free.
func TestRestartWaitsForHubLockLab(t *testing.T) {
	bin, root := harnessBinary(t)
	rexBin, err := exec.LookPath("rex")
	if err != nil {
		t.Skip("no rex CLI")
	}
	dir := filepath.Join(root, "tests", "harness")
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	home, err := homePlugin(root)
	if err != nil {
		t.Fatal(err)
	}
	o := &testOpts{dir: dir, lazyrex: root, home: home, self: bin, rexBin: rexBin, m: m}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	l, err := o.startLab(ctx)
	if l != nil {
		defer func() {
			for _, p := range l.stop() {
				t.Error(p)
			}
			endGoLab(t, l.dir, l.unproved)
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := o.startHub(ctx, l); err != nil {
		t.Fatal(err)
	}
	oldPID := l.hub.pid

	// The holder takes hub.lock as soon as the old hub's exit releases it, and holds it until the
	// restart has seen the lock held once (restartProbeFailed); the cleanup releases it otherwise.
	var holder *heldLock
	restartAfterStop = func(l *lab) { holder = holdLock(t, filepath.Join(l.dir, "hub", "hub.lock")) }
	restartProbeFailed = func() {
		if holder != nil {
			holder.end()
		}
	}
	defer func() { restartAfterStop, restartProbeFailed = nil, nil }()

	if err := o.restartHub(ctx, l, "kill"); err != nil {
		if strings.Contains(err.Error(), "another hub owns") || strings.Contains(readFile(filepath.Join(l.dir, "hub.log")), "another hub owns") {
			t.Fatalf("the new hub started while hub.lock was still held: %v", err)
		}
		t.Fatalf("restart: %v", err)
	}
	if holder == nil || !holder.ended {
		t.Fatal("the restart did not wait for the held hub.lock")
	}
	if l.hub == nil || l.hub.pid == oldPID {
		t.Fatalf("no new hub: old pid %d", oldPID)
	}
}
