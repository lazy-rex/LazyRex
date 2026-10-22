package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Lab tests of expect_run (docs/plugin-sources.md, section 14). Lab only: HARNESS_LAB=1.

// expectEntry returns the hub.plugins entry of the plugin name.
func (s *startLab) expectEntry(name string) map[string]any {
	s.t.Helper()
	list, _ := s.must("hub.plugins", nil)["entries"].([]any)
	for _, x := range list {
		if e, _ := x.(map[string]any); e["name"] == name {
			return e
		}
	}
	s.t.Fatalf("no hub.plugins entry %s", name)
	return nil
}

// expect_run with local sources, on replace and on start:
//   - replace with another source: candidate_changed, the instance and its process unchanged, no
//     intent; with the candidate's source: a new instance;
//   - a stopped start with another source: candidate_changed, still desired stopped, no intent;
//     with the candidate's source: running;
//   - a start of a running plugin that runs another source: candidate_changed;
//   - a stopped start whose dependency is not registered: dependency_not_ready, still desired
//     stopped, no intent (it never waits: a later wake would start any candidate).
func TestExpectRunLocalLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.start()
	aRun, aStart := s.startRun("a")
	s.startRun("b")
	other := map[string]any{"source": "src/b"}
	mine := map[string]any{"source": "src/a"}
	intents := s.intents("a")

	_, err := s.call("hub.admin.replace", map[string]any{"plugin": "a", "expect_instance": aRun.InstanceID, "expect_run": other})
	s.refusedWith("replace, another source", err, "candidate_changed", "plugin a: the candidate is src/a, not src/b",
		"the running instance "+aRun.InstanceID+" is unchanged; nothing was retired or spawned")
	s.same("a", aRun, aStart)
	if s.intents("a") != intents {
		t.Error("the refused replace made an intent")
	}
	r, err := s.call("hub.admin.replace", map[string]any{"plugin": "a", "expect_instance": aRun.InstanceID, "expect_run": mine})
	if err != nil {
		t.Fatalf("replace with the expected run: %v", err)
	}
	id, _ := r["instance_id"].(string)
	s.registeredAs("a", id)

	_, err = s.call("hub.admin.start", map[string]any{"plugin": "a", "expect_run": other})
	s.refusedWith("start of a running plugin, another source", err, "candidate_changed", "plugin a runs src/a, not src/b; nothing was started")

	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	intents = s.intents("a")
	_, err = s.call("hub.admin.start", map[string]any{"plugin": "a", "expect_run": other})
	s.refusedWith("stopped start, another source", err, "candidate_changed", "plugin a: the candidate is src/a, not src/b; nothing was retired or spawned")
	if e := s.expectEntry("a"); e["desired"] != "stopped" || s.intents("a") != intents {
		t.Errorf("after the refused start: desired %v, %d intents (want stopped, %d)", e["desired"], s.intents("a"), intents)
	}
	s.must("hub.admin.start", map[string]any{"plugin": "a", "expect_run": mine})
	s.wait("a registers", func() bool { st := s.status("a"); return st.Registered && st.InstanceID != id })
	if e := s.expectEntry("a"); e["desired"] != "running" {
		t.Errorf("after the start: desired %v", e["desired"])
	}

	s.must("hub.admin.stop", map[string]any{"plugin": "b"})
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	bIntents := s.intents("b")
	_, err = s.call("hub.admin.start", map[string]any{"plugin": "b", "expect_run": map[string]any{"source": "src/b"}})
	s.refusedWith("stopped start, a dependency not registered", err, "dependency_not_ready", "plugin b: dependency a is not registered; nothing was retired or spawned")
	if e := s.expectEntry("b"); e["desired"] != "stopped" || e["state"] == "waiting" || s.intents("b") != bIntents {
		t.Errorf("after dependency_not_ready: %v, %d intents (want desired stopped, not waiting, %d)", e, s.intents("b"), bIntents)
	}
}

// The same commit with a different subdir is another run: the source string binds the subdir.
// The entry is github:lab/r/two@main; an expectation of github:lab/r/one@main at the same commit
// is candidate_changed, on replace and on a stopped start. The exact run (source, commit, digest)
// passes on both.
func TestExpectRunSubdirLab(t *testing.T) {
	s := newStartLab(t)
	gitTestEnv(t)
	r := newTestRepo(t)
	commit := r.commit(map[string]string{
		"one/lazyrex.json": `{"name":"gh","main":"main.lua"}`, "one/main.lua": fmt.Sprintf(startFixture, "gh"),
		"two/lazyrex.json": `{"name":"gh","main":"main.lua"}`, "two/main.lua": fmt.Sprintf(startFixture, "gh"),
	})
	const src, sibling = "github:lab/r/two@main", "github:lab/r/one@main"
	s.setConfig("plugins", []any{map[string]any{"source": src}})
	opts := pluginCmdOpts{remote: func(owner, repo string) string { return r.url() }}
	if err := pluginSync(context.Background(), s.cfg, opts); err != nil {
		t.Fatal(err)
	}
	lock, err := readLock(lockPath(s.cfg, ""))
	if err != nil {
		t.Fatal(err)
	}
	digest := lock.Plugins[src].Digest
	exact := map[string]any{"source": src, "commit": commit, "digest": digest}
	wrong := map[string]any{"source": sibling, "commit": commit, "digest": digest}

	s.start()
	run, start := s.startRun("gh")
	_, err = s.call("hub.admin.replace", map[string]any{"plugin": "gh", "expect_instance": run.InstanceID, "expect_run": wrong})
	s.refusedWith("replace, the other subdir", err, "candidate_changed", "the candidate is "+src+" at "+commit, "not "+sibling+" at "+commit)
	s.same("gh", run, start)
	res, err := s.call("hub.admin.replace", map[string]any{"plugin": "gh", "expect_instance": run.InstanceID, "expect_run": exact})
	if err != nil {
		t.Fatalf("replace with the exact run: %v", err)
	}
	id, _ := res["instance_id"].(string)
	s.registeredAs("gh", id)

	s.must("hub.admin.stop", map[string]any{"plugin": "gh"})
	_, err = s.call("hub.admin.start", map[string]any{"plugin": "gh", "expect_run": wrong})
	s.refusedWith("stopped start, the other subdir", err, "candidate_changed", "not "+sibling+" at "+commit)
	if e := s.expectEntry("gh"); e["desired"] != "stopped" {
		t.Errorf("after the refused start: desired %v", e["desired"])
	}
	s.must("hub.admin.start", map[string]any{"plugin": "gh", "expect_run": exact})
	s.wait("gh registers", func() bool { st := s.status("gh"); return st.Registered && st.InstanceID != id })

	// A run saved before the digest field (no "digest" in the run JSON) is not the exact run: a
	// start of the running plugin with the exact expectation is candidate_changed.
	db, err := openDB(filepath.Join(s.l.dir, "hub", "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE plugins SET run = json_remove(run, '$.digest') WHERE name = 'gh'`); err != nil {
		t.Fatal(err)
	}
	_, err = s.call("hub.admin.start", map[string]any{"plugin": "gh", "expect_run": exact})
	s.refusedWith("a running run without a digest", err, "candidate_changed", "plugin gh runs "+src+" at "+commit+" ()", "nothing was started")
}

// A guarded start admits once, at its final admission: a dependency that was registered at the
// resolution and stops before the final admission (the start is held after the resolution) is
// dependency_not_ready (a crashes, and its respawn is refused). b stays desired stopped, not
// waiting, with no intent. When a registers again, no wake starts b.
func TestExpectRunDependencyStopsLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.start()
	s.startRun("a")
	s.startRun("b")
	s.must("hub.admin.stop", map[string]any{"plugin": "b"})
	intents := s.intents("b")

	// The hub serves one request of this client at a time, so the hold window has no hub call: a
	// ends through its own client, and the test reads its pid. Its respawn is refused (a missing
	// dependency), so a is not registered at the final admission of b.
	a := s.status("a")
	aStart := startTime(a.ChildPID)
	s.must("hub.test.fault", map[string]any{"point": "start-admission", "mode": "hold-after", "key": "b", "plugin": "b", "hold_ms": 6000})
	done := make(chan error, 1)
	go func() {
		_, err := s.call("hub.admin.start", map[string]any{"plugin": "b", "expect_run": map[string]any{"source": "src/b"}})
		done <- err
	}()
	time.Sleep(time.Second) // the start of b reaches its hold
	s.writeSource("a", `{"name":"a","main":"main.lua","depends":["missing"]}`, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	s.c.perform(ctx, a.ClientID, "fix.exit", nil) // the reply may not arrive: the process ends
	cancel()
	for end := time.Now().Add(4 * time.Second); alive(a.ChildPID, aStart) && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(a.ChildPID, aStart) {
		t.Fatal("a did not end during the hold")
	}
	err := <-done
	inOrder(t, s.order(), "fault b", 1, "plugin_exited a", 1)
	s.refusedWith("a dependency that stopped before the final admission", err, "dependency_not_ready", "plugin b: dependency a is not registered; nothing was retired or spawned")
	if e := s.expectEntry("b"); e["desired"] != "stopped" || e["state"] == "waiting" || s.intents("b") != intents {
		t.Errorf("b after the refusal: %v, %d intents (want desired stopped, not waiting, %d)", e, s.intents("b"), intents)
	}

	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "")
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	s.registered("a")
	time.Sleep(2 * time.Second) // a wake, if any, runs now
	if e := s.expectEntry("b"); e["desired"] != "stopped" || s.intents("b") != intents {
		t.Errorf("b after a registered again: %v, %d intents (want desired stopped, %d)", e, s.intents("b"), intents)
	}
}
