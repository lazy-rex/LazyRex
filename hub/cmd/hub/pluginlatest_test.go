package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParseVersionTag(t *testing.T) {
	good := map[string]tagVersion{
		"v0.0.0":                              {"0", "0", "0"},
		"v1.2.3":                              {"1", "2", "3"},
		"v10.0.20":                            {"10", "0", "20"},
		"v123456789012345678901234567890.0.1": {"123456789012345678901234567890", "0", "1"},
	}
	for tag, want := range good {
		if got, ok := parseVersionTag(tag); !ok || got != want {
			t.Errorf("parseVersionTag(%q) = %v, %v; want %v, true", tag, got, ok, want)
		}
	}
	bad := []string{
		"", "v", "1.2.3", "V1.2.3", "v1.2", "v1.2.3.4", "v01.2.3", "v1.02.3", "v1.2.03", "v00.0.0",
		"v1.2.3-rc.1", "v1.2.3+build.5", "v1.2.3-rc.1+b", "v-1.2.3", "v1..3", "v1.2.x", " v1.2.3",
		"v1.2.3 ", "v1.2.3\n", "refs/tags/v1.2.3", "v1.2.3^{}", "release-1.2.3", "latest", "v١.2.3",
	}
	for _, tag := range bad {
		if got, ok := parseVersionTag(tag); ok {
			t.Errorf("parseVersionTag(%q) = %v, true; want false", tag, got)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	// Each case is lower < higher.
	cases := [][2]string{
		{"v1.9.0", "v1.10.0"},
		{"v9.99.99", "v10.0.0"},
		{"v0.0.0", "v0.0.1"},
		{"v0.9.9", "v1.0.0"},
		{"v1.2.9", "v1.2.10"},
		{"v1.2.3", "v1.3.0"},
		{"v99999999999999999999.0.0", "v100000000000000000000.0.0"},
		{"v1.0.99999999999999999999", "v1.0.100000000000000000000"},
	}
	for _, c := range cases {
		lo, ok1 := parseVersionTag(c[0])
		hi, ok2 := parseVersionTag(c[1])
		if !ok1 || !ok2 {
			t.Fatalf("parse %q %q: %v %v", c[0], c[1], ok1, ok2)
		}
		if got := compareVersions(lo, hi); got != -1 {
			t.Errorf("compareVersions(%s, %s) = %d, want -1", c[0], c[1], got)
		}
		if got := compareVersions(hi, lo); got != 1 {
			t.Errorf("compareVersions(%s, %s) = %d, want 1", c[1], c[0], got)
		}
		if got := compareVersions(lo, lo); got != 0 {
			t.Errorf("compareVersions(%s, %s) = %d, want 0", c[0], c[0], got)
		}
	}
}

// @latest selects the highest release tag that peels to a commit. Higher tags that peel to a tree
// or a blob are skipped and named, also through an annotated tag of an annotated tag. Names that
// are not release tags are not candidates.
func TestResolveLatest(t *testing.T) {
	r := newTestRepo(t)
	c1 := r.commit(map[string]string{"a": "1"})
	r.git("tag", "v1.9.0")
	c2 := r.commit(map[string]string{"a": "2"})
	r.git("tag", "-a", "-m", "release", "v1.10.0")
	tree := r.git("rev-parse", "HEAD^{tree}")
	blob := r.git("rev-parse", "HEAD:a")
	r.git("tag", "v2.0.0", tree)
	r.git("tag", "v2.1.0", blob)
	r.git("tag", "-a", "-m", "inner", "inner", tree)
	r.git("tag", "-a", "-m", "outer", "v3.0.0", "inner")
	c3 := r.commit(map[string]string{"a": "3"})
	for _, name := range []string{"v4.0.0-rc.1", "v05.0.0", "V9.0.0", "v9.0", "release-9.0.0", "v9.0.0+b"} {
		r.git("tag", name)
	}
	ctx := context.Background()

	got, err := resolveLatest(ctx, execGit, t.TempDir(), r.url())
	want := latestRelease{
		resolved: resolved{Commit: c2, Kind: "latest", FetchRef: "refs/tags/v1.10.0"},
		Tag:      "v1.10.0",
		Ignored: []string{
			"refs/tags/v3.0.0 points to a tree, not a commit",
			"refs/tags/v2.1.0 points to a blob, not a commit",
			"refs/tags/v2.0.0 points to a tree, not a commit",
		},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
	}

	// A lightweight tag to a commit; c1 is not the tip of a branch.
	r.git("tag", "v3.0.1", c1)
	got, err = resolveLatest(ctx, execGit, t.TempDir(), r.url())
	if err != nil || got.Commit != c1 || got.Tag != "v3.0.1" || got.FetchRef != "refs/tags/v3.0.1" || got.Ignored != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
	r.git("tag", "v10.0.0", c3)
	if got, err = resolveLatest(ctx, execGit, t.TempDir(), r.url()); err != nil || got.Commit != c3 || got.Tag != "v10.0.0" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// A repository without a release tag that points to a commit refuses @latest. There is no
// fallback to a branch.
func TestResolveLatestNone(t *testing.T) {
	r := newTestRepo(t)
	r.commit(map[string]string{"a": "1"})
	ctx := context.Background()
	const none = "the repository has no version tag vX.Y.Z that points to a commit"
	if _, err := resolveLatest(ctx, execGit, t.TempDir(), r.url()); err == nil || err.Error() != none {
		t.Fatalf("no tags: %v", err)
	}
	r.git("tag", "v1.0.0-rc.1")
	r.git("tag", "latest")
	r.git("branch", "v2.0.0")
	r.git("tag", "v1.0.0", r.git("rev-parse", "HEAD^{tree}"))
	if _, err := resolveLatest(ctx, execGit, t.TempDir(), r.url()); err == nil || err.Error() != none {
		t.Fatalf("no release tag to a commit: %v", err)
	}
}

// A failure is never taken as a tag to skip: when the fetch of the highest candidate fails, the
// resolution fails, and the lower release is not selected.
func TestResolveLatestFetchFails(t *testing.T) {
	r := newTestRepo(t)
	r.commit(map[string]string{"a": "1"})
	r.git("tag", "v1.0.0")
	// v2.0.0 names an object that the repository does not have.
	missing := strings.Repeat("ab", 20)
	if err := os.WriteFile(filepath.Join(r.dir, ".git", "refs", "tags", "v2.0.0"), []byte(missing+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.git("ls-remote", "--tags", r.url()), "refs/tags/v2.0.0") {
		t.Fatal("the fixture: ls-remote does not list v2.0.0")
	}
	got, err := resolveLatest(context.Background(), execGit, t.TempDir(), r.url())
	if err == nil || !strings.HasPrefix(err.Error(), "fetch refs/tags/v2.0.0: ") {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// A failure of ls-remote is the error of the resolution.
func TestResolveLatestRemoteFails(t *testing.T) {
	gitTestEnv(t)
	_, err := resolveLatest(context.Background(), execGit, t.TempDir(), "file://"+filepath.Join(t.TempDir(), "none"))
	if err == nil {
		t.Fatal("no error")
	}
}

// The selected release goes through the fetch path of the other refs: its tag ref is fetched, and
// a tag that moves between the resolution and the fetch is refused.
func TestResolveLatestFetch(t *testing.T) {
	r := newTestRepo(t)
	c := r.commit(map[string]string{"lazyrex.json": `{"name":"messaging","main":"m.lua"}`, "m.lua": ""})
	r.git("tag", "-a", "-m", "release", "v1.0.0")
	res, err := resolveLatest(context.Background(), execGit, t.TempDir(), r.url())
	if err != nil || res.Commit != c {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	if _, ft, err := fetchTest(t, r, res.FetchRef, res.Commit, false); err != nil || ft.Manifest.Name != "messaging" {
		t.Fatalf("fetch: %+v %v", ft, err)
	}
	r.commit(map[string]string{"m.lua": "moved"})
	r.git("tag", "-f", "-a", "-m", "moved", "v1.0.0")
	state, _, err := fetchTest(t, r, res.FetchRef, res.Commit, false)
	if err == nil || err.Error() != "ref refs/tags/v1.0.0 moved during the fetch; run the operation again" {
		t.Fatalf("moved: %v", err)
	}
	if p := published(t, state); len(p) != 0 {
		t.Fatalf("published: %v", p)
	}
}

// latestLockEntry is a lock entry that agrees with github:lazy-rex/Messaging@latest.
func latestLockEntry() *lockEntry {
	e := goodLockEntry()
	e.Ref, e.RefKind, e.Tag = "latest", "latest", "v1.2.0"
	return e
}

// The lock is version 2 only while it has a latest entry. Without one, its bytes are those of a
// lock before @latest: version 1 and no tag field. The last latest entry gone, the lock is version 1
// again.
func TestLockVersionFollowsContent(t *testing.T) {
	const src, latest = "github:lazy-rex/Messaging@main", "github:lazy-rex/Messaging@latest"
	v1 := "{\n  \"version\": 1,\n  \"plugins\": {\n    \"" + src + "\": {\n" +
		"      \"name\": \"messaging\",\n      \"owner\": \"lazy-rex\",\n      \"repo\": \"Messaging\",\n      \"subdir\": \"\",\n" +
		"      \"ref\": \"main\",\n      \"ref_kind\": \"branch\",\n      \"commit\": \"" + testCommit + "\",\n" +
		"      \"digest\": \"" + testDigest + "\",\n      \"dir\": \"github/lazy-rex/Messaging/" + testCommit + ".0123456789abcdef\"\n" +
		"    }\n  }\n}\n"
	l := &pluginLock{Version: lockVersionLatest, Plugins: map[string]*lockEntry{src: goodLockEntry()}}
	b, err := encodeLock(l)
	if err != nil || string(b) != v1 {
		t.Fatalf("no latest entry:\n%s\nwant\n%s", b, v1)
	}

	l.Plugins[latest] = latestLockEntry()
	b, err = encodeLock(l)
	if err != nil || !strings.HasPrefix(string(b), "{\n  \"version\": 2,") ||
		!strings.Contains(string(b), "\"ref_kind\": \"latest\",\n      \"tag\": \"v1.2.0\",") {
		t.Fatalf("a latest entry: %v\n%s", err, b)
	}
	if strings.Count(string(b), "\"tag\"") != 1 {
		t.Fatalf("a tag on a branch entry:\n%s", b)
	}
	got, err := decodeLock(b)
	if err != nil || got.Version != lockVersionLatest || !reflect.DeepEqual(got.Plugins, l.Plugins) {
		t.Fatalf("decode: %+v %v", got, err)
	}

	delete(l.Plugins, latest)
	if b, err = encodeLock(l); err != nil || string(b) != v1 {
		t.Fatalf("after the latest entry:\n%s", b)
	}
	// A version 2 lock without a latest entry reads; its next write is version 1.
	got, err = decodeLock([]byte(strings.Replace(v1, `"version": 1`, `"version": 2`, 1)))
	if err != nil || got.Version != lockVersionLatest {
		t.Fatalf("version 2 without a latest entry: %+v %v", got, err)
	}
	if b, err = encodeLock(got); err != nil || string(b) != v1 {
		t.Fatalf("rewrite:\n%s", b)
	}
}

// A version 1 lock has no latest form: an old hub would not read it as the new hub does.
func TestLockVersion1RefusesLatest(t *testing.T) {
	for name, f := range map[string]func(e *lockEntry){
		"ref_kind latest": func(e *lockEntry) { e.Tag = "" },
		"tag":             func(e *lockEntry) { e.RefKind = "branch" },
		"both":            func(e *lockEntry) {},
	} {
		e := latestLockEntry()
		f(e)
		raw, _ := json.Marshal(e)
		b := `{"version":1,"plugins":{"github:lazy-rex/Messaging@latest":` + string(raw) + `}}`
		_, err := decodeLock([]byte(b))
		if want := "the entry for github:lazy-rex/Messaging@latest: a latest entry needs version 2"; err == nil || err.Error() != want {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The tag of a latest entry is part of its pin: sync cannot rebuild it, so a missing or malformed
// tag is not trusted. A tag on another entry is a field that sync rebuilds (it drops the tag).
func TestLockLatestMetadata(t *testing.T) {
	latest := githubRef{Owner: "lazy-rex", Repo: "Messaging", Ref: "latest"}
	main := githubRef{Owner: "lazy-rex", Repo: "Messaging", Ref: "main"}
	if e := latestLockEntry(); e.trust(latest) != "" || e.agree(latest) != "" {
		t.Fatalf("a good latest entry: %q %q", e.trust(latest), e.agree(latest))
	}
	cases := []struct {
		name         string
		r            githubRef
		f            func(e *lockEntry)
		trust, field string
	}{
		{"no tag", latest, func(e *lockEntry) { e.Tag = "" }, "tag is missing", "commit"},
		{"pre-release tag", latest, func(e *lockEntry) { e.Tag = "v1.2.0-rc.1" }, "tag is not a version tag vX.Y.Z", "commit"},
		{"leading zero", latest, func(e *lockEntry) { e.Tag = "v01.2.0" }, "tag is not a version tag vX.Y.Z", "commit"},
		{"tag on a branch entry", main, func(e *lockEntry) { e.Ref, e.RefKind = "main", "branch" }, "", "tag"},
		{"ref_kind latest for main", main, func(e *lockEntry) { e.Ref = "main" }, "", "ref_kind"},
		{"ref latest of kind branch", latest, func(e *lockEntry) { e.RefKind, e.Tag = "branch", "" }, "", "ref_kind"},
		{"ref latest of kind tag", latest, func(e *lockEntry) { e.RefKind, e.Tag = "tag", "" }, "", "ref_kind"},
	}
	for _, c := range cases {
		e := latestLockEntry()
		c.f(e)
		if got := e.trust(c.r); got != c.trust {
			t.Errorf("%s: trust %q, want %q", c.name, got, c.trust)
		}
		if got := e.agree(c.r); got != c.field {
			t.Errorf("%s: agree %q, want %q", c.name, got, c.field)
		}
	}
	if got := refKinds("latest"); !reflect.DeepEqual(got, []string{"latest"}) {
		t.Errorf("refKinds(latest) = %v", got)
	}
	for _, ref := range []string{"refs/heads/latest", "refs/tags/latest", "Latest", "latest2"} {
		if slices.Contains(refKinds(ref), "latest") {
			t.Errorf("refKinds(%s) = %v", ref, refKinds(ref))
		}
	}
}

// testLatestPin is a valid document entry for github:o/r@latest.
func testLatestPin() pinEntry {
	p := testPin()
	p.Source, p.Ref, p.RefKind, p.Tag = "github:o/r@latest", "latest", "latest", "v1.2.0"
	return p
}

// The pin document is version 2 only with a latest entry, and a version 1 document has no latest
// form. Each malformed tag is refused with its own text.
func TestPinDocLatest(t *testing.T) {
	b, err := encodePinDoc(&pinDoc{Version: 2, Entries: []pinEntry{testPin()}})
	if err != nil || !strings.HasPrefix(string(b), "{\n  \"version\": 1,") || strings.Contains(string(b), "\"tag\"") {
		t.Fatalf("no latest entry: %v\n%s", err, b)
	}
	d := &pinDoc{Version: 1, Entries: []pinEntry{testLatestPin(), testPin()}}
	if b, err = encodePinDoc(d); err != nil || !strings.HasPrefix(string(b), "{\n  \"version\": 2,") {
		t.Fatalf("a latest entry: %v\n%s", err, b)
	}
	got, err := decodePinDoc(b)
	if err != nil || got.Version != 2 || len(got.Entries) != 2 || got.Entries[0] != testLatestPin() {
		t.Fatalf("decode: %+v %v", got, err)
	}
	v1 := strings.Replace(string(b), `"version": 2`, `"version": 1`, 1)
	if _, err := decodePinDoc([]byte(v1)); err == nil || err.Error() != "entry 1 (github:o/r@latest): a latest entry needs pin document version 2" {
		t.Errorf("version 1: %v", err)
	}

	for name, c := range map[string]struct {
		f    func(e *pinEntry)
		want string
	}{
		"no tag":             {func(e *pinEntry) { e.Tag = "" }, "tag is missing"},
		"pre-release":        {func(e *pinEntry) { e.Tag = "v1.2.0-rc.1" }, `tag "v1.2.0-rc.1" is not a version tag vX.Y.Z`},
		"build metadata":     {func(e *pinEntry) { e.Tag = "v1.2.0+b" }, `tag "v1.2.0+b" is not a version tag vX.Y.Z`},
		"tag on a branch":    {func(e *pinEntry) { e.Source, e.Ref, e.RefKind = "github:o/r@main", "main", "branch" }, "tag is set, but ref_kind is branch"},
		"latest for main":    {func(e *pinEntry) { e.Source, e.Ref = "github:o/r@main", "main" }, `ref_kind "latest" is not allowed for the ref main`},
		"legacy branch":      {func(e *pinEntry) { e.RefKind, e.Tag = "branch", "" }, `ref_kind "branch" is not allowed for the ref latest`},
		"full latest ref":    {func(e *pinEntry) { e.Source, e.Ref = "github:o/r@refs/tags/latest", "refs/tags/latest" }, `ref_kind "latest" is not allowed for the ref refs/tags/latest`},
		"ref does not match": {func(e *pinEntry) { e.Ref = "main" }, "ref does not match the source"},
	} {
		e := testLatestPin()
		c.f(&e)
		b, _ := encodePinDoc(&pinDoc{Entries: []pinEntry{e}})
		_, err := decodePinDoc(b)
		if err == nil || !strings.HasSuffix(err.Error(), ": "+c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}
