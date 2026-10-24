package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The managed file follows the effective hub.json (lead-c2h3 1): an existing hub.json that names
// another state, or managed_plugins, gets its managed file at that path, and the hub's own load
// reads the entries that setup wrote there.
func TestSetupManagedFilePath(t *testing.T) {
	for _, c := range []struct {
		name, extra string
		want        func(dir string) string
	}{
		{"state", "", func(dir string) string { return filepath.Join(dir, "my state", "plugins.json") }},
		{"managed_plugins", `,"managed_plugins":"mine/plugins.json"`, func(dir string) string { return filepath.Join(dir, "mine", "plugins.json") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, out := setupTestEnv(t, strings.Repeat("y\n", 9), true)
			dir := filepath.Join(e.home, ".lazyrex")
			os.MkdirAll(dir, 0o755)
			hub := `{"server":"` + setupLabServer + `","state":` + string(jsonText(filepath.Join(dir, "my state"))) + c.extra + `}`
			os.WriteFile(filepath.Join(dir, "hub.json"), []byte(hub), 0o600)
			if got := runSetup(e, dir, false); got != 0 {
				t.Fatalf("exit %d\n%s", got, out)
			}
			want := c.want(dir)
			if !strings.Contains(out.String(), "Written:  "+want) {
				t.Fatalf("no managed file at %s:\n%s", want, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "state", "plugins.json")); !os.IsNotExist(err) {
				t.Fatal("a managed file at the default path")
			}
			cfg, err := loadConfig(filepath.Join(dir, "hub.json"))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.managedPath != want || cfg.managedErr != nil || len(cfg.entries) != 6 || cfg.entries[0].managed != "lazyrex" {
				t.Fatalf("the hub's load: %s %v %d", cfg.managedPath, cfg.managedErr, len(cfg.entries))
			}
		})
	}
}

// An existing managed file holds the user's own entries (lead-c2h 3): setup leaves it as it is,
// with one row, and proposes no write.
func TestSetupManagedFileExists(t *testing.T) {
	e, out := setupTestEnv(t, strings.Repeat("y\n", 9), true)
	dir := filepath.Join(e.home, ".lazyrex")
	mine := `{"plugins":[{"id":"mine","source":"github:me/mine@main","args":{},"watch_roots":[]}]}` + "\n"
	os.MkdirAll(filepath.Join(dir, "state"), 0o700)
	path := filepath.Join(dir, "state", "plugins.json")
	os.WriteFile(path, []byte(mine), 0o600)
	if got := runSetup(e, dir, false); got != 0 {
		t.Fatalf("exit %d\n%s", got, out)
	}
	s := out.String()
	if readFile(path) != mine || !strings.Contains(s, setupManagedExists) || strings.Contains(s, "Write "+path) {
		t.Fatalf("the managed file %q:\n%s", readFile(path), s)
	}
}

// answerReader gives one answer for each read; before the answer n (from 1) it runs before.
type answerReader struct {
	answers []string
	n       int
	at      int
	before  func()
}

func (r *answerReader) Read(p []byte) (int, error) {
	if len(r.answers) == 0 {
		return 0, os.ErrClosed
	}
	r.at++
	if r.at == r.n {
		r.before()
	}
	a := r.answers[0]
	r.answers = r.answers[1:]
	return copy(p, a), nil
}

// A managed file that another writer makes after the snapshot (lead-c2h3 2): setup does not write
// it and prints the row of an existing file; that is not a failure.
func TestSetupManagedFileAppears(t *testing.T) {
	e, out := setupTestEnv(t, "", true)
	dir := filepath.Join(e.home, ".lazyrex")
	path := filepath.Join(dir, "state", "plugins.json")
	mine := `{"plugins":[]}` + "\n"
	// The second question is the managed file's: the file appears just before its answer.
	answers := make([]string, 9)
	for i := range answers {
		answers[i] = "y\n"
	}
	e.in = bufio.NewReader(&answerReader{answers: answers, n: 2, before: func() {
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, []byte(mine), 0o600)
	}})
	if got := runSetup(e, dir, false); got != 0 {
		t.Fatalf("exit %d\n%s", got, out)
	}
	s := out.String()
	if readFile(path) != mine || !strings.Contains(s, path+": "+setupManagedExists) || strings.Contains(s, "Failed:") {
		t.Fatalf("the managed file %q:\n%s", readFile(path), s)
	}
}
