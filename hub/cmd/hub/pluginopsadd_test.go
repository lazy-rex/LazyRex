package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func userAdmit(h *Hub, key string, req opRequest) (map[string]any, error) {
	v, err := h.admitOp(context.Background(), opOrigin{"user", "view-1"}, "user", key, req)
	if err != nil {
		return nil, err
	}
	return v.(map[string]any), nil
}

func rawJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// add admission (step 4 C1): a GitHub source only; the default id (the last path element,
// lowercased) or an explicit id, stored with the op; no entry with that id, or with the same
// repository and subdir at any ref (the lock key, without case for owner and repository).
func TestPluginOpAddAdmission(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, `{"plugins":[{"id":"tools","source":"github:acme/tools@latest"}]}`)
	opsLoadEntries(t, h)
	add := func(in map[string]any) (map[string]any, error) {
		return userAdmit(h, "add-"+in["source"].(string)+"-"+strings.Repeat("x", len(in)), opRequest{Op: "add", Input: rawJSON(in)})
	}
	for _, c := range []struct {
		in   map[string]any
		code string
		text string
	}{
		{map[string]any{"source": "/abs/dir"}, "", "needs a GitHub source"},
		{map[string]any{"source": "github:acme/Foo.Bar@v1.0.0"}, "", `the default id "foo.bar"`},
		{map[string]any{"source": "github:acme/notes@v1", "id": "Bad"}, "", "must match"},
		{map[string]any{"source": "github:acme/notes@v1", "id": "tools"}, "conflict", "managed:tools exists"},
		{map[string]any{"source": "github:acme/tools@v2.0.0", "id": "t2"}, "conflict", "the same repository as managed:tools"},
		{map[string]any{"source": "github:ACME/Tools@main", "id": "t3"}, "conflict", "the same repository"},
		{map[string]any{"source": "github:acme/notes@v1", "extra": true}, "", "unknown field"},
	} {
		_, err := add(c.in)
		if err == nil || (c.code != "" && !isRefusal(err, c.code)) || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%v: %v", c.in, err)
		}
	}
	// The default id, and a subdir of the same repository (another lock key).
	v, err := add(map[string]any{"source": "github:acme/notes@latest"})
	if err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, v["op_id"].(string))
	if op.Entry != "managed:notes" || op.Base != "absent" || op.Name != "" || op.Phase != "queued" ||
		op.Input != `{"source":"github:acme/notes@latest","id":"notes","args":{},"watch_roots":[]}` ||
		op.EntryIdentity != `["managed:notes","github:acme/notes@latest"]` {
		t.Fatalf("the add op: %+v", op)
	}
	if _, err := add(map[string]any{"source": "github:acme/tools/sub/Deep@v1"}); err != nil {
		t.Fatalf("a subdir of the same repository: %v", err)
	}
	if op := opsOf(t, h, mustLastOp(t, h)); op.Entry != "managed:deep" {
		t.Fatalf("the default id of a subdir: %+v", op)
	}
	// An agent add is user_only, and the text tells the agent what to do (P1). An agent update is
	// admitted (B9 removed for update, the user's decision of 2026-10-23).
	_, err = opsAdmit(t, h, "a1", "agent-add", opRequest{Op: "add", Input: rawJSON(map[string]any{"source": "github:acme/x@v1"})})
	if !isRefusal(err, "user_only") || !strings.HasSuffix(err.Error(), ": add needs the user: ask the user to open LazyRex: Plugins…") {
		t.Errorf("agent add: %v", err)
	}
	for _, to := range []string{"latest", "@"} {
		v, err := opsAdmit(t, h, "a1", "agent-update-"+to, opRequest{Op: "update", Entry: "managed:tools", Input: rawJSON(map[string]any{"to": to})})
		if err != nil {
			t.Fatalf("agent update to %q: %v", to, err)
		}
		if op := opsOf(t, h, v["op_id"].(string)); op.Kind != "update" || op.Origin != (opOrigin{"agent", "a1"}) || op.Phase != "queued" {
			t.Errorf("agent update to %q: %+v", to, op)
		}
	}
}

func mustLastOp(t *testing.T, h *Hub) string {
	t.Helper()
	var id string
	if err := h.db.QueryRow(`SELECT op_id FROM plugin_ops ORDER BY created DESC, rowid DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// update admission: managed GitHub entries only; to is "latest" or a tag name, never a branch ref
// or a commit; the base is the entry at admission.
func TestPluginOpUpdateAdmission(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	local := opsLocal(t, t.TempDir(), "loc")
	writeManaged(t, dir, strings.Replace(local, `]}`, `,{"id":"tools","source":"github:acme/tools@v1.0.0"}]}`, 1))
	opsLoadEntries(t, h)
	update := func(entry, to string) error {
		_, err := userAdmit(h, "up-"+entry+"-"+to, opRequest{Op: "update", Entry: entry, Input: rawJSON(map[string]any{"to": to})})
		return err
	}
	if err := update("managed:loc", "latest"); err == nil || !strings.Contains(err.Error(), "needs a GitHub source") {
		t.Errorf("a local entry: %v", err)
	}
	for _, to := range []string{"", "refs/heads/main", "refs/tags/v1", strings.Repeat("a", 40), "v1 x", "v1@x"} {
		if err := update("managed:tools", to); err == nil || !strings.Contains(err.Error(), "latest") {
			t.Errorf("to %q: %v", to, err)
		}
	}
	// A refresh ("@") keeps the source: its lock base is the old key only.
	if err := update("managed:tools", "@"); err != nil {
		t.Fatalf("to @: %v", err)
	}
	if op := opsOf(t, h, mustLastOp(t, h)); op.Input != `{"to":"@"}` || op.LockBase != `{"github:acme/tools@v1.0.0":"absent"}` {
		t.Errorf("the refresh op: %+v", op)
	}
	for _, to := range []string{"latest", "v1.2.0", "release/2"} {
		if err := update("managed:tools", to); err != nil {
			t.Errorf("to %q: %v", to, err)
		}
	}
	op := opsOf(t, h, mustLastOp(t, h))
	if op.Kind != "update" || op.Name != "tools" || op.Input != `{"to":"release/2"}` ||
		op.Base != `{"id":"tools","source":"github:acme/tools@v1.0.0","args":{},"watch_roots":[]}` {
		t.Fatalf("the update op: %+v", op)
	}
}

// restart admission: any entry (managed or config), also from an agent; the plugin must run, and
// its candidate must be its run; the home plugin too; the target is the run and the
// instance at admission.
func TestPluginOpRestartAdmission(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	e := opsEntry(t, h, "managed:x")
	pl := &Plugin{Name: "x", entry: e}
	h.plugins["x"] = pl
	restart := func(key string) error {
		_, err := opsAdmit(t, h, "a1", key, opRequest{Op: "restart", Entry: "managed:x"})
		return err
	}
	if err := restart("r0"); !isRefusal(err, "refused") || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("not running: %v", err)
	}
	run := pluginRun{Name: "x", Source: e.Source}
	pl.ClientID, pl.ChildPID, pl.ChildStart, pl.InstanceID, pl.run = "client:x", os.Getpid(), startTime(os.Getpid()), "x-1-aaaaaaaa", run
	other := run
	other.Source = "/elsewhere"
	h.entriesMu.Lock()
	e.cand = &other
	h.entriesMu.Unlock()
	if err := restart("r1"); !isRefusal(err, "candidate_changed") || !strings.Contains(err.Error(), "use update") {
		t.Fatalf("another candidate: %v", err)
	}
	h.entriesMu.Lock()
	e.cand = &run
	h.entriesMu.Unlock()
	if err := restart("r2"); err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, mustLastOp(t, h))
	var x opRestartTarget
	json.Unmarshal([]byte(op.Target), &x)
	if op.Kind != "restart" || x.Instance != "x-1-aaaaaaaa" || x.Source != e.Source || op.Origin.Kind != "agent" {
		t.Fatalf("the restart op: %+v %+v", op, x)
	}
	// The home plugin restarts like any entry (lead-b8-step4: restart of any entry); a stop and a
	// remove of it stay refused.
	h.cfg.Home = "x"
	if err := restart("r3"); err != nil {
		t.Fatalf("the home plugin: %v", err)
	}
}

// The manager's tables on a new store: every column comes from the CREATE TABLE (no release had
// these tables, so there is no migration), and a second run changes nothing.
func TestPluginOpsSchema(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	columns := func() string {
		t.Helper()
		rows, err := h.db.Query(`SELECT name FROM pragma_table_info('plugin_ops') ORDER BY cid`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			names = append(names, n)
		}
		return strings.Join(names, " ")
	}
	for i := 0; i < 2; i++ {
		if err := h.pluginOpsSchemaUp(ctx); err != nil {
			t.Fatalf("schema run %d: %v", i+1, err)
		}
		want := "op_id scope key fingerprint origin kind entry name expect input target phase rev confirmed base output entry_identity result error created updated pin lock_base row_prior lock_old"
		if got := columns(); got != want {
			t.Fatalf("schema run %d: plugin_ops columns %q, want %q", i+1, got, want)
		}
	}
}

// An add admission reads the hub's entries while a reload updates them in place (updateManaged
// under entriesMu): every read of a shared entry is under entriesMu. -race finds a read outside it.
func TestPluginOpAddAdmissionReloadRace(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, `{"plugins":[]}`)
	first, second := &configEntry{Source: "github:acme/tools@v1"}, &configEntry{Source: "github:acme/tools@v2"}
	for _, e := range []*configEntry{first, second} {
		if err := e.checkSource(""); err != nil {
			t.Fatal(err)
		}
		e.managed = "tools"
	}
	h.entriesMu.Lock()
	h.entries = []*configEntry{first}
	h.entriesMu.Unlock()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.entriesMu.Lock()
			updateManaged(first, second)
			h.entriesMu.Unlock()
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for i := range 200 {
		// The same repository: the conflict text reads the shared entry too.
		if _, err := userAdmit(h, fmt.Sprintf("race-%d", i), opRequest{Op: "add", Input: rawJSON(map[string]any{"source": "github:acme/tools@v3", "id": "t2"})}); !isRefusal(err, "conflict") {
			t.Fatalf("the same repository: %v", err)
		}
		if _, err := userAdmit(h, fmt.Sprintf("race-ok-%d", i), opRequest{Op: "add", Input: rawJSON(map[string]any{"source": "github:acme/notes@v1", "id": fmt.Sprintf("n%d", i)})}); err != nil {
			t.Fatal(err)
		}
	}
}

// resolveOp admits req and runs its resolving phase only.
func resolveOp(t *testing.T, h *Hub, key string, req opRequest) (*pluginOp, error) {
	t.Helper()
	v, err := userAdmit(h, key, req)
	if err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, v["op_id"].(string))
	op.Phase = "resolving"
	return op, h.opResolve(context.Background(), op)
}

// The resolving phase at a remote: the target source and commit of each add and update form.
func TestPluginOpResolveTargets(t *testing.T) {
	r := newTestRepo(t)
	r.commit(map[string]string{"a": "1"})
	r.git("tag", "-a", "-m", "v1", "v1.0.0")
	v1 := r.git("rev-parse", "v1.0.0^{}")
	r.git("branch", "only-branch")
	c2 := r.commit(map[string]string{"a": "2"})
	r.git("tag", "v1.1.0")
	r.git("branch", "both")
	r.git("tag", "both")
	h, _, dir := opsTestHub(t, "")
	h.pops.cmd = pluginCmdOpts{remote: func(owner, repo string) string { return r.url() }}
	writeManaged(t, dir, `{"plugins":[{"id":"tools","source":"github:acme/tools/sub@v1.0.0"},{"id":"br","source":"github:acme/br@main"},{"id":"lt","source":"github:acme/lt@latest"}]}`)
	opsLoadEntries(t, h)
	add := func(src string) opRequest { return opRequest{Op: "add", Input: rawJSON(map[string]any{"source": src})} }
	update := func(to string) opRequest {
		return opRequest{Op: "update", Entry: "managed:tools", Input: rawJSON(map[string]any{"to": to})}
	}
	refresh := func(id string) opRequest {
		return opRequest{Op: "update", Entry: "managed:" + id, Input: rawJSON(map[string]any{"to": "@"})}
	}
	for i, c := range []struct {
		req  opRequest
		want opResolved
	}{
		{add("github:acme/a@latest"), opResolved{Source: "github:acme/a@latest", RefKind: "latest", Tag: "v1.1.0", Commit: c2, FetchRef: "refs/tags/v1.1.0"}},
		{add("github:acme/b@main"), opResolved{Source: "github:acme/b@main", RefKind: "branch", Commit: c2, FetchRef: "refs/heads/main"}},
		{add("github:acme/c@v1.0.0"), opResolved{Source: "github:acme/c@v1.0.0", RefKind: "tag", Commit: v1, FetchRef: "refs/tags/v1.0.0"}},
		{add("github:acme/d@" + v1), opResolved{Source: "github:acme/d@" + v1, RefKind: "commit", Commit: v1, FetchRef: v1}},
		{update("latest"), opResolved{Source: "github:acme/tools/sub@latest", RefKind: "latest", Tag: "v1.1.0", Commit: c2, FetchRef: "refs/tags/v1.1.0"}},
		{update("v1.1.0"), opResolved{Source: "github:acme/tools/sub@v1.1.0", RefKind: "tag", Commit: c2, FetchRef: "refs/tags/v1.1.0"}},
		// A tag that a branch also has: the full tag ref, so a CLI sync reads the tag.
		{update("both"), opResolved{Source: "github:acme/tools/sub@refs/tags/both", RefKind: "tag", Commit: c2, FetchRef: "refs/tags/both"}},
		// A refresh resolves the entry's own ref: a tag gives its commit, a branch its tip, latest
		// the newest release. The source does not change.
		{refresh("tools"), opResolved{Source: "github:acme/tools/sub@v1.0.0", RefKind: "tag", Commit: v1, FetchRef: "refs/tags/v1.0.0"}},
		{refresh("br"), opResolved{Source: "github:acme/br@main", RefKind: "branch", Commit: c2, FetchRef: "refs/heads/main"}},
		{refresh("lt"), opResolved{Source: "github:acme/lt@latest", RefKind: "latest", Tag: "v1.1.0", Commit: c2, FetchRef: "refs/tags/v1.1.0"}},
	} {
		op, err := resolveOp(t, h, fmt.Sprintf("res-%d", i), c.req)
		var got opResolved
		json.Unmarshal([]byte(op.Target), &got)
		if err != nil || got != c.want || op.Phase != "fetching" || opsOf(t, h, op.ID).Target != op.Target {
			t.Errorf("%d: %+v phase %s %v, want %+v", i, got, op.Phase, err, c.want)
		}
	}
	// No such tag, a branch-only name, and an add ref that is not found: failed, nothing stored.
	for i, c := range []struct {
		req  opRequest
		text string
	}{
		{update("v9.0.0"), "not a tag: v9.0.0"},
		{update("only-branch"), "not a tag: only-branch"},
		{add("github:acme/e@nosuch"), "ref nosuch is not found"},
		{add("github:acme/f@bad..ref"), "bad..ref is not a valid ref"},
	} {
		op, err := resolveOp(t, h, fmt.Sprintf("fail-%d", i), c.req)
		if stored := opsOf(t, h, op.ID); err != nil || stored.Phase != "failed" || stored.Target != "" ||
			!strings.Contains(stored.Error, c.text) || !strings.Contains(stored.Error, "nothing was written") {
			t.Errorf("%d: %+v %v", i, stored, err)
		}
	}
}

// The first STORED target is the decision (lead ruling 4). A crash after the remote read and
// before the store: the replay resolves again and takes the new answer. A stored target: the
// replay makes no resolver call.
func TestPluginOpResolveStoredTarget(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	calls := 0
	h.pops.resolve = func(ctx context.Context, op *pluginOp) (opResolved, error) {
		calls++
		return opResolved{Source: "github:acme/notes@latest", RefKind: "latest", Tag: fmt.Sprintf("v1.%d.0", calls), Commit: strings.Repeat(fmt.Sprint(calls), 40)}, nil
	}
	crash := true
	h.pops.fault = func(point string) bool { return crash && point == "plugin-op-after-resolve" }
	v, err := userAdmit(h, "a", opRequest{Op: "add", Input: rawJSON(map[string]any{"source": "github:acme/notes@latest"})})
	if err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, v["op_id"].(string))
	op.Phase = "resolving"
	if err := h.opResolve(context.Background(), op); !errors.Is(err, errOpCrash) {
		t.Fatal(err)
	}
	if op := opsOf(t, h, op.ID); op.Target != "" || calls != 1 {
		t.Fatalf("the crash stored a target: %+v (calls %d)", op, calls)
	}
	crash = false
	op = opsOf(t, h, op.ID)
	op.Phase = "resolving"
	if err := h.opResolve(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	var got opResolved
	json.Unmarshal([]byte(opsOf(t, h, op.ID).Target), &got)
	if calls != 2 || got.Tag != "v1.2.0" {
		t.Fatalf("the replay after the crash: calls %d, target %+v", calls, got)
	}
	// A crash after the store and before the phase write: the replay keeps the stored target.
	if _, err := h.db.Exec(`UPDATE plugin_ops SET phase = 'resolving' WHERE op_id = ?`, op.ID); err != nil {
		t.Fatal(err)
	}
	op = opsOf(t, h, op.ID)
	if err := h.opResolve(context.Background(), op); err != nil || calls != 2 || op.Phase != "fetching" {
		t.Fatalf("the replay with a stored target: calls %d, %+v, %v", calls, op, err)
	}
}

// A hub stop during the resolve stores nothing and keeps the phase; a resolve that does not answer
// within the limit ends failed.
func TestPluginOpResolveStopAndLimit(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	h.pops.resolve = func(ctx context.Context, op *pluginOp) (opResolved, error) {
		<-ctx.Done()
		return opResolved{}, ctx.Err()
	}
	limit := resolveLimit
	t.Cleanup(func() { resolveLimit = limit })
	resolveLimit = time.Hour
	v, err := userAdmit(h, "a", opRequest{Op: "add", Input: rawJSON(map[string]any{"source": "github:acme/notes@latest"})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	op := opsOf(t, h, v["op_id"].(string))
	op.Phase = "resolving"
	h.db.Exec(`UPDATE plugin_ops SET phase = 'resolving' WHERE op_id = ?`, op.ID)
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := h.opResolve(ctx, op); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if op := opsOf(t, h, op.ID); op.Phase != "resolving" || op.Target != "" {
		t.Fatalf("a stop changed the op: %+v", op)
	}
	resolveLimit = 50 * time.Millisecond
	if err := h.opResolve(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if op := opsOf(t, h, op.ID); op.Phase != "failed" || !strings.Contains(op.Error, "no answer within 50ms; nothing was written") {
		t.Fatalf("the limit: %+v", op)
	}
}
