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
// then a set of three real 4.2 plugin steps in live order (Tunnels, Messaging, Templates) with lab-made seeds.
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
	{"templates", "github:lazy-rex/Templates", "59b40b539ea863ea87def91055f036346341f724", "3d93b3980786bf486432c09d23431a718d96ecf3"},
}

// setLabTemplates are the seed and probe items of the 4.2 Templates kit entry (owner review
// pending): the seed saves a template of its own fresh pane and starts it to a blocked run; the
// probes replay the keyed seed actions with the seed placeholders, and expect the seed session.
const setLabTemplates = `{"seed":[{"action":"templates.save","args":{"name":"lab-seed","root":"<LAB>","session_id":"<SESSION_ID>","panes":{"<BLOCK_ID>":"profile:lab-inert-missing"},"key":"seed-save-1"},"kind":"action"},{"action":"templates.start","args":{"template":"lab-seed","root":"<LAB>","session_id":"<SESSION_ID>","key":"seed-start-1"},"kind":"action"},{"kind":"action","action":"templates.runs","args":{},"until":[{"pointer":"/runs/0/status","value":"blocked"}]},{"action":"templates.dismiss","args":{"run_id":"1","key":"seed-dismiss-1"},"kind":"action"}],"probe":[{"kind":"action","action":"templates.list","args":{},"expect":[{"pointer":"/templates/0/name","value":"lab-seed"},{"pointer":"/templates/0/tabs","value":1}]},{"kind":"action","action":"templates.get","args":{"template":"lab-seed"},"expect":[{"pointer":"/template/name","value":"lab-seed"},{"pointer":"/template/tabs/0/layout/launch/profile","value":"lab-inert-missing"}]},{"kind":"action","action":"templates.runs","args":{},"expect":[{"pointer":"/runs/0/id","value":"1"},{"pointer":"/runs/0/template","value":"lab-seed"},{"pointer":"/runs/0/status","value":"dismissed"},{"pointer":"/runs/0/tabs","value":1},{"pointer":"/runs/0/next_tab","value":1},{"pointer":"/runs/0/session_id","value":"<SEED_SESSION_ID>"}]},{"kind":"action","action":"templates.save","args":{"name":"lab-seed","root":"<LAB>","session_id":"<SEED_SESSION_ID>","panes":{"<SEED_BLOCK_ID>":"profile:lab-inert-missing"},"key":"seed-save-1"},"expect":[{"pointer":"/name","value":"lab-seed"}]},{"kind":"action","action":"templates.dismiss","args":{"run_id":"1","key":"seed-dismiss-1"},"expect":[{"pointer":"/run_id","value":"1"},{"pointer":"/status","value":"dismissed"}]},{"kind":"action","action":"templates.start","args":{"template":"lab-seed","root":"<LAB>","session_id":"<SEED_SESSION_ID>","key":"seed-start-1"},"expect":[{"pointer":"/run_id","value":"1"}]}]}`

// setLabFile is the set file of the test: Tunnels with no seed and a list probe; Messaging with one
// message to the seed agent itself, and action and sql probes, and an until item in the seed and the probes.
// Templates with the items of its kit entry (setLabTemplates).
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
	// An until item (coordinator ruling c49-kit-option-a): the history read repeats until it has
	// the seed message, in the seed and in every probe run.
	history := act("messaging.history", map[string]any{"thread": "<SELF>", "limit": 1}, val("/thread", "<SELF>"))
	history["until"] = []any{val("/messages/0/body", "lab seed one")}
	var tpl struct{ Seed, Probe []any }
	if err := json.Unmarshal([]byte(setLabTemplates), &tpl); err != nil {
		t.Fatal(err)
	}
	return setBytes(
		entry(0, []any{}, []any{act("tunnels.list", map[string]any{}, empty("/forwards"), empty("/pending"), empty("/cleanups"))}, map[string]any{}),
		entry(1, []any{act("messaging.send", send), history}, []any{
			history,
			act("messaging.history", map[string]any{"thread": "<SELF>", "limit": 1}, val("/messages/0/body", "lab seed one"), val("/thread", "<SELF>")),
			act("messaging.send", send, val("/key", "lab-seed-send-1")),
			map[string]any{"kind": "sql", "sql": "SELECT body, sender FROM messages ORDER BY seq", "rows": []any{[]any{"lab seed one", relSeedAgent}}},
		}, map[string]any{"clock": []any{"one"}, "messages": []any{"id"}}),
		entry(2, tpl.Seed, tpl.Probe, map[string]any{}),
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
	logp, err := labRehearse(t, bin, f, run, lab, nil, "-set", setPath)
	if err != nil {
		t.Fatalf("rehearse -set: %v (log %s; the lab %s is kept)", err, logp, lab)
	}
	// The until item ran through the pane loop: the tool logged its attempts.
	if n := strings.Count(string(mustReadFile(logp)), "(messaging.history): until attempt 1: exit 0"); n < 4 {
		t.Errorf("the log has %d until attempts of messaging.history, want the seed and 3 probe runs or more", n)
	}
	// The Templates seed waited for its blocked run, and the probes replayed the seed actions with
	// the seed placeholders (a replay with other args or a refused seed placeholder fails the run).
	if !strings.Contains(string(mustReadFile(logp)), "(templates.runs): until attempt 1: exit 0") {
		t.Errorf("the log has no until attempt of templates.runs")
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
	// The fault cases ran on the first plugin, both passed, and stay apart from the set steps.
	if len(rec.Faults) != 2 {
		t.Fatalf("record: %d fault runs", len(rec.Faults))
	}
	for i, want := range []string{"failed-forward", "interrupted-replace"} {
		f := rec.Faults[i]
		if f.Case != want || f.Plugin != setLabPlugins[0].name || f.Result != "pass" || len(f.Steps) == 0 {
			t.Errorf("record: fault %d %+v", i+1, f)
		}
	}
	if f := rec.Faults[0]; f.Base != setLabPlugins[0].to || f.Commit == setLabPlugins[0].to || !strings.Contains(strings.Join(f.Steps, "|"), "rollback exit 0") {
		t.Errorf("record: failed-forward %+v", f)
	}
	if f := rec.Faults[1]; f.Commit != setLabPlugins[0].to || !strings.Contains(strings.Join(f.Steps, "|"), "exit 3") || !strings.Contains(strings.Join(f.Steps, "|"), "verify (rerun) exit 0") {
		t.Errorf("record: interrupted-replace %+v", f)
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
	for _, want := range []string{"seed", "seeded", "probes on FROM", "tunnels fault failed-forward rollback", "tunnels fault failed-forward probes on FROM", "tunnels fault interrupted-replace rerun", "tunnels fault interrupted-replace rollback", "tunnels fault interrupted-replace probes on FROM", "tunnels backup", "tunnels replace", "tunnels verify", "tunnels rerun",
		"tunnels probes on TO", "messaging rerun", "messaging probes on TO", "chunking probe", "messaging rollback",
		"templates probes on TO", "templates rollback", "templates probes after the rollback",
		"messaging probes after the rollback", "tunnels rollback", "tunnels probes after the rollback"} {
		if !strings.Contains("|"+got+"|", "|"+want+"|") {
			t.Errorf("record: no check point %q in %s", want, got)
		}
	}
	if strings.Index(got, "templates rollback") > strings.Index(got, "messaging rollback") || strings.Index(got, "messaging rollback") > strings.Index(got, "tunnels rollback") {
		t.Errorf("record: the rollback is not newest first: %s", got)
	}
	if _, err := os.Stat(lab); err == nil {
		t.Errorf("the lab %s is still there after a pass", lab)
	}
	if t.Failed() {
		return
	}

	// Lab B: a second rehearsal of the same fixture in another lab dir, so other paths, its own
	// server, clones and mirrors. Its steps are the real freeze output of each plugin step. Each must
	// equal the step of the first record under the gate's own comparison (compareSetStep), while
	// the paths differ. The setup of each rehearsal already checks that the lab lock, lock without
	// cache dirs, base and hub candidate equal the live fixture.
	first := rec
	labB := lab + "-b"
	if out, err := labRehearse(t, bin, f, run, labB, nil, "-set", setPath); err != nil {
		t.Fatalf("lab B rehearse -set: %v (log %s; the lab %s is kept)", err, out, labB)
	}
	var second relRehearsal
	if err := json.Unmarshal(mustReadFile(filepath.Join(run, relRehearsalFile)), &second); err != nil {
		t.Fatal(err)
	}
	if !second.Pass || len(second.SetSteps) != len(first.SetSteps) {
		t.Fatalf("lab B record: pass %v, %d steps", second.Pass, len(second.SetSteps))
	}
	for i, s := range second.SetSteps {
		if err := compareSetStep(&s.Candidate, s.BasePins, first.SetSteps[i]); err != nil {
			t.Errorf("lab B step %d (%s): %v", i+1, s.Plugin, err)
		}
		if s.Candidate.SDKDir == first.SetSteps[i].Candidate.SDKDir || s.Candidate.SDKDir == "" {
			t.Errorf("lab B step %d: the SDK dirs %q and %q are not two lab paths", i+1, s.Candidate.SDKDir, first.SetSteps[i].Candidate.SDKDir)
		}
	}
}
