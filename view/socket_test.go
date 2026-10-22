package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRealSocketAcceptsOnlyTheRealServer(t *testing.T) {
	home, _ := os.UserHomeDir()
	real := "unix://" + filepath.Join(home, "Library/Application%20Support/rex/server.sock")
	if p, err := realSocket(real); err != nil || p != filepath.Join(home, "Library/Application Support/rex/server.sock") {
		t.Fatalf("real socket refused: %q %v", p, err)
	}
	for _, s := range []string{"unix:///tmp/rexlab/rex.sock", "tcp://127.0.0.1:1", "unix://" + filepath.Join(home, "other/server.sock"), filepath.Join(home, "Library/Application Support/rex/server.sock")} {
		if _, err := realSocket(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}

// The same cases as TestKnownDefault in hub/internal/rexreal: keep the two tables equal.
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

// A Linux pane has no REX_SERVER: with -real-server, the renderer takes this user's default socket
// that exists. Without one, it refuses. A Linux path in REX_SERVER is accepted.
func TestRealSocketOnLinux(t *testing.T) {
	xdg := t.TempDir()
	sock := filepath.Join(xdg, "rex", "server.sock")
	if p, err := realSocketFor("", "/nohome", xdg, 501); err == nil {
		t.Fatalf("no socket exists, but got %q", p)
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil { // a file stands in for the socket
		t.Fatal(err)
	}
	if p, err := realSocketFor("", "/nohome", xdg, 501); err != nil || p != sock {
		t.Fatalf("empty REX_SERVER: %q %v", p, err)
	}
	if p, err := realSocketFor("unix://"+sock, "/nohome", xdg, 501); err != nil || p != sock {
		t.Fatalf("the XDG path: %q %v", p, err)
	}
	if _, err := realSocketFor("unix:///tmp/rexlab/rex.sock", "/nohome", xdg, 501); err == nil {
		t.Fatal("accepted a lab socket")
	}
}

func TestLabSocketRefusesTheRealServer(t *testing.T) {
	home, _ := os.UserHomeDir()
	if _, err := labSocket("unix://" + filepath.Join(home, "Library/Application%20Support/rex/server.sock")); err == nil {
		t.Fatal("labSocket accepted the real server")
	}
	if _, err := labSocket(""); err == nil {
		t.Fatal("labSocket accepted an empty REX_SERVER")
	}
}
