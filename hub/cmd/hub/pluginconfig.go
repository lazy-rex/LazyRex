package main

// Plugin config entries (docs/plugin-sources.md, sections 1 and 7): an entry names a source; the
// source's manifest names the plugin. parseEntries refuses a malformed config, so the hub does not
// start. resolveEntries reads each local manifest at each start attempt and refuses an entry for
// any other problem: the other entries start.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// pluginRun is what one resolution gives a plugin: the manifest's name, main file (Path), events,
// notice and depends, and the entry's source, args and watch roots. A resolution gives a
// candidate; a spawn copies it to Plugin.run, the metadata of the current instance (section 7,
// "Run and candidate"). Notice is "" when the manifest has none (code no_notice).
type pluginRun struct {
	Name    string            `json:"name"`
	Source  string            `json:"source"` // the exact source string
	Dir     string            `json:"dir"`    // the plugin directory
	Path    string            `json:"path"`
	Events  string            `json:"events,omitempty"`
	Notice  string            `json:"notice,omitempty"`
	Depends []string          `json:"depends,omitempty"`
	Args    map[string]string `json:"args,omitempty"` // extra key=value arguments of `rex do`
	// WatchRoots are the directories whose subtrees the plugin may watch (hub.watch, docs/hub.md
	// "File watches"). Absolute paths. Empty: every hub.watch.ensure is refused.
	WatchRoots []string `json:"watch_roots,omitempty"`
}

// configEntry is one item of the config field plugins. Its identity is its index and its exact
// source. The fields after github change at each resolution; Hub.entriesMu guards them.
type configEntry struct {
	Source string `json:"source"`
	// Args are extra key=value arguments of `rex do` (rex.args in the plugin), for example
	// worktrees_root. The hub's own keys (pluginArgReserved) are refused.
	Args       map[string]string `json:"args"`
	WatchRoots []string          `json:"watch_roots"`
	index      int               // 1-based, as the error texts count
	dir        string            // a local source: the absolute directory
	github     bool
	// name binds at the first resolution that gives it without a conflict, and stays for the life
	// of the hub process. "" until then.
	name string
	seen string     // the manifest name of the last resolution that read one (reported; never a record key)
	cand *pluginRun // the last resolution, when it passed
	err  error      // the last resolution, when it refused the entry
}

var (
	githubSource  = regexp.MustCompile(`^github:([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)@(\S+)$`)
	removedFields = []string{"name", "path", "events", "notice"}
)

// parseEntries checks the config field plugins. cfgDir is the directory of the config file: a
// relative local source is relative to it.
func parseEntries(cfgDir string, raws []json.RawMessage) ([]*configEntry, error) {
	var out []*configEntry
	first := map[string]int{}
	for i, raw := range raws {
		n := i + 1
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			return nil, fmt.Errorf("plugin entry %d: %w", n, err)
		}
		for _, k := range removedFields {
			if _, ok := keys[k]; ok {
				return nil, fmt.Errorf("plugin entry %d: %q is removed: use \"source\" (a directory with lazyrex.json, or github:owner/repo@ref)", n, k)
			}
		}
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
		for k := range e.Args {
			if !pluginArgKey.MatchString(k) || pluginArgReserved[k] {
				return nil, fmt.Errorf("plugin entry %d: arg %q is not allowed", n, k)
			}
		}
		for _, r := range e.WatchRoots {
			if !filepath.IsAbs(r) || filepath.Clean(r) != r {
				return nil, fmt.Errorf("plugin entry %d: watch root %q must be absolute and clean", n, r)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// checkSource checks the source string and resolves a local directory. The ref of a GitHub source
// gets its full check in plugin sync (git check-ref-format): hub start does not run git.
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
		e.github = true
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

// resolveEntries resolves every entry again: each start attempt rebuilds the whole graph (section
// 7, "The graph"). It reads each local manifest and applies the name and graph rules. It records
// each entry's candidate or error, and binds each name that resolved without a conflict. It
// returns the named entries: the ones that passed in start order (dependencies first), then the
// refused ones in config order. cfgPath names the config in the "not installed" text. The caller
// holds Hub.entriesMu, or owns the entries.
func resolveEntries(cfgPath string, entries []*configEntry) []*configEntry {
	src := make([]*sourceEntry, len(entries))
	runs := make([]*pluginRun, len(entries))
	bound := map[string]*configEntry{}
	for _, e := range entries {
		if e.name != "" {
			bound[e.name] = e
		}
	}
	for i, e := range entries {
		s := &sourceEntry{Index: e.index, Source: e.Source, Name: e.name}
		src[i] = s
		if e.github {
			s.Err = fmt.Errorf("plugin %s is not installed: run lazyrex-hub plugin sync -config %s", e.Source, cfgPath)
			continue
		}
		m, main, err := readPluginManifest(e.dir)
		if err == nil {
			e.seen = m.Name
		}
		switch {
		case err != nil:
			s.Err = fmt.Errorf("plugin source %s: %w", e.Source, err)
		case e.name != "" && m.Name != e.name:
			s.Err = fmt.Errorf("plugin %s: the manifest name changed from %s to %s; restart the hub", e.name, e.name, m.Name)
		case e.name == "" && bound[m.Name] != nil:
			// The name is in use: only the entry that resolved later is refused.
			s.Err = fmt.Errorf("entries %d and %d have the same name %s", bound[m.Name].index, e.index, m.Name)
		default:
			s.Name, s.Depends = m.Name, m.Depends
			runs[i] = &pluginRun{Name: m.Name, Source: e.Source, Dir: e.dir, Path: main, Events: m.Events, Notice: m.Notice,
				Depends: m.Depends, Args: e.Args, WatchRoots: e.WatchRoots}
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
