package main

// lazyrex-hub setup [-dir DIR] [-print]: the first setup of a user's hub (docs/hub.md, "Setup").
//
// Setup proposes each file (setupfile.go) and asks for a yes on a terminal for each one. It never
// changes a file without that yes: -print and a run without a terminal write nothing, not even a
// directory. A missing prerequisite (the renderer, a running Rex) stops setup before it proposes
// anything. hub.json and profiles.json are only created, never changed.

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// setupPlugins are the plugins of a new managed file: a managed id, a lazy-rex repository and a
// directory in it.
var setupPlugins = []struct{ id, repo, dir string }{
	{"lazyrex", "LazyRex", "plugins/lazyrex"}, {"messaging", "Messaging", ""}, {"profiles", "Profiles", ""},
	{"templates", "Templates", ""}, {"tunnels", "Tunnels", ""}, {"worktrees", "Worktrees", ""},
}

// setupEnv is what setup reads from the system. Tests replace it.
type setupEnv struct {
	home     string
	getenv   func(string) string
	exe      string                              // the running lazyrex-hub
	lookPath func(string) (string, error)        // finds claude and codex
	server   string                              // -server: a Rex server address; "" is the default server
	endpoint func(server string) (string, error) // the server's own address, from Rex
	tags     func(repo string) ([]string, error) // a lazy-rex repository's tag names
	isReal   func(server string) bool            // isRealServer: the hub controls need -real-server
	// skillStatus is the output of `rex skill status --json` (it changes nothing); skillSetup runs
	// `rex skill setup` with args and prints its output to w.
	skillStatus func() ([]byte, error)
	skillSetup  func(args []string, w io.Writer) error
	in          *bufio.Reader // the answers
	tty         bool          // in is a terminal
	out         io.Writer
	now         func() time.Time
	autostart   *setupAutostart // -autostart, -autostart-cmd; nil: no autostart
}

// setupAutostart is the autostart of the boot file: argv in a pane labeled pane, in the session
// labeled session. A nil argv is the Start Hub command.
type setupAutostart struct {
	argv          []string
	session, pane string
}

func setupMain(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	dir := fs.String("dir", "", "the LazyRex directory (default ~/.lazyrex)")
	print := fs.Bool("print", false, "print the proposed files and write nothing")
	server := fs.String("server", "", "the Rex server for hub.json (default: the server that Rex reports as its default)")
	autostart := fs.Bool("autostart", false, "start the hub at each Rex server start (lazyrex_boot.lua)")
	autostartCmd := fs.String("autostart-cmd", "", "the autostart command, a JSON array of strings (default: the Start Hub command); implies -autostart")
	autostartSession := fs.String("autostart-session", "lazyrex-hub", "the label of the autostart session")
	autostartPane := fs.String("autostart-pane", startHubPane, "the label of the autostart pane")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	var auto *setupAutostart
	if *autostart || *autostartCmd != "" {
		auto = &setupAutostart{session: *autostartSession, pane: *autostartPane}
		if *autostartCmd != "" && (json.Unmarshal([]byte(*autostartCmd), &auto.argv) != nil || len(auto.argv) == 0) {
			fmt.Fprintln(os.Stderr, `setup: -autostart-cmd must be a JSON array of strings, such as '["/path/dev.sh", "/path/hub.json"]'`)
			return 2
		}
		if auto.session == "" || auto.pane == "" {
			fmt.Fprintln(os.Stderr, "setup: -autostart-session and -autostart-pane must not be empty")
			return 2
		}
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
		server: *server, endpoint: rexEndpoint, tags: lazyRexTags, isReal: func(s string) bool { return isRealServer(context.Background(), s) },
		skillStatus: rexSkillStatus, skillSetup: rexSkillSetup,
		in: bufio.NewReader(os.Stdin), tty: terr == nil, out: os.Stdout, now: time.Now, autostart: auto,
	}
	if *dir == "" {
		*dir = filepath.Join(home, ".lazyrex")
	}
	return runSetup(env, *dir, *print)
}

// rexEndpoint asks Rex for a server's own address: the server -S names, or without one the
// default server. It removes REX_SERVER from that call, so a pane's server does not count. It
// never starts a server.
func rexEndpoint(server string) (string, error) {
	args := []string{"--autostart=false", "--timeout", "5s", "server", "status", "--json"}
	if server != "" {
		args = append([]string{"-S", server}, args...)
	}
	cmd := exec.Command("rex", args...)
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

// rexSkillStatus is `rex skill status --json`: it reads the agents' files and changes nothing.
func rexSkillStatus() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "rex", "--autostart=false", "skill", "status", "--json").Output()
}

// rexSkillSetup runs `rex skill setup` with args. It never adds --force: Rex refuses to replace
// a skill named rex that it did not install, and setup keeps that refusal.
func rexSkillSetup(args []string, w io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rex", append([]string{"--autostart=false"}, args...)...)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// skillSetupArgs are the arguments of `rex skill setup` for exactly the agents that setup found
// (kinds): a bare `rex skill setup` installs into every agent that Rex finds. No agent, no args.
func skillSetupArgs(agents []setupAgent) (args, kinds []string) {
	for _, a := range agents {
		if a.found {
			kinds = append(kinds, a.kind)
			args = append(args, "--agent", a.kind)
		}
	}
	if kinds == nil {
		return nil, nil
	}
	return append([]string{"skill", "setup"}, args...), kinds
}

// rexSkillCurrent reports whether `rex skill status --json` (b, err) says that Rex's skill and
// hook are both "current" in each agent of kinds. A failed, malformed or incomplete status is not.
func rexSkillCurrent(b []byte, err error, kinds []string) bool {
	var st struct {
		Harnesses []struct {
			ID    string `json:"id"`
			Items []struct {
				Kind  string `json:"kind"`
				State string `json:"state"`
			} `json:"items"`
		} `json:"harnesses"`
	}
	if err != nil || json.Unmarshal(b, &st) != nil || len(kinds) == 0 {
		return false
	}
	for _, k := range kinds {
		current := map[string]bool{}
		for _, h := range st.Harnesses {
			if h.ID != k {
				continue
			}
			for _, it := range h.Items {
				if it.State == "current" {
					current[it.Kind] = true
				}
			}
		}
		if !current["skill"] || !current["hook"] {
			return false
		}
	}
	return true
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

// highestTag is the highest vX.Y.Z tag, or "". Other tags do not count. It uses the parser and
// the order of the hub's @latest (pluginlatest.go), so setup and the hub choose the same tag.
func highestTag(tags []string) string {
	best, bv := "", tagVersion{}
	for _, t := range tags {
		v, ok := parseVersionTag(t)
		if ok && (best == "" || compareVersions(v, bv) > 0) {
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
	*harness
	program string // the CLI's path, or "" if it is not on PATH
	found   bool   // the CLI is on PATH or its configuration directory exists
	hooks   string // the hook file
	skill   string // the skill file
}

func (e setupEnv) agents() []setupAgent {
	var as []setupAgent
	for _, h := range harnesses {
		dir := e.configDir(h.configEnv, h.configDir)
		as = append(as, setupAgent{harness: h, hooks: filepath.Join(dir, h.hooksFile), skill: h.skill(e.home, dir)})
	}
	for i, a := range as {
		if p, err := e.lookPath(a.exe); err == nil {
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
		p := Profile{ID: a.kind, Name: a.harness.name, Kind: "agent", Cwd: e.home,
			Args: []ProfileArg{{Name: "task", Label: "Task"}}, Command: a.command(a.program), Resume: a.resume(a.program)}
		if err := validProfile(p); err != nil {
			return nil, fmt.Errorf("profile %s: %w", p.ID, err)
		}
		profiles = append(profiles, p)
	}
	b, err := json.MarshalIndent(map[string]any{"profiles": profiles}, "", "  ")
	return append(b, '\n'), err
}

// setupHubConfig is a new hub.json. It has no plugins: the plugins of a new hub are in the
// managed file (setupManaged), because a GitHub plugin in hub.json is refused at load.
func setupHubConfig(e setupEnv, dir, server string) ([]byte, error) {
	c := struct {
		Server     string `json:"server"`
		State      string `json:"state"`
		View       string `json:"view"`
		Profiles   string `json:"profiles"`
		PaletteLua string `json:"palette_lua"`
	}{
		Server: server, State: filepath.Join(dir, "state"), View: filepath.Join(filepath.Dir(e.exe), "lazyrex-view"),
		Profiles: filepath.Join(dir, "profiles.json"), PaletteLua: filepath.Join(filepath.Dir(e.rexConfigFile()), "lazyrex_palette.lua"),
	}
	b, err := json.MarshalIndent(c, "", "  ")
	return append(b, '\n'), err
}

// setupManaged is a new managed file (encodeManaged, the format of the plugin manager): the
// plugins of setupPlugins, each at its repository's highest vX.Y.Z tag. skipped lists the plugins
// that have no release tag. The hub installs the entries at its first start (the install op).
func setupManaged(e setupEnv) (b []byte, skipped []string, err error) {
	var plugins []managedFileEntry
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
		plugins = append(plugins, managedFileEntry{ID: p.id, Source: "github:" + name + "@" + tag, Args: map[string]string{}, WatchRoots: []string{}})
	}
	b, err = encodeManaged(plugins)
	if err != nil {
		return nil, nil, fmt.Errorf("the managed file: %w", err)
	}
	return b, skipped, nil
}

// hubControls is what the boot file's hub controls (Start Hub, Stop Hub, Hub Status) run with:
// the hub.json that they read, and whether its server is the real Rex server. err: setup cannot
// tell, so the boot file has no hub controls.
type hubControls struct {
	config string
	server string
	real   bool
	err    string
}

// configServer is the server of an existing hub.json, or why setup cannot read it.
func configServer(b []byte) (string, string) {
	var c struct {
		Server string `json:"server"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", "hub.json cannot be read: " + err.Error()
	}
	if c.Server == "" {
		return "", "hub.json names no server"
	}
	return c.Server, ""
}

// setupBootLua is lazyrex_boot.lua: the palette entries that work without a running hub. Setup
// owns the file and replaces it. Each entry opens a new window in the caller's session with one
// pane that runs an argv (no shell), and keeps the pane open after the command ends, so the user
// can read its output. Set Up runs the same setup command (the binary, -dir, and -server if it was
// given). Start Hub runs the hub under the restart wrapper: that pane owns the wrapper. Stop Hub
// and Hub Status run lazyrex-hub stop and status. Without ctl (ctl.err), only Set Up is written.
// With auto, Set Up keeps the autostart flags, and the file ends with the autostart (setupAutostartLua).
func setupBootLua(exe, dir, server string, ctl hubControls, auto *setupAutostart) string {
	argv := luaString(exe) + `, "setup", "-dir", ` + luaString(dir)
	if server != "" {
		argv += `, "-server", ` + luaString(server)
	}
	if auto != nil {
		argv += `, "-autostart"`
		if auto.argv != nil {
			b, _ := json.Marshal(auto.argv)
			argv += `, "-autostart-cmd", ` + luaString(string(b))
		}
		argv += `, "-autostart-session", ` + luaString(auto.session) + `, "-autostart-pane", ` + luaString(auto.pane)
	}
	s := `-- Written by lazyrex-hub setup. Setup replaces this file: do not edit it.
-- init.lua loads it with: pcall(require, "lazyrex_boot")
local function lazyrex_pane(title, label, argv)
  return function(ctx, _)
    if not ctx.session_id then error("run " .. title .. " from a Rex window", 0) end
    local r, err = rex.call("session.new_window", { session_id = ctx.session_id, focus = true,
      layout = { block = { flavor = "com.superlogical.terminal.shell", label = label,
        options = { command = argv, login = false, exit = { on_completion = false } } } } })
    if err ~= nil then error(err, 0) end
    return r
  end
end
rex.action {
  name = "lazyrexboot_setup",
  title = "LazyRex: Set Up",
  category = "LazyRex",
  description = "Run lazyrex-hub setup in a new tab. It asks before it writes each file.",
  run = lazyrex_pane("LazyRex: Set Up", "LazyRex setup", { ` + argv + ` }),
}
`
	if ctl.err != "" {
		return s
	}
	cfg := `"-config", ` + luaString(ctl.config)
	if ctl.real {
		cfg += `, "-real-server"`
	}
	for _, a := range []struct{ name, title, label, desc, argv string }{
		{"lazyrexboot_start", "LazyRex: Start Hub", "LazyRex hub",
			"Start the LazyRex hub in a new tab. Closing that tab stops the hub.", luaString(exe) + `, ` + cfg + `, "-restart"`},
		{"lazyrexboot_stop", "LazyRex: Stop Hub", "LazyRex stop",
			"Stop the LazyRex hub, and show the result in a new tab.", luaString(exe) + `, "stop", ` + cfg},
		{"lazyrexboot_status", "LazyRex: Hub Status", "LazyRex status",
			"Show whether the LazyRex hub runs, in a new tab.", luaString(exe) + `, "status", ` + cfg},
	} {
		s += `rex.action {
  name = "` + a.name + `",
  title = "` + a.title + `",
  category = "LazyRex",
  description = "` + a.desc + `",
  run = lazyrex_pane("` + a.title + `", "` + a.label + `", { ` + a.argv + ` }),
}
`
	}
	if auto != nil {
		s += setupAutostartLua(exe, cfg, auto)
	}
	return s
}

// setupAutostartLua runs lazyrex-hub autostart at each load of the config: the server start and
// each config reload. The load cannot call the server ("no client in scope"), so a detached
// command does the checks and the start (docs/rex-protocol.md, "The config file at server start").
// cfg is the -config argument (and -real-server) of the hub controls.
func setupAutostartLua(exe, cfg string, auto *setupAutostart) string {
	argv := []string{luaString(exe), cfg, `"-restart"`} // the Start Hub command
	if auto.argv != nil {
		argv = argv[:0]
		for _, a := range auto.argv {
			argv = append(argv, luaString(a))
		}
	}
	return `-- Autostart (lazyrex-hub setup -autostart): start the hub when no hub runs and no hub pane exists.
pcall(rex.exec.start, { ` + luaString(exe) + `, "autostart", ` + cfg + `, "-session", ` + luaString(auto.session) +
		`, "-pane", ` + luaString(auto.pane) + `, "--", ` + strings.Join(argv, ", ") + ` }, { detach = true, timeout = 120 })
`
}

// setupManagedExists is the row of a managed file that exists (lead-c2h): setup never merges or
// replaces the user's own entries.
const setupManagedExists = "The managed file exists: setup leaves it; add plugins in LazyRex: Plugins…"

// setupManagedFile proposes the managed file at the path that the effective hub.json gives
// (managedFilePath, as the hub's load): the existing hub.json, or the new one with the state
// <dir>/state. An existing managed file is left as it is.
func setupManagedFile(e setupEnv, dir string, hub *setupFile) *setupFile {
	c := Config{State: filepath.Join(dir, "state")} // the new hub.json
	if hub.exists {
		c = Config{} // the existing file alone: a missing state is not the default (codex-27)
		// The user's own hub.json that the hub cannot load either: setup leaves the managed file out
		// and says why. It is not an error of setup.
		if err := json.Unmarshal(hub.old, &c); err != nil || c.State == "" {
			f := &setupFile{path: filepath.Join(dir, "state", "plugins.json")}
			f.notes = append(f.notes, "hub.json names no state: setup cannot tell the managed file and leaves it out.")
			if err != nil {
				f.notes[0] = "hub.json cannot be read: setup cannot tell the managed file and leaves it out."
			}
			return f
		}
		// A relative state is relative to the hub's working directory, not to hub.json (lead-c2h5):
		// setup cannot tell that directory.
		if c.ManagedPlugins == "" && !filepath.IsAbs(c.State) {
			f := &setupFile{path: filepath.Join(dir, "state", "plugins.json")}
			f.notes = append(f.notes, "hub.json has a relative state: setup cannot tell the managed file and leaves it out.")
			return f
		}
	}
	f := snapshotFile(managedFilePath(c, hub.path), 0o600)
	switch {
	case f.block != "":
	case f.exists:
		f.notes = append(f.notes, setupManagedExists)
	default:
		b, skipped, err := setupManaged(e)
		if err != nil {
			f.block = err.Error()
			break
		}
		f.next = b
		for _, s := range skipped {
			f.notes = append(f.notes, fmt.Sprintf("Plugin %s has no release tag (vX.Y.Z). Setup leaves it out.", s))
		}
	}
	return f
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
	server, err := e.endpoint(e.server)
	if err != nil {
		// The user text names the command that shows the cause, not the internal error: setup has
		// no log (the onboarding dry run, 2026-10-22).
		status := "env -u REX_SERVER rex server status" // the default server, as e.endpoint asks it (no REX_SERVER)
		if e.server != "" {
			status = "rex -S " + shellWord(e.server) + " server status"
		}
		say("setup: no Rex server answered.")
		say("  Start Rex, then run setup again. Setup does not start a server. To see the cause: %s", status)
		return 1
	}

	var files []*setupFile
	hub := snapshotFile(filepath.Join(dir, "hub.json"), 0o600)
	if !hub.exists && hub.block == "" {
		b, err := setupHubConfig(e, dir, server)
		if err != nil {
			hub.block = err.Error()
		} else {
			createOnly(hub, b)
		}
	} else {
		createOnly(hub, nil)
	}
	managed := setupManagedFile(e, dir, hub)
	files = append(files, hub, managed)
	// The hub controls of the boot file read this hub.json. -real-server must match its server
	// exactly, so it comes from the file that they read: the proposed one, or the existing one,
	// which setup never changes (its server can differ from the one that setup found).
	ctl := hubControls{config: hub.path}
	switch {
	case hub.block != "":
		ctl.err = "hub.json is blocked: " + hub.block
	case hub.exists:
		ctl.server, ctl.err = configServer(hub.old)
	default:
		ctl.server = server
	}
	if ctl.err == "" {
		ctl.real = e.isReal(ctl.server)
	}

	agents := e.agents()
	prof := snapshotFile(filepath.Join(dir, "profiles.json"), 0o600)
	if b, err := setupProfiles(e, agents); err != nil {
		prof.block = err.Error()
	} else {
		createOnly(prof, b)
	}
	files = append(files, prof)

	// The Rex config: the boot palette file beside it, and the lines that load the boot file and the
	// hub's generated palette file (lazyrex_palette.lua).
	rexDir := filepath.Dir(e.rexConfigFile())
	boot := snapshotFile(filepath.Join(rexDir, "lazyrex_boot.lua"), 0o644)
	if boot.block == "" {
		boot.next = []byte(setupBootLua(e.exe, dir, e.server, ctl, e.autostart))
		if ctl.err != "" {
			boot.notes = append(boot.notes, "Start Hub, Stop Hub and Hub Status are left out: "+ctl.err)
			if e.autostart != nil {
				boot.notes = append(boot.notes, "The autostart is left out: "+ctl.err)
			}
		}
	}
	init := snapshotFile(e.rexConfigFile(), 0o644)
	if init.block == "" {
		init.next = appendLines(init.old, `pcall(require, "lazyrex_boot")`, `pcall(require, "lazyrex_palette")`)
	}
	files = append(files, boot, init)

	var skippedAgents []string
	for _, a := range agents {
		if !a.found {
			skippedAgents = append(skippedAgents, fmt.Sprintf("%s: no %s on PATH and no %s. Setup leaves it out.", a.product, a.exe, filepath.Dir(a.hooks)))
			continue
		}
		h := snapshotFile(a.hooks, 0o644)
		hookFile(h, a.kind, shellWord(e.exe)+" hook "+a.kind)
		if a.trustHook && h.changes() {
			h.notes = append(h.notes, "After setup, review and trust the new hook in "+a.harness.name+" with /hooks.")
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
	skillArgs, skillKinds := skillSetupArgs(agents)
	skillCmd := ""
	if skillArgs != nil {
		skillCmd = "rex " + strings.Join(skillArgs, " ")
	}
	if print || !e.tty {
		for _, f := range files {
			if f.changes() {
				say("\n%s (%s):\n%s", f.path, fileState(f), fileDiff(f.old, f.next))
			}
		}
		if skillCmd != "" {
			say("\nRex's own skill and hooks: setup offers `%s` in a terminal.", skillCmd)
		}
		if print {
			say("-print: setup wrote nothing.")
			for _, f := range files {
				if f.block != "" {
					return 1 // a blocked file is an error, as in a run that writes
				}
			}
			return 0
		}
		say("Standard input is not a terminal: setup wrote nothing. Run setup in a terminal to answer for each file.")
		return 2
	}

	var written, declined, blocked, failed []string
	rexWritten := false // the boot file or the Rex config was written
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
		rexWritten = rexWritten || err == nil && (f == boot || f == init)
		switch {
		case f == managed && errors.Is(err, errAppeared):
			// A managed file that another writer made after the snapshot: the user's own entries.
			say("%s: %s", f.path, setupManagedExists)
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
	// Rex's own skill and hooks, for exactly the agents that setup found (the user, 2026-10-22).
	// Only a status that says "current" for the skill and the hook of each one counts as installed.
	skillFailed := false
	skillCurrent := func() bool {
		b, err := e.skillStatus()
		return rexSkillCurrent(b, err, skillKinds)
	}
	names := strings.Join(skillKinds, ", ")
	switch {
	case skillCmd == "":
	case skillCurrent():
		say("\nRex's skill and hooks: installed for %s.", names)
		skillCmd = ""
	case eof:
	default:
		fmt.Fprintf(e.out, "\nInstall Rex's own skill and hooks for %s now (%s)? [y/N] ", names, skillCmd)
		line, err := e.in.ReadString('\n')
		if err != nil {
			say("")
		}
		if a := strings.ToLower(strings.TrimSpace(line)); err == nil && (a == "y" || a == "yes") {
			switch err := e.skillSetup(skillArgs, e.out); {
			case err != nil:
				say("Rex skill setup failed: %v", err)
				skillFailed = true
			case !skillCurrent():
				say("Rex skill setup finished, but rex skill status does not report the skill and hooks as current for %s.", names)
				skillFailed = true
			default:
				say("Rex's skill and hooks: installed for %s.", names)
				skillCmd = ""
			}
		}
	}
	if skillCmd != "" {
		say("\nInstall Rex's own skill and hooks: %s", skillCmd)
	}
	if rexWritten {
		// The server that setup configured: a bare `rex config reload` would reach the caller's.
		say("Load the LazyRex palette entries: rex -S %s config reload", shellWord(server))
	}
	// Remote Control is a setting of the Rex app. Setup does not ask the app (no probe): the hub
	// records what the app does with its actions (remotecontrol.go).
	say("\nRemote Control: LazyRex asks the Rex app for Go to Session and Insert Path. The app runs them only " +
		"with Remote Control on (Rex app: Settings, Rex Server, Remote Control). Hub Status shows what the hub last observed.")
	// A blocked file, a failed write or a failed skill setup is an error; a declined file or skill
	// setup is the user's choice.
	if len(failed) > 0 || len(blocked) > 0 || skillFailed {
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
