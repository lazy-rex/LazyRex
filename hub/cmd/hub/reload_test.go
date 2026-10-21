package main

import (
	"path/filepath"
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
