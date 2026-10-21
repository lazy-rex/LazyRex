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

// Unit tests of the managed file (docs/plugin-sources.md, section 13): its parse, the managed
// entries beside the config entries, and the hub.plugins report.

// writeManaged writes the default managed file <dir>/plugins.json (writeConfig sets state = dir).
func writeManaged(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "plugins.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A malformed managed file never fails the hub start: the config entries stay, and no managed
// entry exists. A missing file is an empty list.
func TestManagedFileMalformed(t *testing.T) {
	dir := t.TempDir()
	plugDir(t, dir, "a", `{"name":"a","main":"a.lua"}`, "a.lua")
	abs := filepath.Join(dir, "a")
	bad := map[string]string{
		`{"plugins":[`: `unexpected EOF`,
		`{"plugins":[{"id":"x","source":"` + abs + `","user_messaging":true}]}`: `unknown field "user_messaging"`,
		`{"plugins":[],"prompts":{}}`:                                          `unknown field "prompts"`,
		`{"plugins":[{"id":"X","source":"` + abs + `"}]}`:                      `plugin 1: id "X" must match [a-z0-9_-]{1,64}`,
		`{"plugins":[{"source":"` + abs + `"}]}`:                               `plugin 1: id "" must match`,
		`{"plugins":[{"id":"x","source":"` + abs + `"},{"id":"x","source":"/other"}]}`: `managed:x: the id is used twice`,
		`{"plugins":[{"id":"x","source":"` + abs + `"},{"id":"y","source":"` + abs + `"}]}`: `managed:y: the source ` + abs + ` is used twice`,
		`{"plugins":[{"id":"x","source":"a"}]}`:                                         `managed:x: source "a" must be an absolute directory`,
		`{"plugins":[{"id":"x","source":"~/a"}]}`:                                       `managed:x: source "~/a" must be an absolute directory`,
		`{"plugins":[{"id":"x","source":"github:o/r"}]}`:                                `managed:x: source github:o/r: write github:<owner>/<repo>@<ref>`,
		`{"plugins":[{"id":"x","source":"` + abs + `","args":{"plugin_session":"s"}}]}`: `managed:x: arg "plugin_session" is not allowed`,
		`{"plugins":[{"id":"x","source":"` + abs + `","watch_roots":["r"]}]}`:          `managed:x: watch root "r" must be absolute and clean`,
		`{"plugins":[]} {}`: `data after the manifest object`,
		// A removal needs an explicit list: a root that is not the object, or no plugins array, is
		// malformed (reviewer, hub-review-2e1-null-blocker).
		`null`:                `the file must be an object with a plugins array`,
		`{}`:                  `the file must be an object with a plugins array`,
		`{"plugins":null}`:    `the file must be an object with a plugins array`,
		`[]`:                  `cannot unmarshal array`,
		`{"plugins":{}}`:      `cannot unmarshal object`,
	}
	for body, want := range bad {
		writeManaged(t, dir, body)
		c, err := writeConfig(t, dir, `[{"source":"a"}]`)
		if err != nil {
			t.Fatalf("%s: the hub start fails: %v", body, err)
		}
		if c.managedErr == nil || !strings.Contains(c.managedErr.Error(), want) || !strings.HasPrefix(c.managedErr.Error(), "managed plugin file "+filepath.Join(dir, "plugins.json")+": ") {
			t.Errorf("%s: got %v, want %q", body, c.managedErr, want)
		}
		if len(c.entries) != 1 || c.entries[0].managed != "" {
			t.Errorf("%s: entries %d; want the config entry only", body, len(c.entries))
		}
	}
	// The limit: exactly 1 MiB is accepted, one byte more is refused.
	empty := `{"plugins":[]}`
	writeManaged(t, dir, empty+strings.Repeat(" ", managedFileLimit-len(empty)))
	if c, _ := writeConfig(t, dir, `[]`); c.managedErr != nil {
		t.Errorf("a file of exactly %d bytes: %v", managedFileLimit, c.managedErr)
	}
	writeManaged(t, dir, empty+strings.Repeat(" ", managedFileLimit-len(empty)+1))
	if c, _ := writeConfig(t, dir, `[]`); c.managedErr == nil || !strings.Contains(c.managedErr.Error(), "over the 1048576-byte limit") {
		t.Errorf("a file of %d bytes: %v", managedFileLimit+1, c.managedErr)
	}
	os.Remove(filepath.Join(dir, "plugins.json"))
	if c, err := writeConfig(t, dir, `[{"source":"a"}]`); err != nil || c.managedErr != nil || len(c.entries) != 1 {
		t.Errorf("a missing file: %v, %v, %d entries", err, c.managedErr, len(c.entries))
	}
}

// Managed entries come after the config entries, in file order. A managed source that the config
// also names (the same directory, also written another way) is refused; the config entry
// resolves. A name in both files refuses both, with the managed identity in the text. The config
// field managed_plugins is relative to the config directory.
func TestManagedEntries(t *testing.T) {
	dir := t.TempDir()
	plugDir(t, dir, "a", `{"name":"a","main":"a.lua"}`, "a.lua")
	plugDir(t, dir, "m/b", `{"name":"b","main":"b.lua","depends":["a"]}`, "b.lua")
	plugDir(t, dir, "m/dup", `{"name":"a","main":"d.lua"}`, "d.lua")
	plugDir(t, dir, "m/c", `{"name":"c","main":"c.lua"}`, "c.lua")
	os.MkdirAll(filepath.Join(dir, "conf"), 0o700)
	os.WriteFile(filepath.Join(dir, "conf", "managed.json"), []byte(`{"plugins":[
		{"id":"b","source":"`+filepath.Join(dir, "m", "b")+`"},
		{"id":"again","source":"`+filepath.Join(dir, "a")+`/"},
		{"id":"c","source":"`+filepath.Join(dir, "m", "c")+`","args":{"k":"v"}}]}`), 0o600)
	body := `{"server":"unix:///tmp/lrt-x/rex.sock","state":"` + dir + `","agents":[{"id":"user","name":"User"}],
		"managed_plugins":"../conf/managed.json","plugins":[{"source":"a"}]}`
	os.MkdirAll(filepath.Join(dir, "cfg"), 0o700)
	p := filepath.Join(dir, "cfg", "hub.json")
	os.WriteFile(p, []byte(body), 0o600)
	plugDir(t, dir, "cfg/a", `{"name":"a","main":"a.lua"}`, "a.lua") // config source "a" is relative to cfg/
	c, err := loadConfig(p, false)
	if err != nil || c.managedErr != nil {
		t.Fatal(err, c.managedErr)
	}
	if c.managedPath != filepath.Join(dir, "conf", "managed.json") {
		t.Errorf("managed path %s", c.managedPath)
	}
	var ids []string
	for _, e := range c.entries {
		ids = append(ids, e.identity())
	}
	if strings.Join(ids, ",") != "entry 1,managed:b,managed:again,managed:c" {
		t.Errorf("entries %v", ids)
	}
	order := resolveEntries(p, c.entries, nil)
	// managed:again names <dir>/a, and the config entry names <dir>/cfg/a: two sources. Both
	// manifests give the name a, so both entries are refused, and b (depends on a) with them.
	dup := "entries 1 and managed:again have the same name a"
	want := []string{dup, "plugin b: dependency a is refused: " + dup, dup, ""}
	for i, e := range c.entries {
		if got := fmt.Sprint(e.err); (want[i] == "" && e.err != nil) || (want[i] != "" && got != want[i]) {
			t.Errorf("%s: %v, want %q", e.identity(), e.err, want[i])
		}
	}
	var names []string
	for _, e := range order {
		names = append(names, e.identity())
	}
	// The passing entries, then the named refused ones (b binds its name; entry 1 and again do not).
	if strings.Join(names, ",") != "managed:c,managed:b" {
		t.Errorf("order %v", names)
	}

	// The same directory in both files: the managed entry is refused, the config entry resolves.
	os.WriteFile(filepath.Join(dir, "conf", "managed.json"), []byte(`{"plugins":[
		{"id":"same","source":"`+filepath.Join(dir, "cfg", "a")+`/"}]}`), 0o600)
	c, err = loadConfig(p, false)
	if err != nil {
		t.Fatal(err)
	}
	resolveEntries(p, c.entries, nil)
	if c.entries[0].err != nil || c.entries[0].name != "a" {
		t.Errorf("the config entry: %v, name %q", c.entries[0].err, c.entries[0].name)
	}
	wantSame := "managed:same: the source " + filepath.Join(dir, "cfg", "a") + "/ is also in the config"
	if got := c.entries[1].err; got == nil || got.Error() != wantSame {
		t.Errorf("the same source: %v, want %s", got, wantSame)
	}
}

// hub.plugins reports a managed entry by its id, and a malformed managed file once, in refused.
func TestManagedPluginsList(t *testing.T) {
	h, _ := workerTestHub(t)
	h.cfg.managedPath = "/m/plugins.json"
	h.managedErr = os.ErrInvalid
	h.entries = []*configEntry{{managed: "notes", Source: "/s/notes", seen: "notes", err: os.ErrPermission}}
	h.plugins = map[string]*Plugin{}
	out, err := h.pluginsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var got struct {
		Refused []map[string]any
		Entries []map[string]any
	}
	json.Unmarshal(b, &got)
	e, _ := json.Marshal(got.Entries)
	if string(e) != `[{"candidate":{"error":"permission denied"},"error":"permission denied","managed":"notes","name":"notes","source":"/s/notes","state":"refused"}]` {
		t.Errorf("entries %s", e)
	}
	r, _ := json.Marshal(got.Refused)
	if string(r) != `[{"error":"permission denied","managed":"notes","name":"notes","source":"/s/notes","state":"refused"},{"error":"invalid argument","managed_file":"/m/plugins.json"}]` {
		t.Errorf("refused %s", r)
	}
}

// hub.admin.start {source} with one exact source in the config and in the managed file selects
// the config entry, which owns the source; the managed duplicate is refused (reviewer,
// hub-review-2e2-findings-1).
func TestAdminStartSourcePrefersConfigEntry(t *testing.T) {
	dir := t.TempDir()
	plugDir(t, dir, "a", `{"name":"a","main":"a.lua"}`, "a.lua")
	abs := filepath.Join(dir, "a")
	writeManaged(t, dir, `{"plugins":[{"id":"dup","source":"`+abs+`"}]}`)
	c, err := writeConfig(t, dir, `[{"source":"`+abs+`"}]`)
	if err != nil || c.managedErr != nil {
		t.Fatal(err, c.managedErr)
	}
	h, _ := workerTestHub(t)
	h.cfg = c
	h.entries = c.entries
	h.plugins = map[string]*Plugin{}
	h.entriesMu.Lock()
	h.resolveLocked()
	h.entriesMu.Unlock()
	if c.entries[1].err == nil {
		t.Fatal("the managed duplicate is not refused")
	}
	pl, err := h.adminTarget("hub.admin.start", map[string]any{"source": abs})
	if err != nil || pl == nil || pl.Name != "a" || pl.entry != c.entries[0] {
		t.Errorf("adminTarget: %v, %v", pl, err)
	}
}
