package main

// The files of `lazyrex-hub setup`. Setup reads each file once into a snapshot, shows the user the
// complete diff, and writes the file only after the user's yes and only if the file still equals
// the snapshot. Nothing here writes before that: -print and a run without a terminal never reach
// write, so they create no file and no directory.
//
// A symlink is followed: setup reads and writes the file that the complete path resolves to (every
// symlink, also in the parent directories), so a link that a dotfile manager keeps stays a link. A
// symlink that does not resolve to a regular file is blocked. The snapshot holds the resolved path
// and the permissions, and write refuses a change of either. That recheck is not an atomic
// compare-and-replace: a writer between the recheck and the rename is not detected.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type setupFile struct {
	path   string      // the path that setup names to the user
	target string      // the file that setup reads and writes: path with every symlink resolved
	exists bool        // the file existed at the snapshot
	old    []byte      // the snapshot
	perm   os.FileMode // the existing file's permissions, or the new file's
	next   []byte      // the proposed content; nil: nothing proposed
	block  string      // why setup does not write the file; "" if it may
	notes  []string    // reported with the file
}

// snapshotFile reads path. newPerm is the permissions of a new file.
func snapshotFile(path string, newPerm os.FileMode) *setupFile {
	f := &setupFile{path: path, target: path, perm: newPerm}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if f.target, err = resolveNew(path); err != nil {
			f.block = err.Error()
		}
		return f
	}
	if err != nil {
		f.block = err.Error()
		return f
	}
	t, err := filepath.EvalSymlinks(path)
	if err != nil {
		if fi.Mode()&fs.ModeSymlink != 0 {
			err = fmt.Errorf("it is a symlink that does not resolve (%v); setup does not replace a symlink", err)
		}
		f.block = err.Error()
		return f
	}
	f.target = t
	if fi, err = os.Stat(t); err != nil {
		f.block = err.Error()
		return f
	}
	if !fi.Mode().IsRegular() {
		f.block = "it is not a regular file"
		return f
	}
	b, err := os.ReadFile(f.target)
	if err != nil {
		f.block = err.Error()
		return f
	}
	f.exists, f.old, f.perm = true, b, fi.Mode().Perm()
	return f
}

// changes reports whether setup proposes to write the file.
func (f *setupFile) changes() bool {
	return f.block == "" && f.next != nil && (!f.exists || !bytes.Equal(f.old, f.next))
}

// write writes f.next. It refuses a file that is not the snapshot any more. Before it changes an
// existing file, it copies the file to a new backup and returns the backup's path. A new file is
// created with a link, so a file that appeared after the snapshot is never replaced.
func (f *setupFile) write(now time.Time) (backup string, err error) {
	cur := snapshotFile(f.path, f.perm)
	if cur.block != "" {
		return "", errors.New(cur.block)
	}
	if cur.target != f.target || cur.exists != f.exists || !bytes.Equal(cur.old, f.old) || f.exists && cur.perm != f.perm {
		return "", errors.New("the file changed after setup showed it; run setup again")
	}
	dir := filepath.Dir(f.target)
	if f.exists {
		if backup, err = writeBackup(f.target, f.old, f.perm, now); err != nil {
			return "", err
		}
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".lazyrex-setup-*")
	if err != nil {
		return backup, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(f.next)
	if err == nil {
		err = tmp.Chmod(f.perm)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return backup, err
	}
	if f.exists {
		return backup, os.Rename(tmp.Name(), f.target)
	}
	if err := os.Link(tmp.Name(), f.target); errors.Is(err, fs.ErrExist) {
		return "", errors.New("the file appeared after setup showed it; run setup again")
	} else if err != nil {
		return "", err
	}
	return "", nil
}

// resolveNew resolves the path of a file that does not exist: the part of its parent chain that
// exists, with every symlink resolved, and then the rest of the path.
func resolveNew(path string) (string, error) {
	p, rest := filepath.Clean(path), ""
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(p)
		if !errors.Is(err, fs.ErrNotExist) || parent == p {
			return "", err
		}
		rest, p = filepath.Join(filepath.Base(p), rest), parent
	}
}

// writeBackup writes old to a new file <target>.lazyrex-bak-<unix time>, with the original's
// permissions. It never replaces a file: a second backup in the same second gets a suffix.
func writeBackup(target string, old []byte, perm os.FileMode, now time.Time) (string, error) {
	base := fmt.Sprintf("%s.lazyrex-bak-%d", target, now.Unix())
	for i := 1; i <= 100; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		b, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = b.Write(old)
		if err == nil {
			err = b.Sync()
		}
		if cerr := b.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(name)
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("%s: too many backups in one second", base)
}

// diffLimit bounds the line diff (lines of old times lines of new). A larger change is shown as
// its complete new text.
const diffLimit = 4 << 20

// fileDiff is the complete diff from old to next: every line, with the prefix "  " (kept), "- "
// (removed) or "+ " (added).
func fileDiff(old, next []byte) string {
	a, b := diffLines(old), diffLines(next)
	var out strings.Builder
	if len(a)*len(b) > diffLimit {
		for _, l := range a {
			out.WriteString("- " + l + "\n")
		}
		for _, l := range b {
			out.WriteString("+ " + l + "\n")
		}
		return out.String()
	}
	// lcs[i][j]: the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			out.WriteString("  " + a[i] + "\n")
			i, j = i+1, j+1
		case j < len(b) && (i == len(a) || lcs[i][j+1] >= lcs[i+1][j]):
			out.WriteString("+ " + b[j] + "\n")
			j++
		default:
			out.WriteString("- " + a[i] + "\n")
			i++
		}
	}
	return out.String()
}

func diffLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// appendLines returns text with each missing line appended. A line is present when a line of
// text, without surrounding spaces, equals it.
func appendLines(text []byte, lines ...string) []byte {
	have := map[string]bool{}
	for _, l := range strings.Split(string(text), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	out := append([]byte(nil), text...)
	for _, l := range lines {
		if have[l] {
			continue
		}
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, l+"\n"...)
		have[l] = true
	}
	return out
}

// shellWord quotes a path for the shell that runs a hook command. The double-quoted form is the
// text of hooks/README.md. A path with a character that double quotes do not protect gets
// single quotes.
func shellWord(s string) string {
	if !strings.ContainsAny(s, "\"$`\\!\n") {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
