package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake world for the hub release steps: the release clone (git), the go command, the H
// binary's plugin commands, the Rex server with the dev pane, and the hub with its plugins. It
// follows the contracts that the steps rely on: plugin pull merges against the base and writes the
// lock, then the base; hub.admin.replace and hub.admin.start follow docs/plugin-sources.md section
// 14; the hub writes hub_started and plugin events to events.jsonl.

const (
	homeSrc   = "github:lazy-rex/LazyRex/plugins/lazyrex@main"
	otherSrc  = "github:o/other@main"
	commitOld = pinCommit
	digestOld = pinDigest
	commitH   = relCommitB
	digestH   = relDigestB
)

type fakeCommit struct {
	devsh     string // the dev.sh text
	sdkLua    bool   // sdk/lua is a tree
	mainGo    bool   // hub/cmd/hub/main.go is a blob
	expectRun bool   // git grep finds "expect_run"
	onMain    bool
}

type fakeRun struct {
	commit, digest, instance, desired string
	registered, pending               bool
}

type relWorld struct {
	t                                     *testing.T
	dir, cfg, state, rel, run, bin, specP string
	commits                               map[string]*fakeCommit
	head                                  string
	dirty                                 bool
	grepErr                               string
	modFail, buildFail                    bool
	binRev, devRev                        string
	pullStop                              string // "lock": the next pull stops after the lock write
	devPane                               bool
	startFail                             bool   // the next dev pane starts no hub
	createLost                            string // the next session.new_block fails: "none" creates nothing, "late" creates the pane and hub
	creates                               int
	hubPID                                int
	hubStartT                             string
	client                                string
	nextPID, nextInst, hubN, fetchN       int
	runs                                  map[string]*fakeRun // by plugin name
	replaceCalls, startCalls              int
	replaceArgs                           []map[string]any
	loseReply                             bool // the next replace runs, and its reply is lost
	// A plugin release (newPluginWorld).
	sdkDir      string      // the -sdk directory of the hub (argv)
	toManifest  string      // the manifest of P at TO
	reach       string      // "", "off" (TO not on the branch), "bad" (git fails), or a tag commit
	perform     []string    // the answers of the next client.perform calls: refused, other, timeout, gone, ok
	performN    int         // client.perform calls
	buildRec    []string    // the build record of the dev hub
	performHook func(n int) // runs at each client.perform, after its answer is chosen
	out         *bytes.Buffer
}

func (w *relWorld) home() string { return homeSrc }

func lockEntryOf(src, name, owner, repo, subdir, commit, digest string) *lockEntry {
	return &lockEntry{lockPin: lockPin{Name: name, Owner: owner, Repo: repo, Subdir: subdir, Ref: "main", RefKind: "branch", Commit: commit, Digest: digest},
		Dir: "github/" + owner + "/" + repo + "/" + commit + ".0000000000000000"}
}

// writePins writes the lock and the base: the home plugin at lock/base, other at C.
func (w *relWorld) writePins(lock, base relPin) {
	lk := lockPath(w.cfg, "")
	l := &pluginLock{Version: 1, Plugins: map[string]*lockEntry{
		homeSrc:  lockEntryOf(homeSrc, "lazyrex", "lazy-rex", "LazyRex", "plugins/lazyrex", lock.Commit, lock.Digest),
		otherSrc: lockEntryOf(otherSrc, "other", "o", "other", "", relCommitC, relDigestC)}}
	if err := writeLock(lk, l); err != nil {
		w.t.Fatal(err)
	}
	b := &peerBase{Version: 1, Peers: map[string]map[string]basePin{pinHub: {
		homeSrc: {Commit: base.Commit, Digest: base.Digest}, otherSrc: {Commit: relCommitC, Digest: relDigestC}}}}
	if err := writePeers(lk, b); err != nil {
		w.t.Fatal(err)
	}
}

// manifest writes the cache manifest of a lock entry dir (sdk "" or "0").
func (w *relWorld) manifest(e *lockEntry, sdk string) {
	d := filepath.Join(w.state, "plugins", e.Dir, e.Subdir)
	os.MkdirAll(d, 0o700)
	m := map[string]string{"name": e.Name, "main": "main.lua"}
	if sdk != "" {
		m["sdk"] = sdk
	}
	b, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(d, "lazyrex.json"), b, 0o600)
}

func newRelWorld(t *testing.T) *relWorld {
	t.Helper()
	dir := t.TempDir()
	w := &relWorld{t: t, dir: dir, cfg: filepath.Join(dir, "hub-live.json"), state: filepath.Join(dir, "state"),
		rel: filepath.Join(dir, "release"), run: filepath.Join(dir, "run"), bin: filepath.Join(dir, "bin"), out: &bytes.Buffer{},
		nextPID: 5000, runs: map[string]*fakeRun{}}
	os.MkdirAll(w.state, 0o700)
	os.MkdirAll(filepath.Join(w.state, "dev"), 0o700)
	if db, err := sql.Open("sqlite", "file:"+filepath.Join(w.state, "hub.db")); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec("CREATE TABLE records (k TEXT)"); err != nil {
		t.Fatal(err)
	} else {
		db.Close()
	}
	os.MkdirAll(filepath.Join(w.rel, "LazyRex"), 0o700)
	os.MkdirAll(w.bin, 0o700)
	for _, n := range []string{"go", "python3"} {
		os.WriteFile(filepath.Join(w.bin, n), []byte("#!/bin/sh\n"), 0o700)
	}
	cfg := map[string]any{"server": "unix:///nowhere/rex.sock", "state": w.state, "agents": []any{map[string]string{"id": "user"}},
		"plugins": []any{map[string]string{"source": homeSrc}}}
	b, _ := json.Marshal(cfg)
	os.WriteFile(w.cfg, b, 0o600)
	os.WriteFile(filepath.Join(w.state, "plugins.json"), []byte(`{"plugins":[{"id":"other","source":"`+otherSrc+`"}]}`), 0o600)
	w.commits = map[string]*fakeCommit{
		commitOld: {devsh: "python3 -c x; hub -sdk \"$DIR/sdk/lua\"", sdkLua: true, mainGo: true, expectRun: true, onMain: true},
		commitH:   {devsh: "python3 -c x; hub -sdk \"$DIR/sdk/lua\"", sdkLua: true, mainGo: true, expectRun: true, onMain: true},
	}
	w.checkout(commitOld)
	w.writePins(relPin{commitOld, digestOld}, relPin{commitOld, digestOld})
	l, _ := readLock(lockPath(w.cfg, ""))
	for _, e := range l.Plugins {
		w.manifest(e, "")
	}
	w.manifest(lockEntryOf(homeSrc, "lazyrex", "lazy-rex", "LazyRex", "plugins/lazyrex", commitH, digestH), "")
	spec := map[string]any{"kind": "hub", "h": commitH, "config": w.cfg, "release_dir": w.rel, "server": "unix:///nowhere/rex.sock", "dev_log": filepath.Join(dir, "dev.log")}
	sb, _ := json.Marshal(spec)
	w.specP = filepath.Join(dir, "spec.json")
	os.WriteFile(w.specP, sb, 0o600)
	// The live shape: OLD runs in one dev pane.
	w.devPane = true
	w.startHub()
	return w
}

// checkout moves HEAD and writes the commit's dev.sh into the clone, as git would.
func (w *relWorld) checkout(c string) {
	w.head = c
	os.WriteFile(filepath.Join(w.rel, "LazyRex", "dev.sh"), []byte(w.commits[c].devsh), 0o700)
}

func (w *relWorld) event(ev map[string]any) {
	b, _ := json.Marshal(ev)
	f, _ := os.OpenFile(filepath.Join(w.state, "events.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	f.Write(append(b, '\n'))
	f.Close()
}

func (w *relWorld) lockPin(src string) relPin {
	l, err := readLock(lockPath(w.cfg, ""))
	if err != nil || l.Plugins[src] == nil {
		return relPin{}
	}
	return relPin{l.Plugins[src].Commit, l.Plugins[src].Digest}
}

// newCacheDir gives a lock entry a new cache dir with the manifest of its old one, as a fetch can
// (case 1 of the kits: the same pin in a new dir).
func (w *relWorld) newCacheDir(e *lockEntry) {
	var m struct {
		SDK string `json:"sdk"`
	}
	b, _ := readManifestBytes(filepath.Join(w.state, "plugins", e.Dir, e.Subdir))
	json.Unmarshal(b, &m)
	w.fetchN++
	e.Dir = fmt.Sprintf("github/%s/%s/%s.%016x", e.Owner, e.Repo, e.Commit, w.fetchN)
	w.manifest(e, m.SDK)
}

// refetch is the fetch of a hub start: every lock entry gets a new cache dir.
func (w *relWorld) refetch() {
	lk := lockPath(w.cfg, "")
	l, err := readLock(lk)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, e := range l.Plugins {
		w.newCacheDir(e)
	}
	if err := writeLock(lk, l); err != nil {
		w.t.Fatal(err)
	}
}

// spawn starts a plugin instance at its lock pin.
func (w *relWorld) spawn(name, src string) {
	w.nextInst++
	p := w.lockPin(src)
	id := fmt.Sprintf("inst-%d", w.nextInst)
	w.runs[name] = &fakeRun{commit: p.Commit, digest: p.Digest, instance: id, desired: "running", registered: true}
	w.event(map[string]any{"event": "plugin_spawned", "plugin": name, "instance_id": id})
	w.event(map[string]any{"event": "plugin_registered", "plugin": name, "instance_id": id, "client_id": "client:" + id})
}

// startHub starts a hub built from HEAD: it writes hub_started and starts every plugin at its pin.
func (w *relWorld) startHub() {
	w.nextPID++
	w.hubN++
	w.hubPID, w.hubStartT, w.client = w.nextPID, fmt.Sprintf("t%d", w.nextPID), fmt.Sprintf("client:hub-%d", w.hubN)
	w.devRev = w.head
	w.refetch()
	os.WriteFile(filepath.Join(w.state, "dev", "hub"), []byte("dev hub of "+w.head), 0o700)
	w.event(map[string]any{"event": "hub_started", "client_id": w.client, "pid": w.hubPID})
	w.spawn("lazyrex", homeSrc)
	w.spawn("other", otherSrc)
}

func (w *relWorld) stopHub() { w.hubPID, w.hubStartT = 0, "" }

// exec is the releaser's exec hook.
func (w *relWorld) exec(r *releaser) func(c relCmd) ([]byte, []byte, error) {
	return func(c relCmd) ([]byte, []byte, error) {
		a := c.Argv
		switch {
		case a[0] == "git" && (strings.HasSuffix(a[2], "reach.git") || a[3] == "init"):
			return w.reachGit(a[3:])
		case a[0] == "git":
			return w.git(a[3:])
		case a[0] == filepath.Join(w.state, "dev", "hub"):
			return w.hubBinCmd(a[1:])
		case a[0] == "go":
			return w.goCmd(a[1:])
		case a[0] == r.hubBin():
			return w.hubBinCmd(a[1:])
		case a[0] == "rex":
			return w.rex(a[6:])
		case a[0] == "/bin/zsh":
			return nil, nil, nil
		}
		return nil, []byte("unknown command " + a[0]), errors.New("exit status 127")
	}
}

type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

func (w *relWorld) git(a []string) ([]byte, []byte, error) {
	switch a[0] {
	case "rev-parse":
		return []byte(w.head + "\n"), nil, nil
	case "status":
		if w.dirty {
			return []byte(" M x\n"), nil, nil
		}
		return nil, nil, nil
	case "fetch":
		return nil, nil, nil
	case "merge-base":
		if c := w.commits[a[2]]; c != nil && c.onMain {
			return nil, nil, nil
		}
		return nil, nil, exitErr(1)
	case "cat-file":
		commit, path, _ := strings.Cut(a[2], ":")
		c := w.commits[commit]
		switch {
		case c == nil:
		case path == "hub/cmd/hub/main.go" && c.mainGo:
			return []byte("blob\n"), nil, nil
		case path == "sdk/lua" && c.sdkLua:
			return []byte("tree\n"), nil, nil
		}
		return nil, []byte("fatal: not a valid object"), exitErr(128)
	case "grep":
		if w.grepErr != "" {
			return nil, []byte(w.grepErr), exitErr(1)
		}
		if c := w.commits[a[4]]; c != nil && c.expectRun {
			return nil, nil, nil
		}
		return nil, nil, exitErr(1)
	case "show":
		commit, _, _ := strings.Cut(a[1], ":")
		if c := w.commits[commit]; c != nil {
			return []byte(c.devsh), nil, nil
		}
		return nil, []byte("fatal: bad object"), exitErr(128)
	case "checkout":
		w.checkout(a[3])
		return nil, nil, nil
	}
	return nil, []byte("fake git: " + strings.Join(a, " ")), exitErr(2)
}

func (w *relWorld) goCmd(a []string) ([]byte, []byte, error) {
	switch {
	case a[0] == "mod" && w.modFail:
		return nil, []byte("go: download failed"), exitErr(1)
	case a[0] == "mod":
		return nil, nil, nil
	case a[0] == "build" && w.buildFail:
		return nil, []byte("build failed"), exitErr(1)
	case a[0] == "build":
		w.binRev = w.head
		os.WriteFile(a[2], []byte("hub-H binary of "+w.head), 0o700)
		return nil, nil, nil
	}
	return nil, nil, exitErr(2)
}

// hubBinCmd is the H binary: plugin pull (the merge of section 6a for the home source) and status.
func (w *relWorld) hubBinCmd(a []string) ([]byte, []byte, error) {
	lk := lockPath(w.cfg, "")
	switch a[1] {
	case "status":
		l, _ := readLock(lk)
		var o strings.Builder
		for s := range l.Plugins {
			fmt.Fprintf(&o, "%s\tcache ok\tx\n", s)
		}
		return []byte(o.String()), nil, nil
	case "pull":
		b, _ := os.ReadFile(a[5])
		d, err := decodePinDoc(b)
		if err != nil {
			return nil, []byte(err.Error()), exitErr(1)
		}
		e := d.Entries[0]
		peer := relPin{e.Commit, e.Digest}
		l, _ := readLock(lk)
		base, _ := readPeers(lk)
		local := relPin{l.Plugins[e.Source].Commit, l.Plugins[e.Source].Digest}
		bp := base.Peers[d.Hub][e.Source]
		var outcome string
		switch {
		case local == peer:
			outcome = "unchanged"
		case local == relPin{bp.Commit, bp.Digest}:
			outcome = "taken"
			l.Plugins[e.Source].Commit, l.Plugins[e.Source].Digest = peer.Commit, peer.Digest
			w.newCacheDir(l.Plugins[e.Source])
		default:
			return []byte(fmt.Sprintf("conflict\t%s\tlocal %s\tpeer %s\n", e.Source, local.Commit, peer.Commit)), nil, exitErr(1)
		}
		writeLock(lk, l)
		if w.pullStop == "lock" {
			w.pullStop = ""
			return nil, []byte("killed after the lock write"), exitErr(137)
		}
		base.Peers[d.Hub][e.Source] = basePin{Commit: peer.Commit, Digest: peer.Digest}
		writePeers(lk, base)
		return []byte(fmt.Sprintf("%s\t%s\tlocal %s\tpeer %s\n", outcome, e.Source, local.Commit, peer.Commit)), nil, nil
	}
	return nil, nil, exitErr(2)
}

func (w *relWorld) rex(a []string) ([]byte, []byte, error) {
	if a[0] == "api" {
		var arg map[string]any
		json.Unmarshal([]byte(a[3]), &arg)
		switch a[2] {
		case "client.perform":
			return w.clientPerform(arg)
		case "session.list":
			return []byte(`{"sessions":[{"session_id":"session:1"}]}`), nil, nil
		case "session.list_blocks":
			blocks := []map[string]string{{"block_id": "block:agent", "label": "claude"}}
			if w.devPane {
				blocks = append(blocks, map[string]string{"block_id": "block:dev", "label": devPaneLabel})
			}
			b, _ := json.Marshal(map[string]any{"blocks": blocks})
			return b, nil, nil
		case "block.close":
			w.devPane = false
			w.stopHub()
			return []byte(`{}`), nil, nil
		case "session.new_block":
			w.creates++
			if lost := w.createLost; lost != "" {
				w.createLost = ""
				if lost == "late" {
					w.devPane = true
					w.startHub()
				}
				return nil, []byte("connection lost"), exitErr(1)
			}
			w.devPane = true
			if w.startFail {
				w.startFail = false
			} else {
				w.startHub()
			}
			return []byte(`{"block_id":"block:dev"}`), nil, nil
		}
	}
	if a[0] == "-C" && a[2] == "do" {
		if a[1] != w.client || w.hubPID == 0 {
			return nil, []byte("client target not found"), exitErr(1)
		}
		var args map[string]any
		if len(a) > 5 {
			json.Unmarshal([]byte(a[5]), &args)
		}
		return w.hubAction(a[3], args)
	}
	return nil, []byte("fake rex: " + strings.Join(a, " ")), exitErr(2)
}

func (w *relWorld) hubAction(action string, args map[string]any) ([]byte, []byte, error) {
	srcOf := map[string]string{"lazyrex": homeSrc, "other": otherSrc}
	switch action {
	case "hub.plugins":
		var entries []map[string]any
		for _, n := range []string{"lazyrex", "other"} {
			r := w.runs[n]
			state := "running"
			if r.instance == "" {
				state = "stopped"
			}
			entries = append(entries, map[string]any{"name": n, "source": srcOf[n], "commit": r.commit, "state": state,
				"registered": r.registered, "instance_id": r.instance, "instance_pending": r.pending, "desired": r.desired})
		}
		b, _ := json.Marshal(map[string]any{"entries": entries, "kept": []any{}})
		return b, nil, nil
	case "hub.admin.replace":
		w.replaceCalls++
		w.replaceArgs = append(w.replaceArgs, args)
		n, _ := args["plugin"].(string)
		r := w.runs[n]
		if r.instance != args["expect_instance"] {
			return nil, []byte("instance_changed"), exitErr(1)
		}
		cand := w.lockPin(srcOf[n])
		if x, ok := args["expect_run"].(map[string]any); ok && (x["commit"] != cand.Commit || x["digest"] != cand.Digest) {
			return nil, []byte("candidate_changed"), exitErr(1)
		}
		w.spawn(n, srcOf[n])
		if w.loseReply {
			w.loseReply = false
			return nil, []byte("disconnected"), exitErr(1)
		}
		return []byte(`{"instance_id":"` + w.runs[n].instance + `"}`), nil, nil
	case "hub.admin.start":
		w.startCalls++
		n, _ := args["plugin"].(string)
		r := w.runs[n]
		x, _ := args["expect_run"].(map[string]any)
		if r.instance != "" {
			if x != nil && (x["commit"] != r.commit || x["digest"] != r.digest) {
				return nil, []byte("candidate_changed: plugin runs another run; nothing was started"), exitErr(1)
			}
			return []byte(`{"desired":"running"}`), nil, nil
		}
		cand := w.lockPin(srcOf[n])
		if x != nil && (x["commit"] != cand.Commit || x["digest"] != cand.Digest) {
			return nil, []byte("candidate_changed"), exitErr(1)
		}
		w.spawn(n, srcOf[n])
		return []byte(`{"desired":"running"}`), nil, nil
	}
	return nil, []byte("unknown action " + action), exitErr(1)
}

// open opens the run for one step with the world's hooks.
func (w *relWorld) open(step string) *releaser {
	w.t.Helper()
	r, err := openRun(w.run, step, w.out)
	if err != nil {
		w.t.Fatalf("open %s: %v", step, err)
	}
	r.env = []string{"PATH=" + w.bin}
	r.exec = w.exec(r)
	r.now = func() time.Time { return time.Date(2026, 10, 22, 1, 2, 3, 0, time.UTC) }
	r.sleep = func(time.Duration) {}
	r.alive = func(pid int, start string) bool { return pid == w.hubPID && start == w.hubStartT }
	r.startOf = func(pid int) string {
		if pid == w.hubPID {
			return w.hubStartT
		}
		return ""
	}
	r.exeOf = func(pid int) string {
		if pid == w.hubPID {
			return filepath.Join(w.state, "dev", "hub")
		}
		return ""
	}
	r.buildInfo = func(p string) (string, bool, error) {
		switch p {
		case r.hubBin(), r.hubBin() + ".new":
			return w.binRev, false, nil
		case filepath.Join(w.state, "dev", "hub"):
			return w.devRev, false, nil
		}
		return "", false, errors.New("no build info")
	}
	r.syncDigest = func(src string) (string, error) {
		if strings.HasSuffix(src, "@"+commitH) {
			return digestH, nil
		}
		return "", errors.New("unknown source " + src)
	}
	r.argvOf = func(pid int) ([]string, error) {
		if pid != w.hubPID {
			return nil, errors.New("no such process")
		}
		argv := []string{filepath.Join(w.state, "dev", "hub"), "-config", w.cfg}
		if w.sdkDir != "" {
			argv = append(argv, "-sdk", w.sdkDir)
		}
		return argv, nil
	}
	r.buildRecord = func(p string) ([]string, error) { return w.buildRec, nil }
	r.syncManifest = func(src string) (string, []byte, error) {
		if strings.HasSuffix(src, "@"+pluginTo) {
			return pluginToDigest, []byte(w.toManifest), nil
		}
		return "", nil, errors.New("unknown source " + src)
	}
	r.tool = filepath.Join(w.dir, "tool")
	return r
}

// step runs one step and returns its error.
func (w *relWorld) step(name string) error {
	w.t.Helper()
	s, ok := relStepFor(name)
	if !ok {
		w.t.Fatalf("no step %s", name)
	}
	r := w.open(name)
	defer r.unlock()
	return s.run(r)
}

// stepStop runs a step with RELEASE_TEST_STOP at op:phase.
func (w *relWorld) stepStop(name, at string) error {
	w.t.Helper()
	s, _ := relStepFor(name)
	r := w.open(name)
	defer r.unlock()
	r.testStop = at
	return s.run(r)
}

func (w *relWorld) must(names ...string) {
	w.t.Helper()
	for _, n := range names {
		if err := w.step(n); err != nil {
			w.t.Fatalf("step %s: %v\n%s", n, err, w.out)
		}
	}
}

// freeze runs freeze and writes a passing rehearsal record for the run, so precheck can pass.
func (w *relWorld) freeze() {
	w.t.Helper()
	if err := createRun(w.run, w.specP); err != nil {
		w.t.Fatal(err)
	}
	w.must("freeze")
	os.WriteFile(filepath.Join(w.dir, "tool"), []byte("tool"), 0o700)
	r := w.open("rehearse")
	defer r.unlock()
	bound, err := r.bindings(r.tool)
	if err != nil {
		w.t.Fatal(err)
	}
	r.writeRehearsal(&relRehearsal{Pass: true, Bound: bound, Tier: tierDefault})
}

func (w *relWorld) wantPins(lock, base relPin) {
	w.t.Helper()
	if got := w.lockPin(homeSrc); got != lock {
		w.t.Errorf("home lock pin %s, want %s", got.Commit[:6], lock.Commit[:6])
	}
	b, _ := readPeers(lockPath(w.cfg, ""))
	if got := b.Peers[pinHub][homeSrc]; (relPin{got.Commit, got.Digest}) != base {
		w.t.Errorf("home base pin %s, want %s", got.Commit[:6], base.Commit[:6])
	}
}

var (
	pinOld = relPin{commitOld, digestOld}
	pinH   = relPin{commitH, digestH}
)

// The forward release and a rollback: the steps of the release kits, end to end. The home plugin
// replace is a no-op under H (zero hub.admin.replace calls); rollback leaves every pin and the
// release clone at OLD.
func TestReleaseHubForwardRollback(t *testing.T) {
	w := newRelWorld(t)
	w.freeze()
	c := mustCandidate(t, w)
	if c.From != pinOld || c.To != pinH || !c.OldExpectRun || !c.OldSDK || c.Peer != pinHub || len(c.Inherited) != 2 {
		t.Fatalf("candidate: %+v", c)
	}
	w.must("precheck", "stop", "backup", "checkout", "pull", "start", "replace", "verify")
	if w.head != commitH || w.devRev != commitH || w.runs["lazyrex"].commit != commitH {
		t.Fatalf("forward: head %s dev %s home runs %s", w.head[:6], w.devRev[:6], w.runs["lazyrex"].commit[:6])
	}
	if w.replaceCalls != 0 || w.startCalls != 1 {
		t.Errorf("forward replace: %d replace calls, %d start calls (want 0 and 1: the no-op path)", w.replaceCalls, w.startCalls)
	}
	w.wantPins(pinH, pinH)
	w.must("rollback")
	if w.head != commitOld || w.devRev != commitOld || w.runs["lazyrex"].commit != commitOld {
		t.Fatalf("rollback: head %s dev %s home runs %s", w.head[:6], w.devRev[:6], w.runs["lazyrex"].commit[:6])
	}
	w.wantPins(pinOld, pinOld)
	if _, err := os.Stat(filepath.Join(w.run, relStampFile)); err == nil {
		t.Error("the stamp is still there after the rollback")
	}
	// Forward again with the same candidate (forward, rollback, forward of the rehearsal). The hub
	// starts fetched new cache dirs for the same pins; the hold compares pins, not lock bytes.
	w.must("precheck", "stop", "backup", "checkout", "pull", "start", "replace", "verify")
	if w.head != commitH || w.runs["lazyrex"].commit != commitH {
		t.Fatalf("second forward: head %s home runs %s", w.head[:6], w.runs["lazyrex"].commit[:6])
	}
	w.wantPins(pinH, pinH)
}

func mustCandidate(t *testing.T, w *relWorld) *relCandidate {
	t.Helper()
	r := w.open("x")
	defer r.unlock()
	c, err := r.readCandidate()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Freeze refuses an H that is not on origin/main, a git grep error in the OLD tree, and
// an OLD tree without the hub; no match gives old_expect_run false.
func TestReleaseHubFreezeRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mod  func(w *relWorld)
		want string
	}{
		"not on main":  {func(w *relWorld) { w.commits[commitH].onMain = false }, "not on origin/main"},
		"grep error":   {func(w *relWorld) { w.grepErr = "fatal: unable to read blob" }, "git grep in the OLD tree failed"},
		"no main.go":   {func(w *relWorld) { w.commits[commitOld].mainGo = false }, "has no hub/cmd/hub/main.go"},
		"dirty clone":  {func(w *relWorld) { w.dirty = true }, "local changes"},
		"lock not OLD": {func(w *relWorld) { w.writePins(pinH, pinH) }, "not at OLD"},
		"H is OLD": {func(w *relWorld) {
			var s map[string]any
			json.Unmarshal(mustRead(w.specP), &s)
			s["h"] = commitOld
			b, _ := json.Marshal(s)
			os.WriteFile(w.specP, b, 0o600)
		}, "a release without a change"},
	} {
		w := newRelWorld(t)
		tc.mod(w)
		createRun(w.run, w.specP)
		if err := w.step("freeze"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	w := newRelWorld(t)
	w.commits[commitOld].expectRun = false
	w.freeze()
	if c := mustCandidate(t, w); c.OldExpectRun {
		t.Error("no expect_run match gave old_expect_run true")
	}
}

// The precheck gates: a changed hold file, a second dev pane, no rehearsal record, a set
// LAZYREX_FETCH_ROLE, a missing inherited python3, and an existing stamp each refuse.
func TestReleaseHubPrecheckRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mod  func(w *relWorld)
		want string
	}{
		"lock changed":   {func(w *relWorld) { w.writePins(pinOld, pinH) }, "frozen"},
		"config changed": {func(w *relWorld) { os.WriteFile(w.cfg, append(mustRead(w.cfg), ' '), 0o600) }, "config file differs"},
		"no rehearsal":   {func(w *relWorld) { os.Remove(filepath.Join(w.run, relRehearsalFile)) }, "no rehearsal record"},
		"no python3":     {func(w *relWorld) { os.Remove(filepath.Join(w.bin, "python3")) }, "python3 is needed"},
		"no dev pane":    {func(w *relWorld) { w.devPane = false }, "expected one"},
		"stamp exists":   {func(w *relWorld) { os.WriteFile(filepath.Join(w.run, relStampFile), []byte("s\n"), 0o600) }, "stamp exists"},
	} {
		w := newRelWorld(t)
		w.freeze()
		tc.mod(w)
		if err := w.step("precheck"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	w := newRelWorld(t)
	w.freeze()
	t.Setenv("LAZYREX_FETCH_ROLE", "watch")
	r := w.open("precheck")
	r.env = append(r.env, "LAZYREX_FETCH_ROLE=watch")
	if err := hubPrecheck(r); err == nil || !strings.Contains(err.Error(), "LAZYREX_FETCH_ROLE") {
		t.Errorf("fetch role: %v", err)
	}
	r.unlock()
}

func mustRead(p string) []byte { b, _ := os.ReadFile(p); return b }

// The SDK rollback boundary (coordinator decision): an OLD that cannot run SDK plugins refuses the
// precheck and the rollback while a configured plugin uses the SDK, and lists them.
func TestReleaseHubSDKBoundary(t *testing.T) {
	w := newRelWorld(t)
	w.commits[commitOld].devsh = "python3 -c x; hub" // no -sdk: OLD cannot run SDK plugins
	w.freeze()
	l, _ := readLock(lockPath(w.cfg, ""))
	w.manifest(l.Plugins[otherSrc], "0")
	if err := w.step("precheck"); err == nil || !strings.Contains(err.Error(), "these plugins use the SDK: other") {
		t.Fatalf("precheck: %v", err)
	}
	w.manifest(l.Plugins[otherSrc], "")
	os.WriteFile(filepath.Join(w.run, relRehearsalFile), mustRead(filepath.Join(w.run, relRehearsalFile)), 0o600)
	w.must("precheck", "stop", "backup", "checkout", "pull", "start", "replace", "verify")
	l, _ = readLock(lockPath(w.cfg, "")) // the start fetched new cache dirs
	w.manifest(l.Plugins[otherSrc], "0")
	if err := w.step("rollback"); err == nil || !strings.Contains(err.Error(), "newest release first") {
		t.Fatalf("rollback: %v", err)
	}
	w.wantPins(pinH, pinH) // nothing moved
}

// Checkout refuses an H whose dev.sh passes -sdk without sdk/lua, and changes nothing.
func TestReleaseHubCheckoutNeedsSDK(t *testing.T) {
	w := newRelWorld(t)
	w.commits[commitH].sdkLua = false
	w.freeze()
	w.must("precheck", "stop", "backup")
	if err := w.step("checkout"); err == nil || !strings.Contains(err.Error(), "no sdk/lua") || w.head != commitOld {
		t.Fatalf("checkout: %v, head %s", err, w.head[:6])
	}
}

// Cases 11 and 8: a failed build stops in state "from from" with no H binary, and the rollback
// needs neither; a pull stopped after the lock write ("to from") converges by a rerun of pull,
// and a rollback from "to from" or "from to" reaches OLD.
func TestReleaseHubPartialPull(t *testing.T) {
	w := newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout")
	w.buildFail = true
	if err := w.step("pull"); err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("pull: %v", err)
	}
	w.wantPins(pinOld, pinOld)
	w.must("rollback")
	w.wantPins(pinOld, pinOld)
	if w.head != commitOld || w.runs["lazyrex"].commit != commitOld || w.replaceCalls != 0 {
		t.Errorf("after the no-pull rollback: head %s, home %s, %d replace calls", w.head[:6], w.runs["lazyrex"].commit[:6], w.replaceCalls)
	}

	for _, tc := range []struct {
		name      string
		mid       func(w *relWorld)
		wantState string
	}{
		{"to from", func(w *relWorld) { w.pullStop = "lock" }, "to from"},
		{"from to", func(w *relWorld) {}, "from to"},
	} {
		w := newRelWorld(t)
		w.freeze()
		w.must("precheck", "stop", "backup", "checkout")
		if tc.name == "to from" {
			tc.mid(w)
			if err := w.step("pull"); err == nil {
				t.Fatal("a pull stopped after the lock write passed")
			}
		} else {
			w.must("pull")
			w.writePins(pinOld, pinH) // a rollback pull stopped after its lock write
		}
		c := mustCandidate(t, w)
		r := w.open("x")
		st, _ := r.pinState(c)
		r.unlock()
		if st != tc.wantState {
			t.Fatalf("%s: state %q", tc.name, st)
		}
		if tc.name == "to from" {
			w.must("pull") // converges
			w.wantPins(pinH, pinH)
			w.writePins(pinH, pinOld)
		}
		w.must("rollback")
		w.wantPins(pinOld, pinOld)
	}
}

// Case 12: a dev pane that starts no hub fails step start; the rollback closes that pane and starts
// OLD. Case 9: when H ended before the rollback, no hub call goes to H.
func TestReleaseHubFailedStartAndEndedH(t *testing.T) {
	w := newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout", "pull")
	w.startFail = true
	if err := w.step("start"); err == nil || !strings.Contains(err.Error(), "no new hub after 180 s") {
		t.Fatalf("start: %v", err)
	}
	if !w.devPane {
		t.Fatal("the failed start left no pane")
	}
	w.must("rollback")
	if w.head != commitOld || w.devRev != commitOld || w.runs["lazyrex"].commit != commitOld {
		t.Fatalf("after the rollback: head %s dev %s home %s", w.head[:6], w.devRev[:6], w.runs["lazyrex"].commit[:6])
	}

	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout", "pull", "start", "replace", "verify")
	w.devPane = false
	w.stopHub() // H ended
	calls := w.replaceCalls + w.startCalls
	w.must("rollback")
	if w.replaceCalls != 0 || w.startCalls != calls+1 {
		t.Errorf("rollback after H ended: %d replace, %d start calls (want 0 replace, one start check under OLD)", w.replaceCalls, w.startCalls-calls)
	}
	if w.runs["lazyrex"].commit != commitOld {
		t.Errorf("home runs %s", w.runs["lazyrex"].commit[:6])
	}
}

// Case 13: after stop, a backup that fails before its stamp leaves no change: resume starts OLD
// again. After a stamp exists, resume refuses.
func TestReleaseHubResume(t *testing.T) {
	w := newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop")
	spec := map[string]any{}
	json.Unmarshal(mustRead(filepath.Join(w.run, relSpecFile)), &spec)
	spec["absent"] = []string{w.state}
	b, _ := json.Marshal(spec)
	os.WriteFile(filepath.Join(w.run, relSpecFile), b, 0o600)
	if err := w.step("backup"); err == nil || !strings.Contains(err.Error(), "must not exist") {
		t.Fatalf("backup: %v", err)
	}
	w.must("resume")
	if !w.devPane || w.devRev != commitOld || w.hubPID == 0 {
		t.Fatal("resume did not start OLD")
	}
	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup")
	if err := w.step("resume"); err == nil || !strings.Contains(err.Error(), "use rollback") {
		t.Errorf("resume after the stamp: %v", err)
	}
}

// The replace rows. The home plugin that already runs the target makes zero replace calls;
// a plugin at from gets one call with its instance; a lost reply rerun makes no second call; a
// stop at begin reruns with the saved instance; a pending intent stops with zero calls; spawn_failed
// starts with the saved expect_run; a target that never registers fails without a call.
func TestReleaseHubReplaceRows(t *testing.T) {
	setup := func() *relWorld {
		w := newRelWorld(t)
		w.freeze()
		w.must("precheck", "stop", "backup", "checkout", "pull", "start")
		// Make the H hub run the home plugin at OLD, so a replace is needed.
		w.runs["lazyrex"].commit, w.runs["lazyrex"].digest = commitOld, digestOld
		return w
	}
	w := setup()
	a := w.runs["lazyrex"].instance
	w.must("replace")
	if w.replaceCalls != 1 || w.replaceArgs[0]["expect_instance"] != a || w.runs["lazyrex"].commit != commitH {
		t.Fatalf("one replace: %d calls %v", w.replaceCalls, w.replaceArgs)
	}
	w.must("replace", "verify")
	if w.replaceCalls != 1 {
		t.Errorf("a repeat of a done replace called again: %d", w.replaceCalls)
	}

	w = setup()
	w.loseReply = true
	if err := w.step("replace"); err == nil {
		t.Fatal("a lost reply passed")
	}
	w.must("replace")
	if w.replaceCalls != 1 {
		t.Errorf("the rerun after a lost reply called replace again: %d", w.replaceCalls)
	}

	w = setup()
	a = w.runs["lazyrex"].instance
	if err := w.stepStop("replace", "replace-"+commitH[:12]+":begin"); !errors.Is(err, errTestStop) {
		t.Fatal(err)
	}
	w.must("replace")
	if w.replaceCalls != 1 || w.replaceArgs[0]["expect_instance"] != a {
		t.Errorf("rerun after a stop at begin: %d calls %v", w.replaceCalls, w.replaceArgs)
	}

	w = setup()
	w.runs["lazyrex"].pending = true
	if err := w.step("replace"); err == nil || !strings.Contains(err.Error(), "do not retry") || w.replaceCalls != 0 {
		t.Errorf("pending: %v, %d calls", err, w.replaceCalls)
	}

	w = setup()
	if err := w.stepStop("replace", "replace-"+commitH[:12]+":begin"); !errors.Is(err, errTestStop) {
		t.Fatal(err)
	}
	w.runs["lazyrex"].instance, w.runs["lazyrex"].registered = "", false // retired, spawn failed
	w.must("replace")
	if w.replaceCalls != 0 || w.startCalls != 1 || w.runs["lazyrex"].commit != commitH {
		t.Errorf("spawn_failed: %d replace %d start, home %s", w.replaceCalls, w.startCalls, w.runs["lazyrex"].commit[:6])
	}

	w = setup()
	w.runs["lazyrex"].commit, w.runs["lazyrex"].digest, w.runs["lazyrex"].registered = commitH, digestH, false
	if err := w.step("replace"); err == nil || w.replaceCalls != 0 {
		t.Errorf("a target that never registers: %v, %d calls", err, w.replaceCalls)
	}
}

// Each mutating op stopped at begin or after, then the step run again, reaches the end
// state of an uninterrupted run, or stops before any mutation on an unresolved state.
func TestReleaseHubInterruptedSteps(t *testing.T) {
	type tc struct {
		step, op string
		before   []string
	}
	for _, c := range []tc{
		{"stop", "dev-close", []string{"precheck"}},
		{"backup", "backup-write", []string{"precheck", "stop"}},
		{"backup", "stamp", []string{"precheck", "stop"}},
		{"checkout", "checkout", []string{"precheck", "stop", "backup"}},
		{"pull", "pull-fwd", []string{"precheck", "stop", "backup", "checkout"}},
		{"start", "dev-start-forward", []string{"precheck", "stop", "backup", "checkout", "pull"}},
	} {
		for _, phase := range []string{"begin", "after"} {
			w := newRelWorld(t)
			w.freeze()
			w.must(c.before...)
			if err := w.stepStop(c.step, c.op+":"+phase); !errors.Is(err, errTestStop) {
				t.Fatalf("%s %s:%s: %v", c.step, c.op, phase, err)
			}
			if err := w.step(c.step); err != nil {
				t.Errorf("%s rerun after %s:%s: %v\n%s", c.step, c.op, phase, err, w.out)
				continue
			}
			rest := map[string][]string{"stop": {"backup", "checkout", "pull", "start", "replace", "verify"}, "backup": {"checkout", "pull", "start", "replace", "verify"},
				"checkout": {"pull", "start", "replace", "verify"}, "pull": {"start", "replace", "verify"}, "start": {"replace", "verify"}}[c.step]
			w.must(rest...)
			w.wantPins(pinH, pinH)
			if w.head != commitH || w.runs["lazyrex"].commit != commitH || w.hubN > 2 {
				t.Errorf("%s %s:%s: head %s home %s hubs %d", c.step, c.op, phase, w.head[:6], w.runs["lazyrex"].commit[:6], w.hubN)
			}
		}
	}
}

// R3 (review of c2): another writer changes the managed file after the backup. Each step that
// depends on the hold refuses before its mutation: checkout keeps OLD, pull keeps the pins, replace
// makes no call, and rollback keeps the pins at H. The first case is the reviewer's regression.
func TestReleaseHubHoldBeforeMutation(t *testing.T) {
	touch := func(w *relWorld) {
		p := filepath.Join(w.state, "plugins.json")
		if err := os.WriteFile(p, append(mustRead(p), ' '), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout")
	touch(w)
	if err := w.step("pull"); err == nil || w.lockPin(homeSrc) != pinOld {
		t.Errorf("pull: %v, home pin %s", err, w.lockPin(homeSrc).Commit[:6])
	}

	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup")
	touch(w)
	if err := w.step("checkout"); err == nil || w.head != commitOld {
		t.Errorf("checkout: %v, head %s", err, w.head[:6])
	}

	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout", "pull", "start")
	w.runs["lazyrex"].commit, w.runs["lazyrex"].digest = commitOld, digestOld
	touch(w)
	if err := w.step("replace"); err == nil || w.replaceCalls != 0 {
		t.Errorf("replace: %v, %d calls", err, w.replaceCalls)
	}

	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout", "pull", "start", "replace", "verify")
	touch(w)
	calls := w.replaceCalls + w.startCalls
	if err := w.step("rollback"); err == nil || w.replaceCalls+w.startCalls != calls || w.head != commitH {
		t.Errorf("rollback: %v, %d hub calls, head %s", err, w.replaceCalls+w.startCalls-calls, w.head[:6])
	}
	w.wantPins(pinH, pinH)
}

// R4 (review of c2): a close stopped at its begin record keeps the saved pane and hub. When
// another hub generation runs in the pane, the rerun stops and closes nothing. The stop case is
// the reviewer's regression; the rollback close follows the same rule.
func TestReleaseHubCloseKeepsSavedHub(t *testing.T) {
	w := newRelWorld(t)
	w.freeze()
	w.must("precheck")
	if err := w.stepStop("stop", "dev-close:begin"); !errors.Is(err, errTestStop) {
		t.Fatal(err)
	}
	w.startHub()
	pid := w.hubPID
	if err := w.step("stop"); err == nil || !strings.Contains(err.Error(), "began to close") || !w.devPane || w.hubPID != pid {
		t.Errorf("stop: %v, pane %v, pid %d want %d", err, w.devPane, w.hubPID, pid)
	}

	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout", "pull", "start", "replace", "verify")
	if err := w.stepStop("rollback", "dev-close-rollback:begin"); !errors.Is(err, errTestStop) {
		t.Fatal(err)
	}
	w.startHub()
	pid = w.hubPID
	if err := w.step("rollback"); err == nil || !strings.Contains(err.Error(), "began to close") || !w.devPane || w.hubPID != pid {
		t.Errorf("rollback: %v, pane %v, pid %d want %d", err, w.devPane, w.hubPID, pid)
	}
}

// R5 (review of c2): a session.new_block whose result is unknown. With no pane, the rerun stops
// with no second create (the reviewer's regression, through the step); when the create finishes
// late, the rerun adopts its hub with one create. Rollback and resume follow the same rule.
func TestReleaseHubUnknownCreate(t *testing.T) {
	forward := func() *relWorld {
		w := newRelWorld(t)
		w.freeze()
		w.must("precheck", "stop", "backup", "checkout", "pull")
		w.creates = 0
		return w
	}
	w := forward()
	w.createLost = "none"
	if err := w.step("start"); err == nil {
		t.Fatal("a lost create passed")
	}
	if err := w.step("start"); err == nil || !strings.Contains(err.Error(), "result is unknown") || w.creates != 1 {
		t.Errorf("rerun: %v, %d creates", err, w.creates)
	}
	// The create finishes now: the next rerun adopts it.
	w.devPane = true
	w.startHub()
	w.must("start", "replace", "verify")
	if w.creates != 1 || w.devRev != commitH {
		t.Errorf("late adopt: %d creates, dev %s", w.creates, w.devRev[:6])
	}

	w = forward()
	w.createLost = "late"
	if err := w.step("start"); err == nil {
		t.Fatal("a lost create reply passed")
	}
	w.must("start", "replace", "verify")
	if w.creates != 1 || w.hubN != 2 {
		t.Errorf("late create: %d creates, %d hubs", w.creates, w.hubN)
	}

	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout", "pull", "start", "replace", "verify")
	w.creates = 0
	w.createLost = "none"
	if err := w.step("rollback"); err == nil {
		t.Fatal("a lost rollback create passed")
	}
	if err := w.step("rollback"); err == nil || !strings.Contains(err.Error(), "result is unknown") || w.creates != 1 {
		t.Errorf("rollback rerun: %v, %d creates", err, w.creates)
	}

	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop")
	w.creates = 0
	w.createLost = "late"
	if err := w.step("resume"); err == nil {
		t.Fatal("a lost resume create passed")
	}
	w.must("resume")
	if w.creates != 1 || w.devRev != commitOld {
		t.Errorf("resume: %d creates, dev %s", w.creates, w.devRev[:6])
	}
}

// R5 across steps (review of a88a649): a pending create blocks the rollback before any pin or
// clone change and before a second create; a rollback rerun adopts its own late OLD hub instead of
// closing it. Both are the reviewer's regressions. A gated step refuses while a create is pending.
func TestReleaseHubPendingCreateAcrossSteps(t *testing.T) {
	w := newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout", "pull")
	w.creates = 0
	w.createLost = "none"
	if err := w.step("start"); err == nil {
		t.Fatal("start must lose its reply")
	}
	if err := w.step("rollback"); err == nil || w.creates != 1 || w.head != commitH || w.lockPin(homeSrc) != pinH {
		t.Errorf("rollback with a pending forward create: %v, %d creates, head %s, pin %s", err, w.creates, w.head[:6], w.lockPin(homeSrc).Commit[:6])
	}
	if err := w.step("replace"); err == nil || !strings.Contains(err.Error(), "is pending") {
		t.Errorf("replace with a pending create: %v", err)
	}

	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout", "pull", "start", "replace", "verify")
	w.creates = 0
	w.createLost = "late"
	if err := w.step("rollback"); err == nil {
		t.Fatal("rollback must lose its reply")
	}
	pid := w.hubPID
	if err := w.step("rollback"); err != nil || !w.devPane || w.hubPID != pid || w.creates != 1 {
		t.Errorf("rollback rerun: %v, pane %v, pid %d want %d, %d creates", err, w.devPane, w.hubPID, pid, w.creates)
	}
	w.wantPins(pinOld, pinOld)
	if _, err := os.Stat(filepath.Join(w.run, relStampFile)); err == nil {
		t.Error("the stamp is still there after the rollback")
	}

	// The rollback rerun after a failure under its own running OLD hub keeps that hub.
	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout", "pull", "start", "replace", "verify")
	if err := w.stepStop("rollback", "start-expected:begin"); !errors.Is(err, errTestStop) {
		t.Fatal(err)
	}
	pid, n := w.hubPID, w.hubN
	w.must("rollback")
	if w.hubPID != pid || w.hubN != n {
		t.Errorf("the rerun replaced the OLD hub of the rollback: pid %d want %d", w.hubPID, pid)
	}
}

// The H binary: in "from from" a failed build leaves no H binary, also when an older
// one exists. After a pin moved, a failed retry keeps the verified binary, and the
// rollback pulls the pins back with it.
func TestReleaseHubBuildKeepsVerifiedBinary(t *testing.T) {
	w := newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout")
	r := w.open("x")
	bin := r.hubBin()
	r.unlock()
	os.MkdirAll(filepath.Dir(bin), 0o700)
	os.WriteFile(bin, []byte("an older H binary"), 0o700)
	w.buildFail = true
	if err := w.step("pull"); err == nil {
		t.Fatal("a failed build passed")
	}
	if _, err := os.Stat(bin); err == nil {
		t.Error("a failed build in \"from from\" left an H binary")
	}
	w.wantPins(pinOld, pinOld)

	w = newRelWorld(t)
	w.freeze()
	w.must("precheck", "stop", "backup", "checkout")
	w.pullStop = "lock"
	if err := w.step("pull"); err == nil {
		t.Fatal("a pull stopped after the lock write passed")
	}
	w.wantPins(pinH, pinOld) // "to from"
	r = w.open("x")
	bin = r.hubBin()
	r.unlock()
	w.buildFail = true
	if err := w.step("pull"); err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("retry: %v", err)
	}
	if b, err := os.ReadFile(bin); err != nil || !strings.HasPrefix(string(b), "hub-H binary") {
		t.Fatalf("the failed retry did not keep the verified H binary: %q %v", b, err)
	}
	w.buildFail = false
	w.must("rollback")
	w.wantPins(pinOld, pinOld)
	if w.head != commitOld || w.devRev != commitOld {
		t.Errorf("rollback: head %s dev %s", w.head[:6], w.devRev[:6])
	}
}

// A lab spec runs only on a proved lab server, with every path that the steps
// write or run inside the lab; each path alone outside the lab refuses before the lock file. A spec
// without "lab" refuses a lab server; "lab" and "real" together refuse.
func TestReleaseLabSpec(t *testing.T) {
	lab, err := os.MkdirTemp("/tmp", "rexlab-release-t-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(lab)
	lab, _ = filepath.EvalSymlinks(lab)
	out := t.TempDir()
	sock := filepath.Join(lab, "rex.sock")
	server := "unix://" + sock
	// The stand-in lab server: a shell whose builtin read blocks on a pipe that the test holds. It has
	// no child process, and it exits by itself when the test closes the pipe.
	stand := exec.Command("/bin/sh", "-c", "read x", "sh", "--listen", server, "--state-dir", filepath.Join(lab, "state"))
	in, err := stand.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := stand.Start(); err != nil {
		t.Fatal(err)
	}
	pid, start := stand.Process.Pid, startTime(stand.Process.Pid)
	defer func() {
		if kids := childPIDs(t, pid); len(kids) > 0 {
			t.Errorf("the stand-in lab server has child processes %v", kids)
		}
		in.Close()
		done := make(chan error, 1)
		go func() { done <- stand.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the stand-in lab server did not exit 10 s after its stdin closed")
			stand.Process.Kill()
			<-done
		}
		if startTime(pid) == start {
			t.Errorf("the stand-in lab server pid %d still runs", pid)
		}
	}()
	proof, _ := json.Marshal(map[string]any{"socket": server, "server_pid": stand.Process.Pid, "nonce": "n"})
	os.WriteFile(filepath.Join(lab, "lab.json"), proof, 0o600)

	type tc struct {
		cfg  map[string]any
		spec map[string]any
		run  string
		want string
	}
	base := func() tc {
		return tc{
			cfg: map[string]any{"server": server, "state": filepath.Join(lab, "hub"), "agents": []any{map[string]string{"id": "user"}},
				"view": filepath.Join(lab, "view.sh"), "profiles": filepath.Join(lab, "home", "profiles.json")},
			spec: map[string]any{"kind": "hub", "h": commitH, "release_dir": filepath.Join(lab, "release"), "server": server,
				"dev_log": filepath.Join(lab, "dev.log"), "lab": true},
			run: filepath.Join(lab, "run"),
		}
	}
	n := 0
	try := func(c tc) error {
		n++
		cfg := filepath.Join(lab, fmt.Sprintf("hub-%d.json", n))
		b, _ := json.Marshal(c.cfg)
		os.WriteFile(cfg, b, 0o600)
		c.spec["config"] = cfg
		sb, _ := json.Marshal(c.spec)
		sp := filepath.Join(out, fmt.Sprintf("spec-%d.json", n))
		os.WriteFile(sp, sb, 0o600)
		run := fmt.Sprintf("%s-%d", c.run, n)
		if err := createRun(run, sp); err != nil {
			return err
		}
		r, err := openRun(run, "precheck", &bytes.Buffer{})
		if err == nil {
			r.unlock()
			return nil
		}
		if _, serr := os.Stat(strings.TrimSuffix(cfg, ".json") + ".release.lock"); serr == nil {
			t.Errorf("a refused lab spec wrote the lock file: %v", err)
		}
		return err
	}
	if err := try(base()); err != nil {
		t.Fatalf("a valid lab spec: %v", err)
	}
	for name, mod := range map[string]func(c *tc){
		"config server":    func(c *tc) { c.cfg["server"] = "unix:///tmp/other/rex.sock" },
		"state":            func(c *tc) { c.cfg["state"] = filepath.Join(out, "state") },
		"lock override":    func(c *tc) { c.cfg["lock"] = filepath.Join(out, "lock.json") },
		"managed override": func(c *tc) { c.cfg["managed_plugins"] = filepath.Join(out, "plugins.json") },
		"view":             func(c *tc) { c.cfg["view"] = filepath.Join(out, "view.sh") },
		"palette_lua":      func(c *tc) { c.cfg["palette_lua"] = filepath.Join(out, "palette.lua") },
		"release_dir":      func(c *tc) { c.spec["release_dir"] = filepath.Join(out, "release") },
		"dev_log":          func(c *tc) { c.spec["dev_log"] = filepath.Join(out, "dev.log") },
		"probes":           func(c *tc) { c.spec["probes"] = filepath.Join(out, "probes.sql") },
		"run dir":          func(c *tc) { c.run = filepath.Join(out, "run") },
		"symlink out": func(c *tc) {
			os.Symlink(out, filepath.Join(lab, "link"))
			c.spec["dev_log"] = filepath.Join(lab, "link", "dev.log")
		},
	} {
		c := base()
		mod(&c)
		if err := try(c); err == nil || !strings.Contains(err.Error(), "lab spec") {
			t.Errorf("%s: %v", name, err)
		}
	}
	c := base()
	c.spec["lab"] = false
	if err := try(c); err == nil || !strings.Contains(err.Error(), "is a lab server") {
		t.Errorf("no lab field on a lab server: %v", err)
	}
	c = base()
	c.spec["real"] = true
	if err := try(c); err == nil || !strings.Contains(err.Error(), `"lab" and "real"`) {
		t.Errorf("lab and real: %v", err)
	}
	os.Remove(filepath.Join(lab, "lab.json"))
	if err := try(base()); err == nil || !strings.Contains(err.Error(), "no lab proof") {
		t.Errorf("no proof: %v", err)
	}
}

// childPIDs lists the live child processes of pid.
func childPIDs(t *testing.T, pid int) []int {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "pid=,ppid=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var kids []int
	for _, line := range strings.Split(string(out), "\n") {
		var p, pp int
		if n, _ := fmt.Sscan(line, &p, &pp); n == 2 && pp == pid {
			kids = append(kids, p)
		}
	}
	return kids
}
