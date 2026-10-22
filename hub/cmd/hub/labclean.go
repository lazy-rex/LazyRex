package main

// Lab directories (/tmp/lrt-<digits>) of `lazyrex-hub test` and the Go lab tests: their removal.
// A run removes its lab when it ends. A lab that a run keeps on purpose (-keep, or a failed Go lab
// test) gets a KEEP marker that says who kept it and why. Labs that a killed run left (SIGKILL, a
// go test timeout) are removed by the sweep: at most once per hour per user, at the start of a
// run, it removes each old lab of this user that has no KEEP marker and no live server. A kept lab
// is removed with `lazyrex-hub test -clean <dir>` after its review (docs/testing.md).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
	By  string `json:"by"`  // the command or test that kept it
	Why string `json:"why"` // for example the file and its result
	At  string `json:"at"`
}

// markLabKept writes the KEEP marker of the lab dir.
func markLabKept(dir, by, why string) error {
	b, _ := json.Marshal(labKeep{By: by, Why: why, At: time.Now().UTC().Format(time.RFC3339)})
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

// labLive reports whether a server accepts a connection on the lab's socket.
func labLive(dir string) bool {
	c, err := net.DialTimeout("unix", filepath.Join(dir, "rex.sock"), 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
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
// that has no KEEP marker, is older than labStaleAfter and has no live server. It runs at most
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
	for _, e := range entries {
		if !labDirName.MatchString(e.Name()) || e.Type()&fs.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() || !labOwned(fi) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, labKeepFile)); !errors.Is(err, fs.ErrNotExist) {
			continue // kept, or the marker cannot be checked
		}
		age := labAge(dir, fi, now)
		if age < labStaleAfter || labLive(dir) {
			continue
		}
		if err := removeLab(dir); err != nil {
			logf("lab sweep: " + err.Error())
			continue
		}
		logf(fmt.Sprintf("lab sweep: removed %s (age %s, no KEEP marker, no server; to keep a lab, put a file named KEEP in it)", dir, age.Round(time.Minute)))
	}
}

// cleanLab is `lazyrex-hub test -clean <dir>`: it removes a lab, kept or not, after its review.
// It refuses a path that is not a lab directory of this user in root, and a lab with a live server.
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
	if labLive(abs) {
		return fmt.Errorf("%s has a live server on rex.sock; stop it first", dir)
	}
	return removeLab(abs)
}
