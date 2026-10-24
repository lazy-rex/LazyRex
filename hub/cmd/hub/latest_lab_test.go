package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Lab tests of @latest (docs/plugin-sources.md, sections 4 to 6). Lab only: LAB=1.

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

// A hub runs an @latest source at its release tag. A new release, plugin update and
// hub.plugins.reload give a candidate with the new commit and tag; the instance keeps its run.
// hub.admin.replace with the old run is candidate_changed; with the new run (source, commit,
// digest) it replaces the instance, and the entry reports the new tag with no candidate.
func TestLatestLab(t *testing.T) {
	s := newStartLab(t)
	r, _ := latestRepo(t, "gh")
	r.git("tag", "v1.0.0")
	c1 := r.git("rev-parse", "HEAD")
	r.commit(map[string]string{"notes.txt": "tip"}) // the branch tip is not a release
	const src = "github:lab/gh@latest"
	s.setConfig("plugins", []any{map[string]any{"source": src}})
	opts := latestOpts(map[string]*testRepo{"gh": r})
	if err := pluginSync(context.Background(), s.cfg, opts); err != nil {
		t.Fatal(err)
	}
	lock, err := readLock(lockPath(s.cfg, ""))
	if err != nil {
		t.Fatal(err)
	}
	if le := lock.Plugins[src]; le.RefKind != "latest" || le.Tag != "v1.0.0" || le.Commit != c1 {
		t.Fatalf("lock %+v", le)
	}
	d1 := lock.Plugins[src].Digest

	s.start()
	run, start := s.startRun("gh")
	if e := s.expectEntry("gh"); e["tag"] != "v1.0.0" || e["commit"] != c1 || e["candidate"] != nil {
		t.Fatalf("the first run: %v", e)
	}

	c2 := r.commit(map[string]string{"main.lua": fmt.Sprintf(startFixture, "gh") + "-- 1.1\n"})
	r.git("tag", "-a", "-m", "release", "v1.1.0")
	var out bytes.Buffer
	if err := runPluginCmd(context.Background(), "update", s.cfg, nil, opts, &out); err != nil || !strings.Contains(out.String(), "updated "+src) {
		t.Fatalf("update: %q %v", out.String(), err)
	}
	lock, _ = readLock(lockPath(s.cfg, ""))
	d2 := lock.Plugins[src].Digest
	s.must("hub.plugins.reload", nil)
	e := s.expectEntry("gh")
	c, _ := e["candidate"].(map[string]any)
	if e["tag"] != "v1.0.0" || e["commit"] != c1 || c["tag"] != "v1.1.0" || c["commit"] != c2 {
		t.Fatalf("after the update and reload: %v", e)
	}
	s.same("gh", run, start)

	_, err = s.call("hub.admin.replace", map[string]any{"plugin": "gh", "expect_instance": run.InstanceID,
		"expect_run": map[string]any{"source": src, "commit": c1, "digest": d1}})
	s.refusedWith("replace with the old run", err, "candidate_changed", "the candidate is "+src+" at "+c2, "not "+src+" at "+c1)
	s.same("gh", run, start)
	res, err := s.call("hub.admin.replace", map[string]any{"plugin": "gh", "expect_instance": run.InstanceID,
		"expect_run": map[string]any{"source": src, "commit": c2, "digest": d2}})
	if err != nil {
		t.Fatalf("replace with the new run: %v", err)
	}
	id, _ := res["instance_id"].(string)
	s.registeredAs("gh", id)
	if e := s.expectEntry("gh"); e["tag"] != "v1.1.0" || e["commit"] != c2 || e["candidate"] != nil {
		t.Errorf("after the replace: %v", e)
	}
	out.Reset()
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
