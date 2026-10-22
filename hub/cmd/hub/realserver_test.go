package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeStatusRex puts a rex on PATH that answers "server status --json". The default server (no
// -S) answers DEF, a status JSON or "down" (not running) or "fail"; with REX_SERVER set it answers
// as that server, as the real CLI does. Any -S server answers CFG ("" = no answer).
func fakeStatusRex(t *testing.T, def, cfg string) {
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
case "$FAKE_DEF" in
  down) echo "Error: rex server is not running" >&2; exit 1 ;;
  fail) echo "Error: timeout" >&2; exit 1 ;;
esac
echo "$FAKE_DEF"
`
	if err := os.WriteFile(filepath.Join(dir, "rex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DEF", def)
	t.Setenv("FAKE_CFG", cfg)
	t.Setenv("REX_SERVER", "unix:///tmp/rexlab-x/rex.sock")
}

// isRealServer asks Rex for its default server, not only for the macOS path: on Linux the real
// server is at another path. Each uncertain answer is "real".
func TestIsRealServer(t *testing.T) {
	const linux = "unix:///run/user/1000/rex/server.sock"
	def := `{"endpoint":"` + linux + `","instance_id":"i-real"}`
	cases := []struct {
		name, server, def, cfg string
		want                   bool
	}{
		{"the macOS path", "unix:///Users/u/Library/Application%20Support/rex/server.sock", "fail", "", true},
		{"the default endpoint", linux, def, `{"instance_id":"i-lab"}`, true},
		{"the default endpoint, another spelling", "unix:///run/user/1000/./rex//server.sock", def, "", true},
		{"another path to the same server", "unix:///tmp/link/server.sock", def, `{"instance_id":"i-real"}`, true},
		{"a lab server", "unix:///tmp/rexlab-x/rex.sock", def, `{"instance_id":"i-lab"}`, false},
		{"a lab server that does not answer", "unix:///tmp/rexlab-x/rex.sock", def, "", false},
		{"no default server, a lab that answers", "unix:///tmp/rexlab-x/rex.sock", "down", `{"instance_id":"i-lab"}`, false},
		{"no default server, a lab that does not answer", "unix:///tmp/rexlab-x/rex.sock", "down", "", true},
		{"the default query fails", "unix:///tmp/rexlab-x/rex.sock", "fail", `{"instance_id":"i-lab"}`, true},
		{"a default answer without an instance", "unix:///tmp/rexlab-x/rex.sock", `{"endpoint":"` + linux + `"}`, `{"instance_id":""}`, true},
		// Codex review of a87d0ee: an empty default answer proves nothing, also when the config
		// server does not answer; a config answer that is not JSON is not an absent server.
		{"an empty default answer, a server that does not answer", linux, `{}`, "", true},
		{"a default without an instance, a server that does not answer", "unix:///tmp/rexlab-x/rex.sock", `{"endpoint":"` + linux + `"}`, "", true},
		{"a config answer that is not JSON", "unix:///tmp/rexlab-x/rex.sock", def, `not json`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeStatusRex(t, c.def, c.cfg)
			if got := isRealServer(c.server); got != c.want {
				t.Fatalf("isRealServer(%s) = %v, want %v", c.server, got, c.want)
			}
		})
	}
}

// In test mode the hub asks no Rex server (checkLabProof follows); outside test mode it asks, and
// -real-server must match the answer.
func TestCheckServer(t *testing.T) {
	fakeStatusRex(t, "fail", "")
	if err := checkServer("unix:///tmp/rexlab-x/rex.sock", false, true); err != nil {
		t.Fatalf("test mode asked Rex: %v", err)
	}
	if err := checkServer("unix:///Users/u/Library/Application%20Support/rex/server.sock", false, true); err == nil {
		t.Fatal("test mode accepted the macOS real server")
	}
	if err := checkServer("unix:///tmp/rexlab-x/rex.sock", false, false); err == nil {
		t.Fatal("a failed default query is not a proof")
	}
	fakeStatusRex(t, `{"endpoint":"unix:///run/user/1000/rex/server.sock","instance_id":"i-real"}`, `{"instance_id":"i-lab"}`)
	if err := checkServer("unix:///tmp/rexlab-x/rex.sock", false, false); err != nil {
		t.Fatalf("a lab server: %v", err)
	}
	if err := checkServer("unix:///tmp/rexlab-x/rex.sock", true, false); err == nil {
		t.Fatal("-real-server accepted a lab server")
	}
	if err := checkServer("unix:///run/user/1000/rex/server.sock", false, false); err == nil {
		t.Fatal("refused nothing on the Linux real server")
	}
	if err := checkServer("unix:///run/user/1000/rex/server.sock", true, false); err != nil {
		t.Fatalf("-real-server on the Linux real server: %v", err)
	}
}
