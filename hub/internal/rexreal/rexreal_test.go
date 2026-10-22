package rexreal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The known default sockets of both systems count on either host: a false answer means only "not
// a known default path", and the identity checks still follow. view/socket_test.go has the same
// table for its copy of knownDefault (view/wire.go): keep the two tables equal.
func TestKnownDefault(t *testing.T) {
	home, xdg := "/Users/u", "/run/user/501"
	mac := "/Users/u/Library/Application Support/rex/server.sock"
	for _, c := range []struct {
		name, endpoint string
		xdg            string
		want           bool
	}{
		{"macOS, escaped", "unix:///Users/u/Library/Application%20Support/rex/server.sock", xdg, true},
		{"macOS, plain", "unix://" + mac, xdg, true},
		{"macOS, with ..", "unix:///Users/u/Library/x/../Application%20Support/rex/server.sock", xdg, true},
		{"Linux, XDG", "unix:///run/user/501/rex/server.sock", xdg, true},
		{"Linux, /run/user/<uid> without XDG", "unix:///run/user/501/rex/server.sock", "", true},
		{"Linux, another XDG dir", "unix:///xdg/rex/server.sock", "/xdg", true},
		{"another uid", "unix:///run/user/502/rex/server.sock", xdg, true},
		{"another home", "unix:///Users/v/Library/Application%20Support/rex/server.sock", xdg, true},
		{"not a uid", "unix:///run/user/x/rex/server.sock", xdg, false},
		{"a lab", "unix:///tmp/rexlab-1/rex.sock", xdg, false},
		{"login-server socket", "unix:///run/user/501/rex/login-server.sock", xdg, false},
		{"relative XDG makes no candidate", "unix://rel/rex/server.sock", "rel", false},
		{"relative endpoint", "unix://rex/server.sock", "", false},
		{"not unix", "https://host.example/rex/server.sock", xdg, false},
		{"empty", "", xdg, false},
		{"malformed escape", "unix:///run/user/501/rex/server%zz.sock", xdg, false},
	} {
		if got := knownDefault(c.endpoint, home, c.xdg, 501); got != c.want {
			t.Errorf("%s: knownDefault(%q, xdg %q) = %v, want %v", c.name, c.endpoint, c.xdg, got, c.want)
		}
	}
}

// A symlink to a default socket is the default socket, in both directions: an endpoint that links
// to the default path, and a default directory that is itself a link.
func TestKnownDefaultSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "runtime")
	if err := os.MkdirAll(filepath.Join(real, "rex"), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(real, "rex", "server.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.sock")
	if err := os.Symlink(sock, alias); err != nil {
		t.Fatal(err)
	}
	linkedXDG := filepath.Join(root, "linked-runtime")
	if err := os.Symlink(real, linkedXDG); err != nil {
		t.Fatal(err)
	}
	if !knownDefault("unix://"+alias, "/nohome", real, 501) {
		t.Error("an endpoint that links to the default socket")
	}
	if !knownDefault("unix://"+sock, "/nohome", linkedXDG, 501) {
		t.Error("a default directory that is a link")
	}
	if knownDefault("unix://"+filepath.Join(root, "other.sock"), "/nohome", real, 501) {
		t.Error("another path in the same directory")
	}
}

// DefaultEndpoint is Rex's own default endpoint. An empty, malformed or failed answer is no
// endpoint: the caller refuses.
func TestDefaultEndpoint(t *testing.T) {
	for _, c := range []struct {
		def, want string
		ok        bool
	}{
		{`{"endpoint":"unix:///run/user/501/rex/server.sock","instance_id":"i1"}`, "unix:///run/user/501/rex/server.sock", true},
		{`{"endpoint":"","instance_id":"i1"}`, "", false},
		{`{"endpoint":"unix:///run/user/501/rex/server.sock"}`, "", false},
		{`not json`, "", false},
		{"down", "", false},
	} {
		fakeStatusRex(t, c.def, "")
		got, err := DefaultEndpoint(context.Background())
		if got != c.want || (err == nil) != c.ok {
			t.Errorf("default %q: %q, %v", c.def, got, err)
		}
	}
}

// fakeStatusRex puts a rex on PATH that answers "server status --json" as cmd/hub's test of the
// same name does: the default server answers DEF ("down" = not running); any -S server answers
// CFG ("" = refused); with FAKE_CFG_HOLD set, it creates that file and does not answer for 5 s. It
// records whether it saw REX_SERVER or --autostart=false.
func fakeStatusRex(t *testing.T, def, cfg string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "REX_SERVER=$REX_SERVER $*" >>"` + log + `"
s=""
while [ $# -gt 0 ]; do
  case "$1" in -S) s="$2"; shift ;; esac
  shift
done
[ -z "$s" ] && s="$REX_SERVER"
if [ -n "$s" ]; then
  [ -n "$FAKE_CFG_HOLD" ] && { : >"$FAKE_CFG_HOLD"; exec sleep 5; }
  [ -z "$FAKE_CFG" ] && { echo "Error: connecting to Rex server $s: refused" >&2; exit 1; }
  echo "$FAKE_CFG"; exit 0
fi
case "$FAKE_DEF" in
  down) echo "Error: rex server is not running" >&2; exit 1 ;;
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
	return log
}

// The default query removes REX_SERVER and never starts a server.
func TestDefaultQueryHasNoServer(t *testing.T) {
	log := fakeStatusRex(t, `{"endpoint":"unix:///run/user/501/rex/server.sock","instance_id":"i1"}`, "")
	if _, err := DefaultEndpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if s := string(b); !strings.HasPrefix(s, "REX_SERVER= ") || !strings.Contains(s, "--autostart=false") || strings.Contains(s, "-S") {
		t.Errorf("the default query: %q", s)
	}
}

// A cancel while the configured server's query runs (the default server answered) is doubt, so
// real: a stopped call is not an absent server. The call ends at once.
func TestIsRealCanceledBetweenQueries(t *testing.T) {
	fakeStatusRex(t, `{"endpoint":"unix:///run/user/501/rex/server.sock","instance_id":"i1"}`, "")
	hold := filepath.Join(t.TempDir(), "held")
	t.Setenv("FAKE_CFG_HOLD", hold)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(hold); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	start := time.Now()
	if !IsReal(ctx, "unix:///tmp/rexlab-x/rex.sock") {
		t.Error("a canceled query of the configured server proved \"not real\"")
	}
	if _, err := os.Stat(hold); err != nil {
		t.Errorf("the configured server was not asked: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("IsReal returned after %v, not at the cancel", d)
	}
}

// A context canceled before the call is doubt too: real.
func TestIsRealCanceledBefore(t *testing.T) {
	fakeStatusRex(t, `{"endpoint":"unix:///run/user/501/rex/server.sock","instance_id":"i1"}`, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !IsReal(ctx, "unix:///tmp/rexlab-x/rex.sock") {
		t.Error("a canceled context proved \"not real\"")
	}
}
