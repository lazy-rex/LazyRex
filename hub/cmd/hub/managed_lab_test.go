package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lab tests of managed entries (docs/plugin-sources.md, section 13). Lab only: HARNESS_LAB=1.

// writeManagedFile writes the default managed file <state>/plugins.json of the lab hub.
func (s *startLab) writeManagedFile(body string) {
	s.t.Helper()
	d := filepath.Join(s.l.dir, "hub")
	os.MkdirAll(d, 0o700)
	if err := os.WriteFile(filepath.Join(d, "plugins.json"), []byte(body), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// Hub start with a managed entry m that depends on the config entry a: m starts after a registers,
// and hub.plugins reports it by its id. Then a malformed managed file and a hub restart: a runs,
// m does not, and hub.plugins reports the file error once.
func TestManagedHubStartLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("m", `{"name":"m","main":"main.lua","depends":["a"]}`, "m")
	s.writeManagedFile(fmt.Sprintf(`{"plugins":[{"id":"mid","source":%q}]}`, filepath.Join(s.src, "m")))
	s.start()
	s.registered("a")
	s.registered("m")
	before(t, s.order(), "plugin_registered a", "plugin_spawn_intent m")
	var managed map[string]any
	for _, e := range s.must("hub.plugins", nil)["entries"].([]any) {
		if e := e.(map[string]any); e["managed"] == "mid" {
			managed = e
		}
	}
	if managed == nil || managed["name"] != "m" || managed["state"] != "running" || managed["entry"] != nil {
		t.Errorf("the managed entry: %v", managed)
	}
	s.stop()
	s.writeManagedFile(`{"plugins":[{"id":"mid","source":"relative"}]}`)
	s.start()
	s.registered("a")
	refused := s.must("hub.plugins", nil)["refused"].([]any)
	if len(refused) != 1 || !strings.Contains(fmt.Sprint(refused[0]), `must be an absolute directory`) || refused[0].(map[string]any)["managed_file"] != filepath.Join(s.l.dir, "hub", "plugins.json") {
		t.Errorf("refused %v", refused)
	}
	if st := s.status("m"); st.Registered {
		t.Errorf("m runs with a malformed managed file: %+v", st)
	}
}

// writeManagedIDs writes the managed file with one entry for each "id=dir" pair (dir is a source
// dir under src).
func (s *startLab) writeManagedIDs(pairs ...string) {
	s.t.Helper()
	list := []any{}
	for _, p := range pairs {
		id, dir, _ := strings.Cut(p, "=")
		list = append(list, map[string]any{"id": id, "source": filepath.Join(s.src, dir)})
	}
	b, _ := json.Marshal(map[string]any{"plugins": list})
	s.writeManagedFile(string(b))
}

// reload writes the managed file with the pairs, and calls hub.plugins.reload as an operator.
func (s *startLab) reload(pairs ...string) map[string]any {
	s.t.Helper()
	s.writeManagedIDs(pairs...)
	return s.must("hub.plugins.reload", nil)
}

// managedEntry returns the hub.plugins entry of the managed id, or nil.
func (s *startLab) managedEntry(id string) map[string]any {
	s.t.Helper()
	b, _ := json.Marshal(s.must("hub.plugins", nil)["entries"])
	var list []map[string]any
	json.Unmarshal(b, &list)
	for _, e := range list {
		if e["managed"] == id {
			return e
		}
	}
	return nil
}

// fix calls a fixture action on the current client of the plugin name.
func (s *startLab) fix(name, action string) (map[string]any, error) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	raw, err := s.c.perform(ctx, s.status(name).ClientID, action, nil)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out, err
}

// exit ends the process of the plugin name with fix.exit, and waits until it ends.
func (s *startLab) exit(name string) {
	s.t.Helper()
	st := s.status(name)
	start := startTime(st.ChildPID)
	s.fix(name, "fix.exit") // the reply may not arrive: the process ends
	s.wait(name+" exits", func() bool { return !alive(st.ChildPID, start) })
}

// same checks that the plugin name still runs the instance and the process of was.
func (s *startLab) same(name string, was startStatus, start string) {
	s.t.Helper()
	st := s.status(name)
	if st.InstanceID != was.InstanceID || st.ChildPID != was.ChildPID || !alive(st.ChildPID, start) {
		s.t.Errorf("%s: instance %s pid %d; want instance %s pid %d, alive", name, st.InstanceID, st.ChildPID, was.InstanceID, was.ChildPID)
	}
}

func list(v any) string { return fmt.Sprint(v) }

// hub.plugins.reload (section 13) with one managed entry m that depends on the config entry a:
//   - add: m starts;
//   - remove: m is kept and retained, and its instance runs on registered; m cannot reload;
//   - re-add: the same instance runs on, ready again;
//   - remove, then a crash: no respawn; re-add: a new instance spawns;
//   - a malformed file: nothing changes but the reported error;
//   - a changed source: the instance runs on, hub.plugins reports the candidate dir, and a stop
//     and a start run the new source.
//
// The registered config plugin a calls the reload of the removal (an entry-backed caller).
func TestManagedReloadLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("m", `{"name":"m","main":"main.lua","depends":["a"]}`, "m")
	s.writeManagedIDs()
	s.start()
	s.registered("a")

	r := s.reload("mid=m")
	if list(r["added"]) != "[mid]" || list(r["removed"]) != "[]" || len(r["errors"].(map[string]any)) != 0 {
		t.Errorf("add: %v", r)
	}
	first, start := s.startRun("m")
	if e := s.managedEntry("mid"); e == nil || e["state"] != "running" || e["candidate"] != nil {
		t.Errorf("the added entry: %v", e)
	}

	// Remove, from the registered plugin a.
	s.writeManagedIDs()
	out, err := s.fix("a", "fix.reload")
	if err != nil || out["error"] != nil || list(out["result"].(map[string]any)["removed"]) != "[mid]" {
		t.Fatalf("a reloads: %v, %v", out, err)
	}
	s.same("m", first, start)
	if st := s.status("m"); !st.Registered || st.State != "retained" || !strings.Contains(st.Error, "no entry gives this name now") {
		t.Errorf("the removed m: %+v", st)
	}
	if k := s.kept("m"); k == nil || k["state"] != "retained" || s.managedEntry("mid") != nil {
		t.Errorf("kept m %v, entry %v", k, s.managedEntry("mid"))
	}
	if out, err := s.fix("m", "fix.reload"); err != nil || !strings.Contains(fmt.Sprint(out["error"]), "may not reload") {
		t.Errorf("the kept m reloads: %v, %v", out, err)
	}

	// Re-add: the same instance, ready again.
	r = s.reload("mid=m")
	if list(r["readded"]) != "[m]" || len(r["errors"].(map[string]any)) != 0 {
		t.Errorf("re-add: %v", r)
	}
	s.same("m", first, start)
	if st := s.status("m"); !st.Registered || st.State != "running" || st.Error != "" || s.kept("m") != nil {
		t.Errorf("the re-added m: %+v", st)
	}
	if n := len(s.events("plugin_spawn_intent", "m")); n != 1 {
		t.Errorf("%d spawn intents of m; want 1", n)
	}

	// A new id with the same name and source adopts the kept record: after a removal, and as one
	// id change in one reload. The instance runs on.
	s.reload()
	if r = s.reload("mid2=m"); list(r["readded"]) != "[m]" || list(r["added"]) != "[mid2]" {
		t.Errorf("re-add as mid2: %v", r)
	}
	s.same("m", first, start)
	if r = s.reload("mid=m"); list(r["readded"]) != "[m]" || list(r["removed"]) != "[mid2]" || list(r["added"]) != "[mid]" {
		t.Errorf("the id change mid2 to mid: %v", r)
	}
	s.same("m", first, start)
	if st := s.status("m"); !st.Registered || st.State != "running" || len(s.events("plugin_spawn_intent", "m")) != 1 {
		t.Errorf("m after the id changes: %+v", st)
	}

	// Remove, then a crash: no respawn. Re-add: a new instance.
	s.reload()
	s.exit("m")
	time.Sleep(time.Second) // a respawn would come now
	if st := s.status("m"); st.State != "refused" || len(s.events("plugin_spawn_intent", "m")) != 1 {
		t.Errorf("the crashed kept m: %+v, %d intents", st, len(s.events("plugin_spawn_intent", "m")))
	}
	r = s.reload("mid=m")
	if list(r["readded"]) != "[m]" || len(r["errors"].(map[string]any)) != 0 {
		t.Errorf("re-add after the crash: %v", r)
	}
	second, start := s.startRun("m")
	if second.InstanceID == first.InstanceID || second.ChildPID == first.ChildPID || s.kept("m") != nil {
		t.Errorf("re-add after the crash: %+v", second)
	}

	// A malformed file changes nothing but the reported error.
	s.writeManagedFile(`{"plugins":[{"id":"mid","source":"relative"}]}`)
	if _, err := s.call("hub.plugins.reload", nil); err == nil || !strings.Contains(err.Error(), "must be an absolute directory") || !strings.HasSuffix(err.Error(), "; the managed entries are unchanged") {
		t.Errorf("a malformed file: %v", err)
	}
	s.same("m", second, start)
	if e := s.managedEntry("mid"); e == nil || e["state"] != "running" {
		t.Errorf("the entry after a malformed file: %v", e)
	}
	if refused := s.must("hub.plugins", nil)["refused"].([]any); len(refused) != 1 || refused[0].(map[string]any)["managed_file"] == nil {
		t.Errorf("refused %v", refused)
	}

	// A changed source: the instance runs on; a stop and a start run the new source.
	s.writeSource("m2", `{"name":"m","main":"main.lua","depends":["a"]}`, "m")
	r = s.reload("mid=m2")
	if list(r["changed"]) != "[mid]" || len(r["errors"].(map[string]any)) != 0 {
		t.Errorf("change: %v", r)
	}
	s.same("m", second, start)
	e := s.managedEntry("mid")
	if e == nil || !s.snapshot(e["dir"]) || fmt.Sprint(e["candidate"]) != fmt.Sprint(map[string]any{"dir": filepath.Join(s.src, "m2")}) {
		t.Errorf("the changed entry: %v", e)
	}
	if refused := s.must("hub.plugins", nil)["refused"].([]any); len(refused) != 0 {
		t.Errorf("refused after a valid reload: %v", refused)
	}
	s.must("hub.admin.stop", map[string]any{"plugin": "m"})
	s.must("hub.admin.start", map[string]any{"plugin": "m"})
	s.registered("m")
	if e := s.managedEntry("mid"); !s.snapshot(e["dir"]) || e["candidate"] != nil {
		t.Errorf("the restarted entry: %v", e)
	}
	before(t, s.order(), "plugins_reloaded", "plugin_spawn_intent m")
}

// Dependents and a re-add whose check fails:
//   - b (managed, depends on the managed a) is removed, and its manifest gains a missing
//     dependency. The re-add reports the error; the instance runs on, retained. hub.admin.start b
//     is refused by the check (b does not wait).
//   - a is removed while b runs: b runs on. A crash of b: the respawn is refused (no entry gives
//     a). The re-add of a keeps its instance, and does not restart b. hub.admin.start b
//     spawns it.
func TestManagedReloadDependentsLab(t *testing.T) {
	s := newStartLab(t)
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.writeManagedIDs("ma=a", "mb=b")
	s.start()
	s.registered("a")
	bRun, bStart := s.startRun("b")

	s.reload("ma=a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a","missing"]}`, "")
	r := s.reload("ma=a", "mb=b")
	if list(r["readded"]) != "[b]" || !strings.Contains(fmt.Sprint(r["errors"].(map[string]any)["b"]), "dependency missing is not configured") {
		t.Errorf("the invalid re-add: %v", r)
	}
	s.same("b", bRun, bStart)
	if st := s.status("b"); st.State != "retained" || !strings.Contains(st.Error, "dependency missing is not configured") {
		t.Errorf("b after the invalid re-add: %+v", st)
	}
	if _, err := s.call("hub.admin.start", map[string]any{"plugin": "b"}); err == nil || !strings.Contains(err.Error(), "dependency missing is not configured") {
		t.Errorf("hub.admin.start b: %v", err)
	}
	if st := s.status("b"); st.State != "retained" {
		t.Errorf("b after hub.admin.start: %+v", st)
	}

	// Repair b, start it again, then remove a.
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "")
	s.must("hub.admin.stop", map[string]any{"plugin": "b"})
	s.must("hub.admin.start", map[string]any{"plugin": "b"})
	bRun, bStart = s.startRun("b")
	aRun, aStart := s.startRun("a")
	intents := len(s.events("plugin_spawn_intent", "b"))
	s.reload("mb=b")
	s.same("a", aRun, aStart)
	s.same("b", bRun, bStart)
	if st := s.status("a"); st.State != "retained" {
		t.Errorf("the removed a: %+v", st)
	}
	s.exit("b")
	s.wait("the respawn of b is refused", func() bool { return s.status("b").State == "refused" })
	if st := s.status("b"); !strings.HasPrefix(st.Error, "plugin b: dependency a of run sha256:") || !strings.HasSuffix(st.Error, " is not available") {
		t.Errorf("b after the crash: %+v", st)
	}
	r = s.reload("ma=a", "mb=b")
	if list(r["readded"]) != "[a]" || len(r["errors"].(map[string]any)) != 0 {
		t.Errorf("the re-add of a: %v", r)
	}
	s.same("a", aRun, aStart)
	time.Sleep(time.Second) // a restart of b would come now
	if st := s.status("b"); st.Registered || st.State != "refused" || len(s.events("plugin_spawn_intent", "b")) != intents {
		t.Errorf("b after the re-add of a: %+v", st)
	}
	s.must("hub.admin.start", map[string]any{"plugin": "b"})
	s.registered("b")
	if n := len(s.events("plugin_spawn_intent", "b")); n != intents+1 {
		t.Errorf("%d spawn intents of b; want %d", n, intents+1)
	}
}

// holdStart arms a start-admission hold of the plugin name that ends by itself after 4 s: the
// reload that waits on the held start holds the hub's request queue, so no release can arrive.
func (s *startLab) holdStart(name, mode string) {
	s.t.Helper()
	s.must("hub.test.fault", map[string]any{"point": "start-admission", "mode": mode, "key": name, "plugin": name, "hold_ms": 4000})
}

// A reload removes m while a respawn of m is held at its admission (section 13, the admission rule):
//   - before the admission: the start sees the removal and spawns nothing;
//   - after the admission: the admitted start completes. The new instance is kept and retained,
//     its register is refused, and its crash gives no respawn.
func TestManagedReloadAdmissionLab(t *testing.T) {
	s := newStartLab(t)
	s.writeSource("m", `{"name":"m","main":"main.lua"}`, "m")
	s.writeManagedIDs("mid=m")
	s.start()
	s.registered("m")

	s.holdStart("m", "hold-before")
	s.exit("m")
	s.wait("the hold fires", func() bool { return len(s.events("fault", "m")) == 1 })
	s.reload()
	time.Sleep(time.Second)
	if n := len(s.events("plugin_spawn_intent", "m")); n != 1 {
		t.Errorf("hold-before: %d spawn intents; want 1", n)
	}
	if st := s.status("m"); st.State != "refused" || st.Registered {
		t.Errorf("hold-before: %+v", st)
	}
	// The held start sees the removal: the hold fires, then the swap, then the kept refusal.
	inOrder(t, s.order(), "fault m", 1, "plugins_reloaded", 1, "plugin_refused m", 1)

	s.reload("mid=m")
	s.registered("m")
	s.holdStart("m", "hold-after")
	s.exit("m")
	s.wait("the hold fires", func() bool { return len(s.events("fault", "m")) == 2 })
	s.reload()
	if n := len(s.events("plugin_spawn_intent", "m")); n != 3 {
		t.Errorf("hold-after: %d spawn intents; want 3 (the admitted start completes)", n)
	}
	s.wait("the kept instance is retained", func() bool { return s.status("m").State == "retained" })
	st := s.status("m")
	start := startTime(st.ChildPID)
	time.Sleep(time.Second) // its register comes now
	if st := s.status("m"); st.Registered || st.State != "retained" {
		t.Errorf("hold-after: %+v", st)
	}
	// The start was admitted before the swap (the hold fires after the admission), and its spawn
	// completes after the swap.
	inOrder(t, s.order(), "fault m", 2, "plugins_reloaded", 3, "plugin_spawn_intent m", 3)
	s.stopKept("m", st.ChildPID, start)
}

// Hub start recovery and a reload are serialized (reloadMu): the recovery start of m is held after
// its admission, and a reload that removes m waits for the recovery to end. So the spawn comes
// before the removal, and m is kept and retained.
func TestManagedReloadAfterRecoveryLab(t *testing.T) {
	s := newStartLab(t)
	s.writeSource("m", `{"name":"m","main":"main.lua"}`, "m")
	s.writeManagedIDs("mid=m")
	s.setConfig("test_hold_recovery", true)
	s.start()
	s.holdStart("m", "hold-after")
	s.must("hub.test.recover", nil)
	s.wait("the hold fires", func() bool { return len(s.events("fault", "m")) == 1 })
	r := s.reload()
	if list(r["removed"]) != "[mid]" {
		t.Errorf("reload: %v", r)
	}
	o := s.order()
	before(t, o, "plugin_spawn_intent m", "plugins_reloaded")
	s.wait("m is retained", func() bool { return s.status("m").State == "retained" })
	st := s.status("m")
	s.stopKept("m", st.ChildPID, startTime(st.ChildPID))
}

// nth returns the index of the n-th (1-based) x in order, or -1.
func nth(order []string, x string, n int) int {
	for i, o := range order {
		if o == x {
			if n--; n == 0 {
				return i
			}
		}
	}
	return -1
}

// inOrder checks that each (event, n) pair comes after the one before it.
func inOrder(t *testing.T, order []string, pairs ...any) {
	t.Helper()
	last := -1
	for i := 0; i < len(pairs); i += 2 {
		x, n := pairs[i].(string), pairs[i+1].(int)
		j := nth(order, x, n)
		if j < 0 || j < last {
			t.Errorf("%s #%d at %d must come after index %d: %v", x, n, j, last, order)
			return
		}
		last = j
	}
}

// A reload removes m while an unresolved spawn intent of m is open (a create that may still
// complete in a live session): m is kept and keeps ownership. The reload reports the intent, a
// stop gives unresolved_intent and the record stays, and a hub restart keeps the kept record.
// When the intent's session closes, the stop drops it.
func TestManagedReloadUnresolvedIntentLab(t *testing.T) {
	s := newStartLab(t)
	s.writeSource("m", `{"name":"m","main":"main.lua"}`, "m")
	s.writeManagedIDs("mid=m")
	s.start()
	s.registered("m")
	s.must("hub.admin.stop", map[string]any{"plugin": "m"})
	s.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := s.l.r.Call(ctx, "session.create", map[string]any{"label": "intent-probe"}, &created); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(filepath.Join(s.l.dir, "hub", "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO spawn_intents (instance_id, plugin, status, session_id) VALUES ('m-0-held', 'm', 'sending', ?)`, created.SessionID)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s.start()
	r := s.reload()
	if list(r["removed"]) != "[mid]" || !strings.Contains(fmt.Sprint(r["errors"].(map[string]any)["m"]), "may still create a block") {
		t.Errorf("reload: %v", r)
	}
	if k := s.kept("m"); k == nil || k["state"] != "refused" {
		t.Errorf("kept: %v", k)
	}
	stopText := "unresolved_intent: plugin m: the stop is not complete: spawn m-0-held in session " + created.SessionID + " may still create a block."
	if _, err := s.call("hub.admin.stop", map[string]any{"plugin": "m"}); err == nil || !strings.Contains(err.Error(), stopText) {
		t.Fatalf("stop with an open intent: %v", err)
	}
	if !s.hasRecord("m") {
		t.Error("the record is dropped while its intent is open")
	}
	intents := len(s.events("plugin_spawn_intent", "m"))
	s.stop()
	s.start()
	s.wait("the kept m after the restart", func() bool { return s.kept("m") != nil })
	if k := s.kept("m"); k["state"] != "refused" || len(s.events("plugin_spawn_intent", "m")) != intents {
		t.Errorf("kept after the restart: %v", k)
	}
	if err := s.l.r.Call(ctx, "session.attach", map[string]any{"session_id": created.SessionID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.l.r.Call(ctx, "session.destroy", map[string]any{"session_id": created.SessionID}, nil); err != nil {
		t.Fatal(err)
	}
	s.must("hub.admin.stop", map[string]any{"plugin": "m"})
	if s.hasRecord("m") || s.kept("m") != nil {
		t.Error("the record stays after a complete stop")
	}
}
