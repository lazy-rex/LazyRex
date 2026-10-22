package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cmdHub is a hub with the palette tables and one registered plugin "p" (instance i1). saved
// counts the hub.changed (plugins) calls that follow a successful save; timers collects the
// scheduled retries without running them.
func cmdHub(t *testing.T) (h *Hub, pl *Plugin, saved *atomic.Int32, timers *[]func()) {
	t.Helper()
	h, _ = workerTestHub(t)
	if _, err := h.db.Exec(formSchema); err != nil {
		t.Fatal(err)
	}
	pl = &Plugin{Name: "p", InstanceID: "i1", ClientID: "c1"}
	h.plugins = map[string]*Plugin{"p": pl}
	saved = &atomic.Int32{}
	oldSaved, oldAfter := commandsSaved, commandsAfter
	commandsSaved = func(*Hub) { saved.Add(1) }
	timers = &[]func(){}
	commandsAfter = func(d time.Duration, f func()) { *timers = append(*timers, f) }
	t.Cleanup(func() { commandsSaved, commandsAfter = oldSaved, oldAfter })
	return h, pl, saved, timers
}

// admit runs admitCommands under h.mu, as register does.
func admit(h *Hub, pl *Plugin, ids ...string) func() {
	cmds := []paletteCommand{}
	for _, id := range ids {
		cmds = append(cmds, paletteCommand{ID: id, Title: id, Route: "/forms/" + id})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.admitCommands(context.Background(), pl, cmds)
}

// stored is the plugin's stored command IDs, or nil when it has no row.
func stored(t *testing.T, h *Hub) []string {
	t.Helper()
	var raw string
	if err := h.db.QueryRow(`SELECT commands FROM ui_commands WHERE plugin = 'p'`).Scan(&raw); err != nil {
		return nil
	}
	var list []paletteCommand
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	return ids
}

func sameIDs(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",") && (a == nil) == (b == nil)
}

// The reviewer's schedule: A is admitted, then B; B's save runs, then A's save runs last. The
// stored set is B: a save writes the latest admitted set, not the one of its own registration.
func TestCommandSaveLatestAdmittedWins(t *testing.T) {
	h, pl, saved, _ := cmdHub(t)
	saveA := admit(h, pl, "agents")
	saveB := admit(h, pl, "agents", "launch")
	saveB()
	saveA()
	if got := stored(t, h); !sameIDs(got, []string{"agents", "launch"}) {
		t.Fatalf("stored %v, want the latest admitted set [agents launch]", got)
	}
	if saved.Load() != 2 {
		t.Fatalf("hub.changed after %d saves, want 2", saved.Load())
	}
}

// A first registration has no ui_commands row: no row is an empty stored set, not an error. An
// admitted empty set is stored as [], and a record with no registration stores nothing.
func TestCommandSaveFirstAndEmpty(t *testing.T) {
	h, pl, saved, _ := cmdHub(t)
	h.persistCommands(context.Background(), "p") // no registration yet
	if got := stored(t, h); got != nil || saved.Load() != 0 {
		t.Fatalf("no registration: stored %v, %d changed", got, saved.Load())
	}
	admit(h, pl, "agents")()
	if got := stored(t, h); !sameIDs(got, []string{"agents"}) {
		t.Fatalf("first registration: stored %v", got)
	}
	admit(h, pl)()
	if got := stored(t, h); got == nil || len(got) != 0 {
		t.Fatalf("empty set: stored %v, want []", got)
	}
	if saved.Load() != 2 {
		t.Fatalf("hub.changed %d times, want 2", saved.Load())
	}
}

// Only the current instance's registration is saved: a stopped instance and a replacement that
// has not registered save nothing; a registered replacement saves its own set.
func TestCommandSaveCurrentRegistrationOnly(t *testing.T) {
	h, pl, saved, _ := cmdHub(t)
	admit(h, pl, "agents")()
	save := admit(h, pl, "agents", "launch")
	pl.ClientID = "" // the instance stopped before its save ran
	save()
	if got := stored(t, h); !sameIDs(got, []string{"agents"}) {
		t.Fatalf("stopped instance: stored %v, want [agents]", got)
	}
	pl.InstanceID, pl.ClientID = "i2", "c1" // a replacement started; it has not registered
	h.persistCommands(context.Background(), "p")
	if got := stored(t, h); !sameIDs(got, []string{"agents"}) {
		t.Fatalf("unregistered replacement: stored %v, want [agents]", got)
	}
	pl.ClientID = "c2"
	admit(h, pl, "stop")() // the replacement registers
	if got := stored(t, h); !sameIDs(got, []string{"stop"}) {
		t.Fatalf("registered replacement: stored %v, want [stop]", got)
	}
	if saved.Load() != 2 {
		t.Fatalf("hub.changed %d times, want 2", saved.Load())
	}
}

// A failed save is reported (log, event commands_save_failed), sends no hub.changed, and keeps one
// retry pending, also after a second failed registration. The retry saves the latest set.
func TestCommandSaveFailureRetriesLatest(t *testing.T) {
	h, pl, saved, timers := cmdHub(t)
	admit(h, pl, "agents")()
	if _, err := h.db.Exec(`CREATE TRIGGER no_commands BEFORE UPDATE ON ui_commands BEGIN SELECT RAISE(FAIL, 'test write failure'); END`); err != nil {
		t.Fatal(err)
	}
	admit(h, pl, "agents", "launch")()
	admit(h, pl, "agents", "launch", "stop")()
	if got := stored(t, h); !sameIDs(got, []string{"agents"}) {
		t.Fatalf("during the failure: stored %v", got)
	}
	if saved.Load() != 1 {
		t.Fatalf("hub.changed %d times, want 1 (the first save only)", saved.Load())
	}
	if len(*timers) != 1 {
		t.Fatalf("%d retries scheduled, want 1 for the plugin", len(*timers))
	}
	b, _ := os.ReadFile(h.ev.f.Name())
	if n := strings.Count(string(b), `"commands_save_failed"`); n != 2 {
		t.Fatalf("%d commands_save_failed events, want 2", n)
	}
	if _, err := h.db.Exec(`DROP TRIGGER no_commands`); err != nil {
		t.Fatal(err)
	}
	(*timers)[0]()
	if got := stored(t, h); !sameIDs(got, []string{"agents", "launch", "stop"}) {
		t.Fatalf("after the retry: stored %v, want the latest set", got)
	}
	if saved.Load() != 2 || pl.commands.retry || pl.commands.delay != 0 {
		t.Fatalf("after the retry: %d changed, retry %v, delay %s", saved.Load(), pl.commands.retry, pl.commands.delay)
	}
}

// A read error other than no row is returned, not taken as an empty stored set.
func TestSaveCommandsReadError(t *testing.T) {
	h, _, _, _ := cmdHub(t)
	if _, err := h.db.Exec(`DROP TABLE ui_commands`); err != nil {
		t.Fatal(err)
	}
	if err := h.saveCommands(context.Background(), "p", nil); err == nil {
		t.Fatal("no error for a missing table")
	}
}

// A pending retry ends at the hub stop: after the run context ends, the retry saves nothing and
// schedules no other retry.
func TestCommandSaveRetryEndsAtStop(t *testing.T) {
	h, pl, saved, timers := cmdHub(t)
	admit(h, pl, "agents")()
	if _, err := h.db.Exec(`CREATE TRIGGER no_commands BEFORE UPDATE ON ui_commands BEGIN SELECT RAISE(FAIL, 'test write failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	save := h.admitCommands(ctx, pl, []paletteCommand{{ID: "launch", Title: "launch", Route: "/forms/launch"}})
	h.mu.Unlock()
	save()
	if len(*timers) != 1 {
		t.Fatalf("%d retries scheduled, want 1", len(*timers))
	}
	cancel() // the hub stops
	if _, err := h.db.Exec(`DROP TRIGGER no_commands`); err != nil {
		t.Fatal(err)
	}
	(*timers)[0]()
	if got := stored(t, h); !sameIDs(got, []string{"agents"}) || saved.Load() != 1 || len(*timers) != 1 {
		t.Fatalf("after the stop: stored %v, %d changed, %d retries", got, saved.Load(), len(*timers))
	}
}
