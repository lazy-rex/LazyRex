package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

type testLog struct {
	*restartLog
	dir  string
	mu   sync.Mutex
	pane *bytes.Buffer
	now  time.Time
}

// newTestLog makes a log in a new directory with a small file size; start is left to the test.
func newTestLog(t *testing.T, fileMax int64) *testLog {
	t.Helper()
	tl := &testLog{dir: t.TempDir(), pane: &bytes.Buffer{}, now: time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)}
	tl.restartLog = newRestartLog(tl.dir, syncWriter{&tl.mu, tl.pane})
	tl.fileMax = fileMax
	tl.restartLog.now = func() time.Time { tl.mu.Lock(); defer tl.mu.Unlock(); return tl.now }
	return tl
}

func (tl *testLog) advance(d time.Duration) { tl.mu.Lock(); tl.now = tl.now.Add(d); tl.mu.Unlock() }

func (tl *testLog) paneText() string { tl.mu.Lock(); defer tl.mu.Unlock(); return tl.pane.String() }

// sizes returns the size of each log file that exists, by name.
func (tl *testLog) sizes() map[string]int64 {
	out := map[string]int64{}
	for _, p := range append(tl.files(), tl.path+".4") {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			out[filepath.Base(p)] = st.Size()
		}
	}
	return out
}

func (tl *testLog) checkBound(t *testing.T) {
	t.Helper()
	for name, n := range tl.sizes() {
		if n > tl.fileMax {
			t.Errorf("%s is %d bytes, above %d", name, n, tl.fileMax)
		}
	}
	if _, ok := tl.sizes()["hub.log.4"]; ok {
		t.Error("hub.log.4 exists")
	}
}

// Output without a newline and long lines: each file stays at the size, four files at most. The
// newest bytes are in hub.log.
func TestRestartLogSizeBound(t *testing.T) {
	tl := newTestLog(t, 64<<10)
	tl.queMax = 8 << 20 // no drop here
	tl.start()
	chunk := bytes.Repeat([]byte("x"), 7<<10)
	for i := 0; i < 300; i++ { // 2.1 MB, no newline
		tl.push(chunk)
	}
	tl.push(bytes.Repeat([]byte("y"), 20<<10)) // one long line
	tl.push([]byte("end\n"))
	tl.close(5 * time.Second)
	tl.checkBound(t)
	if b, _ := os.ReadFile(tl.path); !strings.HasSuffix(string(b), "end\n") {
		t.Errorf("hub.log does not end with the newest bytes")
	}
	if len(tl.sizes()) != 4 {
		t.Errorf("files %v, want hub.log and .1 to .3", tl.sizes())
	}
}

// A stalled disk: the reader side never waits. Pushing 10 MiB finishes while the writer is blocked;
// the queue drops what does not fit, the pane gets one notice, and the drop line is written when
// the writer writes again. The drop line counts toward the file size.
func TestRestartLogStalledDisk(t *testing.T) {
	tl := newTestLog(t, 4<<20) // all output fits in hub.log and .1 to .3: the drop line is not rotated out
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	write := tl.writeFile
	tl.writeFile = func(f *os.File, b []byte) (int, error) {
		<-release
		return write(f, b)
	}
	tl.start()
	start := time.Now()
	chunk := bytes.Repeat([]byte("z"), logChunk)
	for i := 0; i < (10<<20)/logChunk; i++ {
		tl.push(chunk)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("pushing 10 MiB took %v while the disk stalled", d)
	}
	tl.mu.Lock()
	dropped := tl.dropped
	tl.mu.Unlock()
	if dropped == 0 {
		t.Fatal("nothing was dropped")
	}
	waitLog(t, func() bool { return strings.Contains(tl.paneText(), "hub output is dropped") })
	if n := strings.Count(tl.paneText(), "hub output is dropped"); n != 1 {
		t.Errorf("%d drop notices in the pane, want 1:\n%s", n, tl.paneText())
	}
	once.Do(func() { close(release) })
	tl.push([]byte("after\n"))
	tl.close(5 * time.Second)
	all := ""
	for _, p := range tl.files() {
		b, _ := os.ReadFile(p)
		all += string(b)
	}
	if !strings.Contains(all, "restart: dropped ") {
		t.Error("no drop line in the files")
	}
	tl.checkBound(t)
}

// A failed rotation (the state directory is read-only after the open) enters drop mode: no file
// grows, one pane line. After the retry time, with the directory writable again, writing resumes
// with a drop line.
func TestRestartLogRotationFailure(t *testing.T) {
	tl := newTestLog(t, 1024)
	tl.start()
	os.Chmod(tl.dir, 0o500)
	t.Cleanup(func() { os.Chmod(tl.dir, 0o700) })
	for i := 0; i < 10; i++ {
		tl.push(bytes.Repeat([]byte("a"), 300))
		time.Sleep(20 * time.Millisecond)
	}
	waitLog(t, func() bool { return strings.Contains(tl.paneText(), "cannot be written") })
	tl.checkBound(t)
	if n := strings.Count(tl.paneText(), "cannot be written"); n != 1 {
		t.Errorf("%d drop-mode lines, want 1:\n%s", n, tl.paneText())
	}
	os.Chmod(tl.dir, 0o700)
	tl.advance(logRetry + time.Second)
	tl.push([]byte("resumed\n"))
	waitLog(t, func() bool { b, _ := os.ReadFile(tl.path); return strings.Contains(string(b), "resumed") })
	tl.close(5 * time.Second)
	b, _ := os.ReadFile(tl.path)
	if !strings.Contains(string(b), "restart: dropped ") {
		t.Errorf("no drop line before the resumed output: %q", b)
	}
	tl.checkBound(t)
}

// An open that fails (hub.log is a directory): drop mode, a pane line, and push does not wait.
func TestRestartLogOpenFailure(t *testing.T) {
	tl := newTestLog(t, 1024)
	os.MkdirAll(filepath.Join(tl.path, "x"), 0o700)
	tl.start()
	for i := 0; i < 100; i++ {
		tl.push(bytes.Repeat([]byte("b"), 1000))
	}
	waitLog(t, func() bool { return strings.Contains(tl.paneText(), "cannot be written") })
	tl.close(time.Second)
}

// A hub.log above the size at open is rotated first: the run writes to a new hub.log.
func TestRestartLogFullAtOpen(t *testing.T) {
	tl := newTestLog(t, 1024)
	os.WriteFile(tl.path, bytes.Repeat([]byte("o"), 2048), 0o600)
	tl.start()
	tl.push([]byte("new\n"))
	tl.close(time.Second)
	if s := tl.sizes(); s["hub.log.1"] != 2048 || s["hub.log"] != 4 {
		t.Errorf("sizes %v", s)
	}
}

// A pane that nobody reads does not slow the log: wrapper lines and output are taken at once.
func TestRestartLogBlockedPane(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	l := newRestartLog(t.TempDir(), w)
	l.start()
	start := time.Now()
	line := []byte(strings.Repeat("p", 200) + "\n")
	for i := 0; i < 5000; i++ { // 1 MB of pane lines; a pipe buffer is far smaller
		l.Write(line)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("5000 lines took %v with a blocked pane", d)
	}
	l.close(100 * time.Millisecond)
}

// The hub's last output is in hub.log before the wrapper's exit line.
func TestRestartLogOrder(t *testing.T) {
	wr := newWrapperTest(t, "panic-after-ready", "ready-exit0")
	if code := waitCode(t, wr.runAsync()); code != 0 {
		t.Fatalf("exit %d\n%s", code, wr.text())
	}
	b, _ := os.ReadFile(filepath.Join(wr.w.state, "hub.log"))
	p, e := strings.Index(string(b), "panic: x"), strings.Index(string(b), "exited: status 2")
	if p < 0 || e < 0 || p > e {
		t.Fatalf("hub.log:\n%s", b)
	}
	if strings.Contains(wr.text(), "panic: x") {
		t.Error("the hub's output reached the pane; it belongs in hub.log only")
	}
}

// A stalled disk never blocks the hub: the child writes 10 MiB and exits, and the wrapper restarts
// it as usual.
func TestRestartLogStalledDiskDoesNotBlockTheHub(t *testing.T) {
	wr := newWrapperTest(t, "spew", "ready-exit0")
	t.Setenv("FAKE_SPEW", strconv.Itoa(10<<20))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	wr.w.logHook = func(l *restartLog) {
		l.writeFile = func(*os.File, []byte) (int, error) { <-release; return 0, nil }
	}
	start := time.Now()
	if code := waitCode(t, wr.runAsync()); code != 0 {
		t.Fatalf("exit %d\n%s", code, wr.text())
	}
	if _, err := os.Stat(wr.starts + ".spewed"); err != nil {
		t.Fatalf("the child did not finish its write: %v", err)
	}
	if n := wr.startCount(); n != 2 {
		t.Errorf("%d starts, want 2", n)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("the runs took %v", d)
	}
}

// Five runs whose descendant keeps the output pipe open: each run's readers end at the 2 s limit,
// so no reader goroutine and no read descriptor is left.
func TestRestartLogRetiresReaders(t *testing.T) {
	plan := []string{"leak-out", "leak-out", "leak-out", "leak-out", "ready-exit0"}
	wr := newWrapperTest(t, plan...)
	t.Cleanup(func() {
		b, _ := os.ReadFile(wr.starts + ".keep")
		for _, s := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(s); err == nil && pid > 0 {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	fds := openFDs(t)
	if code := waitCode(t, wr.runAsync()); code != 0 {
		t.Fatalf("exit %d\n%s", code, wr.text())
	}
	buf := make([]byte, 1<<20)
	stacks := string(buf[:runtime.Stack(buf, true)])
	if n := strings.Count(stacks, "readRun") + strings.Count(stacks, "(*wrapper).runChild.func"); n != 0 {
		t.Errorf("%d reader goroutines are left", n)
	}
	if now := openFDs(t); now > fds {
		t.Errorf("open descriptors %d, %d before the runs", now, fds)
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	dir := "/dev/fd"
	if runtime.GOOS == "linux" {
		dir = "/proc/self/fd"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// Under the wrapper the hub ignores SIGPIPE: with its output pipe closed, a log write fails with
// EPIPE and the hub ends with its own status (here a start error: 1), not by SIGPIPE.
func TestHubUnderTheWrapperIgnoresSIGPIPE(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cfg := filepath.Join(tmp, "hub.json")
	b, _ := json.Marshal(map[string]any{"server": "unix:///nonexistent/rex.sock", "state": filepath.Join(tmp, "state"),
		"agents": []map[string]any{{"id": "user", "name": "User", "status_record": ""}}})
	os.WriteFile(cfg, b, 0o600)
	for _, wrapped := range []bool{true, false} {
		or, ow, _ := os.Pipe()
		rr, rw, _ := os.Pipe()
		or.Close() // nobody reads the hub's output
		cmd := exec.Command(bin, "-config", cfg)
		cmd.Stdout, cmd.Stderr = ow, ow
		if wrapped {
			cmd.ExtraFiles = []*os.File{rw}
			cmd.Env = append(os.Environ(), "LAZYREX_READY_FD=3")
		}
		cmd.Run()
		ow.Close()
		rr.Close()
		rw.Close()
		ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if wrapped && (ws.Signaled() || ws.ExitStatus() != 1) {
			t.Errorf("under the wrapper: %v, want exit status 1", cmd.ProcessState)
		}
		if !wrapped && !(ws.Signaled() && ws.Signal() == syscall.SIGPIPE) {
			t.Logf("without the wrapper: %v (SIGPIPE expected; the check above is the test)", cmd.ProcessState)
		}
	}
}

func waitLog(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("the condition did not hold within 10 s")
}

// TestSigpipeHelper is a child of TestHubChildrenKeepTheDefaultSIGPIPE, not a test. It sets up
// SIGPIPE as main does under the wrapper (or ignores it, for the control), then runs a shell
// pipeline whose writer dies by SIGPIPE (status 141) only with the default action. A Go child
// cannot show it: the Go runtime installs its own SIGPIPE handler at its start.
func TestSigpipeHelper(t *testing.T) {
	switch os.Getenv("SIGPIPE_HELPER") {
	case "":
		t.Skip("a helper process")
	case "parent", "parent-ignore":
		if os.Getenv("SIGPIPE_HELPER") == "parent" {
			catchSIGPIPE()
		} else {
			signal.Ignore(syscall.SIGPIPE)
		}
		out, _ := exec.Command("/bin/sh", "-c", `(yes; echo "yes-status=$?" >&2) | head -n 1 >/dev/null`).CombinedOutput()
		os.Stdout.Write(out)
		os.Exit(0)
	}
}

// Under the wrapper the hub catches SIGPIPE; a child of the hub starts with the default action.
// The control (signal.Ignore) shows that the check sees an inherited ignore.
func TestHubChildrenKeepTheDefaultSIGPIPE(t *testing.T) {
	for mode, want := range map[string]string{"parent": "yes-status=141", "parent-ignore": "yes-status=1"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSigpipeHelper$")
		cmd.Env = append(os.Environ(), "SIGPIPE_HELPER="+mode, "GORACE=atexit_sleep_ms=0")
		out, err := cmd.Output()
		if err != nil || !strings.Contains(string(out), want) {
			t.Errorf("%s: %q, %v; want %s", mode, out, err, want)
		}
	}
}

// The output readers start right after the child starts, before any status write: a status write
// that hangs (a FIFO at restart.json.tmp holds it) does not stop the hub's output. The hub writes
// 10 MiB and finishes while the status write still waits.
func TestRestartOutputDrainsBeforeStatusIO(t *testing.T) {
	wr := newWrapperTest(t, "spew", "ready-exit0")
	t.Setenv("FAKE_SPEW", strconv.Itoa(10<<20))
	state := filepath.Join(wr.dir, "state")
	os.Mkdir(state, 0o700)
	fifo := filepath.Join(state, "restart.json.tmp")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := wr.runAsync()
	released := false
	release := func() {
		if !released {
			released = true
			go func() {
				f, err := os.Open(fifo) // the reader end lets the status write finish
				if err == nil {
					io.Copy(io.Discard, f)
					f.Close()
				}
			}()
		}
	}
	t.Cleanup(release)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(wr.starts + ".spewed"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			release()
			waitCode(t, done)
			t.Fatal("the hub could not finish its output while the status write waited")
		}
	}
	release()
	if code := waitCode(t, done); code != 0 {
		t.Fatalf("exit %d\n%s", code, wr.text())
	}
}

// A failed or short write counts only the bytes that did not reach the file as dropped, and puts
// the writer in drop mode.
func TestRestartLogPartialWriteCountsOnlyLostBytes(t *testing.T) {
	for _, fail := range []error{errors.New("disk error"), nil} { // nil: a short write without an error
		tl := newTestLog(t, 1<<20)
		tl.writeFile = func(f *os.File, b []byte) (int, error) {
			n, _ := f.Write(b[:10])
			return n, fail
		}
		tl.start()
		tl.push(bytes.Repeat([]byte("q"), 100))
		waitLog(t, func() bool { tl.restartLog.mu.Lock(); defer tl.restartLog.mu.Unlock(); return tl.dropped != 0 })
		tl.restartLog.mu.Lock()
		dropped := tl.dropped
		tl.restartLog.mu.Unlock()
		if dropped != 90 {
			t.Errorf("error %v: dropped %d, want 90", fail, dropped)
		}
		waitLog(t, func() bool { return strings.Contains(tl.paneText(), "cannot be written") })
		tl.close(time.Second)
	}
}
