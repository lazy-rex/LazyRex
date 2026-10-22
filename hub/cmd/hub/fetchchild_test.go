package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fetchHubSimEnv makes the test binary act as a hub that runs one fetch child (TestFetchChildHubKill).
// Its value is the file where the hub records the watcher and gate pids.
const fetchHubSimEnv = "LAZYREX_FETCH_HUBSIM"

func TestMain(m *testing.M) {
	fetchChildMain()
	if path := os.Getenv(fetchHubSimEnv); path != "" {
		_, err := runFetchChild(context.Background(), fetchCmd{Argv: os.Args[1:]}, func(_ context.Context, w, g int) error {
			return os.WriteFile(path, fmt.Appendf(nil, "%d %d\n", w, g), 0o600)
		})
		fmt.Fprintln(os.Stderr, "hubsim:", err)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeGit writes its pid and starts a helper in its group, as git starts git-remote-https. With
// "quick" it exits 0 and leaves the helper running; otherwise it waits.
const fakeGit = `echo $$ > "$1/git.pid"
sleep 60 &
echo $! > "$1/helper.pid"
if [ "$2" = quick ]; then exit 0; fi
wait`

func fakeGitArgv(stage, mode string) []string {
	return []string{"/bin/sh", "-c", fakeGit, "fake-git", stage, mode}
}

// waitPid waits for a pid file and returns the pid.
func waitPid(t *testing.T, path string) int {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(b), "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

// psEvidence logs the raw process table rows of pids (ps prints only the ones that exist).
func psEvidence(t *testing.T, when string, pids ...int) {
	t.Helper()
	var list []string
	for _, p := range pids {
		if p > 0 {
			list = append(list, strconv.Itoa(p))
		}
	}
	out, _ := exec.Command("ps", "-o", "pid,ppid,pgid,stat,command", "-p", strings.Join(list, ",")).CombinedOutput()
	t.Logf("ps %s (pids %s):\n%s", when, strings.Join(list, ","), out)
}

// assertGone checks that each pid has no process (ESRCH) within 2 s. A zombie still counts as
// present, so a pass also proves that the process was reaped.
func assertGone(t *testing.T, what string, pids ...int) {
	t.Helper()
	for _, p := range pids {
		end := time.Now().Add(2 * time.Second)
		for syscall.Kill(p, 0) == nil || !errors.Is(syscall.Kill(p, 0), syscall.ESRCH) {
			if time.Now().After(end) {
				psEvidence(t, "failure", p)
				t.Fatalf("%s: pid %d still exists", what, p)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// bystander starts a process whose argv names the stage. No stop rule may kill it.
func bystander(t *testing.T, stage string) *exec.Cmd {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", "sleep 60; : "+stage)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	return c
}

func assertAlive(t *testing.T, c *exec.Cmd) {
	t.Helper()
	if err := syscall.Kill(c.Process.Pid, 0); err != nil {
		t.Fatalf("bystander %d: %v", c.Process.Pid, err)
	}
}

// recordPids returns a record function and a channel that gets the recorded pids once.
func recordPids() (func(context.Context, int, int) error, chan [2]int) {
	c := make(chan [2]int, 1)
	return func(_ context.Context, watcher, gate int) error { c <- [2]int{watcher, gate}; return nil }, c
}

// A normal fetch: git exits 0, the watcher kills the helper that git left, every process ends,
// and the result allows the publish. One run uses a real git clone of a local repository.
func TestFetchChildNormal(t *testing.T) {
	stage := t.TempDir()
	by := bystander(t, stage)
	rec, pids := recordPids()
	res, err := runFetchChild(context.Background(), fetchCmd{Argv: fakeGitArgv(stage, "quick")}, rec)
	p := <-pids
	w, g := p[0], p[1]
	if err != nil || !res.OK() || w != res.Watcher || g != res.Gate || g == 0 {
		t.Fatalf("result %+v, %v (recorded %d %d)", res, err, w, g)
	}
	git, helper := waitPid(t, filepath.Join(stage, "git.pid")), waitPid(t, filepath.Join(stage, "helper.pid"))
	psEvidence(t, "after the fetch", w, g, git, helper, by.Process.Pid)
	assertGone(t, "normal", w, g, git, helper)
	assertAlive(t, by)

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main", src}, {"-C", src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "one"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	dst := filepath.Join(t.TempDir(), "clone")
	rec, pids = recordPids()
	res, err = runFetchChild(context.Background(), fetchCmd{Argv: []string{"git", "clone", "-q", "file://" + src, dst}}, rec)
	p = <-pids
	w, g = p[0], p[1]
	if err != nil || !res.OK() {
		t.Fatalf("real git: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); err != nil {
		t.Fatal(err)
	}
	assertGone(t, "real git", w, g)
}

// A cancel closes the watcher's stdin: the watcher kills the group, and every process ends
// within the bound. The result does not allow a publish.
func TestFetchChildCancel(t *testing.T) {
	stage := t.TempDir()
	by := bystander(t, stage)
	ctx, cancel := context.WithCancel(context.Background())
	rec, pids := recordPids()
	done := make(chan fetchResult)
	go func() { res, _ := runFetchChild(ctx, fetchCmd{Argv: fakeGitArgv(stage, "wait")}, rec); done <- res }()
	p := <-pids
	w, g := p[0], p[1]
	git, helper := waitPid(t, filepath.Join(stage, "git.pid")), waitPid(t, filepath.Join(stage, "helper.pid"))
	psEvidence(t, "before the cancel", w, g, git, helper)
	start := time.Now()
	cancel()
	res := <-done
	t.Logf("cancel to return: %v, result %+v", time.Since(start), res)
	if time.Since(start) > 5*time.Second || res.OK() || res.Stopped {
		t.Fatalf("cancel: %v %+v", time.Since(start), res)
	}
	psEvidence(t, "after the cancel", w, g, git, helper, by.Process.Pid)
	assertGone(t, "cancel", w, g, git, helper)
	assertAlive(t, by)
}

// A hub that dies (SIGKILL) closes the watcher's stdin through the kernel: the watcher ends the
// fetch without any hub action.
func TestFetchChildHubKill(t *testing.T) {
	stage := t.TempDir()
	by := bystander(t, stage)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(t.TempDir(), "pids")
	hub := exec.Command(self, fakeGitArgv(stage, "wait")...)
	hub.Env = append(os.Environ(), fetchHubSimEnv+"="+rec)
	hub.Stderr = os.Stderr
	if err := hub.Start(); err != nil {
		t.Fatal(err)
	}
	git, helper := waitPid(t, filepath.Join(stage, "git.pid")), waitPid(t, filepath.Join(stage, "helper.pid"))
	b, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	var w, g int
	fmt.Sscanf(string(b), "%d %d", &w, &g)
	psEvidence(t, "before the hub kill", hub.Process.Pid, w, g, git, helper)
	hub.Process.Signal(syscall.SIGKILL)
	hub.Wait()
	assertGone(t, "hub kill", w, g, git, helper)
	psEvidence(t, "after the hub kill", w, g, git, helper, by.Process.Pid)
	assertAlive(t, by)
}

// A watcher that stalls before it reports the gate: the hub stops it after the grace time; the
// gate gets EOF and exits; git never starts.
func TestFetchChildStalledBeforeReport(t *testing.T) {
	stage := t.TempDir()
	by := bystander(t, stage)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := runFetchChild(ctx, fetchCmd{Argv: fakeGitArgv(stage, "wait"), TestStop: "before-report"}, func(context.Context, int, int) error {
		t.Error("record called for a watcher that did not report")
		return nil
	})
	elapsed := time.Since(start)
	t.Logf("start to return: %v, result %+v, %v", elapsed, res, err)
	if !res.Stopped || res.OK() || res.Gate != 0 || elapsed > 5*time.Second+200*time.Millisecond {
		t.Fatalf("stalled before report: %v %+v", elapsed, res)
	}
	assertGone(t, "stalled before report", res.Watcher)
	// The hub never learned the gate's pid. Evidence only (never a kill rule): within 2 s no
	// process except the bystander names the stage.
	var rows []string
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		out, _ := exec.Command("ps", "-A", "-o", "pid,ppid,pgid,stat,command").Output()
		rows = rows[:0]
		for _, row := range strings.Split(string(out), "\n") {
			if f := strings.Fields(row); len(f) > 0 && strings.Contains(row, stage) && f[0] != strconv.Itoa(by.Process.Pid) {
				rows = append(rows, row)
			}
		}
		if len(rows) == 0 || time.Now().After(end) {
			break
		}
	}
	if len(rows) != 0 {
		t.Fatalf("processes of the fetch remain:\n%s", strings.Join(rows, "\n"))
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(stage, "git.pid")); !os.IsNotExist(err) {
		t.Fatalf("git ran: %v", err)
	}
	assertAlive(t, by)
}

// A watcher that stalls after git started: the hub stops it, kills the gate group (reserved by
// the stopped watcher) and the watcher, within the bound.
func TestFetchChildStalledAfterGo(t *testing.T) {
	stage := t.TempDir()
	by := bystander(t, stage)
	ctx, cancel := context.WithCancel(context.Background())
	rec, pids := recordPids()
	done := make(chan fetchResult)
	go func() {
		res, _ := runFetchChild(ctx, fetchCmd{Argv: fakeGitArgv(stage, "wait"), TestStop: "after-go"}, rec)
		done <- res
	}()
	p := <-pids
	w, g := p[0], p[1]
	git, helper := waitPid(t, filepath.Join(stage, "git.pid")), waitPid(t, filepath.Join(stage, "helper.pid"))
	psEvidence(t, "stalled watcher (state T)", w, g, git, helper)
	start := time.Now()
	cancel()
	res := <-done
	elapsed := time.Since(start)
	t.Logf("cancel to return: %v, result %+v", elapsed, res)
	if !res.Stopped || res.OK() || elapsed > 5*time.Second {
		t.Fatalf("stalled after go: %v %+v", elapsed, res)
	}
	assertGone(t, "stalled after go", w, g, git, helper)
	psEvidence(t, "after the hard stop", w, g, git, helper, by.Process.Pid)
	assertAlive(t, by)
}

// psField returns one ps field of pid ("" when the process does not exist).
func psField(pid int, field string) string {
	out, _ := exec.Command("ps", "-o", field+"=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out))
}

// A watcher that stalls after it killed the gate group and before it exits: the gate is still its
// unreaped child (a zombie), so the number G is reserved when the hub's hard stop kills -G. The
// test checks the reservation (gate state Z, parent W) at the moment of the hard stop.
func TestFetchChildStalledBeforeExit(t *testing.T) {
	stage := t.TempDir()
	by := bystander(t, stage)
	ctx, cancel := context.WithCancel(context.Background())
	rec, pids := recordPids()
	done := make(chan fetchResult)
	go func() {
		res, _ := runFetchChild(ctx, fetchCmd{Argv: fakeGitArgv(stage, "quick"), TestStop: "before-exit"}, rec)
		done <- res
	}()
	p := <-pids
	w, g := p[0], p[1]
	git, helper := waitPid(t, filepath.Join(stage, "git.pid")), waitPid(t, filepath.Join(stage, "helper.pid"))
	for end := time.Now().Add(5 * time.Second); !strings.HasPrefix(psField(w, "stat"), "T"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("watcher %d did not stop: %q", w, psField(w, "stat"))
		}
	}
	if st, ppid := psField(g, "stat"), psField(g, "ppid"); !strings.HasPrefix(st, "Z") || ppid != strconv.Itoa(w) {
		psEvidence(t, "failure", w, g)
		t.Fatalf("gate %d: stat %q ppid %q, want a zombie child of %d", g, st, ppid, w)
	}
	psEvidence(t, "stalled before exit (watcher T, gate Z)", w, g, git, helper)
	start := time.Now()
	cancel()
	res := <-done
	t.Logf("cancel to return: %v, result %+v", time.Since(start), res)
	if !res.Stopped || res.OK() || time.Since(start) > 5*time.Second {
		t.Fatalf("stalled before exit: %v %+v", time.Since(start), res)
	}
	assertGone(t, "stalled before exit", w, g, git, helper)
	assertAlive(t, by)
}

// Repeated fetches, normal and cancelled, leave no file descriptor and no goroutine of the hub
// behind. GC is off, so no finalizer closes a leaked pipe.
func TestFetchChildNoLeak(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	fds := func() int {
		e, err := os.ReadDir("/dev/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(e)
	}
	settle := func(want int, now func() int) int {
		n := now()
		for end := time.Now().Add(2 * time.Second); n > want && time.Now().Before(end); n = now() {
			time.Sleep(10 * time.Millisecond)
		}
		return n
	}
	fd0, g0 := fds(), runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		if res, err := runFetchChild(context.Background(), fetchCmd{Argv: []string{"/usr/bin/true"}}, func(context.Context, int, int) error { return nil }); err != nil || !res.OK() {
			t.Fatalf("normal %d: %+v %v", i, res, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if res, _ := runFetchChild(ctx, fetchCmd{Argv: []string{"/bin/sleep", "60"}}, func(context.Context, int, int) error { cancel(); return nil }); res.OK() {
			t.Fatalf("cancelled %d: %+v", i, res)
		}
		cancel()
		if _, err := runFetchChild(context.Background(), fetchCmd{Argv: []string{"/usr/bin/true"}}, func(context.Context, int, int) error { return errors.New("refused") }); err == nil {
			t.Fatalf("refused record %d: no error", i)
		}
		var out strings.Builder
		if res, err := runFetchChild(context.Background(), fetchCmd{Argv: []string{"/bin/echo", "x"}, Output: copyTo(&out)}, func(context.Context, int, int) error { return nil }); err != nil || !res.OK() || out.String() != "x\n" {
			t.Fatalf("stdout %d: %+v %v %q", i, res, err, out.String())
		}
	}
	fd1, g1 := fds(), settle(g0, runtime.NumGoroutine)
	t.Logf("fds %d -> %d, goroutines %d -> %d", fd0, fd1, g0, g1)
	if fd1 != fd0 || g1 > g0 {
		t.Fatalf("leak: fds %d -> %d, goroutines %d -> %d", fd0, fd1, g0, g1)
	}
}

// git's stdout reaches the hub through the watcher and the gate, in git's working directory, also
// for a large output and a real git archive.
func TestFetchChildStdout(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	none := func(context.Context, int, int) error { return nil }
	var out strings.Builder
	res, err := runFetchChild(context.Background(), fetchCmd{Argv: []string{"/bin/sh", "-c", "pwd; printf data"}, Dir: dir, Output: copyTo(&out)}, none)
	if err != nil || !res.OK() || out.String() != dir+"\ndata" {
		t.Fatalf("pwd: %+v %v %q", res, err, out.String())
	}
	var big countWriter
	res, err = runFetchChild(context.Background(), fetchCmd{Argv: []string{"/bin/sh", "-c", "head -c 4194304 /dev/zero"}, Output: copyTo(&big)}, none)
	if err != nil || !res.OK() || big != 4194304 {
		t.Fatalf("4 MiB: %+v %v %d bytes", res, err, big)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main", dir}, {"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "one"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	var tar countWriter
	res, err = runFetchChild(context.Background(), fetchCmd{Argv: []string{"git", "archive", "--format=tar", "HEAD"}, Dir: dir, Output: copyTo(&tar)}, none)
	if err != nil || !res.OK() || tar == 0 || tar%512 != 0 {
		t.Fatalf("git archive: %+v %v %d bytes", res, err, tar)
	}
}

type countWriter int

func (c *countWriter) Write(p []byte) (int, error) { *c += countWriter(len(p)); return len(p), nil }

// A stop variable in the hub's own environment does not reach the watcher: only fetchCmd.TestStop
// sets it.
func TestFetchChildInheritedStopIgnored(t *testing.T) {
	t.Setenv(fetchStopEnv, "after-go")
	start := time.Now()
	res, err := runFetchChild(context.Background(), fetchCmd{Argv: []string{"/usr/bin/true"}}, func(context.Context, int, int) error { return nil })
	if err != nil || !res.OK() || time.Since(start) > 3*time.Second {
		t.Fatalf("inherited stop: %v %+v %v", time.Since(start), res, err)
	}
}

// A record that blocks (a slow database write) does not hold off a cancel: the fetch stops, and
// git never starts.
func TestFetchChildSlowRecord(t *testing.T) {
	stage := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	inRecord := make(chan struct{})
	go func() { <-inRecord; time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	res, err := runFetchChild(ctx, fetchCmd{Argv: fakeGitArgv(stage, "wait")}, func(ctx context.Context, _, _ int) error {
		close(inRecord)
		<-ctx.Done()
		return ctx.Err()
	})
	t.Logf("cancel to return: %v, result %+v, %v", time.Since(start), res, err)
	if res.OK() || res.Stopped || time.Since(start) > 3*time.Second || !errors.Is(err, context.Canceled) {
		t.Fatalf("slow record: %v %+v %v", time.Since(start), res, err)
	}
	assertGone(t, "slow record", res.Watcher, res.Gate)
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(stage, "git.pid")); !os.IsNotExist(err) {
		t.Fatalf("git ran: %v", err)
	}
}

// copyTo is an Output that copies git's stdout to w.
func copyTo(w io.Writer) func(io.Reader) error {
	return func(r io.Reader) error {
		_, err := io.Copy(w, r)
		return err
	}
}

type fetchFailWriter struct{}

func (fetchFailWriter) Write([]byte) (int, error) { return 0, errors.New("destination failed") }

// An Output that fails, or that returns before the end, makes the fetch fail and stops git, also
// for a large output. (The first case is codex-21's regression on f11603c.)
func TestFetchChildOutputFailure(t *testing.T) {
	none := func(context.Context, int, int) error { return nil }
	res, err := runFetchChild(context.Background(), fetchCmd{Argv: []string{"/bin/echo", "archive-data"}, Output: copyTo(fetchFailWriter{})}, none)
	if err == nil || res.OK() {
		t.Fatalf("failed destination: %+v %v", res, err)
	}
	big := []string{"/bin/sh", "-c", "head -c 67108864 /dev/zero"} // 64 MiB: git blocks on a full pipe
	for _, c := range []struct {
		name   string
		output func(io.Reader) error
		want   error
	}{
		{"error after 1 KiB", func(r io.Reader) error {
			io.CopyN(io.Discard, r, 1024)
			return errors.New("consumer failed")
		}, nil},
		{"nil after 1 KiB", func(r io.Reader) error {
			io.CopyN(io.Discard, r, 1024)
			return nil
		}, errFetchOutputEarly},
	} {
		start := time.Now()
		res, err := runFetchChild(context.Background(), fetchCmd{Argv: big, Output: c.output}, none)
		t.Logf("%s: %v, %+v, %v", c.name, time.Since(start), res, err)
		if err == nil || res.OK() || time.Since(start) > 5*time.Second || (c.want != nil && !errors.Is(err, c.want)) {
			t.Fatalf("%s: %v %+v %v", c.name, time.Since(start), res, err)
		}
		assertGone(t, c.name, res.Watcher, res.Gate)
	}
}

// A cancel while Output has stopped reading: the fetch returns within the bound without waiting
// for Output. When Output reads again, its Read fails, and it ends: no copier is left.
func TestFetchChildCancelPausedOutput(t *testing.T) {
	g0 := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	paused, release, ended := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	output := func(r io.Reader) error {
		b := make([]byte, 1)
		r.Read(b)
		close(paused)
		<-release // the consumer stops reading
		_, err := io.Copy(io.Discard, r)
		ended <- err
		return err
	}
	go func() { <-paused; cancel() }()
	start := time.Now()
	res, err := runFetchChild(ctx, fetchCmd{Argv: []string{"/bin/sh", "-c", "head -c 67108864 /dev/zero"}, Output: output}, func(context.Context, int, int) error { return nil })
	t.Logf("cancel to return: %v, %+v, %v", time.Since(start), res, err)
	if res.OK() || time.Since(start) > 5*time.Second || !errors.Is(err, context.Canceled) {
		t.Fatalf("paused output: %v %+v %v", time.Since(start), res, err)
	}
	assertGone(t, "paused output", res.Watcher, res.Gate)
	close(release)
	if err := <-ended; err == nil {
		t.Fatal("the paused Output read on after the cancel without an error")
	}
	for end := time.Now().Add(2 * time.Second); runtime.NumGoroutine() > g0 && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > g0 {
		t.Fatalf("goroutines %d -> %d", g0, g)
	}
}
