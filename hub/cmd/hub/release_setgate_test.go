package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setGateFixture is a live plugin step "beta" (the second step of the set alpha, beta) whose state
// equals the rehearsed state: the hub run, its record and set file, the live inputs and the
// candidate. Each case of TestReleaseSetGate changes one part.
type setGateFixture struct {
	r    *releaser
	c    *relCandidate
	hub  *relHub
	tool string
	run  string // the hub run
	sdk  string // the SDK of the release clone
}

func newSetGateFixture(t *testing.T) *setGateFixture {
	t.Helper()
	dir := t.TempDir()
	write := func(p string, b []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := &setGateFixture{tool: filepath.Join(dir, "tool"), run: filepath.Join(dir, "hubrun")}
	write(f.tool, []byte("tool v1"))
	cfg := &pluginCmdConfig{path: filepath.Join(dir, "live", "config.json"), managed: filepath.Join(dir, "live", "managed.json"), lock: filepath.Join(dir, "live", "lock.json")}
	write(cfg.path, []byte(`{"config":1}`))
	write(cfg.managed, []byte(`{"managed":1}`))
	base, _ := json.Marshal(peerBase{Version: pinDocVersion, Peers: map[string]map[string]basePin{setGatePeer: {"gh:o/beta": {Commit: setFrom, Digest: sgB}}}})
	write(peersPath(cfg.lock), base)
	releaseDir := filepath.Join(dir, "release")
	f.sdk = filepath.Join(releaseDir, "LazyRex", "sdk", "lua")
	write(filepath.Join(f.sdk, "x.lua"), []byte("x"))

	// The hub run: spec, candidate, journal, the H hub, set file and record.
	hubSpec := map[string]any{"kind": "hub", "h": strings.Repeat("a", 40), "config": cfg.path, "server": "unix:///live.sock",
		"release_dir": releaseDir, "dev_log": filepath.Join(dir, "dev.log")}
	sb, _ := json.Marshal(hubSpec)
	write(filepath.Join(f.run, relSpecFile), sb)
	cb, _ := json.Marshal(relCandidate{Kind: "hub", To: relPin{Commit: strings.Repeat("a", 40)}})
	write(filepath.Join(f.run, relCandidateFile), cb)
	write(filepath.Join(f.run, "journal.jsonl"), []byte(`{"at":"t","step":"start","op":"x","phase":"end"}`+"\n"))
	f.hub = &relHub{PID: 4242, Start: "Mon 1", Client: "c"}
	hb, _ := json.Marshal(f.hub)
	write(filepath.Join(f.run, relHubFile), hb)
	write(filepath.Join(f.run, relSetJSON), setBytes(seal(t, setEntry("alpha")), seal(t, setEntry("beta"))))

	f.c = &relCandidate{Kind: "plugin", Source: "gh:o/beta", Name: "beta", From: relPin{setFrom, sgB}, To: relPin{setTo, sgT},
		Peer: setGatePeer, Pins: []string{"gh:o/alpha " + setTo + " " + sgAT, "gh:o/beta " + setFrom + " " + sgB},
		HubCommit: strings.Repeat("a", 40), SDKDir: f.sdk, SDKDigest: "sdk1"}
	lab := *f.c
	lab.SDKDir = "/tmp/lab/sdk"
	bp, err := basePinLines(cfg.lock)
	if err != nil {
		t.Fatal(err)
	}
	f.r = &releaser{run: filepath.Join(dir, "pluginrun"), cfg: cfg,
		spec: &relSpec{Kind: "plugin", Plugin: "beta", To: setTo, Config: cfg.path, Server: "unix:///live.sock"}}
	f.writeRecord(t, func(rec *relRehearsal) {
		rec.SetSteps = []relSetStep{{Plugin: "alpha", Candidate: relCandidate{Name: "alpha"}}, {Plugin: "beta", Candidate: lab, BasePins: bp}}
	})
	return f
}

// writeRecord writes a passing set record that binds the current files, with edit applied, and
// points the spec at it.
func (f *setGateFixture) writeRecord(t *testing.T, edit func(*relRehearsal)) {
	t.Helper()
	sum := func(p string) string {
		h, err := sha256File(p)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	rec := &relRehearsal{Pass: true, Tier: tierFull, Chunks: []relChunkCheck{{}, {}}, Bound: map[string]string{
		"tool": sum(f.tool), "set": sum(filepath.Join(f.run, relSetJSON)), "spec": sum(filepath.Join(f.run, relSpecFile)),
		"candidate": sum(filepath.Join(f.run, relCandidateFile)), "live-config": sum(f.r.cfg.path), "live-managed": sum(f.r.cfg.managed)}}
	if f.r.spec.Set != nil {
		old, err := os.ReadFile(filepath.Join(f.run, relRehearsalFile))
		if err == nil {
			json.Unmarshal(old, rec)
		}
	}
	edit(rec)
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(f.run, relRehearsalFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	f.r.spec.Set = &relSpecSet{Run: f.run, RecordSHA256: sha256Bytes(b)}
}

// TestReleaseSetGate: the gate passes on the rehearsed state and refuses each mismatch (plan r3,
// section 4).
func TestReleaseSetGate(t *testing.T) {
	f := newSetGateFixture(t)
	if err := f.r.checkSetGate(f.c, f.hub, f.tool); err != nil {
		t.Fatalf("the rehearsed state: %v", err)
	}
	for name, tc := range map[string]struct {
		change func(t *testing.T, f *setGateFixture)
		want   string
	}{
		"tool":            {func(t *testing.T, f *setGateFixture) { os.WriteFile(f.tool, []byte("tool v2"), 0o600) }, "is not the tool of the rehearsal"},
		"record bytes":    {func(t *testing.T, f *setGateFixture) { f.r.spec.Set.RecordSHA256 = strings.Repeat("0", 64) }, "has sha256"},
		"record not pass": {func(t *testing.T, f *setGateFixture) { f.writeRecord(t, func(r *relRehearsal) { r.Pass = false }) }, "not a passing set record"},
		"one chunk window": {func(t *testing.T, f *setGateFixture) {
			f.writeRecord(t, func(r *relRehearsal) { r.Chunks = r.Chunks[:1] })
		}, "not a passing set record"},
		"set bytes": {func(t *testing.T, f *setGateFixture) {
			os.WriteFile(filepath.Join(f.run, relSetJSON), setBytes(seal(t, setEntry("alpha")), seal(t, setEntry("gamma"))), 0o600)
		}, "set file of the hub run changed"},
		"config":  {func(t *testing.T, f *setGateFixture) { os.WriteFile(f.r.cfg.path, []byte(`{"config":2}`), 0o600) }, "live-config"},
		"managed": {func(t *testing.T, f *setGateFixture) { os.WriteFile(f.r.cfg.managed, []byte(`{}`), 0o600) }, "live-managed"},
		"spec plugin": {func(t *testing.T, f *setGateFixture) {
			f.r.spec.Plugin = "gamma"
		}, "has no plugin gamma"},
		"spec to": {func(t *testing.T, f *setGateFixture) { f.r.spec.To = strings.Repeat("e", 40) }, "this run moves it"},
		"candidate TO": {func(t *testing.T, f *setGateFixture) {
			f.c.To.Commit = strings.Repeat("e", 40)
		}, "this run moves it"},
		"candidate digest": {func(t *testing.T, f *setGateFixture) { f.c.To.Digest = "sha256:other" }, "the TO pin of the frozen candidate"},
		"source":           {func(t *testing.T, f *setGateFixture) { f.c.Source = "gh:o/other" }, "the source of the frozen candidate"},
		// The order: an earlier step not yet released leaves its FROM pin in the lock.
		"lock pins (order)": {func(t *testing.T, f *setGateFixture) {
			f.c.Pins = []string{"gh:o/alpha " + setFrom + " " + sgAF, "gh:o/beta " + setFrom + " " + sgB}
		}, "the lock pins of the frozen candidate"},
		"base pins": {func(t *testing.T, f *setGateFixture) {
			b, _ := json.Marshal(peerBase{Version: pinDocVersion, Peers: map[string]map[string]basePin{setGatePeer: {"gh:o/beta": {Commit: setTo, Digest: sgT}}}})
			os.WriteFile(peersPath(f.r.cfg.lock), b, 0o600)
		}, "the base pins differ"},
		"SDK digest": {func(t *testing.T, f *setGateFixture) { f.c.SDKDigest = "sdk2" }, "the SDK digest of the frozen candidate"},
		"SDK root": {func(t *testing.T, f *setGateFixture) {
			other := filepath.Join(t.TempDir(), "sdk")
			os.MkdirAll(other, 0o700)
			f.c.SDKDir = other
		}, "uses the SDK"},
		"no SDK": {func(t *testing.T, f *setGateFixture) { f.c.SDKDir = "" }, "has no -sdk directory"},
		"hub instance": {func(t *testing.T, f *setGateFixture) {
			f.hub = &relHub{PID: f.hub.PID, Start: "Tue 2", Client: f.hub.Client}
		}, "is not the H hub that the hub run started"},
		"hub commit": {func(t *testing.T, f *setGateFixture) { f.c.HubCommit = strings.Repeat("b", 40) }, "the hub commit of the frozen candidate"},
		"hub run rolled back": {func(t *testing.T, f *setGateFixture) {
			fh, _ := os.OpenFile(filepath.Join(f.run, "journal.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
			fh.WriteString(`{"at":"t","step":"rollback","op":"y","phase":"begin"}` + "\n")
			fh.Close()
		}, "the hub run has a rollback"},
		"hub run spec": {func(t *testing.T, f *setGateFixture) {
			b, _ := os.ReadFile(filepath.Join(f.run, relSpecFile))
			os.WriteFile(filepath.Join(f.run, relSpecFile), []byte(strings.Replace(string(b), "live.sock", "other.sock", 1)), 0o600)
		}, "the spec of the hub run is not the rehearsed spec"},
		"hub run candidate": {func(t *testing.T, f *setGateFixture) {
			os.WriteFile(filepath.Join(f.run, relCandidateFile), []byte(`{"kind":"hub"}`), 0o600)
		}, "the candidate of the hub run is not the rehearsed candidate"},
		// A second passing hub run: its own record passes, but the spec binds the bytes of the first.
		"second hub run": {func(t *testing.T, f *setGateFixture) {
			g := newSetGateFixture(t)
			f.r.spec.Set.Run = g.run
		}, "has sha256"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSetGateFixture(t)
			tc.change(t, f)
			err := f.r.checkSetGate(f.c, f.hub, f.tool)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want %q", err, tc.want)
			}
		})
	}
}

// TestReleaseSetSpec: "set" belongs to a live plugin spec only.
func TestReleaseSetSpec(t *testing.T) {
	ok := &relSpecSet{Run: "/runs/hub", RecordSHA256: strings.Repeat("a", 64)}
	plugin := func(s *relSpecSet, lab bool) *relSpec {
		return &relSpec{Kind: "plugin", Plugin: "beta", To: strings.Repeat("b", 40), Config: "/c.json", Server: "unix:///s", Lab: lab, Set: s}
	}
	if err := plugin(ok, false).check(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*relSpec{
		"lab":          plugin(ok, true),
		"relative run": plugin(&relSpecSet{Run: "runs/hub", RecordSHA256: ok.RecordSHA256}, false),
		"short sha256": plugin(&relSpecSet{Run: ok.Run, RecordSHA256: "abc"}, false),
		"upper sha256": plugin(&relSpecSet{Run: ok.Run, RecordSHA256: strings.Repeat("A", 64)}, false),
		"hub with set": {Kind: "hub", H: strings.Repeat("a", 40), Config: "/c.json", Server: "unix:///s", ReleaseDir: "/r", DevLog: "/d", Set: ok},
	} {
		if err := s.check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// setGatePeer is the pull identity of the fixture base.
var setGatePeer = strings.Repeat("c", 32)

// The fixture digests.
var (
	sgB  = "sha256:" + strings.Repeat("1", 64)
	sgT  = "sha256:" + strings.Repeat("2", 64)
	sgAT = "sha256:" + strings.Repeat("3", 64)
	sgAF = "sha256:" + strings.Repeat("4", 64)
)
