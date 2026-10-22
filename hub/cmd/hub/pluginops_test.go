package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// opsCalls records the manager's lifecycle calls (opAdmin, opReload).
type opsCalls struct {
	mu    sync.Mutex
	calls []string
	reply func(action string, args map[string]any) (any, error)
}

func (c *opsCalls) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.calls...)
}

// opsTestHub is a hub with the op tables, a state directory, a managed file and the entries that
// the file gives. The lifecycle calls go to the returned recorder; reload parses the file again.
func opsTestHub(t *testing.T, managed string) (*Hub, *opsCalls, string) {
	t.Helper()
	h, _ := workerTestHub(t)
	for _, s := range []string{pluginOpsSchema, viewOpsSchema} {
		if _, err := h.db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	h.cfg.State, h.cfg.path, h.cfg.managedPath = dir, filepath.Join(dir, "hub.json"), filepath.Join(dir, "plugins.json")
	h.cfg.Home = "home"
	h.plugins = map[string]*Plugin{}
	h.noticeKick = make(chan struct{}, 1)
	h.vs = &viewSvc{dirty: map[string]bool{}, workers: map[string]chan struct{}{}}
	if managed != "" {
		writeManaged(t, dir, managed)
	}
	opsLoadEntries(t, h)
	c := &opsCalls{}
	admin, reload, call, blocks := opAdmin, opReload, changedCall, opSessionBlocks
	t.Cleanup(func() { opAdmin, opReload, changedCall, opSessionBlocks = admin, reload, call, blocks })
	opSessionBlocks = func(context.Context, *Hub, string) (map[string]bool, error) {
		return nil, errors.New("this test has no Rex")
	}
	t.Cleanup(func() { waitIdle(t, h) }) // runs first: no delivery outlives the fake
	changedCall = func(context.Context, *Hub, string, string) error { return nil }
	opAdmin = func(ctx context.Context, h *Hub, action string, args map[string]any) (any, error) {
		// The fake binds the attempt and commits sent_at as the handler does (adminattempt.go), and
		// logs the call without the attempt ID, which is random.
		logged := map[string]any{}
		for k, v := range args {
			logged[k] = v
		}
		if attempt, _ := args["attempt"].(string); attempt != "" {
			delete(logged, "attempt")
			name, _ := args["plugin"].(string)
			var instance, block, session string
			h.db.QueryRowContext(ctx, `SELECT instance_id, block_id, session_id FROM plugins WHERE name = ?`, name).Scan(&instance, &block, &session)
			if err := h.bindAttempt(ctx, attempt, strings.TrimPrefix(action, "hub.admin."), instance, block, session); err != nil {
				return nil, err
			}
			if err := h.attemptSent(ctx, attempt); err != nil {
				return nil, err
			}
		}
		b, _ := json.Marshal(logged)
		c.mu.Lock()
		c.calls = append(c.calls, action+" "+string(b))
		reply := c.reply
		c.mu.Unlock()
		if reply != nil {
			return reply(action, args)
		}
		return map[string]any{"ok": true}, nil
	}
	opReload = func(ctx context.Context, h *Hub) (any, error) {
		c.mu.Lock()
		c.calls = append(c.calls, "reload")
		c.mu.Unlock()
		opsLoadEntries(t, h)
		return map[string]any{"errors": map[string]string{}}, nil
	}
	return h, c, dir
}

// opsProof is the stored proof of the op's attempt of call.
func opsProof(t *testing.T, h *Hub, id, call string) string {
	t.Helper()
	var proof string
	if err := h.db.QueryRow(`SELECT proof FROM plugin_op_attempts WHERE op_id = ? AND call = ?`, id, call).Scan(&proof); err != nil {
		t.Fatal(err)
	}
	return proof
}

// opsLoadEntries sets the hub's entries from the managed file; each managed entry's name is its id.
func opsLoadEntries(t *testing.T, h *Hub) {
	t.Helper()
	list, err := parseManaged(h.cfg.managedPath)
	if err != nil {
		t.Fatal(err)
	}
	h.entriesMu.Lock()
	var config []*configEntry
	for _, e := range h.entries {
		if e.managed == "" {
			config = append(config, e)
		}
	}
	for _, e := range list {
		e.name = e.managed
	}
	h.entries = addManaged(config, list)
	h.entriesMu.Unlock()
}

func opsAdmit(t *testing.T, h *Hub, agent, key string, req opRequest) (map[string]any, error) {
	t.Helper()
	v, err := h.admitOp(context.Background(), opOrigin{"agent", agent}, "agent:"+agent, key, req)
	if err != nil {
		return nil, err
	}
	return v.(map[string]any), nil
}

func opsMust(t *testing.T, h *Hub, key string, req opRequest) string {
	t.Helper()
	v, err := opsAdmit(t, h, "a1", key, req)
	if err != nil {
		t.Fatal(err)
	}
	return v["op_id"].(string)
}

func opsOf(t *testing.T, h *Hub, id string) *pluginOp {
	t.Helper()
	op, err := h.loadOp(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func opsFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// opsLocal is a managed file with local plugin directories under dir.
func opsLocal(t *testing.T, dir string, ids ...string) string {
	var parts []string
	for _, id := range ids {
		plugDir(t, dir, id, `{"name":"`+id+`","main":"m.lua"}`, "m.lua")
		parts = append(parts, `{"id":"`+id+`","source":"`+filepath.Join(dir, id)+`"}`)
	}
	return `{"plugins":[` + strings.Join(parts, ",") + `]}`
}

// One key with another request is operation_conflict; the same request returns the same op; two
// agents with one key get two ops.
func TestPluginOpKeys(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	req := opRequest{Op: "stop", Entry: "managed:x"}
	one := opsMust(t, h, "k", req)
	if again := opsMust(t, h, "k", req); again != one {
		t.Errorf("a retry made a new op: %s, %s", one, again)
	}
	if _, err := opsAdmit(t, h, "a1", "k", opRequest{Op: "start", Entry: "managed:x"}); !isRefusal(err, "operation_conflict") {
		t.Errorf("another request with the key: %v", err)
	}
	other, err := opsAdmit(t, h, "a2", "k", req)
	if err != nil || other["op_id"] == one {
		t.Errorf("another agent with the key: %v %v", other, err)
	}
	for _, bad := range []opRequest{{Op: "purge", Entry: "managed:x"}, {Op: "stop", Entry: "managed:nope"}, {Op: "stop", Entry: "managed:x", Input: json.RawMessage(`{}`)}} {
		if _, err := opsAdmit(t, h, "a1", "bad-"+bad.Op+bad.Entry+string(bad.Input), bad); err == nil {
			t.Errorf("%+v was admitted", bad)
		}
	}
}

// Settings input is checked as a hand edit is: unknown fields (the grants among them) and reserved
// args are refused at admission, and nothing is stored.
func TestPluginOpSettingsInput(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	writeManaged(t, h.cfg.State, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	for _, in := range []string{`{"user_messaging":true}`, `{"prompts":true}`, `{"args":{"instance_id":"1"}}`, `{"watch_roots":["rel"]}`} {
		if _, err := opsAdmit(t, h, "a1", in, opRequest{Op: "settings", Entry: "managed:x", Input: json.RawMessage(in)}); err == nil {
			t.Errorf("input %s was admitted", in)
		}
	}
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM plugin_ops`).Scan(&n)
	if n != 0 {
		t.Errorf("%d ops stored", n)
	}
}

// Stop and remove of the configured home entry are refused; the entry is the one that binds the
// home name.
func TestPluginOpHomeRefused(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	writeManaged(t, h.cfg.State, opsLocal(t, t.TempDir(), "home"))
	opsLoadEntries(t, h)
	for _, op := range []string{"stop", "remove"} {
		if _, err := opsAdmit(t, h, "a1", op, opRequest{Op: op, Entry: "managed:home"}); err == nil || !strings.Contains(err.Error(), "home plugin") {
			t.Errorf("%s of the home entry: %v", op, err)
		}
	}
	if _, err := opsAdmit(t, h, "a1", "start", opRequest{Op: "start", Entry: "managed:home"}); err != nil {
		t.Errorf("start of the home entry: %v", err)
	}
}

// An agent's start of a GitHub entry is admitted only for the record's last run, with that run as
// expect_run. A new candidate or no run needs the user. A user's start names the run that the
// panel showed, and only that run is expected.
func TestPluginOpStartAuthorization(t *testing.T) {
	h, c, _ := opsTestHub(t, "")
	src := "github:o/r@main"
	run := func(commit string) pluginRun {
		return pluginRun{Source: src, Commit: strings.Repeat(commit, 40), Digest: "sha256:" + strings.Repeat("d", 64)}
	}
	e := &configEntry{Source: src, github: true, name: "g", index: 1}
	h.entries = []*configEntry{e}
	pl := &Plugin{Name: "g", entry: e}
	h.plugins["g"] = pl
	req := opRequest{Op: "start", Entry: src}

	if _, err := opsAdmit(t, h, "a1", "none", req); !isRefusal(err, "user_confirmation_required") {
		t.Errorf("no run: %v", err)
	}
	pl.run = run("a")
	c1 := run("a")
	e.cand = &c1
	id := opsMust(t, h, "same", req)
	c2 := run("b")
	e.cand = &c2
	if _, err := opsAdmit(t, h, "a1", "new", req); !isRefusal(err, "user_confirmation_required") {
		t.Errorf("a new candidate: %v", err)
	}
	// The admitted op starts the last run, even though the candidate changed meanwhile: the start
	// path refuses it (candidate_changed), and the op ends in conflict.
	c.reply = func(string, map[string]any) (any, error) {
		return nil, refuse("candidate_changed", "plugin g: the candidate is b, not a")
	}
	h.opsRun(context.Background())
	calls := c.list()
	eID, _ := json.Marshal(entryIdentity(e))
	want := `hub.admin.start {"expect_entry":` + string(eID) + `,"expect_run":{"commit":"` + run("a").Commit + `","digest":"` + run("a").Digest + `","source":"` + src + `"},"plugin":"g"}`
	if len(calls) != 1 || calls[0] != want {
		t.Errorf("calls %v, want %s", calls, want)
	}
	if op := opsOf(t, h, id); op.Phase != "conflict" || !strings.Contains(op.Error, "candidate_changed") {
		t.Errorf("op %+v", op)
	}

	// The user: the expect of the panel is the target.
	shown := run("b")
	exp, _ := json.Marshal(map[string]string{"source": shown.Source, "commit": shown.Commit, "digest": shown.Digest})
	v, err := h.admitOp(context.Background(), opOrigin{"user", "view-1"}, "user", "view:home:k1", opRequest{Op: "start", Entry: src, Expect: exp})
	if err != nil {
		t.Fatal(err)
	}
	if op := opsOf(t, h, v.(map[string]any)["op_id"].(string)); op.Target != string(exp) {
		t.Errorf("user target %s, want %s", op.Target, exp)
	}
	if _, err := h.admitOp(context.Background(), opOrigin{"user", "view-1"}, "user", "view:home:k2", opRequest{Op: "start", Entry: src}); err == nil {
		t.Error("a user start without expect was admitted")
	}
	// An agent cannot give expect.
	var args map[string]json.RawMessage
	json.Unmarshal([]byte(`{"key":"k","op":"start","entry":"`+src+`","expect":`+string(exp)+`}`), &args)
	if _, _, err := agentOpRequest(args); err == nil || !strings.Contains(err.Error(), "expect is set only") {
		t.Errorf("an agent request with expect: %v", err)
	}
}

// The "plugins" effect: only the home plugin's registered instance, and only for a user view
// action stored in view_ops. A result that is not a user's view action (for example the home
// plugin's answer to an agent's forwarded call) admits nothing.
func TestPluginOpEffectAttestation(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	writeManaged(t, h.cfg.State, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	home := &configEntry{Source: "/h", name: "home", index: 1}
	h.plugins["home"] = &Plugin{Name: "home", entry: home, ClientID: "c-home"}
	h.plugins["other"] = &Plugin{Name: "other", entry: &configEntry{Source: "/o", name: "other"}, ClientID: "c-other"}
	ctx := context.Background()
	raw := json.RawMessage(`{"op":"stop","entry":"managed:x"}`)
	h.db.Exec(`INSERT INTO view_ops (plugin, user, op_key, view_id, kind, fingerprint, args, status) VALUES ('home', 'user', 'k1', 'v1', 'action', 'f', '{}', 'succeeded'), ('other', 'user', 'k1', 'v2', 'action', 'f', '{}', 'succeeded')`)

	if retry, err := h.pluginOpEffect(ctx, "other", "k1", "v2", raw); retry || err == nil || !strings.Contains(err.Error(), "only the home plugin") {
		t.Errorf("another plugin: %v %v", retry, err)
	}
	if retry, err := h.pluginOpEffect(ctx, "home", "forwarded", "v1", raw); retry || err == nil || !strings.Contains(err.Error(), "no user view action") {
		t.Errorf("no view operation: %v %v", retry, err)
	}
	if retry, err := h.pluginOpEffect(ctx, "home", "k1", "v1", raw); retry || err != nil {
		t.Fatalf("the user's view action: %v %v", retry, err)
	}
	if retry, err := h.pluginOpEffect(ctx, "home", "k1", "v1", raw); retry || err != nil {
		t.Errorf("a repeat of the effect: %v %v", retry, err)
	}
	var n int
	var origin string
	h.db.QueryRow(`SELECT COUNT(*), MAX(origin) FROM plugin_ops WHERE scope = 'user' AND key = 'view:home:k1'`).Scan(&n, &origin)
	if n != 1 || origin != `{"kind":"user","id":"v1"}` {
		t.Errorf("%d ops, origin %s", n, origin)
	}
	// A home record without a registered instance is not the current instance.
	h.plugins["home"].ClientID = ""
	if _, err := h.pluginOpEffect(ctx, "home", "k1", "v1", raw); err == nil {
		t.Error("an unregistered home record passed")
	}
}

// Two queued ops on different entries both complete: the entry base gives no false conflict, and
// each write keeps the other entry.
func TestPluginOpQueuedOps(t *testing.T) {
	h, c, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x", "y"))
	opsLoadEntries(t, h)
	h.plugins["x"] = &Plugin{Name: "x", entry: opsEntry(t, h, "managed:x")}
	h.plugins["y"] = &Plugin{Name: "y", entry: opsEntry(t, h, "managed:y")}
	yID, _ := json.Marshal(entryIdentity(opsEntry(t, h, "managed:y")))
	set := opsMust(t, h, "s", opRequest{Op: "settings", Entry: "managed:x", Input: json.RawMessage(`{"args":{"k":"v"}}`)})
	rem := opsMust(t, h, "r", opRequest{Op: "remove", Entry: "managed:y"})
	h.opsRun(context.Background())
	for _, id := range []string{set, rem} {
		if op := opsOf(t, h, id); op.Phase != "done" {
			t.Errorf("op %s: %+v", id, op)
		}
	}
	list, err := parseManaged(h.cfg.managedPath)
	if err != nil || len(list) != 1 || list[0].managed != "x" || list[0].Args["k"] != "v" {
		t.Fatalf("file %s: %v", opsFile(t, h.cfg.managedPath), err)
	}
	if got := strings.Join(c.list(), "; "); got != `reload; hub.admin.stop {"expect_entry":`+string(yID)+`,"plugin":"y"}; reload` {
		t.Errorf("calls %s", got)
	}
}

// A hand edit of the op's own entry after admission is a conflict, and nothing is written.
func TestPluginOpSameEntryEdit(t *testing.T) {
	h, c, dir := opsTestHub(t, "")
	pd := t.TempDir()
	writeManaged(t, dir, opsLocal(t, pd, "x"))
	opsLoadEntries(t, h)
	id := opsMust(t, h, "s", opRequest{Op: "settings", Entry: "managed:x", Input: json.RawMessage(`{"args":{"k":"v"}}`)})
	edited := `{"plugins":[{"id":"x","source":"` + filepath.Join(pd, "x") + `","args":{"k":"hand"}}]}`
	writeManaged(t, dir, edited)
	h.opsRun(context.Background())
	if op := opsOf(t, h, id); op.Phase != "conflict" || !strings.Contains(op.Error, "changed since the operation was admitted") {
		t.Errorf("op %+v", op)
	}
	if got := opsFile(t, h.cfg.managedPath); got != edited {
		t.Errorf("the file changed: %s", got)
	}
	if len(c.list()) != 0 {
		t.Errorf("calls %v", c.list())
	}
	// A reformat by hand of the same entry is not a change.
	h2, _, dir2 := opsTestHub(t, "")
	writeManaged(t, dir2, opsLocal(t, pd, "x"))
	opsLoadEntries(t, h2)
	id2 := opsMust(t, h2, "s", opRequest{Op: "settings", Entry: "managed:x", Input: json.RawMessage(`{"args":{"k":"v"}}`)})
	writeManaged(t, dir2, "{\n  \"plugins\": [ {\"source\": \""+filepath.Join(pd, "x")+"\", \"id\": \"x\", \"args\": {}} ]\n}\n")
	h2.opsRun(context.Background())
	if op := opsOf(t, h2, id2); op.Phase != "done" {
		t.Errorf("after a reformat: %+v", op)
	}
}

// A crash before the rename (the output hash stored, the file not written) and a crash after the
// rename (before the phase record), each followed by a hand edit of another entry: the next run
// writes once, keeps the other entry's edit, and never writes old bytes again.
func TestPluginOpWriteCrashRecovery(t *testing.T) {
	for _, point := range []string{"plugin-op-before-rename", "plugin-op-after-rename"} {
		t.Run(point, func(t *testing.T) {
			h, c, dir := opsTestHub(t, "")
			pd := t.TempDir()
			writeManaged(t, dir, opsLocal(t, pd, "x", "y"))
			opsLoadEntries(t, h)
			id := opsMust(t, h, "s", opRequest{Op: "settings", Entry: "managed:x", Input: json.RawMessage(`{"args":{"k":"v"}}`)})
			h.pops.fault = func(p string) bool { return p == point }
			h.opsRun(context.Background())
			op := opsOf(t, h, id)
			if op.Phase != "writing" || op.Output == "" {
				t.Fatalf("after the crash: %+v", op)
			}
			// Another writer changes y.
			list, _ := parseManaged(h.cfg.managedPath)
			for _, e := range list {
				if e.managed == "y" {
					e.Args = map[string]string{"y": "hand"}
				}
			}
			var f struct {
				Plugins []managedFileEntry `json:"plugins"`
			}
			for _, e := range list {
				f.Plugins = append(f.Plugins, fileEntry(e))
			}
			b, _ := json.Marshal(f)
			writeManaged(t, dir, string(b))
			h.pops.fault = nil
			h.opsRun(context.Background())
			if op := opsOf(t, h, id); op.Phase != "done" {
				t.Fatalf("after recovery: %+v", op)
			}
			got, err := parseManaged(h.cfg.managedPath)
			if err != nil || managedByID(got, "x").Args["k"] != "v" || managedByID(got, "y").Args["y"] != "hand" {
				t.Errorf("file %s: %v", opsFile(t, h.cfg.managedPath), err)
			}
			if calls := c.list(); len(calls) != 1 || calls[0] != "reload" {
				t.Errorf("calls %v", calls)
			}
		})
	}
}

// Remove of a plugin that another entry depends on is refused before the stop: the running
// dependency is not stopped, and the file is unchanged. An entry that is refused already does not
// block a remove of another entry.
func TestPluginOpRemoveDependent(t *testing.T) {
	h, c, dir := opsTestHub(t, "")
	pd := t.TempDir()
	plugDir(t, pd, "tasks", `{"name":"tasks","main":"m.lua"}`, "m.lua")
	plugDir(t, pd, "notes", `{"name":"notes","main":"m.lua","depends":["tasks"]}`, "m.lua")
	plugDir(t, pd, "broken", `{"name":"broken","main":"m.lua","depends":["absent"]}`, "m.lua")
	plugDir(t, pd, "free", `{"name":"free","main":"m.lua"}`, "m.lua")
	body := `{"plugins":[`
	for i, id := range []string{"tasks", "notes", "broken", "free"} {
		if i > 0 {
			body += ","
		}
		body += `{"id":"` + id + `","source":"` + filepath.Join(pd, id) + `"}`
	}
	writeManaged(t, dir, body+`]}`)
	opsLoadEntries(t, h)
	before := opsFile(t, h.cfg.managedPath)
	id := opsMust(t, h, "r", opRequest{Op: "remove", Entry: "managed:tasks"})
	h.opsRun(context.Background())
	if op := opsOf(t, h, id); op.Phase != "conflict" || !strings.Contains(op.Error, "managed:notes") {
		t.Errorf("op %+v", op)
	}
	if len(c.list()) != 0 || opsFile(t, h.cfg.managedPath) != before {
		t.Errorf("calls %v; the file changed: %v", c.list(), opsFile(t, h.cfg.managedPath) != before)
	}
	free := opsMust(t, h, "f", opRequest{Op: "remove", Entry: "managed:free"})
	h.opsRun(context.Background())
	if op := opsOf(t, h, free); op.Phase != "done" {
		t.Errorf("a remove beside a refused entry: %+v", op)
	}
}

// A crash after a lifecycle call and before its outcome: the next run observes and never calls
// again. A stop is observed from the stored record; a start's outcome stays unknown.
func TestPluginOpNoSecondCall(t *testing.T) {
	h, c, dir := opsTestHub(t, "")
	pd := t.TempDir()
	writeManaged(t, dir, opsLocal(t, pd, "x", "y"))
	opsLoadEntries(t, h)
	x := &Plugin{Name: "x", entry: opsEntry(t, h, "managed:x")}
	h.plugins["x"] = x
	h.plugins["y"] = &Plugin{Name: "y", entry: opsEntry(t, h, "managed:y")}
	xID, _ := json.Marshal(entryIdentity(x.entry))
	yID, _ := json.Marshal(entryIdentity(opsEntry(t, h, "managed:y")))
	stop := opsMust(t, h, "stop", opRequest{Op: "stop", Entry: "managed:x"})
	h.pops.fault = func(p string) bool { return p == "plugin-op-after-stop" }
	h.opsRun(context.Background())
	// The stop happened (the fake does not stop): the record of managed:x is saved stopped.
	if err := h.loadPluginRow(context.Background(), x); err != nil {
		t.Fatal(err)
	}
	x.Desired = "stopped"
	if err := h.savePlugin(context.Background(), x); err != nil {
		t.Fatal(err)
	}
	h.pops.fault = nil
	h.opsRun(context.Background())
	if op := opsOf(t, h, stop); op.Phase != "done" || !strings.Contains(op.Result, "observed") {
		t.Errorf("stop: %+v", op)
	}
	start := opsMust(t, h, "start", opRequest{Op: "start", Entry: "managed:y"})
	h.pops.fault = func(p string) bool { return p == "plugin-op-after-start" }
	h.opsRun(context.Background())
	h.pops.fault = nil
	h.opsRun(context.Background())
	if op := opsOf(t, h, start); op.Phase != "unknown" || !strings.Contains(op.Error, "outcome is unknown") || opsProof(t, h, start, "start") != "" {
		t.Errorf("start: %+v", op)
	}
	if got := strings.Join(c.list(), "; "); got != `hub.admin.stop {"expect_entry":`+string(xID)+`,"plugin":"x"}; hub.admin.start {"expect_entry":`+string(yID)+`,"expect_run":{"source":"`+filepath.Join(pd, "y")+`"},"plugin":"y"}` {
		t.Errorf("calls %s", got)
	}
}

// A shell command that holds the plugin command flocks makes the write wait; the op keeps its
// phase and completes after the release. A stop of the hub ends the manager during the wait.
func TestPluginOpBusyFlocks(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	release, err := lockCommands(h.cfg.State, lockPath(h.cfg.path, h.cfg.Lock))
	if err != nil {
		t.Fatal(err)
	}
	id := opsMust(t, h, "s", opRequest{Op: "settings", Entry: "managed:x", Input: json.RawMessage(`{"args":{"k":"v"}}`)})
	if wait := h.opsRun(context.Background()); wait != opBusyRetry {
		t.Errorf("wait %v", wait)
	}
	if op := opsOf(t, h, id); op.Phase != "writing" {
		t.Errorf("while busy: %+v", op)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.pops.kick = make(chan struct{}, 1)
	ended := make(chan struct{})
	go func() { h.opsLoop(ctx); close(ended) }()
	h.kickOps()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the manager did not end at the stop during a busy wait")
	}
	release()
	h.opsRun(context.Background())
	if op := opsOf(t, h, id); op.Phase != "done" {
		t.Errorf("after the release: %+v", op)
	}
}
