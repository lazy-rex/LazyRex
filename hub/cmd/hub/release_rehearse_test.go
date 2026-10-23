package main

import (
	"bytes"
	"database/sql"
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

// rehearsalFixture is a frozen release world whose config names a profile file, and a rehearsal of
// its run that captured the live inputs and passed its phases.
func rehearsalFixture(t *testing.T) (*relWorld, *rehearsal, string) {
	t.Helper()
	w := newRelWorld(t)
	p := filepath.Join(w.dir, "profiles.json")
	if err := os.WriteFile(p, []byte(`{"profiles":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(mustRead(w.cfg), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["profiles"] = p
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(w.cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	w.freeze()
	r := w.open("precheck")
	t.Cleanup(r.unlock)
	c, err := r.readCandidate()
	if err != nil {
		t.Fatal(err)
	}
	h := &rehearsal{r: r, c: c, tool: r.tool, lab: t.TempDir(), phases: []relPhase{{Name: "forward", Result: "pass"}, {Name: "rollback", Result: "pass"}}}
	// As hubRehearse: the earlier record (the world's) is superseded before the capture.
	if err := h.supersede(); err != nil {
		t.Fatal(err)
	}
	if h.start, err = h.capture(); err != nil {
		t.Fatal(err)
	}
	return w, h, p
}

// R8: a live input that changes during the rehearsal fails the finish with no record, and the lab
// stays as evidence. With no change, the finish writes the record, then deletes the lab.
func TestReleaseRehearsalFinish(t *testing.T) {
	_, h, p := rehearsalFixture(t)
	evidence := filepath.Join(h.lab, "diagnostic.log")
	if err := os.WriteFile(evidence, []byte("phase evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"profiles":[{"id":"changed"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.finish(tierDefault, nil); err == nil || !strings.Contains(err.Error(), "live-profiles") {
		t.Fatalf("a changed live input: %v", err)
	}
	if _, err := os.Stat(evidence); err != nil {
		t.Fatalf("a failed finish lost the lab evidence: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.r.run, relRehearsalFile)); err == nil {
		t.Fatal("a failed finish wrote a record")
	}

	_, h, _ = rehearsalFixture(t)
	if err := h.finish(tierDefault, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.lab); err == nil {
		t.Error("the lab is still there after a finish")
	}
	if err := h.r.checkRehearsal(h.tool); err != nil {
		t.Errorf("the record of the finish: %v", err)
	}
}

// R9: the record binds every live input that the lab copies; precheck refuses a later change of
// any of them, the profile file too.
func TestReleaseRehearsalBindsLiveInputs(t *testing.T) {
	_, h, p := rehearsalFixture(t)
	if err := h.publish(tierDefault, nil); err != nil {
		t.Fatal(err)
	}
	bound := map[string]string{}
	json.Unmarshal(mustRead(filepath.Join(h.r.run, relRehearsalFile)), &struct {
		Bound *map[string]string `json:"bound"`
	}{&bound})
	for _, k := range []string{"live-config", "live-managed", "live-lock-pins", "live-base", "live-profiles"} {
		if v := bound[k]; v == "" || v == "-" {
			t.Errorf("the record does not bind %s: %q", k, v)
		}
	}
	if err := os.WriteFile(p, []byte(`{"profiles":[{"id":"changed","command":["sh"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.r.checkRehearsal(h.tool); err == nil || !strings.Contains(err.Error(), "live-profiles") {
		t.Fatalf("the gate after a profile change: %v", err)
	}
	if err := hubPrecheck(h.r); err == nil {
		t.Fatal("precheck accepted changed profiles after the rehearsal")
	}
}

// R10: every schema object counts. An index, trigger, view or constraint that H adds, changes or
// removes selects the full tier; a column of OLD that changed fails.
func TestReleaseRehearsalSchemaObjects(t *testing.T) {
	for name, tc := range map[string]struct {
		before, after string
		fail          bool
	}{
		"unique index":   {after: "CREATE UNIQUE INDEX t_k_unique ON t(k)"},
		"trigger":        {after: "CREATE TRIGGER t_ins AFTER INSERT ON t BEGIN SELECT 1; END"},
		"view":           {after: "CREATE VIEW t_view AS SELECT k FROM t"},
		"removed index":  {before: "CREATE INDEX t_k ON t(k)", after: "DROP INDEX t_k"},
		"new table pk":   {after: "CREATE TABLE u (id INTEGER PRIMARY KEY, v TEXT)"},
		"changed column": {after: "ALTER TABLE t RENAME COLUMN k TO k2", fail: true},
	} {
		lab := t.TempDir()
		os.MkdirAll(filepath.Join(lab, relLabState), 0o700)
		h := &rehearsal{lab: lab, schemas: map[string][]string{}}
		db, err := sql.Open("sqlite", "file:"+filepath.Join(lab, relLabState, "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"CREATE TABLE t (k TEXT)", tc.before} {
			if _, err := db.Exec(s); s != "" && err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if err := h.schema("before"); err != nil {
			t.Fatal(err)
		}
		if tier, _, _ := decideTier(h.schemaChanges("before"), nil, nil, ""); tier != tierDefault {
			t.Fatalf("%s: no change selected %s", name, tier)
		}
		if _, err := db.Exec(tc.after); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		db.Close()
		err = h.schema("forward")
		if tc.fail {
			if err == nil {
				t.Errorf("%s: a changed OLD column passed", name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tier, reasons, err := decideTier(h.schemaChanges("forward"), nil, nil, ""); err != nil || tier != tierFull {
			t.Errorf("%s: selected %s %v %v", name, tier, reasons, err)
		}
	}
}

// R11: a format command that fails is uncertain, also when it fails the same way under OLD and H.
func TestReleaseRehearsalFormatFailuresAreUncertain(t *testing.T) {
	w, h, _ := rehearsalFixture(t)
	h.env, h.cfg, h.run = os.Environ(), w.cfg, w.run
	os.MkdirAll(filepath.Join(h.lab, "home"), 0o700)
	os.MkdirAll(filepath.Join(h.run, "bin"), 0o700)
	os.MkdirAll(filepath.Join(h.lab, relLabState), 0o700)
	h.oldBin = filepath.Join(h.lab, "old-bin")
	script := []byte("#!/bin/sh\nif [ \"$2\" = pull ]; then echo 'pull failed' >&2; exit 1; fi\n")
	for _, p := range []string{h.oldBin, filepath.Join(h.run, "bin", "hub-H")} {
		if err := os.WriteFile(p, script, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(h.lab, relLabState, "plugins.json"), h.live.Managed, 0o600); err != nil {
		t.Fatal(err)
	}
	reasons, err := h.formats()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(reasons, "\n"), "plugin pull exited 1") {
		t.Fatalf("a pull that fails under both binaries is not uncertain: %v", reasons)
	}
}

// c3e: the lab gets the probe agent and the probe profile; a profile file is added when the config
// has none; a live agent or profile with the probe ID refuses.
func TestReleaseLabProbeAgent(t *testing.T) {
	lab := t.TempDir()
	cfg := filepath.Join(lab, "hub-live.json")
	os.WriteFile(cfg, []byte(`{"agents":[{"id":"user","name":"User"}]}`), 0o600)
	out, err := labProbeAgent(lab, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Agents   []map[string]string `json:"agents"`
		Profiles string              `json:"profiles"`
	}
	json.Unmarshal(mustRead(cfg), &c)
	if len(c.Agents) != 2 || c.Agents[1]["id"] != relProbeAgent || c.Profiles != filepath.Join(lab, "home", "profiles.json") {
		t.Fatalf("the lab config: %+v", c)
	}
	var pf struct {
		Profiles []Profile `json:"profiles"`
	}
	json.Unmarshal(mustRead(c.Profiles), &pf)
	if len(pf.Profiles) != 1 || pf.Profiles[0].ID != relProbeProfile || validProfile(pf.Profiles[0]) != nil || pf.Profiles[0].Kind != "accessory" {
		t.Fatalf("the lab profiles: %+v", pf)
	}
	if len(out) != 3 {
		t.Errorf("the transform additions: %+v", out)
	}
	if _, err := labProbeAgent(lab, cfg); err == nil || !strings.Contains(err.Error(), "the live config has the agent") {
		t.Errorf("a second probe agent: %v", err)
	}
	os.WriteFile(cfg, []byte(`{"profiles":"`+c.Profiles+`"}`), 0o600)
	if _, err := labProbeAgent(lab, cfg); err == nil || !strings.Contains(err.Error(), "the live profiles have the profile") {
		t.Errorf("a live probe profile: %v", err)
	}
}

// The failing go logs each call, exits 1 for go build, and runs the real go for
// every other call.
func TestReleaseFailGo(t *testing.T) {
	dir := t.TempDir()
	real, calls, shim := filepath.Join(dir, "realgo"), filepath.Join(dir, "calls"), filepath.Join(dir, "go")
	os.WriteFile(real, []byte("#!/bin/sh\necho \"real $*\"\n"), 0o700)
	os.WriteFile(shim, []byte(failGoScript(calls, real)), 0o700)
	c := exec.Command(shim, "mod", "download")
	c.Dir = dir
	if out, err := c.Output(); err != nil || string(out) != "real mod download\n" {
		t.Fatalf("go mod download: %q %v", out, err)
	}
	c = exec.Command(shim, "build", "-o", "x", "./cmd/hub")
	c.Dir = dir
	if out, err := c.Output(); err == nil || len(out) != 0 {
		t.Fatalf("go build: %q %v", out, err)
	}
	if got := goBuilds(calls); len(got) != 1 || got[0] != dir {
		t.Errorf("the build calls: %v (log %q)", got, mustRead(calls))
	}
}

// fullProbe is a lab hub.db with the table newer (one row before the probe file) and the probe
// file script.
func fullProbe(t *testing.T, script string) (*rehearsal, *sql.DB) {
	t.Helper()
	h := &rehearsal{lab: t.TempDir(), r: &releaser{spec: &relSpec{Probes: "probes.sql"}, out: &bytes.Buffer{}}}
	os.MkdirAll(filepath.Join(h.lab, relLabState), 0o700)
	db, err := h.hubDB(false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE newer (value); INSERT INTO newer(rowid, value) VALUES (5, 'before');
		CREATE TABLE other (value); INSERT INTO other VALUES ('before');
		CREATE TABLE wr (k TEXT PRIMARY KEY, v) WITHOUT ROWID`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.lab, "probes.sql"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return h, db
}

// R12 and R13: every row that the probe file writes, in any table and at any rowid, is checked with
// each value's SQL type. A probe file that deletes a row, changes the schema or a table without
// rowid, or writes no row refuses.
func TestReleaseFileProbes(t *testing.T) {
	for name, tc := range map[string]struct{ script, change string }{
		"null to text":    {`INSERT INTO newer VALUES (NULL);`, `UPDATE newer SET value = 'NULL' WHERE rowid = 6`},
		"integer to text": {`INSERT INTO newer VALUES (1);`, `UPDATE newer SET value = '1' WHERE rowid = 6`},
		"text to blob":    {`INSERT INTO newer VALUES ('ab');`, `UPDATE newer SET value = CAST('ab' AS BLOB) WHERE rowid = 6`},
		"negative rowid":  {`INSERT INTO newer(rowid, value) VALUES (-1, 'negative'), (1, 'positive');`, `DELETE FROM newer WHERE rowid = -1`},
		"changed row":     {`UPDATE newer SET value = 'probe' WHERE rowid = 5; INSERT INTO newer VALUES ('x');`, `UPDATE newer SET value = 'before' WHERE rowid = 5`},
		"updated table":   {`INSERT INTO newer VALUES ('x'); UPDATE other SET value = 'probe';`, `UPDATE other SET value = 'corrupted'`},
	} {
		h, db := fullProbe(t, tc.script)
		if err := h.writeFileProbes(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := h.checkFileProbes("forward"); err != nil {
			t.Fatalf("%s: before the change: %v", name, err)
		}
		if _, err := db.Exec(tc.change); err != nil {
			t.Fatal(err)
		}
		if err := h.checkFileProbes("rollback"); err == nil {
			t.Errorf("%s: the changed probe row passed", name)
		}
	}
	h, _ := fullProbe(t, `DELETE FROM newer WHERE rowid = 5; INSERT INTO newer VALUES ('x');`)
	if err := h.writeFileProbes(); err == nil || !strings.Contains(err.Error(), "deleted the row newer rowid 5") {
		t.Errorf("a probe file that deletes a row: %v", err)
	}
	h, _ = fullProbe(t, `CREATE TABLE t2 (x); INSERT INTO newer VALUES ('x');`)
	if err := h.writeFileProbes(); err == nil || !strings.Contains(err.Error(), "changed the schema") {
		t.Errorf("a probe file that changes the schema: %v", err)
	}
	h, _ = fullProbe(t, `INSERT INTO wr VALUES ('a', 1); INSERT INTO newer VALUES ('x');`)
	if err := h.writeFileProbes(); err == nil || !strings.Contains(err.Error(), "which has no rowid") {
		t.Errorf("a probe file that changes a table without rowid: %v", err)
	}
	h, _ = fullProbe(t, `INSERT INTO newer SELECT value FROM newer WHERE 0;`)
	if err := h.writeFileProbes(); err == nil || !strings.Contains(err.Error(), "wrote no row") {
		t.Errorf("a probe file that writes no row: %v", err)
	}
}

// labHubDB is a lab hub.db made by the hub's own openDB, so it has the schema of this hub, and a
// back verify list with the plugins alpha and beta.
func labHubDB(t *testing.T) *rehearsal {
	t.Helper()
	h := &rehearsal{lab: t.TempDir(), run: t.TempDir(), r: &releaser{out: &bytes.Buffer{}}}
	os.MkdirAll(filepath.Join(h.lab, relLabState), 0o700)
	db, err := openDB(filepath.Join(h.lab, relLabState, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	b, _ := json.Marshal([]relVerifyEntry{{Name: "alpha"}, {Name: "beta"}})
	if err := os.WriteFile(filepath.Join(h.run, relVerifyBack), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

// The setup and phase steps of the rehearsal on the lab hub.db run on the schema of this hub (2d:
// a new hub.db has no records or ops table). A lab hub.db with an agent binding refuses.
func TestReleaseLabHubDB(t *testing.T) {
	h := labHubDB(t)
	if err := h.writeProbes(); err != nil {
		t.Fatalf("the setup step on a new lab hub.db: %v", err)
	}
	if err := h.data("setup"); err != nil {
		t.Fatalf("the phase step on a new lab hub.db: %v", err)
	}
	h = labHubDB(t)
	db, err := h.hubDB(false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO bindings VALUES ('a', 'block:1', 1, 's')`); err != nil {
		t.Fatal(err)
	}
	if err := h.writeProbes(); err == nil || !strings.Contains(err.Error(), "agent bindings") {
		t.Errorf("a lab hub.db with an agent binding: %v", err)
	}
}
