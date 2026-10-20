package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes dir/hub.json with the given plugins field and loads it.
func writeConfig(t *testing.T, dir, plugins string) (Config, error) {
	t.Helper()
	p := filepath.Join(dir, "hub.json")
	body := `{"server":"unix:///tmp/lrt-x/rex.sock","state":"` + dir + `","agents":[{"id":"user","name":"User"}],"plugins":` + plugins + `}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadConfig(p, false)
}

// A malformed plugins field stops hub start (docs/plugin-sources.md, section 1).
func TestPluginEntriesMalformed(t *testing.T) {
	dir := t.TempDir()
	bad := map[string]string{
		`[{"source":"a","name":"a"}]`:                    `plugin entry 1: "name" is removed: use "source" (a directory with lazyrex.json, or github:owner/repo@ref)`,
		`[{"source":"a"},{"path":"a.lua"}]`:              `plugin entry 2: "path" is removed`,
		`[{"source":"a","events":"all"}]`:                `plugin entry 1: "events" is removed`,
		`[{"source":"a","notice":"x"}]`:                  `plugin entry 1: "notice" is removed`,
		`[{"source":"a","color":"red"}]`:                 `plugin entry 1: json: unknown field "color"`,
		`[{"args":{}}]`:                                  `plugin entry 1: source is required`,
		`[{"source":"~/Messaging"}]`:                     `plugin entry 1: source ~/Messaging: ~ is not expanded`,
		`[{"source":"github:lazy-rex/Messaging"}]`:       `write github:<owner>/<repo>@<ref>`,
		`[{"source":"github:lazy rex/Messaging@main"}]`:  `write github:<owner>/<repo>@<ref>`,
		`[{"source":"github:o/..@main"}]`:                `write github:<owner>/<repo>@<ref>`,
		`[{"source":"github:o/r@"}]`:                     `write github:<owner>/<repo>@<ref>`,
		`[{"source":"a"},{"source":"b"},{"source":"a"}]`: `entries 1 and 3 have the same source a`,
		`[{"source":"a","args":{"instance_id":"x"}}]`:    `plugin entry 1: arg "instance_id" is not allowed`,
		`[{"source":"a","watch_roots":["rel"]}]`:         `plugin entry 1: watch root "rel" must be absolute and clean`,
		`[7]`:                                            `plugin entry 1: json: cannot unmarshal`,
	}
	for plugins, want := range bad {
		if _, err := writeConfig(t, dir, plugins); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", plugins, err, want)
		}
	}
}

// Every other problem refuses one entry; the other entries resolve (section 7).
func TestPluginEntriesResolve(t *testing.T) {
	dir := t.TempDir()
	plugDir(t, dir, "plugins/m", `{"name":"messaging","main":"m.lua","notice":"New message. Call messaging_read.","events":"all"}`, "m.lua")
	plugDir(t, dir, "plugins/w", `{"name":"worktrees","main":"w.lua","depends":["profiles"]}`, "w.lua")
	plugDir(t, dir, "plugins/t", `{"name":"templates","main":"t.lua","depends":["messaging"]}`, "t.lua")
	plugDir(t, dir, "plugins/d1", `{"name":"dup","main":"d.lua"}`, "d.lua")
	plugDir(t, dir, "plugins/d2", `{"name":"dup","main":"d.lua"}`, "d.lua")
	os.MkdirAll(filepath.Join(dir, "plugins", "empty"), 0o700)
	abs := filepath.Join(dir, "plugins", "m")
	c, err := writeConfig(t, dir, `[{"source":"plugins/t","args":{"k":"v"}},{"source":"plugins/empty"},{"source":"github:lazy-rex/Profiles@main"},
		{"source":"plugins/w"},{"source":"`+abs+`","watch_roots":["/tmp/x"]},{"source":"plugins/d1"},{"source":"plugins/d2"}]`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range c.Plugins {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "templates,messaging" {
		t.Fatalf("resolved %v, refused %+v", names, c.Refused)
	}
	tp, mp := c.Plugins[0], c.Plugins[1]
	if tp.Dir != filepath.Join(dir, "plugins", "t") || tp.Path != filepath.Join(dir, "plugins", "t", "t.lua") || tp.Args["k"] != "v" || tp.Notice != "" || tp.Source != "plugins/t" {
		t.Errorf("a relative source resolves against the config directory: %+v", tp)
	}
	if mp.Dir != abs || mp.Notice != "New message. Call messaging_read." || mp.Events != "all" || mp.WatchRoots[0] != "/tmp/x" {
		t.Errorf("the manifest owns notice and events: %+v", mp)
	}
	if strings.Join(c.StartOrder, ",") != "messaging,templates" {
		t.Errorf("start order %v: dependencies first", c.StartOrder)
	}
	want := map[int]struct{ name, err string }{
		2: {"", "plugin source plugins/empty: no lazyrex.json in " + filepath.Join(dir, "plugins", "empty")},
		3: {"", "plugin github:lazy-rex/Profiles@main is not installed: run lazyrex-hub plugin sync -config " + filepath.Join(dir, "hub.json")},
		4: {"worktrees", "plugin worktrees: dependency profiles is not configured"},
		6: {"dup", "entries 6 and 7 have the same name dup"},
		7: {"dup", "entries 6 and 7 have the same name dup"},
	}
	if len(c.Refused) != len(want) {
		t.Fatalf("refused %+v", c.Refused)
	}
	for _, r := range c.Refused {
		w := want[r.Entry]
		if r.Name != w.name || r.Err == nil || r.Err.Error() != w.err {
			t.Errorf("entry %d: name %q, error %v; want %q, %q", r.Entry, r.Name, r.Err, w.name, w.err)
		}
	}
}

// A notify op from a plugin without a manifest notice fails with code no_notice, and nothing in
// the batch is written (section 10).
func TestNoNotice(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ops := map[string]any{"ops": []any{
		map[string]any{"key": "k", "value": "v"},
		map[string]any{"notify": map[string]any{"agent": "alice", "pending": true}},
	}}
	_, err = db.storeBatch(context.Background(), "p", ops, map[string]bool{"alice": true}, false)
	var se *storeError
	if !errors.As(err, &se) || se.code != "no_notice" || !strings.Contains(err.Error(), "plugin p has no notice in its manifest") {
		t.Fatalf("got %v", err)
	}
	var n int
	db.QueryRow(`SELECT (SELECT count(*) FROM records) + (SELECT count(*) FROM notices)`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows written", n)
	}
	if out, err := db.storeBatch(context.Background(), "p", ops, map[string]bool{"alice": true}, true); err != nil || !out.committed {
		t.Errorf("with a notice: %v", err)
	}
	h := &Hub{plugins: map[string]*Plugin{"p": {Name: "p"}, "q": {Name: "q", Notice: "x"}}}
	if h.noticed("p") || !h.noticed("q") || h.noticed("none") {
		t.Error("noticed")
	}
}
