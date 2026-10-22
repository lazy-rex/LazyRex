package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestFakeWrapper is a wrapper process for the self-exec tests. The test binary runs the
// production wrapper (newWrapper, main) with the fake hub as its child, so a relaunch execs this
// test binary again, through the path that the test invoked.
func TestFakeWrapper(t *testing.T) {
	state := os.Getenv("FAKE_WRAPPER_STATE")
	if state == "" {
		t.Skip("the process of the self-exec tests")
	}
	// The exec gap made visible: an adopting wrapper waits for the test before it adopts.
	if gate := os.Getenv("FAKE_ADOPT_GATE"); gate != "" && os.Getenv("LAZYREX_RESTART_LOCK_FD") != "" {
		os.WriteFile(gate+".waiting", nil, 0o600)
		for _, err := os.Stat(gate); err != nil; _, err = os.Stat(gate) {
			time.Sleep(20 * time.Millisecond)
		}
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	w, err := newWrapper([]string{"-test.run=^TestFakeHub$"}, sigs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restart: %v\n", err)
		os.Exit(1)
	}
	w.backoff = func(time.Duration) <-chan time.Time { return time.After(0) }
	// A stalled disk: the write of the relaunch line blocks until the test makes the gate.
	if gate := os.Getenv("FAKE_LOG_STALL"); gate != "" {
		w.logHook = func(l *restartLog) {
			l.writeFile = func(f *os.File, b []byte) (int, error) {
				for _, err := os.Stat(gate); err != nil && strings.Contains(string(b), "asked for a relaunch"); _, err = os.Stat(gate) {
					time.Sleep(20 * time.Millisecond)
				}
				return f.Write(b)
			}
		}
	}
	os.Exit(w.main(state))
}

type execRun struct {
	dir, state, starts string
	cmd                *exec.Cmd
	mu                 sync.Mutex
	out                bytes.Buffer
	done               chan int
}

// startFakeWrapper starts TestFakeWrapper through bin (the test binary, or a link to it) with
// the fake hub's plan.
func startFakeWrapper(t *testing.T, bin string, env []string, plan ...string) *execRun {
	t.Helper()
	dir := t.TempDir()
	r := &execRun{dir: dir, state: filepath.Join(dir, "state"), starts: filepath.Join(dir, "starts"), done: make(chan int, 1)}
	os.WriteFile(filepath.Join(dir, "plan"), []byte(strings.Join(plan, "\n")), 0o600)
	r.cmd = exec.Command(bin, "-test.run=^TestFakeWrapper$")
	r.cmd.Env = append(os.Environ(), "FAKE_WRAPPER_STATE="+r.state, "FAKE_HUB_PLAN="+filepath.Join(dir, "plan"),
		"FAKE_HUB_STARTS="+r.starts, "GORACE=atexit_sleep_ms=0")
	r.cmd.Env = append(r.cmd.Env, env...)
	r.cmd.Stdout, r.cmd.Stderr = syncWriter{&r.mu, &r.out}, syncWriter{&r.mu, &r.out}
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		r.cmd.Wait()
		r.done <- r.cmd.ProcessState.ExitCode()
	}()
	t.Cleanup(func() {
		r.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			r.cmd.Process.Kill()
		}
	})
	return r
}

func (r *execRun) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := os.ReadFile(filepath.Join(r.state, "hub.log"))
	return r.out.String() + "\n--- hub.log:\n" + string(b)
}

func (r *execRun) startLines() []string {
	b, _ := os.ReadFile(r.starts)
	return strings.Fields(strings.ReplaceAll(string(b), "start ", ""))
}

// execs is the LAZYREX_EXEC value of each start, in order.
func (r *execRun) execs() string {
	b, _ := os.ReadFile(r.starts + ".execs")
	return strings.Join(strings.Fields(string(b)), " ")
}

func (r *execRun) status(t *testing.T) restartStatus {
	t.Helper()
	s, err := readRestartStatus(r.state)
	if err != nil {
		t.Fatalf("restart.json: %v\n%s", err, r.text())
	}
	return s
}

func (r *execRun) wait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("no %s\n%s", what, r.text())
}

func (r *execRun) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case c := <-r.done:
		r.done <- c
		return c
	case <-time.After(30 * time.Second):
		t.Fatalf("the wrapper did not exit\n%s", r.text())
		return -1
	}
}

// Exit 4 after a ready is a relaunch: the same process (pid and start time) execs itself, with no
// backoff and no restart count. Five in a row give exec 5, not "gave up". The child of the last
// generation has no wrapper variable and no fd for restart.lock.
func TestRelaunchExecsTheWrapper(t *testing.T) {
	r := startFakeWrapper(t, os.Args[0], nil, "ready-exit4", "ready-exit4", "ready-exit4", "ready-exit4", "ready-exit4", "ready-fds")
	if code := r.exitCode(t); code != 0 {
		t.Fatalf("exit %d\n%s", code, r.text())
	}
	if n := len(r.startLines()); n != 6 {
		t.Errorf("%d starts, want 6\n%s", n, r.text())
	}
	if got := r.execs(); got != "0 1 2 3 4 5" {
		t.Errorf("LAZYREX_EXEC of the starts: %q, want the exec count of each generation", got)
	}
	s := r.status(t)
	if s.PID != r.cmd.Process.Pid || s.Exec != 5 || s.Phase != "stopped" || s.LastExec == nil || s.LastExec.Target != 5 || s.LastExec.Outcome != "ok" {
		t.Errorf("status %+v, last_exec %+v; want pid %d, exec 5, stopped, last_exec 5 ok", s, s.LastExec, r.cmd.Process.Pid)
	}
	if text := r.text(); strings.Contains(text, "gave up") || strings.Contains(text, "restart in") {
		t.Errorf("a relaunch counted as a failure:\n%s", text)
	}
	if b, _ := os.ReadFile(r.starts + ".fds"); string(b) != "env=" {
		t.Errorf("the child of exec 5 inherited %q", b)
	}
}

// Exit 4 before the first ready is not a relaunch: only a ready hub accepts one. The wrapper
// stops with the child's status, as for any failure before the first ready.
func TestRelaunchBeforeReadyIsAFailure(t *testing.T) {
	wr := newWrapperTest(t, "exit4")
	wr.w.execFn = func(string, []string, []string) error { t.Error("an exec before a ready"); return nil }
	if code := waitCode(t, wr.runAsync()); code != exitRelaunch {
		t.Fatalf("exit %d, want %d\n%s", code, exitRelaunch, wr.text())
	}
	if s := wr.status(t); s.Exec != 0 || s.LastExec != nil {
		t.Errorf("status %+v", s)
	}
}

// The lock stays held across the exec: while the new wrapper waits before its adoption, a second
// wrapper is refused. Then the new wrapper adopts and starts its child.
func TestRelaunchHoldsTheLock(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "adopt")
	r := startFakeWrapper(t, os.Args[0], []string{"FAKE_ADOPT_GATE=" + gate}, "ready-exit4", "ready-wait")
	r.wait(t, "wait before the adoption", func() bool { _, err := os.Stat(gate + ".waiting"); return err == nil })
	second := newWrapperTest(t, "ready-exit0")
	if code := waitCode(t, func() chan int {
		c := make(chan int, 1)
		go func() { c <- second.w.main(r.state) }()
		return c
	}()); code != 1 || !strings.Contains(second.text(), "another restart wrapper owns") {
		t.Fatalf("a second wrapper in the exec gap: exit %d\n%s", code, second.text())
	}
	if second.startCount() != 0 {
		t.Error("the second wrapper started a child")
	}
	os.WriteFile(gate, nil, 0o600)
	r.wait(t, "child of exec 1", func() bool {
		s, err := readRestartStatus(r.state)
		return err == nil && s.Phase == "running" && s.Exec == 1 && len(r.startLines()) == 2
	})
	r.cmd.Process.Signal(syscall.SIGTERM)
	if code := r.exitCode(t); code != 0 {
		t.Fatalf("exit %d\n%s", code, r.text())
	}
}

// An exec that fails (the invoked link names a file that cannot run) while the disk stalls under
// the log starts the child again from the resolved path, the old version: the same lock, log and
// exec 0, and last_exec "failed" while that child is ready. After the disk comes back, the log has
// the error, and a later relaunch through a good link works.
func TestRelaunchExecFailureStartsTheOldHub(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "current")
	os.Symlink(os.Args[0], link)
	stall := filepath.Join(dir, "disk-back")
	r := startFakeWrapper(t, link, []string{"FAKE_LOG_STALL=" + stall}, "ready-gate-exit4", "ready-gate-exit4", "ready-exit0")
	r.wait(t, "first child", func() bool { return len(r.startLines()) == 1 })
	bad := filepath.Join(dir, "bad")
	os.WriteFile(bad, []byte("not a program\n"), 0o600) // no execute bit: the exec fails
	os.Remove(link)
	os.Symlink(bad, link)
	os.WriteFile(r.starts+".go1", nil, 0o600)
	r.wait(t, "old child after the failed exec", func() bool {
		s, err := readRestartStatus(r.state)
		return err == nil && s.Phase == "running" && len(r.startLines()) == 2
	})
	s := r.status(t)
	if s.Exec != 0 || s.LastExec == nil || s.LastExec.Target != 1 || s.LastExec.Outcome != "failed" || s.LastExec.Error == "" {
		t.Errorf("after a failed exec: status %+v, last_exec %+v", s, s.LastExec)
	}
	if lines := r.startLines(); lines[1] != lines[0] {
		t.Errorf("the second child ran %s, not the old %s", lines[1], lines[0])
	}
	if got := r.execs(); got != "0 0" {
		t.Errorf("LAZYREX_EXEC of the starts: %q; a failed exec keeps exec 0", got)
	}
	os.WriteFile(stall, nil, 0o600)
	r.wait(t, "the exec failure in the log", func() bool {
		b, _ := os.ReadFile(filepath.Join(r.state, "hub.log"))
		return strings.Contains(string(b), "exec "+link+" failed")
	})
	os.Remove(link)
	os.Symlink(os.Args[0], link)
	os.WriteFile(r.starts+".go2", nil, 0o600)
	if code := r.exitCode(t); code != 0 {
		t.Fatalf("exit %d\n%s", code, r.text())
	}
	s = r.status(t)
	if s.PID != r.cmd.Process.Pid || s.Exec != 1 || s.LastExec == nil || s.LastExec.Target != 1 || s.LastExec.Outcome != "ok" {
		t.Errorf("after the second relaunch: status %+v, last_exec %+v", s, s.LastExec)
	}
}

// A relaunch through a switched link runs the new version: the same wrapper process execs the
// file that the invoked link names now, and its child runs that file too. Before the exec, a crash
// respawn would run the old file (TestRestartRespawnKeepsTheResolvedExecutable).
func TestRelaunchRunsTheNewVersion(t *testing.T) {
	dir := t.TempDir()
	newBin := filepath.Join(dir, "new")
	if err := copyFile(os.Args[0], newBin); err != nil {
		t.Fatal(err)
	}
	os.Chmod(newBin, 0o700)
	link := filepath.Join(dir, "current")
	os.Symlink(os.Args[0], link)
	r := startFakeWrapper(t, link, nil, "ready-gate-exit4", "ready-exit0")
	r.wait(t, "first child", func() bool { return len(r.startLines()) == 1 })
	os.Remove(link)
	os.Symlink(newBin, link)
	os.WriteFile(r.starts+".go1", nil, 0o600)
	if code := r.exitCode(t); code != 0 {
		t.Fatalf("exit %d\n%s", code, r.text())
	}
	lines := r.startLines()
	want, _ := filepath.EvalSymlinks(newBin)
	if len(lines) != 2 || lines[0] == lines[1] || !sameFile(lines[1], want) {
		t.Errorf("starts %v, want the second from %s", lines, want)
	}
	if s := r.status(t); s.PID != r.cmd.Process.Pid || s.Exec != 1 {
		t.Errorf("status %+v", s)
	}
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// The inherited variables are one state: both or neither, and numbers.
func TestTakeAdoption(t *testing.T) {
	for _, c := range []struct{ fd, exec, want string }{
		{"", "", ""},
		{"5", "", "come together"},
		{"", "1", "come together"},
		{"x", "1", "not a descriptor"},
		{"2", "1", "not a descriptor"},
		{"5", "0", "not an exec count"},
		{"5", "y", "not an exec count"},
	} {
		os.Unsetenv("LAZYREX_RESTART_LOCK_FD")
		os.Unsetenv("LAZYREX_RESTART_EXEC")
		if c.fd != "" {
			t.Setenv("LAZYREX_RESTART_LOCK_FD", c.fd)
		}
		if c.exec != "" {
			t.Setenv("LAZYREX_RESTART_EXEC", c.exec)
		}
		a, err := takeAdoption()
		got := ""
		if err != nil {
			got = err.Error()
		}
		if c.want == "" && (a != nil || err != nil) || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("fd %q, exec %q: %+v, %v; want %q", c.fd, c.exec, a, err, c.want)
		}
		if _, ok := os.LookupEnv("LAZYREX_RESTART_LOCK_FD"); ok {
			t.Errorf("fd %q, exec %q: the variable stays", c.fd, c.exec)
		}
	}
}

// adoptRestartLock adopts only the read-write fd of this state's restart.lock, with a restart.json
// that names this process in phase exec with the same target. A refusal never closes an fd for
// restart.lock: the lock stays held. The fds are raw, as after an exec: no *os.File owns them, so
// no finalizer closes a reused fd number later.
func TestAdoptRestartLock(t *testing.T) {
	state := t.TempDir()
	pid := os.Getpid()
	writeExec := func(phase string, pid, target int) {
		b := fmt.Sprintf(`{"version":1,"pid":%d,"start":%q,"phase":%q,"exec":%d,"exec_target":%d}`, pid, startTime(pid), phase, target-1, target)
		os.WriteFile(filepath.Join(state, "restart.json"), []byte(b), 0o600)
	}
	lockPath := filepath.Join(state, "restart.lock")
	rw, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(uintptr(rw), syscall.F_SETLK, &lk); err != nil {
		t.Fatal(err)
	}
	ro, err := syscall.Open(lockPath, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := syscall.Open(filepath.Join(state, "other"), syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writeExec("exec", pid, 2)
	for _, c := range []struct {
		name string
		a    adoption
		want string
	}{
		{"closed fd", adoption{fd: 999, target: 2}, "is not open"},
		{"another file", adoption{fd: other, target: 2}, "is not " + lockPath},
		{"read-only", adoption{fd: ro, target: 2}, "not open for reading and writing"},
		{"target", adoption{fd: rw, target: 3}, "not the exec of this process"},
	} {
		if _, err := adoptRestartLock(state, &c.a); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := fcntl(ro, syscall.F_GETFD, 0); err != nil {
		t.Errorf("the refused read-only fd for restart.lock was closed: %v", err)
	}
	if _, err := fcntl(other, syscall.F_GETFD, 0); err == nil {
		t.Error("the fd for another file stays open")
	}
	for _, c := range []struct {
		name  string
		write func()
	}{
		{"phase", func() { writeExec("running", pid, 2) }},
		{"pid", func() { writeExec("exec", pid+1, 2) }},
		{"no file", func() { os.Remove(filepath.Join(state, "restart.json")) }},
	} {
		c.write()
		if _, err := adoptRestartLock(state, &adoption{fd: rw, target: 2}); err == nil {
			t.Errorf("%s: adopted", c.name)
		}
	}
	writeExec("exec", pid, 2)
	f, err := adoptRestartLock(state, &adoption{fd: rw, target: 2})
	if err != nil || f == nil {
		t.Fatalf("a good adoption: %v", err)
	}
	if fl, _ := fcntl(rw, syscall.F_GETFD, 0); fl&syscall.FD_CLOEXEC == 0 {
		t.Error("the adopted fd has no close-on-exec")
	}
	// Another process sees the lock held after the refusals and the adoption.
	wr := newWrapperTest(t, "ready-exit0")
	cmd := exec.Command(os.Args[0], "-test.run=^TestFakeHub$")
	cmd.Env = append(os.Environ(), "FAKE_HUB_PLAN="+filepath.Join(wr.dir, "plan"), "FAKE_HUB_STARTS="+wr.starts, "FAKE_LOCK_STATE="+state)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "another restart wrapper owns") || !strings.Contains(string(out), "pid "+strconv.Itoa(pid)) {
		t.Errorf("another process took the lock: %s", out)
	}
	f.Close() // f owns rw
	syscall.Close(ro)
}

// A failed exec while the disk stalls: the old writer is blocked in a write when the exec fails.
// The wrapper keeps that one log and writer (a flush, not a close, comes before the exec), so
// hub.log never has a second writer, the size bound holds, and the bytes stay in order.
func TestRelaunchFailureKeepsTheStalledWriter(t *testing.T) {
	wr := newWrapperTest(t, "ready-exit4", "ready-exit0")
	wr.w.invoked = "/nonexistent/wrapper"
	wr.w.execFn = func(string, []string, []string) error { return syscall.ENOENT }
	release := make(chan struct{})
	var mu sync.Mutex
	logs, active, most := 0, 0, 0
	var tl *restartLog
	wr.w.logHook = func(l *restartLog) {
		mu.Lock()
		logs++
		mu.Unlock()
		tl = l
		l.fileMax = 1024
		l.writeFile = func(f *os.File, b []byte) (int, error) {
			mu.Lock()
			active++
			most = max(most, active)
			mu.Unlock()
			if strings.Contains(string(b), "asked for a relaunch") {
				<-release // the disk stalls under this chunk
			}
			mu.Lock()
			active--
			mu.Unlock()
			return f.Write(b)
		}
	}
	if code := waitCode(t, wr.runAsync()); code != 0 {
		t.Fatalf("exit %d\n%s", code, wr.text())
	}
	close(release)
	select {
	case <-tl.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer did not end after the release")
	}
	dir := filepath.Join(wr.dir, "state")
	var all string
	for _, name := range []string{"hub.log.3", "hub.log.2", "hub.log.1", "hub.log"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && len(b) > 1024 {
			t.Errorf("%s is %d bytes, above 1024", name, len(b))
		}
		all += string(b)
	}
	i, j := strings.Index(all, "asked for a relaunch"), strings.Index(all, "exec /nonexistent/wrapper failed")
	if i < 0 || j < 0 || i > j {
		t.Errorf("log order: relaunch line at %d, failure line at %d:\n%s", i, j, all)
	}
	mu.Lock()
	defer mu.Unlock()
	if logs != 1 || most != 1 {
		t.Errorf("%d logs, %d writers at the same time; want 1 and 1", logs, most)
	}
	if s := wr.status(t); s.Exec != 0 || s.LastExec == nil || s.LastExec.Outcome != "failed" {
		t.Errorf("status %+v, last_exec %+v", s, s.LastExec)
	}
}

// A new exec attempt clears the outcome of the previous one: while the second attempt runs,
// restart.json has phase exec and no last_exec, not the first attempt's failure.
func TestRelaunchClearsTheLastOutcome(t *testing.T) {
	wr := newWrapperTest(t, "ready-exit4", "ready-exit4", "ready-exit0")
	wr.w.invoked = "/nonexistent/wrapper"
	var seen []restartStatus
	wr.w.execFn = func(string, []string, []string) error {
		s, _ := readRestartStatus(filepath.Join(wr.dir, "state"))
		seen = append(seen, s)
		return syscall.ENOENT
	}
	if code := waitCode(t, wr.runAsync()); code != 0 {
		t.Fatalf("exit %d\n%s", code, wr.text())
	}
	if len(seen) != 2 {
		t.Fatalf("%d exec attempts, want 2", len(seen))
	}
	if s := seen[1]; s.Phase != "exec" || s.ExecTarget != 1 || s.LastExec != nil {
		t.Errorf("during the second attempt: %+v, last_exec %+v", s, s.LastExec)
	}
	if s := wr.status(t); s.LastExec == nil || s.LastExec.Outcome != "failed" {
		t.Errorf("after it: last_exec %+v", s.LastExec)
	}
}
