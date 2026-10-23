package main

// Test isolation (docs/testing.md, "Isolation"). The harness gives the lab server, the hub and
// every process that they start a private environment in the lab directory. This stops accidental
// use of real-home state through HOME, XDG_* and TMPDIR. It is not filesystem confinement: an
// explicit absolute path stays readable by the same OS user.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// reservedEnvKey reports a key that the harness sets itself. Manifest test.env and, in test mode,
// a profile env cannot set it.
func reservedEnvKey(k string) bool {
	switch k {
	case "PATH", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE":
		return true
	}
	return strings.HasPrefix(k, "XDG_") || strings.HasPrefix(k, "REX_")
}

// labHomeDirs are the directories under the lab that the lab environment names.
var labHomeDirs = []string{"home", "home/.config", "home/.local/share", "home/.local/state", "home/.cache", "tmp"}

// labGoCaches holds the harness's Go caches for each PATH (goCaches). A run has one PATH, so
// `go env` runs once per run.
var labGoCaches = struct {
	sync.Mutex
	byPath map[string][]string
}{byPath: map[string][]string{}}

// goCacheLog prints the line that says why a Go cache is not passed.
var goCacheLog = func(line string) { fmt.Fprintln(os.Stderr, line) }

// goCaches returns GOCACHE=... and GOMODCACHE=... from `go env` of the harness process, when go is
// on PATH. These are the one exception to "nothing else is copied": a lab build then reuses the
// user's caches, and the module cache (read-only files) stays out of the lab directory. Go locks
// and verifies both caches itself. A relative value, or one under /tmp or os.TempDir(), is not
// passed (the coordinator's rule: no Go cache in /tmp). GOPATH, GOFLAGS, GOTOOLCHAIN and GOENV are
// never passed: the lab HOME stays isolated.
func goCaches() []string {
	path := os.Getenv("PATH")
	labGoCaches.Lock()
	defer labGoCaches.Unlock()
	if v, ok := labGoCaches.byPath[path]; ok {
		return v
	}
	var out []string
	defer func() { labGoCaches.byPath[path] = out }()
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, goBin, "env", "GOCACHE", "GOMODCACHE").Output()
	if err != nil {
		goCacheLog(fmt.Sprintf("lab Go caches: go env: %v; the lab uses its own caches", err))
		return nil
	}
	vals := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(vals) != 2 {
		goCacheLog(fmt.Sprintf("lab Go caches: go env printed %d lines, want 2; the lab uses its own caches", len(vals)))
		return nil
	}
	for i, k := range []string{"GOCACHE", "GOMODCACHE"} {
		v := vals[i]
		if why := goCacheRefusal(v); why != "" {
			goCacheLog(fmt.Sprintf("lab Go caches: %s %q is not passed: %s; the lab uses its own", k, v, why))
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}

// goCacheRefusal says why a Go cache value is not passed to a lab, or "".
func goCacheRefusal(v string) string {
	if v == "" || v == "off" || !filepath.IsAbs(v) {
		return "not an absolute path"
	}
	for _, root := range []string{"/tmp", os.TempDir()} {
		// Both the path and the root, as given and resolved (/tmp is /private/tmp on macOS).
		for _, r := range []string{filepath.Clean(root), realOrClean(root)} {
			for _, p := range []string{filepath.Clean(v), realOrClean(v)} {
				if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
					return "it is under " + root
				}
			}
		}
	}
	return ""
}

// realOrClean is p with its symbolic links resolved, or p when it does not exist yet.
func realOrClean(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// labEnv returns the environment of every lab process: PATH from the harness, HOME, XDG_* and
// TMPDIR under lab, REX_SERVER = the lab server, the harness's Go caches (goCaches), then extra
// (sorted). Nothing else is copied.
func labEnv(lab, server string, extra map[string]string) []string {
	home := filepath.Join(lab, "home")
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"TMPDIR=" + filepath.Join(lab, "tmp") + "/",
		"REX_SERVER=" + server,
	}
	env = append(env, goCaches()...)
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

var placeholder = regexp.MustCompile(`\$\{([^}]*)\}`)

// expandValue replaces ${LAB} and ${PLUGIN} with vars. Any other placeholder is an error.
func expandValue(v string, vars map[string]string) (string, error) {
	var bad string
	out := placeholder.ReplaceAllStringFunc(v, func(m string) string {
		name := m[2 : len(m)-1]
		if x, ok := vars[name]; ok {
			return x
		}
		if bad == "" {
			bad = m
		}
		return m
	})
	if bad != "" {
		return "", fmt.Errorf("unknown placeholder %s (use ${LAB} or ${PLUGIN})", bad)
	}
	if strings.Contains(out, "${") {
		return "", fmt.Errorf("unterminated placeholder in %q", v)
	}
	return out, nil
}

// pathLike reports a value that can name a file: it has a slash, or it starts with "." or "~".
// Such a value must pass checkPath. Other values (for example "Lab User") are plain strings.
func pathLike(v string) bool {
	return strings.Contains(v, "/") || strings.HasPrefix(v, ".") || strings.HasPrefix(v, "~")
}

// checkPath accepts an absolute path with no ".." component that resolves inside one of roots,
// or a regular executable in a PATH directory (a tool, for example /opt/homebrew/bin/git).
// Symlinks resolve on the longest existing prefix; the rest of the path is not checked again, so
// a symlink that a test creates later is outside this check.
func checkPath(p string, roots []string) error {
	if strings.HasPrefix(p, "~") {
		return fmt.Errorf("%q: ~ is not expanded; use ${LAB} or ${PLUGIN}", p)
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%q must be absolute or start with ${LAB} or ${PLUGIN}", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return fmt.Errorf("%q must not contain ..", p)
		}
	}
	if pathTool(p) {
		return nil
	}
	r, err := resolveExisting(p)
	if err != nil {
		return err
	}
	for _, root := range roots {
		if rr, err := resolveExisting(root); err == nil && within(r, rr) {
			return nil
		}
	}
	return fmt.Errorf("%q is outside the lab and plugin directories", p)
}

// resolveExisting resolves symlinks in the longest existing prefix of the absolute path p, then
// adds the rest of p. A component that exists but does not resolve (a dangling symlink, a loop, a
// permission error) is an error: only a component that does not exist ends the prefix.
func resolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	rest := ""
	for d := p; ; d = filepath.Dir(d) {
		r, err := filepath.EvalSymlinks(d)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		if _, lerr := os.Lstat(d); !os.IsNotExist(lerr) {
			return "", fmt.Errorf("%q does not resolve: %v", d, err)
		}
		if d == "/" {
			return "", fmt.Errorf("%q does not resolve: %v", p, err)
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}

// within reports p == root or p under root. "/lab-other" is not within "/lab".
func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// pathTool reports a regular executable file whose directory is a PATH entry.
func pathTool(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return false
	}
	dir := filepath.Dir(filepath.Clean(p))
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d != "" && filepath.Clean(d) == dir {
			return true
		}
	}
	return false
}

// testProfileCheck is the test-mode rule for a profile (hub config test_roots): no reserved env
// key, a path-like env value passes checkPath, and cwd passes checkPath. The command is trusted
// test code and is not checked.
func testProfileCheck(p Profile, roots []string) error {
	for k, v := range p.Env {
		if reservedEnvKey(k) {
			return fmt.Errorf("env %q is reserved in test mode", k)
		}
		if pathLike(v) {
			if err := checkPath(v, roots); err != nil {
				return fmt.Errorf("env %s: %w", k, err)
			}
		}
	}
	if p.Cwd != "" {
		if err := checkPath(p.Cwd, roots); err != nil {
			return fmt.Errorf("cwd: %w", err)
		}
	}
	return nil
}

// pluginDir is the directory of the manifest's main file: ${PLUGIN}.
func (m *manifest) pluginDir(manifestDir string) string {
	main := m.Main
	if !filepath.IsAbs(main) {
		main = filepath.Join(manifestDir, main)
	}
	return filepath.Dir(filepath.Clean(main))
}

// roots are the directories that manifest paths and test-mode profiles must stay in.
func (m *manifest) roots(manifestDir, lab string) []string {
	return []string{lab, m.pluginDir(manifestDir), filepath.Clean(manifestDir)}
}

// checkIsolation is the static part of the isolation rules, at manifest load: env keys, arg keys
// and plugin names, placeholders, and the test.profiles file. labValues checks the paths.
func (m *manifest) checkIsolation(manifestDir string) error {
	dummy := map[string]string{"LAB": "/lab", "PLUGIN": "/plugin"}
	for k, v := range m.Test.Env {
		if !envName.MatchString(k) || reservedEnvKey(k) {
			return fmt.Errorf("test.env %q is not allowed (reserved: PATH, HOME, TMPDIR, XDG_*, REX_*, GOCACHE, GOMODCACHE)", k)
		}
		if _, err := expandValue(v, dummy); err != nil {
			return fmt.Errorf("test.env %s: %w", k, err)
		}
	}
	names := map[string]bool{m.Name: true, "lazyrex": true}
	for _, d := range m.deps {
		names[d.m.Name] = true
	}
	checkArgs := func(what string, set map[string]map[string]string) error {
		for name, args := range set {
			if !names[name] {
				return fmt.Errorf("%s: %q is not the plugin, lazyrex or a test.plugins entry", what, name)
			}
			for k, v := range args {
				if !pluginArgKey.MatchString(k) || pluginArgReserved[k] {
					return fmt.Errorf("%s %s: arg %q is not allowed", what, name, k)
				}
				if _, err := expandValue(v, dummy); err != nil {
					return fmt.Errorf("%s %s.%s: %w", what, name, k, err)
				}
			}
		}
		return nil
	}
	if err := checkArgs("test.args", m.Test.Args); err != nil {
		return err
	}
	for file, f := range m.Test.Files {
		if file != filepath.Base(file) || !strings.HasSuffix(file, ".lua") {
			return fmt.Errorf("test.files: %q is not the base name of a test file (for example gallery.lua)", file)
		}
		if err := checkArgs("test.files "+file+" args", f.Args); err != nil {
			return err
		}
		for _, name := range f.Prompts.names {
			if !names[name] {
				return fmt.Errorf("test.files %s prompts: %q is not the plugin, lazyrex or a test.plugins entry", file, name)
			}
		}
	}
	for name, roots := range m.Test.WatchRoots {
		if !names[name] {
			return fmt.Errorf("test.watch_roots: %q is not the plugin, lazyrex or a test.plugins entry", name)
		}
		for _, v := range roots {
			if !strings.HasPrefix(v, "${LAB}/") {
				return fmt.Errorf("test.watch_roots %s: %q must start with ${LAB}/", name, v)
			}
			if _, err := expandValue(v, dummy); err != nil {
				return fmt.Errorf("test.watch_roots %s: %w", name, err)
			}
		}
	}
	if s := m.Test.UISlot; s != "" && s != "split" && s != "layer" {
		return fmt.Errorf("test.ui_slot is \"split\" or \"layer\", not %q", s)
	}
	for _, name := range m.Test.Prompts {
		if !names[name] {
			return fmt.Errorf("test.prompts: %q is not the plugin, lazyrex or a test.plugins entry", name)
		}
	}
	if p := m.Test.Profiles; p != "" {
		if filepath.IsAbs(p) || strings.Contains(p, "${") {
			return fmt.Errorf("test.profiles %q must be relative to the manifest dir", p)
		}
		// Reject ".." before Join: Join cleans it away, and a symlink before it would escape.
		full := filepath.Join(filepath.Clean(manifestDir), p)
		if strings.Contains("/"+p+"/", "/../") || checkPath(full, m.roots(manifestDir, "")[1:]) != nil {
			return fmt.Errorf("test.profiles %q must stay in the plugin or manifest directory", p)
		}
		if fi, err := os.Stat(full); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("test.profiles %q is not a file", p)
		}
	}
	return nil
}

// labValues are the manifest env and plugin args for one lab, expanded and checked.
type labValuesT struct {
	env        map[string]string
	args       map[string]map[string]string
	watchRoots map[string][]string
}

func (m *manifest) labValues(manifestDir, lab string, fileArgs map[string]map[string]string) (labValuesT, error) {
	vars := map[string]string{"LAB": lab, "PLUGIN": m.pluginDir(manifestDir)}
	roots := m.roots(manifestDir, lab)
	value := func(what, v string) (string, error) {
		x, err := expandValue(v, vars)
		if err == nil && pathLike(x) {
			err = checkPath(x, roots)
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", what, err)
		}
		return x, nil
	}
	out := labValuesT{env: map[string]string{}, args: map[string]map[string]string{}, watchRoots: map[string][]string{}}
	for name, roots := range m.Test.WatchRoots {
		for _, v := range roots {
			x, err := expandValue(v, vars)
			if err != nil {
				return out, fmt.Errorf("test.watch_roots %s: %w", name, err)
			}
			// The root does not exist yet (startLab creates it), so check its lab parent.
			if filepath.Clean(x) != x || !strings.HasPrefix(x, lab+"/") || strings.Contains(x, "/../") {
				return out, fmt.Errorf("test.watch_roots %s: %q must stay in the lab", name, v)
			}
			out.watchRoots[name] = append(out.watchRoots[name], x)
		}
	}
	for k, v := range m.Test.Env {
		x, err := value("test.env "+k, v)
		if err != nil {
			return out, err
		}
		out.env[k] = x
	}
	for _, set := range []map[string]map[string]string{m.Test.Args, fileArgs} {
		for name, args := range set {
			if out.args[name] == nil {
				out.args[name] = map[string]string{}
			}
			for k, v := range args {
				x, err := value("test.args "+name+"."+k, v)
				if err != nil {
					return out, err
				}
				out.args[name][k] = x
			}
		}
	}
	return out, nil
}

// checkTestSourceRuns checks the config key test_source_runs: only -test accepts it, and each dir
// must be absolute and in test_roots. It cleans each dir, as a local source dir is cleaned.
func checkTestSourceRuns(cfg *Config) error {
	if len(cfg.TestSourceRuns) == 0 {
		return nil
	}
	if !cfg.Test {
		return fmt.Errorf("test_source_runs needs -test")
	}
	for i, d := range cfg.TestSourceRuns {
		if err := checkPath(d, cfg.TestRoots); err != nil {
			return fmt.Errorf("test_source_runs: %w", err)
		}
		cfg.TestSourceRuns[i] = filepath.Clean(d)
	}
	return nil
}
