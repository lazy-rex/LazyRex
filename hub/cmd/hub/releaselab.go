package main

// The lab of a rehearsal (docs/releasing.md; plan v2 A1 and A2, c3 design v2): a new directory
// under /tmp, and the lab copies of the live config, managed file, lock, base and profiles. The lab
// files come from the bytes that rehearse read once from the live files. Each key that names a live
// path gets a lab path, then a scan refuses any string value that is a path outside the lab.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	relLabPrefix = "rexlab-release-"
	relLabConfig = "hub-live.json" // the lab config; its lock is hub-live.lock.json beside it
	relLabState  = "hub"           // the lab hub state (<lab>/state is the state of the lab Rex server)
)

// relRewrite is one key of a live file that the lab copy changes.
type relRewrite struct {
	Key  string `json:"key"`
	Live string `json:"live"`
	Lab  string `json:"lab"`
}

// relLiveFiles are the bytes of the live inputs, read once at the start of a rehearsal.
type relLiveFiles struct {
	Config, Managed, Lock, Base, Profiles []byte
}

// createLab makes the lab directory (plan v2 A1): a new directory named rexlab-release-<name>
// directly under /tmp, mode 0700, owned by this user, not a symlink. It writes owner.json. It
// returns the resolved path.
func createLab(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", relFail("the lab %s must be an absolute clean path", path)
	}
	parent, name := filepath.Dir(path), filepath.Base(path)
	if parent != "/tmp" && parent != "/private/tmp" {
		return "", relFail("the lab %s must be directly under /tmp", path)
	}
	if !strings.HasPrefix(name, relLabPrefix) || name == relLabPrefix {
		return "", relFail("the lab name must start with %s", relLabPrefix)
	}
	tmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", relFail("the lab: %v (the lab must not exist)", err)
	}
	lab := filepath.Join(tmp, name)
	fi, err := os.Lstat(lab)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || !ok || int(st.Uid) != os.Getuid() {
		return "", relFail("the lab %s is not a directory of this user", lab)
	}
	owner, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "start": startTime(os.Getpid()), "created": time.Now().UTC().Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(lab, "owner.json"), owner, 0o600); err != nil {
		return "", err
	}
	return lab, nil
}

// configKeys are the json keys of the hub config (Config). A live config key outside this list
// refuses the rehearsal: the tool cannot know whether it names a live path.
func configKeys() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if tag != "" && tag != "-" {
			keys[tag] = true
		}
	}
	return keys
}

// labFiles writes the lab copies of the live inputs into lab and returns the lab config path and
// the rewritten keys. labArgs gives the lab value of a plugin arg that names a path, relative to the
// lab. Then scanLab checks every string value.
func labFiles(live relLiveFiles, lab string, labArgs map[string]map[string]string) (string, []relRewrite, error) {
	var out []relRewrite
	set := func(m map[string]any, key, field, value string) {
		if v, ok := m[field].(string); ok {
			out = append(out, relRewrite{Key: key, Live: v, Lab: value})
		}
		m[field] = value
	}
	cfg := map[string]any{}
	if err := json.Unmarshal(live.Config, &cfg); err != nil {
		return "", nil, relFail("the live config: %v", err)
	}
	known := configKeys()
	for k := range cfg {
		if !known[k] {
			return "", nil, relFail("the live config has the key %q, which the rehearsal does not know", k)
		}
	}
	for _, k := range []string{"test_roots", "test_hold_recovery"} {
		if _, ok := cfg[k]; ok {
			return "", nil, relFail("the live config has the test-mode key %q", k)
		}
	}
	cfgPath := filepath.Join(lab, relLabConfig)
	state := filepath.Join(lab, relLabState)
	set(cfg, "config.server", "server", "unix://"+filepath.Join(lab, "rex.sock"))
	set(cfg, "config.state", "state", state)
	for _, f := range []struct{ field, lab string }{{"view", filepath.Join(lab, "view")}, {"profiles", filepath.Join(lab, "home", "profiles.json")},
		{"palette_lua", filepath.Join(lab, "home", "lazyrex_palette.lua")}, {"lock", strings.TrimSuffix(cfgPath, ".json") + ".lock.json"},
		{"managed_plugins", filepath.Join(state, "plugins.json")}} {
		if _, ok := cfg[f.field]; ok {
			set(cfg, "config."+f.field, f.field, f.lab)
		}
	}
	lockField, _ := cfg["lock"].(string)
	lock := lockPath(cfgPath, lockField)
	managed := filepath.Join(state, "plugins.json")
	if v, ok := cfg["managed_plugins"].(string); ok {
		managed = v
	}

	var m struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(live.Managed, &m); err != nil {
		return "", nil, relFail("the live managed file: %v", err)
	}
	inbox := 0
	for _, p := range m.Plugins {
		id, _ := p["id"].(string)
		if roots, ok := p["watch_roots"].([]any); ok {
			for i, v := range roots {
				inbox++
				d := filepath.Join(lab, "inbox", fmt.Sprint(inbox))
				if err := os.MkdirAll(d, 0o700); err != nil {
					return "", nil, err
				}
				s, _ := v.(string)
				out = append(out, relRewrite{Key: fmt.Sprintf("managed.%s.watch_roots[%d]", id, i), Live: s, Lab: d})
				roots[i] = d
			}
		}
		args, _ := p["args"].(map[string]any)
		for k, v := range args {
			s, _ := v.(string)
			if rel, ok := labArgs[id][k]; ok {
				if filepath.IsAbs(rel) || filepath.Clean(rel) != rel || strings.HasPrefix(rel, "..") {
					return "", nil, relFail("lab_args %s.%s must be a clean path relative to the lab", id, k)
				}
				out = append(out, relRewrite{Key: fmt.Sprintf("managed.%s.args.%s", id, k), Live: s, Lab: filepath.Join(lab, rel)})
				args[k] = filepath.Join(lab, rel)
			} else if pathLike(s) {
				return "", nil, relFail("the plugin %s arg %s names a path; the spec needs lab_args for it", id, k)
			}
		}
	}

	var profiles []byte
	if live.Profiles != nil {
		var pf map[string]any
		if err := json.Unmarshal(live.Profiles, &pf); err != nil {
			return "", nil, relFail("the live profiles: %v", err)
		}
		list, _ := pf["profiles"].([]any)
		for _, x := range list {
			p, _ := x.(map[string]any)
			if _, ok := p["cwd"]; ok {
				id, _ := p["id"].(string)
				set(p, "profiles."+id+".cwd", "cwd", filepath.Join(lab, "home"))
			}
		}
		profiles, _ = json.MarshalIndent(pf, "", "  ")
	}

	cb, _ := json.MarshalIndent(cfg, "", "  ")
	mb, _ := json.MarshalIndent(m, "", "  ")
	type labWrite struct {
		path string
		b    []byte
	}
	writes := []labWrite{{cfgPath, cb}, {managed, mb}, {lock, live.Lock}, {peersPath(lock), live.Base}}
	if p, ok := cfg["profiles"].(string); ok && profiles != nil {
		writes = append(writes, labWrite{p, profiles})
	}
	for _, w := range writes {
		if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
			return "", nil, err
		}
		if err := os.WriteFile(w.path, w.b, 0o600); err != nil {
			return "", nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	scan := []string{cfgPath, managed}
	if p, ok := cfg["profiles"].(string); ok && profiles != nil {
		scan = append(scan, p)
	}
	return cfgPath, out, scanLab(lab, scan...)
}

// scanLab checks every string value of the lab JSON files: a value that is a path (pathLike) must
// pass checkPath with the lab as the only root (inside the lab, or an executable in a PATH
// directory). A unix:// URL is checked by its path. A GitHub source string is not a path.
func scanLab(lab string, files ...string) error {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return relFail("%s: %v", f, err)
		}
		var walk func(key string, v any) error
		walk = func(key string, v any) error {
			switch x := v.(type) {
			case map[string]any:
				for k, y := range x {
					if err := walk(key+"."+k, y); err != nil {
						return err
					}
				}
			case []any:
				for i, y := range x {
					if err := walk(fmt.Sprintf("%s[%d]", key, i), y); err != nil {
						return err
					}
				}
			case string:
				x = strings.TrimPrefix(x, "unix://") // a socket URL names a path
				if pathLike(x) && !strings.HasPrefix(x, "github:") {
					if err := checkPath(x, []string{lab}); err != nil {
						return relFail("the lab file %s, key %s: %v", f, strings.TrimPrefix(key, "."), err)
					}
				}
			}
			return nil
		}
		if err := walk("", v); err != nil {
			return err
		}
	}
	return nil
}

// labMirrors makes the GitHub mirrors of a lab (coordinator decision 2026-10-22, the method of the
// README lab): no GitHub credential goes into a lab process. This process clones each repo with its
// own git access into <lab>/mirrors/<owner>/<repo>.git (and <repo>, a link, for a URL without .git).
// The lab git config maps https://github.com/<owner>/ and git@github.com:<owner>/ to the mirrors,
// and the lab environment uses only that config, never prompts, and allows only the file transport
// (GIT_ALLOW_PROTOCOL=file): a lab child that reaches for the network fails at once. base is the
// URL prefix of the real repos (https://github.com/; a test uses local repos). It returns the lab environment additions and the HEAD of each mirror.
func labMirrors(lab, base string, repos [][2]string) ([]string, map[string]string, error) {
	root := filepath.Join(lab, "mirrors")
	heads := map[string]string{}
	owners := map[string]bool{}
	for _, or := range repos {
		owner, repo := or[0], or[1]
		if heads[owner+"/"+repo] != "" {
			continue
		}
		dir := filepath.Join(root, owner, repo+".git")
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return nil, nil, err
		}
		c := exec.Command("git", "clone", "-q", "--mirror", base+owner+"/"+repo+".git", dir)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := c.CombinedOutput(); err != nil {
			return nil, nil, relFail("the mirror of %s/%s: %v: %s", owner, repo, err, strings.TrimSpace(string(out)))
		}
		if err := os.Symlink(repo+".git", filepath.Join(root, owner, repo)); err != nil {
			return nil, nil, err
		}
		out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil {
			return nil, nil, relFail("the mirror of %s/%s: %v", owner, repo, err)
		}
		heads[owner+"/"+repo] = strings.TrimSpace(string(out))
		owners[owner] = true
	}
	var cfg strings.Builder
	var names []string
	for o := range owners {
		names = append(names, o)
	}
	sort.Strings(names)
	for _, o := range names {
		fmt.Fprintf(&cfg, "[url \"file://%s/\"]\n\tinsteadOf = https://github.com/%s/\n\tinsteadOf = git@github.com:%s/\n", filepath.Join(root, o), o, o)
	}
	// Any other GitHub repo maps to a path that does not exist (git takes the longest insteadOf
	// match): an unexpected remote fails in the lab, also a public one.
	fmt.Fprintf(&cfg, "[url \"file://%s/\"]\n\tinsteadOf = https://github.com/\n\tinsteadOf = git@github.com:\n", filepath.Join(root, "no-such-mirror"))
	gitcfg := filepath.Join(lab, "gitconfig")
	if err := os.WriteFile(gitcfg, []byte(cfg.String()), 0o600); err != nil {
		return nil, nil, err
	}
	return []string{"GIT_CONFIG_GLOBAL=" + gitcfg, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file"}, heads, nil
}

// lockRepos lists the GitHub repos of a lock, and LazyRex.
func lockRepos(lock []byte) ([][2]string, error) {
	var l pluginLock
	if err := json.Unmarshal(lock, &l); err != nil {
		return nil, relFail("the live lock: %v", err)
	}
	out := [][2]string{{"lazy-rex", "LazyRex"}}
	var srcs []string
	for s := range l.Plugins {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	for _, s := range srcs {
		e := l.Plugins[s]
		out = append(out, [2]string{e.Owner, e.Repo})
	}
	return out, nil
}

// needCommits checks that each commit is in its mirror: a missing commit refuses the rehearsal; it
// never falls back to the network. commits maps owner/repo to commits.
func needCommits(lab string, commits map[string][]string) error {
	for or, list := range commits {
		dir := filepath.Join(lab, "mirrors", or+".git")
		for _, c := range list {
			if err := exec.Command("git", "-C", dir, "cat-file", "-e", c+"^{commit}").Run(); err != nil {
				return relFail("the mirror of %s has no commit %s", or, c)
			}
		}
	}
	return nil
}
