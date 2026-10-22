package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var setupNow = time.Unix(1700000000, 0)

// A new file is written with its permissions; its directory is made only at the write.
func TestSetupFileNew(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "b", "hub.json")
	f := snapshotFile(p, 0o600)
	f.next = []byte("{}\n")
	if !f.changes() {
		t.Fatal("a new file is a change")
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatal("the snapshot made a directory")
	}
	if b, err := f.write(setupNow); err != nil || b != "" {
		t.Fatalf("write: %q %v", b, err)
	}
	if got := readFile(p); got != "{}\n" {
		t.Fatalf("content %q", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

// An existing file keeps its permissions and gets a backup with its old text and permissions.
func TestSetupFileChange(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir()) // the backup is named after the resolved file
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte("old\n"), 0o640)
	os.Chmod(p, 0o640)
	f := snapshotFile(p, 0o644)
	f.next = []byte("new\n")
	b, err := f.write(setupNow)
	if err != nil {
		t.Fatal(err)
	}
	if b != p+".lazyrex-bak-1700000000" || readFile(b) != "old\n" || readFile(p) != "new\n" {
		t.Fatalf("backup %q: %q, file %q", b, readFile(b), readFile(p))
	}
	for _, q := range []string{p, b} {
		if fi, _ := os.Stat(q); fi.Mode().Perm() != 0o640 {
			t.Errorf("%s: mode %v", q, fi.Mode())
		}
	}
	// A second change in the same second makes a second backup; the first stays.
	f = snapshotFile(p, 0o644)
	f.next = []byte("newer\n")
	b2, err := f.write(setupNow)
	if err != nil || b2 == b || readFile(b) != "old\n" || readFile(b2) != "new\n" {
		t.Fatalf("second backup %q (%v): %q, first %q", b2, err, readFile(b2), readFile(b))
	}
}

// A file that changed or appeared after the snapshot is not written.
func TestSetupFileRefusesAChangedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "init.lua")
	os.WriteFile(p, []byte("a\n"), 0o644)
	f := snapshotFile(p, 0o644)
	f.next = []byte("a\nb\n")
	os.WriteFile(p, []byte("edited\n"), 0o644)
	if _, err := f.write(setupNow); err == nil || !strings.Contains(err.Error(), "changed after setup showed it") {
		t.Fatalf("changed: %v", err)
	}
	if readFile(p) != "edited\n" {
		t.Fatal("the edited file was replaced")
	}
	if m, _ := filepath.Glob(p + ".lazyrex-bak-*"); len(m) != 0 {
		t.Fatalf("backups: %v", m)
	}

	q := filepath.Join(dir, "new.json")
	g := snapshotFile(q, 0o600)
	g.next = []byte("{}\n")
	os.WriteFile(q, []byte("theirs\n"), 0o600)
	if _, err := g.write(setupNow); err == nil {
		t.Fatal("a file that appeared was replaced")
	}
	if readFile(q) != "theirs\n" {
		t.Fatal("the new file was replaced")
	}
}

// A symlink is followed and stays a link; a dangling link or a link to a directory is blocked.
func TestSetupFileSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	os.MkdirAll(filepath.Dir(real), 0o755)
	os.WriteFile(real, []byte("old\n"), 0o600)
	link := filepath.Join(dir, "settings.json")
	os.Symlink(real, link)
	f := snapshotFile(link, 0o644)
	f.next = []byte("new\n")
	b, err := f.write(setupNow)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
	resolved, _ := filepath.EvalSymlinks(real)
	if readFile(real) != "new\n" || !strings.HasPrefix(b, resolved+".lazyrex-bak-") {
		t.Fatalf("target %q, backup %q", readFile(real), b)
	}

	dangling := filepath.Join(dir, "dangling.json")
	os.Symlink(filepath.Join(dir, "missing"), dangling)
	if f := snapshotFile(dangling, 0o644); !strings.Contains(f.block, "does not resolve") {
		t.Errorf("dangling: %q", f.block)
	}
	todir := filepath.Join(dir, "todir")
	os.Symlink(filepath.Join(dir, "dotfiles"), todir)
	if f := snapshotFile(todir, 0o644); f.block != "it is not a regular file" {
		t.Errorf("a link to a directory: %q", f.block)
	}
}

func TestFileDiffIsComplete(t *testing.T) {
	got := fileDiff([]byte("a\nb\nc\n"), []byte("a\nc\nd\n"))
	if want := "  a\n- b\n  c\n+ d\n"; got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
	if got := fileDiff(nil, []byte("x\n")); got != "+ x\n" {
		t.Fatalf("new file: %q", got)
	}
}

func TestAppendLines(t *testing.T) {
	l := []string{`pcall(require, "lazyrex_boot")`, `pcall(require, "lazyrex_palette")`}
	got := string(appendLines([]byte("rex.bind('x')\n  pcall(require, \"lazyrex_palette\")"), l...))
	if want := "rex.bind('x')\n  pcall(require, \"lazyrex_palette\")\n" + l[0] + "\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if again := string(appendLines([]byte(got), l...)); again != got {
		t.Fatalf("not idempotent: %q", again)
	}
	if got := string(appendLines(nil, l...)); got != l[0]+"\n"+l[1]+"\n" {
		t.Fatalf("empty: %q", got)
	}
}

// The hook command runs the binary through a shell, for paths with spaces and shell characters.
func TestShellWordRunsThePath(t *testing.T) {
	for _, name := range []string{"plain", "with space", `a"b`, "d$HOME", "back`tick`", `back\slash`, "it's", "bang!"} {
		dir := filepath.Join(t.TempDir(), name)
		os.MkdirAll(dir, 0o755)
		bin := filepath.Join(dir, "lazyrex-hub")
		out := filepath.Join(t.TempDir(), "out")
		os.WriteFile(bin, []byte("#!/bin/sh\necho \"$0 $*\" > \"$OUT\"\n"), 0o755)
		cmd := exec.Command("/bin/sh", "-c", shellWord(bin)+" hook claude")
		cmd.Env = append(os.Environ(), "OUT="+out)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: %v %s", name, err, b)
			continue
		}
		if got := readFile(out); got != bin+" hook claude\n" {
			t.Errorf("%s: ran %q", name, got)
		}
	}
	if got := shellWord("/Users/x y/.lazyrex/bin/lazyrex-hub"); got != `"/Users/x y/.lazyrex/bin/lazyrex-hub"` {
		t.Errorf("the hooks/README form: %s", got)
	}
}

const testHookCmd = `"/h/.lazyrex/bin/lazyrex-hub" hook claude`

func hookProposal(t *testing.T, old, kind string) *setupFile {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if old != "" {
		os.WriteFile(p, []byte(old), 0o644)
	}
	f := snapshotFile(p, 0o644)
	hookFile(f, kind, testHookCmd)
	return f
}

// The merge keeps the user's keys, in order, and the other hooks; a second run changes nothing.
func TestHookFileMerge(t *testing.T) {
	old := `{"model": "opus", "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "rex hook"}]}], "Stop": []}, "env": {"Z": "1", "A": "<&>"}, "n": 1.50}`
	f := hookProposal(t, old, "claude")
	if f.block != "" || !f.changes() {
		t.Fatalf("block %q, changes %v", f.block, f.changes())
	}
	got := string(f.next)
	want := `{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "rex hook"
          }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "\"/h/.lazyrex/bin/lazyrex-hub\" hook claude",
            "timeout": 5
          }
        ]
      }
    ],
    "Stop": [],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "\"/h/.lazyrex/bin/lazyrex-hub\" hook claude",
            "timeout": 5
          }
        ]
      }
    ]
  },
  "env": {
    "Z": "1",
    "A": "<&>"
  },
  "n": 1.50
}
`
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
	if again := hookProposal(t, got, "claude"); again.changes() || len(again.notes) != 0 {
		t.Fatalf("second run: changes %v, notes %v", again.changes(), again.notes)
	}
	if codex := hookProposal(t, "", "codex"); !strings.Contains(string(codex.next), `"SessionStart"`) || strings.Contains(string(codex.next), "UserPromptSubmit") {
		t.Fatalf("codex: %s", codex.next)
	}
}

// The Python hook or another lazyrex-hub hook owns its event; the command in another event does
// not count.
func TestHookFileExistingHooks(t *testing.T) {
	py := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "python3 /h/.lazyrex/bin/session-start.py claude"}]}]}}`
	f := hookProposal(t, py, "claude")
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "Python hook") {
		t.Fatalf("notes %v", f.notes)
	}
	root, _ := parseJSON(f.next)
	if n := len(root.get("hooks").get("SessionStart").vals); n != 1 {
		t.Fatalf("SessionStart has %d groups", n)
	}
	if root.get("hooks").get("UserPromptSubmit") == nil {
		t.Fatal("UserPromptSubmit was not added")
	}

	other := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "/old/lazyrex-hub hook claude"}]}]}}`
	if f := hookProposal(t, other, "claude"); len(f.notes) != 1 || !strings.Contains(f.notes[0], "another lazyrex-hub hook") {
		t.Fatalf("other hub: %v", f.notes)
	}

	elsewhere := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": ` + string(jsonText(testHookCmd)) + `}]}]}}`
	f = hookProposal(t, elsewhere, "codex")
	if root, _ := parseJSON(f.next); root == nil || root.get("hooks").get("SessionStart") == nil {
		t.Fatalf("a command in Stop suppressed SessionStart: %s", f.next)
	}
}

// Setup does not replace a value of another type, and does not touch invalid JSON.
func TestHookFileRefuses(t *testing.T) {
	for old, want := range map[string]string{
		`[]`:                              "not a JSON object",
		`{"hooks": []}`:                   `"hooks" is not an object`,
		`{"hooks": {"SessionStart": {}}}`: `"hooks.SessionStart" is not an array`,
		`{"hooks": `:                      "not valid JSON",
		`{} {}`:                           "not valid JSON",
	} {
		f := hookProposal(t, old, "claude")
		if !strings.Contains(f.block, want) || f.changes() {
			t.Errorf("%s: block %q, changes %v", old, f.block, f.changes())
		}
	}
}

// The snapshot is the resolved file: a parent symlink that changes after it is refused (codex-24's
// review of 3c3f3bd).
func TestSetupFileParentSymlinkChange(t *testing.T) {
	d := t.TempDir()
	a, b := filepath.Join(d, "a"), filepath.Join(d, "b")
	for _, x := range []string{a, b} {
		os.Mkdir(x, 0o700)
		os.WriteFile(filepath.Join(x, "config"), []byte("old"), 0o600)
	}
	l := filepath.Join(d, "link")
	os.Symlink(a, l)
	f := snapshotFile(filepath.Join(l, "config"), 0o600)
	f.next = []byte("new")
	os.Remove(l)
	os.Symlink(b, l)
	if _, err := f.write(setupNow); err == nil {
		t.Fatal("a changed parent symlink was accepted")
	}
	if readFile(filepath.Join(b, "config")) != "old" || readFile(filepath.Join(a, "config")) != "old" {
		t.Fatal("a file was written")
	}

	// A new file: the same for the parent chain that exists.
	g := snapshotFile(filepath.Join(l, "sub", "new.json"), 0o600)
	g.next = []byte("{}")
	os.Remove(l)
	os.Symlink(a, l)
	if _, err := g.write(setupNow); err == nil {
		t.Fatal("a new file under a changed parent symlink was accepted")
	}
}

// Permissions are part of the snapshot.
func TestSetupFilePermissionChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config")
	os.WriteFile(p, []byte("old"), 0o644)
	os.Chmod(p, 0o644)
	f := snapshotFile(p, 0o600)
	f.next = []byte("new")
	os.Chmod(p, 0o600)
	if _, err := f.write(setupNow); err == nil {
		t.Fatal("changed permissions were accepted")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 || readFile(p) != "old" {
		t.Fatalf("mode %v, %q", fi.Mode(), readFile(p))
	}
}

// Every existing hook file is parsed: only an absent file starts as an empty object.
func TestHookFileEmptyExisting(t *testing.T) {
	for _, old := range []string{"", "  \n"} {
		p := filepath.Join(t.TempDir(), "hooks.json")
		os.WriteFile(p, []byte(old), 0o600)
		f := snapshotFile(p, 0o600)
		hookFile(f, "codex", testHookCmd)
		if f.block == "" || f.changes() {
			t.Errorf("%q: block %q, changes %v", old, f.block, f.changes())
		}
	}
}
