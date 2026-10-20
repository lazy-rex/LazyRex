package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A manifest can name its notice (plugin sources, step 2a). The harness passes it to the test hub
// config as plugins[].notice; the hub's config behavior does not change.
func TestManifestNotice(t *testing.T) {
	dir, err := manifestIn(t, `{"name":"p","main":"p.lua","notice":"New message. Call messaging_read."}`)
	if err != nil {
		t.Fatal(err)
	}
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	o := &testOpts{dir: dir, lazyrex: t.TempDir(), self: "/bin/true", m: m}
	l := &lab{dir: t.TempDir(), server: "unix:///tmp/none/rex.sock"}
	path, err := o.hubConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var cfg struct {
		Plugins []struct{ Name, Notice string }
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	notices := map[string]string{}
	for _, p := range cfg.Plugins {
		notices[p.Name] = p.Notice
	}
	if notices["p"] != "New message. Call messaging_read." || notices["lazyrex"] != "" {
		t.Errorf("config notices: %v", notices)
	}

	bad := map[string]string{
		`{"name":"p","main":"p.lua","notice":"two\nlines"}`:                       "control characters",
		`{"name":"p","main":"p.lua","notice":"esc\u001b"}`:                        "control characters",
		`{"name":"p","main":"p.lua","notice":"` + strings.Repeat("x", 201) + `"}`: "at most 200 bytes",
		`{"name":"p","main":"p.lua","notice":7}`:                                  "cannot unmarshal",
	}
	for body, want := range bad {
		if _, err := manifestIn(t, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.60s: got %v, want %q", body, err, want)
		}
	}
}
