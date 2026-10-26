package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestReleaseArchiveMessagingLab starts Messaging with only a release archive: the archive's
// lazyrex-hub and its sdk/lua, never the checkout's SDK. Messaging calls app.peer at its load, and
// app.peer loads sdk/lua/peer.lua, so an archive without that file never registers Messaging.
// MESSAGING_DIR names a Messaging plugin checkout, as in TestPeerMessagingLab.
func TestReleaseArchiveMessagingLab(t *testing.T) {
	needLab(t)
	dir := os.Getenv("MESSAGING_DIR")
	if dir == "" {
		t.Skip("set MESSAGING_DIR to a Messaging plugin checkout")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	build := exec.Command(filepath.Join(root, "scripts", "release-build.sh"), "dev", out)
	build.Env = append(os.Environ(), "TARGETS="+runtime.GOOS+"/"+runtime.GOARCH)
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("release-build.sh: %v\n%s", err, b)
	}
	x, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(out, "lazyrex-dev-"+runtime.GOOS+"-"+runtime.GOARCH+".tar.gz")
	if b, err := exec.Command("tar", "-xzf", archive, "-C", x).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, b)
	}

	s := newServerLab(t, nil, "messaging")
	if err := os.MkdirAll(s.src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(s.src, "messaging"), os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	sdk := filepath.Join(x, "sdk", "lua")
	s.bin = filepath.Join(x, "bin", "lazyrex-hub")
	s.args = append(s.args, "-sdk", sdk)
	s.start()
	s.registered("messaging")
	// The plugin ran the archive's boot.lua, not a checkout's.
	if st := s.status("messaging"); !argvHas(st.ChildPID, filepath.Join(sdk, "boot.lua")) {
		t.Fatalf("the messaging process %d does not run %s", st.ChildPID, filepath.Join(sdk, "boot.lua"))
	}
}
