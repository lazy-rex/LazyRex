package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const setupLabServer = "unix:///tmp/rexlab-setup-test/rex.sock"

// setupTestEnv is a setup environment in a temporary HOME: an installed lazyrex-hub and
// lazyrex-view, claude and codex on PATH, a lab Rex endpoint, and a release tag on each plugin.
func setupTestEnv(t *testing.T, answers string, tty bool) (setupEnv, *strings.Builder) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	home = filepath.Join(home, "the home") // a space in every path
	bin := filepath.Join(home, ".lazyrex", "bin")
	tools := filepath.Join(home, "tools")
	for _, p := range []string{filepath.Join(bin, "lazyrex-hub"), filepath.Join(bin, "lazyrex-view"), filepath.Join(tools, "claude"), filepath.Join(tools, "codex")} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	}
	out := &strings.Builder{}
	return setupEnv{
		home:     home,
		getenv:   func(string) string { return "" },
		exe:      filepath.Join(bin, "lazyrex-hub"),
		lookPath: func(n string) (string, error) { return filepath.Join(tools, n), nil },
		endpoint: func(string) (string, error) { return setupLabServer, nil },
		isReal:   func(string) bool { return false },
		tags:     func(string) ([]string, error) { return []string{"v0.9.0", "v0.10.0", "v1.0.0-rc1", "nightly"}, nil },
		// No test runs git: the plugins are not installed, and a sync is a failure unless a test
		// sets its own (setupsync_test.go).
		installed: func(string) bool { return false },
		sync:      func(string, io.Writer) error { return errors.New("no sync in this test") },
		in:        bufio.NewReader(strings.NewReader(answers)),
		tty:       tty,
		out:       out,
		now:       func() time.Time { return setupNow },
	}, out
}

// tree lists every path under dir with its mode, and a file's content.
func tree(dir string) []string {
	var ps []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		fi, _ := os.Lstat(p)
		line := fmt.Sprintf("%s %v", p, fi.Mode())
		if fi.Mode().IsRegular() {
			line += " " + strconv.Quote(readFile(p))
		}
		ps = append(ps, line)
		return nil
	})
	sort.Strings(ps)
	return ps
}

func TestSetupPrintAndNoTerminalWriteNothing(t *testing.T) {
	for _, c := range []struct {
		print, tty bool
		code       int
		says       string
	}{{true, true, 0, "-print: setup wrote nothing."}, {false, false, 2, "Standard input is not a terminal: setup wrote nothing."}} {
		e, out := setupTestEnv(t, strings.Repeat("y\n", 8), c.tty)
		// Existing files that setup would change, each with its own mode.
		for p, m := range map[string]os.FileMode{
			filepath.Join(e.home, ".claude", "settings.json"):                 0o600,
			filepath.Join(e.home, ".codex", "hooks.json"):                     0o640,
			filepath.Join(e.home, ".config", "rex", "init.lua"):               0o644,
			filepath.Join(e.home, ".agents", "skills", "lazyrex", "SKILL.md"): 0o444,
		} {
			os.MkdirAll(filepath.Dir(p), 0o755)
			body := "-- mine\n"
			if strings.HasSuffix(p, ".json") {
				body = `{"theirs": true}` + "\n"
			}
			os.WriteFile(p, []byte(body), m)
			os.Chmod(p, m)
		}
		before := tree(e.home)
		if got := runSetup(e, filepath.Join(e.home, ".lazyrex"), c.print); got != c.code {
			t.Errorf("print %v tty %v: exit %d\n%s", c.print, c.tty, got, out)
		}
		if after := tree(e.home); strings.Join(after, "\n") != strings.Join(before, "\n") {
			t.Errorf("print %v tty %v: files changed:\n%v", c.print, c.tty, after)
		}
		for _, want := range []string{c.says, "+ " + `  "server": "` + setupLabServer + `",`, "hook claude", "name: lazyrex"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("print %v tty %v: no %q in:\n%s", c.print, c.tty, want, out)
			}
		}
	}
}

func TestSetupPrerequisites(t *testing.T) {
	e, out := setupTestEnv(t, "", true)
	os.Remove(filepath.Join(filepath.Dir(e.exe), "lazyrex-view"))
	before := tree(e.home)
	if runSetup(e, filepath.Join(e.home, ".lazyrex"), false) != 1 || !strings.Contains(out.String(), "the renderer is missing") || !strings.Contains(out.String(), "of the same release") {
		t.Errorf("no renderer:\n%s", out)
	}
	if strings.Join(tree(e.home), "\n") != strings.Join(before, "\n") {
		t.Error("a missing renderer wrote")
	}

	e, out = setupTestEnv(t, "", true)
	e.endpoint = func(string) (string, error) { return "", errors.New("no server") }
	if runSetup(e, filepath.Join(e.home, ".lazyrex"), false) != 1 || !strings.Contains(out.String(), "Setup does not start a server") {
		t.Errorf("no Rex:\n%s", out)
	}

	e, out = setupTestEnv(t, "", true)
	e.exe = "/var/folders/x/go-build123/b001/exe/hub"
	if runSetup(e, filepath.Join(e.home, ".lazyrex"), false) != 1 || !strings.Contains(out.String(), "go run") {
		t.Errorf("go run:\n%s", out)
	}
}

// Every yes: the files are written, the generated configs pass the hub's own readers, and a second
// run has nothing to write.
func TestSetupAllYes(t *testing.T) {
	e, out := setupTestEnv(t, strings.Repeat("y\n", 8), true)
	dir := filepath.Join(e.home, ".lazyrex")
	if got := runSetup(e, dir, false); got != 0 {
		t.Fatalf("exit %d\n%s", got, out)
	}
	if n := strings.Count(out.String(), "Written:"); n != 8 {
		t.Fatalf("%d written:\n%s", n, out)
	}
	cfg, err := loadConfig(filepath.Join(dir, "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != setupLabServer || cfg.View != filepath.Join(dir, "bin", "lazyrex-view") || len(cfg.entries) != 6 ||
		cfg.PaletteLua != filepath.Join(e.home, ".config", "rex", "lazyrex_palette.lua") {
		t.Fatalf("config %+v", cfg)
	}
	if src := cfg.entries[0].Source; src != "github:lazy-rex/LazyRex/plugins/lazyrex@v0.10.0" {
		t.Fatalf("source %q", src)
	}
	// The reload names the server that setup configured, not the caller's (codex-24's review of
	// 87ecc24).
	if want := "rex -S " + shellWord(setupLabServer) + " config reload"; !strings.Contains(out.String(), want) {
		t.Fatalf("no %q in:\n%s", want, out)
	}
	profs, order, err := (&Hub{}).loadFrom([]string{cfg.Profiles})
	if err != nil || strings.Join(order, ",") != "shell,claude,codex" || profs["codex"].Command[1] != "--no-daemon" {
		t.Fatalf("profiles %v %v %v", order, profs, err)
	}
	cmd := shellWord(e.exe) + " hook claude"
	claude := readFile(filepath.Join(e.home, ".claude", "settings.json"))
	if strings.Count(claude, string(jsonText(cmd))) != 2 {
		t.Fatalf("claude hooks:\n%s", claude)
	}
	if readFile(filepath.Join(e.home, ".agents", "skills", "lazyrex", "SKILL.md")) != string(setupSkill) {
		t.Fatal("the codex skill")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "hub.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("hub.json mode %v", fi.Mode())
	}
	// The written hook command runs the installed binary.
	os.WriteFile(e.exe, []byte("#!/bin/sh\necho \"$0 $*\" > \"$OUT\"\n"), 0o755)
	res := filepath.Join(t.TempDir(), "res")
	run := exec.Command("/bin/sh", "-c", cmd)
	run.Env = append(os.Environ(), "OUT="+res)
	if b, err := run.CombinedOutput(); err != nil || readFile(res) != e.exe+" hook claude\n" {
		t.Fatalf("hook command: %v %s %q", err, b, readFile(res))
	}

	e2, out2 := setupTestEnv(t, "", true)
	e2.home, e2.exe, e2.lookPath = e.home, e.exe, e.lookPath
	e2.installed = func(string) bool { return true } // the plugin sync is setupsync_test.go
	if got := runSetup(e2, dir, false); got != 0 || !strings.Contains(out2.String(), "Nothing to write.") || strings.Contains(out2.String(), "[y/N]") {
		t.Fatalf("second run: exit %d\n%s", got, out2)
	}
}

// A partial consent reports each file: written, declined, or not answered.
func TestSetupPartialConsent(t *testing.T) {
	e, out := setupTestEnv(t, "y\nno\n", true)
	dir := filepath.Join(e.home, ".lazyrex")
	if got := runSetup(e, dir, false); got != 0 {
		t.Fatalf("exit %d\n%s", got, out)
	}
	s := out.String()
	if !strings.Contains(s, "Written:  "+filepath.Join(dir, "hub.json")) || !strings.Contains(s, "Declined: "+filepath.Join(dir, "profiles.json")+"\n") ||
		strings.Count(s, "(no answer)") != 6 {
		t.Fatalf("report:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles.json")); !os.IsNotExist(err) {
		t.Fatal("a declined file was written")
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude")); !os.IsNotExist(err) {
		t.Fatal("an unanswered file was written")
	}
}

// An existing hub.json is not changed; a plugin without a release tag is left out and reported; an
// agent without a CLI or a configuration directory is skipped.
func TestSetupExistingAndSkipped(t *testing.T) {
	e, out := setupTestEnv(t, strings.Repeat("y\n", 8), true)
	dir := filepath.Join(e.home, ".lazyrex")
	os.WriteFile(filepath.Join(dir, "hub.json"), []byte("mine\n"), 0o600)
	e.tags = func(repo string) ([]string, error) {
		if repo == "Tunnels" {
			return []string{"v1", "latest"}, nil
		}
		return []string{"v1.2.3"}, nil
	}
	e.lookPath = func(n string) (string, error) {
		if n == "codex" {
			return "", exec.ErrNotFound
		}
		return filepath.Join(e.home, "tools", n), nil
	}
	if got := runSetup(e, dir, false); got != 0 {
		t.Fatalf("exit %d\n%s", got, out)
	}
	s := out.String()
	if readFile(filepath.Join(dir, "hub.json")) != "mine\n" || !strings.Contains(s, "It exists. Setup does not change it.") {
		t.Fatalf("hub.json:\n%s", s)
	}
	if !strings.Contains(s, "skipped    Codex: no codex on PATH") || strings.Contains(readFile(filepath.Join(dir, "profiles.json")), "codex") {
		t.Fatalf("codex:\n%s", s)
	}

	// A new hub.json: Tunnels has no vX.Y.Z tag.
	e2, out2 := setupTestEnv(t, "", false)
	e2.tags = e.tags
	runSetup(e2, filepath.Join(e2.home, ".lazyrex"), true)
	if s := out2.String(); !strings.Contains(s, "Plugin lazy-rex/Tunnels has no release tag") || strings.Contains(s, "Tunnels@") {
		t.Fatalf("tunnels:\n%s", s)
	}
}

// A tag that cannot be read blocks hub.json; the other files are still proposed.
func TestSetupTagsError(t *testing.T) {
	e, out := setupTestEnv(t, "", false)
	e.tags = func(string) ([]string, error) {
		return nil, errors.New("git ls-remote lazy-rex/LazyRex: exit status 128")
	}
	runSetup(e, filepath.Join(e.home, ".lazyrex"), true)
	if s := out.String(); !strings.Contains(s, "blocked: git ls-remote") || !strings.Contains(s, "profiles.json") {
		t.Fatalf("%s", s)
	}
}

func TestSetupConfigLocations(t *testing.T) {
	e, _ := setupTestEnv(t, "", true)
	vars := map[string]string{"CLAUDE_CONFIG_DIR": "/c", "CODEX_HOME": "/x", "XDG_CONFIG_HOME": "/xdg"}
	e.getenv = func(k string) string { return vars[k] }
	as := e.agents()
	if as[0].hooks != "/c/settings.json" || as[0].skill != "/c/skills/lazyrex/SKILL.md" || as[1].hooks != "/x/hooks.json" ||
		as[1].skill != filepath.Join(e.home, ".agents", "skills", "lazyrex", "SKILL.md") {
		t.Fatalf("%+v", as)
	}
	if f := e.rexConfigFile(); f != "/xdg/rex/init.lua" {
		t.Fatal(f)
	}
	vars["REX_CONFIG"] = "/r/my.lua"
	if f := e.rexConfigFile(); f != "/r/my.lua" {
		t.Fatal(f)
	}
}

func TestHighestTag(t *testing.T) {
	for want, tags := range map[string][]string{
		"v0.10.0": {"v0.9.0", "v0.10.0", "v0.2.11"},
		"v2.0.0":  {"v1.99.99", "v2.0.0", "v10.0", "v3.0.0-rc1", "V4.0.0"},
		"":        {"v01.0.0", "latest", "1.0.0", "v99999999999999999999.0.0"},
	} {
		if got := highestTag(tags); got != want {
			t.Errorf("%v: %q, want %q", tags, got, want)
		}
	}
}

// The embedded skill is a copy of skills/lazyrex/SKILL.md (setup.go): they must stay equal.
func TestSetupSkillCopy(t *testing.T) {
	if readFile("../../../skills/lazyrex/SKILL.md") != string(setupSkill) {
		t.Fatal("hub/cmd/hub/setupfiles/SKILL.md differs from skills/lazyrex/SKILL.md: copy it")
	}
}

// A blocked file is a failure (exit 1), also when the other files are written; a decline is not
// (codex-24's review of 0b51ce7).
func TestSetupBlockedFails(t *testing.T) {
	e, out := setupTestEnv(t, strings.Repeat("y\n", 8), true)
	e.tags = func(string) ([]string, error) {
		return nil, errors.New("git ls-remote lazy-rex/LazyRex: exit status 128")
	}
	dir := filepath.Join(e.home, ".lazyrex")
	if got := runSetup(e, dir, false); got != 1 {
		t.Errorf("a blocked hub.json: exit %d\n%s", got, out)
	}
	if s := out.String(); !strings.Contains(s, "Blocked:  "+filepath.Join(dir, "hub.json")) || !strings.Contains(s, "Written:  "+filepath.Join(dir, "profiles.json")) {
		t.Errorf("report:\n%s", s)
	}

	e, out = setupTestEnv(t, strings.Repeat("y\n", 8), true)
	os.MkdirAll(filepath.Join(e.home, ".codex"), 0o755)
	os.WriteFile(filepath.Join(e.home, ".codex", "hooks.json"), []byte("{bad"), 0o644)
	if got := runSetup(e, filepath.Join(e.home, ".lazyrex"), false); got != 1 || readFile(filepath.Join(e.home, ".codex", "hooks.json")) != "{bad" {
		t.Errorf("a malformed hook file: exit %d\n%s", got, out)
	}

	e, out = setupTestEnv(t, "", false)
	e.tags = func(string) ([]string, error) { return nil, errors.New("offline") }
	if got := runSetup(e, filepath.Join(e.home, ".lazyrex"), true); got != 1 {
		t.Errorf("-print with a blocked file: exit %d\n%s", got, out)
	}

	e, out = setupTestEnv(t, strings.Repeat("n\n", 8), true)
	if got := runSetup(e, filepath.Join(e.home, ".lazyrex"), false); got != 0 {
		t.Errorf("only declines: exit %d\n%s", got, out)
	}
}
