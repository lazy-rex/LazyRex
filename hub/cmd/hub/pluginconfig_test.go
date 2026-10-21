package main

import (
	"context"
	"encoding/json"
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
	order := resolveEntries(noLock(c.path), c.entries, nil)
	var names []string
	for _, e := range order {
		names = append(names, e.name)
	}
	// The entries that pass in start order (dependencies first), then the named refused ones.
	if strings.Join(names, ",") != "messaging,templates,worktrees" {
		t.Fatalf("order %v", names)
	}
	tp, mp := c.entries[0].cand, c.entries[4].cand
	if tp == nil || mp == nil {
		t.Fatalf("candidates %+v %+v", tp, mp)
	}
	if tp.Dir != filepath.Join(dir, "plugins", "t") || tp.Path != filepath.Join(dir, "plugins", "t", "t.lua") || tp.Args["k"] != "v" || tp.Notice != "" || tp.Source != "plugins/t" {
		t.Errorf("a relative source resolves against the config directory: %+v", tp)
	}
	if mp.Dir != abs || mp.Notice != "New message. Call messaging_read." || mp.Events != "all" || mp.WatchRoots[0] != "/tmp/x" {
		t.Errorf("the manifest owns notice and events: %+v", mp)
	}
	// Duplicate names bind neither entry: no record gets an invented owner.
	want := map[int]struct{ name, err string }{
		1: {"templates", ""},
		2: {"", "plugin source plugins/empty: no lazyrex.json in " + filepath.Join(dir, "plugins", "empty")},
		3: {"", "plugin github:lazy-rex/Profiles@main is not installed: run lazyrex-hub plugin sync -config " + filepath.Join(dir, "hub.json")},
		4: {"worktrees", "plugin worktrees: dependency profiles is not configured"},
		5: {"messaging", ""},
		6: {"", "entries 6 and 7 have the same name dup"},
		7: {"", "entries 6 and 7 have the same name dup"},
	}
	for _, e := range c.entries {
		w, got := want[e.index], ""
		if e.err != nil {
			got = e.err.Error()
		}
		if e.name != w.name || got != w.err || (e.err == nil) != (e.cand != nil) {
			t.Errorf("entry %d: name %q, error %q; want %q, %q", e.index, e.name, got, w.name, w.err)
		}
	}
}

// Each start attempt resolves again (section 7, "The graph"). A bound name stays: a changed
// manifest name is refused with the restart text, and its dependents are refused. A duplicate name
// refuses every entry with it, also when one entry resolved earlier: the owner keeps its name
// (and a running instance), and the later entry binds nothing.
func TestPluginEntriesResolveAgain(t *testing.T) {
	dir := t.TempDir()
	plugDir(t, dir, "a", `{"name":"a","main":"a.lua"}`, "a.lua")
	plugDir(t, dir, "b", `{"name":"b","main":"b.lua","depends":["a"]}`, "b.lua")
	os.MkdirAll(filepath.Join(dir, "late"), 0o700)
	c, err := writeConfig(t, dir, `[{"source":"a"},{"source":"b"},{"source":"late"}]`)
	if err != nil {
		t.Fatal(err)
	}
	a, b, late := c.entries[0], c.entries[1], c.entries[2]
	resolveEntries(noLock(c.path), c.entries, nil)
	if a.name != "a" || b.name != "b" || late.name != "" || late.err == nil {
		t.Fatalf("first: %q %q %q %v", a.name, b.name, late.name, late.err)
	}
	// The late entry gets a manifest with the name a: both entries are refused; a keeps its name.
	plugDir(t, dir, "late", `{"name":"a","main":"l.lua"}`, "l.lua")
	resolveEntries(noLock(c.path), c.entries, nil)
	if late.name != "" || late.err == nil || late.err.Error() != "entries 1 and 3 have the same name a" {
		t.Errorf("late: %q %v", late.name, late.err)
	}
	if a.name != "a" || a.cand != nil || a.err == nil || a.err.Error() != "entries 1 and 3 have the same name a" {
		t.Errorf("the owner: %q %v", a.name, a.err)
	}
	if b.err == nil || b.err.Error() != "plugin b: dependency a is refused: entries 1 and 3 have the same name a" {
		t.Errorf("the dependent: %v", b.err)
	}
	// a's manifest changes its name.
	plugDir(t, dir, "a", `{"name":"a2","main":"a.lua"}`, "a.lua")
	order := resolveEntries(noLock(c.path), c.entries, nil)
	if a.name != "a" || a.cand != nil || a.err == nil || a.err.Error() != "plugin a: the manifest name changed from a to a2; restart the hub" {
		t.Errorf("renamed: %q %v", a.name, a.err)
	}
	if b.err == nil || b.err.Error() != "plugin b: dependency a is refused: plugin a: the manifest name changed from a to a2; restart the hub" {
		t.Errorf("the dependent: %v", b.err)
	}
	if late.name != "" || late.err == nil {
		t.Errorf("late after the rename: %q %v", late.name, late.err)
	}
	if len(order) != 2 || order[0] != a || order[1] != b {
		t.Errorf("order: the refused named entries in config order: %v", order)
	}
	// A repair: the late entry loses its manifest, and a has the old name again: a passes.
	os.Remove(filepath.Join(dir, "late", "lazyrex.json"))
	plugDir(t, dir, "a", `{"name":"a","main":"a.lua","notice":"Now with a notice."}`, "a.lua")
	resolveEntries(noLock(c.path), c.entries, nil)
	if a.err != nil || a.cand == nil || a.cand.Notice != "Now with a notice." || b.err != nil {
		t.Errorf("repaired: %v %v", a.err, b.err)
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
	h := &Hub{plugins: map[string]*Plugin{"p": {Name: "p"}, "q": {Name: "q", run: pluginRun{Name: "q", Notice: "x"}}}}
	if h.noticed("p") || !h.noticed("q") || h.noticed("none") {
		t.Error("noticed")
	}
}

// hub.register from a retained instance fails with code retained (section 7, "Retained instance").
// The other checks keep their order: an unknown plugin, then a stale instance.
func TestRegisterRetained(t *testing.T) {
	h := &Hub{plugins: map[string]*Plugin{"p": {Name: "p", entry: &configEntry{}, InstanceID: "p-1-x", ChildPID: os.Getpid(), state: "retained"}}}
	if _, err := h.registerTarget("p", "p-1-x", os.Getpid()); codeOf(err) != "retained" || err.Error() != "retained: instance p-1-x of p is retained after a refused start" {
		t.Errorf("retained: %v", err)
	}
	if _, err := h.registerTarget("p", "p-0-old", os.Getpid()); err == nil || codeOf(err) != "" || !strings.Contains(err.Error(), "is not the current instance") {
		t.Errorf("stale: %v", err)
	}
	if _, err := h.registerTarget("q", "q-1", os.Getpid()); err == nil || !strings.Contains(err.Error(), `unknown plugin "q"`) {
		t.Errorf("unknown: %v", err)
	}
	h.plugins["p"].state = ""
	if pl, err := h.registerTarget("p", "p-1-x", os.Getpid()); err != nil || pl != h.plugins["p"] {
		t.Errorf("current: %v", err)
	}
}

// hub.plugins reports each entry with its state (section 7, "States"). The refused list keeps its
// shape: entries in state refused, with the manifest name when one named the entry.
func TestPluginsListStates(t *testing.T) {
	h, _ := workerTestHub(t)
	boom := errors.New("plugin x: boom")
	es := []*configEntry{
		{index: 1, Source: "s1", err: errors.New("plugin source s1: no lazyrex.json")},
		{index: 2, Source: "s2", seen: "dup", err: errors.New("entries 2 and 3 have the same name dup")},
		{index: 3, Source: "s3", name: "run", seen: "run", err: boom},
		{index: 4, Source: "s4", name: "kept", seen: "kept", err: boom},
		{index: 5, Source: "s5", name: "off", seen: "off"},
		{index: 6, Source: "s6", name: "dep", seen: "dep"},
	}
	h.entries = es
	h.plugins = map[string]*Plugin{
		"run":  {Name: "run", entry: es[2], Desired: "running", run: pluginRun{Name: "run", Dir: "/d/run"}},
		"kept": {Name: "kept", entry: es[3], Desired: "running", state: "retained", refusal: "plugin kept: old", run: pluginRun{Name: "kept", Dir: "/d/kept"}},
		"off":  {Name: "off", entry: es[4], Desired: "stopped"},
		"dep":  {Name: "dep", entry: es[5], Desired: "running", state: "waiting", waiting: []string{"run", "off"}},
	}
	out, err := h.pluginsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var got struct {
		Plugins []any
		Refused []map[string]any
		Entries []map[string]any
	}
	json.Unmarshal(b, &got)
	want := []string{
		`{"entry":1,"error":"plugin source s1: no lazyrex.json","name":null,"source":"s1","state":"refused"}`,
		`{"entry":2,"error":"entries 2 and 3 have the same name dup","name":"dup","source":"s2","state":"refused"}`,
	}
	if len(got.Refused) != len(want) {
		t.Fatalf("refused %v", got.Refused)
	}
	for i, r := range got.Refused {
		if b, _ := json.Marshal(r); string(b) != want[i] {
			t.Errorf("refused %d: %s", i, b)
		}
	}
	wantEntries := []string{
		`{"candidate":{"error":"plugin source s1: no lazyrex.json"},"entry":1,"error":"plugin source s1: no lazyrex.json","name":null,"source":"s1","state":"refused"}`,
		`{"candidate":{"error":"entries 2 and 3 have the same name dup"},"entry":2,"error":"entries 2 and 3 have the same name dup","name":"dup","source":"s2","state":"refused"}`,
		`{"candidate":{"error":"plugin x: boom"},"dir":"/d/run","entry":3,"name":"run","source":"s3","state":"running"}`,
		`{"candidate":{"error":"plugin x: boom"},"dir":"/d/kept","entry":4,"error":"plugin kept: old","name":"kept","source":"s4","state":"retained"}`,
		`{"entry":5,"name":"off","source":"s5","state":"stopped"}`,
		`{"entry":6,"name":"dep","source":"s6","state":"waiting","waiting":["run","off"]}`,
	}
	for i, e := range got.Entries {
		if b, _ := json.Marshal(e); i >= len(wantEntries) || string(b) != wantEntries[i] {
			t.Errorf("entry %d: %s", i+1, b)
		}
	}
	if len(got.Plugins) != 0 || len(got.Entries) != len(wantEntries) {
		t.Errorf("plugins %v, %d entries", got.Plugins, len(got.Entries))
	}
}

// The ownership guard (reviewer, hub-review-2d1-ownership-boundary): a kept record (an instance or
// an intent with no entry) holds its name and its source. An entry with that source and another
// name, or that name from another source, is refused and binds nothing. The same name from the
// same source passes, and a kept record without a source (a legacy row) matches only its name. A
// duplicate name still refuses every entry with it.
func TestPluginEntriesGuard(t *testing.T) {
	dir := t.TempDir()
	plugDir(t, dir, "renamed", `{"name":"y","main":"r.lua"}`, "r.lua")
	plugDir(t, dir, "other", `{"name":"a","main":"o.lua"}`, "o.lua")
	plugDir(t, dir, "same", `{"name":"s","main":"s.lua"}`, "s.lua")
	plugDir(t, dir, "legacy", `{"name":"l","main":"l.lua"}`, "l.lua")
	plugDir(t, dir, "legacy2", `{"name":"m","main":"m.lua"}`, "m.lua")
	c, err := writeConfig(t, dir, `[{"source":"renamed"},{"source":"other"},{"source":"same"},{"source":"legacy"},{"source":"legacy2"}]`)
	if err != nil {
		t.Fatal(err)
	}
	kept := []keptName{{Name: "a", Source: "gone"}, {Name: "x", Source: "renamed"}, {Name: "s", Source: "same"}, {Name: "l", Source: ""}}
	resolveEntries(noLock(c.path), c.entries, kept)
	want := []struct{ name, err string }{
		{"", "plugin y: source renamed still has an instance of plugin x; stop x first"},
		{"", "plugin a: the name is held by an instance from source gone; stop a first"},
		{"s", ""},
		{"l", ""},
		{"m", ""}, // a legacy kept record is no wildcard for another name
	}
	for i, e := range c.entries {
		got := ""
		if e.err != nil {
			got = e.err.Error()
		}
		if e.name != want[i].name || got != want[i].err {
			t.Errorf("entry %d: %q %q; want %q %q", i+1, e.name, got, want[i].name, want[i].err)
		}
	}
	// The guard adds a refusal; it does not make a duplicate pass.
	plugDir(t, dir, "legacy2", `{"name":"s","main":"m.lua"}`, "m.lua")
	c2, err := writeConfig(t, dir, `[{"source":"same"},{"source":"legacy2"}]`)
	if err != nil {
		t.Fatal(err)
	}
	resolveEntries(noLock(c2.path), c2.entries, []keptName{{Name: "s", Source: "same"}})
	same, dup := c2.entries[0], c2.entries[1]
	if same.name != "" || same.err == nil || same.err.Error() != "entries 1 and 2 have the same name s" {
		t.Errorf("the owner's source: %q %v", same.name, same.err)
	}
	if dup.name != "" || dup.err == nil || dup.err.Error() != "plugin s: the name is held by an instance from source same; stop s first" {
		t.Errorf("the other source: %q %v", dup.name, dup.err)
	}
}

// A dependency counts as registered (section 7, "States") only with an entry, not retained, with a
// client from hub.register and a live child. An unresolved spawn intent gives no client.
func TestPluginRegisteredLocked(t *testing.T) {
	pid := os.Getpid()
	start := startTime(pid)
	e := &configEntry{index: 1, Source: "s"}
	h := &Hub{plugins: map[string]*Plugin{
		"ok":       {Name: "ok", entry: e, ClientID: "c1", ChildPID: pid, ChildStart: start},
		"kept":     {Name: "kept", ClientID: "c2", ChildPID: pid, ChildStart: start},
		"retained": {Name: "retained", entry: e, state: "retained", ClientID: "c3", ChildPID: pid, ChildStart: start},
		"intent":   {Name: "intent", entry: e, InstanceID: "intent-1-x", SessionID: "s1"},
		"dead":     {Name: "dead", entry: e, ClientID: "c4", ChildPID: pid, ChildStart: start + "x"},
	}}
	for name, want := range map[string]bool{"ok": true, "kept": false, "retained": false, "intent": false, "dead": false, "absent": false} {
		if got := h.registeredLocked(name); got != want {
			t.Errorf("%s: registered %v; want %v", name, got, want)
		}
	}
}
