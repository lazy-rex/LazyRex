package main

// The plugin commands (docs/plugin-sources.md, section 6): lazyrex-hub plugin sync|update|status.
// They are CLI only, because a hub action must answer fast. A command writes the lock only after
// every fetch and check passes, and always by a rename (writeLock). A failure leaves the old lock;
// the entries it published stay on disk, and nothing references them.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// pluginCmdOpts are the options of the command functions. Only tests set them: no CLI flag,
// config field or environment variable does.
type pluginCmdOpts struct {
	// remote gives the fetch URL of github:<owner>/<repo>. Default: https://github.com/<owner>/<repo>.
	remote func(owner, repo string) string
	// afterResolve runs after update resolves the ref of source and before it fetches, so a test
	// can move the ref between the two. nil in production.
	afterResolve func(source string)
}

func (o pluginCmdOpts) url(r githubRef) string {
	if o.remote != nil {
		return o.remote(r.Owner, r.Repo)
	}
	return "https://github.com/" + r.Owner + "/" + r.Repo
}

// pluginCmdConfig is the part of the hub config that the commands read: state, lock and plugins.
// The commands use no Rex, so the server and agent checks of loadConfig do not apply.
type pluginCmdConfig struct {
	path    string // as given: the texts name it
	state   string
	lock    string // the lock file
	entries []*configEntry
}

func readPluginCmdConfig(cfgPath string) (*pluginCmdConfig, error) {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	var c struct {
		State   string            `json:"state"`
		Lock    string            `json:"lock"`
		Plugins []json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", cfgPath, err)
	}
	if c.State == "" {
		return nil, fmt.Errorf("%s: state is required", cfgPath)
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return nil, err
	}
	entries, err := parseEntries(filepath.Dir(abs), c.Plugins)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cfgPath, err)
	}
	// A relative state is relative to the working directory, as for the hub. The command makes it
	// absolute once, because git runs in other directories.
	state, err := filepath.Abs(c.State)
	if err != nil {
		return nil, err
	}
	return &pluginCmdConfig{path: cfgPath, state: state, lock: lockPath(cfgPath, c.Lock), entries: entries}, nil
}

// lockCommands takes the two exclusive flocks of section 6, in order: <state>/plugins/.lock (all
// commands on one state), then <lockfile>.lck. A held flock fails at once. release closes both.
func lockCommands(state, lockFile string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Join(state, "plugins"), 0o700); err != nil {
		return nil, err
	}
	first, err := flockPID(filepath.Join(state, "plugins", ".lock"))
	if err != nil {
		return nil, err
	}
	second, err := flockPID(lockFile + ".lck")
	if err != nil {
		first.Close()
		return nil, err
	}
	return func() { second.Close(); first.Close() }, nil
}

// flockPID takes an exclusive flock on path without waiting, and writes this process's pid into
// the file. A held flock gives "another plugin command runs (pid N)": the waiter only reads the
// file, so it never truncates the holder's pid. An empty or unreadable pid is "unknown" (the
// holder may not have written it yet).
func flockPID(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		defer f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("flock %s: %w", path, err)
		}
		pid := "unknown"
		b := make([]byte, 32)
		n, _ := f.ReadAt(b, 0)
		if p, err := strconv.Atoi(strings.TrimSpace(string(b[:n]))); err == nil && p > 0 {
			pid = strconv.Itoa(p)
		}
		return nil, fmt.Errorf("another plugin command runs (pid %s)", pid)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// pluginSync is lazyrex-hub plugin sync: first install and repair (section 6). A GitHub source
// without a lock entry is resolved and fetched. An entry whose cache entry is missing or changed,
// or whose derived fields do not agree, is fetched again at its trusted commit. It never moves a
// locked commit. It drops the lock entries whose source is not in the config.
func pluginSync(ctx context.Context, cfgPath string, o pluginCmdOpts) error {
	return runPluginCmd(ctx, "sync", cfgPath, nil, o, io.Discard)
}

// pluginUpdate is lazyrex-hub plugin update: it resolves the refs again (of all GitHub entries, or
// of the entries that args select), fetches, and rewrites the lock.
func pluginUpdate(ctx context.Context, cfgPath string, args []string, o pluginCmdOpts) error {
	return runPluginCmd(ctx, "update", cfgPath, args, o, io.Discard)
}

// runPluginCmd runs sync or update. It prints one line for each change to w.
func runPluginCmd(ctx context.Context, cmd, cfgPath string, args []string, o pluginCmdOpts, w io.Writer) error {
	fail := func(err error) error {
		return fmt.Errorf("plugin %s: %w; the lock is unchanged", cmd, err)
	}
	cfg, err := readPluginCmdConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("plugin %s: %w", cmd, err)
	}
	release, err := lockCommands(cfg.state, cfg.lock)
	if err != nil {
		return fmt.Errorf("plugin %s: %w", cmd, err)
	}
	defer release()
	old, err := readLock(cfg.lock)
	if err != nil {
		// A lock that does not decode is never taken as an empty lock, and never replaced.
		return fmt.Errorf("plugin %s: %w", cmd, err)
	}
	tmpRoot := filepath.Join(cfg.state, "plugins", ".tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return fail(err)
	}

	// Every check that needs no network runs before the first git call that uses it.
	for _, e := range cfg.entries {
		if !e.github {
			continue
		}
		r := e.githubRef()
		if err := checkRef(ctx, tmpRoot, r.Ref); err != nil {
			return fail(fmt.Errorf("%s: %w", e.Source, err))
		}
	}
	selected := map[*configEntry]bool{}
	if cmd == "update" {
		if selected, err = selectEntries(cfg, old, args); err != nil {
			return fmt.Errorf("plugin update: %w", err)
		}
	}

	next := &pluginLock{Version: lockVersion, Plugins: map[string]*lockEntry{}}
	for _, e := range cfg.entries {
		if !e.github {
			continue
		}
		le := old.Plugins[e.Source]
		r := e.githubRef()
		var (
			got  *lockEntry
			verb string
		)
		switch {
		case cmd == "update" && selected[e]:
			got, verb, err = updateEntry(ctx, cfg, o, tmpRoot, e, r, le)
		case cmd == "update":
			got = le // an unselected entry keeps its lock entry exactly; nil stays absent
		default:
			got, verb, err = syncEntry(ctx, cfg, o, tmpRoot, e, r, le)
		}
		if err != nil {
			return fail(fmt.Errorf("%s: %w", e.Source, err))
		}
		if got != nil {
			next.Plugins[e.Source] = got
		}
		if verb != "" {
			fmt.Fprintf(w, "%s %s %s\n", verb, e.Source, got.Commit)
		}
	}
	var dropped []string
	for src := range old.Plugins {
		if next.Plugins[src] == nil && !hasSource(cfg.entries, src) {
			dropped = append(dropped, src)
		}
	}
	sort.Strings(dropped)

	if err := checkNewGraph(cfg, next); err != nil {
		return fail(err)
	}
	b, err := encodeLock(next)
	if err != nil {
		return fail(err)
	}
	cur, err := os.ReadFile(cfg.lock)
	switch {
	case err == nil && string(cur) == string(b):
		return nil
	case errors.Is(err, os.ErrNotExist) && len(next.Plugins) == 0:
		return nil // no lock file, and nothing to pin
	}
	if err := writeLock(cfg.lock, next); err != nil {
		return fail(err)
	}
	for _, src := range dropped {
		fmt.Fprintf(w, "dropped %s\n", src)
	}
	return nil
}

// syncEntry gives the lock entry of the GitHub entry e after sync, and a verb for the output
// ("" when the entry is unchanged).
func syncEntry(ctx context.Context, cfg *pluginCmdConfig, o pluginCmdOpts, tmpRoot string, e *configEntry, r githubRef, le *lockEntry) (*lockEntry, string, error) {
	if le == nil {
		got, err := fetchNew(ctx, cfg, o, tmpRoot, e, r, nil)
		return got, "installed", err
	}
	if _, _, err := lockedDir(cfg.path, cfg.state, &pluginLock{Plugins: map[string]*lockEntry{e.Source: le}}, e); err == nil {
		return le, "", nil
	}
	// Repair (section 3): keep a trusted commit and rebuild every other field from it.
	if why := le.trust(r); why != "" {
		return nil, "", untrustedCommit(cfg.path, e.Source, why)
	}
	kind, kinds := le.RefKind, refKinds(r.Ref)
	switch {
	case contains(kinds, kind):
	case len(kinds) == 1:
		kind = kinds[0] // the ref form gives the kind: no ls-remote, so a deleted ref does not matter
	default:
		// A bare name is a branch or a tag; ls-remote tells which. The commit stays the locked one.
		k, err := withTmp(tmpRoot, func(tmp string) (string, error) {
			res, err := resolveRef(ctx, tmp, o.url(r), r.Ref)
			return res.Kind, err
		})
		if err != nil {
			return nil, "", err
		}
		kind = k
	}
	got, err := fetchAt(ctx, cfg, o, tmpRoot, r, le.Commit, le.Commit, kind, true)
	return got, "repaired", err
}

// updateEntry resolves the ref of e again and gives its new lock entry. An entry whose ref still
// gives its locked commit, and whose cache entry is good, stays unchanged.
func updateEntry(ctx context.Context, cfg *pluginCmdConfig, o pluginCmdOpts, tmpRoot string, e *configEntry, r githubRef, le *lockEntry) (*lockEntry, string, error) {
	got, err := fetchNew(ctx, cfg, o, tmpRoot, e, r, func(res resolved) bool {
		if le == nil || le.Commit != res.Commit || le.RefKind != res.Kind {
			return false
		}
		_, _, err := lockedDir(cfg.path, cfg.state, &pluginLock{Plugins: map[string]*lockEntry{e.Source: le}}, e)
		return err == nil
	})
	switch {
	case err != nil:
		return nil, "", err
	case got == nil:
		return le, "", nil
	case le == nil:
		return got, "installed", nil
	}
	return got, "updated", nil
}

// fetchNew resolves the ref of e and fetches the resolved commit. keep, when set, can decide that
// the resolved commit needs no fetch: fetchNew then gives nil.
func fetchNew(ctx context.Context, cfg *pluginCmdConfig, o pluginCmdOpts, tmpRoot string, e *configEntry, r githubRef, keep func(resolved) bool) (*lockEntry, error) {
	res, err := withTmp(tmpRoot, func(tmp string) (resolved, error) {
		return resolveRef(ctx, tmp, o.url(r), r.Ref)
	})
	if err != nil {
		return nil, err
	}
	if keep != nil && keep(res) {
		return nil, nil
	}
	if o.afterResolve != nil {
		o.afterResolve(e.Source)
	}
	return fetchAt(ctx, cfg, o, tmpRoot, r, res.FetchRef, res.Commit, res.Kind, res.Kind == "commit")
}

// fetchAt fetches commit (by fetchRef) and gives its complete lock entry.
func fetchAt(ctx context.Context, cfg *pluginCmdConfig, o pluginCmdOpts, tmpRoot string, r githubRef, fetchRef, commit, kind string, byID bool) (*lockEntry, error) {
	ft, err := withTmp(tmpRoot, func(tmp string) (*fetchedTree, error) {
		return fetchCommit(ctx, cfg.state, tmp, o.url(r), r.Owner, r.Repo, r.Subdir, fetchRef, commit, byID)
	})
	if err != nil {
		return nil, err
	}
	return &lockEntry{lockPin: lockPin{Name: ft.Manifest.Name, Owner: r.Owner, Repo: r.Repo, Subdir: r.Subdir, Ref: r.Ref, RefKind: kind,
		Commit: commit, Digest: ft.Digest}, Dir: ft.Dir}, nil
}

// withTmp runs f with a new temporary directory under tmpRoot and removes it after.
func withTmp[T any](tmpRoot string, f func(tmp string) (T, error)) (T, error) {
	var zero T
	tok, err := randomToken()
	if err != nil {
		return zero, err
	}
	tmp := filepath.Join(tmpRoot, tok)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return zero, err
	}
	defer removeTree(tmp)
	return f(tmp)
}

func hasSource(entries []*configEntry, src string) bool {
	for _, e := range entries {
		if e.Source == src {
			return true
		}
	}
	return false
}

// checkNewGraph checks the dependency graph of the config with the new lock (section 6, "Graph
// check"). Any refused entry, local or GitHub, fails the command: a lock that refuses an entry at
// the next start is not written. The text is the first refusal in config order.
func checkNewGraph(cfg *pluginCmdConfig, next *pluginLock) error {
	src := make([]*sourceEntry, len(cfg.entries))
	for i, e := range cfg.entries {
		s := &sourceEntry{Index: e.index, Source: e.Source}
		src[i] = s
		dir := e.dir
		if e.github {
			d, _, err := lockedDir(cfg.path, cfg.state, next, e)
			if err != nil {
				s.Err = err
				continue
			}
			dir = d
		}
		m, _, err := readPluginManifest(dir)
		if err != nil {
			s.Err = fmt.Errorf("plugin source %s: %w", e.Source, err)
			continue
		}
		s.Name, s.Depends = m.Name, m.Depends
	}
	checkGraph(src)
	for _, s := range src {
		if s.Err != nil {
			return s.Err
		}
	}
	return nil
}

// selectEntries gives the entries that the update arguments select (the approved update rule).
// An argument that starts with "managed:" selects only by managed id (no managed entries exist
// yet). Another argument selects by exact source or by plugin name; one entry that matches both
// counts once. Each argument must select exactly one entry, and it must be a GitHub entry. No
// argument selects every GitHub entry. The rule needs no git.
func selectEntries(cfg *pluginCmdConfig, lock *pluginLock, args []string) (map[*configEntry]bool, error) {
	sel := map[*configEntry]bool{}
	if len(args) == 0 {
		for _, e := range cfg.entries {
			if e.github {
				sel[e] = true
			}
		}
		return sel, nil
	}
	// A name is known only from a local manifest, or from a lock entry that passes lockedDir
	// (section 7, "Entry identity").
	names := map[*configEntry]string{}
	for _, e := range cfg.entries {
		if e.github {
			if _, _, err := lockedDir(cfg.path, cfg.state, lock, e); err == nil {
				names[e] = lock.Plugins[e.Source].Name
			}
		} else if m, _, err := readPluginManifest(e.dir); err == nil {
			names[e] = m.Name
		}
	}
	for _, a := range args {
		var match []*configEntry
		if !strings.HasPrefix(a, "managed:") {
			for _, e := range cfg.entries {
				if e.Source == a || names[e] == a {
					match = append(match, e)
				}
			}
		}
		switch len(match) {
		case 0:
			return nil, fmt.Errorf("no entry has the name, source or identity %s", a)
		case 1:
		default:
			ids := make([]string, len(match))
			for i, e := range match {
				ids[i] = fmt.Sprintf("entry %d", e.index)
			}
			return nil, fmt.Errorf("%s selects %s; write managed:<id> or the exact source", a, strings.Join(ids, ", "))
		}
		if !match[0].github {
			return nil, fmt.Errorf("%s is a local source; update changes GitHub sources only", a)
		}
		sel[match[0]] = true
	}
	return sel, nil
}

// runPluginStatus is lazyrex-hub plugin status. It runs no git, uses no network and writes no
// lock. It prints the state of each GitHub entry, the lock entries whose source is not in the
// config, and the cache entries that the lock does not reference. ok is false when a GitHub entry
// is not ok or a lock entry is orphaned.
func runPluginStatus(cfgPath string, w io.Writer) (ok bool, err error) {
	cfg, err := readPluginCmdConfig(cfgPath)
	if err != nil {
		return false, fmt.Errorf("plugin status: %w", err)
	}
	release, err := lockCommands(cfg.state, cfg.lock)
	if err != nil {
		return false, fmt.Errorf("plugin status: %w", err)
	}
	defer release()
	lock, err := readLock(cfg.lock)
	if err != nil {
		return false, fmt.Errorf("plugin status: %w", err)
	}
	ok = true
	referenced := map[string]bool{}
	for _, le := range lock.Plugins {
		referenced[le.Dir] = true
	}
	for _, e := range cfg.entries {
		if !e.github {
			continue
		}
		r := e.githubRef()
		le := lock.Plugins[e.Source]
		commit, state := "-", "ok"
		if le != nil && le.Commit != "" {
			commit = le.Commit
		}
		_, _, err := lockedDir(cfg.path, cfg.state, lock, e)
		var ce *cacheStateError
		switch {
		case err == nil:
		case le == nil:
			state = "not installed"
		case errors.As(err, &ce):
			state = ce.state
		default:
			state = err.Error()
		}
		if state != "ok" {
			ok = false
		}
		fmt.Fprintf(w, "%s\tref %s\tcommit %s\tcache %s\n", e.Source, r.Ref, commit, state)
	}
	var orphans []string
	for src := range lock.Plugins {
		if !hasSource(cfg.entries, src) {
			orphans = append(orphans, src)
		}
	}
	sort.Strings(orphans)
	for _, src := range orphans {
		ok = false
		fmt.Fprintf(w, "%s\torphaned (not in the config; plugin sync drops it)\tcommit %s\n", src, lock.Plugins[src].Commit)
	}
	for _, rel := range cacheEntries(cfg.state) {
		if !referenced[rel] {
			fmt.Fprintf(w, "%s\tunreferenced (no recorded digest; a running instance or another config can use it)\n", rel)
		}
	}
	return ok, nil
}

// cacheEntries lists the published cache entries under <state>/plugins/github, as lock dir values,
// sorted. Stages (names that start with ".") are not entries.
func cacheEntries(state string) []string {
	var out []string
	root := filepath.Join(state, "plugins", "github")
	owners, _ := os.ReadDir(root)
	for _, o := range owners {
		repos, _ := os.ReadDir(filepath.Join(root, o.Name()))
		for _, r := range repos {
			ents, _ := os.ReadDir(filepath.Join(root, o.Name(), r.Name()))
			for _, e := range ents {
				if !strings.HasPrefix(e.Name(), ".") {
					out = append(out, "github/"+o.Name()+"/"+r.Name()+"/"+e.Name())
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// pluginCmdMain runs lazyrex-hub plugin <sync|update|status> -config <cfg> [args]. Exit 0: done;
// 1: the command failed (or status found an entry that is not ok); 2: a usage error.
func pluginCmdMain(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: lazyrex-hub plugin sync|update|status -config <cfg> [update: name|source ...]")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	cmd := args[0]
	fs := flag.NewFlagSet("plugin "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "hub configuration JSON (required)")
	if cmd != "sync" && cmd != "update" && cmd != "status" {
		return usage()
	}
	if err := fs.Parse(args[1:]); err != nil || *cfgPath == "" {
		return usage()
	}
	if cmd != "update" && fs.NArg() > 0 {
		return usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch cmd {
	case "status":
		var ok bool
		ok, err = runPluginStatus(*cfgPath, stdout)
		if err == nil && !ok {
			return 1
		}
	default:
		err = runPluginCmd(ctx, cmd, *cfgPath, fs.Args(), pluginCmdOpts{}, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
