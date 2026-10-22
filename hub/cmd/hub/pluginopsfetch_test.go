package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fetchTestRepo is a plugin repository (manifest name notes) with the tag v1.0.0.
func fetchTestRepo(t *testing.T) (*testRepo, string) {
	t.Helper()
	r := newTestRepo(t)
	c := r.commit(map[string]string{"lazyrex.json": manifestJSON("notes"), "main.lua": "return 1\n", "big.txt": strings.Repeat("x", 1<<20)})
	r.git("tag", "v1.0.0")
	return r, c
}

// fetchTestHub is an ops test hub whose remote is r. Its state holds read-only cache entries, so
// the cleanup makes them writable first.
func fetchTestHub(t *testing.T, r *testRepo) *Hub {
	t.Helper()
	h, _, _ := opsTestHub(t, "")
	h.pops.cmd = pluginCmdOpts{remote: func(owner, repo string) string { return r.url() }}
	state := h.cfg.State
	t.Cleanup(func() { removeTree(state) })
	return h
}

// resolvedAdd admits an add of src and runs its resolving phase.
func resolvedAdd(t *testing.T, h *Hub, key, src string) *pluginOp {
	t.Helper()
	op, err := resolveOp(t, h, key, opRequest{Op: "add", Input: rawJSON(map[string]any{"source": src})})
	if err != nil || op.Phase != "fetching" {
		t.Fatalf("resolve: %+v %v", op, err)
	}
	return op
}

// The fetching phase through the fetch child: it stores the pin; a replay with a pin whose
// directory verifies does not fetch; a pin whose directory is gone is fetched again.
func TestPluginOpFetch(t *testing.T) {
	r, commit := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	op := resolvedAdd(t, h, "f1", "github:acme/notes@v1.0.0")
	if err := h.opFetch(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	stored := opsOf(t, h, op.ID)
	var le lockEntry
	json.Unmarshal([]byte(stored.Pin), &le)
	if stored.Phase != "locking" || le.Name != "notes" || le.Commit != commit || le.RefKind != "tag" || le.Ref != "v1.0.0" || le.Dir == "" || le.Digest == "" {
		t.Fatalf("the fetch: %+v, pin %+v", stored, le)
	}
	e := &configEntry{Source: "github:acme/notes@v1.0.0"}
	e.checkSource("")
	if _, _, err := lockedDir(h.cfg.path, h.cfg.State, &pluginLock{Plugins: map[string]*lockEntry{e.Source: &le}}, e); err != nil {
		t.Fatalf("the pin does not verify: %v", err)
	}
	// The replay with a good pin runs no git.
	h.pops.cmd.git = func(context.Context, string, func(io.Reader) error, ...string) ([]byte, error) {
		t.Error("the replay ran git")
		return nil, errors.New("no git")
	}
	stored.Phase = "fetching"
	if err := h.opFetch(context.Background(), stored); err != nil || stored.Phase != "locking" || stored.Pin != opsOf(t, h, op.ID).Pin {
		t.Fatalf("the replay: %+v %v", stored, err)
	}
	// A pin whose directory is gone: fetched again, into a new directory.
	h.pops.cmd.git = nil
	removeTree(filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(le.Dir)))
	stored.Phase = "fetching"
	if err := h.opFetch(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	var again lockEntry
	json.Unmarshal([]byte(opsOf(t, h, op.ID).Pin), &again)
	if again.Commit != commit || again.Dir == le.Dir {
		t.Fatalf("the fetch again: %+v (was %s)", again, le.Dir)
	}
	// A fetch that fails ends the op failed; the target commit is not available here.
	bad := resolvedAdd(t, h, "f2", "github:acme/other@v1.0.0")
	var tg opResolved
	json.Unmarshal([]byte(bad.Target), &tg)
	tg.Commit = strings.Repeat("1", 40)
	b, _ := json.Marshal(tg)
	bad.Target = string(b)
	if err := h.opFetch(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	if s := opsOf(t, h, bad.ID); s.Phase != "failed" || s.Pin != "" || !strings.Contains(s.Error, "the lock and the managed file are unchanged") {
		t.Fatalf("a failed fetch: %+v", s)
	}
}

// The manager's runner gives the same bytes and the same error text as execGit.
func TestFetchChildGitSameAsExec(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h, _, _ := opsTestHub(t, "")
	run := h.fetchChildGit("op", "fetching")
	ctx := context.Background()
	for _, args := range [][]string{{"rev-parse", "HEAD"}, {"rev-parse", "--verify", "--quiet", "nosuch"}, {"cat-file", "-t", "nosuch"}} {
		want, werr := execGit(ctx, r.dir, nil, args...)
		got, gerr := run(ctx, r.dir, nil, args...)
		if string(got) != string(want) || (werr == nil) != (gerr == nil) || (werr != nil && werr.Error() != gerr.Error()) {
			t.Errorf("%v: %q %v, want %q %v", args, got, gerr, want, werr)
		}
	}
}

// pausedArchive makes the archive stream stop at its first Read until resume is closed. paused
// closes when the stream stops.
func pausedArchive(t *testing.T) (paused, resume chan struct{}) {
	t.Helper()
	paused, resume = make(chan struct{}), make(chan struct{})
	var once sync.Once
	archiveRead = func(r io.Reader) io.Reader {
		return readFunc(func(p []byte) (int, error) {
			once.Do(func() { close(paused); <-resume })
			return r.Read(p)
		})
	}
	t.Cleanup(func() { archiveRead = nil })
	return paused, resume
}

type readFunc func(p []byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// shortStop makes the fetch child's stop bound short (fetchGrace 300 ms: the bound is 1.3 s).
func shortStop(t *testing.T) {
	t.Helper()
	grace := fetchGrace
	t.Cleanup(func() { fetchGrace = grace })
	fetchGrace = 300 * time.Millisecond
}

// stageEntries lists the entries of the test repository's cache directory: published commits and
// .stage-<token> directories.
func stageEntries(t *testing.T, state string) (pub, stages []string) {
	t.Helper()
	for _, n := range published(t, state) {
		if strings.HasPrefix(n, ".stage-") {
			stages = append(stages, n)
		} else {
			pub = append(pub, n)
		}
	}
	return
}

// fetchPaused runs fetchCommit with the manager's runner, cancels it while the archive stream is
// paused, and resumes the stream after resumeAfter. It returns the fetch error, the time from the
// cancel to the return, and whether the fetch returned before the resume.
func fetchPaused(t *testing.T, h *Hub, r *testRepo, commit string, resumeAfter time.Duration) (err error, took time.Duration, early bool, state string) {
	t.Helper()
	paused, resume := pausedArchive(t)
	state = cacheTemp(t)
	tmp := filepath.Join(state, "plugins", ".tmp", "x")
	os.MkdirAll(tmp, 0o700)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		err error
		at  time.Time
	}
	done := make(chan result, 1)
	go func() {
		_, err := fetchCommit(ctx, h.fetchChildGit("op", "fetching"), state, tmp, r.url(), "lazy-rex", "Messaging", "", "refs/tags/v1.0.0", commit, false)
		done <- result{err, time.Now()}
	}()
	select {
	case <-paused:
	case res := <-done:
		t.Fatalf("the fetch ended before the archive stream: %v", res.err)
	case <-time.After(20 * time.Second):
		t.Fatal("the archive stream did not start")
	}
	cancelled := time.Now()
	cancel()
	time.Sleep(resumeAfter)
	resumed := time.Now()
	close(resume)
	res := <-done
	return res.err, res.at.Sub(cancelled), res.at.Before(resumed), state
}

// A cancel while the extraction is paused, and a resume within the stop bound: the runner returns
// after the callback, nothing is published, and the stage is removed (once: by fetchCommit).
func TestFetchChildGitPausedResumed(t *testing.T) {
	shortStop(t)
	r, commit := fetchTestRepo(t)
	h, _, _ := opsTestHub(t, "")
	err, took, early, state := fetchPaused(t, h, r, commit, 300*time.Millisecond)
	if err == nil || early || took > fetchStopBound()+time.Second {
		t.Fatalf("err %v, %v after the cancel, returned before the resume: %v", err, took, early)
	}
	var running *outputRunningError
	if errors.As(err, &running) {
		t.Fatalf("the callback returned within the bound: %v", err)
	}
	if pub, stages := stageEntries(t, state); len(pub) != 0 || len(stages) != 0 {
		t.Fatalf("published %v, stages %v", pub, stages)
	}
}

// A callback that stays paused past the bound: the runner returns *outputRunningError within the
// bound, nothing is published, and the stage stays (the callback owns it). Then the test resumes
// the callback and removes the stage.
func TestFetchChildGitPausedPastBound(t *testing.T) {
	shortStop(t)
	r, commit := fetchTestRepo(t)
	h, _, _ := opsTestHub(t, "")
	err, took, early, state := fetchPaused(t, h, r, commit, fetchStopBound()+2*time.Second)
	var running *outputRunningError
	if !errors.As(err, &running) || !early || took > fetchStopBound()+time.Second {
		t.Fatalf("err %v, %v after the cancel, returned before the resume: %v", err, took, early)
	}
	pub, stages := stageEntries(t, state)
	if len(pub) != 0 || len(stages) != 1 {
		t.Fatalf("published %v, stages %v", pub, stages)
	}
}

// Output and the stderr copy of git archive both held past the bound: the typed output error, not the stderr error, and
// the stage stays.
func TestFetchChildGitOutputAndStderrHeld(t *testing.T) {
	shortStop(t)
	w := blockWriter{make(chan struct{})}
	fetchChildStderr = func(args []string, b io.Writer) io.Writer {
		if args[0] == "archive" {
			return w
		}
		return b
	}
	t.Cleanup(func() { close(w.release); fetchChildStderr = nil })
	r, commit := fetchTestRepo(t)
	h, _, _ := opsTestHub(t, "")
	t.Setenv("GIT_TRACE", "1") // after the repository: git archive writes its trace into the held sink
	err, _, _, state := fetchPaused(t, h, r, commit, fetchStopBound()+2*time.Second)
	var running *outputRunningError
	if !errors.As(err, &running) {
		t.Fatalf("err %v, want *outputRunningError", err)
	}
	if pub, stages := stageEntries(t, state); len(pub) != 0 || len(stages) != 1 {
		t.Fatalf("published %v, stages %v", pub, stages)
	}
}

// stallGit puts a git first in PATH that records its pid and sleeps on "fetch" (a remote that
// does not answer); every other command runs the real git.
func stallGit(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = fetch ]; then echo $$ > \"" + filepath.Join(dir, "git.pid") + "\"; exec sleep 60; fi\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(dir, "git.pid")
}

// A hub stop while the manager's fetch stalls in git: the manager goroutine ends within the
// hub's stop bound (10 s), no process of that fetch remains (its gate group is empty, the
// watcher is gone), and the op stays in fetching with no pin.
func TestPluginOpFetchHubStop(t *testing.T) {
	r, _ := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	pidFile := stallGit(t)
	groups := make(chan [2]int, 8)
	fetchChildRecord = func(w, g int) { groups <- [2]int{w, g} }
	t.Cleanup(func() { fetchChildRecord = nil })
	v, err := userAdmit(h, "stop", opRequest{Op: "add", Input: rawJSON(map[string]any{"source": "github:acme/notes@v1.0.0"})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	h.pops.kick = make(chan struct{}, 1)
	go func() { h.opsLoop(ctx); close(ended) }()
	h.kickOps()
	git := waitPid(t, pidFile)
	var last [2]int
	for drained := false; !drained; {
		select {
		case last = <-groups:
		default:
			drained = true
		}
	}
	stopped := time.Now()
	cancel()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the manager did not end within 10 s of the stop")
	}
	t.Logf("the manager ended %v after the stop", time.Since(stopped))
	assertGone(t, "the stalled git", git)
	assertGone(t, "the watcher", last[0])
	if err := syscall.Kill(-last[1], 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the gate group %d still has a process: %v", last[1], err)
	}
	if op := opsOf(t, h, v["op_id"].(string)); op.Phase != "fetching" || op.Pin != "" || op.Target == "" {
		t.Fatalf("the op after the stop: %+v", op)
	}
}

// A crash after the fetch and before the pin is stored: the replay fetches again (the first
// directory stays unused), and stores the second pin.
func TestPluginOpFetchCrashBeforeStore(t *testing.T) {
	r, commit := fetchTestRepo(t)
	h := fetchTestHub(t, r)
	op := resolvedAdd(t, h, "c1", "github:acme/notes@v1.0.0")
	h.pops.fault = func(p string) bool { return p == "plugin-op-after-fetch" }
	if err := h.opFetch(context.Background(), op); !errors.Is(err, errOpCrash) {
		t.Fatal(err)
	}
	if s := opsOf(t, h, op.ID); s.Pin != "" || s.Phase != "fetching" {
		t.Fatalf("the crash stored a pin: %+v", s)
	}
	h.pops.fault = nil
	op = opsOf(t, h, op.ID)
	if err := h.opFetch(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	var le lockEntry
	json.Unmarshal([]byte(opsOf(t, h, op.ID).Pin), &le)
	ents, _ := os.ReadDir(filepath.Join(h.cfg.State, "plugins", "github", "acme", "notes"))
	if le.Commit != commit || len(ents) != 2 {
		t.Fatalf("the replay: pin %+v, cache entries %d", le, len(ents))
	}
}

// fetchOpSimEnv makes the test binary a hub whose manager fetches through fetchChildGit
// (TestPluginOpFetchHubKill). Its value is "<pids file>\t<state>\t<url>".
const fetchOpSimEnv = "LAZYREX_FETCH_OPSIM"

// fetchOpSimMain runs the simulated hub when fetchOpSimEnv is set (TestMain). Each fetch child
// writes its watcher and gate to the pids file, so the file names the last one: the fetch, when
// the stalled git runs.
func fetchOpSimMain() {
	v := os.Getenv(fetchOpSimEnv)
	if v == "" {
		return
	}
	p := strings.Split(v, "\t")
	fetchChildRecord = func(w, g int) { os.WriteFile(p[0], fmt.Appendf(nil, "%d %d\n", w, g), 0o600) }
	ev, err := openEvents(filepath.Join(p[1], "events.log"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "opsim:", err)
		os.Exit(1)
	}
	h := &Hub{ev: ev}
	tmp := filepath.Join(p[1], "plugins", ".tmp", "x")
	os.MkdirAll(tmp, 0o700)
	_, err = fetchCommit(context.Background(), h.fetchChildGit("sim", "fetching"), p[1], tmp, p[2], "lazy-rex", "Messaging", "", "refs/tags/v1.0.0", strings.Repeat("0", 40), false)
	fmt.Fprintln(os.Stderr, "opsim:", err)
	os.Exit(0)
}

// A SIGKILL of the hub while the manager's fetch stalls in git: no process of that fetch remains
// (the watcher, the gate group and git). This is the manager path: the hub runs fetchCommit with
// fetchChildGit, as the fetching phase does.
func TestPluginOpFetchHubKill(t *testing.T) {
	r, _ := fetchTestRepo(t)
	pidFile := stallGit(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	state := cacheTemp(t)
	rec := filepath.Join(t.TempDir(), "pids")
	hub := exec.Command(self)
	hub.Env = append(os.Environ(), fetchOpSimEnv+"="+rec+"\t"+state+"\t"+r.url())
	hub.Stderr = os.Stderr
	retire := ownChild(t, hub)
	git := waitPid(t, pidFile)
	b, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	var w, g int
	fmt.Sscanf(string(b), "%d %d", &w, &g)
	psEvidence(t, "before the hub kill", hub.Process.Pid, w, g, git)
	retire()
	assertGone(t, "hub kill", w, g, git)
	end := time.Now().Add(2 * time.Second)
	for err := syscall.Kill(-g, 0); !errors.Is(err, syscall.ESRCH); err = syscall.Kill(-g, 0) {
		if time.Now().After(end) {
			t.Fatalf("the gate group %d still has a process: %v", g, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	psEvidence(t, "after the hub kill", w, g, git)
}

// ownChild starts cmd and registers its retirement: a SIGKILL and a reap of that child only. The
// retirement also runs when the test ends early (a Fatal before the test calls retire), and a
// second call does nothing.
func ownChild(t testing.TB, cmd *exec.Cmd) (retire func()) {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	retire = func() { once.Do(func() { cmd.Process.Kill(); cmd.Wait() }) }
	t.Cleanup(retire)
	return retire
}

// A test that ends before its own retire (here a SkipNow, which ends the test as a Fatal does)
// leaves no child: the cleanup of ownChild kills and reaps it.
func TestOwnChildEarlyEnd(t *testing.T) {
	var cmd *exec.Cmd
	t.Run("early", func(t *testing.T) {
		cmd = exec.Command("sleep", "60")
		ownChild(t, cmd)
		t.SkipNow()
	})
	if cmd.ProcessState == nil {
		t.Fatal("the child was not reaped")
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the child %d remains: %v", cmd.Process.Pid, err)
	}
}

// fetchOpKillEnv makes TestPluginOpFetchKillChild a hub with an admitted add (the child of
// TestPluginOpFetchHubKillManager). Its value is "<info file>\t<pids file>\t<url>".
const fetchOpKillEnv = "LAZYREX_FETCH_OPKILL"

// TestPluginOpFetchKillChild runs only as the child of TestPluginOpFetchHubKillManager: it admits
// an add as a user, runs the manager (opsLoop: runOp, opResolve, opFetch, opCmd), writes where
// its store and state are, and waits for its SIGKILL.
func TestPluginOpFetchKillChild(t *testing.T) {
	v := os.Getenv(fetchOpKillEnv)
	if v == "" {
		t.Skip("the child of TestPluginOpFetchHubKillManager")
	}
	p := strings.Split(v, "\t")
	h, _, _ := opsTestHub(t, "")
	h.pops.cmd = pluginCmdOpts{remote: func(owner, repo string) string { return p[2] }}
	fetchChildRecord = func(w, g int) {
		f, _ := os.OpenFile(p[1], os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		fmt.Fprintf(f, "%d %d\n", w, g)
		f.Close()
	}
	v2, err := userAdmit(h, "kill", opRequest{Op: "add", Input: rawJSON(map[string]any{"source": "github:acme/notes@v1.0.0"})})
	if err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, db string
	if err := h.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &db); err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(map[string]string{"db": db, "state": h.cfg.State, "op_id": v2["op_id"].(string)})
	if err := os.WriteFile(p[0], info, 0o600); err != nil {
		t.Fatal(err)
	}
	h.pops.kick = make(chan struct{}, 1)
	h.kickOps()
	h.opsLoop(context.Background()) // never ends: the parent sends SIGKILL
}

// A SIGKILL of the hub while an admitted add stalls in its fetch, on the manager path: the op was
// admitted, opsLoop ran resolving and stored the target, and opFetch's fetch child runs the
// stalled git. After the SIGKILL, no process of that fetch remains, and the store has the op in
// fetching with its target and no pin.
func TestPluginOpFetchHubKillManager(t *testing.T) {
	r, _ := fetchTestRepo(t)
	pidFile := stallGit(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	infoFile, pids := filepath.Join(dir, "info"), filepath.Join(dir, "pids")
	hub := exec.Command(self, "-test.run=^TestPluginOpFetchKillChild$", "-test.count=1")
	hub.Env = append(os.Environ(), fetchOpKillEnv+"="+infoFile+"\t"+pids+"\t"+r.url())
	hub.Stderr = os.Stderr
	retire := ownChild(t, hub)
	git := waitPid(t, pidFile)
	b, err := os.ReadFile(pids)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var w, g int
	fmt.Sscanf(lines[len(lines)-1], "%d %d", &w, &g) // the last fetch child: the stalled fetch
	var info map[string]string
	ib, err := os.ReadFile(infoFile)
	if err != nil || json.Unmarshal(ib, &info) != nil {
		t.Fatalf("the child's info: %q %v", ib, err)
	}
	t.Cleanup(func() { retire(); removeTree(info["state"]); os.RemoveAll(filepath.Dir(info["db"])) }) // the state after the child
	psEvidence(t, "before the hub kill", hub.Process.Pid, w, g, git)
	retire()
	assertGone(t, "hub kill", w, g, git)
	end := time.Now().Add(2 * time.Second)
	for err := syscall.Kill(-g, 0); !errors.Is(err, syscall.ESRCH); err = syscall.Kill(-g, 0) {
		if time.Now().After(end) {
			t.Fatalf("the gate group %d still has a process: %v", g, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	db, err := openDB(info["db"])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var phase, target, pin string
	if err := db.QueryRow(`SELECT phase, target, pin FROM plugin_ops WHERE op_id = ?`, info["op_id"]).Scan(&phase, &target, &pin); err != nil {
		t.Fatal(err)
	}
	if phase != "fetching" || pin != "" || !strings.Contains(target, `"source":"github:acme/notes@v1.0.0"`) {
		t.Fatalf("the stored op: phase %s, target %s, pin %q", phase, target, pin)
	}
	psEvidence(t, "after the hub kill", w, g, git)
}
