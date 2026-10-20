package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A manifest can name its notice (docs/plugin-sources.md, section 10). The lab config names only
// sources, and the hub reads the notice of each plugin from its manifest.
func TestManifestNotice(t *testing.T) {
	dir, err := manifestIn(t, `{"name":"p","main":"p.lua","notice":"New message. Call messaging_read.","test":{"plugins":[{"source":"n"}]}}`)
	if err == nil || !strings.Contains(err.Error(), "no lazyrex.json") {
		t.Fatalf("a test.plugins entry without a manifest: %v", err)
	}
	os.MkdirAll(filepath.Join(dir, "n"), 0o700)
	os.WriteFile(filepath.Join(dir, "n", "lazyrex.json"), []byte(`{"name":"n","main":"n.lua","notice":"Dep notice."}`), 0o600)
	os.WriteFile(filepath.Join(dir, "n", "n.lua"), nil, 0o600)
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	lazyrex := t.TempDir()
	os.MkdirAll(filepath.Join(lazyrex, "plugins", "lazyrex"), 0o700)
	os.WriteFile(filepath.Join(lazyrex, "plugins", "lazyrex", "lazyrex.json"), []byte(`{"name":"lazyrex","main":"lazyrex.lua"}`), 0o600)
	os.WriteFile(filepath.Join(lazyrex, "plugins", "lazyrex", "lazyrex.lua"), nil, 0o600)
	home, err := homePlugin(lazyrex)
	if err != nil {
		t.Fatal(err)
	}
	o := &testOpts{dir: dir, lazyrex: lazyrex, home: home, self: "/bin/true", m: m}
	l := &lab{dir: t.TempDir(), server: "unix:///tmp/none/rex.sock"}
	path, err := o.hubConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var raw struct {
		Plugins []map[string]any
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, p := range raw.Plugins {
		if _, ok := p["source"]; !ok || len(p) != 1 {
			t.Errorf("a lab entry is a source only (no args here): %v", p)
		}
	}
	// The hub reads the notices from the manifests.
	cfg, err := loadConfig(path, false)
	if err != nil {
		t.Fatal(err)
	}
	notices := map[string]string{}
	for _, p := range cfg.Plugins {
		notices[p.Name] = p.Notice
	}
	if len(notices) != 3 || notices["p"] != "New message. Call messaging_read." || notices["n"] != "Dep notice." || notices["lazyrex"] != "" {
		t.Errorf("config notices: %v", notices)
	}

	bad := map[string]string{
		`{"name":"p","main":"p.lua","notice":"two\nlines"}`: "control characters",
		`{"name":"p","main":"p.lua","notice":"esc\u001b"}`:  "control characters",
		`{"name":"p","main":"p.lua","notice":7}`:            "cannot unmarshal",
	}
	// No length limit: the approved contract is a string.
	if _, err := manifestIn(t, `{"name":"p","main":"p.lua","notice":"`+strings.Repeat("x", 201)+`"}`); err != nil {
		t.Errorf("a 201-byte notice: %v", err)
	}
	for body, want := range bad {
		if _, err := manifestIn(t, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.60s: got %v, want %q", body, err, want)
		}
	}
}
