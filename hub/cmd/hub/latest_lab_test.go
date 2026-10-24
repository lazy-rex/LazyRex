package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
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

// latestOpts fetches github:lab/<repo> from the lab repository of that name.
func latestOpts(repos map[string]*testRepo) pluginCmdOpts {
	return pluginCmdOpts{remote: func(owner, repo string) string { return repos[repo].url() }}
}

// entryBySource returns the hub.plugins entry of a source.
func (s *startLab) entryBySource(src string) map[string]any {
	s.t.Helper()
	list, _ := s.must("hub.plugins", nil)["entries"].([]any)
	for _, x := range list {
		if e, _ := x.(map[string]any); e["source"] == src {
			return e
		}
	}
	s.t.Fatalf("no hub.plugins entry for %s", src)
	return nil
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
	var out bytes.Buffer
	if ok, err := runPluginStatus(s.cfg, &out); !ok || err != nil || !strings.Contains(out.String(), src+"\tref latest v1.1.0\tcommit "+c2+"\tcache ok\t") {
		t.Errorf("status: %v %v\n%s", ok, err, out.String())
	}
}

// A legacy entry (ref latest of kind branch, from before @latest) refuses only its plugin: the
// local plugin and the other GitHub plugin register (lead C).
func TestLatestLegacyLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	rl, _ := latestRepo(t, "lgh")
	rb, _ := latestRepo(t, "bgh")
	const legacy, branch = "github:lab/lgh@latest", "github:lab/bgh@main"
	opts := latestOpts(map[string]*testRepo{"lgh": rl, "bgh": rb})
	// The legacy entry: a branch named latest, synced as @latest by a hub before the change.
	rl.git("branch", "latest")
	s.setConfig("plugins", []any{map[string]any{"source": "src/a"}, map[string]any{"source": "github:lab/lgh@refs/heads/latest"}, map[string]any{"source": branch}})
	if err := pluginSync(context.Background(), s.cfg, opts); err != nil {
		t.Fatal(err)
	}
	lp := lockPath(s.cfg, "")
	b, err := os.ReadFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte(`"github:lab/lgh@refs/heads/latest"`), []byte(`"`+legacy+`"`), 1)
	b = bytes.Replace(b, []byte(`"ref": "refs/heads/latest"`), []byte(`"ref": "latest"`), 1)
	if err := os.WriteFile(lp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if le, err := readLock(lp); err != nil || le.Plugins[legacy] == nil || le.Plugins[legacy].Ref != "latest" || le.Plugins[legacy].RefKind != "branch" {
		t.Fatalf("the legacy lock: %v %s", err, b)
	}
	s.setConfig("plugins", []any{map[string]any{"source": "src/a"}, map[string]any{"source": legacy}, map[string]any{"source": branch}})

	s.start()
	s.startRun("a")
	s.startRun("bgh")
	if e := s.entryBySource(legacy); e["state"] != "refused" || !strings.Contains(fmt.Sprint(e["error"]), "@latest now means the highest version tag vX.Y.Z") {
		t.Errorf("the legacy entry: %v", e)
	}
}
