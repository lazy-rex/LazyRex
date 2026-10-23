package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
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
