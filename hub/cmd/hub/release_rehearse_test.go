package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A-u1: the lab must be a new directory rexlab-release-<name> directly under /tmp; anything else
// refuses before any child.
func TestReleaseCreateLab(t *testing.T) {
	name := fmt.Sprintf("rexlab-release-u%d", os.Getpid())
	path := filepath.Join("/tmp", name)
	defer os.RemoveAll(path)
	lab, err := createLab(path)
	if err != nil {
		t.Fatal(err)
	}
	tmp, _ := filepath.EvalSymlinks("/tmp")
	if lab != filepath.Join(tmp, name) {
		t.Errorf("lab %s", lab)
	}
	if fi, err := os.Stat(lab); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("mode: %v %v", fi.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(lab, "owner.json")); err != nil {
		t.Error(err)
	}
	other := t.TempDir()
	link := filepath.Join("/tmp", name+"-link")
	os.Symlink(other, link)
	defer os.Remove(link)
	for _, p := range []string{
		path,                                          // exists
		filepath.Join(other, "rexlab-release-x"),      // not under /tmp
		"/tmp/other-name",                             // the name
		"/tmp/rexlab-release-",                        // an empty name
		"tmp/rexlab-release-rel",                      // relative
		"/tmp/../tmp/rexlab-release-x",                // not clean
		filepath.Join(link, "rexlab-release-in-link"), // a symlinked parent
		link, // the lab name as a symlink (exists)
	} {
		if _, err := createLab(p); err == nil {
			t.Errorf("createLab(%s) passed", p)
			os.RemoveAll(p)
		}
	}
}

// A-u2: the lab files get lab paths for every listed key; a live path that would survive refuses
// with its key named; a plugin arg path needs lab_args; an unknown config key refuses.
func TestReleaseLabFiles(t *testing.T) {
	lab := t.TempDir()
	lab, _ = filepath.EvalSymlinks(lab)
	liveDir := t.TempDir()
	cfg := map[string]any{"server": "unix:///live/rex.sock", "state": liveDir + "/state", "view": liveDir + "/view",
		"profiles": liveDir + "/profiles.json", "palette_lua": liveDir + "/palette.lua", "transcript": true,
		"agents": []any{map[string]string{"id": "user", "name": "User"}}, "plugins": []any{map[string]string{"source": "github:o/r@main"}}}
	managed := map[string]any{"plugins": []any{map[string]any{"id": "auto", "source": "github:o/auto@main",
		"watch_roots": []any{liveDir + "/Automations"}, "args": map[string]any{"gallery": "true"}}}}
	profiles := map[string]any{"profiles": []any{map[string]any{"id": "claude", "name": "Claude", "kind": "agent",
		"command": []any{"sh"}, "cwd": liveDir + "/work"}}}
	files := func(cfg, managed, profiles map[string]any) relLiveFiles {
		a, _ := json.Marshal(cfg)
		b, _ := json.Marshal(managed)
		c, _ := json.Marshal(profiles)
		return relLiveFiles{Config: a, Managed: b, Lock: []byte(`{"version":1,"plugins":{}}`), Base: []byte(`{"version":1,"peers":{}}`), Profiles: c}
	}
	path, rw, err := labFiles(files(cfg, managed, profiles), lab, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, x := range rw {
		got[x.Key] = x.Lab
	}
	for k, want := range map[string]string{"config.server": "unix://" + lab + "/rex.sock", "config.state": lab + "/hub", "config.view": lab + "/view",
		"config.profiles": lab + "/home/profiles.json", "config.palette_lua": lab + "/home/lazyrex_palette.lua",
		"managed.auto.watch_roots[0]": lab + "/inbox/1", "profiles.claude.cwd": lab + "/home"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	for _, p := range []string{path, lab + "/hub/plugins.json", lab + "/hub-live.lock.json", lab + "/hub-live.lock.json.peers.json", lab + "/home/profiles.json"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), liveDir) {
			t.Errorf("%s still names the live dir: %s", p, b)
		}
	}

	refuse := func(name, want string, cfg, managed, profiles map[string]any, labArgs map[string]map[string]string) {
		t.Helper()
		_, _, err := labFiles(files(cfg, managed, profiles), t.TempDir(), labArgs)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	copyMap := func(m map[string]any) map[string]any {
		b, _ := json.Marshal(m)
		var out map[string]any
		json.Unmarshal(b, &out)
		return out
	}
	c := copyMap(cfg)
	c["future_dir"] = liveDir
	refuse("unknown key", `the key "future_dir"`, c, managed, profiles, nil)
	c = copyMap(cfg)
	c["agents"] = []any{map[string]string{"id": "user", "status_record": liveDir + "/status"}}
	refuse("a live path in a known key", "key agents[0].status_record", c, managed, profiles, nil)
	m := copyMap(managed)
	m["plugins"].([]any)[0].(map[string]any)["args"] = map[string]any{"dir": liveDir + "/x"}
	refuse("arg path", "needs lab_args", cfg, m, profiles, nil)
	if _, rw, err := labFiles(files(cfg, m, profiles), t.TempDir(), map[string]map[string]string{"auto": {"dir": "inbox/x"}}); err != nil {
		t.Errorf("arg path with lab_args: %v", err)
	} else {
		found := false
		for _, x := range rw {
			found = found || x.Key == "managed.auto.args.dir" && strings.HasSuffix(x.Lab, "/inbox/x")
		}
		if !found {
			t.Errorf("lab_args not applied: %+v", rw)
		}
	}
	refuse("lab_args out of the lab", "relative to the lab", cfg, m, profiles, map[string]map[string]string{"auto": {"dir": "../x"}})
	p := copyMap(profiles)
	p["profiles"].([]any)[0].(map[string]any)["env"] = map[string]any{"DATA": liveDir}
	refuse("a profile env path", "key profiles[0].env.DATA", cfg, managed, p, nil)
	c = copyMap(cfg)
	c["test_roots"] = []any{lab}
	refuse("test mode key", "test-mode key", c, managed, profiles, nil)
}

// C-u2: each tier reason selects full; none selects default; -tier full raises.
func TestReleaseDecideTier(t *testing.T) {
	for name, tc := range map[string]struct {
		added, files, formats []string
		req, want             string
	}{
		"none":      {want: tierDefault},
		"schema":    {added: []string{"spawns.x TEXT 0 -"}, want: tierFull},
		"files":     {files: []string{"hub/cmd/hub/pinsync.go"}, want: tierFull},
		"formats":   {formats: []string{"format: the output differs"}, want: tierFull},
		"requested": {req: tierFull, want: tierFull},
	} {
		got, reasons, err := decideTier(tc.added, tc.files, tc.formats, tc.req)
		if err != nil || got != tc.want || len(reasons) == 0 {
			t.Errorf("%s: %s %v %v", name, got, reasons, err)
		}
	}
	if _, _, err := decideTier(nil, nil, nil, "low"); err == nil {
		t.Error("an unknown tier passed")
	}
	if !strings.Contains(strings.Join(relFormatFiles(), " "), "pinsync.go") {
		t.Error("the format files")
	}
	for _, f := range relFormatFiles() {
		if _, err := os.Stat(filepath.Join("../../..", f)); err != nil {
			t.Errorf("the format file %s: %v", f, err)
		}
	}
}

// Design v2 D: a rerun renames the current record first, so a failed rerun leaves no current record
// and precheck refuses; the old record stays as history.
func TestReleaseRehearseSupersedes(t *testing.T) {
	f := newRelFixture(t, "hub")
	os.WriteFile(filepath.Join(f.r.run, "tool"), []byte("tool"), 0o700)
	tool := filepath.Join(f.r.run, "tool")
	f.r.tool = tool
	bound, err := f.r.bindings(tool)
	if err != nil {
		t.Fatal(err)
	}
	f.r.writeRehearsal(&relRehearsal{Pass: true, Bound: bound, Tier: tierDefault})
	if err := f.r.checkRehearsal(tool); err != nil {
		t.Fatal(err)
	}
	old := mustReadFile(filepath.Join(f.r.run, relRehearsalFile))
	f.r.now = func() time.Time { return time.Date(2026, 10, 22, 1, 2, 3, 0, time.UTC) }
	h := &rehearsal{r: f.r}
	if err := h.supersede(); err != nil {
		t.Fatal(err)
	}
	if err := f.r.checkRehearsal(tool); err == nil || !strings.Contains(err.Error(), "no rehearsal record") {
		t.Errorf("after a rerun started: %v", err)
	}
	m, _ := filepath.Glob(filepath.Join(f.r.run, relSuperseded+"20261022T010203Z-*"))
	if len(m) != 1 || !bytes.Equal(mustReadFile(m[0]), old) {
		t.Errorf("the superseded record: %v", m)
	}
	if err := h.supersede(); err != nil {
		t.Errorf("a second rerun without a current record: %v", err)
	}
}

// Design v2 with the mirror decision: the lab reaches GitHub repos only through the parent's
// mirrors. Known repos (with and without .git, and the git@ form) come from the mirrors; a missing
// commit refuses; an unexpected repo and every other transport fail at once (file only); and a
// credential variable of the parent does not reach the lab environment.
func TestReleaseLabMirrors(t *testing.T) {
	remote := t.TempDir()
	gitRun := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	heads := map[string]string{}
	for _, or := range [][2]string{{"lazy-rex", "LazyRex"}, {"o", "other"}} {
		work := filepath.Join(remote, "work-"+or[1])
		os.MkdirAll(work, 0o700)
		gitRun(work, "init", "-q")
		os.WriteFile(filepath.Join(work, "f"), []byte(or[1]), 0o600)
		gitRun(work, "add", "f")
		gitRun(work, "commit", "-q", "-m", "c")
		heads[or[0]+"/"+or[1]] = gitRun(work, "rev-parse", "HEAD")
		os.MkdirAll(filepath.Join(remote, or[0]), 0o700)
		gitRun(remote, "clone", "-q", "--bare", work, filepath.Join(remote, or[0], or[1]+".git"))
	}
	lab, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("GH_TOKEN", "parent-secret")
	t.Setenv("GITHUB_TOKEN", "parent-secret")
	gitEnv, got, err := labMirrors(lab, "file://"+remote+"/", [][2]string{{"lazy-rex", "LazyRex"}, {"o", "other"}, {"lazy-rex", "LazyRex"}})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range heads {
		if got[k] != v {
			t.Errorf("mirror HEAD %s = %s, want %s", k, got[k], v)
		}
	}
	env := append(labEnv(lab, "unix://"+lab+"/rex.sock", nil), gitEnv...)
	for _, kv := range env {
		for _, name := range []string{"GH_TOKEN=", "GITHUB_TOKEN=", "GIT_ASKPASS=", "SSH_AUTH_SOCK="} {
			if strings.HasPrefix(kv, name) {
				t.Errorf("the lab env has %s", name)
			}
		}
	}
	os.MkdirAll(filepath.Join(lab, "home"), 0o700)
	labGit := func(args ...string) (string, error) {
		c := exec.Command("git", args...)
		c.Dir, c.Env = filepath.Join(lab, "home"), env
		out, err := c.CombinedOutput()
		return string(out), err
	}
	for _, k := range []struct{ url, repo string }{{"https://github.com/lazy-rex/LazyRex.git", "lazy-rex/LazyRex"},
		{"https://github.com/o/other", "o/other"}, {"git@github.com:lazy-rex/LazyRex.git", "lazy-rex/LazyRex"}} {
		out, err := labGit("ls-remote", k.url, "HEAD")
		if err != nil || !strings.Contains(out, heads[k.repo]) {
			t.Errorf("a known repo %s: %v %s", k.url, err, out)
		}
	}
	// Each refusal must show git's own reason, not a failed network attempt.
	for _, u := range []struct{ url, why string }{{"https://github.com/evil/x.git", "no-such-mirror"},
		{"ssh://git@github.com/lazy-rex/LazyRex.git", "transport 'ssh' not allowed"}, {"git://github.com/lazy-rex/LazyRex.git", "transport 'git' not allowed"},
		{"http://github.com/lazy-rex/LazyRex.git", "transport 'http' not allowed"}, {"https://gitlab.com/lazy-rex/LazyRex.git", "transport 'https' not allowed"}} {
		start := time.Now()
		out, err := labGit("ls-remote", u.url, "HEAD")
		if err == nil || !strings.Contains(out, u.why) {
			t.Errorf("an unexpected remote %s: %v, want %q in:\n%s", u.url, err, u.why, out)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("an unexpected remote %s took %s (a network attempt?)", u.url, d)
		}
	}
	if err := needCommits(lab, map[string][]string{"lazy-rex/LazyRex": {heads["lazy-rex/LazyRex"]}, "o/other": {heads["o/other"]}}); err != nil {
		t.Errorf("present commits: %v", err)
	}
	if err := needCommits(lab, map[string][]string{"o/other": {strings.Repeat("ab", 20)}}); err == nil || !strings.Contains(err.Error(), "no commit") {
		t.Errorf("a missing commit: %v", err)
	}
}
