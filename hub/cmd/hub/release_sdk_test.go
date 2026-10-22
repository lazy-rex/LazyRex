package main

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// procargs2 builds a kern.procargs2 buffer: argc, the executable path, padding, argv, env.
func procargs2(exe string, argv, env []string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(argv)))
	b = append(b, exe...)
	b = append(b, 0, 0, 0, 0)
	for _, a := range append(slices.Clone(argv), env...) {
		b = append(append(b, a...), 0)
	}
	return b
}

func TestParseProcArgs2(t *testing.T) {
	argv := []string{"/dev hub/hub", "-config", "/c.json", "-sdk", "/a b/sdk/lua"}
	got, err := parseProcArgs2(procargs2("/dev hub/hub", argv, []string{"A=1"}))
	if err != nil || !slices.Equal(got, argv) {
		t.Fatalf("got %q, %v; want %q", got, err, argv)
	}
	full := procargs2("/x", []string{"/x", "-sdk", "/s"}, nil)
	for name, b := range map[string][]byte{
		"empty":   nil,
		"argc 0":  procargs2("/x", nil, nil),
		"cut arg": full[:len(full)-1],
	} {
		if got, err := parseProcArgs2(b); err == nil {
			t.Errorf("%s: got %q, want an error", name, got)
		}
	}
}

// TestProcArgvExactChild reads a real child whose argument holds a space: ps words would split it.
func TestProcArgvExactChild(t *testing.T) {
	want := []string{"/bin/sh", "-c", "sleep 30", "x", "a b"}
	cmd := exec.Command(want[0], want[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var got []string
	var err error
	for i := 0; i < 100; i++ {
		if got, err = procArgvExact(cmd.Process.Pid); err == nil && slices.Equal(got, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("procArgvExact = %q, %v; want %q", got, err, want)
}

func TestSDKArgExact(t *testing.T) {
	for name, tc := range map[string]struct {
		argv []string
		want string // a path, or "!<text of the error>"
	}{
		"flag":            {[]string{"hub", "-config", "c", "-sdk", "/a b/sdk/lua"}, "/a b/sdk/lua"},
		"equals":          {[]string{"hub", "--sdk=/s/lua"}, "/s/lua"},
		"argv0 only":      {[]string{"-sdk=/s/lua"}, "!no -sdk"},
		"empty argv":      {nil, "!empty"},
		"after --":        {[]string{"hub", "-config", "/lab/c", "--", "-sdk", "/lab/H/sdk/lua"}, "!arguments after"},
		"positional":      {[]string{"hub", "-config", "/lab/c", "x", "-sdk", "/lab/H/sdk/lua"}, "!arguments after"},
		"subcommand":      {[]string{"hub", "release", "-sdk", "/lab/H/sdk/lua"}, "!arguments after"},
		"config eats":     {[]string{"hub", "-config", "-sdk", "/lab/H/sdk/lua"}, "!arguments after"},
		"config eats all": {[]string{"hub", "-config", "-sdk"}, "!no -sdk"},
		"bool flag":       {[]string{"hub", "-real-server", "-test=false", "-sdk", "/s/lua"}, "/s/lua"},
		"bool no value":   {[]string{"hub", "-restart", "/x", "-sdk", "/s/lua"}, "!arguments after"},
		"lab fault":       {[]string{"hub", "-lab-fault", "point=store-commit,mode=exit", "-sdk", "/s/lua"}, "/s/lua"},
		"sdk eats":        {[]string{"hub", "-sdk", "-config", "/c"}, "!arguments after"},
		"missing":         {[]string{"hub", "-config", "c"}, "!no -sdk"},
		"repeated":        {[]string{"hub", "-sdk", "/a", "-sdk=/b"}, "!2 -sdk"},
		"no value":        {[]string{"hub", "-sdk"}, "!needs an argument"},
		"empty":           {[]string{"hub", "-sdk", ""}, "!empty"},
		"relative":        {[]string{"hub", "-sdk", "sdk/lua"}, "!not absolute"},
		"not clean":       {[]string{"hub", "-sdk", "/a/../b"}, "!not absolute"},
		"value -sdk":      {[]string{"hub", "-sdk", "-sdk"}, "!not absolute"},
		"unknown flag":    {[]string{"hub", "-sdkx", "/a", "-sdk", "/b"}, "!does not parse"},
	} {
		got, err := sdkArgExact(tc.argv)
		if w, bad := strings.CutPrefix(tc.want, "!"); bad {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("%s: got %q, %v; want an error with %q", name, got, err, w)
			}
		} else if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, tc.want)
		}
	}
}

// writeTree writes files (path: content) under dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		f := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSDKTreeDigest: the digest of <commit>:sdk/lua equals dirDigest of the same files, and
// differs between commits with different SDK bytes.
func TestSDKTreeDigest(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	a := map[string]string{"sdk/lua/lazyrex.lua": "return 1\n", "sdk/lua/tests/t.lua": "x\n", "README": "r\n"}
	writeTree(t, repo, a)
	git("add", "-A")
	git("commit", "-q", "-m", "a")
	ca := git("rev-parse", "HEAD")
	writeTree(t, repo, map[string]string{"sdk/lua/lazyrex.lua": "return 2\n"})
	git("commit", "-q", "-am", "b")
	cb := git("rev-parse", "HEAD")

	ref := t.TempDir()
	writeTree(t, ref, map[string]string{"lazyrex.lua": "return 1\n", "tests/t.lua": "x\n"})
	want, err := dirDigest(ref)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	da, err := sdkTreeDigest(context.Background(), repo, ca, tmp)
	if err != nil || da != want {
		t.Fatalf("digest of %s = %q, %v; want %q", ca, da, err, want)
	}
	if db, err := sdkTreeDigest(context.Background(), repo, cb, tmp); err != nil || db == da {
		t.Fatalf("digest of %s = %q, %v; want a different digest from %q", cb, db, err, da)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("the temporary directory keeps %d entries", len(left))
	}
	if _, err := sdkTreeDigest(context.Background(), repo, strings.Repeat("0", 40), tmp); err == nil {
		t.Fatal("an unknown commit: want an error")
	}
}

// sdkWorld is a fake world with the SDK bytes of OLD and H in two directories under the clone
// directory. The expected digest of each commit is the digest of its directory.
func sdkWorld(t *testing.T) (*relWorld, *releaser, map[string]string) {
	w := newRelWorld(t)
	w.freeze()
	r := w.open("rehearse")
	t.Cleanup(r.unlock)
	dirs := map[string]string{commitOld: filepath.Join(w.rel, "old sdk", "lua"), commitH: filepath.Join(w.rel, "LazyRex", "sdk", "lua")}
	writeTree(t, dirs[commitOld], map[string]string{"lazyrex.lua": "return 'old'\n"})
	writeTree(t, dirs[commitH], map[string]string{"lazyrex.lua": "return 'h'\n", "boot.lua": "b\n"})
	r.sdkTreeOf = func(c string) (string, error) {
		if d, ok := dirs[c]; ok {
			return dirDigest(d)
		}
		return "", errors.New("unknown commit " + c)
	}
	r.argvExactOf = func(pid int) ([]string, error) {
		if pid != w.hubPID {
			return nil, errors.New("no such process")
		}
		return []string{filepath.Join(w.state, "dev", "hub"), "-config", w.cfg, "-sdk", w.sdkDir}, nil
	}
	return w, r, dirs
}

func TestReleaseSDKBinding(t *testing.T) {
	// The fake world of the plan: the clone is at H, but the hub argv names OLD's SDK bytes.
	t.Run("OLD bytes under H", func(t *testing.T) {
		w, r, dirs := sdkWorld(t)
		w.sdkDir = dirs[commitOld]
		want, _ := r.currentHub()
		rec, err := r.checkSDK("forward", want, commitH, w.rel)
		if err == nil || !strings.Contains(err.Error(), "not the digest") {
			t.Fatalf("got %v; want a digest mismatch", err)
		}
		if rec == nil || rec.Actual == rec.Expected || rec.Path != dirs[commitOld] {
			t.Fatalf("the record does not show the mismatch: %+v", rec)
		}
	})
	t.Run("H bytes", func(t *testing.T) {
		w, r, dirs := sdkWorld(t)
		w.sdkDir = dirs[commitH]
		want, _ := r.currentHub()
		rec, err := r.checkSDK("forward", want, commitH, w.rel)
		if err != nil {
			t.Fatal(err)
		}
		exp, _ := dirDigest(dirs[commitH])
		if rec.Expected != exp || rec.Actual != exp || rec.PID != w.hubPID || rec.Seq != want.Seq || rec.Commit != commitH || rec.Path != dirs[commitH] || rec.Phase != "forward" {
			t.Fatalf("record %+v", rec)
		}
	})
	for name, tc := range map[string]struct {
		mod  func(w *relWorld, r *releaser, dirs map[string]string, want *relHub)
		want string
	}{
		"seq differs":         {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { h.Seq++ }, "before the argv read"},
		"start differs":       {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { h.Start = "t0" }, "before the argv read"},
		"client differs":      {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { h.Client = "client:x" }, "before the argv read"},
		"no hub":              {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { w.stopHub() }, "before the argv read"},
		"restart during argv": {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { wrapArgv(w, r) }, "after the argv read"},
		"restart during hash": {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { wrapTree(w, r) }, "after the hashing"},
		"argv read fails": {func(w *relWorld, r *releaser, d map[string]string, h *relHub) {
			r.argvExactOf = func(int) ([]string, error) { return nil, errors.New("denied") }
		}, "denied"},
		"empty argv": {func(w *relWorld, r *releaser, d map[string]string, h *relHub) {
			r.argvExactOf = func(int) ([]string, error) { return nil, nil }
		}, "argv is empty"},
		"no -sdk":         {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { w.sdkDir = "" }, "empty -sdk"},
		"relative":        {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { w.sdkDir = "sdk/lua" }, "not absolute"},
		"outside root":    {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { w.sdkDir = mkSDK(w.t, w.t.TempDir()) }, "not inside"},
		"symlink outside": {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { w.sdkDir = linkOut(w) }, "not inside"},
		"the root itself": {func(w *relWorld, r *releaser, d map[string]string, h *relHub) { w.sdkDir = w.rel }, "not inside"},
		"unreadable": {func(w *relWorld, r *releaser, d map[string]string, h *relHub) {
			w.sdkDir = filepath.Join(w.rel, "gone")
		}, "-sdk path"},
		"symlink inside the SDK": {func(w *relWorld, r *releaser, d map[string]string, h *relHub) {
			os.Symlink("lazyrex.lua", filepath.Join(d[commitH], "link.lua"))
		}, "not a regular file"},
		"expected digest fails": {func(w *relWorld, r *releaser, d map[string]string, h *relHub) {
			r.sdkTreeOf = func(string) (string, error) { return "", errors.New("no tree") }
		}, "no tree"},
	} {
		t.Run(name, func(t *testing.T) {
			w, r, dirs := sdkWorld(t)
			w.sdkDir = dirs[commitH]
			want, err := r.currentHub()
			if err != nil {
				t.Fatal(err)
			}
			tc.mod(w, r, dirs, want)
			if _, err := r.checkSDK("forward", want, commitH, w.rel); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want an error with %q", err, tc.want)
			}
		})
	}
}

// wrapArgv restarts the hub during the argv read; wrapTree restarts it during the hashing.
func wrapArgv(w *relWorld, r *releaser) {
	read := r.argvExactOf
	r.argvExactOf = func(pid int) ([]string, error) { argv, err := read(pid); w.startHub(); return argv, err }
}

func wrapTree(w *relWorld, r *releaser) {
	tree := r.sdkTreeOf
	r.sdkTreeOf = func(c string) (string, error) { d, err := tree(c); w.startHub(); return d, err }
}

// mkSDK writes the H SDK bytes into dir/sdk/lua and returns that path.
func mkSDK(t *testing.T, dir string) string {
	p := filepath.Join(dir, "sdk", "lua")
	writeTree(t, p, map[string]string{"lazyrex.lua": "return 'h'\n", "boot.lua": "b\n"})
	return p
}

// linkOut is a symlink under the root to SDK bytes outside it.
func linkOut(w *relWorld) string {
	p := filepath.Join(w.rel, "link")
	os.Symlink(filepath.Dir(mkSDK(w.t, w.t.TempDir())), p)
	return filepath.Join(p, "lua")
}
