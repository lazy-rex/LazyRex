package main

import (
	"strings"
	"testing"
)

// restartCheck: the recorded exec path is the proof, the live executable must be the recorded one,
// and a row without a record needs a complete live answer.
func TestRestartCheck(t *testing.T) {
	const prog = "/opt/bin/claude"
	rec := paneRecord{pid: 7, start: "100.000001", path: "/opt/versions/2", invoked: prog}
	live := paneState{child: 7, path: "/opt/versions/2", invoked: prog, foreground: 7}
	for _, c := range []struct {
		what  string
		p     paneState
		rec   paneRecord
		start string
		want  string
	}{
		{"the recorded program", live, rec, rec.start, ""},
		{"a moved symlink: Rex has no exec path now", paneState{child: 7, path: "/opt/versions/2"}, rec, rec.start, ""},
		{"no child", paneState{}, rec, rec.start, "no program"},
		{"another child", paneState{child: 8, path: "/opt/versions/2"}, rec, rec.start, "not the agent's pid 7"},
		{"a reused pid", live, rec, "200.000001", "another process now"},
		{"no live executable", paneState{child: 7, invoked: prog}, rec, rec.start, "no executable"},
		{"started as another program", live, paneRecord{7, rec.start, "/bin/bash", "/bin/bash"}, rec.start, "started as /bin/bash"},
		{"an exec to another path in the same pid", paneState{child: 7, path: "/bin/dash"}, rec, rec.start, "executable changed"},
		{"an incomplete record", live, paneRecord{7, rec.start, "", prog}, rec.start, "incomplete"},
		{"no record, a complete live answer", live, paneRecord{pid: 7, start: rec.start}, rec.start, ""},
		{"no record, a null exec path", paneState{child: 7, path: "/opt/versions/2"}, paneRecord{pid: 7, start: rec.start}, rec.start, "cannot be verified"},
		// argv0 is not an input: only Rex's exec path counts.
		{"no record, another exec path", paneState{child: 7, path: "/bin/sh", invoked: "/bin/sh"}, paneRecord{pid: 7, start: rec.start}, rec.start, "not the configured program"},
	} {
		got := restartCheck(c.p, c.rec, c.start, prog)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.what, got, c.want)
		}
	}
}
