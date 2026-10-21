package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pull into the config of a running hub (section "Pin sync"): the first pull of equal pins sets
// the base; after an update on the other machine, the pull takes its commit. hub.plugins.reload
// then shows the candidate, and the instance keeps its run commit.
func TestPinSyncPullLab(t *testing.T) {
	s := newStartLab(t)
	gitTestEnv(t)
	r := newTestRepo(t)
	first := r.commit(map[string]string{"lazyrex.json": `{"name":"gh","main":"main.lua"}`, "main.lua": cacheFixture, "name.lua": "return 'gh'\n"})
	const src = "github:lab/gh@main"
	s.setConfig("plugins", []any{map[string]any{"source": src}})
	opts := pluginCmdOpts{remote: func(owner, repo string) string { return r.url() }}
	if err := pluginSync(context.Background(), s.cfg, opts); err != nil {
		t.Fatal(err)
	}
	// The other machine: its own config directory and state, the same repository.
	other := newCmdEnv(t)
	other.opts = opts
	cfgO := other.config(src)
	if err := other.sync(cfgO); err != nil {
		t.Fatal(err)
	}
	pull := func(want string) {
		t.Helper()
		doc := filepath.Join(t.TempDir(), "pins.json")
		os.WriteFile(doc, []byte(other.mustExport(cfgO)), 0o600)
		var out bytes.Buffer
		if conflict, err := pluginPull(context.Background(), s.cfg, doc, nil, nil, opts, &out); err != nil || conflict || !strings.HasPrefix(out.String(), want+"\t") {
			t.Fatalf("pull: %q %v %v, want %s", out.String(), conflict, err, want)
		}
	}
	pull(pullUnchanged)

	s.start()
	old, _ := s.startRun("gh")
	entry := func() map[string]any {
		t.Helper()
		b, _ := json.Marshal(s.must("hub.plugins", nil)["entries"])
		var list []map[string]any
		json.Unmarshal(b, &list)
		for _, e := range list {
			if e["source"] == src {
				return e
			}
		}
		t.Fatalf("no entry: %s", b)
		return nil
	}
	if e := entry(); e["commit"] != first {
		t.Fatalf("the first run: %v", e)
	}

	second := r.commit(map[string]string{"name.lua": "return 'gh' -- second\n"})
	if err := other.update(cfgO); err != nil {
		t.Fatal(err)
	}
	pull(pullTaken)
	s.must("hub.plugins.reload", nil)
	e := entry()
	cand, _ := e["candidate"].(map[string]any)
	if e["commit"] != first || cand["commit"] != second {
		t.Fatalf("after the pull and the reload: %v", e)
	}
	if st := s.status("gh"); st.InstanceID != old.InstanceID || !st.Registered {
		t.Fatalf("the pull changed the instance: %+v, old %+v", st, old)
	}
}
