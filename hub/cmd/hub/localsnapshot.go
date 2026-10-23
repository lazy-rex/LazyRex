package main

// Local snapshots (the P1 plan r6, section 4). An explicit start of a local source copies the
// source directory into <state>/plugins/local and runs the copy, as a GitHub source runs its cache
// entry. So an automatic start runs the same files again, and an edit of the source takes effect
// at the next explicit start.

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The bounds of a local copy. Package variables: a test lowers them.
var (
	localSnapshotFiles       = 10000
	localSnapshotBytes int64 = 64 << 20
)

// localSnapshotHook runs after the candidate check and before the copy (a test changes the source
// there).
var localSnapshotHook func()

// localCopyHook runs for each file after its stat and before its copy (a test makes the file grow
// there).
var localCopyHook func(path string)

// publishLocal gives the run of the local candidate cand of entry e from a published snapshot:
//  1. Copy the source into a stage, local/.stage-<token>. A .git directory is skipped. A symlink or
//     another non-regular file refuses the start; so does a copy over the bounds.
//  2. Set the published modes (as a cache entry has), and compute the digest of the stage.
//  3. Reuse an existing local/<hex>.<token> entry with that digest if it verifies; else rename the
//     stage to a new local/<hex>.<token>. An entry that does not verify is never used or changed.
//  4. Read the manifest again from the snapshot: it must give the candidate's fields, else the
//     start refuses ("the source changed during the start").
//
// A GitHub candidate, a run that is already published (an automatic start's selected run), and
// a source run (testSourceRun) are returned unchanged. A failure after step 3 leaves a new entry
// that no row names; a prune removes it. publishLocal never removes an entry.
func (h *Hub) publishLocal(e *configEntry, cand *pluginRun) (*pluginRun, error) {
	if e.github || cand.Entry != "" || h.testSourceRun(cand.Dir) {
		return cand, nil
	}
	if localSnapshotHook != nil {
		localSnapshotHook()
	}
	root := filepath.Join(h.cfg.State, "plugins", "local")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("plugin %s: the snapshot directory: %w", cand.Name, err)
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	stage := &cacheStage{Dir: filepath.Join(root, ".stage-"+token)}
	if err := copyLocal(cand.Name, cand.Dir, stage.Dir); err != nil {
		stage.discard()
		return nil, err
	}
	if err := stage.seal(); err != nil {
		stage.discard()
		return nil, fmt.Errorf("plugin %s: the snapshot: %w", cand.Name, err)
	}
	digest, err := dirDigest(stage.Dir)
	if err != nil {
		stage.discard()
		return nil, fmt.Errorf("plugin %s: the snapshot: %w", cand.Name, err)
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	entry := ""
	matches, _ := filepath.Glob(filepath.Join(root, hexDigest+".*"))
	slices.Sort(matches)
	for _, m := range matches {
		if d, err := dirDigest(m); err == nil && d == digest {
			entry = "local/" + filepath.Base(m)
			break
		}
	}
	if entry != "" {
		stage.discard()
	} else {
		name := hexDigest + "." + token
		if err := os.Rename(stage.Dir, filepath.Join(root, name)); err != nil {
			stage.discard()
			return nil, fmt.Errorf("plugin %s: publish the snapshot: %w", cand.Name, err)
		}
		entry = "local/" + name
	}
	dir := filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(entry))
	m, main, err := readPluginManifest(dir)
	if err != nil || !sameManifest(m, main, dir, cand) {
		return nil, refuse("refused", "plugin %s: the source changed during the start; start it again", cand.Name)
	}
	run := *cand
	run.Dir, run.Path, run.Entry, run.Digest = dir, main, entry, digest
	return &run, nil
}

// sameManifest reports whether the manifest m of the snapshot dir (main: its main file) gives the
// fields of the candidate cand.
func sameManifest(m *pluginManifest, main, dir string, cand *pluginRun) bool {
	rel, err := filepath.Rel(dir, main)
	want, werr := filepath.Rel(cand.Dir, cand.Path)
	return err == nil && werr == nil && rel == want && m.Name == cand.Name && m.SDK == cand.SDK &&
		m.Events == cand.Events && m.Notice == cand.Notice && slices.Equal(m.Depends, cand.Depends)
}

// copyLocal copies the local source src into the new directory dst, as a stage, within the bounds: files 0600, or
// 0700 when any execute bit is set; directories 0700. A .git directory is skipped.
func copyLocal(name, src, dst string) error {
	files, size := 0, int64(0)
	over := func() error {
		return refuse("refused", "plugin %s: the source %s is over the copy bound (%d files, %d bytes)", name, src, localSnapshotFiles, localSnapshotBytes)
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			if d.Name() == ".git" && p != src {
				return filepath.SkipDir
			}
			return os.Mkdir(out, 0o700)
		}
		if !d.Type().IsRegular() {
			return refuse("refused", "plugin %s: %s is not a regular file; a local source is copied, and the copy has no symlinks", name, filepath.ToSlash(rel))
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if files++; files > localSnapshotFiles {
			return over()
		}
		if localCopyHook != nil {
			localCopyHook(p)
		}
		// The bound counts the bytes that the copy wrote: a file can grow after its stat.
		n, err := copyBounded(p, out, localSnapshotBytes-size)
		if err != nil {
			return err
		}
		if size += n; size > localSnapshotBytes {
			return over()
		}
		if fi.Mode()&0o111 != 0 {
			return os.Chmod(out, 0o700)
		}
		return nil
	})
}

// copyBounded copies the file src into the new file dst (0600), at most limit+1 bytes, and gives
// the number of bytes that it copied.
func copyBounded(src, dst string, limit int64) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// testSourceRun reports a local dir that the hub runs from its source dir: -test and the config
// key test_source_runs name it (a lab of `lazyrex-hub test`, the plugin under test). Its run has
// no Entry and no Digest. Every other run is published.
func (h *Hub) testSourceRun(dir string) bool {
	return h.cfg.Test && dir != "" && slices.Contains(h.cfg.TestSourceRuns, dir)
}
