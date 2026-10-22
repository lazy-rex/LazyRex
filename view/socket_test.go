package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
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

// The renderer never chooses among default sockets: an empty endpoint is refused even when a
// socket listens at each candidate path, a macOS path under HOME and the XDG path (codex-24's
// review of 598b8e5). The hub passes its endpoint with -server; a Linux path is accepted.
func TestRealSocketNeedsAnEndpoint(t *testing.T) {
	// A short directory: a Unix socket path has a length limit (104 bytes on macOS).
	root, err := os.MkdirTemp("/tmp", "rs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	home, xdg := filepath.Join(root, "h"), filepath.Join(root, "x")
	mac := filepath.Join(home, "Library", "Application Support", "rex", "server.sock")
	sock := filepath.Join(xdg, "rex", "server.sock")
	for _, p := range []string{mac, sock} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
	}
	if p, err := realSocketFor("", home, xdg, 501); err == nil {
		t.Fatalf("an empty endpoint chose %q", p)
	}
	if p, err := realSocketFor("unix://"+sock, home, xdg, 501); err != nil || p != sock {
		t.Fatalf("the XDG path: %q %v", p, err)
	}
	if p, err := realSocketFor("unix://"+strings.ReplaceAll(mac, " ", "%20"), home, xdg, 501); err != nil || p != mac {
		t.Fatalf("the macOS path: %q %v", p, err)
	}
	if _, err := realSocketFor("unix:///tmp/rexlab/rex.sock", home, xdg, 501); err == nil {
		t.Fatal("accepted a lab socket")
	}
}

// The same test as TestKnownDefaultSymlinks in hub/internal/rexreal: a symlink to a default socket
// is the default socket, in both directions.
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

func TestLabSocketRefusesTheRealServer(t *testing.T) {
	home, _ := os.UserHomeDir()
	if _, err := labSocket("unix://" + filepath.Join(home, "Library/Application%20Support/rex/server.sock")); err == nil {
		t.Fatal("labSocket accepted the real server")
	}
	if _, err := labSocket(""); err == nil {
		t.Fatal("labSocket accepted an empty REX_SERVER")
	}
}

// The renderer's endpoint: -server from the hub, else REX_SERVER (an older renderer that reloads
// into this binary has no -server). No endpoint, or two that differ, refuse. Equal spellings of
// one socket (escaped or plain, through a symbolic link) are one endpoint.
func TestServerEndpoint(t *testing.T) {
	// Under /tmp, so that labSocket accepts the lab endpoint.
	root, err := os.MkdirTemp("/tmp", "se-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.MkdirAll(filepath.Join(root, "lab"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "lab"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	lab := "unix://" + filepath.Join(root, "lab", "rex.sock")
	linked := "unix://" + filepath.Join(root, "link", "rex.sock")
	other := "unix://" + filepath.Join(root, "other", "rex.sock")
	mac := "unix:///Users/u/Library/Application Support/rex/server.sock"
	macEscaped := "unix:///Users/u/Library/Application%20Support/rex/server.sock"
	for _, c := range []struct {
		name, flag, env, want string
		ok                    bool
	}{
		{"no endpoint", "", "", "", false},
		{"-server only (a Linux pane)", lab, "", lab, true},
		{"REX_SERVER only (an older renderer reloads)", "", lab, lab, true},
		{"both, equal", lab, lab, lab, true},
		{"escaped and plain", mac, macEscaped, mac, true},
		{"through a symbolic link", linked, lab, linked, true},
		{"different servers", lab, other, "", false},
		{"malformed REX_SERVER", lab, "unix:///tmp/x%zz/rex.sock", "", false},
		{"relative -server", "unix://rex.sock", lab, "", false},
		{"not unix", "tcp://127.0.0.1:1", lab, "", false},
	} {
		got, err := serverEndpoint(c.flag, c.env)
		if got != c.want || (err == nil) != c.ok {
			t.Errorf("%s: %q, %v", c.name, got, err)
		}
	}
	// -server goes to the same checks as REX_SERVER did: a lab socket for labSocket only, the real
	// server for realSocket only.
	if _, err := labSocket(lab); err != nil {
		t.Errorf("labSocket refused the lab endpoint: %v", err)
	}
	if _, err := realSocket(lab); err == nil {
		t.Error("realSocket accepted a lab endpoint")
	}
}
