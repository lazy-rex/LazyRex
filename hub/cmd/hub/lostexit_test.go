package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// procRex answers the hub's Rex calls: session.list_blocks with one block b1, and each
// com.superlogical.terminal.process read with the next answer (the last one repeats). An answer
// with a "code" is that Rex error.
func procRex(t *testing.T, h *Hub, answers ...map[string]any) {
	t.Helper()
	fr := newPerformRex(t, func(performCall) (any, string) { return map[string]any{}, "" })
	var mu sync.Mutex
	n := 0
	fr.mu.Lock()
	fr.other = func(method string, _ json.RawMessage) (any, string) {
		switch method {
		case "session.list_blocks":
			return map[string]any{"blocks": []any{map[string]any{"block_id": "b1"}}}, ""
		case "com.superlogical.terminal.process":
			mu.Lock()
			defer mu.Unlock()
			a := answers[min(n, len(answers)-1)]
			n++
			if code, ok := a["code"].(string); ok {
				return nil, code
			}
			return a, ""
		}
		return map[string]any{}, ""
	}
	fr.mu.Unlock()
	h.rex = fr.dial(t)
}

var (
	exitedRead = map[string]any{"last_exit": map[string]any{"exit_code": 1, "error": "exit status 1"}}
	neverRead  = map[string]any{"last_exit": nil}
	liveRead   = map[string]any{"child": map[string]any{"pid": 4242}, "last_exit": nil}
	failedRead = map[string]any{"code": "disconnected"}
)

// exitsOf waits until pl has n plugin_exited events and a moment longer, so that a late one counts.
func exitsOf(t *testing.T, events string, n int) int {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); len(eventsNamed(t, events, "plugin_exited")) < n && time.Now().Before(end); {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	return len(eventsNamed(t, events, "plugin_exited"))
}

func stateOf(h *Hub, pl *Plugin) (state, detail string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pl.respawn.last != nil {
		detail = pl.respawn.last.detail
	}
	return pl.reportedState(), detail
}

// The spawn path: the child exited before the record, so its child_exited was dropped. The read
// after the record takes the exit with Rex's status. A real child_exited after the record counts
// once: one plugin_exited and one crash timer.
func TestCheckMappedTakesAnExitBeforeTheRecord(t *testing.T) {
	h, clock, events, pl := oneHub(t)
	ctx := context.Background()
	procRex(t, h, exitedRead)
	running(h, pl, "p-1")
	pl.lifecycle.Lock()
	h.checkMapped(ctx, pl, "s", "block:p-1")
	pl.lifecycle.Unlock()
	if n := exitsOf(t, events, 1); n != 1 {
		t.Fatalf("%d plugin_exited events after the read, want 1", n)
	}
	if state, detail := stateOf(h, pl); state != "crashing" || detail != "exit status 1" {
		t.Fatalf("after the read: state %s, detail %q", state, detail)
	}
	exited(h, pl, ctx, `{"exit_code":1,"error":"exit status 1"}`)
	if n := exitsOf(t, events, 1); n != 1 || clock.count() != 1 {
		t.Fatalf("after a duplicate child_exited: %d plugin_exited, %d timers; want 1, 1", n, clock.count())
	}
}

// A live child, a failed read, and no child without a last exit are not an exit.
func TestCheckMappedOnlyAnExitCounts(t *testing.T) {
	for name, read := range map[string]map[string]any{"live": liveRead, "read error": failedRead, "no last exit": neverRead} {
		t.Run(name, func(t *testing.T) {
			h, _, events, pl := oneHub(t)
			procRex(t, h, read)
			running(h, pl, "p-1")
			h.checkMapped(context.Background(), pl, "s", "block:p-1")
			if n := exitsOf(t, events, 1); n != 0 {
				t.Fatalf("%d plugin_exited events", n)
			}
			if state, _ := stateOf(h, pl); state != "running" {
				t.Fatalf("state %s", state)
			}
		})
	}
}

// The adoption path: resolveIntents finds the instance's live child in b1, and the child exits
// before the record. The read after the record takes the exit.
func TestAdoptionTakesAnExitBeforeTheRecord(t *testing.T) {
	h, _, events, pl := oneHub(t)
	child := exec.Command("/bin/sh", "-c", "sleep 60; true", "instance_id=p-1") // no exec of sleep: the argv stays
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	procRex(t, h, map[string]any{"child": map[string]any{"pid": child.Process.Pid}}, exitedRead)
	mustExec(t, h, `INSERT INTO spawn_intents (instance_id, plugin, status, session_id) VALUES ('p-1', 'p', 'sending', 's')`)
	h.mu.Lock()
	pl.Desired = "running"
	h.mu.Unlock()
	pl.lifecycle.Lock()
	err := h.resolveIntents(context.Background(), pl)
	pl.lifecycle.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	inst, block := pl.InstanceID, pl.BlockID
	h.mu.Unlock()
	if inst != "p-1" || block != "b1" {
		t.Fatalf("not adopted: instance %s, block %s", inst, block)
	}
	if n := exitsOf(t, events, 1); n != 1 {
		t.Fatalf("%d plugin_exited events after the adoption, want 1", n)
	}
	if state, _ := stateOf(h, pl); state != "crashing" {
		t.Fatalf("after the adoption: state %s", state)
	}
}
