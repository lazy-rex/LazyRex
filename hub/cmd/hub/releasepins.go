package main

// The pins of a release (docs/releasing.md): the frozen candidate, the pin state of the moved
// source, and the two pin documents. A pin moves only through plugin pull of a pin document, never
// by a copy of the lock: pull takes the plugin command flocks and merges against the recorded
// base, so a pin that another writer changed is a conflict, not an overwrite.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// relPin is the pin of one source: commit and digest.
type relPin struct {
	Commit string `json:"commit"`
	Digest string `json:"digest"`
}

// relCandidate is the frozen candidate of a run (<run>/candidate.json): what freeze read from the
// live files. Every later step compares the live files with it.
type relCandidate struct {
	Kind   string `json:"kind"`
	Source string `json:"source"` // the moved source (a hub release: the home plugin source)
	Name   string `json:"name"`   // its plugin name
	From   relPin `json:"from"`   // OLD: the pin at freeze
	To     relPin `json:"to"`     // H, or the plugin's target, with its digest
	Peer   string `json:"peer"`   // the pull identity of the pin documents (the one base peer)
	// Pins are all lock pins at freeze, as "<source> <commit> <digest>", sorted.
	Pins []string `json:"pins"`
	// Files are the sha256 of the config, the managed file, the lock and the base file at freeze,
	// keyed by role.
	Files    map[string]string `json:"files"`
	LockPins string            `json:"lockpins"` // sha256 of the lock without its cache dirs
	// Hub release only.
	OldExpectRun bool     `json:"old_expect_run"` // OLD's hub.admin.replace takes expect_run
	OldSDK       bool     `json:"old_sdk"`        // OLD can run SDK plugins (dev.sh passes -sdk, sdk/lua exists)
	Inherited    []string `json:"inherited"`      // dependencies that OLD or H brings, for example python3
}

const (
	relCandidateFile = "candidate.json"
	relFwdDoc        = "pins-fwd.json"
	relBackDoc       = "pins-back.json"
)

// The roles of the four files that the writer hold covers.
const (
	roleConfig  = "config"
	roleManaged = "managed"
	roleLock    = "lock"
	roleBase    = "base"
)

// holdFiles are the four files of the writer hold, by role.
func (r *releaser) holdFiles() map[string]string {
	return map[string]string{roleConfig: r.cfg.path, roleManaged: r.cfg.managed, roleLock: r.cfg.lock, roleBase: peersPath(r.cfg.lock)}
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sha256Bytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fileHashes returns the sha256 of each hold file. A missing file is an error: a release needs
// the config, the managed file, the lock and its base.
func (r *releaser) fileHashes() (map[string]string, error) {
	out := map[string]string{}
	for role, p := range r.holdFiles() {
		h, err := sha256File(p)
		if err != nil {
			return nil, relFail("the %s file: %v", role, err)
		}
		out[role] = h
	}
	return out, nil
}

// lockPinsHash is the sha256 of the lock without its cache dirs. A fetch can name a new cache dir
// for the same pin, so the lock bytes are not a pin comparison (release 3 lab).
func lockPinsHash(lockFile string) (string, error) {
	l, err := readLock(lockFile)
	if err != nil {
		return "", err
	}
	for _, e := range l.Plugins {
		e.Dir = ""
	}
	b, err := json.Marshal(l) // map keys are sorted
	if err != nil {
		return "", err
	}
	return sha256Bytes(b), nil
}

// lockPinLines are the pins of a lock as "<source> <commit> <digest>", sorted.
func lockPinLines(lockFile string) ([]string, error) {
	l, err := readLock(lockFile)
	if err != nil {
		return nil, err
	}
	var out []string
	for src, e := range l.Plugins {
		out = append(out, fmt.Sprintf("%s %s %s", src, e.Commit, e.Digest))
	}
	sort.Strings(out)
	return out, nil
}

// pinLabel names a pin as "from", "to" or "?".
func pinLabel(p relPin, c *relCandidate) string {
	switch p {
	case c.From:
		return "from"
	case c.To:
		return "to"
	}
	return "?"
}

// pinState is the state of the moved source: the label of its lock pin and of its base pin, for
// example "from from" (before a pull), "to to" (after it), or "to from" (a pull that stopped
// between the lock write and the base write).
func (r *releaser) pinState(c *relCandidate) (string, error) {
	l, err := readLock(r.cfg.lock)
	if err != nil {
		return "", err
	}
	base, err := readPeers(r.cfg.lock)
	if err != nil {
		return "", err
	}
	lock := "?"
	if e := l.Plugins[c.Source]; e != nil {
		lock = pinLabel(relPin{e.Commit, e.Digest}, c)
	}
	b := "?"
	if p, ok := base.Peers[c.Peer][c.Source]; ok {
		b = pinLabel(relPin{p.Commit, p.Digest}, c)
	}
	return lock + " " + b, nil
}

// pinDocs builds the forward and the back pin document of the moved source from its lock entry:
// the same fields, with the pin of To or of From.
func pinDocs(c *relCandidate, e *lockEntry) (fwd, back []byte, err error) {
	doc := func(p relPin) ([]byte, error) {
		pin := e.lockPin
		pin.Commit, pin.Digest = p.Commit, p.Digest
		return encodePinDoc(&pinDoc{Version: pinDocVersion, Hub: c.Peer, Entries: []pinEntry{{Source: c.Source, lockPin: pin}}})
	}
	if fwd, err = doc(c.To); err != nil {
		return nil, nil, err
	}
	back, err = doc(c.From)
	return fwd, back, err
}

// onePeer returns the one peer of the base file. The pin documents name it, so that pull advances
// a base. More or fewer peers: the release cannot know which base to use.
func onePeer(lockFile string) (string, error) {
	base, err := readPeers(lockFile)
	if err != nil {
		return "", err
	}
	if len(base.Peers) != 1 {
		return "", relFail("the base file %s has %d peers, not exactly one", peersPath(lockFile), len(base.Peers))
	}
	for id := range base.Peers {
		return id, nil
	}
	return "", nil
}

// readCandidate reads <run>/candidate.json.
func (r *releaser) readCandidate() (*relCandidate, error) {
	b, err := os.ReadFile(filepath.Join(r.run, relCandidateFile))
	if err != nil {
		return nil, relFail("no frozen candidate: %v (run freeze first)", err)
	}
	var c relCandidate
	if err := decodeStrict(b, &c); err != nil {
		return nil, relFail("%s: %v", relCandidateFile, err)
	}
	return &c, nil
}

// pullLine is the one result line that a pull of a pin document must print.
func pullLine(outcome string, c *relCandidate, local, peer string) string {
	return fmt.Sprintf("%s\t%s\tlocal %s\tpeer %s", outcome, c.Source, local, peer)
}

// pull runs plugin pull of a pin document with the binary bin. Its output must be exactly want:
// one line, the outcome of the moved source.
func (r *releaser) pull(bin, doc, want string) error {
	argv := []string{bin, "plugin", "pull", "-config", r.cfg.path, "-file", filepath.Join(r.run, doc)}
	o, e, err := r.runCmd(relCmd{Argv: argv})
	r.say("plugin pull %s: %s%s", doc, o, e)
	if err != nil {
		return cmdError(argv, e, err)
	}
	if got := string(o); got != want+"\n" {
		return relFail("plugin pull %s printed %q, not %q", doc, got, want)
	}
	return nil
}

// othersEqual checks that every pin and base except the moved source, the config and the managed
// file equal the backup: a release moves one source.
func (r *releaser) othersEqual(c *relCandidate, backup string) error {
	strip := func(lockFile string) (string, string, error) {
		l, err := readLock(lockFile)
		if err != nil {
			return "", "", err
		}
		delete(l.Plugins, c.Source)
		pins := map[string]relPin{}
		for s, e := range l.Plugins {
			pins[s] = relPin{e.Commit, e.Digest}
		}
		base, err := readPeers(lockFile)
		if err != nil {
			return "", "", err
		}
		for _, m := range base.Peers {
			delete(m, c.Source)
		}
		a, _ := json.Marshal(pins)
		b, _ := json.Marshal(base.Peers)
		return string(a), string(b), nil
	}
	lp, lb, err := strip(r.cfg.lock)
	if err != nil {
		return err
	}
	bp, bb, err := strip(filepath.Join(backup, "lock.json"))
	if err != nil {
		return err
	}
	switch {
	case lp != bp:
		return relFail("a pin other than %s differs from the backup", c.Source)
	case lb != bb:
		return relFail("a base other than %s differs from the backup", c.Source)
	}
	for _, f := range []struct{ role, live, saved string }{
		{roleConfig, r.cfg.path, "config.json"}, {roleManaged, r.cfg.managed, "plugins.json"}} {
		a, err := sha256File(f.live)
		if err != nil {
			return err
		}
		b, err := sha256File(filepath.Join(backup, f.saved))
		if err != nil {
			return err
		}
		if a != b {
			return relFail("the %s file differs from the backup", f.role)
		}
	}
	return nil
}
