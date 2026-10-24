package main

import (
	"bytes"
	"context"
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
		`{"version":3,"peers":{}}`:         "version 3 is not supported",
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
		if err == nil || err.Error() != "plugin export: "+want || out != "" {
			t.Errorf("%s: %q %v\nwant %s", what, out, err, want)
		}
		if _, err := os.Stat(lockFile + ".id"); err == nil {
			t.Errorf("%s: a pull identity was made", what)
		}
	}
	check("not installed", "plugin "+srcM+" is not installed: run lazyrex-hub plugin sync -config "+cfg)
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	le := env.lock().Plugins[srcM]
	writeInto(env.entryDir(srcM), "main.lua", "changed")
	check("changed", "plugin messaging: the cache entry for commit "+le.Commit+" changed (digest mismatch): run lazyrex-hub plugin sync -config "+cfg)
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

// plugin export through pluginCmdMain: exit 0 and the document on stdout; an argument is a usage
// error.
func TestExportMain(t *testing.T) {
	env := newCmdEnv(t)
	cfg := env.config(env.local("loc", "loc"))
	var out, errb bytes.Buffer
	if code := pluginCmdMain([]string{"export", "-config", cfg}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"entries": []`) {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	if code := pluginCmdMain([]string{"export", "-config", cfg, "x"}, &out, &errb); code != 2 {
		t.Errorf("an argument: exit %d", code)
	}
}

// failWriter fails each write.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// A failed write of the document has the plugin export prefix.
func TestExportWriteError(t *testing.T) {
	env := newCmdEnv(t)
	cfg := env.config(env.local("loc", "loc"))
	if err := pluginExport(cfg, failWriter{}); err == nil || err.Error() != "plugin export: disk full" {
		t.Fatalf("got %v", err)
	}
}

// commitOf and digestOfCommit give the commit and the digest of a one-letter test pin.
func digestOfCommit(c string) string { return "sha256:" + strings.Repeat(c[:1], 64) }

func commitOf(c string) string { return strings.Repeat(c, 40) }

// Each merge case gives its outcome and its new base (section "Pin sync", the merge).
func TestMergePins(t *testing.T) {
	const src = "github:o/r@main"
	for _, tc := range []struct {
		name              string
		local, base, peer string
		take, keep        bool
		outcome, newBase  string // newBase "" = no base value after the merge
		fetch             bool
	}{
		{"equal", "a", "", "a", false, false, pullUnchanged, "a", false},
		{"equal, old base", "b", "a", "b", false, false, pullUnchanged, "b", false},
		{"not installed", "", "", "b", false, false, pullTaken, "b", true},
		{"only the peer moved", "a", "a", "b", false, false, pullTaken, "b", true},
		{"only local moved", "b", "a", "a", false, false, pullKept, "a", false},
		{"both moved", "b", "a", "c", false, false, pullConflict, "a", false},
		{"no base, differ", "a", "", "b", false, false, pullConflict, "", false},
		{"both moved, -take", "b", "a", "c", true, false, pullResolvedTake, "c", true},
		{"both moved, -keep", "b", "a", "c", false, true, pullResolvedKeep, "c", false},
		{"no base, -keep", "a", "", "b", false, true, pullResolvedKeep, "b", false},
		{"after -keep: the same document again", "a", "b", "b", false, false, pullKept, "b", false},
		{"crash after the lock: the same peer", "b", "a", "b", false, false, pullUnchanged, "b", false},
		{"crash after the lock: the peer moved", "b", "a", "c", false, false, pullConflict, "a", false},
	} {
		lock := &pluginLock{Version: 1, Plugins: map[string]*lockEntry{}}
		if tc.local != "" {
			lock.Plugins[src] = &lockEntry{lockPin: lockPin{Commit: commitOf(tc.local), Digest: digestOfCommit(tc.local)}}
		}
		var base map[string]basePin
		if tc.base != "" {
			base = map[string]basePin{src: {Commit: commitOf(tc.base), Digest: digestOfCommit(tc.base)}}
		}
		p := testPin()
		p.Commit, p.Digest = commitOf(tc.peer), digestOfCommit(tc.peer)
		var take, keep []string
		if tc.take {
			take = []string{src}
		}
		if tc.keep {
			keep = []string{src}
		}
		plan, err := mergePins([]string{src}, lock, base, &pinDoc{Version: 1, Hub: pinHub, Entries: []pinEntry{p}}, take, keep)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := plan.results[0].Outcome; got != tc.outcome {
			t.Errorf("%s: outcome %s, want %s", tc.name, got, tc.outcome)
		}
		if _, ok := plan.take[src]; ok != tc.fetch {
			t.Errorf("%s: fetch %v", tc.name, ok)
		}
		got, ok := plan.base[src]
		switch {
		case tc.newBase == "" && ok:
			t.Errorf("%s: a base %s", tc.name, got.Commit)
		case tc.newBase != "" && got.Commit != commitOf(tc.newBase):
			t.Errorf("%s: base %s, want %s", tc.name, got.Commit, commitOf(tc.newBase))
		}
		if plan.conflicts() != (tc.outcome == pullConflict) {
			t.Errorf("%s: conflicts() %v", tc.name, plan.conflicts())
		}
	}
}

// The scope of a merge: a source that only the document has is ignored; a source that only the
// config has is unchanged, and its base value stays. The output lines are sorted by source.
func TestMergePinsScope(t *testing.T) {
	a, b := testPin(), testPin()
	a.Source, a.Repo = "github:o/a@main", "a"
	b.Source, b.Repo = "github:o/b@main", "b"
	lock := &pluginLock{Version: 1, Plugins: map[string]*lockEntry{
		a.Source:          {lockPin: a.lockPin},
		"github:o/c@main": {lockPin: lockPin{Commit: commitOf("c"), Digest: digestOfCommit("c")}},
	}}
	old := map[string]basePin{"github:o/c@main": {Commit: commitOf("d"), Digest: digestOfCommit("d")}}
	plan, err := mergePins([]string{a.Source, "github:o/c@main"}, lock, old, &pinDoc{Version: 1, Hub: pinHub, Entries: []pinEntry{b, a}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	plan.write(&out)
	want := "unchanged\tgithub:o/a@main\tlocal " + pinCommit + "\tpeer " + pinCommit + "\n" +
		"ignored\tgithub:o/b@main\tlocal -\tpeer " + pinCommit + "\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
	if plan.base["github:o/c@main"].Commit != commitOf("d") || len(plan.take) != 0 {
		t.Errorf("base %v take %v", plan.base, plan.take)
	}
}

// -take and -keep must name a conflict, and never the same source.
func TestMergePinsFlags(t *testing.T) {
	p := testPin()
	lock := &pluginLock{Version: 1, Plugins: map[string]*lockEntry{p.Source: {lockPin: p.lockPin}}}
	doc := &pinDoc{Version: 1, Hub: pinHub, Entries: []pinEntry{p}}
	for _, tc := range []struct {
		take, keep []string
		want       string
	}{
		{[]string{p.Source}, []string{p.Source}, p.Source + " is in both -take and -keep"},
		{[]string{p.Source}, nil, "-take " + p.Source + ": the source has no conflict"},
		{nil, []string{"github:o/x@main"}, "-keep github:o/x@main: the source has no conflict"},
	} {
		if _, err := mergePins([]string{p.Source}, lock, nil, doc, tc.take, tc.keep); err == nil || err.Error() != tc.want {
			t.Errorf("%v %v: %v, want %s", tc.take, tc.keep, err, tc.want)
		}
	}
}

// machines gives two command environments that share the repositories: two machines of the user.
func machines(t *testing.T) (a, b *cmdEnv) {
	a, b = newCmdEnv(t), newCmdEnv(t)
	b.repos = a.repos
	return a, b
}

// pull writes the document doc into a file and runs plugin pull on cfg.
func (env *cmdEnv) pull(cfg, doc string, take, keep []string) (string, bool, error) {
	env.t.Helper()
	p := filepath.Join(env.t.TempDir(), "pins.json")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		env.t.Fatal(err)
	}
	var out bytes.Buffer
	conflict, err := pluginPull(context.Background(), cfg, p, take, keep, env.opts, &out)
	return out.String(), conflict, err
}

func (env *cmdEnv) mustExport(cfg string) string {
	env.t.Helper()
	doc, err := env.export(cfg)
	if err != nil {
		env.t.Fatal(err)
	}
	return doc
}

// files gives the bytes of the lock and of the base file, to show that a pull wrote nothing.
func (env *cmdEnv) files() string {
	l := env.lockBytes()
	b, _ := os.ReadFile(filepath.Join(env.dir, "hub.lock.json.peers.json"))
	return string(l) + "\x00" + string(b)
}

// A pull with no base and equal pins is unchanged and sets the base; a later move of the peer is
// taken: the lock gets the commit (with its cache entry and the imported ref_kind of the short
// ref), and the same document again is unchanged.
func TestPullTakes(t *testing.T) {
	a, b := machines(t)
	r := a.repo("lazy-rex", "Messaging", "messaging")
	cfgA, cfgB := a.config(srcM), b.config(srcM)
	if err := a.sync(cfgA); err != nil {
		t.Fatal(err)
	}
	if err := b.sync(cfgB); err != nil {
		t.Fatal(err)
	}
	c1 := r.git("rev-parse", "HEAD")
	out, conflict, err := b.pull(cfgB, a.mustExport(cfgA), nil, nil)
	if err != nil || conflict || out != "unchanged\t"+srcM+"\tlocal "+c1+"\tpeer "+c1+"\n" {
		t.Fatalf("the first pull: %q %v %v", out, conflict, err)
	}
	c2 := r.commit(map[string]string{"main.lua": "return 2\n"})
	if err := a.update(cfgA); err != nil {
		t.Fatal(err)
	}
	doc := a.mustExport(cfgA)
	out, conflict, err = b.pull(cfgB, doc, nil, nil)
	if err != nil || conflict || out != "taken\t"+srcM+"\tlocal "+c1+"\tpeer "+c2+"\n" {
		t.Fatalf("the second pull: %q %v %v", out, conflict, err)
	}
	la, lb := a.lock().Plugins[srcM], b.lock().Plugins[srcM]
	if lb.lockPin != la.lockPin || lb.RefKind != "branch" {
		t.Errorf("the pin: %+v, want %+v", lb.lockPin, la.lockPin)
	}
	e := githubEntry(t, srcM)
	if _, commit, err := lockedDir(cfgB, b.state, b.lock(), e); err != nil || commit != c2 {
		t.Errorf("lockedDir on b: %s %v", commit, err)
	}
	before := b.files()
	if out, _, err := b.pull(cfgB, doc, nil, nil); err != nil || !strings.HasPrefix(out, "unchanged\t") || b.files() != before {
		t.Errorf("the same document again: %q %v", out, err)
	}
}

// Two machines that moved apart with no base: a conflict, the lock and base unchanged, exit
// conflict. -keep resolves it (the base advances, so the next pull is kept); -take on a new
// conflict takes the peer pin.
func TestPullConflict(t *testing.T) {
	a, b := machines(t)
	r := a.repo("lazy-rex", "Messaging", "messaging")
	cfgA, cfgB := a.config(srcM), b.config(srcM)
	if err := b.sync(cfgB); err != nil {
		t.Fatal(err)
	}
	c1 := r.git("rev-parse", "HEAD")
	c2 := r.commit(map[string]string{"main.lua": "return 2\n"})
	if err := a.sync(cfgA); err != nil {
		t.Fatal(err)
	}
	doc := a.mustExport(cfgA)
	before := b.lockBytes()
	out, conflict, err := b.pull(cfgB, doc, nil, nil)
	if err != nil || !conflict || out != "conflict\t"+srcM+"\tlocal "+c1+"\tpeer "+c2+"\n" || string(b.lockBytes()) != string(before) {
		t.Fatalf("the conflict: %q %v %v", out, conflict, err)
	}
	out, conflict, err = b.pull(cfgB, doc, nil, []string{srcM})
	if err != nil || conflict || !strings.HasPrefix(out, "resolved-keep\t") || b.lock().Plugins[srcM].Commit != c1 {
		t.Fatalf("-keep: %q %v %v", out, conflict, err)
	}
	if out, _, err := b.pull(cfgB, doc, nil, nil); err != nil || !strings.HasPrefix(out, "kept\t") {
		t.Errorf("after -keep: %q %v", out, err)
	}
	if _, _, err := b.pull(cfgB, doc, []string{srcM}, nil); err == nil || !strings.Contains(err.Error(), "-take "+srcM+": the source has no conflict") {
		t.Errorf("-take without a conflict: %v", err)
	}
	c3 := r.commit(map[string]string{"main.lua": "return 3\n"})
	if err := a.update(cfgA); err != nil {
		t.Fatal(err)
	}
	if err := b.update(cfgB); err != nil {
		t.Fatal(err)
	}
	r.commit(map[string]string{"main.lua": "return 4\n"})
	if err := a.update(cfgA); err != nil {
		t.Fatal(err)
	}
	c4 := a.lock().Plugins[srcM].Commit
	out, conflict, err = b.pull(cfgB, a.mustExport(cfgA), []string{srcM}, nil)
	if err != nil || conflict || out != "resolved-take\t"+srcM+"\tlocal "+c3+"\tpeer "+c4+"\n" || b.lock().Plugins[srcM].Commit != c4 {
		t.Fatalf("-take: %q %v %v", out, conflict, err)
	}
}

// A fetch failure, a digest mismatch and a graph failure write nothing: the lock and the base stay
// byte-unchanged.
func TestPullAllOrNothing(t *testing.T) {
	a, b := machines(t)
	r := a.repo("lazy-rex", "Messaging", "messaging")
	cfgB := b.config(srcM) // not installed on b: each pull takes the pin
	// c has a local plugin nope, so its update to a commit that depends on nope passes.
	c := newCmdEnv(t)
	c.repos = a.repos
	cfgC := c.config(srcM, c.local("nope", "nope"))
	r.commit(map[string]string{"lazyrex.json": manifestJSON("messaging", "nope")})
	if err := c.sync(cfgC); err != nil {
		t.Fatal(err)
	}
	graphDoc := c.mustExport(cfgC)
	d, _ := decodePinDoc([]byte(graphDoc))
	digestDoc := strings.Replace(graphDoc, d.Entries[0].Digest, pinDigest, 1)
	missing := strings.Repeat("e", 40)
	fetchDoc := strings.Replace(graphDoc, d.Entries[0].Commit, missing, 1)
	before := b.files()
	for doc, want := range map[string]string{
		graphDoc:  "plugin messaging: dependency nope is not configured",
		digestDoc: "plugin messaging: the tree of " + d.Entries[0].Commit + " differs from the pin document",
		fetchDoc:  srcM + ": ",
	} {
		_, _, err := b.pull(cfgB, doc, nil, nil)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.HasSuffix(err.Error(), "; the lock is unchanged") {
			t.Errorf("got %v, want %q", err, want)
		}
		if b.files() != before {
			t.Errorf("%q: a file changed", want)
		}
	}
}

// A base write that fails after the lock write: the text names the changed pins; the next pull
// with the same peer is unchanged and advances the base; a moved peer, or a local update between
// the two pulls, gives a conflict.
func TestPullBaseWriteFails(t *testing.T) {
	for _, next := range []string{"same peer", "peer moved", "local update"} {
		t.Run(next, func(t *testing.T) {
			a, b := machines(t)
			r := a.repo("lazy-rex", "Messaging", "messaging")
			cfgA, cfgB := a.config(srcM), b.config(srcM)
			a.sync(cfgA)
			b.sync(cfgB)
			if _, _, err := b.pull(cfgB, a.mustExport(cfgA), nil, nil); err != nil {
				t.Fatal(err)
			}
			c2 := r.commit(map[string]string{"main.lua": "return 2\n"})
			a.update(cfgA)
			doc := a.mustExport(cfgA)
			id, _ := readPullID(filepath.Join(a.dir, "hub.lock.json"))
			pullWritePeers = func(string, *peerBase) error { return errors.New("disk full") }
			_, _, err := b.pull(cfgB, doc, nil, nil)
			pullWritePeers = writePeers
			want := "plugin pull: the pins changed, but the base of " + id + " did not: the next pull can report conflicts for " + srcM + ": disk full"
			if err == nil || err.Error() != want || b.lock().Plugins[srcM].Commit != c2 {
				t.Fatalf("got %v", err)
			}
			wantOut := "unchanged"
			switch next {
			case "peer moved":
				r.commit(map[string]string{"main.lua": "return 3\n"})
				a.update(cfgA)
				doc, wantOut = a.mustExport(cfgA), "conflict"
			case "local update":
				r.commit(map[string]string{"main.lua": "return 3\n"})
				b.update(cfgB)
				wantOut = "conflict"
			}
			out, _, err := b.pull(cfgB, doc, nil, nil)
			if err != nil || !strings.HasPrefix(out, wantOut+"\t") {
				t.Errorf("the next pull: %q %v, want %s", out, err, wantOut)
			}
		})
	}
}

// The checks before any fetch: a malformed document, a document from this lock, a malformed base
// file, and -take with -keep on one source. No git runs.
func TestPullRefusedBeforeFetch(t *testing.T) {
	a := newCmdEnv(t)
	a.repo("lazy-rex", "Messaging", "messaging")
	cfg := a.config(srcM)
	if err := a.sync(cfg); err != nil {
		t.Fatal(err)
	}
	own := a.mustExport(cfg)
	d, _ := decodePinDoc([]byte(own))
	d.Hub = strings.Repeat("f", 32)
	ob, _ := encodePinDoc(d)
	other := string(ob)
	a.opts.remote = func(owner, repo string) string { t.Fatal("a git call"); return "" }
	lockFile := filepath.Join(a.dir, "hub.lock.json")
	for _, tc := range []struct {
		doc, want string
		prep      func()
		take      []string
		keep      []string
	}{
		{doc: `{"version":3}`, want: "pin document "},
		{doc: own, want: "comes from this lock"},
		{doc: other, want: lockFile + ".peers.json is not valid", prep: func() { os.WriteFile(lockFile+".peers.json", []byte("{"), 0o600) }},
		{doc: other, want: srcM + " is in both -take and -keep", prep: func() { os.Remove(lockFile + ".peers.json") }, take: []string{srcM}, keep: []string{srcM}},
	} {
		if tc.prep != nil {
			tc.prep()
		}
		before := a.lockBytes()
		if _, _, err := a.pull(cfg, tc.doc, tc.take, tc.keep); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("got %v, want %q", err, tc.want)
		}
		if string(a.lockBytes()) != string(before) {
			t.Errorf("%q: the lock changed", tc.want)
		}
	}
}

// Two configs that name one lock share its pull identity; a document from one is refused by the
// other. Two configs on one state with their own locks have their own identities.
func TestPullSharedLock(t *testing.T) {
	env := newCmdEnv(t)
	cfg := env.config(env.local("loc", "loc"))
	b, _ := os.ReadFile(cfg)
	shared := filepath.Join(env.dir, "second.json")
	own := filepath.Join(env.dir, "third.json")
	os.WriteFile(shared, []byte(strings.Replace(string(b), `{`, `{"lock":"hub.lock.json",`, 1)), 0o600)
	os.WriteFile(own, b, 0o600)
	d1, d2, d3 := env.mustExport(cfg), env.mustExport(shared), env.mustExport(own)
	h := func(doc string) string { d, _ := decodePinDoc([]byte(doc)); return d.Hub }
	if h(d1) != h(d2) || h(d1) == h(d3) {
		t.Errorf("ids %s %s %s", h(d1), h(d2), h(d3))
	}
	if _, _, err := env.pull(shared, d1, nil, nil); err == nil || !strings.Contains(err.Error(), "comes from this lock") {
		t.Errorf("a document of the shared lock: %v", err)
	}
}

// plugin pull through pluginCmdMain: exit 1 on a conflict; no -file is a usage error.
func TestPullMain(t *testing.T) {
	env := newCmdEnv(t)
	cfg := env.config(env.local("loc", "loc"))
	var out, errb bytes.Buffer
	if code := pluginCmdMain([]string{"pull", "-config", cfg}, &out, &errb); code != 2 {
		t.Errorf("no -file: exit %d", code)
	}
	p := filepath.Join(t.TempDir(), "pins.json")
	os.WriteFile(p, []byte(`{"version":1,"hub":"`+pinHub+`","entries":[]}`), 0o600)
	if code := pluginCmdMain([]string{"pull", "-config", cfg, "-file", p}, &out, &errb); code != 0 {
		t.Errorf("an empty document: exit %d: %s", code, errb.String())
	}
}

// A failed write of the result lines fails the pull, with the plugin pull prefix. The lock and
// the base are already written, so the text claims no unchanged lock.
func TestPullOutputFailure(t *testing.T) {
	a, b := machines(t)
	a.repo("lazy-rex", "Messaging", "messaging")
	cfgA := a.config(srcM)
	if err := a.sync(cfgA); err != nil {
		t.Fatal(err)
	}
	cfgB := b.config(b.local("loc", "loc"))
	p := filepath.Join(t.TempDir(), "pins.json")
	if err := os.WriteFile(p, []byte(a.mustExport(cfgA)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := pluginPull(context.Background(), cfgB, p, nil, nil, b.opts, failWriter{})
	if err == nil || err.Error() != "plugin pull: the lock and the base are written, but the result lines are not: disk full" {
		t.Fatalf("got %v", err)
	}
}

// A pull that only removes an orphaned lock entry changes the lock. A base write failure then
// says that the lock changed.
func TestPullBaseFailureAfterOrphanRemoval(t *testing.T) {
	a, b := machines(t)
	a.repo("lazy-rex", "Messaging", "messaging")
	cfgA, cfgB := a.config(srcM), b.config(srcM)
	if err := a.sync(cfgA); err != nil {
		t.Fatal(err)
	}
	if err := b.sync(cfgB); err != nil {
		t.Fatal(err)
	}
	doc := a.mustExport(cfgA)
	cfgB = b.config(b.local("loc", "loc"))
	before := string(b.lockBytes())
	id, _ := readPullID(filepath.Join(a.dir, "hub.lock.json"))
	t.Cleanup(func() { pullWritePeers = writePeers })
	pullWritePeers = func(string, *peerBase) error { return errors.New("disk full") }
	_, _, err := b.pull(cfgB, doc, nil, nil)
	if string(b.lockBytes()) == before {
		t.Fatal("the pull did not remove the orphaned entry")
	}
	if want := "plugin pull: the lock changed, but the base of " + id + " did not: disk full"; err == nil || err.Error() != want {
		t.Fatalf("got %v", err)
	}
}

// A base write failure of a pull that changes nothing in the lock says so.
func TestPullBaseFailureLockUnchanged(t *testing.T) {
	a, b := machines(t)
	a.repo("lazy-rex", "Messaging", "messaging")
	cfgA, cfgB := a.config(srcM), b.config(srcM)
	a.sync(cfgA)
	b.sync(cfgB)
	doc := a.mustExport(cfgA)
	before := string(b.lockBytes())
	id, _ := readPullID(filepath.Join(a.dir, "hub.lock.json"))
	t.Cleanup(func() { pullWritePeers = writePeers })
	pullWritePeers = func(string, *peerBase) error { return errors.New("disk full") }
	_, _, err := b.pull(cfgB, doc, nil, nil)
	if want := "plugin pull: the base of " + id + ": disk full; the lock is unchanged"; err == nil || err.Error() != want || string(b.lockBytes()) != before {
		t.Fatalf("got %v", err)
	}
}

// failSyncDir makes call n (from 1) of replaceSyncDir fail, after the rename.
func failSyncDir(t *testing.T, n int) {
	t.Helper()
	calls := 0
	t.Cleanup(func() { replaceSyncDir = syncDir })
	replaceSyncDir = func(dir string) error {
		calls++
		if calls == n {
			return errors.New("disk full")
		}
		return syncDir(dir)
	}
}

// A directory sync failure after the rename: the new lock is in place, and sync and update say so.
// They claim no unchanged lock and roll nothing back.
func TestLockPublishedSyncFailure(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	cfg := env.config(srcM)
	lock := filepath.Join(env.dir, "hub.lock.json")
	want := func(cmd string) string {
		return "plugin " + cmd + ": " + lock + " is replaced, but the sync of its directory failed (a crash can undo the change): disk full"
	}
	failSyncDir(t, 1)
	if err := env.sync(cfg); err == nil || err.Error() != want("sync") {
		t.Fatalf("sync: %v", err)
	}
	if le := env.lock().Plugins[srcM]; le == nil || le.Commit != strings.TrimSpace(r.git("rev-parse", "HEAD")) {
		t.Fatal("the lock does not have the synced entry")
	}
	c2 := r.commit(map[string]string{"main.lua": "return 2\n"})
	failSyncDir(t, 1)
	if err := env.update(cfg); err == nil || err.Error() != want("update") {
		t.Fatalf("update: %v", err)
	}
	if env.lock().Plugins[srcM].Commit != c2 {
		t.Fatal("the lock does not have the updated commit")
	}
}

// Pull: a directory sync failure after the rename of the lock stops the pull before the base; one
// after the rename of the base reports the base as replaced. The next pull is unchanged in both.
func TestPullPublishedSyncFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		call int
	}{{"lock", 1}, {"base", 2}} {
		t.Run(c.name, func(t *testing.T) {
			a, b := machines(t)
			a.repo("lazy-rex", "Messaging", "messaging")
			cfgA, cfgB := a.config(srcM), b.config(srcM)
			if err := a.sync(cfgA); err != nil {
				t.Fatal(err)
			}
			doc := a.mustExport(cfgA)
			id, _ := readPullID(filepath.Join(a.dir, "hub.lock.json"))
			lock := filepath.Join(b.dir, "hub.lock.json")
			want := "plugin pull: " + lock + " is replaced, but the sync of its directory failed (a crash can undo the change): disk full; the base of " + id + " is not written"
			if c.name == "base" {
				want = "plugin pull: " + peersPath(lock) + " is replaced, but the sync of its directory failed (a crash can undo the change): disk full"
			}
			failSyncDir(t, c.call)
			_, _, err := b.pull(cfgB, doc, nil, nil)
			if err == nil || err.Error() != want {
				t.Fatalf("got %v", err)
			}
			if b.lock().Plugins[srcM] == nil || b.lock().Plugins[srcM].Commit != a.lock().Plugins[srcM].Commit {
				t.Fatal("the lock does not have the pulled pin")
			}
			peers, err := readPeers(lock)
			if err != nil || (c.name == "base") != (peers.Peers[id] != nil) {
				t.Fatalf("the base: %v %v", peers, err)
			}
			replaceSyncDir = syncDir
			out, conflict, err := b.pull(cfgB, doc, nil, nil)
			if err != nil || conflict || !strings.HasPrefix(out, "unchanged\t"+srcM+"\t") {
				t.Fatalf("the next pull: %q %v %v", out, conflict, err)
			}
		})
	}
}
