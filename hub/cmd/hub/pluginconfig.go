package main

// Plugin config entries (docs/plugin-sources.md, sections 1 and 7): an entry names a source; the
// source's manifest names the plugin. parseEntries refuses a malformed config, so the hub does not
// start. resolvePlugins reads each local manifest at hub start and refuses an entry for any other
// problem: the other entries start.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// PluginConfig is a config entry that resolved at hub start: the manifest gives the name, the main
// file (Path), events and notice. Notice is "" when the manifest has none (code no_notice).
type PluginConfig struct {
	Name, Path, Notice, Events string
	Source, Dir                string            // the exact source string; the plugin directory
	Args                       map[string]string // extra key=value arguments of `rex do`
	// WatchRoots are the directories whose subtrees the plugin may watch (hub.watch, docs/hub.md
	// "File watches"). Absolute paths. Empty: every hub.watch.ensure is refused.
	WatchRoots []string
}

// RefusedEntry is a config entry that did not resolve. Name is "" when no manifest named it.
type RefusedEntry struct {
	Entry        int
	Source, Name string
	Err          error
}

type configEntry struct {
	Source string `json:"source"`
	// Args are extra key=value arguments of `rex do` (rex.args in the plugin), for example
	// worktrees_root. The hub's own keys (pluginArgReserved) are refused.
	Args       map[string]string `json:"args"`
	WatchRoots []string          `json:"watch_roots"`
	dir        string            // a local source: the absolute directory
	github     bool
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
		e := &configEntry{}
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

// resolvePlugins reads the manifest of each local entry and applies the graph rules. It returns
// the resolved plugins in config order, the start order (names, dependencies first) and the
// refused entries. cfgPath names the config in the "not installed" text.
func resolvePlugins(cfgPath string, entries []*configEntry) ([]PluginConfig, []string, []RefusedEntry) {
	src := make([]*sourceEntry, len(entries))
	mans := make([]*pluginManifest, len(entries))
	mains := make([]string, len(entries))
	for i, e := range entries {
		s := &sourceEntry{Index: i + 1, Source: e.Source}
		src[i] = s
		if e.github {
			s.Err = fmt.Errorf("plugin %s is not installed: run lazyrex-hub plugin sync -config %s", e.Source, cfgPath)
			continue
		}
		m, main, err := readPluginManifest(e.dir)
		if err != nil {
			s.Err = fmt.Errorf("plugin source %s: %w", e.Source, err)
			continue
		}
		s.Name, s.Depends, mans[i], mains[i] = m.Name, m.Depends, m, main
	}
	checkGraph(src)
	var plugins []PluginConfig
	var refused []RefusedEntry
	for i, s := range src {
		if s.Err != nil {
			refused = append(refused, RefusedEntry{Entry: s.Index, Source: s.Source, Name: s.Name, Err: s.Err})
			continue
		}
		e, m := entries[i], mans[i]
		plugins = append(plugins, PluginConfig{Name: m.Name, Path: mains[i], Notice: m.Notice, Events: m.Events,
			Source: e.Source, Dir: e.dir, Args: e.Args, WatchRoots: e.WatchRoots})
	}
	var order []string
	for _, s := range startOrder(src) {
		order = append(order, s.Name)
	}
	return plugins, order, refused
}
