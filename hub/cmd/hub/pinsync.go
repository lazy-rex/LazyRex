package main

// Pin sync (docs/plugin-sources.md, "Pin sync"): the machines of the user select the same pins
// for the GitHub sources that their configs share. pluginExport writes the pins of one lock as a
// pin document, and hub.peer.plugins serves it to the peer hub (peersync.go). The local lock
// format does not change: the document carries the pins (lockPin) and no path.
//
// One sidecar lives beside the lock and belongs to it (two configs that name one lock share it):
// <lock>.id, the pull identity of this lock, the "hub" of the documents it exports. A stray
// <lock>.peers.json is ignored.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// pinDocVersion is the version of a pin document and of a base file without a release tag, and
// pinDocVersionLatest the version with one (section 6a). Each file takes its version from its own
// content. Both ends are hubs of the user, which the user updates; another version is an error,
// not a format to tolerate.
const (
	pinDocVersion       = 1
	pinDocVersionLatest = 2
)

// pullID is the form of a pull identity: 32 lowercase hex.
var pullID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// pinDoc is a pin document: the pins of one lock, sorted by source.
type pinDoc struct {
	Version int        `json:"version"`
	Hub     string     `json:"hub"` // the pull identity of the exporting lock
	Entries []pinEntry `json:"entries"`
}

// pinEntry is one pin of a document: the exact source and its pin.
type pinEntry struct {
	Source string `json:"source"`
	lockPin
}

// encodePinDoc gives the bytes of a document: entries sorted by source, two-space indent, and a
// final newline. The same pins and identity give the same bytes. The version is
// pinDocVersionLatest when an entry has a latest form, else pinDocVersion.
func encodePinDoc(d *pinDoc) ([]byte, error) {
	sorted := *d
	sorted.Entries = append([]pinEntry{}, d.Entries...) // never null: an empty document has []
	sort.Slice(sorted.Entries, func(i, j int) bool { return sorted.Entries[i].Source < sorted.Entries[j].Source })
	sorted.Version = pinDocVersion
	for _, e := range sorted.Entries {
		if e.latestForm() {
			sorted.Version = pinDocVersionLatest
		}
	}
	b, err := json.MarshalIndent(&sorted, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// decodePinDoc decodes and checks a document (section "Pin sync", the checks before any fetch).
// Any error refuses the whole document.
func decodePinDoc(b []byte) (*pinDoc, error) {
	var top struct {
		Version *int            `json:"version"`
		Hub     string          `json:"hub"`
		Entries json.RawMessage `json:"entries"`
	}
	if err := decodeStrict(b, &top); err != nil {
		return nil, err
	}
	switch {
	case top.Version == nil:
		return nil, errors.New("version is missing")
	case *top.Version != pinDocVersion && *top.Version != pinDocVersionLatest:
		return nil, fmt.Errorf("pin document version %d is not supported by this hub (it reads versions %d and %d)", *top.Version, pinDocVersion, pinDocVersionLatest)
	case !pullID.MatchString(top.Hub):
		return nil, errors.New("hub must be 32 lowercase hex")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(top.Entries, &raws); err != nil || raws == nil {
		return nil, errors.New("entries must be an array")
	}
	d := &pinDoc{Version: *top.Version, Hub: top.Hub, Entries: []pinEntry{}}
	seen := map[string]bool{}
	for i, raw := range raws {
		var e pinEntry
		if err := decodeStrict(raw, &e); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i+1, err)
		}
		if seen[e.Source] {
			return nil, fmt.Errorf("entry %d: the source %s is there twice", i+1, e.Source)
		}
		seen[e.Source] = true
		if err := e.check(); err != nil {
			return nil, fmt.Errorf("entry %d (%s): %w", i+1, e.Source, err)
		}
		if d.Version == pinDocVersion && e.latestForm() {
			return nil, fmt.Errorf("entry %d (%s): a latest entry needs pin document version %d", i+1, e.Source, pinDocVersionLatest)
		}
		d.Entries = append(d.Entries, e)
	}
	return d, nil
}

// check checks one document entry against its source string: a GitHub source whose owner, repo,
// subdir and ref are the entry's; a ref_kind that the ref form allows; a commit of 40 lowercase hex
// that equals a commit ref; a release tag exactly on a latest entry; and the name and digest
// forms.
func (e *pinEntry) check() error {
	c := &configEntry{Source: e.Source}
	if err := c.checkSource(""); err != nil {
		return err
	}
	if !c.github {
		return errors.New("not a GitHub source")
	}
	r := c.githubRef()
	switch {
	case e.Owner != r.Owner:
		return errors.New("owner does not match the source")
	case e.Repo != r.Repo:
		return errors.New("repo does not match the source")
	case e.Subdir != r.Subdir:
		return errors.New("subdir does not match the source")
	case e.Ref != r.Ref:
		return errors.New("ref does not match the source")
	case !slices.Contains(refKinds(r.Ref), e.RefKind):
		return fmt.Errorf("ref_kind %q is not allowed for the ref %s", e.RefKind, r.Ref)
	case !commitID.MatchString(e.Commit):
		return errors.New("commit is not 40 lowercase hex")
	case commitID.MatchString(r.Ref) && e.Commit != r.Ref:
		return errors.New("commit is not the commit of the ref")
	case e.RefKind == "latest" && e.Tag == "":
		return errors.New("tag is missing")
	case e.RefKind == "latest" && !versionTag.MatchString(e.Tag):
		return fmt.Errorf("tag %q is not a version tag vX.Y.Z", e.Tag)
	case e.RefKind != "latest" && e.Tag != "":
		return fmt.Errorf("tag is set, but ref_kind is %s", e.RefKind)
	case !pluginName.MatchString(e.Name):
		return errors.New("name is not a plugin name")
	case !lockDigest.MatchString(e.Digest):
		return errors.New("digest is not sha256:<64 lowercase hex>")
	}
	return nil
}

// idPath is the sidecar of the lock file lockFile that holds its pull identity.
func idPath(lockFile string) string { return lockFile + ".id" }

// sidecarInvalid is the text of a sidecar that does not decode.
func sidecarInvalid(path, fix string, err error) error {
	return fmt.Errorf("%s is not valid: %v; %s", path, err, fix)
}

// readPullID reads the pull identity of a lock. A missing file gives "" and no error.
func readPullID(lockFile string) (string, error) {
	p := idPath(lockFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id, ok := strings.CutSuffix(string(b), "\n")
	if !ok || !pullID.MatchString(id) {
		return "", sidecarInvalid(p, "remove it to make a new pull identity", errors.New("not 32 lowercase hex and a newline"))
	}
	return id, nil
}

// pullIdentity gives the pull identity of a lock, and makes one when the lock has none: a new
// file (O_EXCL), fsync, and fsync of the directory. The caller holds both plugin command flocks.
func pullIdentity(lockFile string) (string, error) {
	id, err := readPullID(lockFile)
	if err != nil || id != "" {
		return id, err
	}
	a, err := randomToken()
	if err != nil {
		return "", err
	}
	b, err := randomToken()
	if err != nil {
		return "", err
	}
	id = a + b
	p := idPath(lockFile)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		f.Close()
		os.Remove(p)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(p)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(p)
		return "", err
	}
	return id, syncDir(filepath.Dir(p))
}

// replaceFile writes b to path by a rename: a reader sees the old file or the new file.
func replaceFile(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after the rename
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := replaceSyncDir(dir); err != nil {
		return &replacedError{path: path, err: err}
	}
	return nil
}

// replacedError is a replaceFile failure after the rename: readers see the new bytes, but a crash
// can still undo the change. A caller never reports the file as unchanged, and rolls nothing back.
type replacedError struct {
	path string
	err  error
}

func (e *replacedError) Error() string {
	return e.path + " is replaced, but the sync of its directory failed (a crash can undo the change): " + e.err.Error()
}

func (e *replacedError) Unwrap() error { return e.err }

// replaceSyncDir makes the rename of replaceFile durable. Tests replace it to fail after the rename.
var replaceSyncDir = syncDir

// syncDir makes a directory entry change durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// pluginExport writes the pin document of the lock to w. Under
// both flocks it reads the lock and the managed file as one snapshot. It exports each GitHub
// entry of the config and of the managed file, and only when every one passes lockedDir: a machine
// never exports a pin that it cannot run itself. A lock entry without an entry is not exported,
// and a local source has no pin. A failed check writes nothing and makes no pull identity. After
// the checks, the first export makes the pull identity; it stays when the write to w then fails.
func pluginExport(cfgPath string, w io.Writer) error {
	fail := func(err error) error { return fmt.Errorf("pin document: %w", err) }
	cfg, err := readPluginCmdConfig(cfgPath)
	if err != nil {
		return fail(err)
	}
	release, err := lockCommands(cfg.state, cfg.lock)
	if err != nil {
		return fail(err)
	}
	defer release()
	lock, err := cfg.loadSnapshot()
	if err != nil {
		return fail(err)
	}
	doc := &pinDoc{Version: pinDocVersion, Entries: []pinEntry{}}
	for _, e := range cfg.entries {
		if !e.github {
			continue
		}
		if e.static != nil {
			return fail(e.static)
		}
		if _, _, err := lockedDir(cfg.state, lock, e); err != nil {
			return fail(err)
		}
		doc.Entries = append(doc.Entries, pinEntry{Source: e.Source, lockPin: lock.Plugins[e.Source].lockPin})
	}
	if doc.Hub, err = pullIdentity(cfg.lock); err != nil {
		return fail(err)
	}
	b, err := encodePinDoc(doc)
	if err != nil {
		return fail(err)
	}
	if _, err := w.Write(b); err != nil {
		return fail(err)
	}
	return nil
}
