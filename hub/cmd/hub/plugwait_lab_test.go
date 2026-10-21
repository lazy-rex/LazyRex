package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Lab tests of the waiting state and the wake rule (docs/plugin-sources.md, section 7): a plugin
// whose dependency has no accepted hub.register waits, and a register, a hub.admin.start or
// hub.admin.stop, or a refusal checks it again. A gated fixture (gate=1) registers only on
// fix.register, so a test decides when the register comes. Lab only: HARNESS_LAB=1.

// setArgs sets the args of the config entry src/<dir>.
func (s *startLab) setArgs(dir string, args map[string]string) {
	s.t.Helper()
	var c map[string]any
	b, _ := os.ReadFile(s.cfg)
	json.Unmarshal(b, &c)
	for _, p := range c["plugins"].([]any) {
		if e := p.(map[string]any); e["source"] == "src/"+dir {
			e["args"] = args
		}
	}
	s.setConfig("plugins", c["plugins"])
}

// release performs fix.register on the gated plugin name: its process registers now.
func (s *startLab) release(name string) {
	s.t.Helper()
	var id string
	s.wait(name+" has a client", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var err error
		id, err = s.c.resolveClient(ctx, "lazyrex-plugin:"+name)
		return err == nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.c.perform(ctx, id, "fix.register", nil); err != nil {
		s.t.Fatalf("fix.register %s: %v", name, err)
	}
}

// waits waits until hub.test.status gives name the state waiting with these dependency names.
func (s *startLab) waits(name string, deps ...string) {
	s.t.Helper()
	s.wait(name+" waits for "+strings.Join(deps, ", "), func() bool {
		var st struct {
			Plugins map[string]struct {
				State   string   `json:"state"`
				Waiting []string `json:"waiting"`
			} `json:"plugins"`
		}
		b, _ := json.Marshal(s.must("hub.test.status", nil))
		json.Unmarshal(b, &st)
		p := st.Plugins[name]
		return p.State == "waiting" && slices.Equal(p.Waiting, deps)
	})
}

// order returns the "event plugin" pairs of the current hub's event log lines, in emit order, and
// the names of the plugins_* events (they have no plugin).
func (s *startLab) order() []string {
	s.t.Helper()
	gen := s.must("hub.test.status", nil)["generation"]
	f, err := os.Open(filepath.Join(s.l.dir, "hub", "events.jsonl"))
	if err != nil {
		s.t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) == nil && fmt.Sprint(e["generation"]) == fmt.Sprint(gen) && e["plugin"] != nil {
			out = append(out, fmt.Sprintf("%v %v", e["event"], e["plugin"]))
		} else if ev, _ := e["event"].(string); fmt.Sprint(e["generation"]) == fmt.Sprint(gen) && strings.HasPrefix(ev, "plugins_") {
			out = append(out, ev)
		}
	}
	return out
}

// before checks that event x comes before event y in order.
func before(t *testing.T, order []string, x, y string) {
	t.Helper()
	i, j := slices.Index(order, x), slices.Index(order, y)
	if i < 0 || j < 0 || i > j {
		t.Errorf("%q (at %d) must come before %q (at %d): %v", x, i, y, j, order)
	}
}

// Hub start with a chain c -> b -> a: each dependent waits until its dependency registers, then
// spawns once. The register of a is gated, so the first waiting state does not depend on process
// timing. b is not gated: its register comes while the wake can still scan, and c must not stay
// waiting.
func TestPluginWaitHubStartChainLab(t *testing.T) {
	s := newStartLab(t, "a", "b", "c")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.writeSource("c", `{"name":"c","main":"main.lua","depends":["b"]}`, "c")
	s.setArgs("a", map[string]string{"gate": "1"})
	s.start()
	s.waits("b", "a")
	s.waits("c", "b")
	if e := s.entry("b"); e["state"] != "waiting" || fmt.Sprint(e["waiting"]) != "[a]" {
		t.Errorf("hub.plugins entry b: %v", e)
	}
	if n := len(s.events("plugin_spawn_intent", "b")); n != 0 {
		t.Errorf("b has %d spawn intents while it waits", n)
	}
	s.release("a")
	s.registered("a")
	s.registered("b")
	s.registered("c")
	time.Sleep(time.Second) // a late extra wake would spawn again now
	o := s.order()
	before(t, o, "plugin_waiting b", "plugin_registered a")
	before(t, o, "plugin_registered a", "plugin_spawn_intent b")
	before(t, o, "plugin_registered b", "plugin_spawn_intent c")
	for _, n := range []string{"a", "b", "c"} {
		if k := len(s.events("plugin_spawn_intent", n)); k != 1 {
			t.Errorf("%s: %d spawn intents; want 1", n, k)
		}
	}
	if n := len(s.events("plugin_waiting", "c")); n != 1 {
		t.Errorf("c: %d plugin_waiting events; want 1 (the names did not change)", n)
	}
	if e := s.entry("b"); e["state"] != "running" || e["waiting"] != nil {
		t.Errorf("hub.plugins entry b after the wake: %v", e)
	}
}

// hub.admin.start of a dependent whose dependency is stopped gives waiting. A stop of the waiting
// dependent ends the wait: the register of its dependency starts nothing. Then a dependency that
// starts after the dependent waits wakes it.
func TestPluginWaitLateRegisterLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.start()
	s.registered("b")
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	s.must("hub.admin.stop", map[string]any{"plugin": "b"})
	intents := len(s.events("plugin_spawn_intent", "b"))
	out := s.must("hub.admin.start", map[string]any{"plugin": "b"})
	if out["state"] != "waiting" || fmt.Sprint(out["waiting"]) != "[a]" {
		t.Errorf("hub.admin.start b: %v", out)
	}
	s.waits("b", "a")
	s.must("hub.admin.stop", map[string]any{"plugin": "b"})
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	s.registered("a")
	time.Sleep(1500 * time.Millisecond)
	if st := s.status("b"); st.State != "stopped" || st.Registered {
		t.Errorf("the stopped dependent: %+v", st)
	}
	if n := len(s.events("plugin_spawn_intent", "b")) - intents; n != 0 {
		t.Errorf("the stopped dependent has %d new spawn intents", n)
	}
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	s.must("hub.admin.start", map[string]any{"plugin": "b"})
	s.waits("b", "a")
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	s.registered("b")
	time.Sleep(time.Second)
	if n := len(s.events("plugin_spawn_intent", "b")) - intents; n != 1 {
		t.Errorf("b: %d new spawn intents; want 1", n)
	}
	before(t, s.order(), "plugin_registered a", "plugin_spawn_intent b")
}

// An unresolved spawn intent of the dependency gives it no registered instance: the dependent
// waits, and the intent stays the owner (the start of a is refused, and no intent of b is made).
// After the intent resolves, a starts and b spawns.
func TestPluginWaitUnresolvedIntentLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.start()
	s.registered("b")
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
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
	_, err = db.Exec(`INSERT INTO spawn_intents (instance_id, plugin, status, session_id) VALUES ('a-0-held', 'a', 'sending', ?)`, created.SessionID)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s.start()
	s.waits("b", "a")
	if _, err := s.call("hub.admin.start", map[string]any{"plugin": "a"}); err == nil || !strings.Contains(err.Error(), "unresolved_intent") {
		t.Fatalf("start of a with an open intent: %v", err)
	}
	s.waits("b", "a")
	for _, e := range s.order() {
		if e == "plugin_spawn_intent b" || e == "plugin_spawn_intent a" {
			t.Errorf("a spawn intent while the intent of a is open: %v", s.order())
			break
		}
	}
	if err := s.l.r.Call(ctx, "session.attach", map[string]any{"session_id": created.SessionID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.l.r.Call(ctx, "session.destroy", map[string]any{"session_id": created.SessionID}, nil); err != nil {
		t.Fatal(err)
	}
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	s.registered("b")
	before(t, s.order(), "plugin_registered a", "plugin_spawn_intent b")
}

// A waiting dependent whose dependency is refused becomes refused by the wake. The register of a
// is gated, so b still waits when a's start is refused.
func TestPluginWaitDependencyRefusedLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.setArgs("a", map[string]string{"gate": "1"})
	s.start()
	s.waits("b", "a")
	s.writeSource("a", `{"name":"a","main":"main.lua","depends":["missing"]}`, "")
	if _, err := s.call("hub.admin.start", map[string]any{"plugin": "a"}); err == nil || !strings.Contains(err.Error(), "dependency missing is not configured") {
		t.Fatalf("start of the broken a: %v", err)
	}
	want := "plugin b: dependency a is refused"
	s.wait("b is refused", func() bool {
		st := s.status("b")
		return st.State == "refused" && strings.Contains(st.Error, want)
	})
	if e := s.entry("b"); e["waiting"] != nil {
		t.Errorf("the refused entry b keeps waiting names: %v", e)
	}
	if n := len(s.events("plugin_spawn_intent", "b")); n != 0 {
		t.Errorf("b has %d spawn intents", n)
	}
}

// A dependency that stops, or crashes and is refused, after its dependent registered does not
// change the dependent's instance.
func TestPluginWaitDependencyLaterChangeLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.start()
	b, bStart := s.startRun("b")
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	time.Sleep(time.Second)
	same := func(when string) {
		t.Helper()
		st := s.status("b")
		if st.InstanceID != b.InstanceID || !st.Registered || st.State != "running" || !alive(b.ChildPID, bStart) {
			t.Errorf("b after %s: %+v, before %+v", when, st, b)
		}
	}
	same("the stop of a")
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	a, _ := s.startRun("a")
	s.writeSource("a", `{"name":"a","main":"main.lua","depends":["missing"]}`, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.c.perform(ctx, a.ClientID, "fix.exit", nil) // the reply can race the exit
	s.wait("a is refused", func() bool { return s.status("a").State == "refused" })
	time.Sleep(time.Second)
	same("the refusal of a")
}
