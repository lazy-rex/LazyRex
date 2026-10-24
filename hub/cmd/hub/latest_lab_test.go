package main

import (
	"fmt"
	"testing"
)

// Lab tests of @latest (docs/plugin-sources.md, sections 4 to 6). Lab only: LAZYREX_LAB=1.

// latestRepo is a GitHub-shaped repository github:lab/<name> of the plugin <name>, with one commit.
func latestRepo(t *testing.T, name string) (*testRepo, string) {
	t.Helper()
	r := newTestRepo(t)
	c := r.commit(map[string]string{"lazyrex.json": `{"name":"` + name + `","main":"main.lua"}`, "main.lua": fmt.Sprintf(startFixture, name)})
	return r, c
}

// A hub installs an @latest source at its release tag and runs it there. A new release and the
// manager's update of the stopped plugin pin the new commit, and the stopped entry
// expects it. hub.admin.start with the old run is candidate_changed; with the
// new run (source, commit, digest) it starts the new release, and the entry reports the new tag
// with no candidate.
func TestLatestLab(t *testing.T) {
	r, _ := latestRepo(t, "gh")
	r.git("tag", "v1.0.0")
	c1 := r.git("rev-parse", "HEAD")
	r.commit(map[string]string{"notes.txt": "tip"}) // the branch tip is not a release
	const src = "github:lab/gh@latest"
	s := installLab(t, map[string]*testRepo{"gh": r})
	s.writeManagedFile(`{"plugins":[{"id":"gh","source":"` + src + `"}]}`)

	s.start() // the hub installs gh at its start
	s.startRun("gh")
	lock := readTestLockFile(t, s)
	if le := lock.Plugins[src]; le.RefKind != "latest" || le.Tag != "v1.0.0" || le.Commit != c1 {
		t.Fatalf("lock %+v", le)
	}
	d1 := lock.Plugins[src].Digest
	if e := s.expectEntry("gh"); e["tag"] != "v1.0.0" || e["commit"] != c1 || e["candidate"] != nil {
		t.Fatalf("the first run: %v", e)
	}

	c2 := r.commit(map[string]string{"main.lua": fmt.Sprintf(startFixture, "gh") + "-- 1.1\n"})
	r.git("tag", "-a", "-m", "release", "v1.1.0")
	s.must("hub.admin.stop", map[string]any{"plugin": "gh"})
	if op := s.opDone(s.opsAgent("alice"), "up-gh", map[string]any{"op": "update", "entry": "managed:gh", "input": map[string]any{"to": "@"}}); op["phase"] != "done" {
		t.Fatalf("update: %v", op)
	}
	d2 := readTestLockFile(t, s).Plugins[src].Digest
	// A stopped entry has no run: expect_run and pinned_commit name the new release.
	e := s.expectEntry("gh")
	x, _ := e["expect_run"].(map[string]any)
	if _, ok := e["commit"]; ok || x["commit"] != c2 || x["digest"] != d2 || e["pinned_commit"] != c2 {
		t.Fatalf("after the update: %v", e)
	}

	_, err := s.call("hub.admin.start", map[string]any{"plugin": "gh", "expect_run": map[string]any{"source": src, "commit": c1, "digest": d1}})
	s.refusedWith("start with the old run", err, "candidate_changed", "not "+src+" at "+c1)
	if e := s.expectEntry("gh"); e["desired"] != "stopped" {
		t.Errorf("after the refused start: desired %v", e["desired"])
	}
	s.must("hub.admin.start", map[string]any{"plugin": "gh", "expect_run": map[string]any{"source": src, "commit": c2, "digest": d2}})
	s.wait("gh runs v1.1.0", func() bool { e := s.expectEntry("gh"); return s.status("gh").Registered && e["commit"] == c2 })
	if e := s.expectEntry("gh"); e["tag"] != "v1.1.0" || e["commit"] != c2 || e["candidate"] != nil {
		t.Errorf("after the start: %v", e)
	}
}
