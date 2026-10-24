package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configPlugin writes a local plugin directory with a manifest named name.
func configPlugin(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, body := range map[string]string{"lazyrex.json": `{"name":"` + name + `","main":"main.lua"}`, "main.lua": ""} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runConfigCmd(t *testing.T, cfg string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := configCmdMain([]string{cfg}, &out, &errOut)
	return code, out.String(), errOut.String()
}

// lazyrex-hub config prints the records of loadConfig: the server, the absolute state (a relative
// state resolves against the working directory, as at the hub start), the view, and one plugin line
// for each local entry, config then managed, with the manifest's name. A GitHub entry has none.
func TestConfigCmdRecords(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := configPlugin(t, filepath.Join(dir, "src", "a"), "alpha")
	b := configPlugin(t, filepath.Join(dir, "src", "b"), "beta")
	if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	// managed_plugins is relative to the config directory; the managed file names b.
	writeManaged(t, filepath.Join(dir, "conf"), `{"plugins":[{"id":"b","source":"`+b+`"}]}`)
	cfg := filepath.Join(dir, "conf", "hub.json")
	body := `{"server":"unix:///lab/rex.sock","state":"rel/state","view":"/opt/view","managed_plugins":"plugins.json",
		"plugins":[{"source":"../src/a"},{"source":"github:o/r@main"}]}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runConfigCmd(t, cfg)
	want := "server\tunix:///lab/rex.sock\n" +
		"state\t" + filepath.Join(dir, "rel", "state") + "\n" +
		"view\t/opt/view\n" +
		"plugin\talpha\t" + a + "\n" +
		"plugin\tbeta\t" + b + "\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("exit %d, stdout:\n%s\nstderr: %q\nwant stdout:\n%s", code, out, errOut, want)
	}
}

// The JSON reader is loadConfig's: an escape in a value or in a key is decoded (codex-29's cases,
// c29-devsh-unicode), so dev.sh and the hub read one state.
func TestConfigCmdEscapes(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "hub.json")
	for _, body := range []string{
		`{"server":"unix:///lab/rex.sock","state":"/tmp/rexlab-\u0074est"}`, // a Unicode escape in a value
		`{"server":"unix:///lab/rex.sock","st\u0061te":"/tmp/rexlab-test"}`, // an escaped key
	} {
		if !strings.Contains(body, `\u00`) {
			t.Fatalf("the case %s has no escape", body)
		}
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := runConfigCmd(t, cfg)
		if code != 0 || !strings.Contains(out, "state\t/tmp/rexlab-test\n") || strings.Contains(out, "view\t") {
			t.Errorf("%s: exit %d, stdout:\n%s\nstderr: %q", body, code, out, errOut)
		}
	}
}

// A value with a TAB or a line break cannot be one field: exit 1, one line that names the key, and
// no output. A config that loadConfig refuses exits 1 with its error. A local entry whose manifest
// the hub refuses has no plugin line, and a warning: the hub starts with it refused.
func TestConfigCmdRefusals(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "hub.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"server":"unix:///lab/a\tb.sock","state":"/s"}`)
	if code, out, errOut := runConfigCmd(t, cfg); code != 1 || out != "" || !strings.Contains(errOut, "the server value") || strings.Count(errOut, "\n") != 1 {
		t.Errorf("a TAB: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	write(`{"server":"unix:///lab/rex.sock","state":"/s\nx"}`)
	if code, out, errOut := runConfigCmd(t, cfg); code != 1 || out != "" || !strings.Contains(errOut, "the state value") {
		t.Errorf("a line break: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	write(`{"state":"/s"}`)
	if code, out, errOut := runConfigCmd(t, cfg); code != 1 || out != "" || !strings.Contains(errOut, "server and state are required") {
		t.Errorf("a refused config: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if err := os.MkdirAll(filepath.Join(dir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(`{"server":"unix:///lab/rex.sock","state":"/s","plugins":[{"source":"broken"}]}`)
	if code, out, errOut := runConfigCmd(t, cfg); code != 0 || strings.Contains(out, "plugin\t") || !strings.Contains(errOut, "warning: entry 1: no lazyrex.json") {
		t.Errorf("a refused manifest: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, errOut := func() (int, string, string) {
		var o, e bytes.Buffer
		return configCmdMain(nil, &o, &e), o.String(), e.String()
	}(); code != 2 || !strings.Contains(errOut, "usage: lazyrex-hub config <hub.json>") {
		t.Errorf("no argument: exit %d, stderr %q", code, errOut)
	}
}
