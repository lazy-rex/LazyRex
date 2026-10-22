package main

// lazyrex-hub setup [-dir DIR] [-print]: the first setup of a user's hub (docs/hub.md, "Setup").
//
// Setup proposes each file (setupfile.go) and asks for a yes on a terminal for each one. It never
// changes a file without that yes: -print and a run without a terminal write nothing, not even a
// directory. A missing prerequisite (the renderer, a running Rex) stops setup before it proposes
// anything. hub.json and profiles.json are only created, never changed.

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// setupSkill is the LazyRex skill that setup installs. It is a copy of skills/lazyrex/SKILL.md,
// because go:embed cannot reach outside the hub module: a duplicate, kept equal by
// TestSetupSkillCopy. Edit skills/lazyrex/SKILL.md and copy it here.
//
//go:embed setupfiles/SKILL.md
var setupSkill []byte

// setupPlugins are the plugins of a new hub.json: a lazy-rex repository and a directory in it.
var setupPlugins = []struct{ repo, dir string }{
	{"LazyRex", "plugins/lazyrex"}, {"Messaging", ""}, {"Profiles", ""}, {"Templates", ""}, {"Tunnels", ""}, {"Worktrees", ""},
}

// setupEnv is what setup reads from the system. Tests replace it.
type setupEnv struct {
	home     string
	getenv   func(string) string
	exe      string                              // the running lazyrex-hub
	lookPath func(string) (string, error)        // finds claude and codex
	endpoint func() (string, error)              // the address of the default Rex server
	tags     func(repo string) ([]string, error) // a lazy-rex repository's tag names
	in       *bufio.Reader                       // the answers
	tty      bool                                // in is a terminal
	out      io.Writer
	now      func() time.Time
}

func setupMain(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	dir := fs.String("dir", "", "the LazyRex directory (default ~/.lazyrex)")
	print := fs.Bool("print", false, "print the proposed files and write nothing")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		return 1
	}
	_, terr := unix.IoctlGetTermios(int(os.Stdin.Fd()), ioctlGetTermios)
	env := setupEnv{
		home: home, getenv: os.Getenv, exe: exe, lookPath: exec.LookPath,
		endpoint: rexEndpoint, tags: lazyRexTags,
		in: bufio.NewReader(os.Stdin), tty: terr == nil, out: os.Stdout, now: time.Now,
	}
	if *dir == "" {
		*dir = filepath.Join(home, ".lazyrex")
	}
	return runSetup(env, *dir, *print)
}

// rexEndpoint asks Rex for the address of the default server. It removes REX_SERVER from that
// call, so a pane's server does not count. It never starts a server.
func rexEndpoint() (string, error) {
	cmd := exec.Command("rex", "--autostart=false", "--timeout", "5s", "server", "status", "--json")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "REX_SERVER=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("rex server status: %w", err)
	}
	var s struct {
		Endpoint string `json:"endpoint"`
	}
	if json.Unmarshal(out, &s) != nil || s.Endpoint == "" {
		return "", errors.New("rex server status gave no endpoint")
	}
	return s.Endpoint, nil
}

// lazyRexTags lists a lazy-rex repository's tags with git.
func lazyRexTags(repo string) ([]string, error) {
	cmd := exec.Command("git", "ls-remote", "--tags", "--refs", "https://github.com/lazy-rex/"+repo+".git")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-remote lazy-rex/%s: %w", repo, err)
	}
	var tags []string
	for _, l := range strings.Split(string(out), "\n") {
		if _, ref, ok := strings.Cut(l, "\t"); ok {
			if t, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
				tags = append(tags, t)
			}
		}
	}
	return tags, nil
}

var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// highestTag is the highest vX.Y.Z tag, or "". Other tags do not count.
func highestTag(tags []string) string {
	best, bv := "", [3]int{-1}
	for _, t := range tags {
		m := releaseTag.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		var v [3]int
		usable := true
		for i := range v {
			n, err := strconv.Atoi(m[i+1])
			usable = usable && err == nil // a number too long for an int is not a usable tag
			v[i] = n
		}
		if usable && (v[0] > bv[0] || v[0] == bv[0] && (v[1] > bv[1] || v[1] == bv[1] && v[2] > bv[2])) {
			best, bv = t, v
		}
	}
	return best
}

// configDir is $<envVar> if it is set, else ~/<name>.
func (e setupEnv) configDir(envVar, name string) string {
	if d := e.getenv(envVar); d != "" {
		return d
	}
	return filepath.Join(e.home, name)
}

// rexConfigFile is the file that Rex reads (rex config --help): REX_CONFIG, else
// $XDG_CONFIG_HOME/rex/init.lua, else ~/.config/rex/init.lua.
func (e setupEnv) rexConfigFile() string {
	if f := e.getenv("REX_CONFIG"); f != "" {
		return f
	}
	return filepath.Join(e.configDir("XDG_CONFIG_HOME", ".config"), "rex", "init.lua")
}

// setupAgent is an agent CLI that setup configures.
type setupAgent struct {
	kind, name string
	program    string // the CLI's path, or "" if it is not on PATH
	found      bool   // the CLI is on PATH or its configuration directory exists
	hooks      string // the hook file
	skill      string // the skill file
}

func (e setupEnv) agents() []setupAgent {
	claudeDir := e.configDir("CLAUDE_CONFIG_DIR", ".claude")
	codexDir := e.configDir("CODEX_HOME", ".codex")
	as := []setupAgent{
		{kind: "claude", name: "Claude Code", hooks: filepath.Join(claudeDir, "settings.json"), skill: filepath.Join(claudeDir, "skills", "lazyrex", "SKILL.md")},
		// Codex reads user skills from ~/.agents/skills; $CODEX_HOME/skills is its deprecated
		// location (codex-rs ext/skills host_roots.rs).
		{kind: "codex", name: "Codex", hooks: filepath.Join(codexDir, "hooks.json"), skill: filepath.Join(e.home, ".agents", "skills", "lazyrex", "SKILL.md")},
	}
	for i, a := range as {
		if p, err := e.lookPath(a.kind); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				as[i].program = abs
			}
		}
		_, err := os.Stat(filepath.Dir(a.hooks))
		as[i].found = as[i].program != "" || err == nil
	}
	return as
}

// setupProfiles is a new profiles.json: one agent profile for each CLI on PATH.
func setupProfiles(e setupEnv, agents []setupAgent) ([]byte, error) {
	profiles := []Profile{}
	for _, a := range agents {
		if a.program == "" {
			continue
		}
		p := Profile{ID: a.kind, Name: map[string]string{"claude": "Claude", "codex": "Codex"}[a.kind], Kind: "agent", Cwd: e.home,
			Args: []ProfileArg{{Name: "task", Label: "Task"}}}
		if a.kind == "claude" {
			p.Command = []string{a.program, "${task}"}
			p.Resume = []string{a.program, "--resume", "${conversation_id}"}
		} else {
			// Without --no-daemon, Codex starts MCP servers outside the pane (docs/hub.md).
			p.Command = []string{a.program, "--no-daemon", "${task}"}
			p.Resume = []string{a.program, "--no-daemon", "resume", "${conversation_id}"}
		}
		if err := validProfile(p); err != nil {
			return nil, fmt.Errorf("profile %s: %w", p.ID, err)
		}
		profiles = append(profiles, p)
	}
	b, err := json.MarshalIndent(map[string]any{"profiles": profiles}, "", "  ")
	return append(b, '\n'), err
}

// setupHubConfig is a new hub.json. skipped lists the plugins that have no release tag.
func setupHubConfig(e setupEnv, dir, server string) (b []byte, skipped []string, err error) {
	type entry struct {
		Source string `json:"source"`
	}
	var plugins []entry
	var raws []json.RawMessage
	for _, p := range setupPlugins {
		tags, err := e.tags(p.repo)
		if err != nil {
			return nil, nil, err
		}
		tag := highestTag(tags)
		name := strings.TrimSuffix("lazy-rex/"+p.repo+"/"+p.dir, "/")
		if tag == "" {
			skipped = append(skipped, name)
			continue
		}
		src := entry{"github:" + name + "@" + tag}
		plugins = append(plugins, src)
		raw, _ := json.Marshal(src)
		raws = append(raws, raw)
	}
	if _, err := parseEntries(dir, raws); err != nil {
		return nil, nil, fmt.Errorf("the plugin entries: %w", err)
	}
	c := struct {
		Server     string              `json:"server"`
		State      string              `json:"state"`
		View       string              `json:"view"`
		Agents     []map[string]string `json:"agents"`
		Plugins    []entry             `json:"plugins"`
		Profiles   string              `json:"profiles"`
		PaletteLua string              `json:"palette_lua"`
		Transcript bool                `json:"transcript"`
	}{
		Server: server, State: filepath.Join(dir, "state"), View: filepath.Join(filepath.Dir(e.exe), "lazyrex-view"),
		Agents:   []map[string]string{{"id": "user", "name": "User", "status_record": ""}},
		Plugins:  plugins,
		Profiles: filepath.Join(dir, "profiles.json"), PaletteLua: filepath.Join(filepath.Dir(e.rexConfigFile()), "lazyrex_palette.lua"),
		Transcript: true,
	}
	b, err = json.MarshalIndent(c, "", "  ")
	return append(b, '\n'), skipped, err
}

// createOnly proposes content for a file that setup creates but never changes.
func createOnly(f *setupFile, content []byte) {
	if f.block != "" {
		return
	}
	if f.exists {
		f.notes = append(f.notes, "It exists. Setup does not change it.")
		return
	}
	f.next = content
}

func runSetup(e setupEnv, dir string, print bool) int {
	say := func(format string, a ...any) { fmt.Fprintf(e.out, format+"\n", a...) }
	dir, err := filepath.Abs(dir)
	if err != nil {
		say("setup: %v", err)
		return 1
	}
	// Prerequisites: each stops setup before it proposes a file.
	if strings.Contains(e.exe, string(filepath.Separator)+"go-build") {
		say("setup: %s is a temporary build (go run). Run setup from the installed lazyrex-hub: the hooks run that file.", e.exe)
		return 1
	}
	view := filepath.Join(filepath.Dir(e.exe), "lazyrex-view")
	if fi, err := os.Stat(view); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		say("setup: the renderer is missing: %s", view)
		say("  Put the lazyrex-view of the same release beside lazyrex-hub.")
		say("  From a source checkout (this needs Go): cd view && go build -o %s .", shellWord(view))
		return 1
	}
	server, err := e.endpoint()
	if err != nil {
		say("setup: no Rex server answered (%v).", err)
		say("  Start Rex, then run setup again. Setup does not start a server.")
		return 1
	}

	var files []*setupFile
	hub := snapshotFile(filepath.Join(dir, "hub.json"), 0o600)
	if !hub.exists && hub.block == "" {
		b, skipped, err := setupHubConfig(e, dir, server)
		if err != nil {
			hub.block = err.Error()
		} else {
			createOnly(hub, b)
			for _, s := range skipped {
				hub.notes = append(hub.notes, fmt.Sprintf("Plugin %s has no release tag (vX.Y.Z). Setup leaves it out.", s))
			}
		}
	} else {
		createOnly(hub, nil)
	}
	files = append(files, hub)

	agents := e.agents()
	prof := snapshotFile(filepath.Join(dir, "profiles.json"), 0o600)
	if b, err := setupProfiles(e, agents); err != nil {
		prof.block = err.Error()
	} else {
		createOnly(prof, b)
	}
	files = append(files, prof)

	var skippedAgents []string
	for _, a := range agents {
		if !a.found {
			skippedAgents = append(skippedAgents, fmt.Sprintf("%s: no %s on PATH and no %s. Setup leaves it out.", a.name, a.kind, filepath.Dir(a.hooks)))
			continue
		}
		h := snapshotFile(a.hooks, 0o644)
		hookFile(h, a.kind, shellWord(e.exe)+" hook "+a.kind)
		if a.kind == "codex" && h.changes() {
			h.notes = append(h.notes, "After setup, review and trust the new hook in Codex with /hooks.")
		}
		s := snapshotFile(a.skill, 0o644)
		if s.block == "" {
			s.next = setupSkill
		}
		files = append(files, h, s)
	}

	say("LazyRex setup: %s", dir)
	for _, s := range skippedAgents {
		say("  skipped    %s", s)
	}
	for _, f := range files {
		say("  %-10s %s", fileState(f), f.path)
		for _, n := range f.notes {
			say("             %s", n)
		}
	}
	if print || !e.tty {
		for _, f := range files {
			if f.changes() {
				say("\n%s (%s):\n%s", f.path, fileState(f), fileDiff(f.old, f.next))
			}
		}
		if print {
			say("-print: setup wrote nothing.")
			return 0
		}
		say("Standard input is not a terminal: setup wrote nothing. Run setup in a terminal to answer for each file.")
		return 2
	}

	var written, declined, blocked, failed []string
	eof := false
	for _, f := range files {
		if f.block != "" {
			blocked = append(blocked, f.path+": "+f.block)
			continue
		}
		if !f.changes() {
			continue
		}
		if eof {
			declined = append(declined, f.path+" (no answer)")
			continue
		}
		say("\n%s (%s):\n%s", f.path, fileState(f), fileDiff(f.old, f.next))
		fmt.Fprintf(e.out, "Write %s? [y/N] ", f.path)
		line, err := e.in.ReadString('\n')
		if err != nil {
			say("")
			eof = true
			declined = append(declined, f.path+" (no answer)")
			continue
		}
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			declined = append(declined, f.path)
			continue
		}
		backup, err := f.write(e.now())
		switch {
		case err != nil:
			failed = append(failed, f.path+": "+err.Error())
		case backup != "":
			written = append(written, f.path+" (backup: "+backup+")")
		default:
			written = append(written, f.path)
		}
	}
	say("")
	for _, l := range []struct {
		name  string
		items []string
	}{{"Written", written}, {"Declined", declined}, {"Blocked", blocked}, {"Failed", failed}} {
		for _, it := range l.items {
			say("%-9s %s", l.name+":", it)
		}
	}
	if len(written)+len(declined)+len(blocked)+len(failed) == 0 {
		say("Nothing to write.")
	}
	say("\nNext: run `rex skill setup` to install Rex's own skill and hooks. Setup does not run it.")
	if len(failed) > 0 {
		return 1
	}
	return 0
}

func fileState(f *setupFile) string {
	switch {
	case f.block != "":
		return "blocked: " + f.block
	case !f.changes():
		if f.exists {
			return "unchanged"
		}
		return "not written"
	case f.exists:
		return "change"
	default:
		return "new"
	}
}
