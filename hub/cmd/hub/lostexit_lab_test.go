package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// exitFixture exits at once, before it registers.
const exitFixture = "rex.stop(1)\n"

// A plugin whose child exits before spawn records its block ID (plugin-create hold-map forces the
// window): the child_exited reaches blockEvent while no plugin has the block. After the hold, the
// exit still counts: plugin_exited once, the crash backoff, and never "running" with a dead block.
// The test releases the hold only after hub.log shows the receipt at the dispatch point.
func TestPluginExitBeforeMapLab(t *testing.T) {
	s := newStartLab(t, "c")
	s.writeSource("c", `{"name":"c","main":"main.lua"}`, "c")
	s.start()
	s.registered("c")
	s.must("hub.admin.stop", map[string]any{"plugin": "c"})
	if err := os.WriteFile(filepath.Join(s.src, "c", "main.lua"), []byte(exitFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	// hub.admin.start holds the request queue while the spawn is held, so the hold ends by hold_ms.
	s.must("hub.test.fault", map[string]any{"point": "plugin-create", "mode": "hold-map", "key": "c", "plugin": "c", "hold_ms": 5000})
	started := make(chan error, 1)
	go func() {
		_, err := s.call("hub.admin.start", map[string]any{"plugin": "c"})
		started <- err
	}()
	logPath := filepath.Join(s.l.dir, fmt.Sprintf("hub.%d.log", s.starts))
	hubLog := func() string { b, _ := os.ReadFile(logPath); return string(b) }
	held := regexp.MustCompile(`lab fault: hold-map of c holds block (\S+)`)
	var block string
	s.wait("the hold-map hold", func() bool {
		if m := held.FindStringSubmatch(hubLog()); m != nil {
			block = m[1]
		}
		return block != ""
	})
	receipt := "child_exited for block " + block + ": no plugin or worker has it"
	released := "lab fault: hold-map of c released"
	s.wait("the receipt of c's child_exited at blockEvent", func() bool { return strings.Contains(hubLog(), receipt) })
	if out := hubLog(); strings.Contains(out, released) && strings.Index(out, released) < strings.Index(out, receipt) {
		t.Fatal("no receipt before the release: the hold ended before c's child_exited reached blockEvent")
	}
	if err := <-started; err != nil {
		t.Fatalf("hub.admin.start c: %v", err)
	}
	exitedOnce := func() bool {
		n := 0
		for _, e := range s.events("plugin_exited", "c") {
			if e["block_id"] == block {
				n++
			}
		}
		return n == 1
	}
	for end := time.Now().Add(10 * time.Second); !exitedOnce(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("the child_exited during the map hold was dropped: c stays %v with a dead block %s", s.entry("c")["state"], block)
		}
	}
	if !strings.Contains(hubLog(), "plugin c: the child exited before the block was recorded") {
		t.Fatal("plugin_exited came, but not from the recorded exit: hub.log has no take line")
	}
	if n := len(s.events("plugin_respawn_scheduled", "c")); n < 1 {
		t.Fatalf("%d respawns scheduled after the exit, want at least 1", n)
	}
	if st := s.entry("c")["state"]; st == "running" {
		t.Fatalf("c is %v after its exit", st)
	}
}
