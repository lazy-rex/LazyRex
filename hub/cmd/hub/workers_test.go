package main

import (
	"os"
	"strings"
	"testing"
)

func TestProfilesLoadValidateAndOverride(t *testing.T) {
	dir := t.TempDir()
	global, point := dir+"/global.json", dir+"/point.json"
	os.WriteFile(global, []byte(`{"profiles":[{"id":"claude","name":"Claude","kind":"agent","command":["claude"]},{"id":"rails","name":"Rails Server","kind":"accessory","command":["bin/rails","server"],"cwd":"/tmp"}]}`), 0o600)
	os.WriteFile(point, []byte(`{"profiles":[{"id":"rails","name":"Rails (api)","kind":"accessory","command":["bin/rails","server","-p","4000"]}]}`), 0o600)
	h := &Hub{}
	h.cfg.Profiles = global
	byID, order, err := h.loadProfiles()
	if err != nil || strings.Join(order, ",") != "shell,claude,rails" || byID["rails"].Cwd != "/tmp" || byID["shell"].Kind != "accessory" {
		t.Fatalf("global: %v %v %v", byID, order, err)
	}
	// A later file (a spawn point) replaces a profile by id and keeps the first position.
	files := []string{global, point}
	byID, order, err = h.loadFrom(files)
	if err != nil || strings.Join(order, ",") != "shell,claude,rails" || byID["rails"].Name != "Rails (api)" {
		t.Fatalf("override: %v %v %v", byID, order, err)
	}
	// A file replaces the built-in shell and keeps its first position.
	os.WriteFile(point, []byte(`{"profiles":[{"id":"shell","name":"Fish","kind":"accessory","command":["/opt/homebrew/bin/fish"]}]}`), 0o600)
	byID, order, err = h.loadFrom(files)
	if err != nil || order[0] != "shell" || byID["shell"].Name != "Fish" {
		t.Fatalf("shell override: %v %v %v", byID, order, err)
	}
	for _, bad := range []string{
		`{"profiles":[{"id":"Bad","name":"x","kind":"agent","command":["x"]}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"tool","command":["x"]}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"agent","command":[]}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["x"],"cwd":"rel"}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["x"],"env":{"LAZYREX_AGENT":"y"}}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["x"],"restart":"always"}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["x"]},{"id":"x","name":"y","kind":"agent","command":["y"]}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["x","say ${task}"],"args":[{"name":"task"}]}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["x","${task}"]}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["x"],"args":[{"name":"task"}]}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["${task}"],"args":[{"name":"task"}]}]}`,
		`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["x","${task}"],"args":[{"name":"task"},{"name":"task"}]}]}`,
	} {
		os.WriteFile(global, []byte(bad), 0o600)
		if _, _, err := h.loadProfiles(); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if got := stripCount("Team (33)"); got != "Team" {
		t.Fatalf("stripCount: %q", got)
	}
}

func TestResolveArgs(t *testing.T) {
	p := Profile{ID: "rails", Command: []string{"rails", "${port}", "${env}"}, Args: []ProfileArg{{Name: "port", Default: "3000"}, {Name: "env", Required: true}}}
	if _, err := resolveArgs(p, map[string]string{}); err == nil {
		t.Fatal("missing required arg accepted")
	}
	if _, err := resolveArgs(p, map[string]string{"env": "dev", "other": "x"}); err == nil {
		t.Fatal("undeclared arg accepted")
	}
	got, err := resolveArgs(p, map[string]string{"env": "dev"})
	if err != nil || got["port"] != "3000" || got["env"] != "dev" {
		t.Fatalf("resolve: %v %v", got, err)
	}
}
