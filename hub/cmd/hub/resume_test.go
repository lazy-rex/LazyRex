package main

import (
	"os"
	"strings"
	"testing"
)

// A profile's resume template: ${conversation_id} is required, other tokens are declared args, each
// token is a whole argument and not the program, and only an agent profile has one.
func TestProfileResumeTemplate(t *testing.T) {
	path := t.TempDir() + "/profiles.json"
	h := &Hub{}
	h.cfg.Profiles = path
	os.WriteFile(path, []byte(`{"profiles":[{"id":"codex","name":"Codex","kind":"agent",
		"command":["codex","--no-daemon","-m","${model}"],"args":[{"name":"model","default":"gpt"}],
		"resume":["codex","--no-daemon","-m","${model}","resume","${conversation_id}"]}]}`), 0o600)
	byID, _, err := h.loadProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p := byID["codex"]
	got := strings.Join(resumeArgv(&spawnRow{Agent: "codex-3"}, p, map[string]string{"model": "sol"}, "conv-1"), " ")
	// argv[0] goes through LookPath, as for the command.
	if !strings.HasSuffix(got, "codex --no-daemon -m sol resume conv-1") || !strings.Contains(got, " LAZYREX_AGENT=codex-3 ") {
		t.Errorf("argv: %s", got)
	}
	// The resume launch keeps the spawn's environment cleanup.
	if !strings.Contains(got, "-u CLAUDE_CODE_CHILD_SESSION") {
		t.Errorf("no env cleanup: %s", got)
	}
	// A conversation_id from the resolved args cannot replace the identity's.
	got = strings.Join(resumeArgv(&spawnRow{}, p, map[string]string{"model": "sol", "conversation_id": "other"}, "conv-1"), " ")
	if !strings.HasSuffix(got, "resume conv-1") {
		t.Errorf("conversation_id override: %s", got)
	}
	for _, bad := range []string{
		`"resume":["codex","resume"]`,                       // no ${conversation_id}
		`"resume":["${conversation_id}"]`,                   // the program
		`"resume":["codex","--resume=${conversation_id}"]`,  // not a whole argument
		`"resume":["codex","${task}","${conversation_id}"]`, // not a declared arg
		`"resume":[]`, // empty
	} {
		os.WriteFile(path, []byte(`{"profiles":[{"id":"x","name":"x","kind":"agent","command":["x"],`+bad+`}]}`), 0o600)
		if _, _, err := h.loadProfiles(); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	os.WriteFile(path, []byte(`{"profiles":[{"id":"x","name":"x","kind":"accessory","command":["x"],"resume":["x","${conversation_id}"]}]}`), 0o600)
	if _, _, err := h.loadProfiles(); err == nil {
		t.Error("an accessory profile has a resume template")
	}
}
