package main

// The backup of a release (docs/releasing.md, step backup): the four hold files, and for a hub
// release hub.db and a listing of the plugin SQL directory. A rollback never copies a file back:
// pins move back through plugin pull, and hub.db is not restored, because a restore rolls back the
// store data of every plugin. The backup is the evidence for the checks and for a person.

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const relStampFile = "stamp"

// Backup file names. The base file keeps its name beside lock.json, so readPeers reads it.
const (
	bakConfig  = "config.json"
	bakManaged = "plugins.json"
	bakLock    = "lock.json"
	bakDB      = "hub.db"
	bakSQLList = "sql-listing.txt"
	bakSums    = "SHA256SUMS"
)

func bakBase() string { return peersPath(bakLock) }

// stamp returns the stamp of the backup of this run, "" without one.
func (r *releaser) stamp() (string, error) {
	b, err := os.ReadFile(filepath.Join(r.run, relStampFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// backupPath is the backup directory of a stamp.
func (r *releaser) backupPath(stamp string) string { return filepath.Join(r.run, "backup-"+stamp) }

// checkAbsent fails when a path of the spec's absent list exists (also as a broken symlink).
func (r *releaser) checkAbsent() error {
	for _, p := range r.spec.Absent {
		if _, err := os.Lstat(p); err == nil {
			return relFail("%s exists; the spec says it must not exist", p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return relFail("%s: %v", p, err)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// backupDB writes a consistent copy of the hub database with VACUUM INTO, then checks the copy with
// integrity_check. The hub is stopped at this step, but the copy does not depend on it.
func backupDB(src, dst string) error {
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	_, err = db.Exec("VACUUM INTO ?", dst)
	db.Close()
	if err != nil {
		return fmt.Errorf("the copy of %s: %w", src, err)
	}
	return checkIntegrity(dst)
}

func checkIntegrity(path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&res); err != nil {
		return fmt.Errorf("integrity_check of %s: %w", path, err)
	}
	if res != "ok" {
		return fmt.Errorf("integrity_check of %s: %s", path, res)
	}
	return nil
}

// sqlListing lists every file under dir as "<relative path> <size> <sha256>", sorted. A missing
// directory gives "" (no plugin opened its SQL database yet).
func sqlListing(dir string) (string, error) {
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := sha256File(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		lines = append(lines, fmt.Sprintf("%s %d %s", rel, info.Size(), h))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// writeBackup writes a new backup directory for stamp and returns its path. The stamp file is not
// written here: the caller writes it last, so a backup without a stamp is incomplete.
func (r *releaser) writeBackup(stamp string) (string, error) {
	b := r.backupPath(stamp)
	if err := os.Mkdir(b, 0o700); err != nil {
		return "", relFail("the backup directory: %v", err)
	}
	for _, f := range []struct{ src, dst string }{
		{r.cfg.path, bakConfig}, {r.cfg.managed, bakManaged}, {r.cfg.lock, bakLock}, {peersPath(r.cfg.lock), bakBase()}} {
		if err := copyFile(f.src, filepath.Join(b, f.dst)); err != nil {
			return "", relFail("the backup of %s: %v", f.src, err)
		}
	}
	if err := backupDB(filepath.Join(r.cfg.state, "hub.db"), filepath.Join(b, bakDB)); err != nil {
		return "", relFail("%v", err)
	}
	list, err := sqlListing(filepath.Join(r.cfg.state, "sql"))
	if err != nil {
		return "", relFail("the listing of %s: %v", filepath.Join(r.cfg.state, "sql"), err)
	}
	if err := os.WriteFile(filepath.Join(b, bakSQLList), []byte(list), 0o600); err != nil {
		return "", err
	}
	return b, r.writeSums(b)
}

// backupNames are the files that a backup holds. A plugin release backs up hub.db and the sql
// listing too: P at TO can change its records and its own database (plan c4 v2, Q1).
func (r *releaser) backupNames() []string {
	return []string{bakConfig, bakManaged, bakLock, bakBase(), bakDB, bakSQLList}
}

// writeSums writes <backup>/SHA256SUMS, the sha256 of each backup file, after every file is
// written. checkBackup compares each file with it.
func (r *releaser) writeSums(b string) error {
	var sums strings.Builder
	for _, n := range r.backupNames() {
		h, err := sha256File(filepath.Join(b, n))
		if err != nil {
			return relFail("the backup %s: %v", b, err)
		}
		fmt.Fprintf(&sums, "%s  %s\n", h, n)
	}
	if err := replaceFile(filepath.Join(b, bakSums), []byte(sums.String())); err != nil {
		return relFail("the backup %s: %v", b, err)
	}
	return nil
}

// checkBackup checks a backup: SHA256SUMS lists exactly the files of this kind, each file has its
// listed sha256, and hub.db passes integrity_check (a hub release).
func (r *releaser) checkBackup(b string) error {
	raw, err := os.ReadFile(filepath.Join(b, bakSums))
	if err != nil {
		return relFail("the backup %s has no %s: %v", b, bakSums, err)
	}
	listed := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		h, n, ok := strings.Cut(line, "  ")
		if !ok || len(h) != 64 {
			return relFail("the backup %s: %s has a malformed line %q", b, bakSums, line)
		}
		listed[n] = h
	}
	names := r.backupNames()
	if len(listed) != len(names) {
		return relFail("the backup %s: %s lists %d files, not %d", b, bakSums, len(listed), len(names))
	}
	for _, n := range names {
		h, err := sha256File(filepath.Join(b, n))
		if err != nil {
			return relFail("the backup %s: %v", b, err)
		}
		if listed[n] != h {
			return relFail("the backup %s: %s differs from %s", b, n, bakSums)
		}
	}
	if err := checkIntegrity(filepath.Join(b, bakDB)); err != nil {
		return relFail("the backup %s: %v", b, err)
	}
	return nil
}

// setStamp writes the stamp file last (temporary file, fsync, rename).
func (r *releaser) setStamp(stamp string) error {
	return replaceFile(filepath.Join(r.run, relStampFile), []byte(stamp+"\n"))
}
