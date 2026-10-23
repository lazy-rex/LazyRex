package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// listVersionTags keeps the release tags of an ls-remote listing, newest first. Each has the ID
// that ls-remote gives: the peeled ID (the ^{} line) of an annotated tag, else the tag's own ID.
// Names that are not release tags are left out.
func TestListVersionTags(t *testing.T) {
	out := "" +
		"1111111111111111111111111111111111111111\trefs/tags/v1.0.0\n" +
		"2222222222222222222222222222222222222222\trefs/tags/v1.10.0\n" +
		"3333333333333333333333333333333333333333\trefs/tags/v1.10.0^{}\n" +
		"4444444444444444444444444444444444444444\trefs/tags/v2.0.0-rc.1\n" +
		"5555555555555555555555555555555555555555\trefs/tags/v1.2.0\n" +
		"6666666666666666666666666666666666666666\trefs/tags/notes\n"
	var got []versionTagRef
	for _, r := range listVersionTags([]byte(out)) {
		r.v = tagVersion{}
		got = append(got, r)
	}
	want := []versionTagRef{
		{tag: "v1.10.0", object: "3333333333333333333333333333333333333333"},
		{tag: "v1.2.0", object: "5555555555555555555555555555555555555555"},
		{tag: "v1.0.0", object: "1111111111111111111111111111111111111111"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// A tags listing and its latest: v9.0.0 points to a tree and v8.0.0 to a commit. The list has both,
// with their ls-remote IDs; latest is v8.0.0, the release that resolveLatest gives for the same
// repository, with the note for v9.0.0.
func TestTagsLatestMatchesResolve(t *testing.T) {
	r := newTestRepo(t)
	c := r.commit(map[string]string{"a": "1"})
	r.git("tag", "v8.0.0")
	tree := r.git("rev-parse", "HEAD^{tree}")
	r.git("tag", "v9.0.0", tree)
	ctx := context.Background()
	want, err := resolveLatest(ctx, execGit, t.TempDir(), r.url())
	if err != nil || want.Tag != "v8.0.0" || want.Commit != c {
		t.Fatalf("resolveLatest: %+v %v", want, err)
	}
	got, err := loadTags(ctx, execGit, t.TempDir(), r.url())
	if err != nil {
		t.Fatal(err)
	}
	list := []map[string]string{{"tag": "v9.0.0", "object": tree}, {"tag": "v8.0.0", "object": c}}
	if !reflect.DeepEqual(got.Tags, list) || got.Latest == nil || got.Latest.Tag != want.Tag || got.Latest.Commit != want.Commit ||
		!reflect.DeepEqual(got.Ignored, want.Ignored) {
		t.Fatalf("got %+v; want the list %v and the latest %+v", got, list, want)
	}
}

// hub.plugins.tags: the first request starts a load and says loading; the ready reply has the
// list, latest and read_at, from the repository of the source (the ref and the subdir do not
// change the key). A source that is not a GitHub source is invalid_input.
func TestPluginTagsAction(t *testing.T) {
	r := newTestRepo(t)
	c := r.commit(map[string]string{"a": "1"})
	r.git("tag", "v1.0.0")
	h := fetchTestHub(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); h.loads.wait() }) // no load notice outlives the test fakes
	h.loadsStart(ctx)
	ask := func(src string) map[string]any {
		t.Helper()
		v, err := h.pluginTags(rawJSON(map[string]any{"source": src}))
		if err != nil {
			t.Fatal(err)
		}
		return v.(map[string]any)
	}
	if v := ask("github:acme/notes@v1.0.0"); v["state"] != "loading" {
		t.Fatalf("the first request: %v", v)
	}
	var v map[string]any
	for end := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if v = ask("github:Acme/Notes/sub@main"); v["state"] != "loading" || time.Now().After(end) {
			break
		}
	}
	want := []map[string]string{{"tag": "v1.0.0", "object": c}}
	if v["state"] != "ready" || !reflect.DeepEqual(v["tags"], want) || !reflect.DeepEqual(v["latest"], map[string]string{"tag": "v1.0.0", "commit": c}) || v["read_at"].(int64) == 0 {
		t.Fatalf("ready: %v", v)
	}
	for _, bad := range []string{"src/local", "github:acme", ""} {
		if _, err := h.pluginTags(rawJSON(map[string]any{"source": bad})); !isRefusal(err, "invalid_input") {
			t.Errorf("source %q: %v", bad, err)
		}
	}
	if _, err := h.pluginTags(rawJSON(map[string]any{"source": "github:acme/notes@v1", "x": 1})); !isRefusal(err, "invalid_input") {
		t.Errorf("an unknown field: %v", err)
	}
}

// The ls-remote output is read through lsRemoteMax: the load stops reading after the bound (the
// fake output has no end), fails, and does not keep a partial list.
func TestLoadTagsOutputBound(t *testing.T) {
	line := strings.Repeat("a", 40) + "\trefs/tags/v1.0.0\n"
	var n int64
	git := func(ctx context.Context, dir string, out func(io.Reader) error, args ...string) ([]byte, error) {
		if out == nil {
			return nil, errors.New("the fake runs only ls-remote")
		}
		// The fake ends with an error after 4 MiB, so a read with no bound fails here, not by a hang.
		endless := io.MultiReader(strings.NewReader(strings.Repeat(line, lsRemoteMax/len(line)+1)), io.LimitReader(zeroReader{}, 4*lsRemoteMax),
			iotest.ErrReader(errors.New("the read did not stop at the bound")))
		return nil, out(countingReader{endless, &n})
	}
	if _, err := loadTags(context.Background(), git, t.TempDir(), "u"); err == nil || !strings.Contains(err.Error(), "too many tags") {
		t.Fatalf("over the bound: %v", err)
	}
	if n > 2*lsRemoteMax {
		t.Fatalf("over the bound: the load read %d bytes", n)
	}
}
