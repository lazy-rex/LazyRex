package main

// The steps of a hub release (docs/releasing.md): freeze, precheck, stop, backup, checkout, pull,
// start, replace, verify; rollback; resume. The home plugin pin moves from OLD to H through plugin
// pull; every other pin stays.
//
// Each step reads the real state first. A rerun after an interruption either reaches the state of
// an uninterrupted run, or stops before any mutation when the state is an unresolved outcome. No
// step copies a file back, and hub.db is never restored.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	relHubFile    = "hub-H.json"   // the H hub that step start saw
	relOldHubFile = "hub-OLD.json" // the OLD hub that rollback started
)

// relClone is the release clone of LazyRex.
func (r *releaser) relClone() string { return filepath.Join(r.spec.ReleaseDir, "LazyRex") }

// git runs git in the release clone and returns its trimmed stdout.
func (r *releaser) git(args ...string) (string, error) {
	argv := append([]string{"git", "-C", r.relClone()}, args...)
	o, e, err := r.runCmd(relCmd{Argv: argv})
	if err != nil {
		return "", cmdError(argv, e, err)
	}
	return strings.TrimSpace(string(o)), nil
}

// head returns HEAD of the release clone, and fails when the clone has local changes.
func (r *releaser) head() (string, error) {
	h, err := r.git("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	st, err := r.git("status", "--porcelain")
	if err != nil {
		return "", err
	}
	if st != "" {
		return "", relFail("the release clone %s has local changes", r.relClone())
	}
	return h, nil
}

func (r *releaser) needHead(want, label string) error {
	h, err := r.head()
	if err != nil {
		return err
	}
	if h != want {
		return relFail("the release clone is at %s, not at %s %s", h, label, want)
	}
	return nil
}

// treeHas reports the type of <commit>:<path> ("blob", "tree"), "" when it does not exist.
func (r *releaser) treeHas(commit, path string) string {
	t, err := r.git("cat-file", "-t", commit+":"+path)
	if err != nil {
		return ""
	}
	return t
}

// fileAt returns the content of <commit>:<path>.
func (r *releaser) fileAt(commit, path string) (string, error) {
	return r.git("show", commit+":"+path)
}

// homeEntry finds the lock entry of the home plugin: github lazy-rex/LazyRex, subdir
// plugins/lazyrex. Exactly one must exist.
func homeEntry(l *pluginLock) (string, *lockEntry, error) {
	var src string
	var found *lockEntry
	for s, e := range l.Plugins {
		if e.Owner == "lazy-rex" && e.Repo == "LazyRex" && e.Subdir == "plugins/lazyrex" {
			if found != nil {
				return "", nil, relFail("the lock has two home plugin entries: %s and %s", src, s)
			}
			src, found = s, e
		}
	}
	if found == nil {
		return "", nil, relFail("the lock has no home plugin entry (github:lazy-rex/LazyRex/plugins/lazyrex)")
	}
	return src, found, nil
}

// verifyLists writes the verify lists: each entry of the config and the managed file, with its
// lock commit; the forward list has to for the moved source. Every entry must be a GitHub source
// with a lock entry: the release tool does not release a hub with local plugin sources.
func (r *releaser) verifyLists(c *relCandidate, l *pluginLock) error {
	managed, err := parseManaged(r.cfg.managed)
	if err != nil {
		return relFail("the managed file: %v", err)
	}
	var back, fwd []relVerifyEntry
	for _, e := range addManaged(r.cfg.config, managed) {
		le := l.Plugins[e.Source]
		if !e.github || le == nil {
			return relFail("%s is not a GitHub source with a lock entry; the release tool releases only locked sources", e.Source)
		}
		v := relVerifyEntry{Name: le.Name, Source: e.Source, Commit: le.Commit}
		back = append(back, v)
		if e.Source == c.Source {
			v.Commit = c.To.Commit
		}
		fwd = append(fwd, v)
	}
	for _, f := range []struct {
		name string
		list []relVerifyEntry
	}{{relVerifyFwd, fwd}, {relVerifyBack, back}} {
		sort.Slice(f.list, func(i, j int) bool { return f.list[i].Name < f.list[j].Name })
		b, _ := json.MarshalIndent(f.list, "", "  ")
		if err := os.WriteFile(filepath.Join(r.run, f.name), append(b, '\n'), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// digestOf computes the digest of a commit-form source by a plugin sync in a scratch state under
// the run directory. Tests replace it (no network).
func (r *releaser) digestOf(source string) (string, error) {
	if r.syncDigest != nil {
		return r.syncDigest(source)
	}
	dir := filepath.Join(r.run, "digest")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	cfg := filepath.Join(dir, "config.json")
	b, _ := json.Marshal(map[string]any{"state": filepath.Join(dir, "state"), "plugins": []any{map[string]string{"source": source}}})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		return "", err
	}
	if err := runPluginCmd(r.ctx, "sync", cfg, nil, pluginCmdOpts{}, io.Discard); err != nil {
		return "", relFail("the digest of %s: %v", source, err)
	}
	l, err := readLock(lockPath(cfg, ""))
	if err != nil {
		return "", err
	}
	if e := l.Plugins[source]; e != nil {
		return e.Digest, nil
	}
	return "", relFail("the digest of %s: no lock entry after the sync", source)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return replaceFile(path, append(b, '\n'))
}

// hubFreeze reads the live files and the release clone, read only except for a git fetch, and
// writes the frozen candidate, the pin documents and the verify lists.
func hubFreeze(r *releaser) error {
	old, err := r.head()
	if err != nil {
		return err
	}
	h := r.spec.H
	if h == old {
		return relFail("H %s is OLD, the commit of the release clone: a release without a change is not supported", h)
	}
	if _, err := r.git("fetch", "-q", "origin"); err != nil {
		return err
	}
	if _, err := r.git("merge-base", "--is-ancestor", h, "origin/main"); err != nil {
		return relFail("H %s is not on origin/main", h)
	}
	if r.treeHas(old, "hub/cmd/hub/main.go") != "blob" {
		return relFail("the OLD tree %s has no hub/cmd/hub/main.go", old)
	}
	// The capability probe of OLD: git grep exits 1 for no match, and also for an unreadable blob
	// (with an error text). Any error text stops: a failure is never read as a missing capability.
	argv := []string{"git", "-C", r.relClone(), "grep", "-q", "-F", `"expect_run"`, old, "--", "hub/cmd/hub"}
	_, e, gerr := r.runCmd(relCmd{Argv: argv})
	var oldExpect bool
	switch {
	case gerr == nil:
		oldExpect = true
	case len(strings.TrimSpace(string(e))) > 0:
		return relFail("git grep in the OLD tree failed: %s", strings.TrimSpace(string(e)))
	case exitCode(gerr) == 1:
		oldExpect = false
	default:
		return relFail("git grep in the OLD tree failed: %v", gerr)
	}
	c := &relCandidate{Kind: "hub", OldExpectRun: oldExpect}
	for _, commit := range []string{old, h} {
		dev, err := r.fileAt(commit, "dev.sh")
		if err != nil {
			return relFail("dev.sh at %s: %v", commit, err)
		}
		if strings.Contains(dev, "python3") {
			c.Inherited = append(c.Inherited, "python3 (dev.sh at "+commit+")")
		}
		if commit == old {
			c.OldSDK = strings.Contains(dev, "-sdk") && r.treeHas(old, "sdk/lua") == "tree"
		}
	}
	l, err := readLock(r.cfg.lock)
	if err != nil {
		return err
	}
	src, le, err := homeEntry(l)
	if err != nil {
		return err
	}
	if le.Commit != old {
		return relFail("the lock pins the home plugin at %s, not at OLD %s", le.Commit, old)
	}
	if c.Peer, err = onePeer(r.cfg.lock); err != nil {
		return err
	}
	base, err := readPeers(r.cfg.lock)
	if err != nil {
		return err
	}
	if b := base.Peers[c.Peer][src]; b.Commit != le.Commit || b.Digest != le.Digest {
		return relFail("the base of the home plugin is not its lock pin")
	}
	c.Source, c.Name, c.From = src, le.Name, relPin{le.Commit, le.Digest}
	cs := strings.SplitN(src, "@", 2)[0] + "@" + h
	d, err := r.digestOf(cs)
	if err != nil {
		return err
	}
	c.To = relPin{h, d}
	if c.Pins, err = lockPinLines(r.cfg.lock); err != nil {
		return err
	}
	if c.Files, err = r.fileHashes(); err != nil {
		return err
	}
	if c.LockPins, err = lockPinsHash(r.cfg.lock); err != nil {
		return err
	}
	fwd, back, err := pinDocs(c, le)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.run, relFwdDoc), fwd, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.run, relBackDoc), back, 0o600); err != nil {
		return err
	}
	if err := r.verifyLists(c, l); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(r.run, relCandidateFile), c); err != nil {
		return err
	}
	r.say("frozen: OLD %s, H %s, home %s (%s -> %s), old_expect_run %v, old_sdk %v, inherited %v", old, h, src, c.From.Digest, c.To.Digest, c.OldExpectRun, c.OldSDK, c.Inherited)
	return nil
}

// exitCode is the exit status of a failed command, -1 when it did not exit.
func exitCode(err error) int {
	var x interface{ ExitCode() int }
	if errors.As(err, &x) {
		return x.ExitCode()
	}
	return -1
}

// holdOK checks the writer hold before the pull: every hold file and every lock pin equal the
// frozen candidate, and the home pin state is "from from".
func (r *releaser) holdOK(c *relCandidate) error {
	pins, err := lockPinLines(r.cfg.lock)
	if err != nil {
		return err
	}
	if strings.Join(pins, "\n") != strings.Join(c.Pins, "\n") {
		return relFail("the lock pins differ from the frozen pins (another writer?)")
	}
	files, err := r.fileHashes()
	if err != nil {
		return err
	}
	// The lock bytes are not compared: a fetch can name a new cache dir for the same pin.
	// lockPinsHash below compares the lock without its cache dirs.
	for role, h := range c.Files {
		if role != roleLock && files[role] != h {
			return relFail("the %s file differs from its frozen hash (another writer?)", role)
		}
	}
	lp, err := lockPinsHash(r.cfg.lock)
	if err != nil {
		return err
	}
	if lp != c.LockPins {
		return relFail("the lock without its cache dirs differs from its frozen hash")
	}
	if st, err := r.pinState(c); err != nil || st != "from from" {
		return relFail("the home pin state is %q, not \"from from\" (%v)", st, err)
	}
	return nil
}

// holdBackup checks the writer hold after the backup, before a mutation that depends on it: the
// home pin state is one of states, and every other pin and base, the config and the managed file
// equal the backup.
func (r *releaser) holdBackup(c *relCandidate, states ...string) error {
	b, err := r.currentBackup()
	if err != nil {
		return err
	}
	st, err := r.pinState(c)
	if err != nil {
		return err
	}
	if !slices.Contains(states, st) {
		return relFail("the home pin state is %q, not one of %q; stop and report", st, states)
	}
	return r.othersEqual(c, b)
}

// holdAfterPull checks the writer hold after the pull: the home pin and base are at to, and
// everything else equals the backup.
func (r *releaser) holdAfterPull(c *relCandidate) error {
	if st, err := r.pinState(c); err != nil || st != "to to" {
		return relFail("the home pin state is %q, not \"to to\" (%v)", st, err)
	}
	b, err := r.currentBackup()
	if err != nil {
		return err
	}
	return r.othersEqual(c, b)
}

// currentBackup is the backup of this run's stamp.
func (r *releaser) currentBackup() (string, error) {
	s, err := r.stamp()
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", relFail("no backup stamp in %s (run backup first)", r.run)
	}
	return r.backupPath(s), nil
}

// fetchRole: LAZYREX_FETCH_ROLE (the fetch child role of the hub) must be unset in this process,
// in a login zsh (the shell of the dev pane), and dev.sh must not set it.
func (r *releaser) fetchRole() error {
	for _, kv := range r.env {
		if strings.HasPrefix(kv, "LAZYREX_FETCH_ROLE=") {
			return relFail("LAZYREX_FETCH_ROLE is set in this shell")
		}
	}
	o, _, _ := r.runCmd(relCmd{Argv: []string{"/bin/zsh", "-lc", "printenv LAZYREX_FETCH_ROLE"}})
	if strings.TrimSpace(string(o)) != "" {
		return relFail("a login zsh sets LAZYREX_FETCH_ROLE")
	}
	b, err := os.ReadFile(filepath.Join(r.relClone(), "dev.sh"))
	if err != nil {
		return err
	}
	if strings.Contains(string(b), "LAZYREX_FETCH_ROLE") {
		return relFail("dev.sh names LAZYREX_FETCH_ROLE")
	}
	return nil
}

// toolPath is the binary of this tool, for the rehearsal gate.
func (r *releaser) toolPath() (string, error) {
	if r.tool != "" {
		return r.tool, nil
	}
	return os.Executable()
}

// sdkPlugins lists the configured plugins whose manifest (in their locked cache entry) has an sdk
// field. A hub that cannot run SDK plugins refuses them, so a rollback to it must first roll back
// each of them.
func (r *releaser) sdkPlugins() ([]string, error) {
	l, err := readLock(r.cfg.lock)
	if err != nil {
		return nil, err
	}
	var out []string
	for src, e := range l.Plugins {
		b, err := readManifestBytes(filepath.Join(r.cfg.state, "plugins", e.Dir, e.Subdir))
		if err != nil {
			return nil, relFail("the manifest of %s: %v", src, err)
		}
		var m struct {
			SDK string `json:"sdk"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, relFail("the manifest of %s: %v", src, err)
		}
		if m.SDK != "" {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// sdkBoundary refuses a hub rollback (and its precheck) to an OLD that cannot run SDK plugins while
// a configured plugin uses the SDK.
func (r *releaser) sdkBoundary(c *relCandidate) error {
	if c.OldSDK {
		return nil
	}
	sdk, err := r.sdkPlugins()
	if err != nil {
		return err
	}
	if len(sdk) == 0 {
		return nil
	}
	return relFail("OLD cannot run SDK plugins, and these plugins use the SDK: %s. A rollback to OLD first needs a plugin rollback of each of them, newest release first (hub.admin.stop; hub.admin.sql_return; the pin back; hub.admin.start), then the hub rollback",
		strings.Join(sdk, ", "))
}

func hubPrecheck(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	if err := r.needHead(c.From.Commit, "OLD"); err != nil {
		return err
	}
	if err := r.holdOK(c); err != nil {
		return err
	}
	// The rehearsal gate binds a live run, after the hold check (its refusal names the frozen file).
	// A lab run is the rehearsal: it has no record.
	if !r.spec.Lab {
		tool, err := r.toolPath()
		if err != nil {
			return err
		}
		if err := r.checkRehearsal(tool); err != nil {
			return err
		}
	}
	panes, err := r.devPanes()
	if err != nil {
		return err
	}
	if len(panes) != 1 {
		return relFail("expected one %s pane, found %d", devPaneLabel, len(panes))
	}
	if err := r.fetchRole(); err != nil {
		return err
	}
	if s, _ := r.stamp(); s != "" {
		return relFail("a backup stamp exists (%s): this run already passed step backup", s)
	}
	for _, d := range c.Inherited {
		name := strings.Fields(d)[0]
		if r.pathDir(name) == "" {
			return relFail("%s is needed (inherited: %s) and is not on PATH", name, d)
		}
	}
	if _, err := r.goBinDir(); err != nil {
		return err
	}
	if err := r.sdkBoundary(c); err != nil {
		return err
	}
	r.say("OK: OLD checked out, every pin and file as frozen, one dev pane, rehearsal record matches")
	return nil
}

func hubStop(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	if err := r.needHead(c.From.Commit, "OLD"); err != nil {
		return err
	}
	if err := r.holdOK(c); err != nil {
		return err
	}
	return r.closeDev("dev-close", true)
}

func hubBackup(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	if s, err := r.stamp(); err != nil {
		return err
	} else if s != "" {
		if err := r.checkBackup(r.backupPath(s)); err != nil {
			return err
		}
		r.say("the backup %s exists and is intact", r.backupPath(s))
		return nil
	}
	// A hub release backs up with the hub stopped; a plugin release with the hub running (hub.db by
	// the SQLite online backup).
	if r.spec.Kind == "hub" {
		panes, err := r.devPanes()
		if err != nil {
			return err
		}
		if len(panes) != 0 {
			return relFail("a %s pane runs; run step stop first", devPaneLabel)
		}
	}
	if err := r.holdOK(c); err != nil {
		return err
	}
	if err := r.checkAbsent(); err != nil {
		return err
	}
	// A backup directory without a stamp is incomplete (an interrupted backup): set it aside.
	dirs, _ := filepath.Glob(filepath.Join(r.run, "backup-*"))
	for _, d := range dirs {
		if strings.Contains(filepath.Base(d), ".partial") {
			continue
		}
		if err := os.Rename(d, d+".partial"); err != nil {
			return err
		}
		r.say("an incomplete backup is set aside: %s.partial", d)
	}
	stamp := fmt.Sprintf("%s-%d", r.now().UTC().Format("20060102T150405Z"), os.Getpid())
	var b string
	if err := r.mutate("backup-write", map[string]string{"stamp": stamp}, func() (any, error) {
		var err error
		b, err = r.writeBackup(stamp)
		return map[string]string{"backup": b}, err
	}); err != nil {
		return err
	}
	if err := r.mutate("stamp", map[string]string{"stamp": stamp}, func() (any, error) { return nil, r.setStamp(stamp) }); err != nil {
		return err
	}
	r.say("backup %s (config, managed file, lock, base, hub.db, sql listing, SHA256SUMS)", b)
	return nil
}

func hubCheckout(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	if _, err := r.currentBackup(); err != nil {
		return err
	}
	panes, err := r.devPanes()
	if err != nil {
		return err
	}
	if len(panes) != 0 {
		return relFail("a %s pane runs", devPaneLabel)
	}
	h := c.To.Commit
	if cur, err := r.head(); err != nil {
		return err
	} else if cur == h {
		r.say("the release clone is at H %s", h)
		return nil
	} else if cur != c.From.Commit {
		return relFail("the release clone is at %s, neither OLD nor H", cur)
	}
	if err := r.holdBackup(c, "from from"); err != nil {
		return err
	}
	if _, err := r.git("fetch", "-q", "origin"); err != nil {
		return err
	}
	dev, err := r.fileAt(h, "dev.sh")
	if err != nil {
		return err
	}
	if strings.Contains(dev, "-sdk") && r.treeHas(h, "sdk/lua") != "tree" {
		return relFail("dev.sh at H passes -sdk, and H has no sdk/lua; nothing is changed")
	}
	if err := r.mutate("checkout", map[string]string{"commit": h}, func() (any, error) {
		_, err := r.git("checkout", "-q", "--detach", h)
		return nil, err
	}); err != nil {
		return err
	}
	if err := r.needHead(h, "H"); err != nil {
		return err
	}
	r.say("the release clone is at H %s", h)
	return nil
}

// hubBin is the H binary that step pull builds.
func (r *releaser) hubBin() string { return filepath.Join(r.run, "bin", "hub-H") }

// statusOK runs plugin status with bin: exit 0, and one "cache ok" line for each lock entry.
func (r *releaser) statusOK(bin string) error {
	argv := []string{bin, "plugin", "status", "-config", r.cfg.path}
	o, e, err := r.runCmd(relCmd{Argv: argv})
	r.say("plugin status:\n%s", o)
	if err != nil {
		return cmdError(argv, e, err)
	}
	l, err := readLock(r.cfg.lock)
	if err != nil {
		return err
	}
	if n := strings.Count(string(o), "\tcache ok\t"); n != len(l.Plugins) {
		return relFail("plugin status: %d entries are cache ok, not %d", n, len(l.Plugins))
	}
	return nil
}

func hubPull(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	if err := r.needHead(c.To.Commit, "H"); err != nil {
		return err
	}
	if err := r.holdBackup(c, "from from", "to to", "to from"); err != nil {
		return err
	}
	st, err := r.pinState(c)
	if err != nil {
		return err
	}
	if err := r.buildHubH(c, st); err != nil {
		return err
	}
	// The build can take minutes: check the hold again right before the pull.
	if err := r.holdBackup(c, st); err != nil {
		return err
	}
	switch st {
	case "from from":
		if err := r.mutate("pull-fwd", map[string]string{"doc": relFwdDoc}, func() (any, error) {
			return nil, r.pull(r.hubBin(), relFwdDoc, pullLine("taken", c, c.From.Commit, c.To.Commit))
		}); err != nil {
			return err
		}
	case "to from":
		// A forward pull that stopped after the lock write: the same document again advances the base.
		r.say("the lock is at H and the base at OLD (a pull stopped after the lock write)")
		if err := r.mutate("pull-fwd", map[string]string{"doc": relFwdDoc}, func() (any, error) {
			return nil, r.pull(r.hubBin(), relFwdDoc, pullLine("unchanged", c, c.To.Commit, c.To.Commit))
		}); err != nil {
			return err
		}
	case "to to":
		r.say("the home pin is already at H")
	}
	if err := r.holdAfterPull(c); err != nil {
		return err
	}
	return r.statusOK(r.hubBin())
}

// buildHubH builds the H binary first: a failed module download or build stops before any pin
// moves. The build goes to hub-H.new, and replaces hub-H only after its provenance check. In pin
// state "from from" no pin moved, so a prior H binary is removed first: a failed build then leaves
// no binary. After a pin moved, the verified H binary stays for the rollback pull when a retry fails.
func (r *releaser) buildHubH(c *relCandidate, st string) error {
	if err := os.MkdirAll(filepath.Dir(r.hubBin()), 0o700); err != nil {
		return err
	}
	if st == "from from" {
		if err := os.Remove(r.hubBin()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	next := r.hubBin() + ".new"
	if err := os.Remove(next); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	hubDir := filepath.Join(r.relClone(), "hub")
	for _, argv := range [][]string{{"go", "mod", "download"}, {"go", "build", "-o", next, "./cmd/hub"}} {
		if _, e, err := r.runCmd(relCmd{Dir: hubDir, Argv: argv}); err != nil {
			return cmdError(argv, e, err)
		}
	}
	if err := r.checkProvenance(next, c.To.Commit, "hub-H"); err != nil {
		return err
	}
	return os.Rename(next, r.hubBin())
}

// runtime checks that the hub h runs <state>/dev/hub, a clean build of commit.
func (r *releaser) runtime(h *relHub, commit, label string) error {
	devHub := filepath.Join(r.cfg.state, "dev", "hub")
	if exe := r.procExe(h.PID); exe != devHub {
		return relFail("the hub pid %d runs %q, not %s", h.PID, exe, devHub)
	}
	return r.checkProvenance(devHub, commit, label)
}

// startHub starts the dev pane of the checked-out commit, and checks the runtime of its hub. The
// caller settles a pending create first (settleCreate): startHub refuses while one is pending, and
// while a dev pane runs. A begin record without a "sent" record (a stop before the send) is not
// pending: the create never went out, and startHub creates the pane.
func (r *releaser) startHub(commit, label string) (*relHub, error) {
	op := "dev-start-" + label
	if p, _, err := r.pendingCreate(); err != nil {
		return nil, err
	} else if p != "" {
		return nil, relFail("the dev pane create of %s is pending", p)
	}
	panes, err := r.devPanes()
	if err != nil {
		return nil, err
	}
	if len(panes) > 0 {
		return nil, relFail("a %s pane runs without a new hub of this step; stop it, or roll back", devPaneLabel)
	}
	last, _, err := hubEvents(r.cfg.state)
	if err != nil {
		return nil, err
	}
	var h *relHub
	if err := r.mutate(op, last, func() (any, error) {
		var err error
		h, err = r.devStart(op, filepath.Join(r.relClone(), "dev.sh"))
		return h, err
	}); err != nil {
		return nil, err
	}
	return h, r.runtime(h, commit, label)
}

func hubStart(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	op, sh, err := r.settleCreate(c)
	if err != nil {
		return err
	}
	if op == "dev-start-forward" {
		if sh == nil {
			return relFail("the dev pane of this step runs no new hub; see %s, then roll back", r.spec.DevLog)
		}
		return r.saveHubH(sh)
	}
	if err := r.needHead(c.To.Commit, "H"); err != nil {
		return err
	}
	if err := r.fetchRole(); err != nil {
		return err
	}
	if err := r.holdAfterPull(c); err != nil {
		return err
	}
	h, err := r.startHub(c.To.Commit, "forward")
	if err != nil {
		return err
	}
	return r.saveHubH(h)
}

// saveHubH writes the H hub of step start for replace and rollback.
func (r *releaser) saveHubH(h *relHub) error {
	if err := writeJSON(filepath.Join(r.run, relHubFile), h); err != nil {
		return err
	}
	r.say("the H hub: pid %d, %s", h.PID, h.Client)
	return nil
}

// relExpectRun is the expect_run of a pin of the moved source.
func relExpectRun(c *relCandidate, p relPin) map[string]string {
	return map[string]string{"source": c.Source, "commit": p.Commit, "digest": p.Digest}
}

// relReplaceSaved is the begin record of a replace: the expectation that a rerun reuses.
type relReplaceSaved struct {
	ExpectInstance string            `json:"expect_instance"`
	ExpectRun      map[string]string `json:"expect_run,omitempty"`
}

// replacePlugin makes the plugin of c run the pin to on the hub client, from the pin from.
// expectRun false is the form of a hub without expect_run (the commit is then the only
// check of a run). A pending intent always stops: do not retry.
func (r *releaser) replacePlugin(c *relCandidate, client string, from, to relPin, expectRun bool) error {
	op := "replace-" + to.Commit[:12]
	var e *relEntryState
	for i := 0; ; i++ {
		entries, _, err := r.hubPlugins(client)
		if err != nil {
			return err
		}
		if e = entryOf(entries, c.Name); e == nil {
			return relFail("hub.plugins has no entry %s", c.Name)
		}
		if e.Source != c.Source {
			return relFail("the %s entry has source %s, not %s", c.Name, e.Source, c.Source)
		}
		// The target that runs, but is not registered yet: wait for it (the verify deadline).
		if e.Commit == to.Commit && e.State == "running" && !e.InstancePending && (e.Registered == nil || !*e.Registered) && i < 60 {
			r.pause(2 * time.Second)
			continue
		}
		break
	}
	if e.InstancePending {
		return relFail("plugin %s has an unresolved spawn intent; do not retry: wait for the reconciliation, or report", c.Name)
	}
	xrun := relExpectRun(c, to)
	if e.Commit == to.Commit && e.State == "running" && e.Registered != nil && *e.Registered {
		if !expectRun {
			r.say("%s already runs %s", c.Name, to.Commit)
			return nil
		}
		// The hub checks the full run (source, commit, digest). It changes nothing when the run
		// matches; a mismatch is candidate_changed.
		return r.mutate("start-expected", map[string]any{"plugin": c.Name, "expect_run": xrun}, func() (any, error) {
			out, err := r.hubDo(client, "hub.admin.start", map[string]any{"plugin": c.Name, "expect_run": xrun})
			if err == nil {
				r.say("%s already runs %s (expect_run accepted: %s)", c.Name, to.Commit, strings.TrimSpace(string(out)))
			}
			return json.RawMessage(nonEmptyJSON(out)), err
		})
	}
	data, open, err := r.lastBegin(op)
	if err != nil {
		return err
	}
	call := func(saved relReplaceSaved) error {
		return r.mutate(op, saved, func() (any, error) {
			args := map[string]any{"plugin": c.Name, "expect_instance": saved.ExpectInstance}
			if saved.ExpectRun != nil {
				args["expect_run"] = saved.ExpectRun
			}
			out, err := r.hubDo(client, "hub.admin.replace", args)
			if err == nil {
				r.say("hub.admin.replace -> %s", strings.TrimSpace(string(out)))
			}
			return json.RawMessage(nonEmptyJSON(out)), err
		})
	}
	if open {
		var saved relReplaceSaved
		if err := json.Unmarshal(data, &saved); err != nil {
			return relFail("the saved replace expectation: %v", err)
		}
		switch {
		case e.InstanceID == saved.ExpectInstance && e.Commit == from.Commit:
			r.say("a replace of this run was interrupted; the hub still has instance %s: replace again with it", saved.ExpectInstance)
			return call(saved)
		case e.InstanceID == "" && e.Desired == "running" && saved.ExpectRun != nil:
			r.say("the old instance is retired and nothing runs (spawn_failed): start with the saved expect_run")
			return r.mutate("start-"+to.Commit[:12], saved, func() (any, error) {
				out, err := r.hubDo(client, "hub.admin.start", map[string]any{"plugin": c.Name, "expect_run": saved.ExpectRun})
				return json.RawMessage(nonEmptyJSON(out)), err
			})
		default:
			return relFail("an interrupted replace left %s at %q (instance %q, state %s): not a state to continue; report", c.Name, e.Commit, e.InstanceID, e.State)
		}
	}
	if e.Commit != from.Commit || e.InstanceID == "" {
		return relFail("%s runs %q (instance %q, state %s), not %s", c.Name, e.Commit, e.InstanceID, e.State, from.Commit)
	}
	saved := relReplaceSaved{ExpectInstance: e.InstanceID}
	if expectRun {
		saved.ExpectRun = xrun
	}
	return call(saved)
}

func nonEmptyJSON(b []byte) []byte {
	if len(strings.TrimSpace(string(b))) == 0 || !json.Valid(b) {
		return []byte("null")
	}
	return b
}

func hubReplace(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	if err := r.holdAfterPull(c); err != nil {
		return err
	}
	h, err := r.currentHub()
	if err != nil {
		return err
	}
	return r.replacePlugin(c, h.Client, c.From, c.To, true)
}

func hubVerify(r *releaser) error {
	return r.verifyList(relVerifyFwd)
}

// readHub reads a hub that a step recorded in the run file name, nil without one.
func (r *releaser) readHub(name string) *relHub {
	b, err := os.ReadFile(filepath.Join(r.run, name))
	if err != nil {
		return nil
	}
	var h relHub
	if json.Unmarshal(b, &h) != nil {
		return nil
	}
	return &h
}

func hubRollback(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	// 0. A pending create first: before a pin, clone or pane change, and before another create.
	op, sh, err := r.settleCreate(c)
	if err != nil {
		return err
	}
	switch {
	case op == "dev-start-forward" && sh != nil:
		if err := r.saveHubH(sh); err != nil {
			return err
		}
	case op == "dev-start-rollback" && sh != nil:
		if err := writeJSON(filepath.Join(r.run, relOldHubFile), sh); err != nil {
			return err
		}
	}
	b, err := r.currentBackup()
	if err != nil {
		return err
	}
	if err := r.checkBackup(b); err != nil {
		return err
	}
	if err := r.sdkBoundary(c); err != nil {
		return err
	}
	if err := r.othersEqual(c, b); err != nil {
		return err
	}
	// A rerun after this rollback started OLD: the OLD hub runs, the pins are back and the clone is
	// at OLD. Do not close it; continue at step 4.
	if h := r.readHub(relOldHubFile); r.running(h) {
		if err := r.holdBackup(c, "from from"); err != nil {
			return err
		}
		if err := r.needHead(c.From.Commit, "OLD"); err != nil {
			return err
		}
		r.say("the OLD hub of this rollback runs (pid %d, %s)", h.PID, h.Client)
		return r.rollbackFinish(c, h)
	}
	// 1. The pins, through plugin pull of the back document (the base advances with it). A pull
	// writes the lock, then the base: a stop between the two leaves the lock ahead of the base.
	st, err := r.pinState(c)
	if err != nil {
		return err
	}
	needBin := func() error {
		if _, err := os.Stat(r.hubBin()); err != nil {
			return relFail("the pin state %q needs a pull, and there is no H binary %s", st, r.hubBin())
		}
		return nil
	}
	switch st {
	case "from from":
		r.say("the home pin is OLD; no pull")
	case "to to":
		if err := needBin(); err != nil {
			return err
		}
		if err := r.mutate("pull-back", map[string]string{"doc": relBackDoc}, func() (any, error) {
			return nil, r.pull(r.hubBin(), relBackDoc, pullLine("taken", c, c.To.Commit, c.From.Commit))
		}); err != nil {
			return err
		}
	case "to from":
		if err := needBin(); err != nil {
			return err
		}
		r.say("the lock is at H and the base at OLD (the forward pull stopped after the lock write)")
		if err := r.mutate("pull-fwd", map[string]string{"doc": relFwdDoc}, func() (any, error) {
			return nil, r.pull(r.hubBin(), relFwdDoc, pullLine("unchanged", c, c.To.Commit, c.To.Commit))
		}); err != nil {
			return err
		}
		if err := r.mutate("pull-back", map[string]string{"doc": relBackDoc}, func() (any, error) {
			return nil, r.pull(r.hubBin(), relBackDoc, pullLine("taken", c, c.To.Commit, c.From.Commit))
		}); err != nil {
			return err
		}
	case "from to":
		if err := needBin(); err != nil {
			return err
		}
		r.say("the lock is at OLD and the base at H (a rollback pull stopped after the lock write)")
		if err := r.mutate("pull-back", map[string]string{"doc": relBackDoc}, func() (any, error) {
			return nil, r.pull(r.hubBin(), relBackDoc, pullLine("unchanged", c, c.From.Commit, c.From.Commit))
		}); err != nil {
			return err
		}
	default:
		return relFail("the home pin state is %q: not a state of this release; stop and report", st)
	}
	if st, err := r.pinState(c); err != nil || st != "from from" {
		return relFail("after the pull, the home pin state is %q (%v)", st, err)
	}
	if err := r.othersEqual(c, b); err != nil {
		return err
	}
	// 2. Only when the H hub of step start still runs (pid, start time, last hub_started): it has
	// expect_run, so it replaces the home plugin back to the OLD pin. A dev pane without that hub
	// (a failed build or start, or H ended) gets no hub call: step 3 closes it.
	if h := r.readHub(relHubFile); r.running(h) {
		r.say("the H hub runs (pid %d, %s)", h.PID, h.Client)
		if err := r.replacePlugin(c, h.Client, c.To, c.From, true); err != nil {
			return err
		}
	} else {
		r.say("the H hub of step start does not run; no replace under H")
	}
	// 3. Stop, OLD, start.
	if err := r.closeDev("dev-close-rollback", false); err != nil {
		return err
	}
	if err := r.holdBackup(c, "from from"); err != nil {
		return err
	}
	os.Remove(filepath.Join(r.run, relHubFile))
	if cur, err := r.head(); err != nil {
		return err
	} else if cur != c.From.Commit {
		if err := r.mutate("checkout-old", map[string]string{"commit": c.From.Commit}, func() (any, error) {
			_, err := r.git("checkout", "-q", "--detach", c.From.Commit)
			return nil, err
		}); err != nil {
			return err
		}
	}
	if err := r.needHead(c.From.Commit, "OLD"); err != nil {
		return err
	}
	if err := r.fetchRole(); err != nil {
		return err
	}
	h, err := r.startHub(c.From.Commit, "rollback")
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(r.run, relOldHubFile), h); err != nil {
		return err
	}
	return r.rollbackFinish(c, h)
}

// rollbackFinish is step 4 of the rollback, under the OLD hub h. A home run at H here means that H
// stopped before step 2: OLD replaces it, in the form of the frozen candidate. No other form is
// tried after a failure.
func (r *releaser) rollbackFinish(c *relCandidate, h *relHub) error {
	if err := r.replacePlugin(c, h.Client, c.To, c.From, c.OldExpectRun); err != nil {
		return err
	}
	if err := r.verifyList(relVerifyBack); err != nil {
		return err
	}
	s, _ := r.stamp()
	if err := os.Rename(filepath.Join(r.run, relStampFile), filepath.Join(r.run, relStampFile+".rolled-back-"+s)); err != nil {
		return err
	}
	r.say("rolled back to OLD %s", c.From.Commit)
	return nil
}

// hubResume starts OLD again after step stop, when step backup failed before its stamp. No pin or
// checkout changed. After the stamp exists, use rollback.
func hubResume(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	if s, _ := r.stamp(); s != "" {
		return relFail("a backup stamp exists (%s): use rollback", s)
	}
	op, sh, err := r.settleCreate(c)
	if err != nil {
		return err
	}
	if op == "dev-start-resume" && sh == nil {
		return relFail("the dev pane of this step runs no new hub; see %s", r.spec.DevLog)
	}
	if op != "dev-start-resume" {
		panes, err := r.devPanes()
		if err != nil {
			return err
		}
		if len(panes) != 0 {
			return relFail("a %s pane runs; nothing to resume", devPaneLabel)
		}
		if err := r.needHead(c.From.Commit, "OLD"); err != nil {
			return err
		}
		if err := r.holdOK(c); err != nil {
			return err
		}
		if err := r.fetchRole(); err != nil {
			return err
		}
		if _, err := r.startHub(c.From.Commit, "resume"); err != nil {
			return err
		}
	}
	if err := r.verifyList(relVerifyBack); err != nil {
		return err
	}
	r.say("OLD %s runs again", c.From.Commit)
	return nil
}

// openFreeze is the open of freeze: -spec <file> -run <dir>; the run directory must not exist.
func openFreeze(name string, args []string, stdout, stderr io.Writer) (*releaser, int) {
	fs := flag.NewFlagSet("release "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	spec := fs.String("spec", "", "the spec file (required)")
	dir := fs.String("run", "", "the new run directory (required)")
	if err := fs.Parse(args); err != nil || *spec == "" || *dir == "" || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage: lazyrex-hub release %s -spec <file> -run <new dir>\n", name)
		return nil, relUsage
	}
	if err := createRun(*dir, *spec); err != nil {
		fmt.Fprintf(stderr, "release %s: FAILED: %v\n", name, err)
		return nil, relFailed
	}
	return openStep(name, []string{"-run", *dir}, stdout, stderr)
}
