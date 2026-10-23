package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lab tests of the one start path (docs/plugin-sources.md, section 7): the start check at each
// attempt, the refused and retained states, the repair entry point and unresolved spawn intents.
// Each test starts its own lab server and a -test hub with plugins from temporary source
// directories. Lab only: HARNESS_LAB=1.

// startFixture is a plugin that registers, and ends its process with status 1 on fix.exit. With
// the entry arg gate=1 it sets its label first and registers only on fix.register: a test decides
// when the register comes. fix.reload calls hub.plugins.reload as the plugin and replies with
// {result} or {error}.
const startFixture = `local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local name = %q
local function register()
  local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.register', args = {plugin = name, instance_id = instance_id, exports = {}}, timeout_ms = 10000})
  if err then print('register: ' .. tostring(err)) end
end
rex.call('client.keymap', {actions = {
  {name = 'fix.exit', kind = 'client', title = 'Exit', args = {type = 'object'}},
  {name = 'fix.register', kind = 'client', title = 'Register', args = {type = 'object'}},
  {name = 'fix.reload', kind = 'client', title = 'Reload', args = {type = 'object'}}}})
rex.on('client_perform_requested', function(ctx, ev)
  if ev.action == 'fix.exit' then
    rex.call('client.reply', {call_id = ev.call_id, result = {}})
    rex.stop(1)
  elseif ev.action == 'fix.register' then
    rex.call('client.reply', {call_id = ev.call_id, result = {}})
    rex.after(0, register)
    return
  elseif ev.action == 'fix.reload' then
    local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.plugins.reload', args = {}, timeout_ms = 20000})
    rex.call('client.reply', {call_id = ev.call_id, result = {result = res and res.result, error = err and (type(err) == 'table' and tostring(err.message) or tostring(err)) or nil}})
    return
  end
  rex.call('client.reply', {call_id = ev.call_id, error = {code = 'not_supported', message = 'no such action'}})
end)
if tostring(rex.args.gate) == '1' then
  rex.call('client.set_label', {label = 'lazyrex-plugin:' .. name})
  return
end
register()
rex.call('client.set_label', {label = 'lazyrex-plugin:' .. name})
`

type startLab struct {
	t        *testing.T
	l        *lab
	o        *testOpts
	bin, cfg string
	src      string // the source directories
	c        *helperConn
	hub      string // the hub client
	starts   int
	args     []string // extra hub flags
}

// writeSource writes the source dir src/<dir> with a manifest and the fixture as main.lua.
func (s *startLab) writeSource(dir, manifest, name string) {
	s.t.Helper()
	d := filepath.Join(s.src, dir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		s.t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(d, "lazyrex.json"), []byte(manifest), 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
	if name != "" {
		if err := os.WriteFile(filepath.Join(d, "main.lua"), []byte(fmt.Sprintf(startFixture, name)), 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
}

// labEnder is the part of a test that endGoLab uses.
type labEnder interface {
	Name() string
	Failed() bool
	Error(args ...any)
	Logf(format string, args ...any)
}

// endGoLab ends the lab dir of a Go lab test after its stop, with endLab: an unproved cleanup is kind
// unproved whatever t.Failed() says, a failed test is kind failed, and a passed test removes the lab.
func endGoLab(t labEnder, dir string, unproved []string) {
	kept, probs := endLab(dir, false, t.Failed(), "go test "+t.Name(), "the test failed", unproved)
	for _, p := range probs {
		t.Error(p)
	}
	if kept {
		t.Logf("lab kept: %s (remove it after the review: lazyrex-hub test -clean %s)", dir, dir)
	}
}

// newStartLab starts a lab and writes a hub config whose plugins are the given source dirs, in
// order, relative to the config. It does not start the hub.
func newStartLab(t *testing.T, sources ...string) *startLab {
	return newServerLab(t, nil, sources...)
}

// newServerLab is newStartLab with more lab server options (serverArgs).
func newServerLab(t *testing.T, serverArgs []string, sources ...string) *startLab {
	bin, root := harnessBinary(t)
	rexBin, err := exec.LookPath("rex")
	if err != nil {
		t.Skip("no rex CLI")
	}
	dir := filepath.Join(root, "tests", "harness")
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Test.Agents, m.Test.Args, m.Test.WatchRoots, m.Test.Env = nil, nil, nil, nil
	o := &testOpts{dir: dir, lazyrex: root, self: bin, rexBin: rexBin, m: m, serverArgs: serverArgs}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l, err := o.startLab(ctx)
	s := &startLab{t: t, l: l, o: o, bin: bin, src: filepath.Join(l.dir, "src")}
	t.Cleanup(func() {
		for _, p := range l.stop() {
			t.Error(p)
		}
		endGoLab(t, l.dir, l.unproved)
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := o.hubConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	b, _ := os.ReadFile(cfg)
	json.Unmarshal(b, &c)
	plugins := []any{}
	for _, src := range sources {
		plugins = append(plugins, map[string]any{"source": "src/" + src})
	}
	c["plugins"] = plugins
	b, _ = json.MarshalIndent(c, "", " ")
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg = cfg
	s.c = &helperConn{r: l.r}
	return s
}

// start starts the hub and waits for its client. It does not wait for the plugins.
func (s *startLab) start() {
	s.t.Helper()
	s.starts++
	logf, err := os.Create(filepath.Join(s.l.dir, fmt.Sprintf("hub.%d.log", s.starts)))
	if err != nil {
		s.t.Fatal(err)
	}
	defer logf.Close() // the started keeper has its own copy
	if s.l.hub, err = startKept(s.bin, "hub", append([]string{s.bin, "-config", s.cfg, "-test"}, s.args...), s.l.env, s.l.home(), logf, logf); err != nil {
		s.t.Fatal(err)
	}
	s.l.trackKept(s.l.hub)
	s.wait("the hub client", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		id, err := s.c.resolveClient(ctx, "lazyrex-hub")
		if err != nil {
			return false
		}
		s.hub = id
		_, err = s.c.perform(ctx, id, "hub.test.status", nil)
		return err == nil
	})
}

// stop stops the hub through its keeper (SIGTERM to its group) and waits until the old hub has
// released hub.lock (stopHubForRestart). Its plugin processes stay in their blocks.
func (s *startLab) stop() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := stopHubForRestart(ctx, s.l, "term"); err != nil {
		s.t.Fatalf("the hub did not stop cleanly: %v", err)
	}
}

func (s *startLab) wait(what string, ok func() bool) {
	s.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(150 * time.Millisecond) {
		if time.Now().After(deadline) {
			s.t.Fatalf("timeout: %s", what)
		}
	}
}

// call performs a hub action as an operator (the test process is in no lab pane).
func (s *startLab) call(action string, args any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := s.c.perform(ctx, s.hub, action, args)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out, err
}

func (s *startLab) must(action string, args any) map[string]any {
	s.t.Helper()
	out, err := s.call(action, args)
	if err != nil {
		s.t.Fatalf("%s: %v", action, err)
	}
	return out
}

type startStatus struct {
	Registered bool   `json:"registered"`
	ClientID   string `json:"client_id"`
	State      string `json:"state"`
	Error      string `json:"error"`
	InstanceID string `json:"instance_id"`
	BlockID    string `json:"block_id"`
	ChildPID   int    `json:"child_pid"`
}

func (s *startLab) status(name string) startStatus {
	s.t.Helper()
	var st struct {
		Plugins map[string]startStatus `json:"plugins"`
	}
	b, _ := json.Marshal(s.must("hub.test.status", nil))
	json.Unmarshal(b, &st)
	return st.Plugins[name]
}

// entry returns the hub.plugins entry of the source dir src/<dir>.
func (s *startLab) entry(dir string) map[string]any {
	s.t.Helper()
	b, _ := json.Marshal(s.must("hub.plugins", nil)["entries"])
	var list []map[string]any
	json.Unmarshal(b, &list)
	for _, e := range list {
		if e["source"] == "src/"+dir {
			return e
		}
	}
	return nil
}

func (s *startLab) events(name, plugin string) []map[string]any {
	s.t.Helper()
	b, _ := json.Marshal(s.must("hub.test.events", map[string]any{"event": name})["events"])
	var list, out []map[string]any
	json.Unmarshal(b, &list)
	for _, e := range list {
		if plugin == "" || e["plugin"] == plugin {
			out = append(out, e)
		}
	}
	return out
}

func (s *startLab) registered(name string) {
	s.t.Helper()
	s.wait(name+" registers", func() bool { return s.status(name).Registered })
}

// snapshot reports whether d (a hub.plugins dir) is a published local snapshot,
// <state>/plugins/local/<hex>.<token>: the run dir of a local source.
func (s *startLab) snapshot(d any) bool {
	p, _ := d.(string)
	return p != "" && filepath.Dir(p) == filepath.Join(s.state(), "plugins", "local")
}

// state gives the hub's canonical state directory (absolute, symlinks resolved): the path that
// the hub uses and reports.
func (s *startLab) state() string {
	s.t.Helper()
	var c Config
	b, _ := os.ReadFile(s.cfg)
	json.Unmarshal(b, &c)
	state, err := canonicalState(c.State)
	if err != nil {
		s.t.Fatal(err)
	}
	return state
}

// spoilRun changes main.lua in the published snapshot of the run of the plugin in src/<dir>, so
// the snapshot no longer has the run's digest: an automatic start refuses the run. It gives the
// refusal text of that start.
func (s *startLab) spoilRun(dir string) string {
	s.t.Helper()
	snap, _ := s.entry(dir)["dir"].(string)
	if !s.snapshot(snap) {
		s.t.Fatalf("the run of %s is not a local snapshot: %q", dir, snap)
	}
	digest, err := dirDigest(snap)
	if err != nil {
		s.t.Fatal(err)
	}
	main := filepath.Join(snap, "main.lua")
	if err := os.Chmod(snap, 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.Chmod(main, 0o600); err != nil {
		s.t.Fatal(err)
	}
	f, err := os.OpenFile(main, os.O_WRONLY|os.O_APPEND, 0)
	if err == nil {
		_, err = f.WriteString("\n-- spoiled\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return "plugin " + dir + ": run " + digest + " is no longer available; start it again"
}

// A plugin crashes after its selected snapshot changed: the respawn is refused, and the old block
// stays (it is not retired). hub.admin.start, an explicit start, publishes a new snapshot of the
// source: a new instance in a new block replaces it.
func TestPluginCrashRefusedKeepsBlockLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	s.registered("a")
	first := s.status("a")
	spoiled := s.entry("a")["dir"]
	refusal := s.spoilRun("a")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.c.perform(ctx, first.ClientID, "fix.exit", nil) // the reply may not arrive: the process ends
	s.wait("the respawn is refused", func() bool { return s.status("a").State == "refused" })
	st := s.status("a")
	if st.Error != refusal {
		t.Errorf("error %q, want %q", st.Error, refusal)
	}
	if st.BlockID != first.BlockID || st.InstanceID != first.InstanceID || st.Registered {
		t.Errorf("the old block is kept: %+v, first %+v", st, first)
	}
	if n := len(s.events("plugin_spawned", "a")); n != 1 {
		t.Errorf("%d spawns: the refused start spawns nothing", n)
	}
	if e := s.entry("a"); e["state"] != "refused" || e["error"] != refusal {
		t.Errorf("hub.plugins: %v", e)
	}
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	s.registered("a")
	if st := s.status("a"); st.InstanceID == first.InstanceID || st.BlockID == first.BlockID || st.State != "running" {
		t.Errorf("after the repair: %+v", st)
	}
	if d := s.entry("a")["dir"]; d == spoiled {
		t.Errorf("the explicit start runs the spoiled snapshot %v", d)
	}
	if n := len(s.events("plugin_spawned", "a")); n != 2 {
		t.Errorf("%d spawns after the repair", n)
	}
}

// A hub restart with a refused candidate (section 7, "Retained instance"): the old process lives
// with its pid and start time, its block stays, and the record is retained. The dependent b also
// has a live instance, and its static check refuses it: b is retained too.
// hub.admin.stop retires the retained block.
func TestPluginRetainedAtRestartLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", `{"name":"b","main":"main.lua","depends":["a"]}`, "b")
	s.start()
	s.registered("a")
	s.registered("b")
	old := s.status("a")
	oldStart := startTime(old.ChildPID)
	if old.ChildPID == 0 || oldStart == "" {
		t.Fatalf("no child: %+v", old)
	}
	spawns := len(s.events("plugin_spawned", "a")) // the event log keeps the first hub's events
	s.stop()
	s.writeSource("a", `{"name":"a","main":"main.lua","depends":["missing"]}`, "")
	s.start()
	s.wait("a is retained", func() bool { return s.status("a").State == "retained" })
	st := s.status("a")
	if st.ChildPID != old.ChildPID || st.BlockID != old.BlockID || !alive(old.ChildPID, oldStart) {
		t.Errorf("the old process lives in its block: %+v, old %+v", st, old)
	}
	if e := s.entry("a"); e["state"] != "retained" || e["error"] != "plugin a: dependency missing is not configured" {
		t.Errorf("hub.plugins a: %v", e)
	}
	s.wait("b is retained", func() bool { return s.status("b").State == "retained" })
	if e := s.entry("b"); e["error"] != "plugin b: dependency a is refused: plugin a: dependency missing is not configured" {
		t.Errorf("hub.plugins b: %v", e)
	}
	if n := len(s.events("plugin_spawned", "a")) - spawns; n != 0 {
		t.Errorf("the restarted hub spawned a %d times", n)
	}
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	s.wait("the retained process ends", func() bool { return !alive(old.ChildPID, oldStart) })
	if st := s.status("a"); st.State != "stopped" || st.BlockID != "" {
		t.Errorf("after the stop: %+v", st)
	}
}

// The repair entry point (section 7): an entry with no manifest at hub start has no name. After
// its manifest appears, hub.admin.start {"source"} resolves it, binds the name and starts it.
func TestPluginStartBySourceLab(t *testing.T) {
	s := newStartLab(t, "a", "late")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("late", "", "")
	s.start()
	s.registered("a")
	if e := s.entry("late"); e["state"] != "refused" || e["name"] != nil || !strings.Contains(fmt.Sprint(e["error"]), "no lazyrex.json") {
		t.Errorf("late at start: %v", e)
	}
	if _, err := s.call("hub.admin.start", map[string]any{"source": "src/late"}); err == nil || !strings.Contains(err.Error(), "no lazyrex.json") {
		t.Errorf("start before the repair: %v", err)
	}
	if _, err := s.call("hub.admin.start", map[string]any{"source": "src/none"}); err == nil || !strings.Contains(err.Error(), `no plugin entry has the source "src/none"`) {
		t.Errorf("an unknown source: %v", err)
	}
	s.writeSource("late", `{"name":"l","main":"main.lua"}`, "l")
	s.must("hub.admin.start", map[string]any{"source": "src/late"})
	s.registered("l")
	if e := s.entry("late"); e["state"] != "running" || e["name"] != "l" {
		t.Errorf("late after the repair: %v", e)
	}
}

// [R2] hub.admin.start does not abandon an unresolved spawn intent: a sending intent in a live
// session with no block stops the start (code unresolved_intent), and nothing spawns. When that
// session closes, the intent is abandoned and the start succeeds.
func TestPluginStartUnresolvedIntentLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	s.registered("a")
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	s.stop()
	// A session that the create named, which has no block of the instance (a create that may still
	// complete).
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
	spawns := len(s.events("plugin_spawned", "a")) // the event log keeps the first hub's events
	_, err = s.call("hub.admin.start", map[string]any{"plugin": "a"})
	want := "unresolved_intent: plugin a: spawn a-0-held in session " + created.SessionID + " may still create a block, so the hub does not start a second instance. It resolves when that block appears or that session closes."
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("start with an open intent: %v", err)
	}
	if n := len(s.events("plugin_spawned", "a")) - spawns; n != 0 {
		t.Errorf("%d spawns with an open intent", n)
	}
	// Rex refuses session.destroy on a connection that has not attached the session (observed on
	// Rex 0.0.1792158932-g88fd41).
	if err := s.l.r.Call(ctx, "session.attach", map[string]any{"session_id": created.SessionID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.l.r.Call(ctx, "session.destroy", map[string]any{"session_id": created.SessionID}, nil); err != nil {
		t.Fatal(err)
	}
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	s.registered("a")
	if n := len(s.events("plugin_intent_abandoned", "a")); n != 1 {
		t.Errorf("%d intents abandoned", n)
	}
}

// kept returns the hub.plugins kept record of name, or nil.
func (s *startLab) kept(name string) map[string]any {
	s.t.Helper()
	b, _ := json.Marshal(s.must("hub.plugins", nil)["kept"])
	var list []map[string]any
	json.Unmarshal(b, &list)
	for _, k := range list {
		if k["name"] == name {
			return k
		}
	}
	return nil
}

// hasRecord reports whether hub.test.status lists a record of name.
func (s *startLab) hasRecord(name string) bool {
	s.t.Helper()
	b, _ := json.Marshal(s.must("hub.test.status", nil)["plugins"])
	var m map[string]any
	json.Unmarshal(b, &m)
	_, ok := m[name]
	return ok
}

// stopKept stops the kept record a, checks that its old process ends and that the record is
// dropped.
func (s *startLab) stopKept(name string, pid int, start string) {
	s.t.Helper()
	s.must("hub.admin.stop", map[string]any{"plugin": name})
	s.wait("the kept process ends", func() bool { return !alive(pid, start) })
	if s.hasRecord(name) || s.kept(name) != nil {
		s.t.Errorf("the stopped kept record %s stays", name)
	}
}

// startRun starts a and returns its status and the start time of its process.
func (s *startLab) startRun(name string) (startStatus, string) {
	s.t.Helper()
	s.registered(name)
	st := s.status(name)
	start := startTime(st.ChildPID)
	if st.ChildPID == 0 || start == "" {
		s.t.Fatalf("no child: %+v", st)
	}
	return st, start
}

// A hub restart with a source that gives no name (reviewer, hub-review-2d1-blockers): the entry is
// refused with no name, and the plugins row gives a kept record a. The old process lives with the
// same pid and start time, hub.plugins lists a as kept, and hub.admin.stop ends it.
func TestPluginKeptUnreadableSourceLab(t *testing.T) {
	breaks := map[string]func(s *startLab){
		"missing manifest":   func(s *startLab) { os.Remove(filepath.Join(s.src, "a", "lazyrex.json")) },
		"malformed manifest": func(s *startLab) { s.writeSource("a", `{"name":`, "") },
		"missing main file":  func(s *startLab) { os.Remove(filepath.Join(s.src, "a", "main.lua")) },
	}
	for what, breakSource := range breaks {
		t.Run(what, func(t *testing.T) {
			s := newStartLab(t, "a")
			s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
			s.start()
			old, oldStart := s.startRun("a")
			s.stop()
			breakSource(s)
			s.start()
			s.wait("a is retained", func() bool { return s.status("a").State == "retained" })
			if st := s.status("a"); st.ChildPID != old.ChildPID || st.BlockID != old.BlockID || st.InstanceID != old.InstanceID || !alive(old.ChildPID, oldStart) {
				t.Errorf("the old process lives in its block: %+v, old %+v", st, old)
			}
			if e := s.entry("a"); e["name"] != nil || e["state"] != "refused" || !strings.HasPrefix(fmt.Sprint(e["error"]), "plugin source src/a: ") {
				t.Errorf("the entry: %v", e)
			}
			if k := s.kept("a"); k == nil || k["source"] != "src/a" || k["state"] != "retained" || !s.snapshot(k["dir"]) ||
				k["error"] != "plugin a: no entry gives this name now (it ran from source src/a); stop it with hub.admin.stop" {
				t.Errorf("kept: %v", k)
			}
			if _, err := s.call("hub.admin.start", map[string]any{"plugin": "a"}); err == nil || !strings.Contains(err.Error(), "has no config entry") {
				t.Errorf("start of a kept record: %v", err)
			}
			s.stopKept("a", old.ChildPID, oldStart)
		})
	}
}

// A manifest renamed across a restart: the source still has the instance of a, so the new name y
// is refused (the guard) and nothing spawns. After a stop of the kept a, a start by source starts y.
func TestPluginKeptRenamedSourceLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	old, oldStart := s.startRun("a")
	s.stop()
	s.writeSource("a", `{"name":"y","main":"main.lua"}`, "y")
	s.start()
	s.wait("a is retained", func() bool { return s.status("a").State == "retained" })
	if e := s.entry("a"); e["state"] != "refused" || e["error"] != "plugin y: source src/a still has an instance of plugin a; stop a first" {
		t.Errorf("the entry: %v", e)
	}
	if n := len(s.events("plugin_spawn_intent", "y")); n != 0 || s.hasRecord("y") {
		t.Errorf("y: %d spawn intents before the stop", n)
	}
	s.stopKept("a", old.ChildPID, oldStart)
	if n := len(s.events("plugin_spawn_intent", "y")); n != 0 {
		t.Errorf("a stop alone starts y: %d", n)
	}
	s.must("hub.admin.start", map[string]any{"source": "src/a"})
	s.registered("y")
}

// A collision (reviewer): src/a lost its manifest and src/b names a. Both entries are refused; the
// kept record a holds the old process and stops. Then a start by source binds a to src/b.
func TestPluginKeptCollisionLab(t *testing.T) {
	s := newStartLab(t, "a", "b")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("b", "", "")
	s.start()
	old, oldStart := s.startRun("a")
	s.stop()
	os.Remove(filepath.Join(s.src, "a", "lazyrex.json"))
	s.writeSource("b", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	s.wait("a is retained", func() bool { return s.status("a").State == "retained" })
	if e := s.entry("a"); e["state"] != "refused" || e["name"] != nil {
		t.Errorf("src/a: %v", e)
	}
	if e := s.entry("b"); e["state"] != "refused" || e["name"] != "a" || e["error"] != "plugin a: the name is held by an instance from source src/a; stop a first" {
		t.Errorf("src/b: %v", e)
	}
	if st := s.status("a"); st.ChildPID != old.ChildPID || !alive(old.ChildPID, oldStart) {
		t.Errorf("the old process: %+v", st)
	}
	s.stopKept("a", old.ChildPID, oldStart)
	s.must("hub.admin.start", map[string]any{"source": "src/b"})
	s.registered("a")
	if e := s.entry("b"); e["state"] != "running" || e["name"] != "a" {
		t.Errorf("src/b after the start: %v", e)
	}
}

// A kept record with only an unresolved intent (no block): it is refused and keeps ownership. Its
// stop gives unresolved_intent (the stop text) until the intent's session closes; then the stop
// drops it.
func TestPluginKeptIntentOnlyLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	s.registered("a")
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
	os.Remove(filepath.Join(s.src, "a", "lazyrex.json"))
	s.start()
	s.wait("a is refused", func() bool { return s.status("a").State == "refused" })
	if k := s.kept("a"); k == nil || k["source"] != "src/a" {
		t.Errorf("kept: %v", k)
	}
	_, err = s.call("hub.admin.stop", map[string]any{"plugin": "a"})
	want := "unresolved_intent: plugin a: the stop is not complete: spawn a-0-held in session " + created.SessionID + " may still create a block. The record stays. Stop again after that block appears or that session closes."
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("stop with an open intent: %v", err)
	}
	if !s.hasRecord("a") {
		t.Error("the record is dropped while its intent is open")
	}
	if err := s.l.r.Call(ctx, "session.attach", map[string]any{"session_id": created.SessionID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.l.r.Call(ctx, "session.destroy", map[string]any{"session_id": created.SessionID}, nil); err != nil {
		t.Fatal(err)
	}
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	if s.hasRecord("a") || s.kept("a") != nil {
		t.Error("the record stays after a complete stop")
	}
}

// setConfig sets one field of the hub config, for the next hub start.
func (s *startLab) setConfig(key string, value any) {
	s.t.Helper()
	var c map[string]any
	b, _ := os.ReadFile(s.cfg)
	json.Unmarshal(b, &c)
	c[key] = value
	b, _ = json.MarshalIndent(c, "", " ")
	if err := os.WriteFile(s.cfg, b, 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// A stop before recovery reaches a kept record (reviewer, hub-review-2d1-kept-startup-blocker):
// the record has its persisted instance from its creation, so the stop retires the old block and
// ends its process. It does not save empty fields over the instance.
func TestPluginKeptStopBeforeRecoveryLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	old, oldStart := s.startRun("a")
	s.stop()
	os.Remove(filepath.Join(s.src, "a", "lazyrex.json"))
	s.setConfig("test_hold_recovery", true)
	s.start()
	if st := s.status("a"); st.BlockID != old.BlockID || st.InstanceID != old.InstanceID {
		t.Errorf("the kept record before recovery: %+v, old %+v", st, old)
	}
	s.must("hub.admin.stop", map[string]any{"plugin": "a"})
	s.wait("the old process ends", func() bool { return !alive(old.ChildPID, oldStart) })
	if s.hasRecord("a") {
		t.Error("the kept record stays after the stop")
	}
	s.must("hub.test.recover", nil)
	time.Sleep(time.Second)
	if s.hasRecord("a") {
		t.Error("recovery made the record again")
	}
}

// A start before recovery reaches an adopted record: the record has its persisted instance, so the
// start retires the old instance and spawns one new instance. Recovery then skips the record: it
// does not replace the new instance.
func TestPluginStartBeforeRecoveryLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.start()
	old, oldStart := s.startRun("a")
	s.stop()
	s.setConfig("test_hold_recovery", true)
	s.start()
	intents := len(s.events("plugin_spawn_intent", "a"))
	s.must("hub.admin.start", map[string]any{"plugin": "a"})
	s.wait("the old process ends", func() bool { return !alive(old.ChildPID, oldStart) })
	cur, _ := s.startRun("a")
	s.must("hub.test.recover", nil)
	time.Sleep(1500 * time.Millisecond)
	if n := len(s.events("plugin_spawn_intent", "a")) - intents; n != 1 {
		t.Errorf("%d spawns after the early start; want 1", n)
	}
	if st := s.status("a"); st.InstanceID != cur.InstanceID || !st.Registered {
		t.Errorf("recovery replaced the instance: %+v, after the start %+v", st, cur)
	}
}

// crashFixture registers, then ends its process with status 1 at once: a crash loop.
const crashFixture = `local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
rex.call('client.set_label', {label = 'lazyrex-plugin:c'})
local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.register', args = {plugin = 'c', instance_id = instance_id, exports = {}}, timeout_ms = 10000})
if err then print('register: ' .. tostring(err)) end
rex.stop(1)
`

// The crash backoff (lead-hf-queue item 2): a plugin that registers and exits at once respawns
// after 1, 2, 4 ... s, so it spawns at most 3 times in 4 s (before the backoff: without a pause,
// without end). While the respawn is due, hub.plugins reports "crashing" with last_exit,
// respawn_at and respawn_delay_ms. hub.admin.stop while crashing cancels the respawn.
func TestPluginCrashBackoffLab(t *testing.T) {
	s := newStartLab(t, "c")
	s.writeSource("c", `{"name":"c","main":"main.lua"}`, "")
	if err := os.WriteFile(filepath.Join(s.src, "c", "main.lua"), []byte(crashFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s.start()
	s.wait("the first spawn", func() bool { return len(s.events("plugin_spawned", "c")) > 0 })
	time.Sleep(4 * time.Second)
	n := len(s.events("plugin_spawned", "c"))
	if n < 2 || n > 3 {
		t.Fatalf("%d spawns in about 4 s; want 2 or 3", n)
	}
	var e map[string]any
	s.wait("crashing", func() bool { e = s.entry("c"); return e["state"] == "crashing" })
	le, _ := e["last_exit"].(map[string]any)
	if le == nil || le["detail"] == "" || le["at"] == nil || e["respawn_at"] == nil || e["desired"] != "running" || e["registered"] != false {
		t.Fatalf("hub.plugins while crashing: %v", e)
	}
	delay, _ := e["respawn_delay_ms"].(float64)
	if delay < 1000 || delay > 60000 {
		t.Fatalf("respawn_delay_ms %v", e["respawn_delay_ms"])
	}
	s.must("hub.admin.stop", map[string]any{"plugin": "c"})
	n = len(s.events("plugin_spawned", "c"))
	time.Sleep(time.Duration(delay)*time.Millisecond + 1500*time.Millisecond)
	if m := len(s.events("plugin_spawned", "c")); m != n {
		t.Fatalf("%d spawns after the stop", m-n)
	}
	if e := s.entry("c"); e["state"] != "stopped" || e["respawn_at"] != nil || e["last_exit"] == nil {
		t.Fatalf("hub.plugins after the stop: %v", e)
	}
	if r := s.events("plugin_respawn", "c"); len(r) == 0 {
		t.Fatal("no plugin_respawn event")
	}
}
