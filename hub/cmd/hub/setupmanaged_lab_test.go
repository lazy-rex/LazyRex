package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The managed file of a new setup (setupManaged) on a fresh hub: no lock entry and no cache. ONE
// hub start installs its entries through the install op, each at its release tag, and runs them;
// no plugin command runs. The lab's git reaches github.com/lazy-rex/LazyRex and
// github.com/lazy-rex/Messaging through url.insteadOf: no network. The other setup plugins have no
// release tag here, so setup leaves them out. Lab only: LAZYREX_LAB=1.
func TestSetupManagedInstallLab(t *testing.T) {
	needLab(t)
	gitTestEnv(t)
	home := newTestRepo(t)
	home.commit(map[string]string{"plugins/lazyrex/lazyrex.json": manifestJSON("lazyrex"), "plugins/lazyrex/main.lua": fmt.Sprintf(startFixture, "lazyrex")})
	home.git("tag", "v1.0.0")
	msg, _ := labPluginRepo(t, "messaging")
	msg.git("tag", "v1.0.0")
	s := startLabWith(t, nil, false, map[string]string{"GIT_CONFIG_GLOBAL": "${LAB}/gitconfig", "GIT_CONFIG_NOSYSTEM": "1"})
	gitcfg := fmt.Sprintf("[url %q]\n\tinsteadOf = https://github.com/lazy-rex/LazyRex\n[url %q]\n\tinsteadOf = https://github.com/lazy-rex/Messaging\n", home.url(), msg.url())
	if err := os.WriteFile(filepath.Join(s.l.dir, "gitconfig"), []byte(gitcfg), 0o600); err != nil {
		t.Fatal(err)
	}
	e := setupEnv{tags: func(repo string) ([]string, error) {
		if repo == "LazyRex" || repo == "Messaging" {
			return []string{"v1.0.0"}, nil
		}
		return nil, nil
	}}
	b, skipped, err := setupManaged(e)
	if err != nil || len(skipped) != 4 {
		t.Fatalf("setupManaged: %v, skipped %v", err, skipped)
	}
	s.writeManagedFile(string(b))
	s.start()
	s.registered("lazyrex")
	s.registered("messaging")
	alice := s.opsAgent("alice")
	ops := installOps(s, alice)
	if len(ops) != 2 {
		t.Fatalf("the install ops: %v", ops)
	}
	for _, op := range ops {
		if op["phase"] != "done" || fmt.Sprint(op["origin"]) != "map[id:start kind:hub]" {
			t.Errorf("an install op: %v", op)
		}
	}
	lock := readTestLockFile(t, s)
	for _, src := range []string{"github:lazy-rex/LazyRex/plugins/lazyrex@v1.0.0", "github:lazy-rex/Messaging@v1.0.0"} {
		if le := lock.Plugins[src]; le == nil || le.Commit == "" {
			t.Errorf("the lock entry of %s: %+v", src, le)
		}
	}
}
