package main

// lazyrex-hub release rehearse -run <live run> -lab /tmp/rexlab-release-<name> [-tier full]
// (docs/releasing.md).
//
// It runs the release of a frozen live run forward and back in a new lab: a lab Rex server
// (labserver.go), lab copies of the live files, a GitHub clone of LazyRex at OLD, and the same
// steps as the live run, as children of this binary with a lab spec. The live files are read once
// at the start and once before the record; rehearse never writes beside them. A pass writes
// <live run>/rehearsal.json, the gate of precheck, and deletes the lab. A failure keeps the lab.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	relLiveRepo   = "https://github.com/lazy-rex/LazyRex.git"
	relSuperseded = relRehearsalFile + ".superseded-"
	relProbeKey   = "relprobe/value"
	relProbeOp    = "relprobe/op"
)

// relFormatFiles are the files that own the lock, base, pin document and managed file formats. A
// change OLD..H in one of them selects the full tier (a second trigger beside the measurement).
func relFormatFiles() []string {
	return []string{"hub/cmd/hub/pluginlock.go", "hub/cmd/hub/pinsync.go", "hub/cmd/hub/pluginconfig.go"}
}

// openRehearse opens a rehearsal of a live run. It takes no lock beside the live config, and it
// reads the live files only.
func openRehearse(name string, args []string, stdout, stderr io.Writer) (*releaser, int) {
	fs := flag.NewFlagSet("release "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	run := fs.String("run", "", "the live run directory, after freeze (required)")
	lab := fs.String("lab", "", "the new lab directory, /tmp/rexlab-release-<name> (required)")
	tier := fs.String("tier", "", "full: run the full tier even when the default tier is enough")
	if err := fs.Parse(args); err != nil || *run == "" || *lab == "" || fs.NArg() > 0 || (*tier != "" && *tier != tierFull) {
		fmt.Fprintf(stderr, "usage: lazyrex-hub release %s -run <dir> -lab /tmp/%s<name> [-tier full]\n", name, relLabPrefix)
		return nil, relUsage
	}
	fail := func(err error) (*releaser, int) {
		fmt.Fprintf(stderr, "release %s: FAILED: %v\n", name, err)
		return nil, relFailed
	}
	s, err := readSpec(filepath.Join(*run, relSpecFile))
	if err != nil {
		return fail(err)
	}
	if s.Kind != "hub" || s.Lab {
		return fail(relFail("rehearse needs the run of a live hub release"))
	}
	cfg, err := readPluginCmdConfig(s.Config)
	if err != nil {
		return fail(err)
	}
	if err := checkLabSpec(*run, s, cfg); err != nil {
		return fail(err)
	}
	return &releaser{run: *run, spec: s, step: name, out: stdout, cfg: cfg, ctx: context.Background(), unlock: func() {},
		now: time.Now, rehearseLab: *lab, rehearseTier: *tier}, relOK
}

// rehearsal is one rehearse run.
type rehearsal struct {
	r          *releaser     // the live run: rehearse writes only its record files there
	c          *relCandidate // the live candidate
	tool       string
	lab        string
	l          *lab
	env        []string // the environment of every lab child (labEnv and the Go caches)
	cfg        string   // the lab config
	run        string   // the lab run
	oldBin     string
	live       relLiveFiles
	start      map[string]string // the live inputs and bindings at the start
	phases     []relPhase
	schemas    map[string][]string   // the hub.db schema items by phase
	mirrors    map[string]string     // the HEAD of each GitHub mirror, by owner/repo
	commits    map[string][]string   // the commits that the mirrors must hold, by owner/repo
	fileProbes map[string][]probeRow // the rows of the spec probe file, by table (full tier)
	sdk        []relSDKCheck         // the SDK byte checks, in order
	n          int                   // the number of child steps (log names)
	stopped    bool                  // stopLab ran
}

func (h *rehearsal) say(format string, args ...any) { h.r.say(format, args...) }

func hubRehearse(r *releaser) (err error) {
	tool, err := r.toolPath()
	if err != nil {
		return err
	}
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	h := &rehearsal{r: r, c: c, tool: tool, schemas: map[string][]string{}}
	if err := h.supersede(); err != nil {
		return err
	}
	if h.lab, err = createLab(r.rehearseLab); err != nil {
		return err
	}
	unlock, err := lockOperator(filepath.Join(h.lab, "rehearse.json"))
	if err != nil {
		return err
	}
	defer unlock()
	h.say("the lab is %s", h.lab)
	finished := false
	defer func() {
		if !finished {
			for _, p := range h.stopLab() {
				h.say("cleanup: %s", p)
			}
			h.say("FAILED; the lab is kept: %s", h.lab)
		}
	}()
	if h.start, err = h.capture(); err != nil {
		return err
	}
	if err := h.setup(); err != nil {
		return err
	}
	if err := h.phase("forward", func() error { return h.forwardAs("forward") }); err != nil {
		return err
	}
	if err := h.phase("rollback", func() error { return h.rollbackAs("rollback") }); err != nil {
		return err
	}
	tier, reasons, err := h.tier()
	if err != nil {
		return err
	}
	if tier == tierFull {
		if err := h.full(); err != nil {
			return err
		}
	}
	if err := h.finish(tier, reasons); err != nil {
		return err
	}
	finished = true
	return nil
}

// finish ends a passing rehearsal. It stops the lab processes, reads the live inputs again and
// writes the record, and only then deletes the lab. A failure before the record keeps the lab as
// evidence and writes no record. A failed delete after the record is a warning: the record is valid.
func (h *rehearsal) finish(tier string, reasons []string) error {
	if problems := h.stopLab(); len(problems) > 0 {
		return relFail("cleanup: %s", strings.Join(problems, "; "))
	}
	if err := h.publish(tier, reasons); err != nil {
		return err
	}
	if problems := removeRehearsalLab(h.lab); len(problems) > 0 {
		h.say("WARNING: the record is written, but %s; delete the lab %s by hand", strings.Join(problems, "; "), h.lab)
		return nil
	}
	h.say("the lab is deleted")
	return nil
}

// supersede renames a current record to rehearsal.json.superseded-<time>-<pid> before any phase:
// a failed or interrupted rerun then leaves no current record.
func (h *rehearsal) supersede() error {
	p := filepath.Join(h.r.run, relRehearsalFile)
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	to := fmt.Sprintf("%s%s%s-%d", h.r.run+"/", relSuperseded, h.r.now().UTC().Format("20060102T150405Z"), os.Getpid())
	if err := os.Rename(p, to); err != nil {
		return err
	}
	h.say("the earlier record is kept as %s", to)
	return nil
}

// capture reads the live inputs once: the bytes for the lab files, and the values that the record
// binds. lab-transform is left out: rehearse writes it later.
func (h *rehearsal) capture() (map[string]string, error) {
	r := h.r
	read := func(p string) ([]byte, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, relFail("the live input %s: %v", p, err)
		}
		return b, nil
	}
	var live relLiveFiles
	var err error
	if live.Config, err = read(r.cfg.path); err != nil {
		return nil, err
	}
	if live.Managed, err = read(r.cfg.managed); err != nil {
		return nil, err
	}
	if live.Lock, err = read(r.cfg.lock); err != nil {
		return nil, err
	}
	if live.Base, err = read(peersPath(r.cfg.lock)); err != nil {
		return nil, err
	}
	if p := configProfiles(r.cfg.path); p != "" {
		if live.Profiles, err = read(p); err != nil {
			return nil, err
		}
	}
	bound, err := r.bindings(h.tool)
	if err != nil {
		return nil, err
	}
	// The record binds the files; the lab gets the bytes read above. They must be the same.
	for _, f := range []struct {
		field string
		b     []byte
	}{{"live-config", live.Config}, {"live-managed", live.Managed}, {"live-base", live.Base}, {"live-profiles", live.Profiles}} {
		if f.b != nil && bound[f.field] != sha256Bytes(f.b) {
			return nil, relFail("a live input changed while the rehearsal read it: %s", f.field)
		}
	}
	out := map[string]string{}
	for k, v := range bound {
		if k != "lab-transform" {
			out["bound "+k] = v
		}
	}
	h.live = live
	return out, nil
}

// cmd runs a command in the lab environment and returns its stdout.
func (h *rehearsal) cmd(dir string, argv ...string) (string, error) {
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir, c.Env = dir, h.env
	var stdout, stderr strings.Builder
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return stdout.String(), relFail("%s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// labEnvironment is labEnv with the Go module and build caches of this process (shared on purpose:
// a fresh cache costs minutes).
func labEnvironment(lab, server string) ([]string, error) {
	extra := map[string]string{}
	out, err := exec.Command("go", "env", "GOPATH", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		return nil, relFail("go env: %v (go must be on PATH)", err)
	}
	vals := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, k := range []string{"GOPATH", "GOMODCACHE", "GOCACHE"} {
		if i < len(vals) && vals[i] != "" {
			extra[k] = vals[i]
		}
	}
	return labEnv(lab, server, extra), nil
}

// setup makes the lab: the clone and the OLD binary, the lab files, the lab server, the lab cache,
// the lab run (freeze), and OLD in a dev pane with the probe rows.
func (h *rehearsal) setup() error {
	L := h.lab
	server := "unix://" + filepath.Join(L, "rex.sock")
	for _, d := range append([]string{"state", "blocks", "bin", "log"}, labHomeDirs...) {
		if err := os.MkdirAll(filepath.Join(L, d), 0o700); err != nil {
			return err
		}
	}
	var err error
	if h.env, err = labEnvironment(L, server); err != nil {
		return err
	}
	repos, err := lockRepos(h.live.Lock)
	if err != nil {
		return err
	}
	h.say("the GitHub mirrors of %d repos (this process's own git access; no credential in the lab)", len(repos))
	gitEnv, heads, err := labMirrors(L, "https://github.com/", repos)
	if err != nil {
		return err
	}
	h.env, h.mirrors = append(h.env, gitEnv...), heads
	need := map[string][]string{"lazy-rex/LazyRex": {h.c.From.Commit, h.c.To.Commit}}
	var ll pluginLock
	json.Unmarshal(h.live.Lock, &ll)
	for _, e := range ll.Plugins {
		need[e.Owner+"/"+e.Repo] = append(need[e.Owner+"/"+e.Repo], e.Commit)
	}
	if err := needCommits(L, need); err != nil {
		return err
	}
	h.commits = need
	home := filepath.Join(L, "home")
	clone := filepath.Join(L, "release", "LazyRex")
	old := h.c.From.Commit
	h.say("the LazyRex clone at OLD %s, from the mirror", old)
	if _, err := h.cmd(home, "git", "clone", "-q", relLiveRepo, clone); err != nil {
		return err
	}
	if _, err := h.cmd(clone, "git", "checkout", "-q", "--detach", old); err != nil {
		return err
	}
	h.oldBin = filepath.Join(L, "bin", "hub-old")
	if _, err := h.cmd(filepath.Join(clone, "hub"), "go", "build", "-o", h.oldBin, "./cmd/hub"); err != nil {
		return err
	}

	cfg, transform, err := labFiles(h.live, L, h.r.spec.LabArgs)
	if err != nil {
		return err
	}
	h.cfg = cfg
	added, err := labProbeAgent(L, cfg)
	if err != nil {
		return err
	}
	transform = append(transform, added...)
	if err := writeJSON(filepath.Join(h.r.run, relLabTransform), transform); err != nil {
		return err
	}
	labCfg, err := readPluginCmdConfig(cfg)
	if err != nil {
		return err
	}

	rexBin, err := exec.LookPath("rex")
	if err != nil {
		return relFail("no rex command on PATH")
	}
	h.l = &lab{dir: L, socket: filepath.Join(L, "rex.sock"), server: server, env: h.env, tracked: map[int]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	self, err := os.Executable() // the server's keeper (labkeeper.go) is this binary
	if err != nil {
		return err
	}
	if err := h.l.startServer(ctx, self, rexBin); err != nil {
		return err
	}
	h.say("the lab server runs (pid %d)", h.l.srv.pid)

	// The lab cache: OLD syncs the lab copy. The lab lock equals the live lock
	// by its pins (a sync can name new cache dirs), and the base byte for byte.
	livePins, err := lockPinLines(labCfg.lock) // still the live bytes
	if err != nil {
		return err
	}
	if _, err := h.cmd(home, h.oldBin, "plugin", "sync", "-config", cfg); err != nil {
		return err
	}
	labPins, err := lockPinLines(labCfg.lock)
	if err != nil {
		return err
	}
	if strings.Join(livePins, "\n") != strings.Join(labPins, "\n") {
		return relFail("after OLD's sync, the lab lock pins differ from the live lock pins")
	}
	if lp, err := lockPinsHash(labCfg.lock); err != nil || lp != h.start["bound live-lock-pins"] {
		return relFail("after OLD's sync, the lab lock differs from the live lock without its cache dirs (%v)", err)
	}
	if b, err := os.ReadFile(peersPath(labCfg.lock)); err != nil || sha256Bytes(b) != h.start["bound live-base"] {
		return relFail("after OLD's sync, the lab base differs from the live base (%v)", err)
	}

	// The lab run: a lab spec, freeze as a child, and the lab candidate equal to the live one.
	spec := map[string]any{"kind": "hub", "h": h.c.To.Commit, "config": cfg, "release_dir": filepath.Join(L, "release"),
		"server": server, "dev_log": filepath.Join(L, "dev.log"), "lab": true}
	if h.r.spec.Probes != "" {
		p := filepath.Join(L, "probes.sql")
		if err := copyFile(h.r.spec.Probes, p); err != nil {
			return err
		}
		spec["probes"] = p
	}
	specPath := filepath.Join(L, "spec.json")
	if err := writeJSON(specPath, spec); err != nil {
		return err
	}
	h.run = filepath.Join(L, "run")
	if err := h.child("freeze", nil, "-spec", specPath, "-run", h.run); err != nil {
		return err
	}
	if err := h.sameCandidate(); err != nil {
		return err
	}

	// OLD in a dev pane in a lab session; then the probe rows, written while OLD is stopped.
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := h.l.r.Call(ctx, "session.create", map[string]any{"label": "release-rehearsal"}, &created); err != nil {
		return relFail("session.create in the lab: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.run, "session"), []byte(created.SessionID+"\n"), 0o600); err != nil {
		return err
	}
	if err := h.withLab(func(lr *releaser) error {
		if _, err := lr.startHub(old, "lab-setup"); err != nil {
			return err
		}
		if err := lr.verifyList(relVerifyBack); err != nil {
			return err
		}
		return lr.closeDev("dev-close-lab-setup", true)
	}); err != nil {
		return err
	}
	if err := h.writeProbes(); err != nil {
		return err
	}
	if err := h.withLab(func(lr *releaser) error {
		if _, err := lr.startHub(old, "lab-setup-2"); err != nil {
			return err
		}
		return lr.verifyList(relVerifyBack)
	}); err != nil {
		return err
	}
	if err := h.data("setup"); err != nil {
		return err
	}
	return h.schema("before")
}

// sameCandidate checks that the lab run froze the same release as the live run: the candidate
// without the file hashes (the lab files have lab paths), the pin documents and the verify lists.
func (h *rehearsal) sameCandidate() error {
	read := func(run string) (*relCandidate, error) {
		b, err := os.ReadFile(filepath.Join(run, relCandidateFile))
		if err != nil {
			return nil, err
		}
		var c relCandidate
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, err
		}
		c.Files = nil
		return &c, nil
	}
	a, err := read(h.r.run)
	if err != nil {
		return err
	}
	b, err := read(h.run)
	if err != nil {
		return err
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		return relFail("the lab candidate differs from the live candidate:\n live %s\n lab  %s", ja, jb)
	}
	for _, f := range []string{relFwdDoc, relBackDoc, relVerifyFwd, relVerifyBack} {
		x, err := sha256File(filepath.Join(h.r.run, f))
		if err != nil {
			return err
		}
		y, err := sha256File(filepath.Join(h.run, f))
		if err != nil {
			return err
		}
		if x != y {
			return relFail("the lab %s differs from the live one", f)
		}
	}
	h.say("the lab candidate equals the live candidate")
	return nil
}

// withLab runs fn with a releaser of the lab run, under the operator lock of the lab config. No
// child step runs meanwhile: rehearse runs them one at a time.
func (h *rehearsal) withLab(fn func(lr *releaser) error) error {
	lr, err := openRun(h.run, "rehearse", h.r.out)
	if err != nil {
		return err
	}
	defer lr.unlock()
	lr.env = h.env
	return fn(lr)
}

// child runs one step of the lab run as a child of this binary, with the lab environment and
// extra variables. Its output goes to <lab>/log/<n>-<step>.log.
func (h *rehearsal) child(step string, extra []string, args ...string) error {
	code, logp, err := h.childCode(step, extra, args...)
	if err != nil {
		return err
	}
	if code != relOK {
		return relFail("step %s exited %d; see %s", step, code, logp)
	}
	return nil
}

func (h *rehearsal) childCode(step string, extra []string, args ...string) (int, string, error) {
	h.n++
	logp := filepath.Join(h.lab, "log", fmt.Sprintf("%02d-%s.log", h.n, step))
	f, err := os.Create(logp)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	if len(args) == 0 {
		args = []string{"-run", h.run}
	}
	c := exec.Command(h.tool, append([]string{"release", step}, args...)...)
	c.Dir, c.Env, c.Stdout, c.Stderr = filepath.Join(h.lab, "home"), append(append([]string{}, h.env...), extra...), f, f
	err = c.Run()
	code := 0
	var x *exec.ExitError
	switch {
	case errors.As(err, &x):
		code = x.ExitCode()
	case err != nil:
		return 0, logp, err
	}
	h.say("step %s: exit %d (%s)", step, code, logp)
	return code, logp, nil
}

// phase runs one phase and records its result.
func (h *rehearsal) phase(name string, fn func() error) error {
	h.say("phase %s", name)
	err := fn()
	res := "pass"
	if err != nil {
		res = "fail: " + err.Error()
	}
	h.phases = append(h.phases, relPhase{Name: name, Result: res})
	return err
}

// forwardAs is a forward run: every step of the live run, then forwardChecks.
func (h *rehearsal) forwardAs(name string) error {
	if err := h.steps("precheck", "stop", "backup", "checkout", "pull", "start", "replace", "verify"); err != nil {
		return err
	}
	return h.forwardChecks(name)
}

// forwardChecks checks a forward run: the environment of the lab processes, the data, the schema,
// zero replaces of the home plugin under H, and OLD's plugin status on the lock that H
// wrote.
func (h *rehearsal) forwardChecks(name string) error {
	if err := h.checkLabEnv(); err != nil {
		return err
	}
	if err := h.checkSDK(name, relHubFile, h.r.spec.H); err != nil {
		return err
	}
	if err := h.data(name); err != nil {
		return err
	}
	if err := h.schema(name); err != nil {
		return err
	}
	labCfg, err := readPluginCmdConfig(h.cfg)
	if err != nil {
		return err
	}
	_, after, err := hubEvents(labCfg.state)
	if err != nil {
		return err
	}
	for _, ev := range after {
		if ev["event"] == "plugin_replaced" && ev["plugin"] == h.c.Name {
			return relFail("the H hub replaced the home plugin %s; the forward run expects zero replaces", h.c.Name)
		}
	}
	out, err := h.cmd(filepath.Join(h.lab, "home"), h.oldBin, "plugin", "status", "-config", h.cfg)
	if err != nil {
		return relFail("OLD's plugin status on the lock that H wrote: %v", err)
	}
	l, err := readLock(labCfg.lock)
	if err != nil {
		return err
	}
	if n := strings.Count(out, "\tcache ok\t"); n != len(l.Plugins) {
		return relFail("OLD's plugin status on the lock that H wrote: %d entries are cache ok, not %d", n, len(l.Plugins))
	}
	return nil
}

// hubDB opens the lab hub.db.
func (h *rehearsal) hubDB(readOnly bool) (*sql.DB, error) {
	p := filepath.Join(h.lab, relLabState, "hub.db")
	q := "?_pragma=busy_timeout(5000)"
	if readOnly {
		q = "?mode=ro&_pragma=busy_timeout(5000)"
	}
	return sql.Open("sqlite", "file:"+p+q)
}

// probeNS are the plugin namespaces of the probe rows: the names of the verify list.
func (h *rehearsal) probeNS() ([]string, error) {
	b, err := os.ReadFile(filepath.Join(h.run, relVerifyBack))
	if err != nil {
		return nil, err
	}
	var list []relVerifyEntry
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	var ns []string
	for _, e := range list {
		ns = append(ns, e.Name)
	}
	sort.Strings(ns)
	return ns, nil
}

// writeProbes refuses an agent binding in the lab hub.db (a lab hub never gets live bindings), then
// writes one record and one saved operation for each plugin (the r31 probe rows).
func (h *rehearsal) writeProbes() error {
	db, err := h.hubDB(false)
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM bindings").Scan(&n); err != nil {
		return relFail("the lab hub.db: %v", err)
	}
	if n != 0 {
		return relFail("the lab hub.db has %d agent bindings", n)
	}
	ns, err := h.probeNS()
	if err != nil {
		return err
	}
	for _, p := range ns {
		if _, err := db.Exec("INSERT INTO records (ns, key, value, rev) VALUES (?, ?, ?, 1)", p, relProbeKey, probeValue(p)); err != nil {
			return relFail("the probe record of %s: %v", p, err)
		}
		if _, err := db.Exec("INSERT INTO ops (ns, key, fingerprint, result) VALUES (?, ?, ?, ?)", p, relProbeOp, "relprobe-fp-"+p, probeResult(p)); err != nil {
			return relFail("the probe operation of %s: %v", p, err)
		}
	}
	return nil
}

func probeValue(ns string) string  { return `{"probe":"` + ns + `-value"}` }
func probeResult(ns string) string { return `{"probe":"` + ns + `-result"}` }

// data checks that each probe row is exactly as written.
func (h *rehearsal) data(phase string) error {
	db, err := h.hubDB(true)
	if err != nil {
		return err
	}
	defer db.Close()
	ns, err := h.probeNS()
	if err != nil {
		return err
	}
	for _, p := range ns {
		var v, fp, res string
		if err := db.QueryRow("SELECT value FROM records WHERE ns = ? AND key = ?", p, relProbeKey).Scan(&v); err != nil {
			return relFail("phase %s: the probe record of %s: %v", phase, p, err)
		}
		if err := db.QueryRow("SELECT fingerprint, result FROM ops WHERE ns = ? AND key = ?", p, relProbeOp).Scan(&fp, &res); err != nil {
			return relFail("phase %s: the probe operation of %s: %v", phase, p, err)
		}
		if v != probeValue(p) || fp != "relprobe-fp-"+p || res != probeResult(p) {
			return relFail("phase %s: the probe rows of %s changed: %q %q %q", phase, p, v, fp, res)
		}
	}
	h.say("data %s: the probe rows of %d plugins are exact", phase, len(ns))
	return nil
}

// schema records the schema of the lab hub.db: every column ("column <table>.<name> <type>
// <notnull> <default> pk=<n>") and every object of sqlite_master ("object <type> <name> on
// <table>: <sql>": tables with their constraints, indexes, triggers and views). After "before"
// (OLD's own schema), a column of OLD that changed or went fails: OLD cannot run on it. Every
// other difference is a reason for the full tier (schemaChanges).
func (h *rehearsal) schema(phase string) error {
	db, err := h.hubDB(true)
	if err != nil {
		return err
	}
	defer db.Close()
	var items []string
	for _, q := range []struct {
		query  string
		format string
		n      int
	}{
		{`SELECT m.name, p.name, p.type, p."notnull", ifnull(p.dflt_value, '-'), p.pk
			FROM sqlite_master m, pragma_table_info(m.name) p WHERE m.type = 'table' ORDER BY m.name, p.cid`, "column %s.%s %s %s %s pk=%s", 6},
		{`SELECT type, name, tbl_name, ifnull(sql, '-') FROM sqlite_master ORDER BY type, name`, "object %s %s on %s: %s", 4},
	} {
		rows, err := db.Query(q.query)
		if err != nil {
			return relFail("phase %s: the schema: %v", phase, err)
		}
		for rows.Next() {
			v := make([]any, q.n)
			for i := range v {
				v[i] = new(string)
			}
			if err := rows.Scan(v...); err != nil {
				rows.Close()
				return err
			}
			s := make([]any, q.n)
			for i := range v {
				s[i] = *v[i].(*string)
			}
			items = append(items, fmt.Sprintf(q.format, s...))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	h.schemas[phase] = items
	if phase != "before" {
		var gone []string
		for _, x := range missing(h.schemas["before"], items) {
			if strings.HasPrefix(x, "column ") {
				gone = append(gone, x)
			}
		}
		if len(gone) > 0 {
			return relFail("phase %s: a column of OLD changed or went: %s", phase, strings.Join(gone, ", "))
		}
	}
	return nil
}

// schemaChanges are the schema differences of phase from "before": each column or object that H
// adds or changes, and each object that it removes.
func (h *rehearsal) schemaChanges(phase string) []string {
	var out []string
	for _, x := range missing(h.schemas[phase], h.schemas["before"]) {
		out = append(out, "H adds "+x)
	}
	for _, x := range missing(h.schemas["before"], h.schemas[phase]) {
		if strings.HasPrefix(x, "object ") {
			out = append(out, "H removes "+x)
		}
	}
	return out
}

// missing lists the items of a that b does not have.
func missing(a, b []string) []string {
	have := map[string]bool{}
	for _, x := range b {
		have[x] = true
	}
	var out []string
	for _, x := range a {
		if !have[x] {
			out = append(out, x)
		}
	}
	return out
}

// tier selects the tier from the default phases: the schema changes of H,
// the format compare, and the format files.
func (h *rehearsal) tier() (string, []string, error) {
	files, err := h.cmd(filepath.Join(h.lab, "release", "LazyRex"), append([]string{"git", "diff", "--name-only", h.c.From.Commit, h.c.To.Commit, "--"}, relFormatFiles()...)...)
	if err != nil {
		return "", nil, err
	}
	formats, err := h.formats()
	if err != nil {
		return "", nil, err
	}
	tier, reasons, err := decideTier(h.schemaChanges("forward"), strings.Fields(files), formats, h.r.rehearseTier)
	if err == nil {
		h.say("tier %s: %s", tier, strings.Join(append([]string{}, reasons...), "; "))
	}
	return tier, reasons, err
}

// decideTier: any schema change, format difference or changed format file selects full; none
// selects default. A requested full raises the tier. (A default request with a full reason cannot
// happen: -tier takes only full.)
func decideTier(schema, files, formats []string, requested string) (string, []string, error) {
	var reasons []string
	for _, c := range schema {
		reasons = append(reasons, "schema: "+c)
	}
	for _, f := range files {
		reasons = append(reasons, "format file changed: "+f)
	}
	reasons = append(reasons, formats...)
	if requested == tierFull {
		reasons = append(reasons, "requested: -tier full")
	}
	switch {
	case requested != "" && requested != tierFull:
		return "", nil, relFail("-tier takes only %s", tierFull)
	case len(reasons) > 0:
		return tierFull, reasons, nil
	}
	return tierDefault, []string{"no schema, format or format file change"}, nil
}

// formats compares the plugin commands of OLD and H on one shared scratch state:
// OLD syncs a scratch copy of the lab inputs and exports once (that makes its pull identity); the H
// scratch is a byte copy of it, with that identity. Then each binary
// runs sync, export, and pull of the forward pin document. Every output and the resulting lock
// (by its pins), base and managed file must be equal. The only normalization replaces each
// scratch directory path with "<scratch>". Any difference is a reason for the full tier.
func (h *rehearsal) formats() ([]string, error) {
	hBin := filepath.Join(h.run, "bin", "hub-H")
	root := filepath.Join(h.lab, "fmt")
	oldDir, hDir := filepath.Join(root, "old"), filepath.Join(root, "h")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		return nil, err
	}
	cfgOf := func(dir string) error {
		var cfg map[string]any
		if err := json.Unmarshal(mustReadFile(h.cfg), &cfg); err != nil {
			return err
		}
		cfg["state"] = filepath.Join(dir, relLabState)
		delete(cfg, "lock")
		delete(cfg, "managed_plugins")
		return writeJSON(filepath.Join(dir, relLabConfig), cfg)
	}
	if err := cfgOf(oldDir); err != nil {
		return nil, err
	}
	lock := strings.TrimSuffix(filepath.Join(oldDir, relLabConfig), ".json") + ".lock.json"
	if err := os.MkdirAll(filepath.Join(oldDir, relLabState), 0o700); err != nil {
		return nil, err
	}
	for _, w := range []struct {
		p string
		b []byte
	}{{lock, h.live.Lock}, {peersPath(lock), h.live.Base}, {filepath.Join(oldDir, relLabState, "plugins.json"), mustReadFile(filepath.Join(h.lab, relLabState, "plugins.json"))}} {
		if err := os.WriteFile(w.p, w.b, 0o600); err != nil {
			return nil, err
		}
	}
	home := filepath.Join(h.lab, "home")
	if _, err := h.cmd(home, h.oldBin, "plugin", "sync", "-config", filepath.Join(oldDir, relLabConfig)); err != nil {
		return nil, err
	}
	// OLD's first export makes the pull identity (a file beside the lock): the copy gives H the same one.
	if _, err := h.cmd(home, h.oldBin, "plugin", "export", "-config", filepath.Join(oldDir, relLabConfig)); err != nil {
		return nil, err
	}
	if _, err := h.cmd(home, "cp", "-Rp", oldDir, hDir); err != nil {
		return nil, err
	}
	if err := cfgOf(hDir); err != nil {
		return nil, err
	}
	// A failed or unrun command, or an unreadable result, on either side is uncertain: a reason for
	// the full tier, also when both sides fail alike.
	type result struct{ outputs, files, uncertain []string }
	runSide := func(side, bin, dir string) result {
		var res result
		cfg := filepath.Join(dir, relLabConfig)
		for _, args := range [][]string{{"plugin", "sync", "-config", cfg}, {"plugin", "export", "-config", cfg},
			{"plugin", "pull", "-config", cfg, "-file", filepath.Join(h.run, relFwdDoc)}} {
			c := exec.Command(bin, args...)
			c.Dir, c.Env = home, h.env
			out, err := c.CombinedOutput()
			code := 0
			if x := (*exec.ExitError)(nil); errors.As(err, &x) {
				code = x.ExitCode()
			} else if err != nil {
				code = -1
			}
			if code != 0 {
				res.uncertain = append(res.uncertain, fmt.Sprintf("format: %s plugin %s exited %d (%v); the compare is uncertain", side, args[1], code, err))
			}
			res.outputs = append(res.outputs, fmt.Sprintf("%s exit %d:\n%s", args[1], code, strings.ReplaceAll(string(out), dir, "<scratch>")))
		}
		lk := strings.TrimSuffix(cfg, ".json") + ".lock.json"
		lp, err := lockPinsHash(lk)
		if err != nil {
			res.uncertain = append(res.uncertain, fmt.Sprintf("format: %s lock pins: %v; the compare is uncertain", side, err))
		}
		res.files = append(res.files, fmt.Sprintf("lock pins %s %v", lp, err))
		for _, f := range []string{peersPath(lk), filepath.Join(dir, relLabState, "plugins.json")} {
			s, err := sha256File(f)
			if err != nil {
				res.uncertain = append(res.uncertain, fmt.Sprintf("format: %s %s: %v; the compare is uncertain", side, filepath.Base(f), err))
			}
			res.files = append(res.files, fmt.Sprintf("%s %s %v", filepath.Base(f), s, err))
		}
		return res
	}
	a, b := runSide("OLD", h.oldBin, oldDir), runSide("H", hBin, hDir)
	reasons := append(append([]string{}, a.uncertain...), b.uncertain...)
	for i := range a.outputs {
		if a.outputs[i] != b.outputs[i] {
			reasons = append(reasons, fmt.Sprintf("format: the output differs:\n OLD %s\n H   %s", a.outputs[i], b.outputs[i]))
		}
	}
	for i := range a.files {
		if a.files[i] != b.files[i] {
			reasons = append(reasons, fmt.Sprintf("format: the result differs: OLD %s, H %s", a.files[i], b.files[i]))
		}
	}
	return reasons, nil
}

func mustReadFile(p string) []byte { b, _ := os.ReadFile(p); return b }

// stopLab closes the dev pane through Rex, stops the lab server and checks that every recorded
// process is gone (labserver.go: a signal only to a recorded pid with its recorded start time). It
// runs once: a second call does nothing. It never deletes the lab.
func (h *rehearsal) stopLab() []string {
	if h.stopped || h.l == nil || h.l.srv == nil {
		return nil
	}
	h.stopped = true
	var problems []string
	if h.run != "" {
		if err := h.withLab(func(lr *releaser) error {
			panes, err := lr.devPanes()
			if err != nil {
				return err
			}
			for _, p := range panes {
				arg, _ := json.Marshal(map[string]string{"session_id": p[0], "block_id": p[1]})
				if _, err := lr.rex("api", "call", "block.close", string(arg)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			problems = append(problems, "closing the dev pane: "+err.Error())
		}
	}
	return append(problems, h.l.stop()...)
}

// removeRehearsalLab makes the lab tree writable, then deletes it.
func removeRehearsalLab(dir string) []string {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil {
		return []string{"deleting the lab: " + err.Error()}
	}
	return nil
}

// publish reads the live inputs again and writes the record only when they did not change.
func (h *rehearsal) publish(tier string, reasons []string) error {
	now, err := h.capture()
	if err != nil {
		return err
	}
	var changed []string
	for k, v := range h.start {
		if now[k] != v {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	if len(changed) > 0 {
		return relFail("a live input changed during the rehearsal: %s; no record", strings.Join(changed, ", "))
	}
	bound, err := h.r.bindings(h.tool)
	if err != nil {
		return err
	}
	rec := &relRehearsal{Pass: true, At: h.r.now().UTC().Format(time.RFC3339), Bound: bound, Tier: tier, TierReasons: reasons, Phases: h.phases, Lab: h.lab, Mirrors: h.mirrors, Commits: h.commits, SDK: h.sdk}
	if err := h.r.writeRehearsal(rec); err != nil {
		return err
	}
	h.say("PASS: tier %s; the record is %s", tier, filepath.Join(h.r.run, relRehearsalFile))
	return nil
}

// relCredentialVars are the environment names that carry a Git or GitHub credential, or a way to
// one. No lab process may have one.
func relCredentialVars() []string {
	return []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GIT_ASKPASS", "SSH_ASKPASS", "SSH_AUTH_SOCK", "GIT_SSH_COMMAND"}
}

// procEnv returns the environment of a process of this user, one "NAME=value" per line on Linux;
// on macOS, ps -E prints it after the command on one line.
func procEnv(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		return "\n" + strings.ReplaceAll(string(b), "\x00", "\n"), err
	}
	out, err := exec.Command("ps", "-Eww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	return " " + string(out), err
}

// checkLabEnv checks every live process of the lab (the lab server and its descendants, by pid and
// start time): no credential variable. The H hub must have the lab git environment
// (GIT_ALLOW_PROTOCOL=file): that shows that the environment reached the hub and its fetch children.
func (h *rehearsal) checkLabEnv() error {
	hub := h.readLabHub(relHubFile)
	if hub == nil {
		return relFail("no H hub record in the lab run")
	}
	h.l.snapshot()
	var pids []int
	for pid, start := range h.l.tracked {
		if startTime(pid) == start {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	checked, hubOK := 0, false
	for _, pid := range pids {
		env, err := procEnv(pid)
		if err != nil || startTime(pid) != h.l.tracked[pid] {
			continue // ended meanwhile
		}
		checked++
		for _, name := range relCredentialVars() {
			if strings.Contains(env, " "+name+"=") || strings.Contains(env, "\n"+name+"=") {
				return relFail("the lab process %d has the credential variable %s", pid, name)
			}
		}
		if pid == hub.PID {
			hubOK = strings.Contains(env, "GIT_ALLOW_PROTOCOL=file")
		}
	}
	if !hubOK {
		return relFail("the H hub pid %d does not show the lab git environment (GIT_ALLOW_PROTOCOL=file)", hub.PID)
	}
	h.say("the environment of %d lab processes has no credential variable; the H hub has the lab git environment", checked)
	return nil
}

// readLabHub reads a hub record of the lab run.
func (h *rehearsal) readLabHub(name string) *relHub {
	var hub relHub
	if json.Unmarshal(mustReadFile(filepath.Join(h.run, name)), &hub) != nil || hub.PID == 0 {
		return nil
	}
	return &hub
}
