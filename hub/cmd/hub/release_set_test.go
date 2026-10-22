package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	setFrom = "1111111111111111111111111111111111111111"
	setTo   = "2222222222222222222222222222222222222222"
)

// setEntry is a valid entry as a map; tests change it, then seal it with a matching review.
func setEntry(plugin string) map[string]any {
	return map[string]any{
		"plugin": plugin, "from": setFrom, "to": setTo,
		"seed": []any{map[string]any{"kind": "action", "action": "x.save", "args": map[string]any{"name": "a<b>&c", "n": 1.50}}},
		"probe": []any{
			map[string]any{"kind": "action", "action": "x.list", "args": map[string]any{}, "expect": []any{
				map[string]any{"pointer": "/items/0/name", "value": "a<b>&c"},
				map[string]any{"pointer": "/pending", "empty": true}}},
			map[string]any{"kind": "sql", "sql": "SELECT name FROM items", "rows": []any{[]any{"a<b>&c"}}},
		},
		"keys":    map[string]any{"items": []any{"name"}},
		"changes": []any{},
		"inserts": []any{},
	}
}

// viewItem is a valid view item with the operation key key.
func viewItem(key string) map[string]any {
	return map[string]any{"kind": "view", "pane_dir": "<LAB>/src", "command": "profiles.start_accessory", "route": "/forms/start-accessory",
		"node_id": "dir_choices", "action": "default", "input": map[string]any{"item_id": "default"}, "operation_key": key,
		"expect": []any{map[string]any{"pointer": "", "empty": true}}}
}

// seal adds a review whose hash matches the entry.
func seal(t *testing.T, e map[string]any) map[string]any {
	t.Helper()
	delete(e, "review")
	b, _ := json.Marshal(e)
	h, err := setEntryHash(b)
	if err != nil {
		t.Fatal(err)
	}
	e["review"] = map[string]any{"by": "owner", "msg_id": "send/1", "entry_sha256": h, "from": e["from"], "to": e["to"]}
	return e
}

func setBytes(entries ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"plugins": entries})
	return b
}

func TestReleaseSetParse(t *testing.T) {
	p := setEntry("profiles")
	p["seed"] = []any{viewItem("rehearsal-seed-1")}
	p["probe"] = append(p["probe"].([]any), viewItem("rehearsal-probe-from"))
	set, err := parseSet(setBytes(seal(t, p), seal(t, setEntry("templates"))))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Plugins) != 2 || set.Plugins[0].Plugin != "profiles" || set.Plugins[1].Plugin != "templates" || set.Plugins[0].hash == "" {
		t.Fatalf("set %+v", set)
	}
	if it := set.Plugins[0].Seed[0]; it.Kind != "view" || it.OperationKey != "rehearsal-seed-1" || it.NodeID != "dir_choices" {
		t.Fatalf("seed item %+v", it)
	}
	if rows := set.Plugins[1].Probe[1].Rows; rows == nil || len(*rows) != 1 {
		t.Fatalf("sql rows %v", rows)
	}
}

// TestReleaseSetHashGolden fixes the canonical form: sorted keys, compact, numbers as written, no
// HTML escapes, without "review". A change of the form changes this hash, and every owner review.
func TestReleaseSetHashGolden(t *testing.T) {
	raw := []byte(`{"to":"` + setTo + `", "plugin":"profiles","from":"` + setFrom + `","seed":[{"kind":"action","action":"x.save","args":{"n":1.50,"name":"a<b>&c"}}],` +
		`"probe":[{"kind":"action","action":"x.list","expect":[{"pointer":"/items/0/name","value":"a<b>&c"}]}],"keys":{"items":["name"]},"changes":[],"inserts":[],"review":{"by":"x"}}`)
	c, h, err := setEntryCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"changes":[],"from":"` + setFrom + `","inserts":[],"keys":{"items":["name"]},"plugin":"profiles",` +
		`"probe":[{"action":"x.list","expect":[{"pointer":"/items/0/name","value":"a<b>&c"}],"kind":"action"}],` +
		`"seed":[{"action":"x.save","args":{"n":1.50,"name":"a<b>&c"},"kind":"action"}],"to":"` + setTo + `"}`
	if string(c) != want {
		t.Fatalf("canonical\n%s\nwant\n%s", c, want)
	}
	if h != "6158b196d0908dcb395585ddedc6c1be5bdcbedea6e489fc07b5f22a31169267" {
		t.Fatalf("hash %s", h)
	}
}

// TestReleaseSetHashStep: set-hash prints the gate's hash and the exact canonical bytes, and
// reports a broken entry, but not a missing review.
func TestReleaseSetHashStep(t *testing.T) {
	dir := t.TempDir()
	good := setEntry("profiles")
	bad := setEntry("templates")
	bad["probe"] = []any{}
	f := filepath.Join(dir, "set.json")
	os.WriteFile(f, setBytes(good, bad), 0o600)
	var out, errb bytes.Buffer
	code := releaseMain([]string{"set-hash", "-set", f}, &out, &errb)
	if code != relFailed || !strings.Contains(errb.String(), "entry 2 (templates): no probe") || strings.Contains(errb.String(), "review") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
	b, _ := json.Marshal(good)
	c, h, _ := setEntryCanonical(b)
	if !strings.HasPrefix(out.String(), "plugin profiles entry_sha256 "+h+"\n"+string(c)+"\n") {
		t.Fatalf("stdout %q", out.String())
	}
	os.WriteFile(f, setBytes(good), 0o600)
	out.Reset()
	errb.Reset()
	if code := releaseMain([]string{"set-hash", "-set", f}, &out, &errb); code != relOK {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
	if code := releaseMain([]string{"set-hash"}, &out, &errb); code != relUsage {
		t.Fatalf("no -set: code %d", code)
	}
	// An empty set: parseSet refuses it, so set-hash does too (codex-26, review of c2aa0bb).
	for _, empty := range []string{`{"plugins":[]}`, `{}`} {
		os.WriteFile(f, []byte(empty), 0o600)
		out.Reset()
		errb.Reset()
		if code := releaseMain([]string{"set-hash", "-set", f}, &out, &errb); code != relFailed || !strings.Contains(errb.String(), "no plugins") || out.Len() != 0 {
			t.Fatalf("%s: code %d stdout %q stderr %q", empty, code, out.String(), errb.String())
		}
	}
}

func TestReleaseSetRefused(t *testing.T) {
	item := func(e map[string]any, field string, v any) { e["probe"].([]any)[0].(map[string]any)[field] = v }
	for name, tc := range map[string]struct {
		mod  func(e map[string]any) // before the seal
		post func(e map[string]any) // after the seal
		more map[string]any         // a second entry (sealed)
		want string
	}{
		"unknown field":   {mod: func(e map[string]any) { e["destructive"] = []any{} }, want: "unknown field"},
		"bad name":        {mod: func(e map[string]any) { e["plugin"] = "Bad Name" }, want: "not allowed"},
		"short commit":    {mod: func(e map[string]any) { e["to"] = "abc" }, want: "40 lowercase hex"},
		"same commit":     {mod: func(e map[string]any) { e["to"] = setFrom }, want: "not a release step"},
		"no probe":        {mod: func(e map[string]any) { e["probe"] = []any{} }, want: "no probe"},
		"sql probe only":  {mod: func(e map[string]any) { e["probe"] = e["probe"].([]any)[1:] }, want: "a sql probe alone"},
		"no kind":         {mod: func(e map[string]any) { item(e, "kind", "") }, want: "is not action, view or sql"},
		"probe no expect": {mod: func(e map[string]any) { item(e, "expect", []any{}) }, want: "no expected value"},
		"bad pointer":     {mod: func(e map[string]any) { item(e, "expect", []any{map[string]any{"pointer": "items", "value": 1}}) }, want: "not a JSON pointer"},
		"value and empty": {mod: func(e map[string]any) {
			item(e, "expect", []any{map[string]any{"pointer": "/a", "value": 1, "empty": true}})
		}, want: "exactly one of value and empty"},
		"neither":         {mod: func(e map[string]any) { item(e, "expect", []any{map[string]any{"pointer": "/a"}}) }, want: "exactly one of value and empty"},
		"hub action":      {mod: func(e map[string]any) { item(e, "action", "hub.sql.exec") }, want: "not a plugin action"},
		"args not object": {mod: func(e map[string]any) { item(e, "args", []any{1}) }, want: "not a JSON object"},
		"action with sql": {mod: func(e map[string]any) { item(e, "sql", "SELECT 1") }, want: "only action, args and expect"},
		"sql seed": {mod: func(e map[string]any) {
			e["seed"] = []any{map[string]any{"kind": "sql", "sql": "SELECT 1", "rows": []any{}}}
		}, want: "probe only"},
		"sql not select": {mod: func(e map[string]any) { e["probe"].([]any)[1].(map[string]any)["sql"] = "DELETE FROM items" }, want: "not a SELECT"},
		"sql no rows":    {mod: func(e map[string]any) { delete(e["probe"].([]any)[1].(map[string]any), "rows") }, want: "needs rows"},
		"view outside":   {mod: func(e map[string]any) { v := viewItem("k"); v["pane_dir"] = "/Users/x"; e["seed"] = []any{v} }, want: "not a lab directory"},
		"view no key":    {mod: func(e map[string]any) { v := viewItem(""); e["seed"] = []any{v} }, want: "needs an operation_key"},
		"view no node":   {mod: func(e map[string]any) { v := viewItem("k"); delete(v, "node_id"); e["seed"] = []any{v} }, want: "needs a command, a route"},
		"key twice":      {mod: func(e map[string]any) { e["seed"] = []any{viewItem("k"), viewItem("k")} }, want: "operation_key \"k\" twice"},
		"key twice in set": {mod: func(e map[string]any) { e["seed"] = []any{viewItem("k")} }, more: func() map[string]any {
			e := setEntry("templates")
			e["seed"] = []any{viewItem("k")}
			return e
		}(), want: "operation_key \"k\" twice"},
		"rowid key":     {mod: func(e map[string]any) { e["keys"] = map[string]any{"items": []any{"rowid"}} }, want: "not a stable key"},
		"hub table key": {mod: func(e map[string]any) { e["keys"] = map[string]any{"lazyrex_ops": []any{"key"}} }, want: "the keys of table"},
		"change no key": {mod: func(e map[string]any) {
			e["changes"] = []any{map[string]any{"table": "items", "column": "n", "expect": 1}}
		}, want: "needs a table, a key"},
		"change bad sql": {mod: func(e map[string]any) {
			e["changes"] = []any{map[string]any{"table": "items", "key": map[string]any{"name": "a"}, "column": "n", "expect": map[string]any{"x": 1}}}
		}, want: `{"sql"`},
		"insert no inv": {mod: func(e map[string]any) { e["inserts"] = []any{map[string]any{"table": "items", "max": 1}} }, want: "invariants"},
		"insert not select": {mod: func(e map[string]any) {
			e["inserts"] = []any{map[string]any{"table": "items", "max": 1, "invariants": []any{"DELETE FROM items"}}}
		}, want: "not a SELECT"},
		"no review":          {post: func(e map[string]any) { delete(e, "review") }, want: "no review"},
		"review other pins":  {post: func(e map[string]any) { e["review"].(map[string]any)["to"] = setFrom }, want: "the review is of"},
		"entry changed":      {post: func(e map[string]any) { e["seed"] = []any{} }, want: "the review is of entry"},
		"review no reviewer": {post: func(e map[string]any) { e["review"].(map[string]any)["by"] = "" }, want: "no reviewer"},
		"duplicate":          {more: setEntry("profiles"), want: "twice"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setEntry("profiles")
			if tc.mod != nil {
				tc.mod(e)
			}
			seal(t, e)
			if tc.post != nil {
				tc.post(e)
			}
			entries := []map[string]any{e}
			if tc.more != nil {
				entries = append(entries, seal(t, tc.more))
			}
			if _, err := parseSet(setBytes(entries...)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want an error with %q", err, tc.want)
			}
		})
	}
	if _, err := parseSet(append(setBytes(seal(t, setEntry("profiles"))), []byte(`{"plugins":[]}`)...)); err == nil || !strings.Contains(err.Error(), "more data") {
		t.Fatalf("trailing data: %v", err)
	}
	if _, err := parseSet([]byte(`{"plugins":[]}`)); err == nil || !strings.Contains(err.Error(), "names no plugin") {
		t.Fatalf("empty set: %v", err)
	}
}
