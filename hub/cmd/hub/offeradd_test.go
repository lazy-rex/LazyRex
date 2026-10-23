package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// setOffers makes offers the state of the last sync with the plugins_from peer "mac" of h.
func setOffers(h *Hub, offers ...peerOffer) {
	if h.peers == nil {
		h.cfg.HubName, h.cfg.Peers = "linux", []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
		h.peers = newPeerSet(h.cfg)
	}
	if offers == nil {
		offers = []peerOffer{}
	}
	l := h.peerLink("mac")
	l.mu.Lock()
	l.sync = &peerSync{Offered: offers, logged: map[string]bool{}}
	l.mu.Unlock()
}

// offerReq is the add that the Plugins panel's confirm form sends for the offer o.
func offerReq(o peerOffer) opRequest {
	return opRequest{Op: "add", Input: rawJSON(map[string]any{"source": o.Source, "id": o.ID}), Target: rawJSON(o)}
}

func testOffer(src, repo, ref, commit, digest string) peerOffer {
	p := pinEntry{Source: src, lockPin: lockPin{Name: repo, Owner: "acme", Repo: repo, Ref: ref, RefKind: "tag", Commit: commit, Digest: digest}}
	return peerOffer{Peer: "mac", ID: repo, Source: src, Pin: p}
}

// The add of an offer (lead-offer4): the target is the complete offer {peer, id, source, pin}, and
// admission takes it only when it equals a current offer of the plugins_from peer. A form that
// shows an older offer (another id at the same pin, another commit, peer or tag) or a withdrawn
// offer is candidate_changed. The admitted pin is the stored target; a later offer does not change
// it. hub.plugins lists the offers.
func TestOfferAddAdmission(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	commit, digest := strings.Repeat("a", 40), "sha256:"+strings.Repeat("b", 64)
	o := testOffer("github:acme/newp@v1", "newp", "v1", commit, digest)

	setOffers(h)
	if _, err := userAdmit(h, "none", offerReq(o)); !isRefusal(err, "candidate_changed") {
		t.Fatalf("no offer: %v", err)
	}
	stale := map[string]func(*peerOffer){
		"another id at the same pin": func(x *peerOffer) { x.ID = "other" },
		"another commit":             func(x *peerOffer) { x.Pin.Commit = strings.Repeat("c", 40) },
		"another peer":               func(x *peerOffer) { x.Peer = "mac2" },
		"another tag":                func(x *peerOffer) { x.Pin.Tag = "v9" },
	}
	for name, change := range stale {
		cur := o
		change(&cur)
		setOffers(h, cur)
		if _, err := userAdmit(h, "stale-"+name, offerReq(o)); !isRefusal(err, "candidate_changed") {
			t.Errorf("%s: %v", name, err)
		}
	}

	setOffers(h, o)
	mine := offerReq(o)
	mine.Input = rawJSON(map[string]any{"source": o.Source, "id": "mine"})
	if _, err := userAdmit(h, "mine", mine); err == nil || !strings.Contains(err.Error(), "the offer's source and id") {
		t.Errorf("another id in the input: %v", err)
	}
	noID := o
	noID.ID = ""
	setOffers(h, noID)
	if _, err := userAdmit(h, "noid", offerReq(noID)); err == nil || !strings.Contains(err.Error(), "the offer's source and id") {
		t.Errorf("an offer with no id: %v", err)
	}
	setOffers(h, o)
	if _, err := userAdmit(h, "upd", opRequest{Op: "update", Entry: "managed:newp", Target: rawJSON(o.Pin)}); err == nil || !strings.Contains(err.Error(), "only a peer sync") {
		t.Errorf("a user update with a target: %v", err)
	}
	var list struct {
		Offered []peerOffer `json:"offered"`
	}
	v, err := h.pluginsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	json.Unmarshal(b, &list)
	if len(list.Offered) != 1 || list.Offered[0] != o {
		t.Fatalf("hub.plugins offered: %s", b)
	}

	res, err := userAdmit(h, "add", offerReq(o))
	if err != nil {
		t.Fatal(err)
	}
	op := opsOf(t, h, res["op_id"].(string))
	want := opResolved{Source: o.Source, RefKind: "tag", Commit: commit, FetchRef: commit, Digest: digest, ByID: true}
	var got opResolved
	json.Unmarshal([]byte(op.Target), &got)
	if got != want || op.Entry != "managed:newp" {
		t.Fatalf("the stored target %+v (entry %s), want %+v", got, op.Entry, want)
	}
	// The offer changes after admission: the op keeps the pin that the user confirmed.
	setOffers(h, testOffer("github:acme/newp@v2", "newp", "v2", strings.Repeat("d", 40), digest))
	json.Unmarshal([]byte(opsOf(t, h, op.ID).Target), &got)
	if got != want {
		t.Fatalf("the target after the offer changed: %+v", got)
	}
	h.pops.resolve = func(context.Context, *pluginOp) (opResolved, error) {
		t.Fatal("the pin of an offer resolved")
		return opResolved{}, nil
	}
	op.Phase = "resolving"
	if err := h.opResolve(context.Background(), op); err != nil || op.Phase != "fetching" {
		t.Fatalf("resolving: %+v %v", op, err)
	}

	// The id of the offer is in use here: the add is refused with the reason.
	dup := testOffer("github:acme/dup@v1", "dup", "v1", commit, digest)
	writeManaged(t, dir, `{"plugins":[{"id":"dup","source":"github:acme/zzz@v1"}]}`)
	opsLoadEntries(t, h)
	setOffers(h, dup)
	if _, err := userAdmit(h, "dup", offerReq(dup)); !isRefusal(err, "conflict") || !strings.Contains(err.Error(), "managed:dup exists") {
		t.Errorf("a duplicate id: %v", err)
	}
}

// The add of an offer fetches the offered commit by its ID and must get the offered digest:
// another digest fails, with no lock written. With the digest, it locks the offered pin.
func TestOfferAddFetch(t *testing.T) {
	r, _ := fetchTestRepo(t)
	c2 := r.commit(map[string]string{"lazyrex.json": manifestJSON("notes"), "main.lua": "return 2\n"})
	r.git("tag", "v1.1.0")
	peer := fetchTestHub(t, r) // the peer: its own fetch gives the digest that it exports
	digest := opPinOf(t, fetchedAdd(t, peer, "peer-add", "github:acme/notes@v1.1.0")).Digest
	h := fetchTestHub(t, r)
	ctx := context.Background()
	run := func(key string, o peerOffer) *pluginOp {
		t.Helper()
		setOffers(h, o)
		v, err := userAdmit(h, key, offerReq(o))
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
	bad := run("wrong", testOffer("github:acme/notes@v1.1.0", "notes", "v1.1.0", c2, "sha256:"+strings.Repeat("0", 64)))
	if bad.Phase != "failed" || !strings.Contains(bad.Error, "not the peer's") {
		t.Fatalf("another digest: %+v", bad)
	}
	if _, err := os.Stat(opLockFile(h)); err == nil {
		if le := readTestLock(t, h).Plugins["github:acme/notes@v1.1.0"]; le != nil {
			t.Fatalf("a failed offer fetch locked %+v", le)
		}
	}
	op := run("right", testOffer("github:acme/notes@v1.1.0", "notes", "v1.1.0", c2, digest))
	if op.Phase != "locking" {
		t.Fatalf("the fetch: %+v", op)
	}
	if err := h.opLock(ctx, op); err != nil || op.Phase != "writing" {
		t.Fatalf("the lock write: %+v %v", op, err)
	}
	if le := readTestLock(t, h).Plugins["github:acme/notes@v1.1.0"]; le == nil || le.Commit != c2 || le.Digest != digest || le.RefKind != "tag" {
		t.Fatalf("the lock entry %+v", le)
	}
}

// A sync that read the peer's document applies its lists also when an op admission failed (a held
// plugin manager refuses the peer update): the new offer shows, the withdrawn offer is no longer
// admitted, and last_error is set (claude-48, c48-offer-rev). A sync that read no document keeps
// the last lists.
func TestPeerSyncPartialResult(t *testing.T) {
	b, _, _, _ := atTestHub(t, "github:acme/notes@v1.0.0")
	ctx := context.Background()
	commit, digest := strings.Repeat("c", 40), "sha256:"+strings.Repeat("d", 64)
	newp := testOffer("github:acme/newp@v1", "newp", "v1", commit, digest)
	setOffers(b, testOffer("github:acme/gone@v1", "gone", "v1", commit, digest))
	gone := b.peerOffered()[0]
	upd := pinEntry{Source: "github:acme/notes@v1.1.0", lockPin: lockPin{Name: "notes", Owner: "acme", Repo: "notes", Ref: "v1.1.0", RefKind: "tag", Commit: commit, Digest: digest}}
	entries := []pinEntry{upd, newp.Pin}
	b.peers.call = func(ctx context.Context, _ PeerConfig, action string, args any) ([]byte, error) {
		doc, err := encodePinDoc(&pinDoc{Hub: strings.Repeat("a", 32), Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		return json.Marshal(map[string]any{"pins": json.RawMessage(doc), "managed": []peerManaged{{ID: "newp", Source: newp.Source}}})
	}
	mustExec(t, b, `INSERT INTO plugin_hold (one, release_id, acquired_at) VALUES (1, 'r1', 't1')`)
	l := b.peerLink("mac")
	s, err := b.peerPluginsSync(ctx, l)
	if err == nil || s == nil {
		t.Fatalf("a held update: %+v %v", s, err)
	}
	b.peerSyncApply(l, s, err)
	if got := b.peerOffered(); len(got) != 1 || got[0] != newp {
		t.Fatalf("the offers after a partial sync: %+v", got)
	}
	l.mu.Lock()
	lastErr, lastOK := l.sync.LastError, l.sync.LastOK
	l.mu.Unlock()
	if lastErr == "" || lastOK != nil {
		t.Errorf("the status of a partial sync: last_error %q, last_ok %v", lastErr, lastOK)
	}
	if _, err := userAdmit(b, "gone", offerReq(gone)); !isRefusal(err, "candidate_changed") {
		t.Errorf("the withdrawn offer: %v", err)
	}
	// The peer is down: no document, so the lists stay.
	b.peerSyncApply(l, nil, errors.New("hub.peer.plugins on mac: down"))
	if got := b.peerOffered(); len(got) != 1 || got[0] != newp {
		t.Fatalf("the offers after a failed read: %+v", got)
	}
	// The in-sync check of an entry fails (the plugin record read: a canceled context), and the
	// sync continues with the other entries (codex-27): the document's offers apply.
	cur := readTestLock(t, b).Plugins["github:acme/notes@v1.0.0"]
	entries = []pinEntry{{Source: "github:acme/notes@v1.0.0", lockPin: cur.lockPin}}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	s, err = b.peerPluginsSync(canceled, l)
	if err == nil || !strings.Contains(err.Error(), "context canceled") || s == nil {
		t.Fatalf("a failed in-sync check: %+v %v", s, err)
	}
	b.peerSyncApply(l, s, err)
	if got := b.peerOffered(); len(got) != 0 {
		t.Fatalf("the offers after a failed in-sync check: %+v", got)
	}
}
