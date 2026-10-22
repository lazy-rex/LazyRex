package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

	got, err := resolveLatest(ctx, t.TempDir(), r.url())
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
	got, err = resolveLatest(ctx, t.TempDir(), r.url())
	if err != nil || got.Commit != c1 || got.Tag != "v3.0.1" || got.FetchRef != "refs/tags/v3.0.1" || got.Ignored != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
	r.git("tag", "v10.0.0", c3)
	if got, err = resolveLatest(ctx, t.TempDir(), r.url()); err != nil || got.Commit != c3 || got.Tag != "v10.0.0" {
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
	if _, err := resolveLatest(ctx, t.TempDir(), r.url()); err == nil || err.Error() != none {
		t.Fatalf("no tags: %v", err)
	}
	r.git("tag", "v1.0.0-rc.1")
	r.git("tag", "latest")
	r.git("branch", "v2.0.0")
	r.git("tag", "v1.0.0", r.git("rev-parse", "HEAD^{tree}"))
	if _, err := resolveLatest(ctx, t.TempDir(), r.url()); err == nil || err.Error() != none {
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
	got, err := resolveLatest(context.Background(), t.TempDir(), r.url())
	if err == nil || !strings.HasPrefix(err.Error(), "fetch refs/tags/v2.0.0: ") {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// A failure of ls-remote is the error of the resolution.
func TestResolveLatestRemoteFails(t *testing.T) {
	gitTestEnv(t)
	_, err := resolveLatest(context.Background(), t.TempDir(), "file://"+filepath.Join(t.TempDir(), "none"))
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
	res, err := resolveLatest(context.Background(), t.TempDir(), r.url())
	if err != nil || res.Commit != c {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	if _, ft, err := fetchTest(t, r, res.FetchRef, res.Commit, false); err != nil || ft.Manifest.Name != "messaging" {
		t.Fatalf("fetch: %+v %v", ft, err)
	}
	r.commit(map[string]string{"m.lua": "moved"})
	r.git("tag", "-f", "-a", "-m", "moved", "v1.0.0")
	state, _, err := fetchTest(t, r, res.FetchRef, res.Commit, false)
	if err == nil || err.Error() != "ref refs/tags/v1.0.0 moved during the fetch; run the command again" {
		t.Fatalf("moved: %v", err)
	}
	if p := published(t, state); len(p) != 0 {
		t.Fatalf("published: %v", p)
	}
}
