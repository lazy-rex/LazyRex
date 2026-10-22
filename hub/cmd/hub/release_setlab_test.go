package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The release-set lab test: a hub release from the live hub commit to an H with tests/sdk-parts,
// then a set of two real 4.2 plugin steps in live order (Tunnels, Messaging) with lab-made seeds.
// Lab only: RELEASE_LAB=1. It needs the network (GitHub), go and rex, and runs for many minutes.
const (
	setLabOld = "25f8c7db5b41d7024db050d434204e1444228404" // the live hub commit on 2026-10-22
	setLabH   = "b008ff8423cd2f5efcba40678cb1f1f13f848b2a" // main with the chunking series (tests/sdk-parts)
)

var setLabPlugins = []struct {
	name, source, from, to string
}{
	{"tunnels", "github:lazy-rex/Tunnels", "b41fa4f64ca7b44566e2e5d595c276357b841555", "78658739c0b9e0b769c50e49d8e596e85c898a5d"},
	{"messaging", "github:lazy-rex/Messaging", "66a31533fd6c047ef60e23eb59e47cdce532a9aa", "a2cc4483652c1be27580bfa0f147d45a80c351d3"},
}

// setLabFile is the set file of the test: Tunnels with no seed and a list probe; Messaging with one
// message to the seed agent itself, and action and sql probes.
func setLabFile(t *testing.T) []byte {
	act := func(action string, args map[string]any, expect ...map[string]any) map[string]any {
		it := map[string]any{"kind": "action", "action": action, "args": args}
		if len(expect) > 0 {
			it["expect"] = expect
		}
		return it
	}
	val := func(p string, v any) map[string]any { return map[string]any{"pointer": p, "value": v} }
	empty := func(p string) map[string]any { return map[string]any{"pointer": p, "empty": true} }
	entry := func(i int, seed, probe []any, keys map[string]any) map[string]any {
		p := setLabPlugins[i]
		return seal(t, map[string]any{"plugin": p.name, "from": p.from, "to": p.to, "seed": seed, "probe": probe,
			"keys": keys, "changes": []any{}, "inserts": []any{}})
	}
	send := map[string]any{"to": "<SELF>", "body": "lab seed one", "key": "lab-seed-send-1"}
	return setBytes(
		entry(0, []any{}, []any{act("tunnels.list", map[string]any{}, empty("/forwards"), empty("/pending"), empty("/cleanups"))}, map[string]any{}),
		entry(1, []any{act("messaging.send", send)}, []any{
			act("messaging.history", map[string]any{"thread": "<SELF>", "limit": 1}, val("/messages/0/body", "lab seed one"), val("/thread", "<SELF>")),
			act("messaging.send", send, val("/key", "lab-seed-send-1")),
			map[string]any{"kind": "sql", "sql": "SELECT body, sender FROM messages ORDER BY seq", "rows": []any{[]any{"lab seed one", relSeedAgent}}},
		}, map[string]any{"clock": []any{"one"}, "messages": []any{"id"}}),
	)
}

func TestReleaseRehearseLabSet(t *testing.T) {
	if os.Getenv("RELEASE_LAB") != "1" {
		t.Skip("set RELEASE_LAB=1 to run the release lab test (GitHub, a lab Rex server, minutes)")
	}
	bin := filepath.Join(t.TempDir(), "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	f := t.TempDir()
	var plugins []labPlugin
	for _, p := range setLabPlugins {
		plugins = append(plugins, labPlugin{p.source, p.from})
	}
	labFixtureAt(t, f, bin, setLabOld, setLabH, plugins)
	run := filepath.Join(f, "run")
	setPath := filepath.Join(f, "set.json")
	if err := os.WriteFile(setPath, setLabFile(t), 0o600); err != nil {
		t.Fatal(err)
	}
	lab := filepath.Join("/tmp", fmt.Sprintf("rexlab-release-t%dset", os.Getpid()))
	if out, err := labRehearse(t, bin, f, run, lab, nil, "-set", setPath); err != nil {
		t.Fatalf("rehearse -set: %v (log %s; the lab %s is kept)", err, out, lab)
	}
	var rec relRehearsal
	if err := json.Unmarshal(mustReadFile(filepath.Join(run, relRehearsalFile)), &rec); err != nil {
		t.Fatal(err)
	}
	if !rec.Pass {
		t.Fatalf("record: not a pass")
	}
	// The record binds the set file that the run holds.
	if got, want := rec.Bound["set"], sha256Bytes(mustReadFile(filepath.Join(run, relSetJSON))); got != want || got == "-" {
		t.Errorf("record: set bound %s, the run's set.json %s", got, want)
	}
	// One frozen step per plugin, in set order, with the set pins.
	if len(rec.SetSteps) != len(setLabPlugins) {
		t.Fatalf("record: %d set steps", len(rec.SetSteps))
	}
	for i, p := range setLabPlugins {
		s := rec.SetSteps[i]
		if s.Plugin != p.name || s.Candidate.From.Commit != p.from || s.Candidate.To.Commit != p.to || s.Candidate.HubCommit != setLabH || len(s.BasePins) == 0 {
			t.Errorf("record: step %d %+v", i+1, s)
		}
	}
	// The second step froze after the first moved: its lock pins hold Tunnels at TO.
	if pins := strings.Join(rec.SetSteps[1].Candidate.Pins, "\n"); !strings.Contains(pins, " "+setLabPlugins[0].to+" ") {
		t.Errorf("record: the Messaging step saw the pins\n%s", pins)
	}
	if len(rec.Chunks) != 2 {
		t.Errorf("record: %d chunking windows", len(rec.Chunks))
	}
	// The check points, in order.
	var phases []string
	for _, c := range rec.Set {
		if len(phases) == 0 || phases[len(phases)-1] != c.Phase {
			phases = append(phases, c.Phase)
		}
	}
	got := strings.Join(phases, "|")
	for _, want := range []string{"seed", "seeded", "probes on FROM", "tunnels backup", "tunnels replace", "tunnels verify", "tunnels rerun",
		"tunnels probes on TO", "messaging rerun", "messaging probes on TO", "chunking probe", "messaging rollback",
		"messaging probes after the rollback", "tunnels rollback", "tunnels probes after the rollback"} {
		if !strings.Contains("|"+got+"|", "|"+want+"|") {
			t.Errorf("record: no check point %q in %s", want, got)
		}
	}
	if strings.Index(got, "messaging rollback") > strings.Index(got, "tunnels rollback") {
		t.Errorf("record: the rollback is not newest first: %s", got)
	}
	if _, err := os.Stat(lab); err == nil {
		t.Errorf("the lab %s is still there after a pass", lab)
	}
}
