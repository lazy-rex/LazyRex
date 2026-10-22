package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// dirFiles gives the relative paths of the files under dir, sorted.
func dirFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// plugin new writes the four files; the manifest passes the hub's manifest rules.
func TestPluginNew(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "notes")
	var out, errb bytes.Buffer
	if code := pluginCmdMain([]string{"new", "notes", "-dir", dest}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	want := []string{"README.md", "lazyrex.json", "main.lua", "tests/notes.lua"}
	if got := dirFiles(t, dest); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("files %v, want %v", got, want)
	}
	m, _, err := readPluginManifest(dest)
	if err != nil || m.Name != "notes" || m.Main != "main.lua" {
		t.Fatalf("manifest %+v: %v", m, err)
	}
	main, _ := os.ReadFile(filepath.Join(dest, "main.lua"))
	test, _ := os.ReadFile(filepath.Join(dest, "tests", "notes.lua"))
	if !strings.Contains(string(main), "require('lazyrex').plugin('notes')") || !strings.Contains(string(main), "'notes.add'") ||
		!strings.Contains(string(test), "alice:call('notes.add'") {
		t.Fatalf("main.lua or the test does not name the plugin:\n%s\n%s", main, test)
	}
	if !strings.Contains(out.String(), "plugin new: wrote "+dest) {
		t.Fatalf("output %q", out.String())
	}
	if ents, _ := os.ReadDir(parent); len(ents) != 1 {
		t.Fatalf("the parent has %d entries; a stage is left", len(ents))
	}
}

// A refusal or a failure writes nothing: no destination files, no stage left.
func TestPluginNewRefusals(t *testing.T) {
	parent := t.TempDir()
	full := filepath.Join(parent, "full")
	os.Mkdir(full, 0o755)
	os.WriteFile(filepath.Join(full, "keep.txt"), []byte("mine"), 0o644)
	empty := filepath.Join(parent, "empty")
	os.Mkdir(empty, 0o755)
	for name, c := range map[string]struct{ name, dir, want string }{
		"bad name":  {"Notes", filepath.Join(parent, "x"), `the name "Notes" must match`},
		"exists":    {"full", full, full + " exists"},
		"empty dir": {"empty", empty, empty + " exists"},
	} {
		err := pluginNew(c.name, c.dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasSuffix(err.Error(), "; nothing is written") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := dirFiles(t, parent); strings.Join(got, " ") != "full/keep.txt" {
		t.Fatalf("files after the refusals: %v", got)
	}
	for _, args := range [][]string{{"new"}, {"new", "-dir", "x"}, {"new", "a", "b"}, {"new", "a", "-x"}} {
		var errb bytes.Buffer
		if code := pluginCmdMain(args, &bytes.Buffer{}, &errb); code != 2 || !strings.Contains(errb.String(), "plugin new <name>") {
			t.Errorf("%v: exit %d %q", args, code, errb.String())
		}
	}
}

// A failed file write, and a destination that becomes nonempty during generation, leave no plugin
// files and no stage.
func TestPluginNewFailures(t *testing.T) {
	orig := writeNewFile
	t.Cleanup(func() { writeNewFile = orig })

	parent := t.TempDir()
	dest := filepath.Join(parent, "p")
	n := 0
	writeNewFile = func(p string, b []byte, m os.FileMode) error {
		if n++; n == 2 {
			return errors.New("disk full")
		}
		return orig(p, b, m)
	}
	err := pluginNew("p", dest, &bytes.Buffer{})
	if err == nil || err.Error() != "plugin new: disk full; nothing is written" {
		t.Fatalf("failed write: %v", err)
	}
	if ents, _ := os.ReadDir(parent); len(ents) != 0 {
		t.Fatalf("failed write: the parent has %d entries", len(ents))
	}

	// Another writer creates the destination with a file while the stage is written.
	writeNewFile = func(p string, b []byte, m os.FileMode) error {
		os.MkdirAll(dest, 0o755)
		os.WriteFile(filepath.Join(dest, "theirs.txt"), []byte("x"), 0o644)
		return orig(p, b, m)
	}
	err = pluginNew("p", dest, &bytes.Buffer{})
	if err == nil || !strings.HasPrefix(err.Error(), "plugin new: "+dest+": ") || !strings.HasSuffix(err.Error(), "; nothing is written") {
		t.Fatalf("raced destination: %v", err)
	}
	if got := dirFiles(t, parent); strings.Join(got, " ") != "p/theirs.txt" {
		t.Fatalf("raced destination: files %v", got)
	}
}

// A directory with a space and a single quote: the files are written, and the output prints the
// path as text, not as a shell command.
func TestPluginNewShellText(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "my 'odd' plugin")
	var out bytes.Buffer
	if err := pluginNew("odd", dest, &out); err != nil || len(dirFiles(t, dest)) != 4 {
		t.Fatalf("%v", err)
	}
	if want := "plugin new: wrote " + dest + "\nNext: run lazyrex-hub test in that directory.\n"; out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
}

// An output failure after the rename says that the plugin is written.
func TestPluginNewOutputFailure(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "p")
	err := pluginNew("p", dest, failWriter{})
	if err == nil || err.Error() != "plugin new: "+dest+" is written, but the result lines are not: disk full" {
		t.Fatalf("got %v", err)
	}
	if len(dirFiles(t, dest)) != 4 {
		t.Fatal("the plugin is not written")
	}
}
