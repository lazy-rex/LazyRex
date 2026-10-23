package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v2Fixture is startFixture with one more action, fix.v2: a test sees which code a process runs.
// startFixture replies not_supported to fix.v2.
func v2Fixture(name string) string {
	f := fmt.Sprintf(startFixture, name)
	f = strings.Replace(f, `  {name = 'fix.exit',`, `  {name = 'fix.v2', kind = 'client', title = 'V2', args = {type = 'object'}},
  {name = 'fix.exit',`, 1)
	return strings.Replace(f, `  elseif ev.action == 'fix.register' then`, `  elseif ev.action == 'fix.v2' then
    rex.call('client.reply', {call_id = ev.call_id, result = {v = 2}})
    return
  elseif ev.action == 'fix.register' then`, 1)
}

// L1 (the P1 plan r6): a local plugin runs its snapshot. An edit of the source, then a crash: the
// respawn runs the same snapshot (the old code), and registers. A stop and a start publish a new
// snapshot of the edited source, and the new code runs.
func TestSelectedRunLocalCrashLab(t *testing.T) {
	f := v2Fixture("a")
	if !strings.Contains(f, "'fix.v2', kind") || !strings.Contains(f, "ev.action == 'fix.v2'") {
		t.Fatal("the v2 fixture has no fix.v2")
	}
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	first, _ := s.startRun("a")
	snap := s.entry("a")["dir"]
	if !s.snapshot(snap) {
		t.Fatalf("the run of a is not a local snapshot: %v", snap)
	}
	if _, err := s.fix("a", "fix.v2"); err == nil {
		t.Fatal("the first code answers fix.v2")
	}

	if err := os.WriteFile(filepath.Join(s.src, "a", "main.lua"), []byte(f), 0o600); err != nil {
		t.Fatal(err)
	}
	s.exit("a")
	s.wait("the respawn registers", func() bool { st := s.status("a"); return st.Registered && st.InstanceID != first.InstanceID })
	if e := s.entry("a"); e["dir"] != snap || e["state"] != "running" {
		t.Errorf("after the respawn: %v (want dir %v)", e, snap)
	}
	if _, err := s.fix("a", "fix.v2"); err == nil {
		t.Error("the respawn runs the edited source, not the snapshot")
	}
	if n := len(s.events("plugin_spawned", "a")); n != 2 {
		t.Errorf("%d spawns, want 2", n)
	}

	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	s.registered("a")
	if e := s.entry("a"); e["dir"] == snap || !s.snapshot(e["dir"]) {
		t.Errorf("after the start: %v (want a new snapshot, not %v)", e, snap)
	}
	if out, err := s.fix("a", "fix.v2"); err != nil || fmt.Sprint(out["v"]) != "2" {
		t.Errorf("the start does not run the edited source: %v, %v", out, err)
	}
}

// L2 (the P1 plan r6): a GitHub plugin runs commit A. A CLI update locks B. A crash: the respawn
// runs A, and hub.plugins shows commit A and pinned_commit B. A stop and a start run B.
func TestSelectedRunGitHubCrashLab(t *testing.T) {
	s := newStartLab(t)
	gitTestEnv(t)
	r := newTestRepo(t)
	a := r.commit(map[string]string{"lazyrex.json": `{"name":"gh","main":"main.lua"}`, "main.lua": cacheFixture, "name.lua": "return 'gh'\n"})
	const src = "github:lab/gh@main"
	s.setConfig("plugins", []any{map[string]any{"source": src}})
	opts := pluginCmdOpts{remote: func(owner, repo string) string { return r.url() }}
	if err := pluginSync(context.Background(), s.cfg, opts); err != nil {
		t.Fatal(err)
	}
	entry := func() map[string]any {
		t.Helper()
		b, _ := json.Marshal(s.must("hub.plugins", nil)["entries"])
		var list []map[string]any
		json.Unmarshal(b, &list)
		for _, e := range list {
			if e["source"] == src {
				return e
			}
		}
		t.Fatalf("no entry for %s: %s", src, b)
		return nil
	}

	s.start()
	first, _ := s.startRun("gh")
	e := entry()
	aDir := e["dir"]
	if e["commit"] != a || e["pinned_commit"] != a {
		t.Fatalf("the first run: %v", e)
	}

	b := r.commit(map[string]string{"name.lua": "return 'gh' -- b\n"})
	if err := pluginUpdate(context.Background(), s.cfg, nil, opts); err != nil {
		t.Fatal(err)
	}
	s.exit("gh")
	s.wait("the respawn registers", func() bool { st := s.status("gh"); return st.Registered && st.InstanceID != first.InstanceID })
	if e := entry(); e["commit"] != a || e["pinned_commit"] != b || e["dir"] != aDir || e["state"] != "running" {
		t.Errorf("after the crash: %v (want commit %s, pinned_commit %s, dir %v)", e, a, b, aDir)
	}

	s.must("hub.admin.stop", map[string]any{"plugin": "gh"})
	s.must("hub.admin.start", map[string]any{"plugin": "gh"})
	s.registered("gh")
	if e := entry(); e["commit"] != b || e["pinned_commit"] != b || e["dir"] == aDir {
		t.Errorf("after the start: %v (want commit %s)", e, b)
	}
}

// A local expect_run on a running local plugin (the lead's ruling lead-local-xrun): the start
// makes the snapshot of the current source and compares its digest with the running run's.
//   - An unchanged source: no restart.
//   - A plain start after a source edit: no restart (a plain start of a running plugin changes
//     nothing).
//   - A failed snapshot (a symlink in the source), and a candidate that does not read: refused;
//     the instance, its state and its run do not change.
//   - The edited source (the recovery of a crash loop): a new instance runs the new snapshot, and
//     a following crash respawn runs it too.
func TestSelectedRunLocalExpectRunLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	first, start := s.startRun("a")
	snap := s.entry("a")["dir"]
	x := map[string]any{"plugin": "a", "expect_run": map[string]any{"source": "src/a"}}
	unchanged := func(what string) {
		t.Helper()
		s.same("a", first, start)
		if e := s.entry("a"); e["dir"] != snap || e["state"] != "running" || e["registered"] != true {
			t.Errorf("%s: %v (want dir %v, running)", what, e, snap)
		}
		if n := len(s.events("plugin_spawned", "a")); n != 1 {
			t.Errorf("%s: %d spawns, want 1", what, n)
		}
	}

	s.must("hub.admin.start", x)
	unchanged("an unchanged source")

	if err := os.WriteFile(filepath.Join(s.src, "a", "main.lua"), []byte(v2Fixture("a")), 0o600); err != nil {
		t.Fatal(err)
	}
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	unchanged("a plain start")

	link := filepath.Join(s.src, "a", "link.lua")
	if err := os.Symlink("main.lua", link); err != nil {
		t.Fatal(err)
	}
	_, err := s.call("hub.admin.start", x)
	s.refusedWith("a failed snapshot", err, "refused", "link.lua is not a regular file", "the running instance "+first.InstanceID+" is unchanged")
	unchanged("a failed snapshot")
	os.Remove(link)

	manifest := filepath.Join(s.src, "a", "lazyrex.json")
	if err := os.WriteFile(manifest, []byte(`{"name":"a","main":"missing.lua"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = s.call("hub.admin.start", x)
	s.refusedWith("a candidate that does not read", err, "refused", "the running instance "+first.InstanceID+" is unchanged")
	if st := s.status("a"); st.State != "running" || st.Error != "" {
		t.Errorf("a candidate that does not read: %+v", st)
	}
	s.same("a", first, start)
	if err := os.WriteFile(manifest, []byte(`{"name":"a","main":"main.lua"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s.must("hub.admin.start", x)
	s.wait("a new instance registers", func() bool { st := s.status("a"); return st.Registered && st.InstanceID != first.InstanceID })
	fixed := s.entry("a")["dir"]
	if fixed == snap || !s.snapshot(fixed) {
		t.Errorf("the expect_run start runs %v, want a new snapshot (not %v)", fixed, snap)
	}
	if out, err := s.fix("a", "fix.v2"); err != nil || fmt.Sprint(out["v"]) != "2" {
		t.Errorf("the expect_run start does not run the edited source: %v, %v", out, err)
	}
	second := s.status("a")
	s.exit("a")
	s.wait("the respawn registers", func() bool { st := s.status("a"); return st.Registered && st.InstanceID != second.InstanceID })
	if e := s.entry("a"); e["dir"] != fixed {
		t.Errorf("the respawn runs %v, want %v", e["dir"], fixed)
	}
	if out, err := s.fix("a", "fix.v2"); err != nil || fmt.Sprint(out["v"]) != "2" {
		t.Errorf("the respawn does not run the fixed snapshot: %v, %v", out, err)
	}
}
