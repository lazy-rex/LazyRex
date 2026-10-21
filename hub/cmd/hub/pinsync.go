package main

// Pin sync (docs/plugin-sources.md, "Pin sync"): the machines of the user select the same pins
// for the GitHub sources that their configs share. plugin export writes the pins of one lock as a
// pin document; plugin pull merges a document into another lock. The local lock format does not
// change: the document carries the pins (lockPin) and no path.
//
// Two sidecars live beside the lock and belong to it (two configs that name one lock share them):
//   - <lock>.id: the pull identity of this lock, the "hub" of the documents it exports;
//   - <lock>.peers.json: for each peer identity, the pins of the last pull (the merge base).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// pinDocVersion is the only pin document version this hub reads and writes. Both ends are hubs
// of the user, which the user updates; another version is an error, not a format to tolerate.
const pinDocVersion = 1

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
// final newline. The same pins and identity give the same bytes.
func encodePinDoc(d *pinDoc) ([]byte, error) {
	sorted := *d
	sorted.Entries = append([]pinEntry{}, d.Entries...) // never null: an empty document has []
	sort.Slice(sorted.Entries, func(i, j int) bool { return sorted.Entries[i].Source < sorted.Entries[j].Source })
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
	case *top.Version != pinDocVersion:
		return nil, fmt.Errorf("pin document version %d is not supported by this hub (it reads version %d)", *top.Version, pinDocVersion)
	case !pullID.MatchString(top.Hub):
		return nil, errors.New("hub must be 32 lowercase hex")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(top.Entries, &raws); err != nil || raws == nil {
		return nil, errors.New("entries must be an array")
	}
	d := &pinDoc{Version: pinDocVersion, Hub: top.Hub, Entries: []pinEntry{}}
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
		d.Entries = append(d.Entries, e)
	}
	return d, nil
}

// check checks one document entry against its source string: a GitHub source whose owner, repo,
// subdir and ref are the entry's; a ref_kind that the ref form allows; a commit of 40 lowercase hex
// that equals a commit ref; and the name and digest forms.
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
	case !pluginName.MatchString(e.Name):
		return errors.New("name is not a plugin name")
	case !lockDigest.MatchString(e.Digest):
		return errors.New("digest is not sha256:<64 lowercase hex>")
	}
	return nil
}

// idPath and peersPath are the sidecars of the lock file lockFile.
func idPath(lockFile string) string    { return lockFile + ".id" }
func peersPath(lockFile string) string { return lockFile + ".peers.json" }

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
		return "", sidecarInvalid(p, "remove it to make a new pull identity (peers then have no base for this lock)", errors.New("not 32 lowercase hex and a newline"))
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

// peerBase is the base file: for each peer identity, the pins of the last pull from it.
type peerBase struct {
	Version int                           `json:"version"`
	Peers   map[string]map[string]basePin `json:"peers"`
}

// basePin is one base value: the commit and the digest of a pin.
type basePin struct {
	Commit string `json:"commit"`
	Digest string `json:"digest"`
}

const peerBaseFix = "repair it, or remove it (the next pull then reports each differing key as a conflict)"

// readPeers reads the base file of a lock. A missing file gives an empty base.
func readPeers(lockFile string) (*peerBase, error) {
	p := peersPath(lockFile)
	empty := &peerBase{Version: pinDocVersion, Peers: map[string]map[string]basePin{}}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return nil, err
	}
	var top struct {
		Version *int                          `json:"version"`
		Peers   map[string]map[string]basePin `json:"peers"`
	}
	if err := decodeStrict(b, &top); err != nil {
		return nil, sidecarInvalid(p, peerBaseFix, err)
	}
	switch {
	case top.Version == nil:
		return nil, sidecarInvalid(p, peerBaseFix, errors.New("version is missing"))
	case *top.Version != pinDocVersion:
		return nil, sidecarInvalid(p, peerBaseFix, fmt.Errorf("version %d is not supported", *top.Version))
	case top.Peers == nil:
		return nil, sidecarInvalid(p, peerBaseFix, errors.New("peers must be an object"))
	}
	for id, pins := range top.Peers {
		if !pullID.MatchString(id) || pins == nil {
			return nil, sidecarInvalid(p, peerBaseFix, fmt.Errorf("peer %q: not a pull identity with an object", id))
		}
		for src, pin := range pins {
			if !commitID.MatchString(pin.Commit) || !lockDigest.MatchString(pin.Digest) {
				return nil, sidecarInvalid(p, peerBaseFix, fmt.Errorf("peer %s, %s: commit or digest of the wrong form", id, src))
			}
		}
	}
	empty.Peers = top.Peers
	return empty, nil
}

// writePeers replaces the base file as writeLock replaces the lock: a temporary file, fsync,
// rename, and fsync of the directory. The caller holds both plugin command flocks.
func writePeers(lockFile string, base *peerBase) error {
	b, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		return err
	}
	return replaceFile(peersPath(lockFile), append(b, '\n'))
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
	return syncDir(dir)
}

// syncDir makes a directory entry change durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
