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
	markLabKept(kept, labKindKeep, "test", "evidence")
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
	markLabKept(kept, labKindKeep, "test", "evidence")
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
	if err := markLabKept(dir, labKindKeep, "lazyrex-hub test -keep", "tests/calls.lua: 5 passed, 0 failed, 0 problems"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, labKeepFile))
	var k labKeep
	if err := json.Unmarshal(b, &k); err != nil || k.By != "lazyrex-hub test -keep" || !strings.Contains(k.Why, "calls.lua") || k.At == "" || k.Kind != labKindKeep {
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
	if st, why := labProbe(filepath.Dir(dir), dir, mustProcs(t)); st != labAbsent {
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
	c := namedProcess(t, filepath.Join(dir, "x"))
	if st, why := labProbe(filepath.Dir(dir), dir, mustProcs(t)); st != labLive || !strings.Contains(why, strconv.Itoa(c.Process.Pid)) {
		t.Errorf("probe with a process that names the lab: %v %s", st, why)
	}
	if st, why := labProbe(filepath.Dir(sibling), sibling, mustProcs(t)); st != labAbsent {
		t.Errorf("a process of %s made %s %v: %s", dir, sibling, st, why)
	}
	self := []labProc{{pid: os.Getpid(), ppid: os.Getppid(), cmd: "lazyrex-hub test -clean " + sibling}}
	if st, why := labProbe(filepath.Dir(sibling), sibling, self); st != labAbsent {
		t.Errorf("this process's own arguments made the lab %v: %s", st, why)
	}
}

// A failed process listing and a dial error other than "refused" or "no such file" are uncertain:
// the sweep and -clean keep the lab.
func TestProbeUncertain(t *testing.T) {
	root := labTestRoot(t)
	dir := fakeLab(t, root, "lrt-930", 30*time.Hour)
	if st, _ := labProbe(filepath.Dir(dir), dir, nil); st != labAbsent {
		t.Fatalf("the base case is not absent: %v", st)
	}
	oldDial := labDial
	t.Cleanup(func() { labDial = oldDial })
	labDial = func(string) error { return syscall.ETIMEDOUT }
	if st, why := labProbe(filepath.Dir(dir), dir, nil); st != labUncertain {
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
	if kept, probs := endLab(clean, false, false, "f.lua", nil); kept || len(probs) != 0 || exists(clean) {
		t.Errorf("a proved cleanup: kept %v, problems %v, exists %v", kept, probs, exists(clean))
	}
	keep := fakeLab(t, root, "lrt-941", 0)
	if kept, probs := endLab(keep, true, false, "f.lua: 1 passed", nil); !kept || len(probs) != 0 || !strings.Contains(marker(t, keep), "-keep") {
		t.Errorf("-keep: kept %v, problems %v, marker %s", kept, probs, marker(t, keep))
	}
	for _, k := range []bool{false, true} {
		dir := fakeLab(t, root, "lrt-942"+strconv.FormatBool(k), 0)
		surv := "cleanup: process survived the lab server: pid 4242"
		kept, probs := endLab(dir, k, false, "f.lua", []string{surv})
		if !kept || !exists(dir) || len(probs) != 1 || !strings.Contains(probs[0], dir) || !strings.Contains(probs[0], "pid 4242") {
			t.Errorf("a survivor (keep %v): kept %v, problems %v", k, kept, probs)
		}
		if m := marker(t, dir); !strings.Contains(m, "cleanup not proved") || !strings.Contains(m, "pid 4242") {
			t.Errorf("a survivor (keep %v): marker %s", k, m)
		}
	}
	named := fakeLab(t, root, "lrt-943", 0)
	namedProcess(t, filepath.Join(named, "x"))
	if kept, probs := endLab(named, false, false, "f.lua", nil); !kept || !exists(named) || len(probs) != 1 || !strings.Contains(probs[0], "live") {
		t.Errorf("a process names the lab: kept %v, problems %v", kept, probs)
	}
}

func marker(t *testing.T, dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, labKeepFile))
	return string(b)
}

// namedProcess starts a process that this test owns, whose command line names path. It is a shell
// that waits in the builtin read on a pipe that the test owns, so the kill at the test's end leaves
// no child (a "sleep" would outlive its killed shell).
func namedProcess(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("/bin/sh", "-c", "read value", path)
	c.Stdin = r
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	t.Cleanup(func() { c.Process.Kill(); c.Wait(); w.Close() })
	return c
}

// A lab has a name under each name of its root (/tmp and /private/tmp on macOS). codex-21's
// regression TestReviewerCleanAliasKeepsLiveServer: a live process names /tmp/..., and -clean of the
// /private/tmp name removed the lab. Both directions, with a process that is not the server pid.
func TestCleanAliasKeepsLiveLab(t *testing.T) {
	root := labTestRoot(t)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if realRoot == root {
		t.Skip("this check needs a root with two names (macOS /tmp)")
	}
	for i, c := range []struct{ procPath, cleanRoot, cleanPath string }{
		{root, root, realRoot},     // the process names /tmp, -clean gets /private/tmp
		{realRoot, root, root},     // the process names /private/tmp, -clean gets /tmp
		{root, realRoot, realRoot}, // the sweep root /private/tmp, the process names /tmp
	} {
		name := "lrt-96" + strconv.Itoa(i)
		dir := fakeLab(t, root, name, 30*time.Hour)
		p := namedProcess(t, filepath.Join(c.procPath, name, "server"))
		if st, why := labProbe(c.cleanRoot, filepath.Join(c.cleanPath, name), mustProcs(t)); st != labLive || !strings.Contains(why, strconv.Itoa(p.Process.Pid)) {
			t.Errorf("case %d: probe %v %s", i, st, why)
		}
		if err := cleanLab(c.cleanRoot, filepath.Join(c.cleanPath, name)); err == nil || !exists(dir) {
			t.Errorf("case %d: -clean removed a live lab through another name: %v", i, err)
		}
		old(t, dir)
	}
	sweepLabs(realRoot, time.Now(), func(string) {})
	for i := 0; i < 3; i++ {
		if !exists(filepath.Join(root, "lrt-96"+strconv.Itoa(i))) {
			t.Errorf("the sweep of %s removed lrt-96%d, a live lab", realRoot, i)
		}
	}
}

// No rex.sock is absent only when lab.json names no server pid or one that has ended: a recorded
// server pid that runs is uncertain, also when no process names the lab.
func TestProbeNoSocketRunningServerPid(t *testing.T) {
	root := labTestRoot(t)
	dir := fakeLab(t, root, "lrt-970", 30*time.Hour)
	labJSON(t, dir, os.Getpid())
	if st, why := labProbe(root, dir, mustProcs(t)); st != labUncertain {
		t.Errorf("no socket, a running server pid: %v %s", st, why)
	}
	labJSON(t, dir, deadPid(t))
	if st, why := labProbe(root, dir, mustProcs(t)); st != labAbsent {
		t.Errorf("no socket, an ended server pid: %v %s", st, why)
	}
}

// Failure evidence (a KEEP marker for an unproved cleanup) stays: codex-21's regression
// TestReviewerHarnessRetainsCleanupEvidence. The survivor can end before the harness test's
// cleanup, and the probe then says absent. An ordinary -keep lab of this test's run is removed.
func TestRemoveIfEndedKeepsEvidence(t *testing.T) {
	root := labTestRoot(t)
	ev := fakeLab(t, root, "lrt-980", 0)
	if kept, probs := endLab(ev, false, false, "failed cleanup", []string{"cleanup: process survived"}); !kept || len(probs) == 0 {
		t.Fatal("endLab did not keep the evidence")
	}
	removeIfEnded(t, ev)
	if !exists(ev) {
		t.Error("removeIfEnded removed failure evidence after the probe became absent")
	}
	harnessStarts.Store(t, time.Now().Add(-time.Second).UTC().Truncate(time.Second))
	t.Cleanup(func() { harnessStarts.Delete(t) })
	ordinary := fakeLab(t, root, "lrt-981", 0)
	markLabKept(ordinary, labKindKeep, "lazyrex-hub test -keep", "tests/a.lua: 1 passed, 0 failed, 0 problems")
	removeIfEnded(t, ordinary)
	if exists(ordinary) {
		t.Error("removeIfEnded kept an ordinary -keep lab of this run")
	}
}

// writeKeep writes a KEEP marker as JSON, with the given time, as an older or newer binary may.
func writeKeep(t *testing.T, dir string, m map[string]string, at time.Time) {
	t.Helper()
	m["at"] = at.UTC().Format(time.RFC3339)
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, labKeepFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A failed file keeps its lab with a "failed" marker; -keep overrides it; a passed file removes its
// lab; an unproved cleanup wins over both (the hub lead's proposal v2, coordinator-approved).
func TestEndLabFailedFile(t *testing.T) {
	root := labTestRoot(t)
	failed := fakeLab(t, root, "lrt-990", 0)
	kept, probs := endLab(failed, false, true, "tests/a.lua: 1 passed, 1 failed, 0 problems", nil)
	k, err := readLabKeep(failed)
	if !kept || len(probs) != 0 || err != nil || k.Kind != labKindFailed || k.By != labEvidenceBy || !strings.HasPrefix(k.Why, "failed: tests/a.lua") {
		t.Errorf("a failed file: kept %v, problems %v, marker %+v %v", kept, probs, k, err)
	}
	withKeep := fakeLab(t, root, "lrt-991", 0)
	endLab(withKeep, true, true, "tests/a.lua", nil)
	if k, _ := readLabKeep(withKeep); k.Kind != labKindKeep {
		t.Errorf("-keep on a failed file: %+v", k)
	}
	passed := fakeLab(t, root, "lrt-992", 0)
	if kept, _ := endLab(passed, false, false, "tests/a.lua", nil); kept || exists(passed) {
		t.Error("a passed file kept its lab")
	}
	both := fakeLab(t, root, "lrt-993", 0)
	endLab(both, false, true, "tests/a.lua", []string{"cleanup: process survived"})
	if k, _ := readLabKeep(both); k.Kind != labKindUnproved {
		t.Errorf("a failed file with an unproved cleanup: %+v", k)
	}
}

// The sweep removes a "failed" lab only 24 hours after its marker's time, and only when the probe
// proves it absent. Every other marker stays: keep, unproved, a marker of an older binary without
// a kind (by -keep is keep; by "lazyrex-hub test" is cleanup evidence; another by is protected),
// and a kind that this binary does not know.
func TestSweepFailedLabs(t *testing.T) {
	root := labTestRoot(t)
	now := time.Now()
	day := now.Add(-25 * time.Hour)
	mk := func(name string, m map[string]string, at time.Time) string {
		d := fakeLab(t, root, name, 30*time.Hour)
		writeKeep(t, d, m, at)
		old(t, d)
		return d
	}
	gone := mk("lrt-1000", map[string]string{"by": labEvidenceBy, "why": "failed: a", "kind": labKindFailed}, day)
	young := mk("lrt-1001", map[string]string{"by": labEvidenceBy, "why": "failed: a", "kind": labKindFailed}, now.Add(-time.Hour))
	live := mk("lrt-1002", map[string]string{"by": labEvidenceBy, "why": "failed: a", "kind": labKindFailed}, day)
	namedProcess(t, filepath.Join(live, "x"))
	stay := []string{young, live,
		mk("lrt-1003", map[string]string{"by": "lazyrex-hub test -keep", "why": "a", "kind": labKindKeep}, day),
		mk("lrt-1004", map[string]string{"by": labEvidenceBy, "why": "cleanup not proved: x", "kind": labKindUnproved}, day),
		mk("lrt-1005", map[string]string{"by": "lazyrex-hub test -keep", "why": "a"}, day),
		mk("lrt-1006", map[string]string{"by": labEvidenceBy, "why": "cleanup not proved: x"}, day),
		mk("lrt-1007", map[string]string{"by": "go test TestX", "why": "the test failed"}, day),
		mk("lrt-1008", map[string]string{"by": labEvidenceBy, "why": "failed: a", "kind": "later"}, day),
	}
	var lines []string
	sweepLabs(root, now, func(s string) { lines = append(lines, s) })
	if exists(gone) {
		t.Errorf("the sweep kept an absent failed lab older than 24 hours; log %v", lines)
	}
	for _, d := range stay {
		if !exists(d) {
			t.Errorf("the sweep removed %s; log %v", d, lines)
		}
	}
	for m, want := range map[labKeep]string{
		{By: "lazyrex-hub test -keep"}:           labKindKeep,
		{By: labEvidenceBy}:                      labKindUnproved,
		{By: "go test TestX"}:                    labKindUnproved,
		{By: labEvidenceBy, Kind: "later"}:       labKindUnproved,
		{By: labEvidenceBy, Kind: labKindFailed}: labKindFailed,
	} {
		if got := m.class(); got != want {
			t.Errorf("class of %+v: %s, want %s", m, got, want)
		}
	}
}
