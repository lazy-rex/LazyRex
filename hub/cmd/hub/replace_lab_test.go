package main

import (
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Lab tests of hub.admin.replace (docs/plugin-sources.md, section 14). Lab only: HARNESS_LAB=1.

func (s *startLab) replace(name, expect string) (map[string]any, error) {
	return s.call("hub.admin.replace", map[string]any{"plugin": name, "expect_instance": expect})
}

// refusedWith checks that err is a refusal with this code whose text contains each part.
func (s *startLab) refusedWith(what string, err error, code string, parts ...string) {
	s.t.Helper()
	if err == nil || !strings.Contains(err.Error(), code+": ") {
		s.t.Errorf("%s: %v; want code %s", what, err, code)
		return
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			s.t.Errorf("%s: %v; want %q", what, err, p)
		}
	}
}

func (s *startLab) intents(name string) int {
	s.t.Helper()
	return len(s.events("plugin_spawn_intent", name))
}

// registeredAs waits until name registers with the instance id.
func (s *startLab) registeredAs(name, id string) {
	s.t.Helper()
	s.wait(name+" registers as "+id, func() bool { st := s.status(name); return st.Registered && st.InstanceID == id })
}

// A replace of a, with b running on a:
//   - hub.plugins reports the owned instance, desired and registered;
//   - a wrong expect_instance: instance_changed, nothing changes;
//   - the expected instance: a new instance from the candidate, the old process ends, b runs on;
//   - a replay of the same request: instance_changed, no new intent;
//   - a stop after the replace stops the new instance; a replace after a stop: stopped.
func TestReplaceLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.start()
	aRun, aStart := s.startRun("a")
	bRun, bStart := s.startRun("b")
	if e := s.entry("a"); e["instance_id"] != aRun.InstanceID || e["instance_pending"] != false || e["desired"] != "running" || e["registered"] != true {
		t.Errorf("hub.plugins a: %v", e)
	}

	intents := s.intents("a")
	_, err := s.replace("a", "a-0-wrong")
	s.refusedWith("a wrong expectation", err, "instance_changed", "plugin a runs instance "+aRun.InstanceID+", not a-0-wrong")
	s.same("a", aRun, aStart)

	r, err := s.replace("a", aRun.InstanceID)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	id, _ := r["instance_id"].(string)
	run, _ := r["run"].(map[string]any)
	if r["previous_instance"] != aRun.InstanceID || id == "" || id == aRun.InstanceID || run["source"] != "src/a" || run["dir"] != filepath.Join(s.src, "a") || run["commit"] != nil {
		t.Errorf("replace result: %v", r)
	}
	s.registeredAs("a", id)
	if alive(aRun.ChildPID, aStart) {
		t.Error("the old process of a still runs")
	}
	s.same("b", bRun, bStart)
	if st := s.status("b"); st.ClientID != bRun.ClientID {
		t.Errorf("b after the replace: %+v", st)
	}
	if n := len(s.events("plugin_replaced", "a")); n != 1 || s.intents("a") != intents+1 {
		t.Errorf("%d plugin_replaced, %d intents; want 1, %d", n, s.intents("a"), intents+1)
	}

	_, err = s.replace("a", aRun.InstanceID)
	s.refusedWith("the replay", err, "instance_changed", "runs instance "+id)
	if s.intents("a") != intents+1 {
		t.Errorf("the replay made an intent")
	}

	newRun, newStart := s.startRun("a")
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	s.wait("the new process of a ends", func() bool { return !alive(newRun.ChildPID, newStart) })
	_, err = s.replace("a", id)
	s.refusedWith("a replace after a stop", err, "stopped", "plugin a is stopped")
	if s.intents("a") != intents+1 {
		t.Errorf("the replace of a stopped plugin made an intent")
	}
}

// A refused candidate leaves the old instance as it is: its process, client and state, and its
// running dependent. A dependency that is not registered refuses the replace before the
// retirement.
func TestReplaceRefusedCandidateLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.start()
	aRun, aStart := s.startRun("a")
	bRun, bStart := s.startRun("b")
	intents := s.intents("a")

	s.writeSource("a", `{"name":"a","main":"main.lua","depends":["missing"]}`, "")
	_, err := s.replace("a", aRun.InstanceID)
	s.refusedWith("a refused candidate", err, "refused", "dependency missing is not configured", "the running instance "+aRun.InstanceID+" is unchanged")
	s.same("a", aRun, aStart)
	if st := s.status("a"); !st.Registered || st.ClientID != aRun.ClientID || st.State != "running" || st.Error != "" {
		t.Errorf("a after the refusal: %+v", st)
	}
	if e := s.entry("a"); e["state"] != "running" || e["registered"] != true {
		t.Errorf("hub.plugins a after the refusal: %v", e)
	}
	s.same("b", bRun, bStart)
	if st := s.status("b"); !st.Registered || st.ClientID != bRun.ClientID {
		t.Errorf("b after the refusal: %+v", st)
	}
	if s.intents("a") != intents {
		t.Error("the refusal made an intent")
	}

	// a stops; b runs on (a stop does not stop dependents), and its replace is refused.
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "")
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	bIntents := s.intents("b")
	_, err = s.replace("b", bRun.InstanceID)
	s.refusedWith("a dependency that is not registered", err, "dependency_not_ready", "dependency a is not registered", "the running instance "+bRun.InstanceID+" is unchanged")
	s.same("b", bRun, bStart)
	if s.intents("b") != bIntents {
		t.Error("dependency_not_ready made an intent")
	}
}

// The spawn outcomes after the retirement:
//   - known (plugin-create refuse): spawn_failed; the intent is abandoned, and a has no instance;
//   - unknown, with the block held (plugin-create hold-send): spawn_unknown with the open intent;
//     a replace while the intent is open is unresolved_intent and makes no intent; after the
//     release, a replace with the old expectation adopts the block and is instance_changed;
//   - unknown, with the block created (plugin-create lose-answer): the next replace adopts it.
func TestReplaceSpawnOutcomesLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	run, start := s.startRun("a")

	s.must("hub.test.fault", map[string]any{"point": "plugin-create", "mode": "refuse", "key": "a", "plugin": "a"})
	_, err := s.replace("a", run.InstanceID)
	s.refusedWith("a known spawn failure", err, "spawn_failed", "Rex refused the new block", "the old instance "+run.InstanceID+" is retired, and the plugin has no instance")
	if alive(run.ChildPID, start) {
		t.Error("the old process runs after spawn_failed")
	}
	if e := s.entry("a"); e["instance_id"] != "" || e["instance_pending"] != false || e["desired"] != "running" {
		t.Errorf("hub.plugins a after spawn_failed: %v", e)
	}
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	run, start = s.startRun("a")

	f := s.must("hub.test.fault", map[string]any{"point": "plugin-create", "mode": "hold-send", "key": "a", "plugin": "a"})
	_, err = s.replace("a", run.InstanceID)
	s.refusedWith("an unknown spawn outcome", err, "spawn_unknown", "the old instance "+run.InstanceID+" is retired", "may still create a block", "do not retry")
	e := s.entry("a")
	pending, _ := e["instance_id"].(string)
	if pending == "" || pending == run.InstanceID || e["instance_pending"] != true {
		t.Fatalf("hub.plugins a with the open intent: %v", e)
	}
	intents := s.intents("a")
	for _, expect := range []string{run.InstanceID, pending} {
		_, err = s.replace("a", expect)
		s.refusedWith("a replace with the open intent", err, "unresolved_intent", pending)
	}
	s.must("hub.test.fault_release", map[string]any{"id": f["id"]})
	s.wait("the held create is sent", func() bool { return len(s.events("fault_sent", "a")) == 1 })
	_, err = s.replace("a", run.InstanceID)
	s.refusedWith("the replay after the release", err, "instance_changed", "runs instance "+pending)
	if e := s.entry("a"); e["instance_id"] != pending || e["instance_pending"] != false {
		t.Errorf("hub.plugins a after the adoption: %v", e)
	}
	if s.intents("a") != intents {
		t.Errorf("%d intents; want %d", s.intents("a"), intents)
	}

	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	run, _ = s.startRun("a")
	s.must("hub.test.fault", map[string]any{"point": "plugin-create", "mode": "lose-answer", "key": "a", "plugin": "a"})
	_, err = s.replace("a", run.InstanceID)
	s.refusedWith("a lost answer", err, "spawn_unknown", "do not retry")
	lost, _ := s.entry("a")["instance_id"].(string)
	_, err = s.replace("a", run.InstanceID)
	s.refusedWith("the replay after a lost answer", err, "instance_changed", "runs instance "+lost)
	if e := s.entry("a"); e["instance_pending"] != false {
		t.Errorf("hub.plugins a after a lost answer: %v", e)
	}
}

// A retirement that the hub cannot verify: the row records another live process (a sleep that
// the test starts) as the child. The replace is retire_failed and spawns nothing; the real child runs on.
func TestReplaceRetireRefusalLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	run, start := s.startRun("a")
	db, err := openDB(filepath.Join(s.l.dir, "hub", "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other := exec.Command("sleep", "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { other.Process.Kill(); other.Wait() }()
	if _, err := db.Exec(`UPDATE plugins SET child_pid = ?, child_start = ? WHERE name = 'a'`, other.Process.Pid, startTime(other.Process.Pid)); err != nil {
		t.Fatal(err)
	}
	intents := s.intents("a")
	_, err = s.replace("a", run.InstanceID)
	s.refusedWith("an unverified child", err, "retire_failed", "instance "+run.InstanceID+" was not retired", "nothing was spawned")
	if !alive(run.ChildPID, start) || s.intents("a") != intents || len(s.events("plugin_conflict", "a")) == 0 {
		t.Errorf("after retire_failed: alive %v, %d intents (want %d), %d conflicts", alive(run.ChildPID, start), s.intents("a"), intents, len(s.events("plugin_conflict", "a")))
	}
	if _, err := db.Exec(`UPDATE plugins SET child_pid = ?, child_start = ? WHERE name = 'a'`, run.ChildPID, start); err != nil {
		t.Fatal(err)
	}
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	s.wait("a ends", func() bool { return !alive(run.ChildPID, start) })
}

// A replace and a reload that removes m. Rex delivers the hub's requests one at a time, so a reload
// request comes before or after the whole replace (reloadMu also serializes a replace with the
// recovery). Here it comes after: the replace completes with a new instance, then the reload
// removes m, and m is kept with the new instance. A replace of the kept record is refused.
func TestReplaceReloadRemovalLab(t *testing.T) {
	s := newStartLab(t)
	s.writeSource("m", `{"name":"m","main":"main.lua"}`, "m")
	s.writeManagedIDs("mid=m")
	s.start()
	run, _ := s.startRun("m")
	r, err := s.replace("m", run.InstanceID)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	id := r["instance_id"].(string)
	s.registeredAs("m", id)
	if rr := s.reload(); list(rr["removed"]) != "[mid]" {
		t.Errorf("reload: %v", rr)
	}
	st := s.status("m")
	start := startTime(st.ChildPID)
	if st.InstanceID != id || !alive(st.ChildPID, start) || s.kept("m") == nil {
		t.Errorf("the kept m: %+v, kept %v", st, s.kept("m"))
	}
	_, err = s.replace("m", id)
	s.refusedWith("a replace of a kept record", err, "refused", "has no config entry")
	s.stopKept("m", st.ChildPID, start)
}

// A known spawn failure whose intent the store keeps open: a trigger in hub.db refuses the status
// updates. The refusal and hub.plugins both report the open intent. A prepared intent does not stop
// a start; a sending intent does.
func TestReplaceOpenIntentLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	run, _ := s.startRun("a")
	db, err := openDB(filepath.Join(s.l.dir, "hub", "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	trigger := func(statuses string) {
		t.Helper()
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS refuse_status`); err != nil {
			t.Fatal(err)
		}
		if statuses == "" {
			return
		}
		if _, err := db.Exec(`CREATE TRIGGER refuse_status BEFORE UPDATE OF status ON spawn_intents
			WHEN NEW.status IN (` + statuses + `) BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
			t.Fatal(err)
		}
	}

	trigger("'sending', 'abandoned'")
	_, err = s.replace("a", run.InstanceID)
	s.refusedWith("a failed sending update", err, "spawn_failed", "the spawn intent was not marked as sending", "stays prepared",
		"the old instance "+run.InstanceID+" is retired", "hub.plugins shows the open intent as pending")
	if e := s.entry("a"); e["instance_id"] == "" || e["instance_pending"] != true {
		t.Errorf("hub.plugins a with a prepared intent: %v", e)
	}
	trigger("")
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	run, _ = s.startRun("a")

	trigger("'abandoned'")
	s.must("hub.test.fault", map[string]any{"point": "plugin-create", "mode": "refuse", "key": "a", "plugin": "a"})
	_, err = s.replace("a", run.InstanceID)
	s.refusedWith("a refusal that is not abandoned", err, "spawn_failed", "Rex refused the new block", "stays sending", "unresolved_intent")
	e := s.entry("a")
	pending, _ := e["instance_id"].(string)
	if pending == "" || e["instance_pending"] != true {
		t.Errorf("hub.plugins a with a sending intent: %v", e)
	}
	trigger("")
	_, err = s.call("hub.admin.start", map[string]any{"plugin": "a"})
	s.refusedWith("a start with the sending intent", err, "unresolved_intent", pending)
}

// A replace with an op attempt, where the intent resolution adopts the block of an earlier intent
// (plugin-create hold-send, then the release) and the replace then refuses: the attempt is bound
// and sent, it has no intent of its own, and its return proves not_applied for this attempt. The
// adopted instance stays.
func TestReplaceAttemptAdoptionLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	run, _ := s.startRun("a")
	f := s.must("hub.test.fault", map[string]any{"point": "plugin-create", "mode": "hold-send", "key": "a", "plugin": "a"})
	_, err := s.replace("a", run.InstanceID)
	s.refusedWith("an unknown spawn outcome", err, "spawn_unknown", "may still create a block")
	pending, _ := s.entry("a")["instance_id"].(string)
	s.must("hub.test.fault_release", map[string]any{"id": f["id"]})
	s.wait("the held create is sent", func() bool { return len(s.events("fault_sent", "a")) == 1 })

	db, err := sql.Open("sqlite", "file:"+filepath.Join(s.l.dir, "hub", "hub.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO plugin_op_attempts (attempt, op_id, call, at) VALUES ('r1', 'op', 'replace', 1)`); err != nil {
		t.Fatal(err)
	}
	_, err = s.call("hub.admin.replace", map[string]any{"plugin": "a", "expect_instance": run.InstanceID, "attempt": "r1"})
	s.refusedWith("the replace after the adoption", err, "instance_changed", "runs instance "+pending)
	if e := s.entry("a"); e["instance_id"] != pending || e["instance_pending"] != false {
		t.Errorf("hub.plugins a after the adoption: %v", e)
	}
	var bound, sent, intent string
	if err := db.QueryRow(`SELECT bound_at, sent_at, intent_id FROM plugin_op_attempts WHERE attempt = 'r1'`).Scan(&bound, &sent, &intent); err != nil {
		t.Fatal(err)
	}
	if bound == "" || sent == "" || intent != "" {
		t.Errorf("the attempt: bound %q, sent %q, intent %q", bound, sent, intent)
	}
	proof, perr := (&Hub{db: &DB{db}}).returnProof(context.Background(), "replace", "r1", refuse("instance_changed", "%v", err)) // in the op core the call is in-process: the typed refusal
	if perr != nil || !strings.HasPrefix(proof, "not_applied") {
		t.Errorf("the proof %q, %v", proof, perr)
	}
}
