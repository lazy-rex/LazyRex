package main

import (
	"context"
	"encoding/json"
	"errors"
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
	link := func(to *Hub) func(context.Context, PeerConfig, string, any) ([]byte, error) {
		return func(ctx context.Context, _ PeerConfig, action string, args any) ([]byte, error) {
			raw, _ := json.Marshal(args)
			res, err := to.dispatch(ctx, perform{Action: action, Args: raw})
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		}
	}
	// a calls b too: a's lock write nudges its peers (hub.peer.nudge what "plugins").
	a.peers.call, b.peers.call = link(b), link(a)
}

// hub.peer.nudge what "plugins" kicks the plugin sync of a plugins_from link only; no what kicks the
// pull; another what is invalid.
func TestPeerNudgePlugins(t *testing.T) {
	b := peerHub(t, "linux", "mac", nil)
	b.cfg.Peers = []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}, {Name: "pi", Server: "lab-pi"}}
	b.peers = newPeerSet(b.cfg)
	mac, pi := b.peers.links["mac"], b.peers.links["pi"]
	if _, err := b.peerNudge(map[string]any{"hub": "mac", "what": "plugins"}); err != nil || len(mac.syncKick) != 1 || len(mac.kick) != 0 {
		t.Fatalf("plugins: %v, sync kicks %d, pull kicks %d", err, len(mac.syncKick), len(mac.kick))
	}
	if _, err := b.peerNudge(map[string]any{"hub": "mac"}); err != nil || len(mac.kick) != 1 {
		t.Fatalf("no what: %v, pull kicks %d", err, len(mac.kick))
	}
	if _, err := b.peerNudge(map[string]any{"hub": "pi", "what": "plugins"}); err != nil || len(pi.syncKick) != 0 || len(pi.kick) != 0 {
		t.Fatalf("not plugins_from: %v, %d %d", err, len(pi.syncKick), len(pi.kick))
	}
	if _, err := b.peerNudge(map[string]any{"hub": "mac", "what": "agents"}); err == nil || !strings.Contains(err.Error(), "what must be") {
		t.Fatalf("another what: %v", err)
	}
}

// A lock write of the plugin manager nudges each peer with what "plugins"; the receiver's plugins
// loop syncs at once on it, not after its minute.
func TestLockWriteNudgesPeers(t *testing.T) {
	r, _ := fetchTestRepo(t)
	a := fetchTestHub(t, r)
	a.cfg.HubName, a.cfg.Peers = "mac", []PeerConfig{{Name: "linux", Server: "lab-linux"}}
	a.peers = newPeerSet(a.cfg)
	nudged := make(chan map[string]any, 4)
	a.peers.call = func(_ context.Context, _ PeerConfig, action string, args any) ([]byte, error) {
		if action == "hub.peer.nudge" {
			var m map[string]any
			json.Unmarshal(rawJSON(args), &m)
			nudged <- m
		}
		return []byte(`{}`), nil
	}
	add := fetchedAdd(t, a, "nudge-add", "github:acme/notes@v1.0.0")
	select {
	case m := <-nudged:
		t.Fatalf("a nudge before the lock write: %v", m)
	default:
	}
	if err := a.opLock(context.Background(), add); err != nil || add.Phase != "writing" {
		t.Fatalf("the lock write: %+v %v", add, err)
	}
	select {
	case m := <-nudged:
		if m["hub"] != "mac" || m["what"] != "plugins" {
			t.Fatalf("the nudge %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no nudge after the lock write")
	}

	// The receiver: its plugins loop syncs at the start, then at once on the nudge.
	b := peerHub(t, "linux", "mac", nil)
	b.cfg.Peers = []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	b.peers = newPeerSet(b.cfg)
	syncs := make(chan struct{}, 4)
	b.peers.call = func(context.Context, PeerConfig, string, any) ([]byte, error) {
		syncs <- struct{}{}
		return nil, errors.New("exit status 1: dial: connection refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.peerPluginsLoop(ctx, b.peers.links["mac"])
	<-syncs // the first sync
	if _, err := b.peerNudge(map[string]any{"hub": "mac", "what": "plugins"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-syncs:
	case <-time.After(5 * time.Second):
		t.Fatal("no sync after the nudge")
	}
}

// peerSourceHub is the source hub of a sync: a managed entry notes at src, fetched and locked, and
// the config file that pluginExport reads.
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
	endFixtureOps(t, b)
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
		doc, err := encodePinDoc(&pinDoc{Entries: entries})
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
	newp := peerOffer{Peer: "mac", ID: "newp", Source: "github:acme/newp@v1", Pin: pin("github:acme/newp@v1", "newp", "v1", commit, digest)}
	if len(s.Offered) != 1 || s.Offered[0] != newp {
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

// A peer that still serves the old document (with the pull identity "hub", claude-48): the receiver
// refuses the document as an unknown field "hub". The managed file and the lock stay byte-unchanged,
// no op is admitted, and plugin_sync.last_error shows the refusal.
func TestPeerPluginsSyncOldDocument(t *testing.T) {
	b, _, _, _ := atTestHub(t, "github:acme/notes@v1.0.0")
	endFixtureOps(t, b)
	b.cfg.HubName, b.cfg.Peers = "linux", []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	b.peers = newPeerSet(b.cfg)
	// A pin at another commit: the new document would admit an update op for it.
	p := pinEntry{Source: "github:acme/notes@v1.1.0", lockPin: lockPin{Name: "notes", Owner: "acme", Repo: "notes", Ref: "v1.1.0", RefKind: "tag", Commit: strings.Repeat("c", 40), Digest: "sha256:" + strings.Repeat("d", 64)}}
	pins, err := json.Marshal(map[string]any{"version": 2, "hub": "00112233445566778899aabbccddeeff", "entries": []pinEntry{p}})
	if err != nil {
		t.Fatal(err)
	}
	b.peers.call = func(ctx context.Context, _ PeerConfig, action string, args any) ([]byte, error) {
		return json.Marshal(map[string]any{"pins": json.RawMessage(pins), "managed": []peerManaged{}})
	}
	lockBefore, err := os.ReadFile(opLockFile(b))
	if err != nil {
		t.Fatal(err)
	}
	managedBefore := managedText(t, b)
	l := b.peerLink("mac")
	s, err := b.peerPluginsSync(context.Background(), l)
	b.peerSyncApply(l, s, err)
	want := `hub.peer.plugins on mac: the pin document: json: unknown field "hub"`
	if s != nil || err == nil || err.Error() != want {
		t.Fatalf("the sync: %+v %v, want %q", s, err, want)
	}
	if l.sync.LastError != want || l.sync.LastOK != nil {
		t.Errorf("last_error %q, last_ok %v", l.sync.LastError, l.sync.LastOK)
	}
	var n int
	b.db.QueryRow(`SELECT COUNT(*) FROM plugin_ops WHERE scope = 'peer:mac'`).Scan(&n)
	if n != 0 {
		t.Errorf("%d peer ops", n)
	}
	if after, _ := os.ReadFile(opLockFile(b)); string(after) != string(lockBefore) {
		t.Errorf("the lock changed:\n%s", after)
	}
	if after := managedText(t, b); after != managedBefore {
		t.Errorf("the managed file changed:\n%s", after)
	}
}

// A sync from the source hub's real export: the receiver's op fetches the peer's commit, gets the
// peer's digest, and locks the peer's pin.
func TestPeerPluginsSyncFetch(t *testing.T) {
	b, r, _, _ := atTestHub(t, "github:acme/notes@v1.0.0")
	endFixtureOps(t, b)
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

// The in-sync check compares the whole pin: a latest source at the same commit and digest under
// another release tag is not in sync.
func TestPeerPluginsSyncLatestTag(t *testing.T) {
	b, _, _, _ := atTestHub(t, "github:acme/notes@latest")
	endFixtureOps(t, b)
	cur := readTestLock(t, b).Plugins["github:acme/notes@v1.0.0"]
	lock := readTestLock(t, b)
	le := *cur
	le.Ref, le.RefKind, le.Tag = "latest", "latest", "v1.0.0"
	lock.Plugins["github:acme/notes@latest"] = &le
	writeTestLock(t, b, lock)
	var tag string
	b.cfg.HubName, b.cfg.Peers = "linux", []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	b.peers = newPeerSet(b.cfg)
	b.peers.call = func(context.Context, PeerConfig, string, any) ([]byte, error) {
		p := pinEntry{Source: "github:acme/notes@latest", lockPin: le.lockPin}
		p.Tag = tag
		doc, err := encodePinDoc(&pinDoc{Entries: []pinEntry{p}})
		if err != nil {
			t.Fatal(err)
		}
		return json.Marshal(map[string]any{"pins": json.RawMessage(doc), "managed": []peerManaged{}})
	}
	for _, c := range []struct {
		tag string
		ops int
	}{{"v1.0.0", 0}, {"v1.0.1", 1}} {
		tag = c.tag
		if _, err := b.peerPluginsSync(context.Background(), b.peerLink("mac")); err != nil {
			t.Fatal(err)
		}
		var n int
		b.db.QueryRow(`SELECT count(*) FROM plugin_ops WHERE scope = 'peer:mac'`).Scan(&n)
		if n != c.ops {
			t.Fatalf("the peer tag %s: %d ops, want %d", c.tag, n, c.ops)
		}
	}
}

// A pin that the lock and the managed entry have is in sync only while the plugin's record is at
// it: a running plugin whose selected run is another commit gets a peer update (alreadyAt).
func TestPeerPluginsSyncRunBehind(t *testing.T) {
	b, _, _, _ := atTestHub(t, "github:acme/notes@v1.0.0")
	endFixtureOps(t, b)
	cur := readTestLock(t, b).Plugins["github:acme/notes@v1.0.0"]
	b.entriesMu.Lock()
	id := entryIdentity(b.entries[len(b.entries)-1])
	b.entriesMu.Unlock()
	b.cfg.HubName, b.cfg.Peers = "linux", []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	b.peers = newPeerSet(b.cfg)
	b.peers.call = func(context.Context, PeerConfig, string, any) ([]byte, error) {
		doc, err := encodePinDoc(&pinDoc{Entries: []pinEntry{{Source: "github:acme/notes@v1.0.0", lockPin: cur.lockPin}}})
		if err != nil {
			t.Fatal(err)
		}
		return json.Marshal(map[string]any{"pins": json.RawMessage(doc), "managed": []peerManaged{}})
	}
	for _, c := range []struct {
		commit string
		ops    int
	}{{cur.Commit, 0}, {strings.Repeat("0", 40), 1}} {
		sel, _ := json.Marshal(selectedRun{Entry: id, Run: pluginRun{Name: "notes", Source: "github:acme/notes@v1.0.0", Commit: c.commit, Digest: cur.Digest}})
		mustExec(t, b, `INSERT INTO plugins (name, desired, selected_run) VALUES ('notes', 'running', ?) ON CONFLICT (name) DO UPDATE SET desired = 'running', selected_run = excluded.selected_run`, string(sel))
		if _, err := b.peerPluginsSync(context.Background(), b.peerLink("mac")); err != nil {
			t.Fatal(err)
		}
		var n int
		b.db.QueryRow(`SELECT count(*) FROM plugin_ops WHERE scope = 'peer:mac'`).Scan(&n)
		if n != c.ops {
			t.Fatalf("a selected run at %s: %d ops, want %d", c.commit, n, c.ops)
		}
	}
}

// A peer update waits while any op on the entry runs, of any scope (an install, a user's or an
// agent's update): the sync admits no peer op, and lists the open op as updating.
func TestPeerPluginsSyncWaitsForAnyOp(t *testing.T) {
	b, _, _, _ := atTestHub(t, "github:acme/notes@v1.0.0")
	endFixtureOps(t, b)
	commit, digest := strings.Repeat("c", 40), "sha256:"+strings.Repeat("d", 64)
	b.cfg.HubName, b.cfg.Peers = "linux", []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	b.peers = newPeerSet(b.cfg)
	b.peers.call = func(context.Context, PeerConfig, string, any) ([]byte, error) {
		p := pinEntry{Source: "github:acme/notes@v1.1.0", lockPin: lockPin{Name: "notes", Owner: "acme", Repo: "notes", Ref: "v1.1.0", RefKind: "tag", Commit: commit, Digest: digest}}
		doc, err := encodePinDoc(&pinDoc{Entries: []pinEntry{p}})
		if err != nil {
			t.Fatal(err)
		}
		return json.Marshal(map[string]any{"pins": json.RawMessage(doc), "managed": []peerManaged{}})
	}
	v, err := b.admitOp(context.Background(), opOrigin{"user", "v1"}, "user:v1", "u1", opRequest{Op: "update", Entry: "managed:notes", Input: rawJSON(map[string]any{"to": "latest"})})
	if err != nil {
		t.Fatal(err)
	}
	userOp := v.(map[string]any)["op_id"]
	count := func() int {
		var n int
		b.db.QueryRow(`SELECT count(*) FROM plugin_ops WHERE scope = 'peer:mac'`).Scan(&n)
		return n
	}
	s, err := b.peerPluginsSync(context.Background(), b.peerLink("mac"))
	if err != nil || count() != 0 || len(s.Updating) != 1 || s.Updating[0]["op_id"] != userOp {
		t.Fatalf("an open user op: %d peer ops, updating %+v, %v", count(), s, err)
	}
	mustExec(t, b, `UPDATE plugin_ops SET phase = 'done' WHERE op_id = ?`, userOp)
	if _, err := b.peerPluginsSync(context.Background(), b.peerLink("mac")); err != nil || count() != 1 {
		t.Fatalf("after the user op ended: %d peer ops, %v", count(), err)
	}
}

// endFixtureOps ends the ops that a test fixture left open (atTestHub's add stays in writing), so
// a peer update does not wait for them (peerUpdate waits for any open op on the entry).
func endFixtureOps(t *testing.T, h *Hub) {
	t.Helper()
	mustExec(t, h, `UPDATE plugin_ops SET phase = 'done' WHERE phase NOT IN ('done', 'failed', 'conflict', 'unknown')`)
}

// TestPeerPluginsSyncAmbiguous (fed-live-hub item 4): a sync that fails because the peer's
// lazyrex-hub label is ambiguous (the peer hub restarted; Rex keeps its lost client about a minute)
// comes again in 2 s, as the pull does; another failure and a success wait the minute.
func TestPeerPluginsSyncAmbiguous(t *testing.T) {
	b := peerHub(t, "linux", "mac", nil)
	b.cfg.Peers = []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	b.peers = newPeerSet(b.cfg)
	var fail error
	b.peers.call = func(context.Context, PeerConfig, string, any) ([]byte, error) { return nil, fail }
	for _, c := range []struct {
		err  string
		want time.Duration
	}{
		{`hub.peer.plugins on mac: exit status 3: Error: client target "lazyrex-hub" is ambiguous`, peerPollUp},
		{`hub.peer.plugins on mac: exit status 1: Error: dial: connection refused`, peerSyncEvery},
	} {
		fail = errors.New(c.err)
		_, err := b.peerPluginsSync(context.Background(), b.peers.links["mac"])
		if err == nil || b.peerSyncWait(err) != c.want {
			t.Fatalf("%s: err %v, wait %s, want %s", c.err, err, b.peerSyncWait(err), c.want)
		}
	}
	if b.peerSyncWait(nil) != peerSyncEvery {
		t.Fatalf("after a success: %s", b.peerSyncWait(nil))
	}
}

// test_peer_sync_every sets the sync interval in a lab; the ambiguous retry stays 2 s. Only -test
// accepts it, and it is at least 1.
func TestPeerSyncEveryTest(t *testing.T) {
	b := peerHub(t, "linux", "mac", nil)
	b.cfg.TestPeerSyncEvery = 3
	if w := b.peerSyncWait(nil); w != 3*time.Second {
		t.Fatalf("after a success: %s, want 3s", w)
	}
	if w := b.peerSyncWait(errors.New(`exit status 3: Error: client target "lazyrex-hub" is ambiguous`)); w != peerPollUp {
		t.Fatalf("ambiguous: %s, want %s", w, peerPollUp)
	}
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{}, ""},
		{Config{Test: true, TestPeerSyncEvery: 2}, ""},
		{Config{TestPeerSyncEvery: 2}, "needs -test"},
		{Config{Test: true, TestPeerSyncEvery: -1}, "at least 1"},
	} {
		err := checkTestPeerSyncEvery(c.cfg)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: %v, want %q", c.cfg, err, c.want)
		}
	}
}
