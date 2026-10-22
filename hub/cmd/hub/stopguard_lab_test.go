package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real lazyrex-hub stop against a real lab hub (row 3). The shutdown goes through the hub's
// Go Rex client, and the hub's answer keeps its error code.

// stopCmd runs the real stop of the lab hub's config from the test process (in no lab pane).
func (s *startLab) stopCmd() (int, string, time.Duration) {
	s.t.Helper()
	start := time.Now()
	out, err := exec.Command(s.bin, "stop", "-config", s.cfg).CombinedOutput()
	return exitCodeOf(err), string(out), time.Since(start)
}

// A stop from a process in an agent pane is refused by the admin guard, and the refused stop exits
// at once with the hub's text. The pane is an agent binding that the hub loads at its start: the
// process that holds it runs the stop.
func TestStopRefusedFromAnAgentPaneLab(t *testing.T) {
	s := newStartLab(t)
	var c map[string]any
	b, _ := os.ReadFile(s.cfg)
	json.Unmarshal(b, &c)
	agents, _ := c["agents"].([]any)
	c["agents"] = append(agents, map[string]any{"id": "guard", "name": "Guard", "status_record": ""})
	b, _ = json.MarshalIndent(c, "", " ")
	os.WriteFile(s.cfg, b, 0o600)
	state, _ := c["state"].(string)

	// The agent process: it waits for go, then runs the real stop and keeps its output.
	dir := t.TempDir()
	goFile, outFile, codeFile := filepath.Join(dir, "go"), filepath.Join(dir, "out"), filepath.Join(dir, "code")
	agent := exec.Command("/bin/sh", "-c", `while [ ! -f "$GO" ]; do sleep 0.05; done; "$BIN" stop -config "$CFG" >"$OUT" 2>&1; echo $? >"$CODE"; sleep 60`)
	agent.Env = append(os.Environ(), "GO="+goFile, "BIN="+s.bin, "CFG="+s.cfg, "OUT="+outFile, "CODE="+codeFile)
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Process.Kill(); agent.Wait() })

	// The first start makes the database; the binding goes in before the second start loads it.
	s.start()
	s.stop()
	db, err := openDB(filepath.Join(state, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO bindings (agent, session_id, block_id, child_pid, child_start) VALUES ('guard', '', 'block:guard-lab', ?, ?)`,
		agent.Process.Pid, startTime(agent.Process.Pid))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s.start()

	sent := time.Now()
	os.WriteFile(goFile, nil, 0o600)
	s.wait("the agent's stop ends", func() bool { _, err := os.Stat(codeFile); return err == nil })
	took := time.Since(sent)
	code, _ := os.ReadFile(codeFile)
	out, _ := os.ReadFile(outFile)
	t.Logf("the refused stop (exit %s, %s):\n%s", strings.TrimSpace(string(code)), took, out)
	if strings.TrimSpace(string(code)) != "1" || !strings.Contains(string(out), "refused: admin actions are refused from agent and plugin panes") ||
		strings.Contains(string(out), "may still apply") || strings.Contains(string(out), "stopped") {
		t.Errorf("the stop from the agent pane: exit %s\n%s", code, out)
	}
	if took > 3*time.Second {
		t.Errorf("the refused stop took %s, want an exit at once", took)
	}
	// The hub still runs; a stop from the test process stops it.
	s.must("hub.test.status", nil)
	if exit, out, _ := s.stopCmd(); exit != 0 || !strings.Contains(out, "stopped") {
		t.Errorf("the operator's stop: exit %d\n%s", exit, out)
	}
}

// A stop whose shutdown waits behind a held hub request queue waits for it: the request is
// delivered when the queue moves, the hub stops, and stop says stopped. It never exits early and
// never sends the request again.
func TestStopUnderAHeldQueueLab(t *testing.T) {
	s := newStartLab(t)
	s.writeSource("m", `{"name":"m","main":"main.lua"}`, "m")
	s.writeManagedIDs("mid=m")
	s.start()
	s.registered("m")

	// A reload that waits on a held start holds the hub's request queue for about 4 s.
	s.holdStart("m", "hold-before")
	s.exit("m")
	s.wait("the hold fires", func() bool { return len(s.events("fault", "m")) == 1 })
	s.writeManagedIDs()
	go s.call("hub.plugins.reload", nil) // its answer waits on the held start
	time.Sleep(300 * time.Millisecond)

	exit, out, took := s.stopCmd()
	t.Logf("the stop under a held queue (exit %d, %s):\n%s", exit, took, out)
	if exit != 0 || !strings.Contains(out, "stopped") {
		t.Errorf("the stop under a held queue: exit %d\n%s", exit, out)
	}
	if took < time.Second {
		t.Errorf("the stop took %s: the queue was not held", took)
	}
	// One send: the unit tests count it (performRex); the hub logs no action names.
}
