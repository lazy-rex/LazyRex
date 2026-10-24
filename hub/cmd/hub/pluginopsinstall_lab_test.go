package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// installLab is a lab hub whose git reaches the repositories repos as github:lab/<name> through
// url.insteadOf in the lab's own git config: no network. An offline name points to no repository.
func installLab(t *testing.T, repos map[string]*testRepo, offline ...string) *startLab {
	t.Helper()
	needLab(t)
	s := startLabWith(t, nil, false, labGitEnv)
	s.labRepos(repos, offline...)
	return s
}

// labGitEnv is the lab environment whose git reads the lab's own git config (labRepos).
var labGitEnv = map[string]string{"GIT_CONFIG_GLOBAL": "${LAB}/gitconfig", "GIT_CONFIG_NOSYSTEM": "1"}

// labRepos writes the git config of a lab started with labGitEnv: github:lab/<name> is the
// repository repos[name], through url.insteadOf. An offline name points to no repository.
func (s *startLab) labRepos(repos map[string]*testRepo, offline ...string) {
	t := s.t
	t.Helper()
	text := ""
	for name, r := range repos {
		u := r.url()
		if slices.Contains(offline, name) {
			u = "file://" + filepath.Join(s.l.dir, "offline", name)
		}
		text += fmt.Sprintf("[url %q]\n\tinsteadOf = https://github.com/lab/%s\n", u, name)
	}
	if err := os.WriteFile(filepath.Join(s.l.dir, "gitconfig"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// labPluginRepo is a repository with the start fixture of name at main.
func labPluginRepo(t *testing.T, name string, depends ...string) (*testRepo, string) {
	t.Helper()
	r := newTestRepo(t)
	return r, r.commit(map[string]string{"lazyrex.json": manifestJSON(name, depends...), "main.lua": fmt.Sprintf(startFixture, name)})
}

// installOps are the install ops of the hub, newest first.
func installOps(s *startLab, agent string) []map[string]any {
	s.t.Helper()
	v, err := s.agentDo(agent, "hub.plugins.ops", map[string]any{})
	if err != nil {
		s.t.Fatal(err)
	}
	var out []map[string]any
	for _, o := range v["ops"].([]any) {
		if m := o.(map[string]any); m["op"] == "install" {
			out = append(out, m)
		}
	}
	return out
}

// labEntry is the hub.plugins entry of the managed id.
func labEntry(s *startLab, id string) map[string]any {
	s.t.Helper()
	for _, e := range s.must("hub.plugins", nil)["entries"].([]any) {
		if m := e.(map[string]any); m["managed"] == id {
			return m
		}
	}
	return nil
}

// A fresh hub (lead-c2i3 b): the managed file lists a, which depends on b, before b, and neither
// has a lock entry or a cache. ONE hub start installs both (origin hub start, each at its ref)
// and runs both, with no reload. Lab only: LAZYREX_LAB=1.
func TestPluginInstallFreshLab(t *testing.T) {
	gitTestEnv(t)
	ra, ca := labPluginRepo(t, "a", "b")
	rb, cb := labPluginRepo(t, "b")
	s := installLab(t, map[string]*testRepo{"a": ra, "b": rb})
	s.writeManagedFile(`{"plugins":[{"id":"a","source":"github:lab/a@main"},{"id":"b","source":"github:lab/b@main"}]}`)
	s.start()
	s.registered("b")
	s.registered("a")
	alice := s.opsAgent("alice")
	ops := installOps(s, alice)
	if len(ops) != 2 {
		t.Fatalf("the install ops: %v", ops)
	}
	for _, op := range ops {
		if op["phase"] != "done" || fmt.Sprint(op["origin"]) != "map[id:start kind:hub]" {
			t.Errorf("an install op: %v", op)
		}
	}
	lock := readTestLockFile(t, s)
	if lock.Plugins["github:lab/a@main"] == nil || lock.Plugins["github:lab/a@main"].Commit != ca || lock.Plugins["github:lab/b@main"] == nil || lock.Plugins["github:lab/b@main"].Commit != cb {
		t.Fatalf("the lock: %+v", lock.Plugins)
	}
}

// b's install fails (its repository is offline): b's row reports "install failed: <op error>",
// and a, which depends on b, does not run (lead-c2i3 c). Lab only: LAZYREX_LAB=1.
func TestPluginInstallOfflineLab(t *testing.T) {
	gitTestEnv(t)
	ra, _ := labPluginRepo(t, "a", "b")
	rb, _ := labPluginRepo(t, "b")
	s := installLab(t, map[string]*testRepo{"a": ra, "b": rb}, "b")
	s.writeManagedFile(`{"plugins":[{"id":"a","source":"github:lab/a@main"},{"id":"b","source":"github:lab/b@main"}]}`)
	s.start()
	alice := s.opsAgent("alice")
	s.waitLong("both installs end", 120e9, func() bool {
		ops := installOps(s, alice)
		return len(ops) == 2 && opFinal[ops[0]["phase"].(string)] && opFinal[ops[1]["phase"].(string)]
	})
	for _, op := range installOps(s, alice) {
		want := map[string]string{"managed:a": "done", "managed:b": "failed"}[op["entry"].(string)]
		if op["phase"] != want {
			t.Errorf("the install of %v: %v, want %s", op["entry"], op, want)
		}
	}
	var eb map[string]any
	s.wait("b's row reports the install", func() bool {
		eb = labEntry(s, "b")
		e, _ := eb["error"].(string)
		return strings.HasPrefix(e, "install failed: resolve managed:b")
	})
	ea := labEntry(s, "a")
	t.Logf("a after b's failed install: state %v, error %v", ea["state"], ea["error"])
	if ea["state"] == "running" || ea["registered"] == true || !strings.Contains(fmt.Sprint(ea["error"]), "b") {
		t.Errorf("a with b not installed: %v", ea)
	}
	if eb["state"] != "refused" {
		t.Errorf("b: %v", eb)
	}
}

// A lock entry with a cleared cache (lead-c2inst; lead-c2i3 a): the hub restarts after main moved
// ahead in the repository. The install fetches the LOCKED commit, not the tip; a disabled entry
// gets its cache back and stays desired stopped and not running. Lab only: LAZYREX_LAB=1.
func TestPluginInstallCacheLab(t *testing.T) {
	gitTestEnv(t)
	rn, cn := labPluginRepo(t, "n")
	rd, cd := labPluginRepo(t, "d")
	s := installLab(t, map[string]*testRepo{"n": rn, "d": rd})
	s.writeManagedFile(`{"plugins":[{"id":"n","source":"github:lab/n@main"},{"id":"d","source":"github:lab/d@main"}]}`)
	s.start()
	s.registered("n")
	s.registered("d")
	alice := s.opsAgent("alice")
	for _, id := range []string{"n", "d"} {
		if op := s.opDone(alice, "stop-"+id, map[string]any{"op": "stop", "entry": "managed:" + id}); op["phase"] != "done" {
			t.Fatalf("stop %s: %v", id, op)
		}
	}
	first := len(installOps(s, alice))
	s.stop()
	removeTree(filepath.Join(s.l.dir, "hub", "plugins", "github"))
	rn.commit(map[string]string{"lazyrex.json": manifestJSON("n"), "main.lua": fmt.Sprintf(startFixture, "n") + "-- 2\n"})
	rd.commit(map[string]string{"lazyrex.json": manifestJSON("d"), "main.lua": fmt.Sprintf(startFixture, "d") + "-- 2\n"})
	s.start()
	// The ops list is newest first: the installs of the restart are before the first ones.
	s.waitLong("the installs of the restart end", 120e9, func() bool {
		ops := installOps(s, alice)
		return len(ops) == first+2 && ops[0]["phase"] == "done" && ops[1]["phase"] == "done"
	})
	for _, op := range installOps(s, alice)[:2] {
		t.Logf("install %v: %v", op["entry"], op["result"])
	}
	lock := readTestLockFile(t, s)
	if lock.Plugins["github:lab/n@main"].Commit != cn || lock.Plugins["github:lab/d@main"].Commit != cd {
		t.Fatalf("the lock after the installs: %+v", lock.Plugins)
	}
	// d stays disabled (the start after the installs binds its record); n starts at the locked
	// commit when the user starts it (an agent start of GitHub code needs the user's confirm).
	s.wait("d's record is stopped", func() bool { e := labEntry(s, "d"); return e["state"] == "stopped" && e["desired"] == "stopped" })
	if st := s.status("d"); st.Registered {
		t.Errorf("d after the install: %+v", st)
	}
	s.wait("n's record is stopped", func() bool { return labEntry(s, "n")["state"] == "stopped" })
	ln := lock.Plugins["github:lab/n@main"]
	s.must("hub.admin.start", map[string]any{"plugin": "n", "expect_run": map[string]any{"source": "github:lab/n@main", "commit": ln.Commit, "digest": ln.Digest}})
	s.registered("n")
	if e := labEntry(s, "n"); e["commit"] != cn {
		t.Errorf("n runs %v, want the locked commit %s", e["commit"], cn)
	}
	if st := s.status("d"); st.Registered {
		t.Errorf("d runs: %+v", st)
	}
}

// readTestLockFile reads the lab hub's lock (next to its config).
func readTestLockFile(t *testing.T, s *startLab) *pluginLock {
	t.Helper()
	l, err := readLock(lockPath(s.cfg, ""))
	if err != nil {
		t.Fatal(err)
	}
	return l
}
