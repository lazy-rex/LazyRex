package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syncRecorder is a plugin sync that records its config paths and answers with err.
type syncRecorder struct {
	configs []string
	err     error
}

func (s *syncRecorder) sync(cfg string, w io.Writer) error {
	s.configs = append(s.configs, cfg)
	if s.err != nil {
		return s.err
	}
	fmt.Fprintln(w, "installed github:lazy-rex/Messaging@v1.0.0 0123")
	return nil
}

const syncQuestion = "Install the plugins of "

// Setup ends with the plugin sync (the coordinator, 2026-10-22: a user who answers y to every
// question has working plugins). The question comes after the files, like their questions. A
// yes runs the sync for the hub.json that setup wrote; no, or no answer, prints the exact command.
// The command names the running binary and the hub.json, quoted for a shell (the paths have
// spaces).
func TestSetupOffersThePluginSync(t *testing.T) {
	for _, c := range []struct {
		name, dir string // dir "" is ~/.lazyrex
		answers   string
		syncErr   error
		ran, ok   bool // the sync ran; the plugins are installed
		exit      int
	}{
		{name: "yes", answers: strings.Repeat("y\n", 9), ran: true, ok: true},
		{name: "yes, another directory", dir: "my lazy rex", answers: strings.Repeat("y\n", 9), ran: true, ok: true},
		{name: "no", answers: strings.Repeat("y\n", 8) + "n\n"},
		{name: "no answer", answers: strings.Repeat("y\n", 8)},
		{name: "a failed sync", answers: strings.Repeat("y\n", 9), syncErr: errors.New("git ls-remote: Authentication failed"), ran: true, exit: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, out := setupTestEnv(t, c.answers, true)
			rec := &syncRecorder{err: c.syncErr}
			e.sync = rec.sync
			dir := filepath.Join(e.home, ".lazyrex")
			if c.dir != "" {
				dir = filepath.Join(e.home, c.dir)
			}
			cfg := filepath.Join(dir, "hub.json")
			if got := runSetup(e, dir, false); got != c.exit {
				t.Fatalf("exit %d, want %d\n%s", got, c.exit, out)
			}
			s := out.String()
			if !strings.Contains(s, syncQuestion+cfg+" now (lazyrex-hub plugin sync)? [y/N]") {
				t.Fatalf("no sync question:\n%s", s)
			}
			if c.ran != (len(rec.configs) == 1) || c.ran && rec.configs[0] != cfg {
				t.Fatalf("sync calls %v, want one for %s: %v", rec.configs, cfg, c.ran)
			}
			cmd := "Install the plugins: " + shellWord(e.exe) + " plugin sync -config " + shellWord(cfg)
			if c.ok {
				if !strings.Contains(s, "installed github:lazy-rex/Messaging") || !strings.Contains(s, "Plugins: installed.") || strings.Contains(s, cmd) {
					t.Fatalf("after a sync:\n%s", s)
				}
				return
			}
			// No sync, or a failed one: the exact command, and no claim that the plugins are ready.
			if !strings.Contains(s, cmd) || strings.Contains(s, "Plugins: installed.") {
				t.Fatalf("want the command %q and no ready claim:\n%s", cmd, s)
			}
			if c.syncErr != nil && !strings.Contains(s, "Plugin sync failed: git ls-remote: Authentication failed") {
				t.Fatalf("the failure is not reported:\n%s", s)
			}
		})
	}
}

// Installed plugins need no question.
func TestSetupSkipsTheSyncWhenInstalled(t *testing.T) {
	e, out := setupTestEnv(t, strings.Repeat("y\n", 9), true)
	rec := &syncRecorder{}
	e.sync = rec.sync
	e.installed = func(string) bool { return true }
	if got := runSetup(e, filepath.Join(e.home, ".lazyrex"), false); got != 0 {
		t.Fatalf("exit %d\n%s", got, out)
	}
	if s := out.String(); strings.Contains(s, syncQuestion) || len(rec.configs) != 0 || !strings.Contains(s, "Plugins: installed.") || strings.Contains(s, "plugin sync -config") {
		t.Fatalf("sync %v:\n%s", rec.configs, s)
	}
}

// No sync, no question and no command without a usable hub.json: declined, blocked, or one
// without a server. -print and a run without a terminal write nothing and run nothing.
func TestSetupNoSyncWithoutAUsableConfig(t *testing.T) {
	for _, c := range []struct {
		name, existing, answers string
		tty, print              bool
		exit                    int
	}{
		{name: "hub.json declined", answers: "n\n" + strings.Repeat("y\n", 8), tty: true},
		{name: "hub.json without a server", existing: `{"state":"/s"}`, answers: strings.Repeat("y\n", 9), tty: true},
		{name: "hub.json not JSON", existing: "mine\n", answers: strings.Repeat("y\n", 9), tty: true},
		{name: "-print", answers: strings.Repeat("y\n", 9), tty: true, print: true},
		{name: "no terminal", answers: strings.Repeat("y\n", 9), exit: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, out := setupTestEnv(t, c.answers, c.tty)
			rec := &syncRecorder{}
			e.sync = rec.sync
			asked := 0
			e.installed = func(string) bool { asked++; return false }
			dir := filepath.Join(e.home, ".lazyrex")
			if c.existing != "" {
				os.MkdirAll(dir, 0o755)
				os.WriteFile(filepath.Join(dir, "hub.json"), []byte(c.existing), 0o600)
			}
			if got := runSetup(e, dir, c.print); got != c.exit {
				t.Fatalf("exit %d, want %d\n%s", got, c.exit, out)
			}
			if s := out.String(); len(rec.configs) != 0 || asked != 0 || strings.Contains(s, syncQuestion) || strings.Contains(s, "plugin sync") || strings.Contains(s, "Plugins: installed.") {
				t.Fatalf("sync %v, status looks %d:\n%s", rec.configs, asked, s)
			}
		})
	}
}

// The sync follows what happened to hub.json in this run, and the file as it is now (codex-24's
// review of 10a9632: a write that failed because another writer made hub.json after the diff was
// still followed by the status read and the sync). A failed write, and a kept file that lost its
// server during setup, give no status read, no question, no sync and no command.
func TestSetupNoSyncAfterTheConfigChanged(t *testing.T) {
	for _, c := range []struct {
		name, existing, during string // existing: hub.json before setup; during: written at the first file write
		exit                   int
	}{
		{name: "another writer made hub.json after the diff", during: `{"server":"unix:///another.sock","state":"/another"}`, exit: 1},
		{name: "the kept hub.json lost its server", existing: `{"server":"unix:///other/rex.sock"}`, during: `{"state":"/s"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, out := setupTestEnv(t, strings.Repeat("y\n", 10), true)
			dir := filepath.Join(e.home, ".lazyrex")
			cfg := filepath.Join(dir, "hub.json")
			if c.existing != "" {
				os.MkdirAll(dir, 0o755)
				os.WriteFile(cfg, []byte(c.existing), 0o600)
			}
			once := false
			e.now = func() time.Time {
				if !once {
					once = true
					os.MkdirAll(dir, 0o755)
					os.WriteFile(cfg, []byte(c.during), 0o600)
				}
				return setupNow
			}
			looks := 0
			e.installed = func(string) bool { looks++; return false }
			rec := &syncRecorder{}
			e.sync = rec.sync
			if got := runSetup(e, dir, false); got != c.exit {
				t.Fatalf("exit %d, want %d\n%s", got, c.exit, out)
			}
			if s := out.String(); looks != 0 || len(rec.configs) != 0 || strings.Contains(s, syncQuestion) || strings.Contains(s, "plugin sync") || strings.Contains(s, "Plugins: installed.") {
				t.Fatalf("status looks %d, sync %v:\n%s", looks, rec.configs, s)
			}
		})
	}
}
