package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestFakeHub is the child of the wrapper tests, not a test: the test binary runs it as a fake hub
// with FAKE_HUB_PLAN set. The plan file has one behavior for each start; FAKE_HUB_STARTS gets one
// line for each start. The fake uses the hub's own restartenv code for the ready line.
//   - fail2: exit 2, no ready line          - exit3: exit 3, no ready line
//   - ready-exit0 / ready-exit1: ready, then exit 0 or 1
//   - ready-wait: ready, then exit 0 at SIGTERM
//   - wait: no ready line, exit 0 at SIGTERM
//   - ready-ignore: ready, then ignore SIGTERM
//   - ready-other: a ready line for another state, then wait
//   - leak-ready: a descendant inherits the ready fd; ready, then exit 1
//   - noready-exit1: the ready fd closes without a line; exit 1 after 300 ms
//   - lock: take the restart lock on FAKE_LOCK_STATE, print "locked", wait
func TestFakeHub(t *testing.T) {
	plan := os.Getenv("FAKE_HUB_PLAN")
	if plan == "" {
		t.Skip("the child of the restart wrapper tests")
	}
	env, _ := takeRestartEnv()
	if s := os.Getenv("FAKE_LOCK_STATE"); s != "" {
		if _, err := takeRestartLock(s); err != nil {
			os.Stdout.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("locked\n")
		time.Sleep(time.Hour)
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM) // before the start record: a test signals after it
	b, _ := os.ReadFile(os.Getenv("FAKE_HUB_STARTS"))
	n := strings.Count(string(b), "\n")
	f, _ := os.OpenFile(os.Getenv("FAKE_HUB_STARTS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	f.WriteString("start\n")
	f.Close()
	steps, _ := os.ReadFile(plan)
	lines := strings.Fields(string(steps))
	step := lines[min(n, len(lines)-1)]
	switch step {
	case "fail2":
		os.Exit(2)
	case "exit3":
		os.Exit(3)
	case "ready-exit0":
		signalReady(env.readyFD, env.expectState)
		os.Exit(0)
	case "ready-exit1":
		signalReady(env.readyFD, env.expectState)
		os.Exit(1)
	case "ready-wait":
		signalReady(env.readyFD, env.expectState)
		<-term
		os.Exit(0)
	case "wait":
		<-term
		os.Exit(0)
	case "ready-ignore":
		signalReady(env.readyFD, env.expectState)
		time.Sleep(time.Hour)
	case "leak-ready":
		// A descendant keeps the ready fd open after this child exits.
		keep := exec.Command("/bin/sleep", "30")
		keep.ExtraFiles = []*os.File{os.NewFile(uintptr(env.readyFD), "ready")}
		keep.Start()
		os.WriteFile(os.Getenv("FAKE_HUB_STARTS")+".keep", []byte(strconv.Itoa(keep.Process.Pid)), 0o600)
		signalReady(env.readyFD, env.expectState)
		os.Exit(1)
	case "noready-exit1":
		// A failed ready write: the fd closes without a line, the hub runs on, then fails.
		syscall.Close(env.readyFD)
		time.Sleep(300 * time.Millisecond)
		os.Exit(1)
	case "ready-other":
		signalReady(env.readyFD, "/elsewhere")
		<-term
		os.Exit(0)
	}
	os.Exit(99)
}

type wrapperRun struct {
	w      *wrapper
	dir    string
	out    *bytes.Buffer
	starts string
	delays []time.Duration
	mu     sync.Mutex
	clock  time.Time
}

// newWrapperTest makes a wrapper whose child is TestFakeHub with the given plan. Backoff timers
// fire at once and are recorded; the stop wait is short.
func newWrapperTest(t *testing.T, plan ...string) *wrapperRun {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "plan"), []byte(strings.Join(plan, "\n")), 0o600)
	t.Setenv("FAKE_HUB_PLAN", filepath.Join(dir, "plan"))
	t.Setenv("FAKE_HUB_STARTS", filepath.Join(dir, "starts"))
	t.Setenv("GORACE", "atexit_sleep_ms=0") // a -race child otherwise sleeps 1 s at its exit
	wr := &wrapperRun{dir: dir, out: &bytes.Buffer{}, starts: filepath.Join(dir, "starts"),
		clock: time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)}
	wr.w = &wrapper{bin: os.Args[0], args: []string{"-test.run=^TestFakeHub$"}, out: log.New(syncWriter{&wr.mu, wr.out}, "", 0),
		now:   func() time.Time { wr.mu.Lock(); defer wr.mu.Unlock(); return wr.clock },
		after: time.After,
		backoff: func(d time.Duration) <-chan time.Time {
			wr.mu.Lock()
			wr.delays = append(wr.delays, d)
			wr.mu.Unlock()
			c := make(chan time.Time, 1)
			c <- time.Time{}
			return c
		},
		stopWait: 300 * time.Millisecond, sigs: make(chan os.Signal, 1)}
	return wr
}

type syncWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (s syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (wr *wrapperRun) startCount() int {
	b, _ := os.ReadFile(wr.starts)
	return strings.Count(string(b), "\n")
}

func (wr *wrapperRun) status(t *testing.T) restartStatus {
	t.Helper()
	var s restartStatus
	b, err := os.ReadFile(filepath.Join(wr.w.state, "restart.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return s
}

func (wr *wrapperRun) text() string {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	return wr.out.String()
}

// runAsync runs the wrapper; the channel gets its exit status.
func (wr *wrapperRun) runAsync() chan int {
	done := make(chan int, 1)
	go func() { done <- wr.w.main(filepath.Join(wr.dir, "state")) }()
	return done
}

func (wr *wrapperRun) waitPhase(t *testing.T, phase string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(filepath.Join(wr.dir, "state", "restart.json"))
		var s restartStatus
		if json.Unmarshal(b, &s) == nil && s.Phase == phase {
			return
		}
	}
	t.Fatalf("no phase %s\n%s", phase, wr.text())
}

func waitCode(t *testing.T, done chan int) int {
	t.Helper()
	select {
	case c := <-done:
		return c
	case <-time.After(20 * time.Second):
		t.Fatal("the wrapper did not exit")
		return -1
	}
}

// Before the first ready, a failure ends the wrapper with the child's status; no restart.
func TestRestartFailureBeforeFirstReady(t *testing.T) {
	wr := newWrapperTest(t, "fail2")
	if code := waitCode(t, wr.runAsync()); code != 2 {
		t.Fatalf("exit %d, want 2\n%s", code, wr.text())
	}
	if n := wr.startCount(); n != 1 {
		t.Errorf("%d starts, want 1", n)
	}
	if s := wr.status(t); s.Phase != "stopped" || s.LastExit != "status 2" {
		t.Errorf("status %+v", s)
	}
}

// Ready, then exit 0 (hub.admin.shutdown): the wrapper exits 0, one start.
func TestRestartDeliberateStop(t *testing.T) {
	wr := newWrapperTest(t, "ready-exit0")
	if code := waitCode(t, wr.runAsync()); code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, wr.text())
	}
	if n := wr.startCount(); n != 1 {
		t.Errorf("%d starts, want 1", n)
	}
	if !strings.Contains(wr.text(), "ready") {
		t.Errorf("no ready line:\n%s", wr.text())
	}
}

// Ready, then exit 1 (the Rex connection closed): restarts after 1, 2, 4, 8 s. The fifth restart
// within 10 minutes is refused: gave_up, exit 1. "Ready, then an immediate exit" counts as after
// ready, so a restarted child is not a failure before the first ready.
func TestRestartBackoffAndGiveUp(t *testing.T) {
	wr := newWrapperTest(t, "ready-exit1")
	if code := waitCode(t, wr.runAsync()); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, wr.text())
	}
	if n := wr.startCount(); n != 5 {
		t.Errorf("%d starts, want 5\n%s", n, wr.text())
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(wr.delays) != len(want) {
		t.Fatalf("delays %v, want %v", wr.delays, want)
	}
	for i := range want {
		if wr.delays[i] != want[i] {
			t.Errorf("delays %v, want %v", wr.delays, want)
		}
	}
	if s := wr.status(t); s.Phase != "gave_up" || s.Attempt != 5 || s.LastExit != "status 1" {
		t.Errorf("status %+v", s)
	}
}

// A restarted child that fails before its ready is a restart, not the end; restarts older than
// 10 minutes do not count.
func TestRestartWindow(t *testing.T) {
	wr := newWrapperTest(t, "ready-exit1", "fail2", "fail2", "fail2", "fail2", "fail2", "ready-exit0")
	wr.w.now = func() time.Time {
		wr.mu.Lock()
		defer wr.mu.Unlock()
		wr.clock = wr.clock.Add(4 * time.Minute)
		return wr.clock
	}
	if code := waitCode(t, wr.runAsync()); code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, wr.text())
	}
	if n := wr.startCount(); n != 7 {
		t.Errorf("%d starts, want 7\n%s", n, wr.text())
	}
}

// A stop signal while a child runs: SIGTERM to the child, the wrapper exits 0 and never restarts.
// The same before the ready.
func TestRestartSignalStopsChild(t *testing.T) {
	for _, step := range []string{"ready-wait", "wait"} {
		wr := newWrapperTest(t, step, "ready-exit0")
		done := wr.runAsync()
		phase := "running"
		if step == "wait" {
			phase = "starting"
		}
		wr.waitPhase(t, phase)
		for deadline := time.Now().Add(10 * time.Second); wr.startCount() == 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		wr.w.sigs <- syscall.SIGHUP
		if code := waitCode(t, done); code != 0 {
			t.Fatalf("%s: exit %d, want 0\n%s", step, code, wr.text())
		}
		if n := wr.startCount(); n != 1 {
			t.Errorf("%s: %d starts, want 1", step, n)
		}
		if s := wr.status(t); s.Phase != "stopped" {
			t.Errorf("%s: status %+v", step, s)
		}
	}
}

// A child that ignores SIGTERM is killed after the stop wait: exit 1.
func TestRestartKillsAChildThatDoesNotStop(t *testing.T) {
	wr := newWrapperTest(t, "ready-ignore")
	done := wr.runAsync()
	wr.waitPhase(t, "running")
	wr.w.sigs <- syscall.SIGTERM
	if code := waitCode(t, done); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, wr.text())
	}
	if !strings.Contains(wr.text(), "killed") {
		t.Errorf("no kill line:\n%s", wr.text())
	}
}

// A signal during backoff: no new child, exit 0 at once. A signal before the first start: no child.
func TestRestartSignalDuringBackoff(t *testing.T) {
	wr := newWrapperTest(t, "ready-exit1", "ready-exit0")
	wr.w.backoff = func(time.Duration) <-chan time.Time { return nil } // the backoff never ends by itself
	done := wr.runAsync()
	wr.waitPhase(t, "backoff")
	wr.w.sigs <- syscall.SIGTERM
	if code := waitCode(t, done); code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, wr.text())
	}
	if n := wr.startCount(); n != 1 {
		t.Errorf("%d starts, want 1", n)
	}

	wr = newWrapperTest(t, "ready-exit0")
	wr.w.sigs <- syscall.SIGTERM
	if code := waitCode(t, wr.runAsync()); code != 0 || wr.startCount() != 0 {
		t.Errorf("a signal before the first start: exit %d, %d starts", code, wr.startCount())
	}
}

// Exit 3 (the config names another state) stops the wrapper: exit 3, no restart, also after an
// earlier ready.
func TestRestartStateMismatchStops(t *testing.T) {
	wr := newWrapperTest(t, "ready-exit1", "exit3", "ready-exit0")
	if code := waitCode(t, wr.runAsync()); code != exitStateMismatch {
		t.Fatalf("exit %d, want 3\n%s", code, wr.text())
	}
	if n := wr.startCount(); n != 2 {
		t.Errorf("%d starts, want 2", n)
	}
}

// A ready line for another state is a defect: the wrapper stops the child and exits 1.
func TestRestartReadyForAnotherState(t *testing.T) {
	wr := newWrapperTest(t, "ready-other")
	if code := waitCode(t, wr.runAsync()); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, wr.text())
	}
	if !strings.Contains(wr.text(), "a defect") {
		t.Errorf("text:\n%s", wr.text())
	}
}

// A second wrapper on a state that another process owns starts no child, names the owner's pid
// and exits 1. A symlink to the state reaches the same lock.
func TestRestartSecondWrapperStartsNoChild(t *testing.T) {
	wr := newWrapperTest(t, "ready-wait")
	state := filepath.Join(wr.dir, "state")
	os.Mkdir(state, 0o700)
	canon, _ := canonicalState(state)
	holder := exec.Command(os.Args[0], "-test.run=^TestFakeHub$")
	holder.Env = append(os.Environ(), "FAKE_LOCK_STATE="+canon)
	out, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	line := make([]byte, 7)
	if _, err := out.Read(line); err != nil || string(line) != "locked\n" {
		t.Fatalf("holder: %q, %v", line, err)
	}
	link := filepath.Join(wr.dir, "link")
	os.Symlink(state, link)
	for _, dir := range []string{state, link} {
		code := wr.w.main(dir)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", dir, code)
		}
		if want := "another restart wrapper owns " + canon + " (pid "; !strings.Contains(wr.text(), want) {
			t.Errorf("%s: text %q, want %q", dir, wr.text(), want)
		}
	}
	if n := wr.startCount(); n != 0 {
		t.Errorf("%d starts, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(canon, "restart.json")); !os.IsNotExist(err) {
		t.Errorf("the refused wrapper wrote a status: %v", err)
	}
}

func TestRestartArgs(t *testing.T) {
	got := restartArgs([]string{"-config", "c.json", "-restart", "--restart=true", "-restart=1", "-restart=TRUE", "--restart=t", "-real-server"})
	if strings.Join(got, " ") != "-config c.json -real-server" {
		t.Errorf("%q", got)
	}
}

// A descendant that keeps the ready pipe open after the child's exit: the wrapper stops waiting
// after readyEOFWait, and it closes the read end and ends the reader on every return of runChild.
// No reader goroutine is left for a later restart window.
func TestRestartRetiresTheReadyReader(t *testing.T) {
	wr := newWrapperTest(t, "leak-ready", "ready-exit0")
	t.Cleanup(func() {
		if b, err := os.ReadFile(wr.starts + ".keep"); err == nil {
			pid, _ := strconv.Atoi(string(b))
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if code := waitCode(t, wr.runAsync()); code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, wr.text())
	}
	if !strings.Contains(wr.text(), "still open") {
		t.Errorf("no line about the open pipe:\n%s", wr.text())
	}
	buf := make([]byte, 1<<20)
	if n := strings.Count(string(buf[:runtime.Stack(buf, true)]), "(*wrapper).runChild.func"); n != 0 {
		t.Errorf("%d runChild goroutines are left", n)
	}
}

// A state path with a newline cannot be framed in the ready line: the wrapper refuses it before it
// makes the directory or starts a hub. A symlink to such a directory is refused after resolution.
func TestRestartRefusesANewlineInTheState(t *testing.T) {
	wr := newWrapperTest(t, "ready-exit0")
	bad := filepath.Join(wr.dir, "a\nb")
	if code := wr.w.main(bad); code != 1 || !strings.Contains(wr.text(), "newline") {
		t.Fatalf("exit %d\n%s", code, wr.text())
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Errorf("the wrapper made the directory: %v", err)
	}
	os.Mkdir(bad, 0o700)
	link := filepath.Join(wr.dir, "link")
	os.Symlink(bad, link)
	if code := wr.w.main(link); code != 1 {
		t.Errorf("a symlink to it: exit %d", code)
	}
	if n := wr.startCount(); n != 0 {
		t.Errorf("%d starts, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(bad, "restart.lock")); !os.IsNotExist(err) {
		t.Errorf("restart.lock exists: %v", err)
	}
}

// A hub whose ready write failed runs without a ready line. The wrapper has no ready limit: it
// stays in "starting" while the hub runs. The hub's exit then counts as an exit before ready, so a
// failure on the first start ends the wrapper with the hub's status, and a later one is restarted.
func TestRestartWithoutAReadyLine(t *testing.T) {
	wr := newWrapperTest(t, "noready-exit1")
	done := wr.runAsync()
	wr.waitPhase(t, "starting")
	time.Sleep(100 * time.Millisecond) // the fd is closed; the hub still runs
	if s := wr.status(t); s.Phase != "starting" {
		t.Errorf("phase %s while the hub runs without a ready line", s.Phase)
	}
	if code := waitCode(t, done); code != 1 || wr.startCount() != 1 {
		t.Fatalf("first start: exit %d, %d starts\n%s", code, wr.startCount(), wr.text())
	}
	wr = newWrapperTest(t, "ready-exit1", "noready-exit1", "ready-exit0")
	if code := waitCode(t, wr.runAsync()); code != 0 || wr.startCount() != 3 {
		t.Fatalf("after a ready: exit %d, %d starts\n%s", code, wr.startCount(), wr.text())
	}
}
