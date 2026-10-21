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
	"io"
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

// pluginExport is lazyrex-hub plugin export: it writes the pin document of the lock to w. Under
// both flocks it reads the lock and the managed file as one snapshot. It exports each GitHub
// entry of the config and of the managed file, and only when every one passes lockedDir: a machine
// never exports a pin that it cannot run itself. A lock entry without an entry is not exported,
// and a local source has no pin. A failed check writes nothing and makes no pull identity. After
// the checks, the first export makes the pull identity; it stays when the write to w then fails.
func pluginExport(cfgPath string, w io.Writer) error {
	fail := func(err error) error { return fmt.Errorf("plugin export: %w", err) }
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
		if _, _, err := lockedDir(cfg.path, cfg.state, lock, e); err != nil {
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

// The outcomes of a pull for one key (section "Pin sync", the merge).
const (
	pullUnchanged    = "unchanged"     // local equals peer; the base advances
	pullTaken        = "taken"         // the peer pin replaces the local pin
	pullKept         = "kept"          // only local changed since the base
	pullConflict     = "conflict"      // both changed; local stays, the base stays
	pullResolvedTake = "resolved-take" // a conflict that -take resolved: taken
	pullResolvedKeep = "resolved-keep" // a conflict that -keep resolved: kept, the base advances
	pullIgnored      = "ignored"       // the document has a source that this config does not have
)

// pullResult is the outcome for one source, with the local pin ("" when not installed) and the
// peer pin.
type pullResult struct {
	Source, Outcome string
	Local, Peer     basePin
}

// pullPlan is what a merge decides, before any fetch: one result for each source, the imported
// pins to fetch, and the base of the peer after a successful pull.
type pullPlan struct {
	results []pullResult
	take    map[string]pinEntry
	base    map[string]basePin
}

// mergePins merges the document doc into the lock for the GitHub sources keys of this config and
// managed file. base is the base of doc.Hub (nil when there is none). take and keep are the
// -take and -keep sources; each must name a conflict. It reads no file and runs no git.
func mergePins(keys []string, lock *pluginLock, base map[string]basePin, doc *pinDoc, take, keep []string) (*pullPlan, error) {
	takeSet, keepSet := map[string]bool{}, map[string]bool{}
	for _, s := range take {
		takeSet[s] = true
	}
	for _, s := range keep {
		if takeSet[s] {
			return nil, fmt.Errorf("%s is in both -take and -keep", s)
		}
		keepSet[s] = true
	}
	local := map[string]bool{}
	for _, k := range keys {
		local[k] = true
	}
	plan := &pullPlan{take: map[string]pinEntry{}, base: map[string]basePin{}}
	for k, v := range base {
		plan.base[k] = v
	}
	resolved := map[string]bool{}
	for _, p := range doc.Entries {
		r := pullResult{Source: p.Source, Peer: basePin{p.Commit, p.Digest}}
		if !local[p.Source] {
			r.Outcome = pullIgnored
			plan.results = append(plan.results, r)
			continue
		}
		le := lock.Plugins[p.Source]
		b, hasBase := base[p.Source]
		if le != nil {
			r.Local = basePin{le.Commit, le.Digest}
		}
		switch {
		case le == nil:
			r.Outcome = pullTaken
		case r.Local == r.Peer:
			r.Outcome = pullUnchanged
		case hasBase && r.Local == b:
			r.Outcome = pullTaken
		case hasBase && r.Peer == b:
			r.Outcome = pullKept
		case takeSet[p.Source]:
			r.Outcome = pullResolvedTake
		case keepSet[p.Source]:
			r.Outcome = pullResolvedKeep
		default:
			r.Outcome = pullConflict
		}
		switch r.Outcome {
		case pullTaken, pullResolvedTake:
			plan.take[p.Source] = p
			plan.base[p.Source] = r.Peer
		case pullUnchanged, pullResolvedKeep:
			plan.base[p.Source] = r.Peer
		}
		if r.Outcome == pullResolvedTake || r.Outcome == pullResolvedKeep {
			resolved[p.Source] = true
		}
		plan.results = append(plan.results, r)
	}
	for _, flagged := range []struct {
		name string
		set  []string
	}{{"-take", take}, {"-keep", keep}} {
		for _, s := range flagged.set {
			if !resolved[s] {
				return nil, fmt.Errorf("%s %s: the source has no conflict", flagged.name, s)
			}
		}
	}
	sort.Slice(plan.results, func(i, j int) bool { return plan.results[i].Source < plan.results[j].Source })
	return plan, nil
}

// conflicts reports whether a conflict remains.
func (p *pullPlan) conflicts() bool {
	return slices.ContainsFunc(p.results, func(r pullResult) bool { return r.Outcome == pullConflict })
}

// write writes one line for each result: <outcome>\t<source>\tlocal <commit|->\tpeer <commit>.
func (p *pullPlan) write(w io.Writer) {
	for _, r := range p.results {
		l := r.Local.Commit
		if l == "" {
			l = "-"
		}
		fmt.Fprintf(w, "%s\t%s\tlocal %s\tpeer %s\n", r.Outcome, r.Source, l, r.Peer.Commit)
	}
}
