package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// labTestRoot is a short directory under /tmp: a lab socket path must stay under the Unix limit.
func labTestRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "lrtclean-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeLab(root) })
	return root
}

// fakeLab makes root/name with a lab.json and the given age.
func fakeLab(t *testing.T, root, name string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{}`), 0o600)
	at := time.Now().Add(-age)
	os.Chtimes(filepath.Join(dir, "lab.json"), at, at)
	os.Chtimes(dir, at, at)
	return dir
}

// readOnlyCache adds a read-only plugin cache entry, as a lab's hub writes one.
func readOnlyCache(t *testing.T, dir string) {
	t.Helper()
	entry := filepath.Join(dir, "hub", "plugins", "github", "lab", "gh", "c0ffee.sha256-x")
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(entry, "main.lua"), []byte("x"), 0o400)
	os.Chmod(entry, 0o500)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// A lab with a read-only plugin cache entry: os.RemoveAll fails on it, removeLab removes it.
func TestRemoveLabReadOnlyCache(t *testing.T) {
	root := labTestRoot(t)
	plain := fakeLab(t, root, "lrt-1", 0)
	readOnlyCache(t, plain)
	if err := os.RemoveAll(plain); err == nil {
		t.Fatal("os.RemoveAll removed a read-only cache entry: the test does not show the defect")
	}
	dir := fakeLab(t, root, "lrt-2", 0)
	readOnlyCache(t, dir)
	if err := removeLab(dir); err != nil || exists(dir) {
		t.Errorf("removeLab: %v, exists %v", err, exists(dir))
	}
}

// The sweep removes only an old lab of this user without a KEEP marker and without a live server.
// It keeps a marked lab, a new lab, a live lab, other names, a file and a symbolic link, and it
// runs at most once per hour.
func TestSweepLabs(t *testing.T) {
	root := labTestRoot(t)
	old := fakeLab(t, root, "lrt-100", 30*time.Hour)
	readOnlyCache(t, old)
	os.Chtimes(old, time.Now().Add(-30*time.Hour), time.Now().Add(-30*time.Hour)) // the cache entry made the directory new
	kept := fakeLab(t, root, "lrt-101", 30*time.Hour)
	markLabKept(kept, "test", "evidence")
	os.Chtimes(kept, time.Now().Add(-30*time.Hour), time.Now().Add(-30*time.Hour))
	young := fakeLab(t, root, "lrt-102", 10*time.Minute)
	live := fakeLab(t, root, "lrt-103", 30*time.Hour)
	ln, err := net.Listen("unix", filepath.Join(live, "rex.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	at := time.Now().Add(-30 * time.Hour)
	os.Chtimes(live, at, at)
	other := fakeLab(t, root, "lrt-x1", 30*time.Hour)
	os.WriteFile(filepath.Join(root, "lrt-104"), nil, 0o600)
	target := fakeLab(t, root, "target", 30*time.Hour)
	os.Symlink(target, filepath.Join(root, "lrt-105"))

	var lines []string
	sweepLabs(root, time.Now(), func(s string) { lines = append(lines, s) })
	if exists(old) {
		t.Errorf("the old unmarked lab stays; log %v", lines)
	}
	for _, p := range []string{kept, young, live, other, filepath.Join(root, "lrt-104"), filepath.Join(root, "lrt-105"), target} {
		if !exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "removed "+old) {
		t.Errorf("log %v", lines)
	}
	// Within the hour the sweep does not run again.
	again := fakeLab(t, root, "lrt-106", 30*time.Hour)
	sweepLabs(root, time.Now(), func(string) {})
	if !exists(again) {
		t.Error("the sweep ran twice in one hour")
	}
	sweepLabs(root, time.Now().Add(30*time.Hour), func(string) {})
	if exists(again) {
		t.Error("the sweep did not run after an hour")
	}
}

// -clean removes a kept lab after its review, and refuses other paths and a live lab.
func TestCleanLab(t *testing.T) {
	root := labTestRoot(t)
	kept := fakeLab(t, root, "lrt-200", 0)
	markLabKept(kept, "test", "evidence")
	readOnlyCache(t, kept)
	if err := cleanLab(root, kept); err != nil || exists(kept) {
		t.Errorf("clean of a kept lab: %v", err)
	}
	other := fakeLab(t, root, "notalab", 0)
	if err := cleanLab(root, other); err == nil || !exists(other) {
		t.Errorf("clean of another name: %v", err)
	}
	nested := fakeLab(t, other, "lrt-201", 0)
	if err := cleanLab(root, nested); err == nil || !exists(nested) {
		t.Errorf("clean outside the root: %v", err)
	}
	live := fakeLab(t, root, "lrt-202", 0)
	ln, err := net.Listen("unix", filepath.Join(live, "rex.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := cleanLab(root, live); err == nil || !strings.Contains(err.Error(), "is live") || !exists(live) {
		t.Errorf("clean of a live lab: %v", err)
	}
}

// The KEEP marker says who kept the lab and why.
func TestMarkLabKept(t *testing.T) {
	dir := fakeLab(t, labTestRoot(t), "lrt-300", 0)
	if err := markLabKept(dir, "lazyrex-hub test -keep", "tests/calls.lua: 5 passed, 0 failed, 0 problems"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, labKeepFile))
	var k labKeep
	if err := json.Unmarshal(b, &k); err != nil || k.By != "lazyrex-hub test -keep" || !strings.Contains(k.Why, "calls.lua") || k.At == "" {
		t.Errorf("marker %s: %v", b, err)
	}
}

// The real /tmp is never swept by a test of this package: sweepLabs panics on it.
func init() { sweepForbidden = []string{"/tmp", os.TempDir()} }

func TestSweepRefusesRealTmp(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a sweep of /tmp in a test did not panic")
		}
	}()
	sweepLabs("/tmp", time.Now(), func(string) {})
}

// busyListener listens on the lab socket with a full connection queue: a dial of it fails with
// "connection refused" on macOS, as a dial of a stale socket does (codex-21's regression,
// TestReviewerSweepKeepsBusyServer).
func busyListener(t *testing.T, dir string) {
	t.Helper()
	sock := filepath.Join(dir, "rex.sock")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: sock}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		c, err := net.DialTimeout("unix", sock, 50*time.Millisecond)
		if err != nil {
			return
		}
		t.Cleanup(func() { c.Close() })
	}
	t.Fatal("the listener's queue did not fill")
}

// labJSON writes the lab's lab.json with the given server pid.
func labJSON(t *testing.T, dir string, pid int) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"socket": "unix://" + filepath.Join(dir, "rex.sock"), "server_pid": pid, "nonce": "n"})
	if err := os.WriteFile(filepath.Join(dir, "lab.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// deadPid is the pid of a process that this test started and that has ended.
func deadPid(t *testing.T) int {
	t.Helper()
	c := exec.Command("/usr/bin/true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

func old(t *testing.T, dir string) {
	at := time.Now().Add(-30 * time.Hour)
	os.Chtimes(filepath.Join(dir, "lab.json"), at, at)
	os.Chtimes(dir, at, at)
}

// A live server whose connection queue is full refuses a dial, as a stale socket does. Neither the
// sweep nor -clean may remove its lab: without lab.json, and with a lab.json whose server pid is
// alive, the probe is uncertain.
func TestSweepAndCleanKeepBusyServer(t *testing.T) {
	root := labTestRoot(t)
	bare := fakeLab(t, root, "lrt-900", 30*time.Hour)
	os.Remove(filepath.Join(bare, "lab.json"))
	busyListener(t, bare)
	alive := fakeLab(t, root, "lrt-901", 30*time.Hour)
	labJSON(t, alive, os.Getpid())
	busyListener(t, alive)
	old(t, bare)
	old(t, alive)
	var lines []string
	sweepLabs(root, time.Now(), func(s string) { lines = append(lines, s) })
	for _, d := range []string{bare, alive} {
		if !exists(d) {
			t.Errorf("the sweep removed %s, which has a live listener with a full queue; log %v", d, lines)
		}
		if err := cleanLab(root, d); err == nil || !exists(d) {
			t.Errorf("-clean removed %s, which has a live listener with a full queue: %v", d, err)
		}
	}
}

// A stale socket (no listener) and a lab.json whose server pid has ended: the lab is absent, and
// the sweep removes it. A missing socket with no process: absent too.
func TestSweepRemovesStaleSocket(t *testing.T) {
	root := labTestRoot(t)
	dir := fakeLab(t, root, "lrt-910", 30*time.Hour)
	ln, err := net.Listen("unix", filepath.Join(dir, "rex.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	labJSON(t, dir, deadPid(t))
	old(t, dir)
	if st, why := labProbe(dir, mustProcs(t)); st != labAbsent {
		t.Fatalf("probe of a stale socket: %v %s", st, why)
	}
	sweepLabs(root, time.Now(), func(string) {})
	if exists(dir) {
		t.Error("the sweep kept a lab with a stale socket and an ended server")
	}
}

func mustProcs(t *testing.T) []labProc {
	t.Helper()
	p, err := labProcs()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A process whose command line names the lab makes it live, even with no socket. The probe does
// not count this process or its ancestors (a -clean names the lab in its own arguments).
func TestProbeProcessNamesLab(t *testing.T) {
	root := labTestRoot(t)
	dir := fakeLab(t, root, "lrt-920", 30*time.Hour)
	sibling := fakeLab(t, root, "lrt-9201", 30*time.Hour)
	c := exec.Command("/bin/sh", "-c", "sleep 30; :", filepath.Join(dir, "x"))
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { c.Process.Kill(); c.Wait() }() // the process this test started
	if st, why := labProbe(dir, mustProcs(t)); st != labLive || !strings.Contains(why, strconv.Itoa(c.Process.Pid)) {
		t.Errorf("probe with a process that names the lab: %v %s", st, why)
	}
	if st, why := labProbe(sibling, mustProcs(t)); st != labAbsent {
		t.Errorf("a process of %s made %s %v: %s", dir, sibling, st, why)
	}
	self := []labProc{{pid: os.Getpid(), ppid: os.Getppid(), cmd: "lazyrex-hub test -clean " + sibling}}
	if st, why := labProbe(sibling, self); st != labAbsent {
		t.Errorf("this process's own arguments made the lab %v: %s", st, why)
	}
}

// A failed process listing and a dial error other than "refused" or "no such file" are uncertain:
// the sweep and -clean keep the lab.
func TestProbeUncertain(t *testing.T) {
	root := labTestRoot(t)
	dir := fakeLab(t, root, "lrt-930", 30*time.Hour)
	if st, _ := labProbe(dir, nil); st != labAbsent {
		t.Fatalf("the base case is not absent: %v", st)
	}
	oldDial := labDial
	t.Cleanup(func() { labDial = oldDial })
	labDial = func(string) error { return syscall.ETIMEDOUT }
	if st, why := labProbe(dir, nil); st != labUncertain {
		t.Errorf("a dial timeout: %v %s", st, why)
	}
	labDial = oldDial
	oldProcs := labProcs
	t.Cleanup(func() { labProcs = oldProcs })
	labProcs = func() ([]labProc, error) { return nil, errors.New("ps failed") }
	old(t, dir)
	var lines []string
	sweepLabs(root, time.Now(), func(s string) { lines = append(lines, s) })
	if !exists(dir) {
		t.Errorf("the sweep removed a lab without a process listing; log %v", lines)
	}
	if err := cleanLab(root, dir); err == nil || !strings.Contains(err.Error(), "ps failed") || !exists(dir) {
		t.Errorf("-clean without a process listing: %v", err)
	}
}

// endLab: a stop that proves the cleanup removes the lab (or keeps it with -keep). A stop that
// reports a survivor, or a probe that is not absent, keeps the lab with a KEEP marker that says
// why, and a problem with its path; -keep does not hide that problem.
func TestEndLab(t *testing.T) {
	root := labTestRoot(t)
	clean := fakeLab(t, root, "lrt-940", 0)
	if kept, probs := endLab(clean, false, "f.lua", nil); kept || len(probs) != 0 || exists(clean) {
		t.Errorf("a proved cleanup: kept %v, problems %v, exists %v", kept, probs, exists(clean))
	}
	keep := fakeLab(t, root, "lrt-941", 0)
	if kept, probs := endLab(keep, true, "f.lua: 1 passed", nil); !kept || len(probs) != 0 || !strings.Contains(marker(t, keep), "-keep") {
		t.Errorf("-keep: kept %v, problems %v, marker %s", kept, probs, marker(t, keep))
	}
	for _, k := range []bool{false, true} {
		dir := fakeLab(t, root, "lrt-942"+strconv.FormatBool(k), 0)
		surv := "cleanup: process survived the lab server: pid 4242"
		kept, probs := endLab(dir, k, "f.lua", []string{surv})
		if !kept || !exists(dir) || len(probs) != 1 || !strings.Contains(probs[0], dir) || !strings.Contains(probs[0], "pid 4242") {
			t.Errorf("a survivor (keep %v): kept %v, problems %v", k, kept, probs)
		}
		if m := marker(t, dir); !strings.Contains(m, "cleanup not proved") || !strings.Contains(m, "pid 4242") {
			t.Errorf("a survivor (keep %v): marker %s", k, m)
		}
	}
	named := fakeLab(t, root, "lrt-943", 0)
	c := exec.Command("/bin/sh", "-c", "sleep 30; :", filepath.Join(named, "x"))
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { c.Process.Kill(); c.Wait() }() // the process this test started
	if kept, probs := endLab(named, false, "f.lua", nil); !kept || !exists(named) || len(probs) != 1 || !strings.Contains(probs[0], "live") {
		t.Errorf("a process names the lab: kept %v, problems %v", kept, probs)
	}
}

func marker(t *testing.T, dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, labKeepFile))
	return string(b)
}
