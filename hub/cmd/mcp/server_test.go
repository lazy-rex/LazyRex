package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeRex puts a rex on PATH for "server status --json": the default server (no -S, no
// REX_SERVER) answers def ("down" = not running); a -S server answers cfg ("" = refused).
func fakeRex(t *testing.T, def, cfg string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
s=""
while [ $# -gt 0 ]; do
  case "$1" in -S) s="$2"; shift ;; esac
  shift
done
[ -z "$s" ] && s="$REX_SERVER"
if [ -n "$s" ]; then
  [ -z "$FAKE_CFG" ] && { echo "Error: connecting to Rex server $s: refused" >&2; exit 1; }
  echo "$FAKE_CFG"; exit 0
fi
[ "$FAKE_DEF" = down ] && { echo "Error: rex server is not running" >&2; exit 1; }
echo "$FAKE_DEF"
`
	if err := os.WriteFile(filepath.Join(dir, "rex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DEF", def)
	t.Setenv("FAKE_CFG", cfg)
}

// The shim's server: -server, else REX_SERVER, else Rex's default endpoint (a Linux pane has no
// REX_SERVER). The real server needs -real-server, on either system, by path or by identity.
func TestResolveServer(t *testing.T) {
	const linux = "unix:///run/user/1000/rex/server.sock"
	const lab = "unix:///tmp/rexlab-x/rex.sock"
	def := `{"endpoint":"` + linux + `","instance_id":"i-real"}`
	for _, c := range []struct {
		name, flag, env, def, cfg string
		real                      bool
		want                      string // "" = refused
	}{
		{"-server wins over REX_SERVER", lab, "unix:///tmp/other/rex.sock", def, `{"instance_id":"i-lab"}`, false, lab},
		{"REX_SERVER", "", lab, def, `{"instance_id":"i-lab"}`, false, lab},
		{"no server: the default needs -real-server", "", "", def, "", false, ""},
		{"no server, -real-server: the default", "", "", def, "", true, linux},
		{"no server, the default is not resolved", "", "", "down", "", true, ""},
		{"no server, an empty default answer", "", "", `{}`, "", true, ""},
		{"the Linux real path without -real-server", linux, "", "down", "", false, ""},
		{"the macOS real path without -real-server", "unix:///Users/u/Library/Application%20Support/rex/server.sock", "", "down", "", false, ""},
		{"another path to the real server", "unix:///tmp/link/server.sock", "", def, `{"instance_id":"i-real"}`, false, ""},
		{"a lab with -real-server", lab, "", def, `{"instance_id":"i-lab"}`, true, ""},
		{"an uncertain default", lab, "", `{"endpoint":"` + linux + `"}`, `{"instance_id":"i-lab"}`, false, ""},
	} {
		fakeRex(t, c.def, c.cfg)
		got, err := resolveServer(c.flag, c.env, c.real)
		if got != c.want || (err == nil) != (c.want != "") {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}
