package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// pruneTry takes the exclusive flock of prune without waiting, as `plugin prune` does. It reports
// whether the flock was free, and releases it.
func pruneTry(t *testing.T, state string) bool {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(state, "plugins", "prune.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatal(err)
	}
	return err == nil
}

// Each start owns its flock: two starts hold the prune lock together, the release of one leaves
// the other, and prune gets the lock only when both have released it. release can be called
// again, and on nil.
func TestStartLeases(t *testing.T) {
	h := &Hub{cfg: Config{State: t.TempDir()}}
	a, err := h.takeStartLease()
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.takeStartLease()
	if err != nil {
		t.Fatal(err)
	}
	if pruneTry(t, h.cfg.State) {
		t.Fatal("prune got the lock while two starts hold it")
	}
	a.release()
	a.release()
	if pruneTry(t, h.cfg.State) {
		t.Fatal("the release of one start released the other")
	}
	b.release()
	if !pruneTry(t, h.cfg.State) {
		t.Fatal("prune cannot get the lock after both releases")
	}
	var none *startLease
	none.release()
}

// A run that is not saved stops the spawn before any create request: the hub has no Rex client
// here, so a request would panic. The intent is abandoned, the record keeps its earlier values,
// and the lease stays with the caller.
func TestSpawnSaveFailureSendsNothing(t *testing.T) {
	h, _ := appModeHub(t)
	h.cfg.State = t.TempDir()
	h.ui.sess["session:home"] = uiSessionKey
	if _, err := h.db.Exec(`DROP TABLE plugins`); err != nil {
		t.Fatal(err)
	}
	lease, err := h.takeStartLease()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	pl := &Plugin{Name: "p", InstanceID: "p-0-old", run: pluginRun{Name: "p", Dir: "/old"}}
	err = h.spawn(context.Background(), pl, &pluginRun{Name: "p", Dir: "/new", Source: "s"}, lease)
	var se *spawnError
	if !errors.As(err, &se) || se.stage != "save" || !se.known || se.open != "" {
		t.Fatalf("a failed save: %#v", err)
	}
	var status string
	if err := h.db.QueryRow(`SELECT status FROM spawn_intents WHERE plugin = 'p'`).Scan(&status); err != nil || status != "abandoned" {
		t.Errorf("the intent: %q, %v", status, err)
	}
	if pl.InstanceID != "p-0-old" || pl.run.Dir != "/old" || pl.source != "" {
		t.Errorf("the record after a failed save: %+v", pl)
	}
	if pruneTry(t, h.cfg.State) {
		t.Error("spawn released the lease without a saved run")
	}
}

// The prune lock in the lab: a crash respawn of a is held after its admission (start-admission
// hold-after), and a crash respawn of b runs at the same time. Prune cannot get the lock while a
// is held, also after b has finished its start. It gets the lock after a has spawned. A replace
// releases its lock too. Lab only: HARNESS_LAB=1.
func TestPruneLockStartsLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua"}`, "b")
	s.start()
	s.registered("a")
	b, _ := s.startRun("b")
	state := filepath.Join(s.l.dir, "hub")

	s.must("hub.test.fault", map[string]any{"point": "start-admission", "mode": "hold-after", "key": "a", "plugin": "a", "hold_ms": 6000})
	s.exit("a")
	s.wait("the hold fires", func() bool { return len(s.events("fault", "a")) == 1 })
	if pruneTry(t, state) {
		t.Error("prune got the lock during an admitted start")
	}
	s.exit("b")
	s.wait("b starts again", func() bool { st := s.status("b"); return st.Registered && st.InstanceID != b.InstanceID })
	if len(s.events("plugin_spawn_intent", "a")) != 1 {
		t.Fatal("the held start of a ended before the check")
	}
	if pruneTry(t, state) {
		t.Error("the end of the start of b released the lock of a")
	}
	s.wait("a starts again", func() bool { return len(s.events("plugin_spawn_intent", "a")) == 2 })
	s.registered("a")
	if !pruneTry(t, state) {
		t.Error("prune cannot get the lock after the starts")
	}

	run := s.status("a")
	if _, err := s.replace("a", run.InstanceID); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !pruneTry(t, state) {
		t.Error("prune cannot get the lock after a replace")
	}
}
