package main

import (
	"bytes"
	"errors"
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
		`{"version":3,"hub":"` + pinHub + `","entries":[]}`:               "pin document version 3 is not supported by this hub (it reads versions 1 and 2)",
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
		want := lock + ".id is not valid: not 32 lowercase hex and a newline; remove it to make a new pull identity"
		if _, err := pullIdentity(lock); err == nil || err.Error() != want {
			t.Errorf("%q: %v", body, err)
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

// export runs plugin export on cfg and gives its output.
func (env *cmdEnv) export(cfg string) (string, error) {
	var b bytes.Buffer
	err := pluginExport(cfg, &b)
	return b.String(), err
}

// export writes the pins of each managed GitHub entry that passes lockedDir, with the
// lock's pull identity. A local source and an orphaned lock entry are not exported. A second
// export gives the same bytes.
func TestExport(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.repo("lazy-rex", "Templates", "templates")
	const srcT = "github:lazy-rex/Templates@main"
	env.managed("messaging="+srcM, "t="+srcT)
	cfg := env.config(env.local("loc", "loc"))
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	l := env.lock()
	orphan := *l.Plugins[srcM]
	orphan.Ref = "old"
	l.Plugins["github:lazy-rex/Messaging@old"] = &orphan
	if err := writeLock(filepath.Join(env.dir, "hub.lock.json"), l); err != nil {
		t.Fatal(err)
	}
	out, err := env.export(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := readPullID(filepath.Join(env.dir, "hub.lock.json"))
	d, err := decodePinDoc([]byte(out))
	if err != nil || d.Hub != id || len(d.Entries) != 2 {
		t.Fatalf("document %s: %v (id %s)", out, err, id)
	}
	for _, e := range d.Entries {
		if want := l.Plugins[e.Source].lockPin; e.lockPin != want {
			t.Errorf("%s: %+v, want %+v", e.Source, e.lockPin, want)
		}
	}
	if d.Entries[0].Source != srcM || d.Entries[1].Source != srcT {
		t.Errorf("sources %s, %s", d.Entries[0].Source, d.Entries[1].Source)
	}
	if strings.Contains(out, `"dir"`) || strings.Contains(out, env.state) {
		t.Errorf("a path in the document:\n%s", out)
	}
	if again, _ := env.export(cfg); again != out {
		t.Errorf("a second export changed the bytes")
	}
}

// A GitHub entry that does not pass lockedDir, a static refusal and a malformed lock fail the
// export: nothing is written, and no pull identity is made.
func TestExportRefused(t *testing.T) {
	env := newCmdEnv(t)
	env.repo("lazy-rex", "Messaging", "messaging")
	env.managed("messaging=" + srcM)
	cfg := env.config(env.local("loc", "loc"))
	lockFile := filepath.Join(env.dir, "hub.lock.json")
	check := func(what, want string) {
		t.Helper()
		out, err := env.export(cfg)
		if err == nil || err.Error() != "pin document: "+want || out != "" {
			t.Errorf("%s: %q %v\nwant %s", what, out, err, want)
		}
		if _, err := os.Stat(lockFile + ".id"); err == nil {
			t.Errorf("%s: a pull identity was made", what)
		}
	}
	check("not installed", "plugin "+srcM+" is not installed: the hub installs it at its next start or reload")
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	le := env.lock().Plugins[srcM]
	writeInto(env.entryDir(srcM), "main.lua", "changed")
	check("changed", "plugin messaging: the cache entry for commit "+le.Commit+" changed (digest mismatch): the hub installs the locked commit again at its next start or reload")
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	env.config(srcM, "loc")
	check("static", "managed:messaging: the source "+srcM+" is also in the config")
	env.config("loc")
	os.Remove(filepath.Join(env.state, "plugins.json"))
	os.WriteFile(lockFile, []byte(`{"version":3,"plugins":{}}`), 0o600)
	check("malformed lock", "the lock file "+lockFile+" is not valid: version 3 is not supported (this hub reads versions 1 and 2); repair the file or restore it from a backup")
}

// failWriter fails each write.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// A failed write of the document has the plugin export prefix.
func TestExportWriteError(t *testing.T) {
	env := newCmdEnv(t)
	cfg := env.config(env.local("loc", "loc"))
	if err := pluginExport(cfg, failWriter{}); err == nil || err.Error() != "pin document: disk full" {
		t.Fatalf("got %v", err)
	}
}
