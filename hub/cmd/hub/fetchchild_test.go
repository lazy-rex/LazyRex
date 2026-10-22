package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		_, err := runFetchChild(context.Background(), os.Args[1:], nil, func(w, g int) error {
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
func recordPids() (func(int, int) error, chan [2]int) {
	c := make(chan [2]int, 1)
	return func(watcher, gate int) error { c <- [2]int{watcher, gate}; return nil }, c
}

// A normal fetch: git exits 0, the watcher kills the helper that git left, every process ends,
// and the result allows the publish. One run uses a real git clone of a local repository.
func TestFetchChildNormal(t *testing.T) {
	stage := t.TempDir()
	by := bystander(t, stage)
	rec, pids := recordPids()
	res, err := runFetchChild(context.Background(), fakeGitArgv(stage, "quick"), nil, rec)
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
	res, err = runFetchChild(context.Background(), []string{"git", "clone", "-q", "file://" + src, dst}, nil, rec)
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
	go func() { res, _ := runFetchChild(ctx, fakeGitArgv(stage, "wait"), nil, rec); done <- res }()
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
	res, err := runFetchChild(ctx, fakeGitArgv(stage, "wait"), []string{fetchStopEnv + "=before-report"}, func(int, int) error {
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
		res, _ := runFetchChild(ctx, fakeGitArgv(stage, "wait"), []string{fetchStopEnv + "=after-go"}, rec)
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
