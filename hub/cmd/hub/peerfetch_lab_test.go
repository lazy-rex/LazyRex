package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// peerFetchSource is the plugin of TestPeerFetchLab: a subdir source of this repository, so its
// cache entry is the whole commit tree (docs/plugin-sources.md section 4), the size of the home
// plugin's entry. tests/sdk-peer (speer) runs in a lab hub.
const peerFetchSource = "github:lazy-rex/LazyRex/tests/sdk-peer@main"

// peerFetchGit runs git with the test process's environment and returns its trimmed output.
func peerFetchGit(t *testing.T, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// peerFetchGitConfig gives the lab hub a git config. With mirror set, the GitHub URLs of lazy-rex
// go to that bare mirror; every other URL goes to a mirror that does not exist. Without it, every
// URL does: the hub has no git source at all.
func (s *startLab) peerFetchGitConfig(mirror string) {
	s.t.Helper()
	none := "file://" + filepath.Join(s.l.dir, "mirrors", "none") + "/"
	body := fmt.Sprintf("[url %q]\n\tinsteadOf = https://\n\tinsteadOf = git@\n\tinsteadOf = ssh://\n", none)
	if mirror != "" {
		body = fmt.Sprintf("[url %q]\n\tinsteadOf = https://github.com/lazy-rex/\n\tinsteadOf = git@github.com:lazy-rex/\n", "file://"+mirror+"/") + body
	}
	cfg := filepath.Join(s.l.dir, "gitconfig")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		s.t.Fatal(err)
	}
	s.l.env = append(s.l.env, "GIT_CONFIG_GLOBAL="+cfg, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file")
}

// editConfig changes the lab hub's config file with fn.
func (s *startLab) editConfig(fn func(c map[string]any)) {
	s.t.Helper()
	var c map[string]any
	b, _ := os.ReadFile(s.cfg)
	json.Unmarshal(b, &c)
	fn(c)
	b, _ = json.MarshalIndent(c, "", " ")
	if err := os.WriteFile(s.cfg, b, 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// hubLog is the lab hub's log so far: every hub log of the lab (hub.1.log, and one more for each
// start).
func (s *startLab) hubLog() string {
	logs, _ := filepath.Glob(filepath.Join(s.l.dir, "hub*.log"))
	var out strings.Builder
	for _, f := range logs {
		b, _ := os.ReadFile(f)
		out.Write(b)
	}
	return out.String()
}

// peerServed counts the serve lines of the source hub's log for commit (peerfetch.go): one line
// for each hub.peer.plugin_tree and each hub.peer.plugin_bytes call.
func (s *startLab) peerServed(commit string) (tree, bytes int) {
	for _, line := range strings.Split(s.hubLog(), "\n") {
		switch {
		case !strings.Contains(line, " "+commit+" "):
		case strings.Contains(line, ": plugin_tree "):
			tree++
		case strings.Contains(line, ": plugin_bytes "):
			bytes++
		}
	}
	return tree, bytes
}

// peerFetched is the receiver's log line of a peer fetch: "fetched <name> <commit> from peer <p>:
// <t> tree calls, <b> byte calls, <n> bytes in <d>s".
var peerFetched = regexp.MustCompile(`fetched \S+ ([0-9a-f]{40}) from peer ([a-z0-9-]+): (\d+) tree calls, (\d+) byte calls, (\d+) bytes in ([0-9.]+)s`)

// peerFetchLine waits for the receiver's fetch line of commit and returns its fields.
func (s *startLab) peerFetchLine(commit string, d time.Duration) []string {
	s.t.Helper()
	var m []string
	s.waitLong("the peer fetch of "+commit[:8], d, func() bool {
		for _, x := range peerFetched.FindAllStringSubmatch(s.hubLog(), -1) {
			if x[1] == commit {
				m = x
				return true
			}
		}
		return false
	})
	return m
}

// TestPeerFetchLab: a receiving hub takes a plugin from its plugins_from peer over the peer link
// (managed entry "fetch": "peer"; lead-plugsrc, lead-plugsrc3, lead-plugsrc5), and has no git
// source at all. The source hub's reply limit is 64 KiB (test_reply_limit), and the plugin's cache
// entry is a tree of the home plugin's size (the whole repository), so the file list and the bytes
// both come in several pages:
//   - b installs the plugin with no lock entry: the target is a's pin; b runs it.
//   - a moves to a new commit (an update "@"); b's plugin sync takes it, from a again.
//   - a's cache entry of a third commit no longer verifies: a refuses its pin document, b keeps
//     its commit; after a's restart repairs the entry, b takes the third commit.
//   - b's cache entry goes while a moves back: b's install of its locked commit fails not_cached
//     (a no longer has that pin), and b's next sync takes a's pin.
//
// The log has the call counts and the fetch time of each fetch (the 120 s phase bound applies).
func TestPeerFetchLab(t *testing.T) {
	needLab(t)
	_, root := harnessBinary(t)
	c1 := peerFetchGit(t, "-C", root, "rev-parse", "HEAD~2")
	c2 := peerFetchGit(t, "-C", root, "rev-parse", "HEAD~1")
	c3 := peerFetchGit(t, "-C", root, "rev-parse", "HEAD")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp := "http://" + ln.Addr().String()
	ln.Close()
	a := newServerLab(t, nil)
	b := newServerLab(t, []string{"--extra-listen", tcp, "--allow-anonymous"})
	for _, s := range []*startLab{a, b} {
		s.args = append(s.args, "-sdk", filepath.Join(root, "sdk", "lua"))
	}
	// a's mirror has main at c1; a push from this checkout only reads the checkout.
	mirror := filepath.Join(a.l.dir, "mirrors", "lazy-rex")
	bare := filepath.Join(mirror, "LazyRex.git")
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	peerFetchGit(t, "init", "-q", "--bare", bare)
	moveMain := func(c string) { peerFetchGit(t, "-C", root, "push", "-q", "--force", bare, c+":refs/heads/main") }
	moveMain(c1)
	if err := os.Symlink("LazyRex.git", filepath.Join(mirror, "LazyRex")); err != nil {
		t.Fatal(err)
	}
	a.peerFetchGitConfig(mirror)
	b.peerFetchGitConfig("")
	a.setPeer("a", "b", tcp)
	b.setPeer("b", "a", a.l.server)
	a.editConfig(func(c map[string]any) { c["test_reply_limit"] = 65536 })
	b.editConfig(func(c map[string]any) { c["peers"].([]any)[0].(map[string]any)["plugins_from"] = true })
	a.writeManagedFile(`{"plugins":[{"id":"speer","source":"` + peerFetchSource + `"}]}`)
	b.writeManagedFile(`{"plugins":[{"id":"speer","source":"` + peerFetchSource + `","fetch":"peer"}]}`)
	a.start()
	a.registered("speer")

	// 1. b installs from a, with no lock entry and no git.
	b.start()
	report := func(step, c string) {
		t.Helper()
		m := b.peerFetchLine(c, 3*time.Minute)
		tree, bytes := a.peerServed(c)
		t.Logf("%s: b fetched %s from peer %s: %s tree calls, %s byte calls, %s bytes in %ss; a served %d plugin_tree and %d plugin_bytes calls",
			step, c[:8], m[2], m[3], m[4], m[5], m[6], tree, bytes)
		if m[2] != "a" || m[3] == "1" || m[4] == "1" || fmt.Sprint(tree) != m[3] || fmt.Sprint(bytes) != m[4] {
			t.Fatalf("%s: a 64 KiB source pages both actions, and the counts of both sides agree: %v; a: %d, %d", step, m, tree, bytes)
		}
	}
	report("install", c1)
	b.waitLong("b runs speer", time.Minute, func() bool { return b.status("speer").Registered })

	// 2. An update: a's mirror main moves to c2, a refreshes; b's sync takes a's pin from a.
	moveMain(c2)
	x := a.opsAgent("x")
	if op := a.opDone(x, "up-c2", map[string]any{"op": "update", "entry": "managed:speer", "input": map[string]any{"to": "@"}}); op["phase"] != "done" {
		t.Fatalf("a's update to c2: %v", op)
	}
	report("sync update", c2)
	b.waitLong("b runs speer at c2", 2*time.Minute, func() bool {
		return b.status("speer").Registered && strings.Contains(fmt.Sprint(b.managedEntry("speer")), c2)
	})

	// 3. A source cache entry that no longer verifies: while b is down, a moves to c3, and one file
	// of a's c3 entry changes. a then refuses its whole pin document (hub.peer.plugins), so b's sync
	// fetches nothing and b keeps running c2. A restart of a installs c3 again; b then takes c3.
	b.stop()
	moveMain(c3)
	if op := a.opDone(x, "up-c3", map[string]any{"op": "update", "entry": "managed:speer", "input": map[string]any{"to": "@"}}); op["phase"] != "done" {
		t.Fatalf("a's update to c3: %v", op)
	}
	f := filepath.Join(cacheEntry(t, a, c3), "tests", "sdk-peer", "README.md")
	os.Chmod(filepath.Dir(f), 0o755)
	os.Chmod(f, 0o644)
	if err := os.WriteFile(f, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.start()
	b.waitLong("b's sync sees a's refusal", 3*time.Minute, func() bool {
		return logHas(b, "plugin sync", "the cache entry for commit "+c3+" changed")
	})
	b.waitLong("b runs speer", time.Minute, func() bool { return b.status("speer").Registered })
	if e := fmt.Sprint(b.managedEntry("speer")); !strings.Contains(e, c2) {
		t.Fatalf("b's speer while a refuses its pins: %s, want c2", e)
	}
	a.stop()
	a.start()
	a.registered("speer")
	report("after a's repair", c3)
	b.waitLong("b runs speer at c3", 2*time.Minute, func() bool {
		return b.status("speer").Registered && strings.Contains(fmt.Sprint(b.managedEntry("speer")), c3)
	})

	// 4. not_cached: while b is down, b's own cache entry of c3 goes, and a moves back to c1. b's
	// start installs its locked commit c3 from a; a has no c3 in its lock now, so the fetch is
	// refused not_cached and nothing is published. b's next sync takes a's pin c1, from a.
	b.stop()
	removeTree(cacheEntry(t, b, c3))
	moveMain(c1)
	if op := a.opDone(x, "up-c1", map[string]any{"op": "update", "entry": "managed:speer", "input": map[string]any{"to": "@"}}); op["phase"] != "done" {
		t.Fatalf("a's update back to c1: %v", op)
	}
	b.start()
	b.waitLong("b's install of c3 is refused not_cached", 3*time.Minute, func() bool { return logHas(b, "not_cached", c3) })
	b.waitLong("b runs speer at c1", 3*time.Minute, func() bool {
		return b.status("speer").Registered && strings.Contains(fmt.Sprint(b.managedEntry("speer")), c1)
	})
}

// cacheEntry is the one published cache entry of commit in the lab hub s.
func cacheEntry(t *testing.T, s *startLab, commit string) string {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(s.l.dir, "hub", "plugins", "github", "lazy-rex", "LazyRex", commit+".*"))
	if len(dirs) != 1 {
		t.Fatalf("the cache entries of %s: %v", commit, dirs)
	}
	return dirs[0]
}

// logHas reports whether one line of the lab hub's log has every part, and logs that line.
func logHas(s *startLab, parts ...string) bool {
	for _, line := range strings.Split(s.hubLog(), "\n") {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			s.t.Logf("%s", line)
			return true
		}
	}
	return false
}
