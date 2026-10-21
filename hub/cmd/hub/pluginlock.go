package main

// The plugin lock (docs/plugin-sources.md, section 3): a file that pins each GitHub source to one
// commit. The key and the commit are the pin; every other field is derived from them, so sync can
// rebuild it. The hub only reads the lock (readLock, lockedDir); the plugin commands write it
// (writeLock), always by a rename.
//
// For one source string and one commit, the pin (lockPin) is the same on every machine: the name
// and the digest depend only on the commit's tree. dir is local: it names this machine's cache
// entry. A lock entry from another machine therefore has a dir that the local cache may not have,
// and sync repairs it like any missing cache entry.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// lockVersion is the only lock version this hub reads and writes. The lock file is written and
// read by the same binary, so another version is an error, not a newer format to tolerate.
const lockVersion = 1

// pluginLock is the decoded lock file. Plugins is keyed by the exact source string of the config.
type pluginLock struct {
	Version int                   `json:"version"`
	Plugins map[string]*lockEntry `json:"plugins"`
}

// lockEntry is one lock entry: the pin, and the cache entry of this machine.
type lockEntry struct {
	lockPin
	Dir string `json:"dir"` // relative to <state>/plugins: github/<owner>/<repo>/<commit>.<token>
}

// lockPin is the part of a lock entry that does not depend on the machine.
type lockPin struct {
	Name    string `json:"name"`
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Subdir  string `json:"subdir"` // the plugin directory in the repository; "" for the root
	Ref     string `json:"ref"`
	RefKind string `json:"ref_kind"` // branch, tag or commit
	Commit  string `json:"commit"`
	Digest  string `json:"digest"`
}

var (
	commitID   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	lockDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	cacheToken = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// lockInvalidError is a lock file that does not decode: bad JSON, a wrong structure or another
// version. It gives no trustworthy pins, so its text does not name sync, and the commands never
// replace such a file.
type lockInvalidError struct {
	path string
	err  error
}

func (e *lockInvalidError) Error() string {
	return fmt.Sprintf("the lock file %s is not valid: %v; repair the file or restore it from a backup", e.path, e.err)
}

func (e *lockInvalidError) Unwrap() error { return e.err }

// lockReadError is a lock file that exists but cannot be read (for example, no read permission).
type lockReadError struct {
	path string
	err  error
}

func (e *lockReadError) Error() string {
	return fmt.Sprintf("cannot read the lock file %s: %v", e.path, e.err)
}

func (e *lockReadError) Unwrap() error { return e.err }

// lockPath is the lock file of the config cfgPath (section 1). field is the config field lock:
// relative to the config directory. Without it, a final .json of the config file name becomes
// .lock.json; another name gets .lock.json appended. It reads no file.
func lockPath(cfgPath, field string) string {
	if field != "" {
		if filepath.IsAbs(field) {
			return filepath.Clean(field)
		}
		return filepath.Join(filepath.Dir(cfgPath), field)
	}
	return strings.TrimSuffix(cfgPath, ".json") + ".lock.json"
}

// readLock reads the lock file. A missing file gives an empty lock and no error. A file that does
// not parse, or has another version, gives a *lockInvalidError. A file that cannot be read gives a
// *lockReadError. Both texts are final: a caller adds only its own prefix, for example
// "plugin <source>: ".
//
// Only the file structure is checked here. An empty or missing string field in an entry decodes to
// "" and is an agreement failure of that entry (lockEntry.agree), so sync can repair it.
func readLock(path string) (*pluginLock, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &pluginLock{Version: lockVersion, Plugins: map[string]*lockEntry{}}, nil
	}
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return nil, &lockReadError{path, err}
	}
	l, err := decodeLock(b)
	if err != nil {
		return nil, &lockInvalidError{path, err}
	}
	return l, nil
}

// decodeLock decodes and checks the structure of a lock file.
func decodeLock(b []byte) (*pluginLock, error) {
	var top struct {
		Version *int            `json:"version"`
		Plugins json.RawMessage `json:"plugins"`
	}
	if err := decodeStrict(b, &top); err != nil {
		return nil, err
	}
	if top.Version == nil {
		return nil, errors.New("version is missing")
	}
	if *top.Version != lockVersion {
		return nil, fmt.Errorf("version %d is not supported (this hub reads version %d)", *top.Version, lockVersion)
	}
	if !isObject(top.Plugins) {
		return nil, errors.New("plugins must be an object")
	}
	var raws map[string]json.RawMessage
	if err := json.Unmarshal(top.Plugins, &raws); err != nil {
		return nil, fmt.Errorf("plugins: %w", err)
	}
	l := &pluginLock{Version: lockVersion, Plugins: map[string]*lockEntry{}}
	for src, raw := range raws {
		if !isObject(raw) {
			return nil, fmt.Errorf("the entry for %s must be an object", src)
		}
		e := &lockEntry{}
		if err := decodeStrict(raw, e); err != nil {
			return nil, fmt.Errorf("the entry for %s: %w", src, err)
		}
		l.Plugins[src] = e
	}
	return l, nil
}

// decodeStrict decodes one JSON value into v. It refuses unknown fields and data after the value.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the object")
	}
	return nil
}

// isObject reports whether raw is a JSON object (not missing, not null, not another type).
func isObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

// encodeLock gives the bytes of a lock file. Equal lock values give equal bytes: the fields have
// a fixed order, map keys are sorted, and the file ends with a newline.
func encodeLock(l *pluginLock) ([]byte, error) {
	out := pluginLock{Version: lockVersion, Plugins: l.Plugins}
	if out.Plugins == nil {
		out.Plugins = map[string]*lockEntry{}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// writeLock replaces the lock file with one rename: a temporary file in the same directory,
// fsync, rename, and fsync of the directory. A reader sees the old file or the new file, never a
// mix. The caller holds both plugin command flocks (section 6).
func writeLock(path string, l *pluginLock) error {
	b, err := encodeLock(l)
	if err != nil {
		return err
	}
	return replaceFile(path, b)
}

// githubRef is a parsed GitHub source: github:<owner>/<repo>[/<subdir>]@<ref>.
type githubRef struct{ Owner, Repo, Subdir, Ref string }

// githubRef gives the parts of a GitHub entry. parseEntries (checkSource) parsed them.
func (e *configEntry) githubRef() githubRef {
	return githubRef{Owner: e.owner, Repo: e.repo, Subdir: e.subdir, Ref: e.gitRef}
}

// refKinds gives the ref kinds that the form of ref allows (section 5, "Ref forms"): a commit
// ID, a full branch or tag ref, or a bare name (a branch or a tag; ls-remote decides).
func refKinds(ref string) []string {
	switch {
	case commitID.MatchString(ref):
		return []string{"commit"}
	case strings.HasPrefix(ref, "refs/heads/"):
		return []string{"branch"}
	case strings.HasPrefix(ref, "refs/tags/"):
		return []string{"tag"}
	}
	return []string{"branch", "tag"}
}

// trust reports why the locked commit cannot be trusted, or "" when it is trusted (section 3,
// "Repair"): 40 lowercase hex and, for a commit ref, equal to the ref. Whether the commit exists
// and is available needs git; plugin sync checks that.
func (e *lockEntry) trust(r githubRef) string {
	switch {
	case e.Commit == "":
		return "commit is missing"
	case !commitID.MatchString(e.Commit):
		return "commit is not 40 lowercase hex"
	case commitID.MatchString(r.Ref) && e.Commit != r.Ref:
		return "commit is not the commit of the ref"
	}
	return ""
}

// agree gives the first field of the lock entry that does not agree with the source r, or ""
// (section 3, "Agreement check"). It checks the fields that need no file: the manifest name and
// the digest are checked against the cache entry (lockedDir). A name or digest that is missing,
// or of the wrong form, is reported here.
func (e *lockEntry) agree(r githubRef) string {
	switch {
	case e.Owner != r.Owner:
		return "owner"
	case e.Repo != r.Repo:
		return "repo"
	case e.Subdir != r.Subdir:
		return "subdir"
	case e.Ref != r.Ref:
		return "ref"
	case !slices.Contains(refKinds(r.Ref), e.RefKind):
		return "ref_kind"
	case e.trust(r) != "":
		return "commit"
	case !e.dirForm(r):
		return "dir"
	case !pluginName.MatchString(e.Name):
		return "name"
	case !lockDigest.MatchString(e.Digest):
		return "digest"
	}
	return ""
}

// dirForm reports whether dir is github/<owner>/<repo>/<commit>.<token>, with the source's owner
// and repo and the entry's commit.
func (e *lockEntry) dirForm(r githubRef) bool {
	want := "github/" + r.Owner + "/" + r.Repo + "/" + e.Commit + "."
	tok, ok := strings.CutPrefix(e.Dir, want)
	return ok && cacheToken.MatchString(tok)
}

// lockMismatch is the text of an agreement failure. It names sync when the commit is trusted, and
// update when it is not (section 3). cfgPath is the config as its command names it.
func lockMismatch(cfgPath, source string, e *lockEntry, r githubRef, field string) error {
	if why := e.trust(r); why != "" {
		return untrustedCommit(cfgPath, source, why)
	}
	return fmt.Errorf("the lock entry for %s does not match: %s; run lazyrex-hub plugin sync -config %s", source, field, cfgPath)
}

// untrustedCommit is the text of a locked commit that sync must not keep (section 3).
func untrustedCommit(cfgPath, source, why string) error {
	return fmt.Errorf("the locked commit for %s cannot be trusted: %s; run lazyrex-hub plugin update -config %s %s", source, why, cfgPath, source)
}
