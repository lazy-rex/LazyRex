package main

// Plugin sources (design v3, approved 2026-10-20): the manifest rules and the dependency graph.
// A config entry names a source; the manifest (lazyrex.json at the source root) owns the name, the
// main file, events, notice and depends. These functions read and check; they start nothing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var pluginName = regexp.MustCompile(`^[a-z0-9_-]+$`)

// pluginManifest is the runtime part of lazyrex.json. The test section belongs to the harness
// (testcmd.go); the hub keeps it unread but still refuses unknown fields.
type pluginManifest struct {
	Name     string          `json:"name"`
	Main     string          `json:"main"`
	Events   string          `json:"events"`
	Notice   string          `json:"notice"`
	Depends  []string        `json:"depends"`
	Requires json.RawMessage `json:"requires"` // harness only, until test.plugins replaces it
	Test     json.RawMessage `json:"test"`
}

// readPluginManifest reads dir/lazyrex.json and checks every rule. It returns the manifest and the
// path of its main file (dir joined with main; dir is made absolute).
func readPluginManifest(dir string) (*pluginManifest, string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, "lazyrex.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("no lazyrex.json in %s", dir)
	} else if err != nil {
		return nil, "", err
	}
	var m pluginManifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, "", fmt.Errorf("%s/lazyrex.json: %w", dir, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, "", fmt.Errorf("%s/lazyrex.json: data after the manifest object", dir)
	}
	if err := m.check(); err != nil {
		return nil, "", fmt.Errorf("%s/lazyrex.json: %w", dir, err)
	}
	main, err := pluginMain(dir, m.Main)
	if err != nil {
		return nil, "", fmt.Errorf("plugin %s: %w", m.Name, err)
	}
	return &m, main, nil
}

// check checks the fields that need no file system.
func (m *pluginManifest) check() error {
	if !pluginName.MatchString(m.Name) {
		return errors.New("name (a-z, 0-9, _, -) is required")
	}
	if m.Main == "" {
		return errors.New("main is required")
	}
	if m.Events != "" && m.Events != "session" && m.Events != "all" {
		return errors.New("events must be session or all")
	}
	if err := checkNotice(m.Notice); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, d := range m.Depends {
		if !pluginName.MatchString(d) {
			return fmt.Errorf("depends: %q is not a plugin name", d)
		}
		if seen[d] {
			return fmt.Errorf("depends: %s appears twice", d)
		}
		seen[d] = true
	}
	return nil
}

// pluginMain returns dir/main if main is a regular file inside dir. main is relative, has no ".."
// component and is not a symlink, also when it points inside dir. A parent directory can be a
// symlink if main still resolves inside the resolved dir (a release clone path, for example).
func pluginMain(dir, main string) (string, error) {
	outside := fmt.Errorf("main %s must be a file inside the plugin directory", main)
	if filepath.IsAbs(main) || main != filepath.Clean(main) {
		return "", outside
	}
	for _, part := range strings.Split(main, string(filepath.Separator)) {
		if part == ".." {
			return "", outside
		}
	}
	p := filepath.Join(dir, main)
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("main %s does not exist in %s", main, dir)
	} else if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("main %s must not be a symlink", main)
	}
	if !fi.Mode().IsRegular() {
		return "", outside
	}
	rd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(rp, rd+string(filepath.Separator)) {
		return "", outside
	}
	return p, nil
}

// sourceEntry is one config entry during a start check. Index is 1-based (as the error texts
// count). Name is empty until it is known (a manifest, or a lock entry that passed its check). Err
// is the reason the entry is refused; checkGraph adds graph reasons.
type sourceEntry struct {
	Index   int
	Source  string
	Name    string
	Depends []string
	Err     error
}

// duplicateSources refuses a config with two entries of the same source string: a malformed
// config, so the hub does not start.
func duplicateSources(entries []*sourceEntry) error {
	first := map[string]int{}
	for _, e := range entries {
		if i, ok := first[e.Source]; ok {
			return fmt.Errorf("entries %d and %d have the same source %s", i, e.Index, e.Source)
		}
		first[e.Source] = e.Index
	}
	return nil
}

// checkGraph refuses entries for graph reasons: a shared name (every entry with it), a missing
// dependency, a refused dependency (transitive), and a cycle (every member). An entry that is
// already refused keeps its own error.
func checkGraph(entries []*sourceEntry) {
	byName := map[string][]*sourceEntry{}
	for _, e := range entries {
		if e.Name != "" {
			byName[e.Name] = append(byName[e.Name], e)
		}
	}
	for name, list := range byName {
		if len(list) < 2 {
			continue
		}
		idx := make([]string, len(list))
		for i, e := range list {
			idx[i] = fmt.Sprint(e.Index)
		}
		for _, e := range list {
			if e.Err == nil {
				e.Err = fmt.Errorf("entries %s have the same name %s", strings.Join(idx, " and "), name)
			}
		}
	}
	const (
		visiting = 1
		done     = 2
	)
	state := map[*sourceEntry]int{}
	var stack []*sourceEntry
	var visit func(e *sourceEntry)
	visit = func(e *sourceEntry) {
		if state[e] == done {
			return
		}
		state[e] = visiting
		stack = append(stack, e)
		for _, d := range e.Depends {
			list := byName[d]
			if len(list) != 1 {
				if e.Err == nil {
					if len(list) == 0 {
						e.Err = fmt.Errorf("plugin %s: dependency %s is not configured", e.Name, d)
					} else {
						e.Err = fmt.Errorf("plugin %s: dependency %s is refused: %v", e.Name, d, list[0].Err)
					}
				}
				continue
			}
			dep := list[0]
			if state[dep] == visiting {
				// Every member from dep to the top of the stack is in the cycle.
				start := len(stack) - 1
				for stack[start] != dep {
					start--
				}
				names := []string{}
				for _, m := range stack[start:] {
					names = append(names, m.Name)
				}
				names = append(names, dep.Name)
				for _, m := range stack[start:] {
					if m.Err == nil {
						m.Err = fmt.Errorf("plugin %s: dependency cycle %s", m.Name, strings.Join(names, " -> "))
					}
				}
				continue
			}
			visit(dep)
			if dep.Err != nil && e.Err == nil {
				e.Err = fmt.Errorf("plugin %s: dependency %s is refused: %v", e.Name, d, dep.Err)
			}
		}
		stack = stack[:len(stack)-1]
		state[e] = done
	}
	for _, e := range entries {
		if e.Name != "" {
			visit(e)
		}
	}
}

// startOrder lists the entries that checkGraph did not refuse, in a stable topological order: each
// step takes the first entry in config order whose dependencies are all listed.
func startOrder(entries []*sourceEntry) []*sourceEntry {
	var ready []*sourceEntry
	for _, e := range entries {
		if e.Err == nil && e.Name != "" {
			ready = append(ready, e)
		}
	}
	listed := map[string]bool{}
	var out []*sourceEntry
	for len(ready) > 0 {
		i := 0
		for ; i < len(ready); i++ {
			ok := true
			for _, d := range ready[i].Depends {
				ok = ok && listed[d]
			}
			if ok {
				break
			}
		}
		if i == len(ready) {
			break // not reached after checkGraph: each dependency is listed or refused its dependent
		}
		listed[ready[i].Name] = true
		out = append(out, ready[i])
		ready = append(ready[:i], ready[i+1:]...)
	}
	return out
}
