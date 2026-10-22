package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The release lab test (c3 design v2 C; plan v2 A-l1, C-l1). It runs lazyrex-hub release rehearse
// on a small fixture of "live" files in its own temp dir: a config with the home plugin only, its
// lock and base at OLD, a GitHub clone at OLD, and a frozen run. It never reads the real live files
// and never calls the real Rex server. Lab only: RELEASE_LAB=1. It needs the network (GitHub), go,
// python3 and rex, and it runs for minutes: never under a tool time limit.
const (
	labOld = "98d6fb49add69777ebe77fff63ccd2f9a1a7f63e"
	labH   = "790010c0c8517a01eec42a27530ad04ede2b33fd"
)

func TestReleaseRehearseLab(t *testing.T) {
	if os.Getenv("RELEASE_LAB") != "1" {
		t.Skip("set RELEASE_LAB=1 to run the release lab test (GitHub, a lab Rex server, minutes)")
	}
	bin := filepath.Join(t.TempDir(), "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	f := t.TempDir()
	run := filepath.Join(f, "run")
	labFixture(t, f, bin)

	lab := filepath.Join("/tmp", fmt.Sprintf("rexlab-release-t%d", os.Getpid()))
	logp := filepath.Join(f, "rehearse.log")
	logf, err := os.Create(logp)
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin, "release", "rehearse", "-run", run, "-lab", lab)
	c.Stdout, c.Stderr = io.MultiWriter(logf, os.Stderr), io.MultiWriter(logf, os.Stderr)
	err = c.Run()
	logf.Close()
	if err != nil {
		t.Fatalf("rehearse: %v (log %s; the lab %s is kept)", err, logp, lab)
	}
	if _, err := os.Stat(lab); err == nil {
		t.Errorf("the lab %s is still there after a pass", lab)
	}
	var rec relRehearsal
	if err := json.Unmarshal(mustReadFile(filepath.Join(run, relRehearsalFile)), &rec); err != nil {
		t.Fatal(err)
	}
	if !rec.Pass || rec.Tier != tierDefault {
		t.Errorf("record: pass %v tier %s %v", rec.Pass, rec.Tier, rec.TierReasons)
	}
	// C-l1: the record passes the precheck gate of this run and this tool.
	r, err := openRun(run, "precheck", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.unlock()
	if err := r.checkRehearsal(bin); err != nil {
		t.Errorf("the record does not pass the gate: %v", err)
	}
}

// labFixture writes the "live" fixture into f and freezes it into f/run.
func labFixture(t *testing.T, f, bin string) {
	t.Helper()
	// The plugin cache dirs are read-only: make them writable before the TempDir cleanup removes f.
	t.Cleanup(func() { removeLab(f) })
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	clone := filepath.Join(f, "release", "LazyRex")
	os.MkdirAll(filepath.Dir(clone), 0o700)
	if out, err := exec.Command("git", "clone", "-q", relLiveRepo, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	git(clone, "checkout", "-q", "--detach", labOld)

	// The digest of the home plugin at OLD: a sync of the commit form in a scratch config.
	home := "github:lazy-rex/LazyRex/plugins/lazyrex"
	scratch := filepath.Join(f, "digest")
	os.MkdirAll(scratch, 0o700)
	sc := filepath.Join(scratch, "hub.json")
	b, _ := json.Marshal(map[string]any{"state": filepath.Join(scratch, "state"), "plugins": []any{map[string]string{"source": home + "@" + labOld}}})
	os.WriteFile(sc, b, 0o600)
	if err := runPluginCmd(context.Background(), "sync", sc, nil, pluginCmdOpts{}, io.Discard); err != nil {
		t.Fatalf("the digest sync: %v", err)
	}
	sl, err := readLock(lockPath(sc, ""))
	if err != nil {
		t.Fatal(err)
	}
	e := sl.Plugins[home+"@"+labOld]
	if e == nil || e.Commit != labOld {
		t.Fatalf("the digest sync: %+v", sl.Plugins)
	}

	cfg := filepath.Join(f, "hub-live.json")
	state := filepath.Join(f, "state")
	os.MkdirAll(state, 0o700)
	b, _ = json.Marshal(map[string]any{"server": "unix:///nonexistent/release-fixture/rex.sock", "state": state, "view": filepath.Join(f, "view"),
		"agents": []any{map[string]string{"id": "user", "name": "User"}}, "plugins": []any{map[string]string{"source": home + "@main"}}})
	os.WriteFile(cfg, b, 0o600)
	os.WriteFile(filepath.Join(state, "plugins.json"), []byte(`{"plugins":[]}`), 0o600)
	src := home + "@main"
	entry := *e
	entry.Ref, entry.RefKind = "main", "branch"
	lk := lockPath(cfg, "")
	if err := writeLock(lk, &pluginLock{Version: 1, Plugins: map[string]*lockEntry{src: &entry}}); err != nil {
		t.Fatal(err)
	}
	if err := writePeers(lk, &peerBase{Version: 1, Peers: map[string]map[string]basePin{pinHub: {src: {Commit: e.Commit, Digest: e.Digest}}}}); err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(f, "spec.json")
	b, _ = json.Marshal(map[string]any{"kind": "hub", "h": labH, "config": cfg, "release_dir": filepath.Join(f, "release"),
		"server": "unix:///nonexistent/release-fixture/rex.sock", "dev_log": filepath.Join(f, "dev.log")})
	os.WriteFile(spec, b, 0o600)
	if out, err := exec.Command(bin, "release", "freeze", "-spec", spec, "-run", filepath.Join(f, "run")).CombinedOutput(); err != nil {
		t.Fatalf("freeze: %v\n%s", err, out)
	}
}
