package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Lab tests of @latest (docs/plugin-sources.md, sections 4 to 6). Lab only: HARNESS_LAB=1.

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

// oldHub is the hub binary of LAZYREX_OLD_HUB: a clean build of a revision before @latest. The
// test logs its revision and build metadata.
func oldHub(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("LAZYREX_OLD_HUB")
	if bin == "" {
		t.Skip("LAZYREX_OLD_HUB is not set")
	}
	out, err := exec.Command("go", "version", "-m", bin).Output()
	if err != nil {
		t.Fatalf("go version -m %s: %v", bin, err)
	}
	if !strings.Contains(string(out), "\tvcs.modified=false\n") {
		t.Fatalf("%s is not a clean build:\n%s", bin, out)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "vcs.revision=") || strings.Contains(l, "vcs.modified=") || strings.Contains(l, "\tpath\t") || strings.HasPrefix(l, bin) {
			t.Logf("old hub: %s", strings.TrimSpace(l))
		}
	}
	return bin
}

// oldCmd runs `<old hub> plugin <args>` with a git config, for this process only, that maps each
// github.com/lab/<repo> URL to its lab repository.
func oldCmd(t *testing.T, bin string, repos map[string]*testRepo, args ...string) (string, error) {
	t.Helper()
	gc := filepath.Join(t.TempDir(), "gitconfig")
	var b strings.Builder
	for name, r := range repos {
		fmt.Fprintf(&b, "[url %q]\n\tinsteadOf = https://github.com/lab/%s\n", r.url(), name)
	}
	if err := os.WriteFile(gc, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, append([]string{"plugin"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+gc, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// suffixed normalizes the random token of each cache dir in the raw lock bytes, after it checks
// that the rest of the dir is github/lab/<repo>/<pinned commit>. No other byte changes.
func suffixed(t *testing.T, raw []byte, lock *pluginLock) []byte {
	t.Helper()
	for src, le := range lock.Plugins {
		re := regexp.MustCompile(`^github/` + regexp.QuoteMeta(le.Owner) + `/` + regexp.QuoteMeta(le.Repo) + `/` + le.Commit + `\.[0-9a-f]{16}$`)
		if le.Owner != "lab" || !re.MatchString(le.Dir) {
			t.Fatalf("%s: the dir %s is not github/lab/%s/%s.<token>", src, le.Dir, le.Repo, le.Commit)
		}
		quoted := []byte(`"dir": "` + le.Dir + `"`)
		if bytes.Count(raw, quoted) != 1 {
			t.Fatalf("%s: the dir %s is not in the lock once", src, le.Dir)
		}
		raw = bytes.Replace(raw, quoted, []byte(`"dir": "github/lab/`+le.Repo+`/`+le.Commit+`.TOKEN"`), 1)
	}
	return raw
}

// The old hub (a revision before @latest) and the files of this one (lead D):
//
//	(a) local plugins in the config, GitHub entries in a version 2 lock (a latest entry and a
//	    branch entry): the old hub refuses the lock with its text, every GitHub entry is refused,
//	    and the local plugin registers;
//	(b) a version 1 lock (no latest entry): the new binary's lock equals the old binary's after the
//	    normalization of the random cache token only; an old sync leaves the new lock unchanged, byte
//	    for byte; the old hub starts from the new lock, and its GitHub entry registers;
//	(c) a version 2 pin document (an export with a latest entry): the old pull refuses it with its
//	    text, and the lock is unchanged.
func TestLatestOldBinaryLab(t *testing.T) {
	bin := oldHub(t)
	rl, _ := latestRepo(t, "lgh")
	rl.git("tag", "v1.0.0")
	rb, _ := latestRepo(t, "bgh")
	repos := map[string]*testRepo{"lgh": rl, "bgh": rb}
	opts := latestOpts(repos)
	const latest, branch = "github:lab/lgh@latest", "github:lab/bgh@main"
	doc, written := filepath.Join(t.TempDir(), "pins-v2.json"), false // the parent dir: it outlives case a

	t.Run("a", func(t *testing.T) {
		s := newStartLab(t, "a")
		s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
		s.setConfig("plugins", []any{map[string]any{"source": "src/a"}, map[string]any{"source": latest}, map[string]any{"source": branch}})
		if err := pluginSync(context.Background(), s.cfg, opts); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(lockPath(s.cfg, "")); !bytes.HasPrefix(b, []byte("{\n  \"version\": 2,")) {
			t.Fatalf("not a version 2 lock:\n%s", b)
		}
		var out, errb bytes.Buffer
		if code := pluginCmdMain([]string{"export", "-config", s.cfg}, &out, &errb); code != 0 || !bytes.HasPrefix(out.Bytes(), []byte("{\n  \"version\": 2,")) {
			t.Fatalf("export: %d %s %s", code, out.String(), errb.String())
		}
		if err := os.WriteFile(doc, out.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		written = true
		s.bin = bin
		s.start()
		s.startRun("a")
		for _, src := range []string{latest, branch} {
			if e := s.entryBySource(src); e["state"] != "refused" || !strings.Contains(fmt.Sprint(e["error"]), "version 2 is not supported (this hub reads version 1)") {
				t.Errorf("%s under the old hub: %v", src, e)
			}
		}
	})
	if !written {
		t.Fatal("case a gave no pin document")
	}

	t.Run("b and c", func(t *testing.T) {
		s := newStartLab(t, "a")
		s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
		s.setConfig("plugins", []any{map[string]any{"source": "src/a"}, map[string]any{"source": branch}})
		lp := lockPath(s.cfg, "")
		if out, err := oldCmd(t, bin, repos, "sync", "-config", s.cfg); err != nil {
			t.Fatalf("old sync: %v\n%s", err, out)
		}
		oldRaw, _ := os.ReadFile(lp)
		oldLock, err := readLock(lp)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(lp); err != nil {
			t.Fatal(err)
		}
		if err := pluginSync(context.Background(), s.cfg, opts); err != nil {
			t.Fatal(err)
		}
		newRaw, _ := os.ReadFile(lp)
		newLock, err := readLock(lp)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(newRaw, []byte("{\n  \"version\": 1,")) {
			t.Fatalf("not a version 1 lock:\n%s", newRaw)
		}
		if a, b := suffixed(t, oldRaw, oldLock), suffixed(t, newRaw, newLock); !bytes.Equal(a, b) {
			t.Fatalf("the locks differ after the token normalization:\nold:\n%s\nnew:\n%s", a, b)
		}
		if out, err := oldCmd(t, bin, repos, "sync", "-config", s.cfg); err != nil {
			t.Fatalf("old sync of the new lock: %v\n%s", err, out)
		}
		if b, _ := os.ReadFile(lp); !bytes.Equal(b, newRaw) {
			t.Fatalf("the old sync changed the new lock:\n%s\nwas:\n%s", b, newRaw)
		}

		// (c) The version 2 pin document of case a.
		out, err := oldCmd(t, bin, repos, "pull", "-config", s.cfg, "-file", doc)
		if err == nil || !strings.Contains(out, "pin document version 2 is not supported by this hub (it reads version 1)") {
			t.Errorf("old pull of a version 2 document: %v\n%s", err, out)
		}
		if b, _ := os.ReadFile(lp); !bytes.Equal(b, newRaw) {
			t.Errorf("the refused pull changed the lock")
		}

		s.bin = bin
		s.start()
		s.startRun("a")
		s.startRun("bgh")
		if e := s.entryBySource(branch); e["state"] != "running" || e["commit"] != newLock.Plugins[branch].Commit {
			t.Errorf("the GitHub entry under the old hub: %v", e)
		}
	})
}
