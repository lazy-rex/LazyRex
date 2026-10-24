package main

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// TestSpawnArgvEnv runs the pane command with stale agent variables in its environment, as a Rex
// server started from an agent's pane has. Neither a shell nor an agent gets them. The lab check is
// tests/isolation (a worker of a real hub).
func TestSpawnArgvEnv(t *testing.T) {
	for _, k := range agentEnvUnset() {
		t.Setenv(k, "stale")
	}
	t.Setenv("LRT_KEPT", "kept")
	prof := Profile{ID: "envdump", Command: []string{"/bin/sh", "-c", "env"}, Env: map[string]string{"LRT_PROFILE": "p"}}
	run := func(s *spawnRow) map[string]string {
		argv := spawnArgv(s, prof, nil)
		out, err := exec.Command(argv[0], argv[1:]...).Output()
		if err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		env := map[string]string{}
		for _, l := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				env[k] = v
			}
		}
		return env
	}
	check := func(who string, env map[string]string) {
		for _, k := range agentEnvUnset() {
			if v, ok := env[k]; ok {
				t.Errorf("%s: %s = %q, want unset", who, k, v)
			}
		}
		if env["LRT_KEPT"] != "kept" || env["LRT_PROFILE"] != "p" {
			t.Errorf("%s: LRT_KEPT = %q, LRT_PROFILE = %q: other variables must pass", who, env["LRT_KEPT"], env["LRT_PROFILE"])
		}
	}
	check("a shell", run(&spawnRow{ID: "s1"}))
	check("an agent", run(&spawnRow{ID: "s2", Agent: "spawn:s2"}))
	// The command is the profile's own, with no argument added (for example no --session-id: the
	// agent's hook reports its conversation).
	argv := spawnArgv(&spawnRow{ID: "s3", Agent: "spawn:s3"}, prof, nil)
	if tail := argv[len(argv)-len(prof.Command):]; !reflect.DeepEqual(tail, prof.Command) || strings.Contains(strings.Join(argv, " "), "--session-id") {
		t.Errorf("argv %q: want the profile command %q at the end and no added argument", argv, prof.Command)
	}
}
