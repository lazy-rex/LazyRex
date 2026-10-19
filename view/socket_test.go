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

func TestLabSocketRefusesTheRealServer(t *testing.T) {
	home, _ := os.UserHomeDir()
	if _, err := labSocket("unix://" + filepath.Join(home, "Library/Application%20Support/rex/server.sock")); err == nil {
		t.Fatal("labSocket accepted the real server")
	}
}
