package main

// The live gate of a release set (plan r3, section 4). A live plugin spec with "set" names the hub
// run that released H and the sha256 of its rehearsal.json. The plugin precheck keeps all its
// other checks; the set gate replaces only its read of a rehearsal record in its own run. The gate
// passes only when the live state before this step is the state that the set rehearsal saw before
// the same step: the same tool, set file and live inputs, the same frozen candidate, the same lock
// and base pins (so every earlier step is released and no later one), the hub instance that the
// hub run started, and the SDK of the release clone.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

// basePinLines are the base pins of the lock lockFile as "<peer> <source> <commit> <digest> <tag>",
// sorted.
func basePinLines(lockFile string) ([]string, error) {
	b, err := readPeers(lockFile)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for peer, pins := range b.Peers {
		for src, p := range pins {
			out = append(out, fmt.Sprintf("%s %s %s %s %s", peer, src, p.Commit, p.Digest, p.Tag))
		}
	}
	sort.Strings(out)
	return out, nil
}

// recordStep saves the lab candidate of one plugin step and the base pins of the lab lock, right
// after its lab freeze.
func (h *rehearsal) recordStep(e *relSetEntry, run string) error {
	c, err := (&releaser{run: run}).readCandidate()
	if err != nil {
		return relFail("%s: %v", e.Plugin, err)
	}
	cfg, err := readPluginCmdConfig(h.cfg)
	if err != nil {
		return err
	}
	base, err := basePinLines(cfg.lock)
	if err != nil {
		return relFail("the base pins of the lab: %v", err)
	}
	h.set.steps = append(h.set.steps, relSetStep{Plugin: e.Plugin, Candidate: *c, BasePins: base})
	return nil
}

// checkSetGate is the set gate of a live plugin precheck. c is the frozen candidate of this run,
// hub the running hub that the precheck found, tool the tool binary.
func (r *releaser) checkSetGate(c *relCandidate, hub *relHub, tool string) error {
	set := r.spec.Set
	fail := func(format string, a ...any) error {
		return relFail("the set gate: "+format+"; this is not the rehearsed step", a...)
	}
	// The record: its exact bytes, a pass, both chunking windows and a frozen step per plugin.
	recPath := filepath.Join(set.Run, relRehearsalFile)
	recBytes, err := os.ReadFile(recPath)
	if err != nil {
		return fail("no set record: %v", err)
	}
	if h := sha256Bytes(recBytes); h != set.RecordSHA256 {
		return fail("the record %s has sha256 %s, not %s", recPath, h, set.RecordSHA256)
	}
	var rec relRehearsal
	if err := decodeStrict(recBytes, &rec); err != nil {
		return fail("%s: %v", recPath, err)
	}
	if !rec.Pass || len(rec.Chunks) != 2 || len(rec.SetSteps) == 0 {
		return fail("the record is not a passing set record (pass %v, %d chunking windows, %d steps)", rec.Pass, len(rec.Chunks), len(rec.SetSteps))
	}
	// The tool and the set file.
	toolSum, err := sha256File(tool)
	if err != nil {
		return relFail("the sha256 of the tool %s: %v", tool, err)
	}
	if toolSum != rec.Bound["tool"] {
		return fail("the tool %s is not the tool of the rehearsal", tool)
	}
	setBytes, err := os.ReadFile(filepath.Join(set.Run, relSetJSON))
	if err != nil {
		return fail("the set file: %v", err)
	}
	if sha256Bytes(setBytes) != rec.Bound["set"] {
		return fail("the set file of the hub run changed after the rehearsal")
	}
	sf, err := parseSet(setBytes)
	if err != nil {
		return fail("the set file: %v", err)
	}
	if len(sf.Plugins) != len(rec.SetSteps) {
		return fail("the set has %d plugins and the record %d steps", len(sf.Plugins), len(rec.SetSteps))
	}
	// The live inputs that the rehearsal copied.
	for _, f := range [][2]string{{"live-config", r.cfg.path}, {"live-managed", r.cfg.managed}} {
		h, err := sha256File(f[1])
		if err != nil {
			return relFail("the sha256 of %s: %v", f[1], err)
		}
		if h != rec.Bound[f[0]] {
			return fail("%s (%s) changed after the rehearsal", f[0], f[1])
		}
	}
	// This step: its entry, and the candidate that the lab froze before the same step.
	i := slices.IndexFunc(rec.SetSteps, func(s relSetStep) bool { return s.Plugin == r.spec.Plugin })
	if i < 0 {
		return fail("the set has no plugin %s", r.spec.Plugin)
	}
	e, step := sf.Plugins[i], rec.SetSteps[i]
	if e.Plugin != step.Plugin {
		return fail("set entry %d is %s, but record step %d is %s", i+1, e.Plugin, i+1, step.Plugin)
	}
	if e.From != c.From.Commit || e.To != c.To.Commit || r.spec.To != e.To {
		return fail("the set moves %s from %s to %s; this run moves it from %s to %s", e.Plugin, e.From, e.To, c.From.Commit, c.To.Commit)
	}
	lc := step.Candidate
	for _, f := range []struct {
		name string
		ok   bool
	}{
		{"kind", c.Kind == lc.Kind},
		{"source", c.Source == lc.Source},
		{"name", c.Name == lc.Name},
		{"FROM pin", c.From == lc.From},
		{"TO pin", c.To == lc.To},
		{"peer", c.Peer == lc.Peer},
		{"lock pins", slices.Equal(c.Pins, lc.Pins)},
		{"hub commit", c.HubCommit == lc.HubCommit},
		{"SDK digest", c.SDKDigest == lc.SDKDigest},
		{"FROM manifest sdk", c.FromSDK == lc.FromSDK},
		{"TO manifest sdk", c.ToSDK == lc.ToSDK},
		{"full-tier reasons", slices.Equal(c.Reasons, lc.Reasons)},
	} {
		if !f.ok {
			return fail("the %s of the frozen candidate differs from the rehearsed step %d (%s)", f.name, i+1, step.Plugin)
		}
	}
	base, err := basePinLines(r.cfg.lock)
	if err != nil {
		return relFail("the base pins: %v", err)
	}
	if !slices.Equal(base, step.BasePins) {
		return fail("the base pins differ from the rehearsed step %d (%s)", i+1, step.Plugin)
	}
	// The hub run: the hub release of this config and server that started the running H hub, never
	// rolled back.
	hr := &releaser{run: set.Run}
	if sum, err := sha256File(filepath.Join(set.Run, relSpecFile)); err != nil || sum != rec.Bound["spec"] {
		return fail("the spec of the hub run is not the rehearsed spec (%v)", err)
	}
	if sum, err := sha256File(filepath.Join(set.Run, relCandidateFile)); err != nil || sum != rec.Bound["candidate"] {
		return fail("the candidate of the hub run is not the rehearsed candidate (%v)", err)
	}
	hs, err := readSpec(filepath.Join(set.Run, relSpecFile))
	if err != nil {
		return fail("the hub run spec: %v", err)
	}
	hc, err := hr.readCandidate()
	if err != nil {
		return fail("the hub run: %v", err)
	}
	if hs.Kind != "hub" || hs.Lab || hs.Config != r.spec.Config || hs.Server != r.spec.Server {
		return fail("the hub run is not a live hub release of config %s on %s", r.spec.Config, r.spec.Server)
	}
	if hc.To.Commit != c.HubCommit {
		return fail("the hub run released %s, but the running hub is %s", hc.To.Commit, c.HubCommit)
	}
	recs, err := hr.journal()
	if err != nil {
		return fail("the hub run journal: %v", err)
	}
	for _, x := range recs {
		if x.Step == "rollback" {
			return fail("the hub run has a rollback")
		}
	}
	hh := hr.readHub(relHubFile)
	if hh == nil || hh.PID != hub.PID || hh.Start != hub.Start {
		return fail("the running hub (pid %d) is not the H hub that the hub run started", hub.PID)
	}
	// The SDK of the running hub is the SDK of the release clone.
	if c.SDKDir == "" {
		return fail("the running hub has no -sdk directory")
	}
	want, err := resolveExisting(filepath.Join(hs.ReleaseDir, "LazyRex", "sdk", "lua"))
	if err != nil {
		return fail("the SDK of the release clone: %v", err)
	}
	got, err := resolveExisting(c.SDKDir)
	if err != nil {
		return fail("the SDK of the running hub: %v", err)
	}
	if got != want {
		return fail("the running hub uses the SDK %s, not %s", got, want)
	}
	return nil
}
