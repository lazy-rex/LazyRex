package main

// The plugin command (docs/plugin-sources.md, section 6): lazyrex-hub plugin new. The other
// helpers here serve the plugin manager ops and pluginExport: the config read, the flocks of the
// lock writers, the snapshot of the lock and the managed file, and the fetch of a commit. A lock
// writer writes the lock only after every fetch and check passes, and always by a rename
// (writeLock). A failure leaves the old lock; the entries it published stay on disk, and nothing
// references them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// pluginCmdOpts are the fetch options of the lock writers. Only tests and the plugin manager set
// them: no CLI flag, config field or environment variable does.
type pluginCmdOpts struct {
	// remote gives the fetch URL of github:<owner>/<repo>. Default: https://github.com/<owner>/<repo>.
	remote func(owner, repo string) string
	// git runs the git commands. nil: execGit. The plugin manager sets the fetch child.
	git gitRunner
}

func (o pluginCmdOpts) runner() gitRunner {
	if o.git != nil {
		return o.git
	}
	return execGit
}

func (o pluginCmdOpts) url(r githubRef) string {
	if o.remote != nil {
		return o.remote(r.Owner, r.Repo)
	}
	return "https://github.com/" + r.Owner + "/" + r.Repo
}

// pluginCmdConfig is the part of the hub config that the commands read: state, lock, plugins and
// managed_plugins. The commands use no Rex, so the server and agent checks of loadConfig do not
// apply.
type pluginCmdConfig struct {
	path    string // as given: the texts name it
	state   string
	lock    string // the lock file
	managed string // the managed file (section 13)
	config  []*configEntry
	// entries are the config entries, then the managed entries (addManaged). loadSnapshot sets them
	// under both flocks.
	entries []*configEntry
}

func readPluginCmdConfig(cfgPath string) (*pluginCmdConfig, error) {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	var c struct {
		State          string            `json:"state"`
		Lock           string            `json:"lock"`
		Plugins        []json.RawMessage `json:"plugins"`
		ManagedPlugins string            `json:"managed_plugins"`
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
	// The managed file: the field relative to the config directory, else <state>/plugins.json (as
	// loadConfig).
	managed := filepath.Join(state, "plugins.json")
	if c.ManagedPlugins != "" {
		managed = c.ManagedPlugins
		if !filepath.IsAbs(managed) {
			managed = filepath.Join(filepath.Dir(abs), managed)
		}
	}
	return &pluginCmdConfig{path: cfgPath, state: state, lock: lockPath(cfgPath, c.Lock), managed: managed, config: entries}, nil
}

// loadSnapshot reads the lock and then the managed file, as one snapshot. The caller holds both
// flocks, so a writer that follows the flock protocol cannot add a managed source and its pin
// between the two reads. A managed file that does not parse fails the command: a command that
// ignored it could drop the managed lock entries (section 13).
func (cfg *pluginCmdConfig) loadSnapshot() (*pluginLock, error) {
	lock, err := readLock(cfg.lock)
	if err != nil {
		return nil, err
	}
	managed, err := parseManaged(cfg.managed)
	if err != nil {
		return nil, fmt.Errorf("managed plugin file %s: %w", cfg.managed, err)
	}
	cfg.entries = addManaged(cfg.config, managed)
	return lock, nil
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

// writeLines writes the lines in one write. A short write without an error is io.ErrShortWrite.
// A writer can take a prefix before it fails, so a failure does not mean that nothing was written.
func writeLines(w io.Writer, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	b := strings.Join(lines, "")
	n, err := io.WriteString(w, b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	return err
}

// fetchAt fetches commit (by fetchRef) and gives its complete lock entry. tag is the tag of a
// latest entry, else "".
func fetchAt(ctx context.Context, cfg *pluginCmdConfig, o pluginCmdOpts, tmpRoot string, r githubRef, fetchRef, commit, kind, tag string, byID bool) (*lockEntry, error) {
	ft, err := withTmp(tmpRoot, func(tmp string) (*fetchedTree, error) {
		return fetchCommit(ctx, o.runner(), cfg.state, tmp, o.url(r), r.Owner, r.Repo, r.Subdir, fetchRef, commit, byID)
	})
	if err != nil {
		return nil, err
	}
	return &lockEntry{lockPin: lockPin{Name: ft.Manifest.Name, Owner: r.Owner, Repo: r.Repo, Subdir: r.Subdir, Ref: r.Ref, RefKind: kind,
		Tag: tag, Commit: commit, Digest: ft.Digest}, Dir: ft.Dir}, nil
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

// graphRefusals are the refusals of the graph check of checkNewGraph, by entry (the plugin
// manager compares them before and after a change).
func graphRefusals(cfg *pluginCmdConfig, next *pluginLock) map[*configEntry]error {
	out := map[*configEntry]error{}
	for i, s := range graphSources(cfg, next) {
		if s.Err != nil {
			out[cfg.entries[i]] = s.Err
		}
	}
	return out
}

// graphSources runs the graph check on cfg.entries with the lock next: one result for each entry,
// in order.
func graphSources(cfg *pluginCmdConfig, next *pluginLock) []*sourceEntry {
	src := make([]*sourceEntry, len(cfg.entries))
	for i, e := range cfg.entries {
		s := &sourceEntry{Index: e.index, Source: e.Source}
		if e.managed != "" {
			s.Label = e.identity()
		}
		src[i] = s
		dir := e.dir
		if e.github {
			d, _, err := lockedDir(cfg.state, next, e)
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
	return src
}

// pluginCmdMain runs lazyrex-hub plugin new <name> [-dir <path>]. Exit 0: done; 1: the command
// failed; 2: a usage error. The manager installs and updates plugins (LazyRex: Plugins…).
func pluginCmdMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "new" {
		fmt.Fprintln(stderr, "usage: lazyrex-hub plugin new <name> [-dir <path>]")
		return 2
	}
	return pluginNewMain(args[1:], stdout, stderr)
}
