package main

// Plugin config entries (docs/plugin-sources.md, sections 1 and 7): an entry names a source; the
// source's manifest names the plugin. parseEntries refuses a malformed config, so the hub does not
// start. resolveEntries reads each local manifest at each start attempt and refuses an entry for
// any other problem: the other entries start.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// pluginRun is what one resolution gives a plugin: the manifest's name, main file (Path), events,
// notice and depends, and the entry's source, args and watch roots. A resolution gives a
// candidate; a spawn copies it to Plugin.run, the metadata of the current instance (section 7,
// "Run and candidate"). Notice is "" when the manifest has none (code no_notice).
type pluginRun struct {
	Name   string `json:"name"`
	Source string `json:"source"` // the exact source string
	Dir    string `json:"dir"`    // the plugin directory
	// Entry is the published directory of the run, relative to <state>/plugins: a GitHub cache
	// entry (github/<owner>/<repo>/<commit>.<token>; Dir is Entry plus the subdir), or the snapshot
	// of a local source (local/<hex>.<token>, localsnapshot.go). A local candidate has "" until an
	// explicit start publishes it.
	Entry   string            `json:"entry,omitempty"`
	Commit  string            `json:"commit,omitempty"` // a GitHub source: the locked commit; "" for a local source
	Digest  string            `json:"digest,omitempty"` // the digest of the whole Entry: the locked digest, or the snapshot's; "" for an unpublished local candidate
	Tag     string            `json:"tag,omitempty"`    // a latest source: the locked release tag; "" for every other source
	Path    string            `json:"path"`
	SDK     string            `json:"sdk,omitempty"` // the manifest sdk: start through the SDK launcher
	Events  string            `json:"events,omitempty"`
	Notice  string            `json:"notice,omitempty"`
	Depends []string          `json:"depends,omitempty"`
	Args    map[string]string `json:"args,omitempty"` // extra key=value arguments of `rex do`
	// WatchRoots are the directories whose subtrees the plugin may watch (hub.watch, docs/hub.md
	// "File watches"). Absolute paths. Empty: every hub.watch.ensure is refused.
	WatchRoots []string `json:"watch_roots,omitempty"`
	// Prompts and UserMessaging are the grants of the Telegram hub support (docs/hub.md, "Agent
	// prompts and acting as the user"). Only the user's config entry sets them; the manifest has no
	// such fields. A grant check reads the run of the registered instance.
	Prompts       bool `json:"prompts,omitempty"`
	UserMessaging bool `json:"user_messaging,omitempty"`
}

// configEntry is one item of the config field plugins. Its identity is its index and its exact
// source. The fields after github change at each resolution; Hub.entriesMu guards them.
type configEntry struct {
	Source string `json:"source"`
	// Args are extra key=value arguments of `rex do` (rex.args in the plugin), for example
	// worktrees_root. The hub's own keys (pluginArgReserved) are refused.
	Args          map[string]string `json:"args"`
	WatchRoots    []string          `json:"watch_roots"`
	Prompts       bool              `json:"prompts"`
	UserMessaging bool              `json:"user_messaging"`
	index         int               // 1-based, as the error texts count; 0 for a managed entry
	managed       string            // the managed id (section 13); "" for a config entry
	fetch         string            // a managed GitHub entry: "peer" (the plugins_from peer), or "" (GitHub)
	dir           string            // a local source: the absolute directory
	github        bool
	static        error // a refusal that no resolution changes (a managed source that the config also names)
	// A GitHub source: github:<owner>/<repo>[/<subdir>]@<ref> (docs/plugin-sources.md, section 1).
	owner, repo, subdir, gitRef string
	// name binds at the first resolution that gives it without a conflict, and stays for the life
	// of the hub process. "" until then.
	name string
	seen string     // the manifest name of the last resolution that read one (reported; never a record key)
	cand *pluginRun // the last resolution, when it passed
	err  error      // the last resolution, when it refused the entry
	// installErr is the error of the entry's last install op when it did not end done
	// (pluginopsinstall.go); hub.plugins reports "install failed: <it>" while the entry is refused.
	installErr string
}

var (
	// githubSource is github:<owner>/<repo>[/<subdir>]@<ref>. The first @ ends the path: owner, repo
	// and subdir never contain @, and the ref is the rest (it can contain @ or #).
	githubSource  = regexp.MustCompile(`^github:([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)(/[^@]*)?@(\S+)$`)
	subdirSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// parseEntries checks the config field plugins. cfgDir is the directory of the config file: a
// relative local source is relative to it.
func parseEntries(cfgDir string, raws []json.RawMessage) ([]*configEntry, error) {
	var out []*configEntry
	first := map[string]int{}
	messenger := 0 // the entry with user_messaging
	for i, raw := range raws {
		n := i + 1
		e := &configEntry{index: n}
		if err := decodeManifest(fmt.Sprintf("plugin entry %d", n), raw, e); err != nil {
			return nil, err
		}
		if err := e.checkSource(cfgDir); err != nil {
			return nil, fmt.Errorf("plugin entry %d: %w", n, err)
		}
		if j, ok := first[e.Source]; ok {
			return nil, fmt.Errorf("entries %d and %d have the same source %s", j, n, e.Source)
		}
		first[e.Source] = n
		if err := e.checkFields(); err != nil {
			return nil, fmt.Errorf("plugin entry %d: %w", n, err)
		}
		if e.UserMessaging {
			if messenger != 0 {
				return nil, fmt.Errorf("entries %d and %d both set user_messaging", messenger, n)
			}
			messenger = n
		}
		out = append(out, e)
	}
	return out, nil
}

// checkFields checks the args and the watch roots of an entry (config or managed).
func (e *configEntry) checkFields() error {
	for k := range e.Args {
		if !pluginArgKey.MatchString(k) || pluginArgReserved[k] {
			return fmt.Errorf("arg %q is not allowed", k)
		}
	}
	for _, r := range e.WatchRoots {
		if !filepath.IsAbs(r) || filepath.Clean(r) != r {
			return fmt.Errorf("watch root %q must be absolute and clean", r)
		}
	}
	return nil
}

// identity is the entry's identity in texts: "entry N" for a config entry, "managed:<id>" for a
// managed entry (section 13).
func (e *configEntry) identity() string {
	if e.managed != "" {
		return "managed:" + e.managed
	}
	return fmt.Sprintf("entry %d", e.index)
}

// managedFileLimit bounds the read of the managed file (the coordinator, 2026-10-21): a larger
// file is malformed. The plugin manager refuses to write a file over it.
const managedFileLimit = 1 << 20

var managedID = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// parseManaged reads the managed file (section 13): the plugin manager's entries. A missing file
// is an empty list. Any other problem makes the whole file malformed: the caller refuses every
// managed entry and keeps the config entries.
func parseManaged(path string) ([]*configEntry, error) {
	b, err := readManagedBytes(path)
	if err != nil || b == nil {
		return nil, err
	}
	return parseManagedBytes(b)
}

// readManagedBytes reads at most managedFileLimit+1 bytes of the managed file, so that
// parseManagedBytes refuses a larger file without a full read. A missing file gives nil, nil.
func readManagedBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, managedFileLimit+1))
}

// managedFileEntry is one entry of the managed file, as the file has it. The plugin manager
// writes the file from these values (pluginops.go).
type managedFileEntry struct {
	ID         string            `json:"id"`
	Source     string            `json:"source"`
	Args       map[string]string `json:"args"`
	WatchRoots []string          `json:"watch_roots"`
	// Fetch is where a fetch of the entry's pin takes the files (peerfetch.go): "peer" (the
	// plugins_from peer), or absent (GitHub). The writers write it only for "peer".
	Fetch string `json:"fetch,omitempty"`
}

// encodeManaged is the managed file of the entries, as every writer writes it (the plugin
// manager's write and setup): JSON with 2-space indentation and a final newline. The strict loader
// checks the whole file (ids, sources, fields, the size bound) before a writer may write it.
func encodeManaged(plugins []managedFileEntry) ([]byte, error) {
	if plugins == nil {
		plugins = []managedFileEntry{}
	}
	b, err := json.MarshalIndent(struct {
		Plugins []managedFileEntry `json:"plugins"`
	}{plugins}, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if _, err := parseManagedBytes(b); err != nil {
		return nil, err
	}
	return b, nil
}

// parseManagedBytes is parseManaged on the bytes of the file.
func parseManagedBytes(b []byte) ([]*configEntry, error) {
	if len(b) > managedFileLimit {
		return nil, fmt.Errorf("the file is over the %d-byte limit", managedFileLimit)
	}
	var file struct {
		// A pointer: a root of null, {} or {"plugins": null} leaves it nil. Such a file is malformed,
		// never an empty list, so it cannot remove the managed entries.
		Plugins *[]managedFileEntry `json:"plugins"`
	}
	if err := decodeManifest("the file", b, &file); err != nil {
		return nil, err
	}
	if file.Plugins == nil {
		return nil, errors.New("the file must be an object with a plugins array: {\"plugins\": [...]}")
	}
	var out []*configEntry
	ids, sources := map[string]bool{}, map[string]bool{}
	for i, p := range *file.Plugins {
		n := i + 1
		if !managedID.MatchString(p.ID) {
			return nil, fmt.Errorf("plugin %d: id %q must match [a-z0-9_-]{1,64}", n, p.ID)
		}
		if ids[p.ID] {
			return nil, fmt.Errorf("managed:%s: the id is used twice", p.ID)
		}
		e := &configEntry{Source: p.Source, Args: p.Args, WatchRoots: p.WatchRoots, managed: p.ID}
		switch p.Fetch {
		case "", "github":
		case "peer":
			if !strings.HasPrefix(p.Source, "github:") {
				return nil, fmt.Errorf("managed:%s: fetch peer needs a GitHub source", p.ID)
			}
			e.fetch = "peer"
		default:
			return nil, fmt.Errorf("managed:%s: fetch %q must be github or peer", p.ID, p.Fetch)
		}
		if !strings.HasPrefix(p.Source, "github:") && !filepath.IsAbs(p.Source) {
			return nil, fmt.Errorf("managed:%s: source %q must be an absolute directory or github:<owner>/<repo>@<ref>", p.ID, p.Source)
		}
		if err := e.checkSource(""); err != nil {
			return nil, fmt.Errorf("managed:%s: %w", p.ID, err)
		}
		if sources[p.Source] {
			return nil, fmt.Errorf("managed:%s: the source %s is used twice", p.ID, p.Source)
		}
		if err := e.checkFields(); err != nil {
			return nil, fmt.Errorf("managed:%s: %w", p.ID, err)
		}
		ids[p.ID], sources[p.Source] = true, true
		out = append(out, e)
	}
	return out, nil
}

// sameSource reports whether two entries name one source: the same directory for local sources,
// the same string for GitHub sources.
func sameSource(a, b *configEntry) bool {
	if a.github || b.github {
		return a.Source == b.Source
	}
	return a.dir == b.dir
}

// refuseConfigGithub refuses each GitHub entry of the config file (the hub lead, lead-c2c3): a
// GitHub plugin goes in the managed file, where the plugin manager installs and updates it. The
// refusal is static: no resolution changes it, and the graph sees it.
func refuseConfigGithub(entries []*configEntry) {
	for _, e := range entries {
		if e.github {
			e.static = fmt.Errorf("plugin %s in hub.json: a GitHub plugin goes in the managed file: add it in LazyRex: Plugins…", e.Source)
		}
	}
}

// addManaged appends the managed entries to the config entries. A managed entry whose source a
// config entry also names is refused at each resolution (static); the config entry is checked as
// usual.
func addManaged(config, managed []*configEntry) []*configEntry {
	out := append([]*configEntry{}, config...)
	for _, m := range managed {
		for _, c := range config {
			if sameSource(m, c) {
				m.static = fmt.Errorf("%s: the source %s is also in the config", m.identity(), m.Source)
				break
			}
		}
		out = append(out, m)
	}
	return out
}

// checkSource checks the source string and resolves a local directory. The ref of a GitHub source
// gets its full check where git runs (the resolve of an op, git check-ref-format): a plugin start
// does not run git.
func (e *configEntry) checkSource(cfgDir string) error {
	s := e.Source
	switch {
	case s == "":
		return fmt.Errorf("source is required")
	case strings.HasPrefix(s, "github:"):
		m := githubSource.FindStringSubmatch(s)
		if m == nil || m[2] == "." || m[2] == ".." {
			return fmt.Errorf("source %s: write github:<owner>/<repo>@<ref>", s)
		}
		if m[3] != "" {
			d := m[3][1:]
			for _, seg := range strings.Split(d, "/") {
				if !subdirSegment.MatchString(seg) || seg == "." || seg == ".." {
					return fmt.Errorf("source %s: subdir %s must be a relative path of names (A-Z, a-z, 0-9, ., _, -)", s, d)
				}
			}
		}
		e.github = true
		e.owner, e.repo, e.gitRef = m[1], m[2], m[4]
		e.subdir = strings.TrimPrefix(m[3], "/")
	case strings.HasPrefix(s, "~"):
		return fmt.Errorf("source %s: ~ is not expanded: write an absolute path, or a path relative to the config directory", s)
	default:
		e.dir = s
		if !filepath.IsAbs(s) {
			e.dir = filepath.Join(cfgDir, s)
		}
		e.dir = filepath.Clean(e.dir)
	}
	return nil
}

// keptName is a record without an entry (a kept record, plugins.go): the name of a recorded
// instance or spawn intent, and the source that it ran from ("" in rows from before the source was
// recorded).
type keptName struct{ Name, Source string }

// guard refuses an entry whose source or name a kept record holds with another name or source:
// that record's instance still has unresolved ownership, so the entry may not start another one.
// The same name from the same source (or a kept record without a source) passes: the entry adopts
// the record (Hub.resolveLocked).
func guard(kept []keptName, e *configEntry, name string) error {
	for _, k := range kept {
		switch {
		case k.Name == name && (k.Source == "" || k.Source == e.Source):
			return nil
		case k.Source == e.Source:
			return fmt.Errorf("plugin %s: source %s still has an instance of plugin %s; stop %s first", name, e.Source, k.Name, k.Name)
		case k.Name == name:
			return fmt.Errorf("plugin %s: the name is held by an instance from source %s; stop %s first", name, k.Source, name)
		}
	}
	return nil
}

// lockSnapshot is the lock as one resolution reads it: readLock once, for every GitHub entry.
type lockSnapshot struct {
	cfgPath string      // the config, as the texts name it
	state   string      // the hub state directory
	lock    *pluginLock // nil when err is set
	err     error       // the readLock error: it refuses every GitHub entry, and no local entry
}

// readLockSnapshot reads the lock of cfg for one resolution of entries. It reads no file when no
// entry is a GitHub source.
func readLockSnapshot(cfg *Config, entries []*configEntry) lockSnapshot {
	snap := lockSnapshot{cfgPath: cfg.path, state: cfg.State, lock: &pluginLock{Version: lockVersion, Plugins: map[string]*lockEntry{}}}
	for _, e := range entries {
		if e.github {
			snap.lock, snap.err = readLock(lockPath(cfg.path, cfg.Lock))
			break
		}
	}
	return snap
}

// resolveEntries resolves every entry again: each start attempt rebuilds the whole graph (section
// 7, "The graph"). It reads each manifest (a GitHub entry: in its cache entry) and applies the
// name, ownership (guard) and graph rules. It records each entry's candidate or error, and binds
// each name that one entry gives without a conflict. A bound name stays bound. It returns the named entries: the ones that passed
// in start order (dependencies first), then the refused ones in config order. snap is the lock of
// this resolution: a GitHub entry resolves from its cache entry (lockedDir). kept are the records
// without an entry. It reads files and writes only the entry fields; it reads no lock file. The
// caller holds Hub.entriesMu, or owns the entries.
func resolveEntries(snap lockSnapshot, entries []*configEntry, kept []keptName) []*configEntry {
	src := make([]*sourceEntry, len(entries))
	runs := make([]*pluginRun, len(entries))
	for i, e := range entries {
		s := &sourceEntry{Index: e.index, Source: e.Source, Name: e.name}
		if e.managed != "" {
			s.Label = e.identity()
		}
		src[i] = s
		if e.static != nil {
			s.Err = e.static
			continue
		}
		dir, entry, commit, digest, tag := e.dir, "", "", "", ""
		if e.github {
			if snap.err != nil {
				s.Err = fmt.Errorf("plugin %s: %w", e.Source, snap.err)
				continue
			}
			var err error
			if dir, commit, err = lockedDir(snap.state, snap.lock, e); err != nil {
				s.Err = err
				continue
			}
			digest = snap.lock.Plugins[e.Source].Digest // lockedDir checked the cache entry against it
			tag = snap.lock.Plugins[e.Source].Tag       // set only on a latest entry (pluginlock.go)
			entry = snap.lock.Plugins[e.Source].Dir
		}
		m, main, err := readPluginManifest(dir)
		if err == nil {
			e.seen = m.Name
		}
		var held error
		if err == nil {
			held = guard(kept, e, m.Name)
		}
		switch {
		case err != nil:
			s.Err = fmt.Errorf("plugin source %s: %w", e.Source, err)
		case e.name != "" && m.Name != e.name:
			s.Err = fmt.Errorf("plugin %s: the manifest name changed from %s to %s; restart the hub", e.name, e.name, m.Name)
		case held != nil:
			// Refused, and it binds nothing (no run). Its name still counts in the duplicate check: the
			// guard adds a refusal and never makes another entry with that name pass.
			s.Err, s.Name = held, m.Name
		default:
			// A name in use is a duplicate name: checkGraph refuses every entry with it. The owner
			// keeps the binding (uses below), and its running instance continues.
			s.Name, s.Depends = m.Name, m.Depends
			// A run is a copy: it shares no map or slice with the entry, which a reload can change.
			runs[i] = &pluginRun{Name: m.Name, Source: e.Source, Dir: dir, Entry: entry, Commit: commit, Digest: digest, Tag: tag, Path: main, SDK: m.SDK, Events: m.Events,
				Notice: m.Notice, Depends: slices.Clone(m.Depends), Args: maps.Clone(e.Args), WatchRoots: slices.Clone(e.WatchRoots), Prompts: e.Prompts, UserMessaging: e.UserMessaging}
		}
	}
	checkGraph(src)
	uses := map[string]int{}
	for _, s := range src {
		if s.Name != "" {
			uses[s.Name]++
		}
	}
	for i, e := range entries {
		s := src[i]
		if runs[i] != nil && uses[s.Name] == 1 {
			e.name = s.Name
		}
		e.err, e.cand = s.Err, nil
		if s.Err == nil {
			e.cand = runs[i]
		}
	}
	pos := map[*sourceEntry]int{}
	for i, s := range src {
		pos[s] = i
	}
	var out []*configEntry
	for _, s := range startOrder(src) {
		out = append(out, entries[pos[s]])
	}
	for i, e := range entries {
		if e.name != "" && src[i].Err != nil {
			out = append(out, e)
		}
	}
	return out
}

// ref is the identity part of an entry's hub.plugins item: {"entry": N} for a config entry,
// {"managed": "<id>"} for a managed entry, with the exact source.
func (e *configEntry) ref() map[string]any {
	if e.managed != "" {
		return map[string]any{"managed": e.managed, "source": e.Source}
	}
	return map[string]any{"entry": e.index, "source": e.Source}
}
