package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// The action golden files (testdata/actions) hold what the hub's actions are today: the exact
// keymap of each face, the agent set that tool parity reads, the faultable set, and the outcome of
// one call of each action through the real dispatcher for each kind of caller. They were made from
// main before the action table (B2), so a refactor of the action lists must keep them byte for
// byte. -update-actions writes them again; a change to them is a change of the hub's interface.
var updateActions = flag.Bool("update-actions", false, "write testdata/actions from the current code")

func actionGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "actions", name)
	if *updateActions {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from the current code (run with -update-actions to see the diff in git)", path)
	}
}

// TestActionFacesGolden: the plugin face (outside and in test mode) and the agent face declare
// exactly the golden actions, in the golden order; the agent set of tool parity and the faultable
// set are the golden sets.
func TestActionFacesGolden(t *testing.T) {
	actionGolden(t, "plugin.json", hubClientActions(false))
	actionGolden(t, "plugin-test.json", hubClientActions(true))
	actionGolden(t, "agent.json", agentHubFace())
	var agentSet []string
	for name := range agentHubActions {
		agentSet = append(agentSet, name)
	}
	sort.Strings(agentSet)
	actionGolden(t, "agent-set.json", agentSet)
	var faultable []string
	for _, name := range actionCallNames() {
		if faultableAction(name) {
			faultable = append(faultable, name)
		}
	}
	actionGolden(t, "faultable.json", faultable)
	// The render contract (coveredReads) names only actions of the plugin face.
	plugin := map[string]bool{}
	for _, a := range hubClientActions(false) {
		plugin[a["name"].(string)] = true
	}
	for name := range coveredReads {
		if !plugin[name] {
			t.Errorf("coveredReads names %s, which the plugin face does not declare", name)
		}
	}
}

// actionCallNames is every action that a face declares, sorted, and two unknown names.
func actionCallNames() []string {
	seen := map[string]bool{"hub.nope": true, "hub.test.nope": true}
	for _, a := range append(hubClientActions(true), agentHubFace()...) {
		seen[a["name"].(string)] = true
	}
	var names []string
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// goldenCaller is one kind of caller of the dispatch golden. pid 5000040 runs in the pane of
// plugin "p" (its child is pid 5000030); pid 5000500 runs in no managed pane. The pids are above
// the largest pid of macOS (99998) and of Linux (4194304), so a handler cannot signal a real
// process.
type goldenCaller struct {
	name, client string
	pid          int
}

var goldenCallers = []goldenCaller{
	{"plain", "c-user", 5000500}, // a client of lazyrex-hub outside agent and plugin panes: the user's CLI
	{"pane", "c-other", 5000040}, // a client in a plugin pane that is not a registered instance
	{"plugin", "c-p", 5000040},   // the registered instance of plugin p
}

// goldenHub is a hub for one golden call: the hub's full schema (the schema steps of the hub start,
// without their background work), a Rex that answers every request with {}, one registered plugin
// p, and a fixed process tree. Every file is in the test's own directory. No background worker
// runs: plugin p's view worker is a kick channel without a goroutine, and the plugin manager's
// queue has no loop, so the golden records only what the request itself does. close ends the SQL
// service and closes the files.
func goldenHub(t *testing.T, r *rex.Rex, test bool) (h *Hub, close func()) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := openEvents(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{State: dir, path: filepath.Join(dir, "config.json"), managedPath: filepath.Join(dir, "plugins.json")}
	h = &Hub{noticeKick: make(chan struct{}, 1), paletteKick: make(chan struct{}, 1), spawnKick: make(chan struct{}, 1), resumeKick: make(chan struct{}, 1), waitKick: make(chan struct{}, 1),
		cfg: cfg, db: db, rex: r, ev: ev, calls: newCallLog(), clientID: "c-hub",
		idents: map[string]*Agent{}, spawnAddrs: map[string]*spawnAddr{}, selfClosed: map[string]bool{},
		plugins: map[string]*Plugin{"p": {Name: "p", ClientID: "c-p", ChildPID: 5000030, Desired: "running", entry: &configEntry{}}},
		ui:      &uiState{kick: make(chan struct{}, 1), sess: map[string]string{}, labeled: map[string]string{}, own: map[string]bool{}}}
	h.peers = newPeerSet(cfg)
	h.sql = newSQLService(dir, h.currentClient)
	close = func() {
		h.sql.close()
		db.Close()
		ev.f.Close()
	}
	// The schema part of uiStart, then the other start steps that make tables, in the hub's order.
	steps := []func() error{
		func() error { _, err := db.ExecContext(ctx, uiSchema); return err },
		func() error { return formsStart(ctx, db) },
		func() error { _, err := db.ExecContext(ctx, ensuredSchema); return err },
		func() error { _, err := db.ExecContext(ctx, panelSchema); return err },
		func() error { return h.slotStart(ctx) },
		func() error { _, err := db.ExecContext(ctx, homeSchema); return err },
		func() error { return h.workersSchemaStart(ctx) },
		func() error { return h.viewsStart(ctx) },
		func() error { return h.pluginOpsSchemaUp(ctx) },
		func() error { return h.watchStart(ctx) },
		func() error { _, err := db.ExecContext(ctx, promptAnswerSchema); return err },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			close()
			t.Fatal(err)
		}
	}
	h.vs.workers["p"] = make(chan struct{}, 1)
	h.pops.kick = make(chan struct{}, 1)
	parents := map[int]int{5000040: 5000030, 5000030: 1, 5000500: 1}
	h.procRead = func(pid int) (procRecord, error) { return procRecord{ppid: parents[pid]}, nil }
	if test {
		h.test = newTestState()
	}
	return h, close
}

// goldenQuiet fails the test if a golden call left a goroutine of this package running (a view
// worker of another plugin, a watch, a load): base is runtime.NumGoroutine before the call.
func goldenQuiet(t *testing.T, call string, base int) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if runtime.NumGoroutine() <= base {
			return
		}
	}
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var left []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "hub/cmd/hub.(*Hub)") {
			left = append(left, g)
		}
	}
	t.Errorf("%s left %d goroutines over %d; hub goroutines:\n%s", call, runtime.NumGoroutine()-base, base, strings.Join(left, "\n\n"))
}

// goldenCall runs one call and returns its outcome: "ok", "error: <text>" or "panic: <value>".
func goldenCall(run func(context.Context) (any, error), dir string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("panic: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := run(ctx)
	if err != nil {
		return "error: " + strings.ReplaceAll(err.Error(), dir, "$STATE")
	}
	return "ok"
}

// TestActionDispatchGolden: each action, called once with args {} through h.dispatch as each kind
// of caller (outside and in test mode), and each agent face action through agentDispatch, has its
// golden outcome.
func TestActionDispatchGolden(t *testing.T) {
	r := limitRex(t, 1<<20, func(string, json.RawMessage) (any, string) { return map[string]any{}, "" })
	out := map[string]map[string]string{}
	add := func(name, key, outcome string) {
		if out[name] == nil {
			out[name] = map[string]string{}
		}
		out[name][key] = outcome
	}
	agent := map[string]bool{"hub.nope": true}
	for _, a := range agentHubFace() {
		agent[a["name"].(string)] = true
	}
	for _, name := range actionCallNames() {
		for _, test := range []bool{false, true} {
			for _, c := range goldenCallers {
				key := c.name
				if test {
					key += "/test"
				}
				base := runtime.NumGoroutine()
				h, close := goldenHub(t, r, test)
				var p perform
				p.Action, p.Args = name, json.RawMessage(`{}`)
				p.Requester.ClientID, p.Requester.Principal.PID = c.client, c.pid
				add(name, key, goldenCall(func(ctx context.Context) (any, error) { return h.dispatch(ctx, p) }, h.cfg.State))
				close()
				goldenQuiet(t, name+" "+key, base)
			}
		}
		if agent[name] {
			base := runtime.NumGoroutine()
			h, close := goldenHub(t, r, false)
			var p perform
			p.Action, p.Args = name, json.RawMessage(`{}`)
			p.Requester.Principal.PID = 5000500
			add(name, "agent", goldenCall(func(ctx context.Context) (any, error) { return h.agentDispatch(ctx, p) }, h.cfg.State))
			close()
			goldenQuiet(t, name+" agent", base)
		}
	}
	actionGolden(t, "dispatch.json", out)
}
