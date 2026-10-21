package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A changed managed id takes every parsed field of the new entry, and keeps its bound name: a
// GitHub source with another subdir or ref resolves the new one (owner, repo, subdir, gitRef).
func TestUpdateManagedTakesEveryParsedField(t *testing.T) {
	dir := t.TempDir()
	parse := func(src string) *configEntry {
		writeManaged(t, dir, `{"plugins":[{"id":"x","source":"`+src+`","args":{"k":"v"}}]}`)
		list, err := parseManaged(filepath.Join(dir, "plugins.json"))
		if err != nil || len(list) != 1 {
			t.Fatal(list, err)
		}
		return list[0]
	}
	o := parse("github:o/r/one@v1")
	o.name = "bound"
	m := parse("github:p/s/two@v2")
	updateManaged(o, m)
	if o.owner != "p" || o.repo != "s" || o.subdir != "two" || o.gitRef != "v2" || o.Source != m.Source || o.Args["k"] != "v" {
		t.Errorf("after the update: owner %q repo %q subdir %q ref %q source %q", o.owner, o.repo, o.subdir, o.gitRef, o.Source)
	}
	if o.name != "bound" || o.managed != "x" {
		t.Errorf("the update changed the identity: name %q managed %q", o.name, o.managed)
	}
}

// hub.plugins.reload from an agent pane, or from a plugin pane process that is not the registered
// instance, is refused before the reload reads the managed file. An operator (no pane) passes the
// guard.
func TestReloadRefusesPaneCallers(t *testing.T) {
	h, _ := workerTestHub(t)
	dir := t.TempDir()
	h.cfg.managedPath = filepath.Join(dir, "plugins.json")
	writeManaged(t, dir, `{"plugins":[`) // malformed: a reload that runs reports it
	pid := os.Getpid()
	var p perform
	json.Unmarshal([]byte(fmt.Sprintf(`{"action":"hub.plugins.reload","requester":{"client_id":"c9","principal":{"pid":%d}}}`, pid)), &p)
	want := "hub.plugins.reload is refused from agent panes and from plugin processes that are not the registered instance"
	cases := map[string]func(){
		"an agent pane": func() {
			h.agents = map[string]*Agent{"x": {BlockID: "b1", ChildPID: pid, ChildStart: startTime(pid)}}
			h.plugins = map[string]*Plugin{}
		},
		"a plugin pane, not the registered instance": func() {
			h.agents = map[string]*Agent{}
			h.plugins = map[string]*Plugin{"p": {Name: "p", entry: &configEntry{name: "p"}, ClientID: "c1", ChildPID: pid, ChildStart: startTime(pid)}}
		},
	}
	for name, set := range cases {
		set()
		_, err := h.reloadAction(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
		if h.managedErr != nil {
			t.Errorf("%s: the reload ran: %v", name, h.managedErr)
		}
	}
	h.agents, h.plugins = map[string]*Agent{}, map[string]*Plugin{}
	if _, err := h.reloadAction(context.Background(), p); !isRefusal(err, "invalid") || h.managedErr == nil {
		t.Errorf("an operator: %v (the reload must run and report the malformed file)", err)
	}
}
