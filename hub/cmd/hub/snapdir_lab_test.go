package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapLoader is a main.lua that loads the file at a path relative to its own directory, and writes
// the result to the file out (outside the lab's sources): its directory, then ok, then the error.
const snapLoader = `local dir = assert(debug.getinfo(1, 'S').source:match('^@?(/.*)/[^/]*$'), 'the main path is unknown')
local ok, err = pcall(dofile, dir .. '/' .. %q)
local f = assert(io.open(%q, 'w'))
f:write(dir .. '\n' .. tostring(ok) .. '\n' .. tostring(err))
f:close()
`

// A plugin is its source directory (the hub lead's ruling lead-snapdir; docs/plugin-sources.md,
// section 4): a local plugin runs from a snapshot of that directory only. A main that loads
// ../outside.lua fails with the missing file, although the file is in the source tree; a main that
// loads a file inside its own directory runs that file.
func TestLocalSnapshotHasOnlyItsDirLab(t *testing.T) {
	s := newStartLab(t, "out", "in")
	outRes, inRes := filepath.Join(s.l.dir, "out-load.txt"), filepath.Join(s.l.dir, "in-load.txt")
	s.writeSource("out", `{"name":"out","main":"main.lua"}`, "")
	s.writeSource("in", `{"name":"in","main":"main.lua"}`, "")
	for _, f := range []struct{ path, body string }{
		{filepath.Join(s.src, "outside.lua"), fmt.Sprintf(startFixture, "out")},
		{filepath.Join(s.src, "out", "main.lua"), fmt.Sprintf(snapLoader, "../outside.lua", outRes)},
		{filepath.Join(s.src, "in", "inside.lua"), fmt.Sprintf(startFixture, "in")},
		{filepath.Join(s.src, "in", "main.lua"), fmt.Sprintf(snapLoader, "inside.lua", inRes)},
	} {
		if err := os.WriteFile(f.path, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s.start()
	s.registered("in")
	if d := s.entry("in")["dir"]; !s.snapshot(d) {
		t.Errorf("in runs from %v, want a local snapshot", d)
	}
	result := func(path string) []string {
		var parts []string
		s.wait("the load result "+filepath.Base(path), func() bool {
			b, err := os.ReadFile(path)
			parts = strings.SplitN(string(b), "\n", 3)
			return err == nil && len(parts) == 3
		})
		return parts
	}
	if r := result(inRes); !s.snapshot(r[0]) || r[1] != "true" {
		t.Errorf("the load of inside.lua: %q, want ok from a snapshot", r)
	}
	r := result(outRes)
	t.Logf("the load of ../outside.lua: %q", r)
	if !s.snapshot(r[0]) || r[1] != "false" || !strings.Contains(r[2], r[0]+"/../outside.lua") || !strings.Contains(r[2], "No such file") {
		t.Errorf("the load of ../outside.lua: %q, want the missing-file error from a snapshot", r)
	}
	if s.status("out").Registered {
		t.Error("out registered: its main loaded ../outside.lua from the source tree")
	}
}
