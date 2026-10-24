package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// peerFetchRepo commits a second tree to r and tags it v1.1.0: many files (several tree pages at
// a 16 KiB reply limit), an executable file, an empty file and the 1 MiB file of fetchTestRepo
// (several byte parts).
func peerFetchRepo(t *testing.T, r *testRepo) string {
	t.Helper()
	files := map[string]string{"lazyrex.json": manifestJSON("notes"), "main.lua": "return 2\n", "bin/run*": "#!/bin/sh\n", "empty.txt": ""}
	for i := range 300 {
		files[fmt.Sprintf("lib/a-long-directory-name/file-%03d.lua", i)] = fmt.Sprintf("return %d\n", i)
	}
	c := r.commit(files)
	r.git("tag", "v1.1.0")
	return c
}

// peerFetchHubs is a source hub a (notes at v1.1.0, a 16 KiB reply limit) and a receiver b with
// the managed entry notes at v1.0.0 with fetch peer, linked (b's plugins_from peer is a). b's
// remote does not exist: b has no git source.
func peerFetchHubs(t *testing.T) (a, b *Hub, r *testRepo, c2 string) {
	t.Helper()
	b, r, _, _ = atTestHub(t, "github:acme/notes@v1.0.0")
	endFixtureOps(t, b)
	c2 = peerFetchRepo(t, r)
	a = peerSourceHub(t, r, "github:acme/notes@v1.1.0")
	a.cfg.TestReplyLimit = testReplyLimitMin
	peerSyncLink(t, a, b)
	b.pops.cmd = pluginCmdOpts{remote: func(owner, repo string) string { return "file:///nonexistent/no-git-source" }}
	writeManaged(t, b.cfg.State, `{"plugins":[{"id":"notes","source":"github:acme/notes@v1.0.0","fetch":"peer"}]}`)
	opsLoadEntries(t, b)
	return a, b, r, c2
}

// A sync of a peer entry fetches the peer's pin from the peer, in tree pages and byte parts that
// the source sizes by its reply limit, with no git. The receiver locks the peer's pin, and its
// cache entry verifies with the published modes. The op's result names the counts.
func TestPeerFetchSync(t *testing.T) {
	a, b, _, c2 := peerFetchHubs(t)
	ctx := context.Background()
	s, err := b.peerPluginsSync(ctx, b.peerLink("mac"))
	if err != nil || len(s.Updating) != 1 {
		t.Fatalf("the sync: %+v %v", s, err)
	}
	op := opsOf(t, b, s.Updating[0]["op_id"].(string))
	op.Phase = "resolving"
	for _, phase := range []func(context.Context, *pluginOp) error{b.opResolve, b.opFetch} {
		if err := phase(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	fetched := op.Result
	if err := b.opLock(ctx, op); err != nil {
		t.Fatal(err)
	}
	want := readTestLock(t, a).Plugins["github:acme/notes@v1.1.0"]
	le := readTestLock(t, b).Plugins["github:acme/notes@v1.1.0"]
	if op.Phase != "writing" || le == nil || le.Commit != c2 || le.lockPin != want.lockPin {
		t.Fatalf("after the lock: %+v, the lock entry %+v, the peer's %+v", op, le, want)
	}
	m := regexp.MustCompile(`from peer mac: (\d+) tree calls, (\d+) byte calls, (\d+) bytes in `).FindStringSubmatch(fetched)
	if m == nil {
		t.Fatalf("the fetch result %q", fetched)
	}
	var tree, parts int
	fmt.Sscan(m[1], &tree)
	fmt.Sscan(m[2], &parts)
	if tree < 2 || parts < 10 {
		t.Errorf("%d tree calls and %d byte calls at a 16 KiB limit; want several of each", tree, parts)
	}
	lock := readTestLock(t, b)
	e := &configEntry{Source: "github:acme/notes@v1.1.0"}
	e.checkSource("")
	if _, _, err := lockedDir(b.cfg.State, lock, e); err != nil {
		t.Fatalf("the cache entry: %v", err)
	}
	root := filepath.Join(b.cfg.State, "plugins", filepath.FromSlash(le.Dir))
	for p, mode := range map[string]os.FileMode{"bin/run": 0o555, "empty.txt": 0o444, "main.lua": 0o444} {
		fi, err := os.Stat(filepath.Join(root, p))
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v, want %v", p, fi.Mode(), err, mode)
		}
	}
}

// Each serve call names a configured peer and a pin that the source's lock has with a cache entry
// that verifies; else unknown_hub, an invalid input (refused) or not_cached.
func TestPeerFetchServeRefusals(t *testing.T) {
	a, _, _, _ := peerFetchHubs(t)
	le := readTestLock(t, a).Plugins["github:acme/notes@v1.1.0"]
	call := func(over map[string]any) error {
		args := map[string]any{"hub": "linux", "source": "github:acme/notes@v1.1.0", "commit": le.Commit, "digest": le.Digest, "from": 0}
		for k, v := range over {
			args[k] = v
		}
		_, err := a.peerPluginTreeServe(jsonArgs(args))
		return err
	}
	if err := call(nil); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		over map[string]any
		code string
	}{
		"an unknown hub":       {map[string]any{"hub": "nobody"}, "unknown_hub"},
		"a bad commit":         {map[string]any{"commit": "x"}, "refused"},
		"another commit":       {map[string]any{"commit": strings.Repeat("c", 40)}, "not_cached"},
		"another digest":       {map[string]any{"digest": "sha256:" + strings.Repeat("0", 64)}, "not_cached"},
		"a source not locked":  {map[string]any{"source": "github:acme/notes@v9"}, "not_cached"},
		"a from past the list": {map[string]any{"from": 100000}, "refused"},
	}
	for name, c := range cases {
		if err := call(c.over); errCode(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	// A cache entry that changed is not served.
	root := filepath.Join(a.cfg.State, "plugins", filepath.FromSlash(le.Dir))
	os.Chmod(root, 0o755)
	os.Chmod(filepath.Join(root, "main.lua"), 0o644)
	if err := os.WriteFile(filepath.Join(root, "main.lua"), []byte("return 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := call(nil); errCode(err) != "not_cached" {
		t.Errorf("a changed cache entry: %v", err)
	}
	if _, err := a.peerPluginBytesServe(jsonArgs(map[string]any{"hub": "linux", "source": "github:acme/notes@v1.1.0", "commit": le.Commit, "digest": le.Digest, "offset": 0})); errCode(err) != "not_cached" {
		t.Errorf("bytes of a changed cache entry: %v", err)
	}
}

// A peer fetch that the source refuses fails the op; the lock and the managed file stay, and no
// stage stays.
func TestPeerFetchNotCachedFails(t *testing.T) {
	a, b, _, _ := peerFetchHubs(t)
	ctx := context.Background()
	s, err := b.peerPluginsSync(ctx, b.peerLink("mac"))
	if err != nil || len(s.Updating) != 1 {
		t.Fatalf("the sync: %+v %v", s, err)
	}
	os.Remove(opLockFile(a)) // the source lost its pin
	lockBytes, _ := os.ReadFile(opLockFile(b))
	op := opsOf(t, b, s.Updating[0]["op_id"].(string))
	op.Phase = "resolving"
	for _, phase := range []func(context.Context, *pluginOp) error{b.opResolve, b.opFetch} {
		if err := phase(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	if op.Phase != "failed" || !strings.Contains(op.Error, "not in this hub's verified cache") {
		t.Fatalf("the op: %+v", op)
	}
	if got, _ := os.ReadFile(opLockFile(b)); string(got) != string(lockBytes) {
		t.Error("a failed peer fetch wrote the lock")
	}
	stages, _ := filepath.Glob(filepath.Join(b.cfg.State, "plugins", "github", "acme", "notes", ".stage-*"))
	if len(stages) != 0 {
		t.Errorf("stages left: %v", stages)
	}
}

// fakeTree is a tree list of files with their content, for a fake source.
type fakeTree struct {
	files []digestFile
	data  []byte
}

func newFakeTree(contents map[string]string) fakeTree {
	var ft fakeTree
	for p, c := range contents {
		f := digestFile{Path: p, Size: int64(len(c))}
		f.Sum = [32]byte(sha256Bytes([]byte(c)))
		ft.files = append(ft.files, f)
	}
	sortTestFiles(ft.files)
	for _, f := range ft.files {
		ft.data = append(ft.data, contents[f.Path]...)
	}
	return ft
}

func sha256Bytes(b []byte) []byte {
	s, _ := hex.DecodeString(strings.TrimPrefix(hashBytes(b), "sha256:"))
	return s
}

func sortTestFiles(f []digestFile) {
	for i := range f {
		for j := i + 1; j < len(f); j++ {
			if f[j].Path < f[i].Path {
				f[i], f[j] = f[j], f[i]
			}
		}
	}
}

func treeLines(files []digestFile) []peerTreeFile {
	out := []peerTreeFile{}
	for _, f := range files {
		out = append(out, peerTreeFile{Path: f.Path, Exec: f.Exec, Size: f.Size, SHA256: hex.EncodeToString(f.Sum[:])})
	}
	return out
}

// The receiver checks each page against the pages before, and the whole list before it fetches a
// byte: the asked index, count and size, the line total, the paths, the order and the digest.
func TestPeerFetchTreeChecks(t *testing.T) {
	ft := newFakeTree(map[string]string{"a.lua": "a", "b/c.lua": "bc", "d.lua": "d"})
	digest := digestOf(ft.files)
	lines := treeLines(ft.files)
	two := 2
	cases := map[string]struct {
		pages func(from int) peerTreePage
		want  string
	}{
		"the right list": {func(from int) peerTreePage {
			if from == 0 {
				return peerTreePage{Files: lines[:2], From: 0, Next: &two, Count: 3, Size: 4}
			}
			return peerTreePage{Files: lines[2:], From: 2, Count: 3, Size: 4}
		}, ""},
		"another from": {func(from int) peerTreePage {
			return peerTreePage{Files: lines, From: 1, Count: 3, Size: 4}
		}, "answers from 1"},
		"a count that changes": {func(from int) peerTreePage {
			if from == 0 {
				return peerTreePage{Files: lines[:2], From: 0, Next: &two, Count: 3, Size: 4}
			}
			return peerTreePage{Files: lines[2:], From: 2, Count: 4, Size: 4}
		}, "not 3 and 4"},
		"fewer lines than count": {func(from int) peerTreePage {
			return peerTreePage{Files: lines[:2], From: 0, Count: 3, Size: 4}
		}, "2 lines, not count 3"},
		"another digest": {func(from int) peerTreePage {
			l := append([]peerTreeFile{}, lines...)
			l[0].Exec = true
			return peerTreePage{Files: l, From: 0, Count: 3, Size: 4}
		}, "not the pin's"},
		"too many files": {func(from int) peerTreePage {
			return peerTreePage{Files: lines, From: 0, Count: peerFetchFiles + 1, Size: 4}
		}, "at most 10000"},
		"too many bytes": {func(from int) peerTreePage {
			return peerTreePage{Files: lines, From: 0, Count: 3, Size: peerFetchBytes + 1}
		}, "at most 16777216"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			b := peerFakeSource(t, func(action string, args map[string]any) any {
				from, _ := intArg(args["from"])
				return c.pages(from)
			})
			_, _, err := b.peerFetchTree(context.Background(), b.pluginsFrom(), map[string]any{}, digest, &peerFetchStats{})
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("%v, want %q", err, c.want)
			}
		})
	}
}

// peerFakeSource is a receiver hub whose plugins_from peer "mac" answers each call with reply.
func peerFakeSource(t *testing.T, reply func(action string, args map[string]any) any) *Hub {
	t.Helper()
	b, _, _ := opsTestHub(t, "")
	b.cfg.HubName, b.cfg.Peers = "linux", []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	b.peers = newPeerSet(b.cfg)
	b.peers.call = func(_ context.Context, _ PeerConfig, action string, args any) ([]byte, error) {
		return json.Marshal(reply(action, jsonArgs(args)))
	}
	return b
}

// jsonArgs is args as an action gets them: decoded from JSON (numbers are float64).
func jsonArgs(args any) map[string]any {
	var m map[string]any
	json.Unmarshal(rawJSON(args), &m)
	return m
}

func TestCheckTreePaths(t *testing.T) {
	ok := []string{"a", "a.b/c", "b/..c", "z"}
	bad := map[string][]string{
		"absolute":       {"/etc/passwd"},
		"dot dot":        {"a/../b"},
		"dot":            {"./a"},
		"empty part":     {"a//b"},
		"trailing slash": {"a/"},
		"duplicate":      {"a", "a"},
		"out of order":   {"b", "a"},
		"under a file":   {"a", "a/b"},
		"nul":            {"a\x00b"},
	}
	files := func(paths []string) []digestFile {
		var out []digestFile
		for _, p := range paths {
			out = append(out, digestFile{Path: p})
		}
		return out
	}
	if err := checkTreePaths(files(ok)); err != nil {
		t.Errorf("valid paths: %v", err)
	}
	for name, p := range bad {
		if err := checkTreePaths(files(p)); err == nil {
			t.Errorf("%s %q: accepted", name, p)
		}
	}
}

// The receiver checks each part (its offset, end and length) and each file's sha256, and writes
// the stage files 0600, or 0700 for an executable line.
func TestPeerFetchBytesChecks(t *testing.T) {
	ft := newFakeTree(map[string]string{"a.lua": "hello", "b.sh": "#!/bin/sh\n", "c.txt": ""})
	ft.files[1].Exec = true
	part := func(off, n int64) peerBytesPart {
		return peerBytesPart{Data: base64.StdEncoding.EncodeToString(ft.data[off : off+n]), Offset: off, End: off + n}
	}
	size := int64(len(ft.data))
	cases := map[string]struct {
		parts func(off int64) peerBytesPart
		want  string
	}{
		"parts of 3 bytes": {func(off int64) peerBytesPart { return part(off, min(3, size-off)) }, ""},
		"another offset": {func(off int64) peerBytesPart {
			p := part(off, 1)
			p.Offset++
			return p
		}, "the part is offset"},
		"a short part": {func(off int64) peerBytesPart {
			p := part(off, 2)
			p.End++
			return p
		}, "the part is offset"},
		"other bytes": {func(off int64) peerBytesPart {
			p := part(off, size-off)
			b := append([]byte{}, ft.data[off:]...)
			b[0] ^= 1
			p.Data = base64.StdEncoding.EncodeToString(b)
			return p
		}, "does not have the sha256"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			b := peerFakeSource(t, func(action string, args map[string]any) any {
				off, _ := intArg(args["offset"])
				return c.parts(int64(off))
			})
			dir := t.TempDir()
			err := b.peerFetchBytes(context.Background(), b.pluginsFrom(), map[string]any{}, dir, ft.files, size, &peerFetchStats{})
			if c.want != "" {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("%v, want %q", err, c.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for p, mode := range map[string]os.FileMode{"a.lua": 0o600, "b.sh": 0o700, "c.txt": 0o600} {
				fi, err := os.Stat(filepath.Join(dir, p))
				if err != nil || fi.Mode().Perm()&^0o077 != mode {
					t.Errorf("%s: %v %v, want %v", p, fi.Mode(), err, mode)
				}
			}
			if d, _ := dirDigest(dir); d != digestOf(ft.files) {
				t.Errorf("the written digest %s, want %s", d, digestOf(ft.files))
			}
		})
	}
}

// The managed file's fetch: absent and "github" are the same entry; "peer" is written; another
// value, or peer on a local source, is refused by the strict loader.
func TestManagedFetchField(t *testing.T) {
	read := func(body string) ([]*configEntry, error) { return parseManagedBytes([]byte(body)) }
	absent, err := read(`{"plugins":[{"id":"n","source":"github:acme/notes@v1"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	github, err := read(`{"plugins":[{"id":"n","source":"github:acme/notes@v1","fetch":"github"}]}`)
	if err != nil || canonicalEntry(github[0]) != canonicalEntry(absent[0]) || strings.Contains(canonicalEntry(absent[0]), "fetch") {
		t.Fatalf("github: %v %s %s", err, canonicalEntry(github[0]), canonicalEntry(absent[0]))
	}
	peer, err := read(`{"plugins":[{"id":"n","source":"github:acme/notes@v1","fetch":"peer"}]}`)
	if err != nil || peer[0].fetch != "peer" || !strings.Contains(canonicalEntry(peer[0]), `"fetch":"peer"`) {
		t.Fatalf("peer: %v %+v", err, peer)
	}
	b, err := encodeManaged([]managedFileEntry{fileEntry(absent[0]), fileEntry(&configEntry{managed: "m", Source: "github:acme/m@v1", fetch: "peer"})})
	if err != nil || strings.Count(string(b), `"fetch"`) != 1 {
		t.Fatalf("the encoded file: %v\n%s", err, b)
	}
	for _, body := range []string{
		`{"plugins":[{"id":"n","source":"github:acme/notes@v1","fetch":"git"}]}`,
		`{"plugins":[{"id":"n","source":"/tmp/notes","fetch":"peer"}]}`,
	} {
		if _, err := read(body); err == nil {
			t.Errorf("accepted: %s", body)
		}
	}
}

// settings sets fetch: peer needs a plugins_from peer; a missing fetch keeps the value; an expect
// with another fetch is a conflict; the intended entry has the new fetch. An update of a peer entry
// with no target is refused.
func TestSettingsFetch(t *testing.T) {
	h, _, _ := opsTestHub(t, `{"plugins":[{"id":"tools","source":"github:acme/tools@v1.0.0"}]}`)
	set := func(key string, input, expect any) (*pluginOp, error) {
		req := opRequest{Op: "settings", Entry: "managed:tools", Input: rawJSON(input)}
		if expect != nil {
			req.Expect = rawJSON(expect)
		}
		v, err := userAdmit(h, key, req)
		if err != nil {
			return nil, err
		}
		return opsOf(t, h, v["op_id"].(string)), nil
	}
	if _, err := set("s1", map[string]any{"fetch": "peer"}, nil); err == nil || !strings.Contains(err.Error(), "needs a plugins_from peer") {
		t.Fatalf("no plugins_from peer: %v", err)
	}
	h.cfg.HubName, h.cfg.Peers = "linux", []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	h.peers = newPeerSet(h.cfg)
	if _, err := set("s2", map[string]any{"fetch": "git"}, nil); err == nil || !strings.Contains(err.Error(), "github or peer") {
		t.Fatalf("a bad value: %v", err)
	}
	if _, err := set("s3", map[string]any{"fetch": "peer"}, map[string]any{"fetch": "peer"}); !isRefusal(err, "conflict") {
		t.Fatalf("an expect with another fetch: %v", err)
	}
	op, err := set("s4", map[string]any{"fetch": "peer"}, map[string]any{"fetch": "github"})
	if err != nil {
		t.Fatal(err)
	}
	if intended, err := op.intended(); err != nil || !strings.Contains(intended, `"fetch":"peer"`) {
		t.Fatalf("the intended entry %s %v", intended, err)
	}
	endFixtureOps(t, h)
	writeManaged(t, h.cfg.State, `{"plugins":[{"id":"tools","source":"github:acme/tools@v1.0.0","args":{"k":"v"},"fetch":"peer"}]}`)
	opsLoadEntries(t, h)
	op, err = set("s5", map[string]any{"args": map[string]string{"k": "w"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if intended, _ := op.intended(); !strings.Contains(intended, `"fetch":"peer"`) || !strings.Contains(op.Input, `"fetch":"peer"`) {
		t.Fatalf("a missing fetch: input %s, intended %s", op.Input, intended)
	}
	endFixtureOps(t, h)
	if _, err := userAdmit(h, "u1", opRequest{Op: "update", Entry: "managed:tools", Input: rawJSON(map[string]any{"to": "latest"})}); err == nil || !strings.Contains(err.Error(), "fetches from the peer") {
		t.Fatalf("an update of a peer entry: %v", err)
	}
}

// An install of a peer entry with no lock entry takes the peer's pin of the exact source, with no
// git; a peer with no pin of the source fails the resolve.
func TestPeerFetchInstallTarget(t *testing.T) {
	a, b, _, c2 := peerFetchHubs(t)
	_ = a
	ctx := context.Background()
	t1, err := b.peerPinTarget(ctx, "github:acme/notes@v1.1.0")
	if err != nil || t1.Commit != c2 || !t1.ByID || t1.Digest == "" {
		t.Fatalf("the target %+v %v", t1, err)
	}
	if _, err := b.peerPinTarget(ctx, "github:acme/notes@v1.0.0"); err == nil || !strings.Contains(err.Error(), "has no pin for github:acme/notes@v1.0.0") {
		t.Fatalf("no pin: %v", err)
	}
}

// A reload takes a change of fetch alone, in both directions (codex-21, c21-plugsrc-fetch-reload):
// the entry has the file's fetch, and hub.plugins shows it.
func TestReloadFetch(t *testing.T) {
	h, _, _ := opsTestHub(t, `{"plugins":[{"id":"tools","source":"github:acme/tools@v1.0.0"}]}`)
	h.cfg.HubName, h.cfg.Peers = "linux", []PeerConfig{{Name: "mac", Server: "lab-mac", PluginsFrom: true}}
	h.peers = newPeerSet(h.cfg)
	for _, want := range []string{"peer", "github", "peer"} {
		fetch := ""
		if want == "peer" {
			fetch = `,"fetch":"peer"`
		}
		writeManaged(t, h.cfg.State, `{"plugins":[{"id":"tools","source":"github:acme/tools@v1.0.0"`+fetch+`}]}`)
		if _, err := h.reload(context.Background(), nil, ""); err != nil {
			t.Fatal(err)
		}
		h.entriesMu.Lock()
		got := fetchName(h.entries[0].fetch)
		h.entriesMu.Unlock()
		if got != want {
			t.Fatalf("after a reload to %s: the entry has fetch %s", want, got)
		}
		v, err := h.pluginsList(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var list struct {
			Entries     []map[string]any `json:"entries"`
			PluginsFrom *string          `json:"plugins_from"`
		}
		json.Unmarshal(rawJSON(v), &list)
		if list.PluginsFrom == nil || *list.PluginsFrom != "mac" {
			t.Fatalf("hub.plugins plugins_from %v, want mac", list.PluginsFrom)
		}
		if f := list.Entries[0]["fetch"]; f != want {
			t.Fatalf("after a reload to %s: hub.plugins shows fetch %v", want, f)
		}
	}
}
