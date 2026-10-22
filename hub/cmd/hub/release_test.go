package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	relSrc     = "github:o/r@main"
	relOther   = "github:o/other@main"
	relCommitB = "1111111111111111111111111111111111111111"
	relDigestB = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	relCommitC = "2222222222222222222222222222222222222222"
	relDigestC = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// relFixture is a release environment in a temporary directory: a config with two GitHub sources,
// their lock and base file, a state directory, and a run directory with a spec.
type relFixture struct {
	dir, cfg, run string
	r             *releaser
	out           *bytes.Buffer
}

func relEntry(src, name, commit, digest string) *lockEntry {
	repo := strings.TrimSuffix(strings.TrimPrefix(src, "github:o/"), "@main")
	return &lockEntry{lockPin: lockPin{Name: name, Owner: "o", Repo: repo, Ref: "main", RefKind: "branch", Commit: commit, Digest: digest},
		Dir: "github/o/" + repo + "/" + commit + ".0000000000000000"}
}

// setPins writes the lock and the base of relSrc (the other source stays at C).
func (f *relFixture) setPins(t *testing.T, lock, base relPin) {
	t.Helper()
	lk := f.r.cfg.lock
	l := &pluginLock{Version: 1, Plugins: map[string]*lockEntry{
		relSrc:   relEntry(relSrc, "p", lock.Commit, lock.Digest),
		relOther: relEntry(relOther, "q", relCommitC, relDigestC)}}
	if err := writeLock(lk, l); err != nil {
		t.Fatal(err)
	}
	b := &peerBase{Version: 1, Peers: map[string]map[string]basePin{pinHub: {
		relSrc:   {Commit: base.Commit, Digest: base.Digest},
		relOther: {Commit: relCommitC, Digest: relDigestC}}}}
	if err := writePeers(lk, b); err != nil {
		t.Fatal(err)
	}
}

func newRelFixture(t *testing.T, kind string) *relFixture {
	t.Helper()
	dir := t.TempDir()
	f := &relFixture{dir: dir, cfg: filepath.Join(dir, "hub.json"), run: filepath.Join(dir, "run")}
	state := filepath.Join(dir, "state")
	os.MkdirAll(state, 0o700)
	cfg := map[string]any{"server": "unix:///nowhere/rex.sock", "state": state, "agents": []any{map[string]string{"id": "user"}},
		"plugins": []any{map[string]string{"source": relSrc}, map[string]string{"source": relOther}}}
	b, _ := json.Marshal(cfg)
	os.WriteFile(f.cfg, b, 0o600)
	os.WriteFile(filepath.Join(state, "plugins.json"), []byte(`{"version":1,"plugins":[]}`), 0o600)
	spec := map[string]any{"kind": kind, "config": f.cfg, "server": "unix:///nowhere/rex.sock"}
	if kind == "hub" {
		spec["h"], spec["release_dir"], spec["dev_log"] = relCommitB, filepath.Join(dir, "release"), filepath.Join(dir, "dev.log")
	} else {
		spec["plugin"], spec["to"] = "p", relCommitB
	}
	sb, _ := json.Marshal(spec)
	specPath := filepath.Join(dir, "spec.json")
	os.WriteFile(specPath, sb, 0o600)
	if err := createRun(f.run, specPath); err != nil {
		t.Fatal(err)
	}
	f.out = &bytes.Buffer{}
	r, err := openRun(f.run, "test", f.out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.unlock)
	r.now = func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) }
	f.r = r
	f.setPins(t, relPin{pinCommit, pinDigest}, relPin{pinCommit, pinDigest})
	return f
}

func (f *relFixture) candidate() *relCandidate {
	return &relCandidate{Kind: f.r.spec.Kind, Source: relSrc, Name: "p", From: relPin{pinCommit, pinDigest}, To: relPin{relCommitB, relDigestB}, Peer: pinHub}
}

// Each spec field is checked; unknown fields are refused.
func TestReleaseSpecRefused(t *testing.T) {
	ok := func() relSpec {
		return relSpec{Kind: "hub", H: relCommitB, Config: "/c.json", Server: "unix:///s", ReleaseDir: "/rel", DevLog: "/dev.log"}
	}
	if s := ok(); s.check() != nil {
		t.Fatal(s.check())
	}
	p := relSpec{Kind: "plugin", Plugin: "messaging", To: relCommitB, Tag: "v1.2.3", Config: "/c.json", Server: "unix:///s"}
	if err := p.check(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*relSpec){
		"kind":         func(s *relSpec) { s.Kind = "other" },
		"h form":       func(s *relSpec) { s.H = "abc" },
		"no config":    func(s *relSpec) { s.Config = "" },
		"no dev_log":   func(s *relSpec) { s.DevLog = "" },
		"relative":     func(s *relSpec) { s.ReleaseDir = "rel" },
		"plugin field": func(s *relSpec) { s.Plugin = "x" },
		"absent rel":   func(s *relSpec) { s.Absent = []string{"sql"} },
	} {
		s := ok()
		mod(&s)
		if s.check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p.Tag = "1.2"
	if p.check() == nil {
		t.Error("a tag without the vX.Y.Z form was accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	os.WriteFile(path, []byte(`{"kind":"hub","h":"`+relCommitB+`","config":"/c","server":"s","release_dir":"/r","dev_log":"/d","extra":1}`), 0o600)
	if _, err := readSpec(path); err == nil || !strings.Contains(err.Error(), "extra") {
		t.Errorf("an unknown field: %v", err)
	}
}

// B-u1: a second operator fails at once, with the holder's pid.
func TestReleaseOperatorLock(t *testing.T) {
	f := newRelFixture(t, "hub")
	if _, err := lockOperator(f.cfg); err == nil || !strings.Contains(err.Error(), "another release step runs") {
		t.Fatalf("a second operator: %v", err)
	}
	if _, err := openRun(f.run, "x", &bytes.Buffer{}); err == nil {
		t.Fatal("a second step opened the run while the first held the flock")
	}
	f.r.unlock()
	unlock, err := lockOperator(f.cfg)
	if err != nil {
		t.Fatalf("after the release: %v", err)
	}
	unlock()
	f.r.unlock = func() {}
}

// A mutation writes begin and end records. A test stop at begin skips the mutation; at after it
// skips the end record. lastBegin then returns the saved expectation.
func TestReleaseMutateJournal(t *testing.T) {
	f := newRelFixture(t, "hub")
	calls := 0
	op := func() (any, error) { calls++; return map[string]int{"n": calls}, nil }
	if err := f.r.mutate("replace", map[string]string{"expect_instance": "A"}, op); err != nil {
		t.Fatal(err)
	}
	if _, open, _ := f.r.lastBegin("replace"); open {
		t.Error("a completed mutation is open")
	}
	f.r.testStop = "replace:begin"
	if err := f.r.mutate("replace", map[string]string{"expect_instance": "B"}, op); !errors.Is(err, errTestStop) || calls != 1 {
		t.Fatalf("stop at begin: %v, calls %d", err, calls)
	}
	data, open, err := f.r.lastBegin("replace")
	if err != nil || !open || !strings.Contains(string(data), `"B"`) {
		t.Fatalf("the saved expectation after a stop at begin: %s %v %v", data, open, err)
	}
	f.r.testStop = "replace:after"
	if err := f.r.mutate("replace", map[string]string{"expect_instance": "C"}, op); !errors.Is(err, errTestStop) || calls != 2 {
		t.Fatalf("stop after: %v, calls %d", err, calls)
	}
	if data, open, _ := f.r.lastBegin("replace"); !open || !strings.Contains(string(data), `"C"`) {
		t.Fatalf("after a stop after the mutation: %s %v", data, open)
	}
	recs, _ := f.r.journal()
	if len(recs) != 4 || recs[0].Phase != "begin" || recs[1].Phase != "end" {
		t.Errorf("journal: %+v", recs)
	}
}

// RELEASE_TEST_STOP is refused on a real release and outside a proved lab: a live run never stops
// half way on purpose.
func TestReleaseTestStopGate(t *testing.T) {
	f := newRelFixture(t, "hub")
	t.Setenv("RELEASE_TEST_STOP", "replace:begin")
	if err := f.r.maybeStop("replace", "begin"); err == nil || errors.Is(err, errTestStop) || !strings.Contains(err.Error(), "proved lab") {
		t.Errorf("no lab proof: %v", err)
	}
	f.r.spec.Real = true
	if err := f.r.maybeStop("replace", "begin"); err == nil || !strings.Contains(err.Error(), "real release") {
		t.Errorf("a real release: %v", err)
	}
}

// A-u4 (part): a child gets no Rex selector of the caller's pane, and the release's server.
func TestReleaseChildEnv(t *testing.T) {
	env := childEnv([]string{"PATH=/bin", "REX_SESSION=s", "REX_BLOCK=b", "REX_CLIENT=c", "REX_SERVER=unix:///real", "HOME=/h"}, "unix:///lab/rex.sock")
	got := strings.Join(env, " ")
	if got != "PATH=/bin HOME=/h REX_SERVER=unix:///lab/rex.sock" {
		t.Errorf("env: %s", got)
	}
}

// The pin state names the lock pin and the base pin of the moved source.
func TestReleasePinState(t *testing.T) {
	f := newRelFixture(t, "hub")
	c := f.candidate()
	from, to, other := relPin{pinCommit, pinDigest}, relPin{relCommitB, relDigestB}, relPin{relCommitC, relDigestC}
	for _, tc := range []struct {
		lock, base relPin
		want       string
	}{{from, from, "from from"}, {to, to, "to to"}, {to, from, "to from"}, {from, to, "from to"}, {other, from, "? from"}} {
		f.setPins(t, tc.lock, tc.base)
		if got, err := f.r.pinState(c); err != nil || got != tc.want {
			t.Errorf("lock %s base %s: %q %v, want %q", tc.lock.Commit[:4], tc.base.Commit[:4], got, err, tc.want)
		}
	}
	// A digest that differs makes another pin.
	f.setPins(t, relPin{relCommitB, relDigestC}, to)
	if got, _ := f.r.pinState(c); got != "? to" {
		t.Errorf("same commit, other digest: %q", got)
	}
}

// The documents move only the moved source, carry the base peer, and pass the hub's own check.
func TestReleasePinDocs(t *testing.T) {
	f := newRelFixture(t, "hub")
	c := f.candidate()
	l, _ := readLock(f.r.cfg.lock)
	fwd, back, err := pinDocs(c, l.Plugins[relSrc])
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		b    []byte
		want relPin
	}{{fwd, c.To}, {back, c.From}} {
		d, err := decodePinDoc(tc.b)
		if err != nil {
			t.Fatal(err)
		}
		if d.Hub != pinHub || len(d.Entries) != 1 || d.Entries[0].Source != relSrc || (relPin{d.Entries[0].Commit, d.Entries[0].Digest}) != tc.want || d.Entries[0].Name != "p" {
			t.Errorf("document: %s", tc.b)
		}
	}
	if p, err := onePeer(f.r.cfg.lock); err != nil || p != pinHub {
		t.Errorf("onePeer: %q %v", p, err)
	}
}

// Failure case 1: the pin comparison ignores the cache dirs; the pins count.
func TestReleaseLockPinsIgnoreDir(t *testing.T) {
	f := newRelFixture(t, "hub")
	a, _ := lockPinsHash(f.r.cfg.lock)
	l, _ := readLock(f.r.cfg.lock)
	l.Plugins[relSrc].Dir = "github/o/r/" + pinCommit + ".ffffffffffffffff"
	writeLock(f.r.cfg.lock, l)
	b, _ := lockPinsHash(f.r.cfg.lock)
	if a != b {
		t.Error("a new cache dir changed the pin hash")
	}
	l.Plugins[relOther].Commit = relCommitB
	writeLock(f.r.cfg.lock, l)
	if c, _ := lockPinsHash(f.r.cfg.lock); c == a {
		t.Error("a changed pin kept the pin hash")
	}
}

// A release moves one source: another pin, another base, the config or the managed file that
// differs from the backup is refused.
func TestReleaseOthersEqual(t *testing.T) {
	f := newRelFixture(t, "plugin")
	c := f.candidate()
	b, err := f.r.writeBackup("s1")
	if err != nil {
		t.Fatal(err)
	}
	f.setPins(t, c.To, c.To) // the moved source changes: fine
	if err := f.r.othersEqual(c, b); err != nil {
		t.Fatalf("only the moved source changed: %v", err)
	}
	l, _ := readLock(f.r.cfg.lock)
	l.Plugins[relOther].Commit = relCommitB
	writeLock(f.r.cfg.lock, l)
	if err := f.r.othersEqual(c, b); err == nil || !strings.Contains(err.Error(), "a pin other than") {
		t.Errorf("another pin: %v", err)
	}
	f.setPins(t, c.To, c.To)
	base, _ := readPeers(f.r.cfg.lock)
	base.Peers[pinHub][relOther] = basePin{Commit: relCommitB, Digest: relDigestB}
	writePeers(f.r.cfg.lock, base)
	if err := f.r.othersEqual(c, b); err == nil || !strings.Contains(err.Error(), "a base other than") {
		t.Errorf("another base: %v", err)
	}
	f.setPins(t, c.To, c.To)
	os.WriteFile(f.r.cfg.managed, []byte(`{"version":1,"plugins":[ ]}`), 0o600)
	if err := f.r.othersEqual(c, b); err == nil || !strings.Contains(err.Error(), "managed") {
		t.Errorf("the managed file: %v", err)
	}
}

// The hub backup holds the four files, a hub.db copy that passes integrity_check, and the SQL
// listing; an absent path that exists is refused.
func TestReleaseBackup(t *testing.T) {
	f := newRelFixture(t, "hub")
	state := f.r.cfg.state
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE records (k TEXT); INSERT INTO records VALUES ('probe')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	os.MkdirAll(filepath.Join(state, "sql"), 0o700)
	os.WriteFile(filepath.Join(state, "sql", "p.db"), []byte("x"), 0o600)
	b, err := f.r.writeBackup("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.checkBackup(b); err != nil {
		t.Fatal(err)
	}
	copyDB, _ := sql.Open("sqlite", "file:"+filepath.Join(b, bakDB)+"?mode=ro")
	var k string
	if err := copyDB.QueryRow("SELECT k FROM records").Scan(&k); err != nil || k != "probe" {
		t.Errorf("the hub.db copy: %q %v", k, err)
	}
	copyDB.Close()
	list, _ := os.ReadFile(filepath.Join(b, bakSQLList))
	if !strings.HasPrefix(string(list), "p.db 1 ") {
		t.Errorf("listing: %q", list)
	}
	if _, err := f.r.writeBackup("s1"); err == nil {
		t.Error("a second backup with the same stamp was written")
	}
	os.Remove(filepath.Join(b, bakManaged))
	if err := f.r.checkBackup(b); err == nil {
		t.Error("a backup without the managed file passed")
	}
	f.r.spec.Absent = []string{filepath.Join(state, "sql")}
	if err := f.r.checkAbsent(); err == nil {
		t.Error("an absent path that exists passed")
	}
	if err := f.r.setStamp("s1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.r.stamp(); s != "s1" {
		t.Errorf("stamp %q", s)
	}
}

// A binary must be a clean build of its commit; a pass appends one provenance record.
func TestReleaseProvenance(t *testing.T) {
	f := newRelFixture(t, "hub")
	bin := filepath.Join(f.dir, "hub-H")
	os.WriteFile(bin, []byte("binary"), 0o700)
	for _, tc := range []struct {
		rev      string
		modified bool
		want     string
	}{{relCommitC, false, "is built from"}, {relCommitB, true, "not a clean build"}, {relCommitB, false, ""}} {
		f.r.buildInfo = func(string) (string, bool, error) { return tc.rev, tc.modified, nil }
		err := f.r.checkProvenance(bin, relCommitB, "hub-H")
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("rev %s modified %v: %v", tc.rev[:4], tc.modified, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(f.run, relProvenanceFile))
	if n := strings.Count(string(b), "\n"); n != 1 || !strings.Contains(string(b), "hub-H 2026-10-22T00:00:00Z sha256 "+sha256Bytes([]byte("binary"))) {
		t.Errorf("provenance: %q", b)
	}
}

// C-u1 (core): the record binds the tool, the spec, the run files and the probe bytes; each one
// changed alone refuses, and names the field. A record that is not a pass refuses.
func TestReleaseRehearsalGate(t *testing.T) {
	f := newRelFixture(t, "hub")
	tool := filepath.Join(f.dir, "lazyrex-hub")
	os.WriteFile(tool, []byte("tool"), 0o700)
	probes := filepath.Join(f.dir, "probes.sql")
	os.WriteFile(probes, []byte("select 1;"), 0o600)
	f.r.spec.Probes = probes
	for _, n := range []string{relCandidateFile, relFwdDoc, relBackDoc, relVerifyFwd, relVerifyBack} {
		os.WriteFile(filepath.Join(f.run, n), []byte(n), 0o600)
	}
	if err := f.r.checkRehearsal(tool); err == nil {
		t.Fatal("no record passed")
	}
	bound, err := f.r.bindings(tool)
	if err != nil {
		t.Fatal(err)
	}
	if bound["lab-transform"] != "-" {
		t.Errorf("a missing file is bound as -: %q", bound["lab-transform"])
	}
	f.r.writeRehearsal(&relRehearsal{Pass: true, Bound: bound, Tier: tierDefault})
	if err := f.r.checkRehearsal(tool); err != nil {
		t.Fatalf("the same run: %v", err)
	}
	change := map[string]string{"tool": tool, "probes": probes, "spec": filepath.Join(f.run, relSpecFile),
		"candidate": filepath.Join(f.run, relCandidateFile), "pins-fwd": filepath.Join(f.run, relFwdDoc),
		"pins-back": filepath.Join(f.run, relBackDoc), "verify-fwd": filepath.Join(f.run, relVerifyFwd),
		"verify-back": filepath.Join(f.run, relVerifyBack), "lab-transform": filepath.Join(f.run, relLabTransform)}
	for field, path := range change {
		old, had := os.ReadFile(path)
		os.WriteFile(path, append(old, '!'), 0o600)
		if err := f.r.checkRehearsal(tool); err == nil || !strings.Contains(err.Error(), "does not cover this release: "+field) {
			t.Errorf("%s changed: %v", field, err)
		}
		if had == nil {
			os.WriteFile(path, old, 0o600)
		} else {
			os.Remove(path)
		}
	}
	f.r.writeRehearsal(&relRehearsal{Pass: false, Bound: bound})
	if err := f.r.checkRehearsal(tool); err == nil || !strings.Contains(err.Error(), "not a pass") {
		t.Errorf("a failed record: %v", err)
	}
}

// An unknown step and a step without -run are usage errors.
func TestReleaseMainUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if got := releaseMain(nil, &out, &errb); got != relUsage {
		t.Errorf("no step: %d", got)
	}
	if got := releaseMain([]string{"nosuchstep"}, &out, &errb); got != relUsage || !strings.Contains(errb.String(), "unknown step") {
		t.Errorf("unknown step: %d %s", got, errb.String())
	}
}
