package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sdkBoot is a launcher with the contract of sdk/lua/boot.lua: it gets main=<absolute main>, puts
// the plugin directory and its own directory in package.path, and runs main.
const sdkBoot = `local main = rex.args.main
local dir = main:match('^(.*)/[^/]*$') or '.'
local sdk = debug.getinfo(1, 'S').source:gsub('^@', ''):match('^(.*)/[^/]*$') or '.'
package.path = dir .. '/?.lua;' .. dir .. '/?/init.lua;' .. sdk .. '/?.lua;' .. package.path
return dofile(main)
`

// sdkFixture is startFixture with its name from the SDK module and a sibling module.
var sdkFixture = strings.Replace(fmt.Sprintf(startFixture, "sdkp"), `local name = "sdkp"`,
	`local name = require('lazyrex').name .. require('sib')`, 1)

func writeSDK(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "sdk dir") // a space in the path
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "boot.lua"), []byte(sdkBoot), 0o600)
	os.WriteFile(filepath.Join(dir, "lazyrex.lua"), []byte("return {name = 'sdk'}\n"), 0o600)
	return dir
}

func (s *startLab) writeSDKPlugin() {
	s.writeSource("sdkp", `{"name":"sdkp","main":"main.lua","sdk":"0"}`, "")
	d := filepath.Join(s.src, "sdkp")
	os.WriteFile(filepath.Join(d, "main.lua"), []byte(sdkFixture), 0o600)
	os.WriteFile(filepath.Join(d, "sib.lua"), []byte("return 'p'\n"), 0o600)
}

// A plugin that declares sdk starts through the launcher: it requires the SDK module and a
// sibling module, and its process has main=<its main>. A plugin without sdk starts as before.
func TestPluginSDKLaunchLab(t *testing.T) {
	s := newStartLab(t, "sdkp", "plain")
	s.writeSDKPlugin()
	s.writeSource("plain", `{"name":"plain","main":"main.lua"}`, "plain")
	s.args = []string{"-sdk", writeSDK(t)}
	s.start()
	sdk, _ := s.startRun("sdkp")
	plain, _ := s.startRun("plain")
	// The run's main is in its local snapshot (localsnapshot.go).
	snap, _ := s.entry("sdkp")["dir"].(string)
	if !s.snapshot(snap) {
		t.Fatalf("the run of sdkp is not a local snapshot: %q", snap)
	}
	main, _ := filepath.EvalSymlinks(filepath.Join(snap, "main.lua"))
	if !argvHas(sdk.ChildPID, "main="+main) && !argvHas(sdk.ChildPID, "main="+filepath.Join(snap, "main.lua")) {
		t.Fatalf("the SDK plugin process has no main=%s", main)
	}
	plainSnap, _ := s.entry("plain")["dir"].(string)
	if plainSnap == "" {
		t.Fatal("hub.plugins gives no run dir for the entry of plain")
	}
	if argvHas(plain.ChildPID, "main="+filepath.Join(plainSnap, "main.lua")) {
		t.Fatal("a plugin without sdk started through the launcher")
	}
}

// Without an SDK directory, a plugin that declares sdk is refused, and the others start. The hub
// has no -sdk, and harnessBinary builds it in a temporary directory with no sdk/lua above it, so
// findSDK finds none.
func TestPluginSDKRefusedWithoutSDKLab(t *testing.T) {
	s := newStartLab(t, "sdkp", "plain")
	s.writeSDKPlugin()
	s.writeSource("plain", `{"name":"plain","main":"main.lua"}`, "plain")
	for d := filepath.Dir(s.bin); d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "sdk", "lua", "boot.lua")); err == nil {
			t.Fatalf("the fixture: %s has an SDK above the hub binary", d)
		}
	}
	s.start()
	s.startRun("plain")
	s.wait("the refusal", func() bool { return s.status("sdkp").State == "refused" })
	if st := s.status("sdkp"); !strings.Contains(st.Error, "no SDK directory") {
		t.Fatalf("the refusal: %+v", st)
	}
	plainSnap, _ := s.entry("plain")["dir"].(string)
	if p := s.pluginProcs(); plainSnap == "" || len(p) != 1 || !strings.Contains(p[0], plainSnap) {
		t.Errorf("plugin processes %q, want plain only (its run dir %q)", p, plainSnap)
	}
}

// A -sdk value is the operator's (or the wrapper's) explicit choice: a value without boot.lua, or
// one that cannot be resolved, stops the start with exit status 1 before the store opens, so no
// plugin starts (docs/hub.md, "Paths at the start").
func TestHubRefusesABadSDKLab(t *testing.T) {
	s := newStartLab(t, "sdkp", "plain")
	s.writeSDKPlugin()
	s.writeSource("plain", `{"name":"plain","main":"main.lua"}`, "plain")
	cfg, err := loadConfig(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	for _, c := range []struct{ sdk, want string }{
		{empty, fmt.Sprintf("-sdk %s has no boot.lua", empty)},
		{filepath.Join(empty, "missing"), fmt.Sprintf("-sdk %s cannot be resolved", filepath.Join(empty, "missing"))},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, s.bin, "-config", s.cfg, "-test", "-sdk", c.sdk)
		cmd.Env, cmd.Dir = s.l.env, s.l.home()
		out, _ := cmd.CombinedOutput()
		cancel()
		if code := cmd.ProcessState.ExitCode(); code != 1 || !strings.Contains(string(out), c.want) {
			t.Errorf("-sdk %s: exit %d, want 1 and %q\n%s", c.sdk, code, c.want, out)
		}
		if _, err := os.Stat(filepath.Join(cfg.State, "hub.db")); !os.IsNotExist(err) {
			t.Errorf("-sdk %s: the store opened (hub.db: %v)", c.sdk, err)
		}
	}
	if p := s.pluginProcs(); len(p) > 0 {
		t.Errorf("plugin processes run: %q", p)
	}
}

// pluginProcs returns the command lines of the processes whose argv names a source directory or a
// local snapshot (<state>/plugins/local) of this lab: its plugin processes. A local plugin runs from
// its snapshot. TestPluginSDKRefusedWithoutSDKLab proves that it sees one.
func (s *startLab) pluginProcs() []string {
	s.t.Helper()
	// The hub reports the canonical state (symlinks resolved). The state dir may not exist (a hub
	// that stopped before it made the dir), so resolve its parent, the lab dir.
	var c Config
	b, _ := os.ReadFile(s.cfg)
	json.Unmarshal(b, &c)
	abs, err := filepath.Abs(c.State)
	if err != nil {
		s.t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		s.t.Fatal(err)
	}
	snaps := filepath.Join(parent, filepath.Base(abs), "plugins", "local")
	ps, err := exec.Command("ps", "-axww", "-o", "command=").Output()
	if err != nil {
		s.t.Fatal(err)
	}
	var found []string
	for _, line := range strings.Split(string(ps), "\n") {
		if strings.Contains(line, s.src) || strings.Contains(line, snaps) {
			found = append(found, line)
		}
	}
	return found
}

func TestManifestSDK(t *testing.T) {
	for _, c := range []struct {
		sdk string
		ok  bool
	}{{"", true}, {"0", true}, {"1", false}, {"latest", false}} {
		m := pluginManifest{Name: "p", Main: "main.lua", SDK: c.sdk}
		if err := m.check(); (err == nil) != c.ok {
			t.Errorf("sdk %q: %v", c.sdk, err)
		}
	}
}
