package main

// Lab directories (/tmp/lrt-<digits>) of `lazyrex-hub test` and the Go lab tests: their removal.
// A run removes its lab when it ends. A lab that a run keeps on purpose (-keep, or a failed Go lab
// test) gets a KEEP marker that says who kept it and why. Labs that a killed run left (SIGKILL, a
// go test timeout) are removed by the sweep: at most once per hour per user, at the start of a
// run, it removes each old lab of this user that has no KEEP marker and that a probe proves absent:
// no server and no process (labProbe). A kept lab
// is removed with `lazyrex-hub test -clean <dir>` after its review (docs/testing.md).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var labDirName = regexp.MustCompile(`^lrt-[0-9]+$`)

const (
	labKeepFile = "KEEP"
	// labStaleAfter: a lab younger than this is never swept. 24 hours for the first release, so
	// that owners can mark the labs that they kept before the KEEP marker existed; 1 hour after one
	// week (the coordinator, 2026-10-22).
	labStaleAfter = 24 * time.Hour
	labSweepEvery = time.Hour // the sweep runs at most once in this time per user (the stamp file)
)

// labSweepRootEnv names another sweep root: the lab tests of the hub set it to a test directory,
// so a lazyrex-hub test run by a test never sweeps the real /tmp.
const labSweepRootEnv = "LAZYREX_LAB_SWEEP_ROOT"

// sweepForbidden is set by the package's tests (labclean_test.go): a sweep of one of these roots
// in a test panics. A test never acts on the real /tmp.
var sweepForbidden []string

// labKeep is the KEEP marker of a kept lab.
type labKeep struct {
	By   string `json:"by"`  // the command or test that kept it
	Why  string `json:"why"` // for example the file and its result
	At   string `json:"at"`
	Kind string `json:"kind,omitempty"` // labKindKeep, labKindFailed or labKindUnproved
}

// The kinds of a KEEP marker. Each removal rule keys on the kind (labKeep.class):
//   - keep: -keep. Only -clean removes it; a Go harness test removes the -keep labs of its own run
//     (removeIfEnded).
//   - failed: a failed test file or Go lab test. The sweep removes it 24 hours after its "at", when
//     labProbe proves the lab absent.
//   - unproved: failure evidence, a lab whose cleanup a run did not prove. Only -clean removes it.
const (
	labKindKeep     = "keep"
	labKindFailed   = "failed"
	labKindUnproved = "unproved"
)

// keptHow says how a kept lab is removed, for the output.
func keptHow(k labKeep) string {
	if k.class() == labKindFailed {
		return "the sweep removes it 24 hours after it failed; or lazyrex-hub test -clean after its review"
	}
	return "remove it after its review: lazyrex-hub test -clean"
}

// labFailedKeptFor is how long a lab with a "failed" marker stays before the sweep may remove it.
const labFailedKeptFor = 24 * time.Hour

// class is the kind that the removal rules use. A marker of an older binary has no kind: by
// "lazyrex-hub test -keep" is keep, by "lazyrex-hub test" (the cleanup evidence) is unproved.
// Every other marker without a kind, and a kind that this binary does not know, is protected as
// unproved: only -clean removes it.
func (k labKeep) class() string {
	switch k.Kind {
	case labKindKeep, labKindFailed, labKindUnproved:
		return k.Kind
	case "":
		if k.By == "lazyrex-hub test -keep" {
			return labKindKeep
		}
	}
	return labKindUnproved
}

// labEvidenceBy is the "by" of a KEEP marker that a lazyrex-hub test run wrote for its own reason:
// an unproved cleanup (kind unproved) or a failed file (kind failed).
const labEvidenceBy = "lazyrex-hub test"

// readLabKeep reads the KEEP marker of the lab dir.
func readLabKeep(dir string) (labKeep, error) {
	var k labKeep
	b, err := os.ReadFile(filepath.Join(dir, labKeepFile))
	if err == nil {
		err = json.Unmarshal(b, &k)
	}
	return k, err
}

// markLabKept writes the KEEP marker of the lab dir, of the given kind.
func markLabKept(dir, kind, by, why string) error {
	b, _ := json.Marshal(labKeep{By: by, Why: why, At: time.Now().UTC().Format(time.RFC3339), Kind: kind})
	return os.WriteFile(filepath.Join(dir, labKeepFile), append(b, '\n'), 0o600)
}

// removeLab removes a lab directory. A lab can hold read-only directories (a plugin cache entry
// under hub/plugins/github/...), which os.RemoveAll cannot empty: each directory is made writable
// by its owner first. The error names a removal that still fails.
func removeLab(dir string) error {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			if fi, err := d.Info(); err == nil && fi.Mode().Perm()&0o700 != 0o700 {
				os.Chmod(p, fi.Mode().Perm()|0o700)
			}
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove lab %s: %w", dir, err)
	}
	return nil
}

// labState is what a probe can prove about a lab: a lab is removed only when it is absent.
type labState int

const (
	labAbsent    labState = iota // no server on the socket and no process that names the lab
	labLive                      // a server accepts a connection, or a process names the lab
	labUncertain                 // the probe cannot prove either: the lab stays
)

func (s labState) String() string {
	return [...]string{"absent", "live", "uncertain"}[s]
}

// labProc is one line of the process listing.
type labProc struct {
	pid, ppid int
	cmd       string
}

// labProcs lists every process with its parent and command line. ps is a base tool on macOS and
// Linux; -e, -ww and -o pid=,ppid=,command= mean the same on both. A test replaces it.
var labProcs = func() ([]labProc, error) {
	out, err := exec.Command("ps", "-eww", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("process listing: %w", err)
	}
	var procs []labProc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("process listing: a line that is not pid, ppid, command: %q", line)
		}
		procs = append(procs, labProc{pid, ppid, strings.Join(f[2:], " ")})
	}
	if len(procs) == 0 {
		return nil, errors.New("process listing: no process")
	}
	return procs, nil
}

// labDial connects to a lab socket. A test replaces it.
var labDial = func(sock string) error {
	c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err == nil {
		c.Close()
	}
	return err
}

// labProbe tells whether the lab dir is live, absent or uncertain, with the reason. procs is one
// process listing (labProcs); nil means that the listing failed or was not made, and only a test
// passes nil for a base case. The sweep, -clean and endLab use this one function. Rules, in order:
//   - a process whose command line names the lab is live. root is the lab root that the caller
//     accepts: a lab has a name under each name of the root (labNames), and a command line matches
//     a name without resolving its paths (namesLab). This process and its ancestors do not count:
//     a -clean names the lab in its own arguments.
//   - a connection to <dir>/rex.sock that is accepted is live.
//   - "no such file" (no socket) is absent only when lab.json is missing or names no server pid,
//     or names a server pid that no longer runs.
//   - "connection refused" is absent only when lab.json names a server pid that no longer runs. A
//     live server with a full connection queue refuses too (macOS).
//   - a recorded server pid that runs (even a reused pid) is uncertain; any other dial error is
//     uncertain.
func labProbe(root, dir string, procs []labProc) (labState, string) {
	if procs != nil {
		mine := map[int]bool{}
		parent := map[int]int{}
		for _, p := range procs {
			parent[p.pid] = p.ppid
		}
		for pid := os.Getpid(); pid > 1 && !mine[pid]; pid = parent[pid] {
			mine[pid] = true
		}
		names := labNames(root, dir)
		for _, p := range procs {
			if !mine[p.pid] && namesLab(p.cmd, names) {
				return labLive, fmt.Sprintf("process %d names the lab: %s", p.pid, p.cmd)
			}
		}
	}
	err := labDial(filepath.Join(dir, "rex.sock"))
	var noSocket bool
	switch {
	case err == nil:
		return labLive, "a server accepts connections on rex.sock"
	case errors.Is(err, syscall.ENOENT):
		noSocket = true
	case !errors.Is(err, syscall.ECONNREFUSED):
		return labUncertain, "rex.sock: " + err.Error()
	}
	what := "rex.sock refuses connections"
	if noSocket {
		what = "no rex.sock"
	}
	var lj struct {
		ServerPID int `json:"server_pid"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "lab.json"))
	switch {
	case err != nil && noSocket && errors.Is(err, fs.ErrNotExist):
		return labAbsent, "no rex.sock, no lab.json and no process"
	case err != nil:
		return labUncertain, what + " and lab.json cannot be read: " + err.Error()
	case json.Unmarshal(b, &lj) != nil:
		return labUncertain, what + " and lab.json cannot be parsed"
	case lj.ServerPID <= 0 && noSocket:
		return labAbsent, "no rex.sock, no server pid in lab.json and no process"
	case lj.ServerPID <= 0:
		return labUncertain, what + " and lab.json names no server pid"
	case procs == nil:
		return labUncertain, what + " and there is no process listing"
	}
	for _, p := range procs {
		if p.pid == lj.ServerPID {
			return labUncertain, fmt.Sprintf("%s and the server pid %d runs (%s)", what, p.pid, p.cmd)
		}
	}
	return labAbsent, fmt.Sprintf("%s and the server pid %d has ended", what, lj.ServerPID)
}

// labNames is every name of the lab dir: its base name under each name of the lab root, and dir as
// given. A lab is <root>/lrt-<digits> by contract, so these are all the paths that a process can
// use for it. The names of the root: as given, resolved, and each name under a top-level link
// (labRootLinks; on macOS /tmp is a link to /private/tmp), so that a root given in either form has
// both names.
func labNames(root, dir string) []string {
	dir = filepath.Clean(dir)
	base := filepath.Base(dir)
	roots := []string{filepath.Clean(root)}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		roots = append(roots, real)
		for _, link := range labRootLinks {
			lr, err := filepath.EvalSymlinks(link)
			if err != nil || lr == link {
				continue
			}
			if real == lr || strings.HasPrefix(real, lr+"/") {
				roots = append(roots, link+strings.TrimPrefix(real, lr))
			}
		}
	}
	names := []string{dir}
	for _, r := range roots {
		names = append(names, filepath.Join(r, base))
	}
	return names
}

// labRootLinks are the top-level directories that can be links (macOS: /tmp, /var and /etc are
// links into /private). Only a link that resolves elsewhere adds a name.
var labRootLinks = []string{"/tmp", "/var", "/etc"}

// namesLab reports whether a command line names one of the lab's names: name + "/" anywhere (also
// inside a word, for example unix:///tmp/lrt-1/rex.sock), or the name as a whole word. Paths in
// the command line are not resolved: they can be embedded in a word and need not exist.
func namesLab(cmd string, names []string) bool {
	for _, n := range names {
		if strings.Contains(cmd, n+"/") {
			return true
		}
		for _, f := range strings.Fields(cmd) {
			if f == n {
				return true
			}
		}
	}
	return false
}

// labOwned reports whether fi is owned by this user.
func labOwned(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// labAge is the time since the newest of the directory and its lab.json changed.
func labAge(dir string, fi os.FileInfo, now time.Time) time.Duration {
	t := fi.ModTime()
	if lj, err := os.Stat(filepath.Join(dir, "lab.json")); err == nil && lj.ModTime().After(t) {
		t = lj.ModTime()
	}
	return now.Sub(t)
}

// sweepLabs removes, in root, each lab directory (lrt-<digits>, not a symbolic link) of this user
// that is absent (labProbe) and either has no KEEP marker and is older than labStaleAfter, or has a
// "failed" marker older than labFailedKeptFor (by the marker's own time). Every other KEEP marker
// stays. A failed process
// listing stops the sweep, and an uncertain lab stays, with one log line. It runs at most
// once per labSweepEvery per user: the stamp file <root>/lrt-sweep-<uid> holds the time of the
// last sweep. logf gets one line for each removal and each failure.
func sweepLabs(root string, now time.Time, logf func(string)) {
	if real, err := filepath.EvalSymlinks(root); err == nil {
		for _, f := range sweepForbidden {
			if fr, err := filepath.EvalSymlinks(f); err == nil && fr == real {
				panic("sweepLabs: a test must not sweep " + root)
			}
		}
	}
	stamp := filepath.Join(root, "lrt-sweep-"+strconv.Itoa(os.Getuid()))
	if fi, err := os.Stat(stamp); err == nil && now.Sub(fi.ModTime()) < labSweepEvery {
		return
	}
	if err := os.WriteFile(stamp, nil, 0o600); err == nil {
		os.Chtimes(stamp, now, now)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		logf(fmt.Sprintf("lab sweep: %v", err))
		return
	}
	procs, err := labProcs()
	if err != nil {
		logf(fmt.Sprintf("lab sweep: no lab is removed: %v", err))
		return
	}
	for _, e := range entries {
		if !labDirName.MatchString(e.Name()) || e.Type()&fs.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() || !labOwned(fi) {
			continue
		}
		var age time.Duration
		failedLab := false
		if _, err := os.Lstat(filepath.Join(dir, labKeepFile)); !errors.Is(err, fs.ErrNotExist) {
			// Kept. Only a "failed" marker older than labFailedKeptFor (by its own time) can go.
			k, err := readLabKeep(dir)
			at, terr := time.Parse(time.RFC3339, k.At)
			if err != nil || terr != nil || k.class() != labKindFailed || now.Sub(at) < labFailedKeptFor {
				continue
			}
			age, failedLab = now.Sub(at), true
		} else if age = labAge(dir, fi, now); age < labStaleAfter {
			continue
		}
		if st, why := labProbe(root, dir, procs); st != labAbsent {
			if st == labUncertain {
				logf(fmt.Sprintf("lab sweep: kept %s (uncertain: %s)", dir, why))
			}
			continue
		}
		if err := removeLab(dir); err != nil {
			logf("lab sweep: " + err.Error())
			continue
		}
		if failedLab {
			logf(fmt.Sprintf("lab sweep: removed %s (a failed file's lab, kept %s, no server or process)", dir, age.Round(time.Minute)))
			continue
		}
		logf(fmt.Sprintf("lab sweep: removed %s (age %s, no KEEP marker, no server or process; to keep a lab, put a file named KEEP in it)", dir, age.Round(time.Minute)))
	}
}

// endLab ends the lab dir of one test file after its stop. unproved lists why the stop could not
// prove the cleanup (a survivor, the guard). In order:
//   - a cleanup that is not proved (unproved, or a probe after the stop that is not absent): the lab
//     stays with a KEEP marker of kind unproved, and a problem with its path. The evidence stays.
//   - -keep: the lab stays with kind keep, also when the file failed.
//   - a failed file: the lab stays with kind failed; the sweep removes it after labFailedKeptFor.
//   - else the lab is removed.
//
// It returns whether the lab stays, and the problems for the run's result.
func endLab(dir string, keep, failed bool, why string, unproved []string) (bool, []string) {
	reason := strings.Join(unproved, "; ")
	if reason == "" {
		procs, err := labProcs()
		if err != nil {
			reason = err.Error()
		} else if st, w := labProbe(filepath.Dir(dir), dir, procs); st != labAbsent {
			reason = st.String() + ": " + w
		}
	}
	kind, by := labKindKeep, "lazyrex-hub test -keep"
	switch {
	case reason != "":
		kind, by, why = labKindUnproved, labEvidenceBy, "cleanup not proved: "+reason
	case keep:
	case failed:
		kind, by, why = labKindFailed, labEvidenceBy, "failed: "+why
	default:
		if err := removeLab(dir); err != nil {
			return false, []string{err.Error()}
		}
		return false, nil
	}
	var probs []string
	if reason != "" {
		probs = append(probs, fmt.Sprintf("lab kept: %s: the cleanup is not proved (%s)", dir, reason))
	}
	if err := markLabKept(dir, kind, by, why); err != nil {
		probs = append(probs, "lab KEEP marker: "+err.Error())
	}
	return true, probs
}

// cleanLab is `lazyrex-hub test -clean <dir>`: it removes a lab, kept or not, after its review.
// It refuses a path that is not a lab directory of this user in root, and a lab that is live or
// uncertain (labProbe).
func cleanLab(root, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil || parent != rootReal || !labDirName.MatchString(filepath.Base(abs)) {
		return fmt.Errorf("%s is not a lab directory (%s/lrt-<digits>)", dir, root)
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if !fi.IsDir() || !labOwned(fi) {
		return fmt.Errorf("%s is not a lab directory of this user", dir)
	}
	procs, err := labProcs()
	if err != nil {
		return fmt.Errorf("%s is kept: %w", dir, err)
	}
	switch st, why := labProbe(root, abs, procs); st {
	case labLive:
		return fmt.Errorf("%s is live (%s); stop it first", dir, why)
	case labUncertain:
		return fmt.Errorf("%s is kept: the probe cannot prove that its server and processes ended (%s)", dir, why)
	}
	return removeLab(abs)
}
