package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func peerAdmit(h *Hub, key string, req opRequest) (map[string]any, error) {
	v, err := h.admitOp(context.Background(), peerOrigin("mac"), "peer:mac", key, req)
	if err != nil {
		return nil, err
	}
	return v.(map[string]any), nil
}

// peerPin is a pin document entry of github:acme/<repo>@<ref> (pinsync.go), as a peer exports it.
func peerPin(repo, ref, kind, tag, commit, digest string) map[string]any {
	return map[string]any{"source": "github:acme/" + repo + "@" + ref, "name": repo, "owner": "acme", "repo": repo, "subdir": "",
		"ref": ref, "ref_kind": kind, "tag": tag, "commit": commit, "digest": digest}
}

// A peer update (peersync.go): only a peer gives a target, only on an update with no input; the
// target is a valid pin document entry of the same repository; it is the stored target, with the
// peer's digest and a fetch by ID, and the lock base has the target key and the old key.
func TestPeerUpdateAdmission(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, `{"plugins":[{"id":"tools","source":"github:acme/tools@v1.0.0"}]}`)
	opsLoadEntries(t, h)
	commit, digest := strings.Repeat("a", 40), "sha256:"+strings.Repeat("b", 64)
	good := peerPin("tools", "v1.1.0", "tag", "", commit, digest)
	delete(good, "tag")
	target := rawJSON(good)
	if _, err := userAdmit(h, "u1", opRequest{Op: "update", Entry: "managed:tools", Target: target}); err == nil || !strings.Contains(err.Error(), "only a peer sync") {
		t.Errorf("a user target: %v", err)
	}
	if _, err := peerAdmit(h, "p0", opRequest{Op: "update", Entry: "managed:tools", Input: rawJSON(map[string]any{"to": "v1.1.0"})}); err == nil || !strings.Contains(err.Error(), "only a peer sync") {
		t.Errorf("a peer update with no target: %v", err)
	}
	if _, err := peerAdmit(h, "p1", opRequest{Op: "start", Entry: "managed:tools", Target: target}); err == nil || !strings.Contains(err.Error(), "only a peer sync") {
		t.Errorf("a peer start: %v", err)
	}
	bads := map[string]map[string]any{
		"another repository":     peerPin("other", "v1.1.0", "tag", "", commit, digest),
		"a bad digest":           peerPin("tools", "v1.1.0", "tag", "", commit, "sha256:x"),
		"a latest without a tag": peerPin("tools", "latest", "latest", "", commit, digest),
	}
	for name, p := range bads {
		if p["tag"] == "" {
			delete(p, "tag")
		}
		if _, err := peerAdmit(h, "bad-"+name, opRequest{Op: "update", Entry: "managed:tools", Target: rawJSON(p)}); err == nil || !strings.Contains(err.Error(), "target") {
			t.Errorf("%s: %v", name, err)
		}
	}
	v, err := peerAdmit(h, "p2", opRequest{Op: "update", Entry: "managed:tools", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, v["op_id"].(string))
	var got opResolved
	json.Unmarshal([]byte(op.Target), &got)
	want := opResolved{Source: "github:acme/tools@v1.1.0", RefKind: "tag", Commit: commit, FetchRef: commit, Digest: digest, ByID: true}
	if got != want || op.Input != "" {
		t.Fatalf("the stored target %+v (input %q), want %+v", got, op.Input, want)
	}
	var base map[string]json.RawMessage
	json.Unmarshal([]byte(op.LockBase), &base)
	if _, ok := base["github:acme/tools@v1.1.0"]; !ok || len(base) != 2 {
		t.Errorf("the lock base keys: %s", op.LockBase)
	}
	// The same key with another target is another operation.
	other := peerPin("tools", "v1.2.0", "tag", "", commit, digest)
	delete(other, "tag")
	if _, err := peerAdmit(h, "p2", opRequest{Op: "update", Entry: "managed:tools", Target: rawJSON(other)}); !isRefusal(err, "operation_conflict") {
		t.Errorf("the same key, another target: %v", err)
	}
	// resolving does not resolve a stored target.
	op.Phase = "resolving"
	h.pops.resolve = func(context.Context, *pluginOp) (opResolved, error) {
		t.Fatal("a peer target resolved again")
		return opResolved{}, nil
	}
	if err := h.opResolve(context.Background(), op); err != nil || op.Phase != "fetching" {
		t.Fatalf("resolving: %+v %v", op, err)
	}
}

// A peer update fetches the peer's commit by its ID and must get the peer's digest: another digest
// fails with the lock and the managed file unchanged. With the digest, the op locks the peer's pin
// under the peer's source and writes that source into the managed entry.
func TestPeerUpdateFetch(t *testing.T) {
	h, r, _, _ := atTestHub(t, "github:acme/notes@v1.0.0")
	ctx := context.Background()
	c2 := r.commit(map[string]string{"lazyrex.json": manifestJSON("notes"), "main.lua": "return 2\n"})
	r.git("tag", "v1.1.0")
	other := fetchTestHub(t, r) // the peer: its own fetch gives the digest that it exports
	digest := opPinOf(t, fetchedAdd(t, other, "peer-add", "github:acme/notes@v1.1.0")).Digest

	peerOp := func(key, digest string) *pluginOp {
		t.Helper()
		p := peerPin("notes", "v1.1.0", "tag", "", c2, digest)
		delete(p, "tag")
		v, err := peerAdmit(h, key, opRequest{Op: "update", Entry: "managed:notes", Target: rawJSON(p)})
		if err != nil {
			t.Fatal(err)
		}
		op := opsOf(t, h, v["op_id"].(string))
		op.Phase = "resolving"
		if err := h.opResolve(ctx, op); err != nil {
			t.Fatal(err)
		}
		if err := h.opFetch(ctx, op); err != nil {
			t.Fatal(err)
		}
		return op
	}
	lockBytes, _ := os.ReadFile(opLockFile(h))
	managedBytes, _ := os.ReadFile(h.cfg.managedPath)
	bad := peerOp("wrong", "sha256:"+strings.Repeat("0", 64))
	if bad.Phase != "failed" || !strings.Contains(bad.Error, "not the peer's") {
		t.Fatalf("another digest: %+v", bad)
	}
	if b, _ := os.ReadFile(opLockFile(h)); string(b) != string(lockBytes) {
		t.Error("a failed peer fetch wrote the lock")
	}
	if b, _ := os.ReadFile(h.cfg.managedPath); string(b) != string(managedBytes) {
		t.Error("a failed peer fetch wrote the managed file")
	}

	op := peerOp("right", digest)
	if op.Phase != "locking" {
		t.Fatalf("the fetch: %+v", op)
	}
	if err := h.opLock(ctx, op); err != nil || op.Phase != "writing" {
		t.Fatalf("the lock write: %+v %v", op, err)
	}
	le := readTestLock(t, h).Plugins["github:acme/notes@v1.1.0"]
	if le == nil || le.Commit != c2 || le.Digest != digest || le.RefKind != "tag" {
		t.Fatalf("the lock entry %+v", le)
	}
	if err := h.opWrite(ctx, op); err != nil || op.Phase != "reloading" {
		t.Fatalf("the managed write: %+v %v", op, err)
	}
	_, list, err := readManaged(h.cfg.managedPath)
	if m := managedByID(list, "notes"); err != nil || m == nil || m.Source != "github:acme/notes@v1.1.0" {
		t.Fatalf("the managed entry: %+v %v", m, err)
	}
}

func opPinOf(t *testing.T, op *pluginOp) *lockEntry {
	t.Helper()
	var le lockEntry
	if err := json.Unmarshal([]byte(op.Pin), &le); err != nil {
		t.Fatalf("the pin of %+v: %v", op, err)
	}
	return &le
}

// peerSyncHub links hub b ("linux", plugins_from the peer "mac") to the action handler of the peer:
// a peer call runs the action in a's dispatch.
func peerSyncLink(t *testing.T, a, b *Hub) {
	t.Helper()
	a.cfg.HubName, a.cfg.Peers = "mac", []PeerConfig{{Name: "linux", Server: "lab-linux"}}
	b.cfg.HubName, b.cfg.Peers = "linux", []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	a.peers, b.peers = newPeerSet(a.cfg), newPeerSet(b.cfg)
	b.peers.call = func(ctx context.Context, _ PeerConfig, action string, args any) ([]byte, error) {
		raw, _ := json.Marshal(args)
		res, err := a.dispatch(ctx, perform{Action: action, Args: raw})
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)
	}
}

// peerSourceHub is the source hub of a sync: a managed entry notes at src, fetched and locked, and
// the config file that lazyrex-hub plugin export reads.
func peerSourceHub(t *testing.T, r *testRepo, src string) *Hub {
	t.Helper()
	a := fetchTestHub(t, r)
	add := fetchedAdd(t, a, "src-add", src)
	if err := a.opLock(context.Background(), add); err != nil || add.Phase != "writing" {
		t.Fatalf("the lock write: %+v %v", add, err)
	}
	writeManaged(t, a.cfg.State, `{"plugins":[{"id":"notes","source":"`+src+`"}]}`)
	opsLoadEntries(t, a)
	if err := os.WriteFile(a.cfg.path, []byte(`{"state":`+string(rawJSON(a.cfg.State))+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return a
}

// hub.peer.plugins gives the pin document of the source hub and its managed GitHub entries; only a
// configured peer calls it.
func TestPeerPluginsServe(t *testing.T) {
	r, _ := fetchTestRepo(t)
	a := peerSourceHub(t, r, "github:acme/notes@v1.0.0")
	b, _, _ := opsTestHub(t, "")
	peerSyncLink(t, a, b)
	ctx := context.Background()
	if _, err := a.dispatch(ctx, perform{Action: "hub.peer.plugins", Args: rawJSON(map[string]any{"hub": "nobody"})}); !isRefusal(err, "unknown_hub") {
		t.Fatalf("an unknown hub: %v", err)
	}
	out, err := b.peerCall(ctx, b.cfg.Peers[0], "hub.peer.plugins", map[string]any{"hub": "linux"})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Pins    json.RawMessage `json:"pins"`
		Managed []peerManaged   `json:"managed"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	doc, err := decodePinDoc(res.Pins)
	if err != nil {
		t.Fatal(err)
	}
	le := readTestLock(t, a).Plugins["github:acme/notes@v1.0.0"]
	if len(doc.Entries) != 1 || doc.Entries[0].Source != "github:acme/notes@v1.0.0" || doc.Entries[0].lockPin != le.lockPin {
		t.Errorf("the pins: %+v, the lock entry %+v", doc.Entries, le)
	}
	if len(res.Managed) != 1 || res.Managed[0] != (peerManaged{ID: "notes", Source: "github:acme/notes@v1.0.0"}) {
		t.Errorf("the managed entries: %+v", res.Managed)
	}
}

// One sync with a peer document: a managed entry of the same repository at another pin gets a peer
// update op; a source that this hub does not have is offered with the peer's id; a config entry is
// skipped; a pin that the lock has is in sync. A later sync makes no second op while the op runs,
// waits 5 minutes after the same target ended, and goes on at once for another target.
func TestPeerPluginsSync(t *testing.T) {
	b, _, _, _ := atTestHub(t, "github:acme/notes@v1.0.0")
	ctx := context.Background()
	cfgEntry := &configEntry{Source: "github:acme/cfg@v1"}
	if err := cfgEntry.checkSource(""); err != nil {
		t.Fatal(err)
	}
	b.entriesMu.Lock()
	b.entries = append(b.entries, cfgEntry)
	b.entriesMu.Unlock()
	cur := readTestLock(t, b).Plugins["github:acme/notes@v1.0.0"]
	commit, digest := strings.Repeat("c", 40), "sha256:"+strings.Repeat("d", 64)
	pin := func(src, repo, ref string, c, d string) pinEntry {
		return pinEntry{Source: src, lockPin: lockPin{Name: repo, Owner: "acme", Repo: repo, Ref: ref, RefKind: "tag", Commit: c, Digest: d}}
	}
	var entries []pinEntry
	b.cfg.HubName, b.cfg.Peers = "linux", []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	b.peers = newPeerSet(b.cfg)
	b.peers.call = func(ctx context.Context, _ PeerConfig, action string, args any) ([]byte, error) {
		if action != "hub.peer.plugins" || args.(map[string]any)["hub"] != "linux" {
			t.Errorf("the peer call %s %v", action, args)
		}
		doc, err := encodePinDoc(&pinDoc{Hub: strings.Repeat("a", 32), Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		return json.Marshal(map[string]any{"pins": json.RawMessage(doc), "managed": []peerManaged{{ID: "newp", Source: "github:acme/newp@v1"}}})
	}
	l := b.peerLink("mac")
	sync := func() *peerSync {
		t.Helper()
		s, err := b.peerPluginsSync(ctx, l)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	ops := func() []*pluginOp {
		t.Helper()
		rows, err := b.db.Query(`SELECT op_id FROM plugin_ops WHERE scope = 'peer:mac' ORDER BY created, rowid`)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for rows.Next() {
			var id string
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		var list []*pluginOp
		for _, id := range ids {
			list = append(list, opsOf(t, b, id))
		}
		return list
	}

	// In sync: the lock has the peer's pin.
	entries = []pinEntry{{Source: "github:acme/notes@v1.0.0", lockPin: cur.lockPin}}
	if s := sync(); len(s.Updating) != 0 || len(ops()) != 0 {
		t.Fatalf("in sync: %+v, ops %d", s, len(ops()))
	}

	v11 := pin("github:acme/notes@v1.1.0", "notes", "v1.1.0", commit, digest)
	entries = []pinEntry{v11, pin("github:acme/cfg@v2", "cfg", "v2", commit, digest), pin("github:acme/newp@v1", "newp", "v1", commit, digest)}
	s := sync()
	list := ops()
	if len(list) != 1 || list[0].Kind != "update" || list[0].Entry != "managed:notes" || list[0].Origin != peerOrigin("mac") {
		t.Fatalf("the ops: %+v", list)
	}
	var got opResolved
	json.Unmarshal([]byte(list[0].Target), &got)
	if got.Source != v11.Source || got.Commit != commit || got.Digest != digest || !got.ByID {
		t.Errorf("the stored target %+v", got)
	}
	if len(s.Updating) != 1 || s.Updating[0]["op_id"] != list[0].ID {
		t.Errorf("updating: %+v", s.Updating)
	}
	if len(s.Offered) != 1 || s.Offered[0] != (peerManaged{ID: "newp", Source: "github:acme/newp@v1"}) {
		t.Errorf("offered: %+v", s.Offered)
	}
	if len(s.Skipped) != 1 || s.Skipped[0].Source != "github:acme/cfg@v2" || s.Skipped[0].Entry != "github:acme/cfg@v1" {
		t.Errorf("skipped: %+v", s.Skipped)
	}

	// The op runs: no second op.
	if s := sync(); len(ops()) != 1 || len(s.Updating) != 1 || s.Updating[0]["op_id"] != list[0].ID {
		t.Fatalf("a running op: %+v, ops %d", s.Updating, len(ops()))
	}
	// The op failed just now: the same target waits.
	mustExec(t, b, `UPDATE plugin_ops SET phase = 'failed', updated = ? WHERE op_id = ?`, time.Now().UnixMilli(), list[0].ID)
	if s := sync(); len(ops()) != 1 || len(s.Updating) != 0 {
		t.Fatalf("a recent failure: %+v, ops %d", s.Updating, len(ops()))
	}
	// After 5 minutes the same target is a new op with the next count.
	mustExec(t, b, `UPDATE plugin_ops SET updated = ? WHERE op_id = ?`, time.Now().Add(-peerSyncRetry-time.Second).UnixMilli(), list[0].ID)
	sync()
	if list = ops(); len(list) != 2 {
		t.Fatalf("a retry after the wait: ops %d", len(list))
	}
	var keys []string
	for _, op := range list {
		var key string
		b.db.QueryRow(`SELECT key FROM plugin_ops WHERE op_id = ?`, op.ID).Scan(&key)
		keys = append(keys, key)
	}
	if !strings.HasSuffix(keys[0], "#1") || !strings.HasSuffix(keys[1], "#2") || strings.TrimSuffix(keys[0], "#1") != strings.TrimSuffix(keys[1], "#2") {
		t.Errorf("the keys %v", keys)
	}
	// Another target goes on at once after an op ended.
	mustExec(t, b, `UPDATE plugin_ops SET phase = 'failed', updated = ? WHERE op_id = ?`, time.Now().UnixMilli(), list[1].ID)
	entries = []pinEntry{pin("github:acme/notes@v1.2.0", "notes", "v1.2.0", commit, digest)}
	sync()
	if list = ops(); len(list) != 3 {
		t.Fatalf("another target: ops %d", len(list))
	}
}

// A sync from the source hub's real export: the receiver's op fetches the peer's commit, gets the
// peer's digest, and locks the peer's pin.
func TestPeerPluginsSyncFetch(t *testing.T) {
	b, r, _, _ := atTestHub(t, "github:acme/notes@v1.0.0")
	c2 := r.commit(map[string]string{"lazyrex.json": manifestJSON("notes"), "main.lua": "return 2\n"})
	r.git("tag", "v1.1.0")
	a := peerSourceHub(t, r, "github:acme/notes@v1.1.0")
	peerSyncLink(t, a, b)
	ctx := context.Background()
	s, err := b.peerPluginsSync(ctx, b.peerLink("mac"))
	if err != nil || len(s.Updating) != 1 {
		t.Fatalf("the sync: %+v %v", s, err)
	}
	op := opsOf(t, b, s.Updating[0]["op_id"].(string))
	op.Phase = "resolving"
	for _, phase := range []func(context.Context, *pluginOp) error{b.opResolve, b.opFetch, b.opLock} {
		if err := phase(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	want := readTestLock(t, a).Plugins["github:acme/notes@v1.1.0"]
	le := readTestLock(t, b).Plugins["github:acme/notes@v1.1.0"]
	if op.Phase != "writing" || le == nil || le.Commit != c2 || le.lockPin != want.lockPin {
		t.Fatalf("after the lock: %+v, the lock entry %+v, the peer's %+v", op, le, want)
	}
}
