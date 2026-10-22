package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The op-call fault (docs/testing.md, "Faults"): its fields and bounds; a hold of the stop handler
// of the keyed plugin only, which fault_state reports as held and the event log as fault_held,
// until the release; one shot.
func TestOpCallFault(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	h.test = newTestState()
	events := filepath.Join(t.TempDir(), "events.jsonl")
	ev, err := openEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	h.ev = ev
	for _, bad := range []map[string]any{
		{"point": "op-call", "mode": "hold"},
		{"point": "op-call", "mode": "hold-after-sent", "hold_ms": float64(60001)},
		{"point": "op-call", "mode": "hold-after-sent", "nth": float64(2)},
		{"point": "op-call", "mode": "hold-after-sent", "when": "before"},
	} {
		if _, err := h.testFaultArm(bad, ""); err == nil {
			t.Errorf("%v: armed", bad)
		}
	}
	res, err := h.testFaultArm(map[string]any{"point": "op-call", "mode": "hold-after-sent", "key": "y", "hold_ms": float64(60000)}, "")
	if err != nil {
		t.Fatal(err)
	}
	id := float64(res.(map[string]any)["id"].(int))
	h.opCallFault("z") // another plugin: no hold
	if st, _ := h.testFaultState(id); st.(map[string]any)["fired"] != false {
		t.Fatalf("another plugin fired it: %v", st)
	}
	done := make(chan struct{})
	go func() {
		h.opCallFault("y")
		close(done)
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if st, _ := h.testFaultState(id); st.(map[string]any)["held"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fault is not held")
		}
	}
	select {
	case <-done:
		t.Fatal("the hold ended before the release")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := h.testFaultRelease(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the release did not end the hold")
	}
	if st, _ := h.testFaultState(id); st.(map[string]any)["held"] != false || st.(map[string]any)["fired"] != true {
		t.Fatalf("after the release: %v", st)
	}
	b, _ := os.ReadFile(events)
	if !strings.Contains(string(b), `"event":"fault_held"`) || !strings.Contains(string(b), `"plugin":"y"`) {
		t.Fatalf("no fault_held event: %s", b)
	}
	start := time.Now()
	h.opCallFault("y") // one shot: no second hold
	if time.Since(start) > time.Second {
		t.Fatal("the fault held twice")
	}
}
