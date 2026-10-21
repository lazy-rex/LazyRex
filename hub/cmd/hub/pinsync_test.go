package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	pinCommit = "0123456789abcdef0123456789abcdef01234567"
	pinDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	pinHub    = "00112233445566778899aabbccddeeff"
)

// testPin is a valid document entry for github:o/r@main.
func testPin() pinEntry {
	return pinEntry{Source: "github:o/r@main", lockPin: lockPin{Name: "p", Owner: "o", Repo: "r", Ref: "main", RefKind: "branch", Commit: pinCommit, Digest: pinDigest}}
}

// The document has deterministic bytes, sorted by source, and decodes to the same pins.
func TestPinDocRoundTrip(t *testing.T) {
	a, b := testPin(), testPin()
	b.Source, b.Subdir, b.Ref, b.RefKind = "github:o/r/plugins/x@refs/tags/v1", "plugins/x", "refs/tags/v1", "tag"
	one, err := encodePinDoc(&pinDoc{Version: 1, Hub: pinHub, Entries: []pinEntry{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	two, _ := encodePinDoc(&pinDoc{Version: 1, Hub: pinHub, Entries: []pinEntry{b, a}})
	if !bytes.Equal(one, two) {
		t.Fatalf("the entry order changed the bytes:\n%s\n%s", one, two)
	}
	if !strings.HasSuffix(string(one), "}\n") || strings.Index(string(one), b.Source) > strings.Index(string(one), a.Source) {
		t.Errorf("not sorted by source, or no final newline:\n%s", one)
	}
	d, err := decodePinDoc(one)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := encodePinDoc(d); !bytes.Equal(again, one) {
		t.Errorf("a decode and encode changed the bytes:\n%s", again)
	}
}

// A short ref allows a branch or a tag; a full form fixes the kind.
func TestPinDocRefKinds(t *testing.T) {
	for _, tc := range []struct{ ref, kind string }{{"main", "branch"}, {"main", "tag"}, {"refs/heads/main", "branch"}, {"refs/tags/v1", "tag"}, {pinCommit, "commit"}} {
		e := testPin()
		e.Source, e.Ref, e.RefKind = "github:o/r@"+tc.ref, tc.ref, tc.kind
		if err := e.check(); err != nil {
			t.Errorf("%s %s: %v", tc.ref, tc.kind, err)
		}
	}
}

// Each malformed document is refused as a whole, with its reason.
func TestPinDocRefused(t *testing.T) {
	entry := func(f func(e *pinEntry)) string {
		e := testPin()
		f(&e)
		b, _ := encodePinDoc(&pinDoc{Version: 1, Hub: pinHub, Entries: []pinEntry{e}})
		return string(b)
	}
	good := entry(func(*pinEntry) {})
	for doc, want := range map[string]string{
		`{"hub":"` + pinHub + `","entries":[]}`:                           "version is missing",
		`{"version":2,"hub":"` + pinHub + `","entries":[]}`:               "pin document version 2 is not supported by this hub (it reads version 1)",
		`{"version":1,"hub":"ABC","entries":[]}`:                          "hub must be 32 lowercase hex",
		`{"version":1,"hub":"` + pinHub + `"}`:                            "entries must be an array",
		`{"version":1,"hub":"` + pinHub + `","entries":null}`:             "entries must be an array",
		`{"version":1,"hub":"` + pinHub + `","entries":[],"x":1}`:         `unknown field "x"`,
		`{"version":1,"hub":"` + pinHub + `","entries":[]} {}`:            "data after the object",
		strings.Replace(good, `"digest"`, `"extra": 1, "digest"`, 1):      `entry 1: json: unknown field "extra"`,
		entry(func(e *pinEntry) { e.Source, e.Owner = "/local/dir", "" }): "entry 1 (/local/dir): not a GitHub source",
		entry(func(e *pinEntry) { e.Owner = "x" }):                        "owner does not match the source",
		entry(func(e *pinEntry) { e.Repo = "x" }):                         "repo does not match the source",
		entry(func(e *pinEntry) { e.Subdir = "x" }):                       "subdir does not match the source",
		entry(func(e *pinEntry) { e.Ref = "dev" }):                        "ref does not match the source",
		entry(func(e *pinEntry) { e.RefKind = "commit" }):                 `ref_kind "commit" is not allowed for the ref main`,
		entry(func(e *pinEntry) { e.RefKind = "" }):                       `ref_kind "" is not allowed for the ref main`,
		entry(func(e *pinEntry) {
			e.Source, e.Ref, e.RefKind = "github:o/r@refs/heads/main", "refs/heads/main", "tag"
		}): `ref_kind "tag" is not allowed for the ref refs/heads/main`,
		entry(func(e *pinEntry) { e.Commit = strings.ToUpper(pinCommit) }): "commit is not 40 lowercase hex",
		entry(func(e *pinEntry) {
			other := strings.Repeat("a", 40)
			e.Source, e.Ref, e.RefKind = "github:o/r@"+other, other, "commit"
		}): "commit is not the commit of the ref",
		entry(func(e *pinEntry) { e.Name = "Bad Name" }): "name is not a plugin name",
		entry(func(e *pinEntry) { e.Digest = "sha1:x" }): "digest is not sha256:<64 lowercase hex>",
		strings.Replace(good, `"entries": [`, `"entries": [`+strings.TrimSuffix(strings.SplitN(strings.SplitN(good, `"entries": [`, 2)[1], "]", 2)[0], "\n  ")+",", 1): "entry 2: the source github:o/r@main is there twice",
	} {
		_, err := decodePinDoc([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\ngot %v, want %q", doc, err, want)
		}
	}
	if _, err := decodePinDoc([]byte(good)); err != nil {
		t.Fatalf("the good document: %v", err)
	}
}

// The pull identity is made once (0600) and read back; a malformed file is refused with the text
// that says how to repair it.
func TestPullIdentity(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "hub.lock.json")
	if id, err := readPullID(lock); id != "" || err != nil {
		t.Fatalf("no file: %q %v", id, err)
	}
	id, err := pullIdentity(lock)
	if err != nil || !pullID.MatchString(id) {
		t.Fatalf("made %q: %v", id, err)
	}
	if fi, _ := os.Stat(lock + ".id"); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
	if again, err := pullIdentity(lock); again != id || err != nil {
		t.Errorf("a second call gave %q %v, want %q", again, err, id)
	}
	for _, body := range []string{id, "xyz\n", id + "\n\n"} {
		os.WriteFile(lock+".id", []byte(body), 0o600)
		want := lock + ".id is not valid: not 32 lowercase hex and a newline; remove it to make a new pull identity (peers then have no base for this lock)"
		if _, err := pullIdentity(lock); err == nil || err.Error() != want {
			t.Errorf("%q: %v", body, err)
		}
	}
}

// The base file: missing is empty; a write reads back; a malformed file is refused before any use.
func TestPeerBase(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "hub.lock.json")
	b, err := readPeers(lock)
	if err != nil || len(b.Peers) != 0 {
		t.Fatalf("no file: %v %v", b, err)
	}
	b.Peers[pinHub] = map[string]basePin{"github:o/r@main": {Commit: pinCommit, Digest: pinDigest}}
	if err := writePeers(lock, b); err != nil {
		t.Fatal(err)
	}
	got, err := readPeers(lock)
	if err != nil || got.Peers[pinHub]["github:o/r@main"].Commit != pinCommit {
		t.Fatalf("read back: %v %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(lock), ".*tmp-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	fix := "; repair it, or remove it (the next pull then reports each differing key as a conflict)"
	for body, why := range map[string]string{
		`{`:                                "unexpected EOF",
		`{"peers":{}}`:                     "version is missing",
		`{"version":2,"peers":{}}`:         "version 2 is not supported",
		`{"version":1}`:                    "peers must be an object",
		`{"version":1,"peers":{},"x":1}`:   `json: unknown field "x"`,
		`{"version":1,"peers":{"abc":{}}}`: `peer "abc": not a pull identity with an object`,
		`{"version":1,"peers":{"` + pinHub + `":{"s":{"commit":"x","digest":"` + pinDigest + `"}}}}`: "peer " + pinHub + ", s: commit or digest of the wrong form",
	} {
		os.WriteFile(lock+".peers.json", []byte(body), 0o600)
		_, err := readPeers(lock)
		if want := lock + ".peers.json is not valid: " + why + fix; err == nil || err.Error() != want {
			t.Errorf("%s\ngot  %v\nwant %s", body, err, want)
		}
	}
}

// A document without entries is an empty array, also from a new pinDoc and after a decode.
func TestPinDocEmpty(t *testing.T) {
	b, err := encodePinDoc(&pinDoc{Version: 1, Hub: pinHub})
	if err != nil || !strings.Contains(string(b), `"entries": []`) {
		t.Fatalf("a new empty document: %s %v", b, err)
	}
	d, err := decodePinDoc([]byte(`{"version":1,"hub":"` + pinHub + `","entries":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := encodePinDoc(d)
	if _, err := decodePinDoc(again); err != nil {
		t.Fatalf("the round trip: %s: %v", again, err)
	}
}
