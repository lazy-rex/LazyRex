package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The release lab test. It runs lazyrex-hub release rehearse
// on a small fixture of "live" files in its own temp dir: a config with the home plugin only, its
// lock and base at OLD, a GitHub clone at OLD, and a frozen run. It never reads the real live files
// and never calls the real Rex server. Lab only: RELEASE_LAB=1. It needs the network (GitHub), go,
// python3 and rex, and it runs for minutes: never under a tool time limit.
const (
	labOld = "98d6fb49add69777ebe77fff63ccd2f9a1a7f63e"
	labH   = "790010c0c8517a01eec42a27530ad04ede2b33fd"
)

func TestReleaseRehearseLab(t *testing.T) {
	bin, f, run := labStart(t)
	lab := filepath.Join("/tmp", fmt.Sprintf("rexlab-release-t%d", os.Getpid()))
	if out, err := labRehearse(t, bin, f, run, lab, nil); err != nil {
		t.Fatalf("rehearse: %v (log %s; the lab %s is kept)", err, out, lab)
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

// A-l2 and R8: a live input that changes during the rehearsal fails it after the phases. There is
// no record, the lab stays as evidence, and no lab process runs. The test deletes the lab only
// when every check passes.
func TestReleaseRehearseLabFailure(t *testing.T) {
	bin, f, run := labStart(t)
	lab := filepath.Join("/tmp", fmt.Sprintf("rexlab-release-t%df", os.Getpid()))
	managed := filepath.Join(f, "state", "plugins.json")
	changed := false
	logp, err := labRehearse(t, bin, f, run, lab, func(line string) {
		if !changed && strings.Contains(line, "phase forward") {
			changed = true
			if err := os.WriteFile(managed, append(mustReadFile(managed), '\n'), 0o600); err != nil {
				t.Error(err)
			}
		}
	})
	if err == nil {
		t.Fatalf("the rehearsal passed with a changed live input (log %s)", logp)
	}
	log := string(mustReadFile(logp))
	for _, want := range []string{"a live input changed during the rehearsal", "live-managed", "FAILED; the lab is kept"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log %s has no %q", logp, want)
		}
	}
	if _, err := os.Stat(filepath.Join(run, relRehearsalFile)); err == nil {
		t.Error("a failed rehearsal wrote a record")
	}
	for _, p := range []string{"log", "run", "hub"} {
		if _, err := os.Stat(filepath.Join(lab, p)); err != nil {
			t.Errorf("the kept lab has no %s: %v", p, err)
		}
	}
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, lab+"/") || strings.HasSuffix(line, lab) {
			t.Errorf("a lab process still runs: %s", line)
		}
	}
	if !t.Failed() {
		if p := removeRehearsalLab(lab); len(p) > 0 {
			t.Error(p)
		}
	}
}

// labStart builds the tool and writes the fixture. It skips without RELEASE_LAB=1.
func labStart(t *testing.T) (bin, f, run string) {
	if os.Getenv("RELEASE_LAB") != "1" {
		t.Skip("set RELEASE_LAB=1 to run the release lab test (GitHub, a lab Rex server, minutes)")
	}
	bin = filepath.Join(t.TempDir(), "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	f = t.TempDir()
	labFixture(t, f, bin)
	return bin, f, filepath.Join(f, "run")
}

// labRehearse runs release rehearse on the fixture run. Its output goes to f/rehearse.log and to
// stderr; onLine (if set) sees each line while the rehearsal runs. It returns the log path.
func labRehearse(t *testing.T, bin, f, run, lab string, onLine func(string), extra ...string) (string, error) {
	logp := filepath.Join(f, "rehearse.log")
	logf, err := os.Create(logp)
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	pr, pw := io.Pipe()
	c := exec.Command(bin, append([]string{"release", "rehearse", "-run", run, "-lab", lab}, extra...)...)
	c.Stdout, c.Stderr = pw, pw
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			fmt.Fprintln(io.MultiWriter(logf, os.Stderr), sc.Text())
			if onLine != nil {
				onLine(sc.Text())
			}
		}
		io.Copy(io.Discard, pr)
	}()
	err = c.Run()
	pw.Close()
	<-done
	return logp, err
}

// labFixture writes the "live" fixture into f and freezes it into f/run.
func labFixture(t *testing.T, f, bin string) {
	t.Helper()
	// The plugin cache dirs are read-only: make them writable before the TempDir cleanup removes f.
	t.Cleanup(func() { removeRehearsalLab(f) })
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
	// The probe file (case 14 of the full tier): rows in a table of OLD, as a stand-in for a table that H adds.
	probes := filepath.Join(f, "probes.sql")
	os.WriteFile(probes, []byte(`INSERT INTO records (ns, key, value, rev) VALUES ('relprobe-file', 'k1', '{"v":1}', 1);
INSERT INTO records (ns, key, value, rev) VALUES ('relprobe-file', 'k2', '{"v":2}', 1);
`), 0o600)
	spec := filepath.Join(f, "spec.json")
	b, _ = json.Marshal(map[string]any{"kind": "hub", "h": labH, "config": cfg, "release_dir": filepath.Join(f, "release"),
		"server": "unix:///nonexistent/release-fixture/rex.sock", "dev_log": filepath.Join(f, "dev.log"), "probes": probes})
	os.WriteFile(spec, b, 0o600)
	if out, err := exec.Command(bin, "release", "freeze", "-spec", spec, "-run", filepath.Join(f, "run")).CombinedOutput(); err != nil {
		t.Fatalf("freeze: %v\n%s", err, out)
	}
}

// The full tier (c3e): -tier full runs every full phase after the default phases (spawn, pull-stop,
// to-from, from-to, build-fails, start-fails, resume, h-ended) on the fixture, and passes.
func TestReleaseRehearseLabFull(t *testing.T) {
	bin, f, run := labStart(t)
	lab := filepath.Join("/tmp", fmt.Sprintf("rexlab-release-t%dfull", os.Getpid()))
	if out, err := labRehearse(t, bin, f, run, lab, nil, "-tier", tierFull); err != nil {
		t.Fatalf("rehearse -tier full: %v (log %s; the lab %s is kept)", err, out, lab)
	}
	var rec relRehearsal
	if err := json.Unmarshal(mustReadFile(filepath.Join(run, relRehearsalFile)), &rec); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range rec.Phases {
		if p.Result != "pass" {
			t.Errorf("phase %s: %s", p.Name, p.Result)
		}
		names = append(names, p.Name)
	}
	want := "forward rollback spawn pull-stop to-from from-to build-fails start-fails resume h-ended"
	if got := strings.Join(names, " "); !rec.Pass || rec.Tier != tierFull || got != want {
		t.Errorf("record: pass %v tier %s phases %q", rec.Pass, rec.Tier, got)
	}
	if _, err := os.Stat(lab); err == nil {
		t.Errorf("the lab %s is still there after a pass", lab)
	}
}
