package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The plugin release steps (plan c4a) in the fake world of release_hub_test.go: P is the managed
// plugin "other" (github:o/other@main), FROM is its lock pin C, and TO is pluginTo. The hub binary
// and its SDK directory do not change.

const (
	pluginTo       = "3333333333333333333333333333333333333333"
	pluginToDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

var (
	pinFrom = relPin{relCommitC, relDigestC}
	pinTo   = relPin{pluginTo, pluginToDigest}
)

// newPluginWorld is the fake world with a plugin release spec for "other". sdk is the manifest
// sdk field of P at FROM and TO ("" or "0").
func newPluginWorld(t *testing.T, sdk string) *relWorld {
	t.Helper()
	w := newRelWorld(t)
	w.sdkDir = filepath.Join(w.dir, "sdk")
	os.MkdirAll(w.sdkDir, 0o700)
	os.WriteFile(filepath.Join(w.sdkDir, "lazyrex.lua"), []byte("return {}\n"), 0o600)
	os.WriteFile(filepath.Join(w.sdkDir, "boot.lua"), []byte("return dofile(rex.args.main)\n"), 0o600)
	w.buildRec = []string{"go go1.27.2", "build vcs.revision=" + commitOld}
	l, _ := readLock(lockPath(w.cfg, ""))
	w.manifest(l.Plugins[otherSrc], sdk)
	m := map[string]string{"name": "other", "main": "main.lua"}
	if sdk != "" {
		m["sdk"] = sdk
	}
	b, _ := json.Marshal(m)
	w.toManifest = string(b)
	w.setSpec(nil)
	return w
}

// setSpec writes the plugin spec, with the fields of extra changed.
func (w *relWorld) setSpec(extra map[string]any) {
	spec := map[string]any{"kind": "plugin", "plugin": "other", "to": pluginTo, "config": w.cfg, "server": "unix:///nowhere/rex.sock"}
	for k, v := range extra {
		spec[k] = v
	}
	b, _ := json.Marshal(spec)
	os.WriteFile(w.specP, b, 0o600)
}

// reachGit is git in <run>/reach.git: init, the ref fetch, the tag commit and the ancestry check.
func (w *relWorld) reachGit(a []string) ([]byte, []byte, error) {
	switch a[0] {
	case "init", "fetch":
		return nil, nil, nil
	case "rev-parse":
		return []byte(w.reach + "\n"), nil, nil
	case "merge-base":
		switch w.reach {
		case "off":
			return nil, nil, exitErr(1)
		case "bad":
			return nil, []byte("fatal: Not a valid commit name " + a[2]), exitErr(128)
		}
		return nil, nil, nil
	}
	return nil, []byte("fake reach git: " + strings.Join(a, " ")), exitErr(2)
}

// clientPerform is client.perform of view.seen to a plugin client. The answer is the next entry of
// w.perform (default refused, the SDK 0 refusal). A client that is not the current one is gone.
func (w *relWorld) clientPerform(arg map[string]any) ([]byte, []byte, error) {
	w.performN++
	mode := "refused"
	if len(w.perform) > 0 {
		mode, w.perform = w.perform[0], w.perform[1:]
	}
	if r := w.runs["other"]; r == nil || arg["client_id"] != "client:"+r.instance {
		mode = "gone"
	}
	// The hook runs after the answer is chosen: the instance can change after the plugin answered.
	if w.performHook != nil {
		w.performHook(w.performN)
	}
	fail := func(msg string, status int, code string) ([]byte, []byte, error) {
		return nil, []byte(fmt.Sprintf("Error: stream response \"control-1\" failed: %s (status=%d code=%s)\n", msg, status, code)), exitErr(1)
	}
	switch mode {
	case "refused":
		return fail("Only the hub calls view.seen", 403, "refused")
	case "other":
		return fail("Unknown user", 403, "refused")
	case "timeout":
		return fail(`"lazyrex-plugin:other" did not answer within 30s`, 504, "timeout")
	case "gone":
		return fail(`"lazyrex-plugin:other" is waiting to reconnect`, 409, "client_unavailable")
	}
	return []byte(`{"result":{}}`), nil, nil
}

// stepErr runs a step and requires a refusal that contains want.
func (w *relWorld) stepErr(name, want string) {
	w.t.Helper()
	err := w.step(name)
	if err == nil || !strings.Contains(err.Error(), want) {
		w.t.Fatalf("step %s: %v, want %q\n%s", name, err, want, w.out)
	}
}

// The forward release of a plugin and its rollback: one replace each way, the hub is never
// restarted, and the start barrier runs for the SDK plugin in precheck, verify and rollback.
func TestReleasePluginForwardRollback(t *testing.T) {
	w := newPluginWorld(t, "0")
	w.freeze()
	c := mustCandidate(t, w)
	if c.Kind != "plugin" || c.Source != otherSrc || c.From != pinFrom || c.To != pinTo || c.HubCommit != commitOld ||
		c.HubSHA256 == "" || c.SDKDir != w.sdkDir || c.SDKDigest == "" || c.FromSDK != "0" || c.ToSDK != "0" || len(c.HubBuild) != 2 {
		t.Fatalf("candidate %+v", c)
	}
	hub := w.hubPID
	w.must("precheck", "backup", "pull", "replace", "verify")
	if got := w.lockPin(otherSrc); got != pinTo {
		t.Fatalf("lock pin %v", got)
	}
	if r := w.runs["other"]; r.commit != pluginTo || w.replaceCalls != 1 || w.hubPID != hub {
		t.Fatalf("after forward: run %+v, %d replaces, hub %d (was %d)", r, w.replaceCalls, w.hubPID, hub)
	}
	if w.performN != 2 {
		t.Fatalf("%d view.seen calls, want 2 (precheck, verify)", w.performN)
	}
	s, _ := os.ReadFile(filepath.Join(w.run, relStampFile))
	if _, err := os.Stat(filepath.Join(w.run, "backup-"+strings.TrimSpace(string(s)), bakDB)); err != nil {
		t.Fatalf("the plugin backup has no hub.db: %v", err)
	}
	w.must("rollback")
	if got := w.lockPin(otherSrc); got != pinFrom {
		t.Fatalf("lock pin after rollback %v", got)
	}
	if r := w.runs["other"]; r.commit != relCommitC || w.replaceCalls != 2 || w.hubPID != hub || w.performN != 3 {
		t.Fatalf("after rollback: run %+v, %d replaces, hub %d, %d view.seen", r, w.replaceCalls, w.hubPID, w.performN)
	}
	if _, err := os.Stat(filepath.Join(w.run, relStampFile)); err == nil {
		t.Fatal("the stamp is not set aside")
	}
}

// A plugin without "sdk": "0" gets no barrier call; the steps say that the start completion is not
// established.
func TestReleasePluginNoSDKNoBarrier(t *testing.T) {
	w := newPluginWorld(t, "")
	w.freeze()
	w.must("precheck", "backup", "pull", "replace", "verify", "rollback")
	if w.performN != 0 || !strings.Contains(w.out.String(), "start completion of other: not established") {
		t.Fatalf("%d view.seen calls\n%s", w.performN, w.out)
	}
}

// The dev pane steps of a hub release are not steps of a plugin release.
func TestReleasePluginHubOnlySteps(t *testing.T) {
	w := newPluginWorld(t, "0")
	w.freeze()
	for _, s := range []string{"stop", "checkout", "start", "resume"} {
		w.stepErr(s, "not a step of a plugin release")
	}
}

func TestReleasePluginFreezeRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(w *relWorld)
		want string
	}{
		{"unknown plugin", func(w *relWorld) { w.setSpec(map[string]any{"plugin": "nope"}) }, "no config or managed entry"},
		{"home plugin", func(w *relWorld) { w.setSpec(map[string]any{"plugin": "lazyrex"}) }, "is the home plugin"},
		{"commit ref", func(w *relWorld) {
			l, _ := readLock(lockPath(w.cfg, ""))
			l.Plugins[otherSrc].RefKind = "commit"
			writeLock(lockPath(w.cfg, ""), l)
		}, "has a commit ref"},
		{"no change", func(w *relWorld) { w.setSpec(map[string]any{"to": relCommitC}) }, "a release without a change"},
		{"base", func(w *relWorld) {
			b, _ := readPeers(lockPath(w.cfg, ""))
			b.Peers[pinHub][otherSrc] = basePin{Commit: pinCommit, Digest: pinDigest}
			writePeers(lockPath(w.cfg, ""), b)
		}, "the base of"},
		{"not on the branch", func(w *relWorld) { w.reach = "off" }, "is not on the branch main"},
		{"unknown commit", func(w *relWorld) { w.reach = "bad" }, "is not reachable from the branch main"},
		{"tag", func(w *relWorld) {
			l, _ := readLock(lockPath(w.cfg, ""))
			l.Plugins[otherSrc].RefKind, l.Plugins[otherSrc].Ref = "tag", "v1.0.0"
			writeLock(lockPath(w.cfg, ""), l)
			w.reach = relCommitB
		}, "the tag v1.0.0"},
		{"name change", func(w *relWorld) { w.toManifest = `{"name":"renamed","main":"main.lua","sdk":"0"}` }, `name changes from "other" to "renamed"`},
		{"bad manifest", func(w *relWorld) { w.toManifest = `{` }, "the manifest at TO"},
		{"no -sdk", func(w *relWorld) { w.sdkDir = "" }, "runs without -sdk"},
		{"no hub", func(w *relWorld) { w.stopHub() }, "does not run"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newPluginWorld(t, "0")
			c.set(w)
			if err := createRun(w.run, w.specP); err != nil {
				t.Fatal(err)
			}
			w.stepErr("freeze", c.want)
		})
	}
	// A tag ref at TO passes.
	w := newPluginWorld(t, "0")
	l, _ := readLock(lockPath(w.cfg, ""))
	l.Plugins[otherSrc].RefKind, l.Plugins[otherSrc].Ref = "tag", "v1.0.0"
	writeLock(lockPath(w.cfg, ""), l)
	w.reach = pluginTo
	w.freeze()
}

func TestReleasePluginManifestReasons(t *testing.T) {
	from := `{"name":"p","main":"main.lua","sdk":"0","depends":["a"],"test":{"args":{"x":"1"}}}`
	for _, c := range []struct {
		to   string
		want []string
	}{
		{`{"name":"p","main":"main.lua","sdk":"0","depends":["a"],"test":{"args":{"x":"2"}}}`, nil},
		{`{"depends":["a"],"sdk":"0","main":"main.lua","name":"p"}`, nil},
		{`{"name":"p","main":"main.lua","depends":["a"]}`, []string{"the manifest field sdk changes"}},
		{`{"name":"p","main":"init.lua","sdk":"0","depends":["a","b"],"routes":["/x"]}`, []string{"the manifest field depends changes",
			"the manifest field main changes", "the manifest field routes changes (a field the release tool does not know)"}},
	} {
		_, _, got, err := manifestReasons([]byte(from), []byte(c.to))
		if err != nil || strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: %q %v, want %q", c.to, got, err, c.want)
		}
	}
}

// The hub gate (plan c4 v2 section 3, A2): another binary, another SDK directory content, or
// another hub generation stops a forward step before its mutation. Rollback refuses another binary,
// and accepts another generation of the same binary.
func TestReleasePluginHubGate(t *testing.T) {
	devHub := func(w *relWorld) string { return filepath.Join(w.state, "dev", "hub") }
	t.Run("binary changes before replace", func(t *testing.T) {
		w := newPluginWorld(t, "0")
		w.freeze()
		w.must("precheck", "backup", "pull")
		os.WriteFile(devHub(w), []byte("another hub"), 0o700)
		w.stepErr("replace", "not the frozen hub")
		if w.replaceCalls != 0 {
			t.Fatalf("%d replace calls", w.replaceCalls)
		}
	})
	t.Run("hub restart before replace", func(t *testing.T) {
		w := newPluginWorld(t, "0")
		w.freeze()
		w.must("precheck", "backup", "pull")
		w.stopHub()
		w.startHub()
		w.stepErr("replace", "the hub restarted after precheck")
		if w.replaceCalls != 0 {
			t.Fatalf("%d replace calls", w.replaceCalls)
		}
	})
	t.Run("sdk changes before verify", func(t *testing.T) {
		w := newPluginWorld(t, "0")
		w.freeze()
		w.must("precheck", "backup", "pull", "replace")
		os.WriteFile(filepath.Join(w.sdkDir, "lazyrex.lua"), []byte("return {changed = true}\n"), 0o600)
		w.stepErr("verify", "the SDK directory")
	})
	t.Run("sdk path changes", func(t *testing.T) {
		w := newPluginWorld(t, "0")
		w.freeze()
		w.must("precheck", "backup")
		other := filepath.Join(w.dir, "sdk2")
		os.MkdirAll(other, 0o700)
		// The same files: only the path differs.
		for _, n := range []string{"lazyrex.lua", "boot.lua"} {
			os.WriteFile(filepath.Join(other, n), mustRead(filepath.Join(w.sdkDir, n)), 0o600)
		}
		w.sdkDir = other
		w.stepErr("pull", "not the frozen")
		if got := w.lockPin(otherSrc); got != pinFrom {
			t.Fatalf("the pin moved: %v", got)
		}
	})
	t.Run("another binary before rollback", func(t *testing.T) {
		w := newPluginWorld(t, "0")
		w.freeze()
		w.must("precheck", "backup", "pull", "replace")
		os.WriteFile(devHub(w), []byte("another hub"), 0o700)
		w.stepErr("rollback", "not the frozen hub")
		if got := w.lockPin(otherSrc); got != pinTo || w.replaceCalls != 1 {
			t.Fatalf("rollback mutated: pin %v, %d replaces", got, w.replaceCalls)
		}
	})
	t.Run("same binary, new generation before rollback", func(t *testing.T) {
		w := newPluginWorld(t, "0")
		w.freeze()
		w.must("precheck", "backup", "pull", "replace")
		w.stopHub()
		w.startHub() // P starts again at its lock pin TO
		w.must("rollback")
		if got := w.lockPin(otherSrc); got != pinFrom || w.runs["other"].commit != relCommitC {
			t.Fatalf("pin %v, run %+v", got, w.runs["other"])
		}
		if !strings.Contains(string(mustRead(filepath.Join(w.run, "journal.jsonl"))), `"op":"hub-generation"`) {
			t.Fatal("the new generation is not journaled")
		}
	})
	t.Run("live hash after the rehearsal", func(t *testing.T) {
		w := newPluginWorld(t, "0")
		w.freeze()
		os.WriteFile(devHub(w), []byte("dev hub of "+commitOld+", rebuilt"), 0o700)
		w.stepErr("precheck", "not the frozen hub")
	})
}

// The runtime rows of a plugin rollback (plan c4 v2 section 5).
func TestReleasePluginRollbackRows(t *testing.T) {
	forward := func(t *testing.T, steps ...string) *relWorld {
		w := newPluginWorld(t, "0")
		w.freeze()
		w.must(append([]string{"precheck", "backup"}, steps...)...)
		return w
	}
	t.Run("a: P still runs FROM", func(t *testing.T) {
		w := forward(t, "pull")
		w.must("rollback")
		if w.replaceCalls != 0 || w.startCalls != 1 || w.runs["other"].commit != relCommitC {
			t.Fatalf("%d replaces, %d starts, run %+v", w.replaceCalls, w.startCalls, w.runs["other"])
		}
	})
	t.Run("b: P runs TO, observed instance", func(t *testing.T) {
		w := forward(t, "pull", "replace")
		w.runs["other"].instance = "inst-crashed-and-respawned" // a crash respawn after forward
		w.event(map[string]any{"event": "plugin_registered", "plugin": "other", "instance_id": "inst-crashed-and-respawned", "client_id": "client:inst-crashed-and-respawned"})
		w.must("rollback")
		last := w.replaceArgs[len(w.replaceArgs)-1]
		if last["expect_instance"] != "inst-crashed-and-respawned" || w.runs["other"].commit != relCommitC {
			t.Fatalf("replace %v, run %+v", last, w.runs["other"])
		}
	})
	t.Run("c: no instance, desired running", func(t *testing.T) {
		w := forward(t, "pull", "replace")
		w.runs["other"].instance, w.runs["other"].registered = "", false
		w.must("rollback")
		if w.replaceCalls != 1 || w.startCalls != 1 || w.runs["other"].commit != relCommitC {
			t.Fatalf("%d replaces, %d starts, run %+v", w.replaceCalls, w.startCalls, w.runs["other"])
		}
	})
	t.Run("c: rerun after the start", func(t *testing.T) {
		w := forward(t, "pull", "replace")
		w.runs["other"].instance, w.runs["other"].registered = "", false
		if err := w.stepStop("rollback", "start-"+relCommitC[:12]+":after"); err != errTestStop {
			t.Fatalf("stop: %v", err)
		}
		w.must("rollback")
		if w.runs["other"].commit != relCommitC || w.startCalls != 2 {
			t.Fatalf("run %+v, %d starts (the start, then the expect_run check)", w.runs["other"], w.startCalls)
		}
	})
	t.Run("b: rerun after a lost reply", func(t *testing.T) {
		w := forward(t, "pull", "replace")
		w.loseReply = true
		w.stepErr("rollback", "disconnected")
		w.must("rollback")
		if w.replaceCalls != 2 || w.runs["other"].commit != relCommitC {
			t.Fatalf("%d replaces, run %+v", w.replaceCalls, w.runs["other"])
		}
	})
	for _, c := range []struct {
		name string
		set  func(r *fakeRun)
		want string
	}{
		{"d: pending intent", func(r *fakeRun) { r.pending = true }, "unresolved spawn intent"},
		{"e: another commit", func(r *fakeRun) { r.commit = pinCommit }, "neither FROM nor TO"},
		{"external stop", func(r *fakeRun) { r.instance, r.desired = "", "stopped" }, "an external stop"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := forward(t, "pull", "replace")
			c.set(w.runs["other"])
			calls := w.replaceCalls + w.startCalls
			w.stepErr("rollback", c.want)
			if w.replaceCalls+w.startCalls != calls {
				t.Fatalf("a hub call after the refusal")
			}
		})
	}
}

// The start barrier (plan c4 v3 section 1, A1, A4): only the SDK 0 refusal of the same instance
// passes; a timeout is sent again, at most 4 times.
func TestReleasePluginStartBarrier(t *testing.T) {
	for _, c := range []struct {
		name    string
		answers []string
		respawn bool
		want    string // "" passes
		calls   int
	}{
		{"refused", nil, false, "", 1},
		{"timeout then refused", []string{"timeout", "refused"}, false, "", 2},
		{"four timeouts", []string{"timeout", "timeout", "timeout", "timeout"}, false, "did not end in 4 tries", 4},
		{"gone", []string{"gone"}, false, "did not finish its start", 1},
		{"another refusal", []string{"other"}, false, "not the SDK 0 refusal", 1},
		{"an answer", []string{"ok"}, false, "answered view.seen", 1},
		{"the instance changes", nil, true, "ended during its start", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newPluginWorld(t, "0")
			w.freeze()
			w.perform = c.answers
			if c.respawn {
				w.performHook = func(int) { w.spawn("other", otherSrc) }
			}
			r := w.open("verify")
			defer r.unlock()
			cand, _ := r.readCandidate()
			err := r.startEnded(cand, w.client, "0")
			if (c.want == "" && err != nil) || (c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want))) || w.performN != c.calls {
				t.Fatalf("%v (want %q), %d calls (want %d)", err, c.want, w.performN, c.calls)
			}
		})
	}
	// A failed start fails precheck.
	w := newPluginWorld(t, "0")
	w.freeze()
	w.perform = []string{"gone"}
	w.stepErr("precheck", "did not finish its start")
}

// Review R14 (codex-26's TestReviewPluginFailedPrecheckCannotPull): a failed precheck leaves no
// checkpoint, so pull refuses after backup and the pin stays at FROM. A failed rerun after a passed
// precheck removes the earlier checkpoint too.
func TestReleasePluginFailedPrecheckCannotPull(t *testing.T) {
	w := newPluginWorld(t, "0")
	w.freeze()
	w.perform = []string{"gone"}
	w.stepErr("precheck", "did not finish its start")
	w.must("backup")
	w.stepErr("pull", "no hub of precheck")
	if got := w.lockPin(otherSrc); got != pinFrom {
		t.Fatalf("the pin moved after a failed precheck: %v", got)
	}

	w = newPluginWorld(t, "0")
	w.freeze()
	w.must("precheck")
	w.perform = []string{"gone"}
	w.stepErr("precheck", "did not finish its start")
	w.must("backup")
	w.stepErr("pull", "no hub of precheck")
}

// Review R15 (codex-26's TestReviewPluginFailedArgvReadCannotFreeze): a failed or empty argv read
// of the hub refuses freeze; it is not "no -sdk". A read argv without -sdk is that state (a plugin
// without the SDK passes).
func TestReleasePluginArgvReadFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		read func(int) ([]string, error)
		want string
	}{
		{"error", func(int) ([]string, error) { return nil, errors.New("sysctl failed") }, "sysctl failed"},
		{"empty", func(int) ([]string, error) { return nil, nil }, "the argv is empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newPluginWorld(t, "")
			if err := createRun(w.run, w.specP); err != nil {
				t.Fatal(err)
			}
			r := w.open("freeze")
			defer r.unlock()
			r.argvOf = c.read
			if err := relPluginFreeze(r); err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "-sdk directory is unknown") {
				t.Fatalf("freeze: %v", err)
			}
		})
	}
	w := newPluginWorld(t, "")
	w.sdkDir = "" // the argv is read, and it has no -sdk
	w.freeze()
	if c := mustCandidate(t, w); c.SDKDir != "" || c.SDKDigest != "" {
		t.Fatalf("candidate %+v", c)
	}
}

// Review R16 (codex-26's TestReviewPluginVerifyChecksPins): verify checks the pins. The lock of P
// back at FROM while P runs TO, or another pin moved, fails verify.
func TestReleasePluginVerifyChecksPins(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(w *relWorld)
		want string
	}{
		{"P lock at FROM", func(w *relWorld) {
			l, _ := readLock(lockPath(w.cfg, ""))
			l.Plugins[otherSrc].Commit, l.Plugins[otherSrc].Digest = pinFrom.Commit, pinFrom.Digest
			writeLock(lockPath(w.cfg, ""), l)
		}, `not "to to"`},
		{"another pin", func(w *relWorld) {
			l, _ := readLock(lockPath(w.cfg, ""))
			l.Plugins[homeSrc].Commit = commitH
			writeLock(lockPath(w.cfg, ""), l)
		}, "a pin other than"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newPluginWorld(t, "0")
			w.freeze()
			w.must("precheck", "backup", "pull", "replace")
			c.set(w)
			w.stepErr("verify", c.want)
		})
	}
}
