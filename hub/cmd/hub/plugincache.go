package main

// The plugin cache (docs/plugin-sources.md, section 4): fetched GitHub sources under
// <state>/plugins/github/<owner>/<repo>/<commit>.<token>/. A published entry never moves and is
// never written: a repair publishes a new entry. lockedDir is the hub's check of a lock entry and
// its cache entry; it runs no git and uses no network.
//
// Publication: the plugin commands extract a tree into a stage directory beside the final path
// (<repo dir>/.stage-<token>), set every mode, and rename the stage to <commit>.<token>. The rename
// stays in one parent directory: a rename of a 0555 directory into another parent fails with
// EACCES, because the move must update the directory's "..". Observed on macOS 27.0.1 (APFS); the
// reviewer saw the same. Linux: not tested.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// digestFile is one line of a cache digest: a file path (slash separated, relative to the entry),
// whether it is executable, its size, and the sha256 of its content.
type digestFile struct {
	Path string
	Exec bool
	Size int64
	Sum  [sha256.Size]byte
}

// digestOf gives the digest of a tree (section 4): sha256 over the lines
// <path>\0<x|->\0<size>\0<sha256 hex of content>\n, sorted by path bytes.
func digestOf(files []digestFile) string {
	sorted := append([]digestFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		x := "-"
		if f.Exec {
			x = "x"
		}
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%x\n", f.Path, x, f.Size, f.Sum)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// dirDigest gives the digest of the published entry root. A file is executable when any execute
// bit is set. A symlink, or any type other than a file or a directory, is an error: a published
// entry has none.
func dirDigest(root string) (string, error) {
	var files []digestFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: %w", filepath.ToSlash(rel), errNotRegular)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		df := digestFile{Path: filepath.ToSlash(rel), Exec: fi.Mode()&0o111 != 0, Size: n}
		copy(df.Sum[:], h.Sum(nil))
		files = append(files, df)
		return nil
	})
	if err != nil {
		return "", err
	}
	return digestOf(files), nil
}

// errNotRegular is a path in a cache entry that is not a regular file or a directory.
var errNotRegular = errors.New("not a regular file")

// randomToken gives 16 random hex digits: the token of a cache entry or a temporary directory.
func randomToken() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// cacheStage is a tree that is not published yet: <state>/plugins/github/<owner>/<repo>/.stage-<token>.
type cacheStage struct {
	Dir   string // the stage directory; the extraction writes into it
	final string // <commit>.<token> beside it
	rel   string // final, relative to <state>/plugins (the lock field dir)
}

// newStage makes an empty stage for a tree of owner/repo at commit. The parent directories are
// 0700. Files written into the stage must be 0600, or 0700 for an executable file; publish sets
// the published modes from these.
func newStage(stateDir, owner, repo, commit string) (*cacheStage, error) {
	parent := filepath.Join(stateDir, "plugins", "github", owner, repo)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	tok, err := randomToken()
	if err != nil {
		return nil, err
	}
	s := &cacheStage{
		Dir:   filepath.Join(parent, ".stage-"+tok),
		final: filepath.Join(parent, commit+"."+tok),
		rel:   "github/" + owner + "/" + repo + "/" + commit + "." + tok,
	}
	if err := os.Mkdir(s.Dir, 0o700); err != nil {
		return nil, err
	}
	return s, nil
}

// cacheChmod sets a mode during publish. Tests replace it to make a chmod fail.
var cacheChmod = os.Chmod

// publish sets every published mode (files 0444, executable files 0555, directories 0555, the
// stage root last) and then renames the stage to its final path. It returns the lock field dir.
// Nothing writes the entry after the rename. On an error, the stage is removed and nothing is
// published.
func (s *cacheStage) publish() (string, error) {
	if err := s.seal(); err != nil {
		s.discard()
		return "", err
	}
	if _, err := os.Lstat(s.final); !errors.Is(err, fs.ErrNotExist) {
		s.discard()
		return "", fmt.Errorf("cache entry %s exists", s.final)
	}
	if err := os.Rename(s.Dir, s.final); err != nil {
		s.discard()
		return "", err
	}
	return s.rel, nil
}

// seal sets the published modes, deepest paths first, so each directory becomes 0555 after its
// contents.
func (s *cacheStage) seal() error {
	var paths []string
	modes := map[string]os.FileMode{}
	err := filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			modes[p] = 0o555
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			modes[p] = 0o444
			if fi.Mode()&0o100 != 0 {
				modes[p] = 0o555
			}
		default:
			return fmt.Errorf("stage %s: %s is not a file or a directory", s.Dir, p)
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return err
	}
	// WalkDir gives a directory before its contents, so the reverse order puts the root last.
	for i := len(paths) - 1; i >= 0; i-- {
		if err := cacheChmod(paths[i], modes[paths[i]]); err != nil {
			return err
		}
	}
	return nil
}

// discard removes the stage. It makes the directories writable again, only inside the stage.
func (s *cacheStage) discard() {
	removeTree(s.Dir)
}

// removeTree removes a tree that this command made (a stage, or a temporary directory under
// <state>/plugins/.tmp). Its directories can be 0555, so it adds u+w to each directory first. It
// never follows a symlink. It is never used on a published entry.
func removeTree(dir string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o700)
		}
		return nil
	})
	os.RemoveAll(dir)
}

// cacheStateError is a lockedDir error about the cache entry: state is "missing" or "changed" (the
// states of plugin status). The text is the final text of section 8.
type cacheStateError struct{ state, text string }

func (e *cacheStateError) Error() string { return e.text }

// lockedDir checks the lock entry of the GitHub entry e and its cache entry, without git and
// without the network: the key lookup, the agreement check (section 3), the cache entry, the
// digest (section 4) and the manifest name. It returns the absolute cache directory and the
// locked commit. It only reads files, and never the lock file: lock is the caller's snapshot.
// cfgPath is the config as the commands name it in the texts.
func lockedDir(cfgPath, stateDir string, lock *pluginLock, e *configEntry) (dir, commit string, err error) {
	le := lock.Plugins[e.Source]
	if le == nil {
		return "", "", fmt.Errorf("plugin %s is not installed: run lazyrex-hub plugin sync -config %s", e.Source, cfgPath)
	}
	r, ok := parseGithub(e.Source)
	if !ok {
		return "", "", fmt.Errorf("plugin %s: not a GitHub source", e.Source)
	}
	if field := le.agree(r); field != "" {
		return "", "", lockMismatch(cfgPath, e.Source, le, r, field)
	}
	dir, err = filepath.Abs(filepath.Join(stateDir, "plugins", filepath.FromSlash(le.Dir)))
	if err != nil {
		return "", "", err
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", &cacheStateError{"missing", fmt.Sprintf("plugin %s: the cache has no entry for commit %s: run lazyrex-hub plugin sync -config %s", le.Name, le.Commit, cfgPath)}
	}
	changed := &cacheStateError{"changed", fmt.Sprintf("plugin %s: the cache entry for commit %s changed (digest mismatch): run lazyrex-hub plugin sync -config %s", le.Name, le.Commit, cfgPath)}
	if err != nil {
		return "", "", fmt.Errorf("plugin %s: cannot read the cache entry %s: %w", le.Name, dir, err)
	}
	if !fi.IsDir() {
		return "", "", changed
	}
	d, err := dirDigest(dir)
	if errors.Is(err, errNotRegular) || err == nil && d != le.Digest {
		return "", "", changed
	}
	if err != nil {
		return "", "", fmt.Errorf("plugin %s: cannot read the cache entry %s: %w", le.Name, dir, err)
	}
	m, _, err := readPluginManifest(dir)
	if err != nil {
		return "", "", fmt.Errorf("plugin source %s: %w", e.Source, err)
	}
	if m.Name != le.Name {
		return "", "", lockMismatch(cfgPath, e.Source, le, r, "name")
	}
	return dir, le.Commit, nil
}
