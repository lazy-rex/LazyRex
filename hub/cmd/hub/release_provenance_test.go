package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The coordinator ruling r42-provenance-rule (codex-26 found the gap): the freeze takes H from
// main, or from a release branch that the operator names with -branch. No fallback to main.

func TestReleaseHubFreezeBranch(t *testing.T) {
	onBranch := func(w *relWorld) {
		w.commits[commitH].onMain = false
		w.commits[commitH].branches = []string{"release/4.2"}
	}
	freeze := func(w *relWorld, branch string) error {
		createRun(w.run, w.specP)
		r := w.open("freeze")
		defer r.unlock()
		r.branch = branch
		return hubFreeze(r)
	}
	// H only on release/4.2: the named branch passes, and the candidate records it.
	w := newRelWorld(t)
	onBranch(w)
	if err := freeze(w, "release/4.2"); err != nil {
		t.Fatalf("release/4.2: %v", err)
	}
	var c relCandidate
	if err := json.Unmarshal(mustRead(filepath.Join(w.run, relCandidateFile)), &c); err != nil || c.Branch != "release/4.2" {
		t.Fatalf("the candidate branch %q (%v)", c.Branch, err)
	}
	// Without -branch the main rule applies; another branch, or a name outside release/*, refuses.
	for name, tc := range map[string]struct{ branch, want string }{
		"no branch":    {"", "not on origin/main"},
		"other branch": {"release/4.3", "not on origin/release/4.3"},
		"not release":  {"feature/x", "not a release branch"},
		"patch form":   {"release/4.2.1", "not a release branch"},
	} {
		w := newRelWorld(t)
		onBranch(w)
		if err := freeze(w, tc.branch); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(w.run, relCandidateFile)); !os.IsNotExist(err) {
			t.Errorf("%s: a refused freeze wrote a candidate (%v)", name, err)
		}
	}
	// The usage refuses a bad -branch before it makes the run.
	var out, errb bytes.Buffer
	run := filepath.Join(t.TempDir(), "run")
	if r, code := openFreeze("freeze", []string{"-spec", "x.json", "-run", run, "-branch", "main"}, &out, &errb); r != nil || code != relUsage || !strings.Contains(errb.String(), "not a release branch") {
		t.Errorf("-branch main: %v %d %s", r, code, errb.String())
	}
	if _, err := os.Stat(run); !os.IsNotExist(err) {
		t.Errorf("a refused -branch made the run (%v)", err)
	}
	// A plugin release refuses -branch.
	if err := relPluginFreeze(&releaser{branch: "release/4.2"}); err == nil || !strings.Contains(err.Error(), "for a hub release") {
		t.Errorf("plugin freeze: %v", err)
	}
}

// TestReleaseHubProvenanceGit runs the check with real git: an origin with H only on
// release/4.2, and a release clone whose fetch config holds only main.
func TestReleaseHubProvenanceGit(t *testing.T) {
	dir := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=lab", "GIT_AUTHOR_EMAIL=lab@example.invalid", "GIT_COMMITTER_NAME=lab", "GIT_COMMITTER_EMAIL=lab@example.invalid"}
	git := func(d string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", d}, args...)...)
		c.Env = env
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin, work := filepath.Join(dir, "origin.git"), filepath.Join(dir, "work")
	git(dir, "init", "-q", "--bare", "-b", "main", origin)
	git(dir, "init", "-q", "-b", "main", work)
	git(work, "commit", "-q", "--allow-empty", "-m", "a")
	a := git(work, "rev-parse", "HEAD")
	git(work, "commit", "-q", "--allow-empty", "-m", "h")
	h := git(work, "rev-parse", "HEAD")
	git(work, "push", "-q", origin, a+":refs/heads/main", a+":refs/heads/release/4.1", h+":refs/heads/release/4.2")
	rel := filepath.Join(dir, "release")
	git(dir, "clone", "-q", "--single-branch", "--branch", "main", origin, filepath.Join(rel, "LazyRex"))
	clone := filepath.Join(rel, "LazyRex")
	if got := git(clone, "config", "--get-all", "remote.origin.fetch"); got != "+refs/heads/main:refs/remotes/origin/main" {
		t.Fatalf("the clone fetches %q, want main only", got)
	}
	r := &releaser{spec: &relSpec{ReleaseDir: rel}, ctx: context.Background(), env: env}
	check := func(name, commit, branch, want string) {
		t.Helper()
		err := r.hubProvenance(commit, branch)
		if want == "" && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	check("main", a, "", "")
	check("H without -branch", h, "", "not on origin/main")
	check("H on release/4.2", h, "release/4.2", "")
	// A stale remote-tracking ref cannot answer for the branch: the fetch overwrites it.
	git(clone, "update-ref", "refs/remotes/origin/release/4.1", h)
	check("stale release/4.1", h, "release/4.1", "not on origin/release/4.1")
	if got := git(clone, "rev-parse", "refs/remotes/origin/release/4.1"); got != a {
		t.Errorf("origin/release/4.1 is %s after the check, want %s", got, a)
	}
	check("no such branch", h, "release/4.3", "fetching the branch release/4.3")
	check("not release", h, "release/4.2.1", "not a release branch")
}
