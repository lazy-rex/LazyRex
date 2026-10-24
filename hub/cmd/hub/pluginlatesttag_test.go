package main

// Tests of the release tag of an @latest run in hub.plugins (docs/plugin-sources.md, section 7):
// the entry and a kept record report the tag of their run, and the candidate reports a tag that
// differs from the run's, with null for a candidate without a tag.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// The candidate tag: absent when equal (both absent or the same string), a string when it
// differs, null when the candidate has none and the run has one. A tag-only change is a change.
func TestCandidateChangeTag(t *testing.T) {
	for _, tc := range []struct {
		run, cand pluginRun
		want      string
	}{
		{pluginRun{Commit: "c1", Dir: "/d1", Tag: "v1.0.0"}, pluginRun{Commit: "c1", Dir: "/d1", Tag: "v1.0.0"}, "null"},
		{pluginRun{Commit: "c1", Dir: "/d1"}, pluginRun{Commit: "c1", Dir: "/d1"}, "null"},
		{pluginRun{Commit: "c1", Dir: "/d1", Tag: "v1.0.0"}, pluginRun{Commit: "c1", Dir: "/d1"}, `{"tag":null}`},
		{pluginRun{Commit: "c1", Dir: "/d1"}, pluginRun{Commit: "c1", Dir: "/d1", Tag: "v1.0.0"}, `{"tag":"v1.0.0"}`},
		{pluginRun{Commit: "c1", Dir: "/d1", Tag: "v1.0.0"}, pluginRun{Commit: "c1", Dir: "/d1", Tag: "v1.1.0"}, `{"tag":"v1.1.0"}`},
		{pluginRun{Commit: "c1", Dir: "/d1", Tag: "v1.0.0"}, pluginRun{Commit: "c2", Dir: "/d2", Tag: "v2.0.0"}, `{"commit":"c2","dir":"/d2","tag":"v2.0.0"}`},
		{pluginRun{Commit: "c1", Dir: "/d1", Tag: "v1.0.0"}, pluginRun{Dir: "/local"}, `{"commit":null,"dir":"/local","tag":null}`},
	} {
		if b, _ := json.Marshal(candidateChange(&tc.cand, &tc.run)); string(b) != tc.want {
			t.Errorf("run %+v, candidate %+v: %s, want %s", tc.run, tc.cand, b, tc.want)
		}
	}
}

// hub.plugins: an entry and a kept record of a latest run report its tag; any other entry has no
// tag key (not null, not "").
func TestPluginsListTag(t *testing.T) {
	h, _ := workerTestHub(t)
	es := []*configEntry{
		{index: 1, Source: "github:o/a@latest", name: "a", seen: "a", cand: &pluginRun{Name: "a", Commit: "c1", Dir: "/d1", Tag: "v1.0.0"}},
		{index: 2, Source: "github:o/b@main", name: "b", seen: "b", cand: &pluginRun{Name: "b", Commit: "c2", Dir: "/d2"}},
		{index: 3, Source: "github:o/c@main", name: "c", seen: "c", cand: &pluginRun{Name: "c", Commit: "c3", Dir: "/d3"}},
	}
	h.entries = es
	h.plugins = map[string]*Plugin{
		"a": {Name: "a", entry: es[0], Desired: "stopped", run: pluginRun{Name: "a", Commit: "c1", Dir: "/d1", Tag: "v1.0.0"}},
		"b": {Name: "b", entry: es[1], Desired: "stopped", run: pluginRun{Name: "b", Commit: "c2", Dir: "/d2"}},
		// c ran a latest release, and its source is a branch now.
		"c": {Name: "c", entry: es[2], Desired: "stopped", run: pluginRun{Name: "c", Commit: "c3", Dir: "/d3", Tag: "v3.0.0"}},
		"k": {Name: "k", source: "github:o/k@latest", Desired: "running", run: pluginRun{Name: "k", Commit: "ck", Dir: "/dk", Tag: "v0.1.0"}},
	}
	out, err := h.pluginsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var got struct{ Entries, Kept []map[string]any }
	json.Unmarshal(b, &got)
	if v, ok := got.Entries[0]["tag"]; !ok || v != "v1.0.0" || got.Entries[0]["candidate"] != nil {
		t.Errorf("latest entry: %v", got.Entries[0])
	}
	if _, ok := got.Entries[1]["tag"]; ok {
		t.Errorf("branch entry has a tag key: %v", got.Entries[1])
	}
	c, _ := got.Entries[2]["candidate"].(map[string]any)
	if v, ok := c["tag"]; got.Entries[2]["tag"] != "v3.0.0" || !ok || v != nil || len(c) != 1 {
		t.Errorf("latest run with a branch candidate: %v", got.Entries[2])
	}
	if len(got.Kept) != 1 || got.Kept[0]["tag"] != "v0.1.0" {
		t.Errorf("kept %v", got.Kept)
	}
}

// The tag belongs to the run, not to the current lock: a resolution sets the candidate's tag from
// the lock; a saved run keeps its tag across a load; an update to a new release shows the new tag
// in the candidate only. A saved run from before the tag field has no tag, and a newer lock does
// not give it one.
func TestLatestTagRunAndReload(t *testing.T) {
	env := newCmdEnv(t)
	r := env.repo("lazy-rex", "Messaging", "messaging")
	c1 := r.commit(map[string]string{"main.lua": "1"})
	r.git("tag", "v1.0.0")
	env.managed("messaging=" + srcLatest)
	cfg := env.config()
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := partBHub(t, env.load(cfg), db)
	h.entriesMu.Lock()
	h.resolveLocked()
	h.entriesMu.Unlock()
	e, pl := h.entries[0], h.plugins["messaging"]
	if e.cand == nil || e.cand.Commit != c1 || e.cand.Tag != "v1.0.0" || pl == nil {
		t.Fatalf("first resolution: %+v %v", e.cand, e.err)
	}
	pl.run = *e.cand // as spawn does
	if err := h.savePlugin(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	if got := entryItem(t, h); got["tag"] != "v1.0.0" || got["commit"] != c1 || got["candidate"] != nil {
		t.Errorf("after the spawn: %v", got)
	}

	c2 := r.commit(map[string]string{"main.lua": "2"})
	r.git("tag", "v2.0.0")
	if err := env.updateOp(cfg, "messaging"); err != nil {
		t.Fatal(err)
	}
	h.entriesMu.Lock()
	h.resolveLocked()
	h.entriesMu.Unlock()
	got := entryItem(t, h)
	c, _ := got["candidate"].(map[string]any)
	if got["tag"] != "v1.0.0" || got["commit"] != c1 || c["tag"] != "v2.0.0" || c["commit"] != c2 {
		t.Errorf("after the update: %v", got)
	}

	// A new hub on the same store: the loaded run keeps its tag; the candidate is the new release.
	h2 := partBHub(t, env.load(cfg), db)
	h2.entriesMu.Lock()
	h2.resolveLocked()
	h2.entriesMu.Unlock()
	if pl2 := h2.plugins["messaging"]; pl2.run.Tag != "v1.0.0" || h2.entries[0].cand.Tag != "v2.0.0" {
		t.Errorf("after a load: run %+v, candidate %+v", pl2.run, h2.entries[0].cand)
	}

	// A run saved before the tag field: no tag, also with a lock that has one.
	pl2 := h2.plugins["messaging"]
	pl2.run.Tag = ""
	if err := h2.savePlugin(context.Background(), pl2); err != nil {
		t.Fatal(err)
	}
	h3 := partBHub(t, env.load(cfg), db)
	h3.entriesMu.Lock()
	h3.resolveLocked()
	h3.entriesMu.Unlock()
	got = entryItem(t, h3)
	c, _ = got["candidate"].(map[string]any)
	if _, ok := got["tag"]; ok || h3.plugins["messaging"].run.Tag != "" || c["tag"] != "v2.0.0" {
		t.Errorf("an old saved run: %v", got)
	}
}

// The hub resolution of a latest source, a pinned tag source and a branch source: only the latest
// candidate has a tag; the pinned tag source has its tag name as its ref and no tag.
func TestLatestCandidateTag(t *testing.T) {
	env := newCmdEnv(t)
	rm := env.repo("lazy-rex", "Messaging", "messaging")
	cm := rm.commit(map[string]string{"main.lua": "1"})
	rm.git("tag", "v1.0.0")
	rp := env.repo("lazy-rex", "Profiles", "profiles")
	cp := rp.commit(map[string]string{"main.lua": "1"})
	rp.git("tag", "v0.2.0")
	rw := env.repo("lazy-rex", "Worktrees", "worktrees")
	cw := rw.git("rev-parse", "HEAD")
	srcP, srcW := "github:lazy-rex/Profiles@v0.2.0", "github:lazy-rex/Worktrees@main"
	env.managed("messaging="+srcLatest, "profiles="+srcP, "worktrees="+srcW)
	cfg := env.config()
	if err := env.install(cfg); err != nil {
		t.Fatal(err)
	}
	c := env.load(cfg)
	resolve(&c)
	commits := []string{cm, cp, cw}
	for i, wantTag := range []string{"v1.0.0", "", ""} {
		if e := c.entries[i]; e.err != nil || e.cand == nil || e.cand.Tag != wantTag || e.cand.Commit != commits[i] {
			t.Errorf("entry %d: candidate %+v, error %v, want tag %q at %s", i+1, e.cand, e.err, wantTag, commits[i])
		}
	}
}
