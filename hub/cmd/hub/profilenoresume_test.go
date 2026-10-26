package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const noResumeProfiles = `{"profiles":[
	{"id":"good","name":"Good","kind":"agent","command":["/bin/echo"],"resume":["/bin/echo","${conversation_id}"]},
	{"id":"bare","name":"Bare","kind":"agent","command":["/bin/echo"]},
	{"id":"tool","name":"Tool","kind":"accessory","command":["/bin/echo"]}]}`

// noResumeEvents lists the profile of each profile_without_resume event in the hub's event log.
func noResumeEvents(t *testing.T, h *Hub) []string {
	t.Helper()
	b, err := os.ReadFile(h.ev.f.Name())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e struct{ Event, Profile string }
		if json.Unmarshal([]byte(l), &e) == nil && e.Event == "profile_without_resume" {
			out = append(out, e.Profile)
		}
	}
	return out
}

// An agent profile without resume is reported at the start (the baseline check) and at each
// profile change (lead-restore1 item 1): one profile_without_resume event for each such profile.
// An accessory needs no resume. A check without a change reports nothing again.
func TestProfileWithoutResumeReported(t *testing.T) {
	h, _ := workerTestHub(t)
	h.cfg.Profiles = filepath.Join(t.TempDir(), "profiles.json")
	os.WriteFile(h.cfg.Profiles, []byte(noResumeProfiles), 0o600)
	h.checkProfiles()
	if got := noResumeEvents(t, h); strings.Join(got, ",") != "bare" {
		t.Fatalf("at the start: %v, want [bare]", got)
	}
	h.checkProfiles()
	if got := noResumeEvents(t, h); len(got) != 1 {
		t.Fatalf("a check without a change: %v", got)
	}
	// A change: bare gets a resume, and a new agent profile has none.
	fixed := strings.Replace(noResumeProfiles, `"id":"bare","name":"Bare","kind":"agent","command":["/bin/echo"]`,
		`"id":"bare","name":"Bare","kind":"agent","command":["/bin/echo"],"resume":["/bin/echo","${conversation_id}"]},{"id":"new","name":"New","kind":"agent","command":["/bin/echo"]`, 1)
	os.WriteFile(h.cfg.Profiles, []byte(fixed), 0o600)
	h.checkProfiles()
	if got := noResumeEvents(t, h); strings.Join(got, ",") != "bare,new" {
		t.Fatalf("after the change: %v, want [bare new]", got)
	}
}

// lazyrex-hub config warns about each agent profile without resume, on stderr; stdout is unchanged.
func TestConfigCmdWarnsProfileWithoutResume(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "profiles.json")
	os.WriteFile(prof, []byte(noResumeProfiles), 0o600)
	cfg := filepath.Join(dir, "hub.json")
	os.WriteFile(cfg, []byte(`{"server":"unix:///lab/rex.sock","state":"`+filepath.Join(dir, "state")+`","profiles":"`+prof+`"}`), 0o600)
	code, out, errOut := runConfigCmd(t, cfg)
	if code != 0 || !strings.HasPrefix(out, "server\t") || errOut != "warning: "+noResumeText("bare")+"\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// setup leaves an existing profiles file as it is and warns about each agent profile without
// resume; the profiles that setup makes (claude and codex) have resume.
func TestSetupProfilesResume(t *testing.T) {
	e, out := setupTestEnv(t, strings.Repeat("y\n", 12), true)
	dir := filepath.Join(e.home, ".lazyrex")
	if got := runSetup(e, dir, false); got != 0 {
		t.Fatalf("exit %d\n%s", got, out)
	}
	var made struct{ Profiles []Profile }
	json.Unmarshal([]byte(readFile(filepath.Join(dir, "profiles.json"))), &made)
	kinds := []string{}
	for _, p := range made.Profiles {
		kinds = append(kinds, p.ID)
		if len(p.Resume) == 0 {
			t.Errorf("setup made profile %s without resume", p.ID)
		}
	}
	if strings.Join(kinds, ",") != "claude,codex" {
		t.Fatalf("profiles %v", kinds)
	}

	e, out = setupTestEnv(t, strings.Repeat("y\n", 12), true)
	dir = filepath.Join(e.home, ".lazyrex")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "profiles.json"), []byte(noResumeProfiles), 0o600)
	if got := runSetup(e, dir, false); got != 0 {
		t.Fatalf("exit %d\n%s", got, out)
	}
	if !strings.Contains(out.String(), "Warning: "+noResumeText("bare")+".") || strings.Contains(out.String(), "profile good") ||
		readFile(filepath.Join(dir, "profiles.json")) != noResumeProfiles {
		t.Fatalf("an existing profiles file:\n%s", out)
	}
}

// setup checks the profiles file that the hub reads: the "profiles" of an existing hub.json (codex-28:
// another file than DIR/profiles.json). It names that file in the warning and leaves it unchanged;
// DIR/profiles.json is still made as usual. A relative "profiles" is not checked, and setup says so.
func TestSetupWarnsForConfiguredProfiles(t *testing.T) {
	e, out := setupTestEnv(t, strings.Repeat("y\n", 12), true)
	dir := filepath.Join(e.home, ".lazyrex")
	active := filepath.Join(e.home, "active-profiles.json")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(active, []byte(noResumeProfiles), 0o600)
	os.WriteFile(filepath.Join(dir, "hub.json"), []byte(`{"server":"`+setupLabServer+`","state":`+strconv.Quote(filepath.Join(dir, "state"))+`,"profiles":`+strconv.Quote(active)+`}`), 0o600)
	if got := runSetup(e, dir, false); got != 0 {
		t.Fatalf("exit %d\n%s", got, out)
	}
	if !strings.Contains(out.String(), "Warning: "+active+": "+noResumeText("bare")+".") || readFile(active) != noResumeProfiles ||
		!strings.Contains(readFile(filepath.Join(dir, "profiles.json")), `"resume"`) {
		t.Fatalf("a configured profiles file:\n%s", out)
	}

	e, out = setupTestEnv(t, strings.Repeat("y\n", 12), true)
	dir = filepath.Join(e.home, ".lazyrex")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "hub.json"), []byte(`{"server":"`+setupLabServer+`","state":`+strconv.Quote(filepath.Join(dir, "state"))+`,"profiles":"rel/profiles.json"}`), 0o600)
	if got := runSetup(e, dir, false); got != 0 || !strings.Contains(out.String(), "hub.json names the relative profiles file rel/profiles.json: setup does not check it.") {
		t.Fatalf("a relative profiles file: exit %d\n%s", got, out)
	}
}
