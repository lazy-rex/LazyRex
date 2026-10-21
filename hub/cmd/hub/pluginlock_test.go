package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const (
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
	testCommit2 = "89abcdef0123456789abcdef0123456789abcdef"
	testDigest  = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// goodLockEntry is a lock entry that agrees with github:lazy-rex/Messaging@main.
func goodLockEntry() *lockEntry {
	return &lockEntry{
		lockPin: lockPin{Name: "messaging", Owner: "lazy-rex", Repo: "Messaging", Ref: "main", RefKind: "branch",
			Commit: testCommit, Digest: testDigest},
		Dir: "github/lazy-rex/Messaging/" + testCommit + ".0123456789abcdef",
	}
}

func TestLockPath(t *testing.T) {
	cases := []struct{ cfg, field, want string }{
		{"/c/hub.json", "", "/c/hub.lock.json"},
		{"/c/hub", "", "/c/hub.lock.json"},
		{"/c/hub.json.bak", "", "/c/hub.json.bak.lock.json"},
		{"cfg/hub.json", "", "cfg/hub.lock.json"},
		{"/c/hub.json", "locks/my.lock", "/c/locks/my.lock"},
		{"/c/hub.json", "../x.json", "/x.json"},
		{"/c/hub.json", "/abs//l.json", "/abs/l.json"},
	}
	for _, c := range cases {
		if got := lockPath(c.cfg, c.field); got != c.want {
			t.Errorf("lockPath(%q, %q) = %q, want %q", c.cfg, c.field, got, c.want)
		}
	}
}

// A missing lock file is an empty lock (section 11: each GitHub entry is then "not installed").
func TestReadLockMissing(t *testing.T) {
	l, err := readLock(filepath.Join(t.TempDir(), "none", "hub.lock.json"))
	if err != nil || l.Version != 1 || len(l.Plugins) != 0 {
		t.Fatalf("got %+v, %v", l, err)
	}
}

// A lock file that does not decode is not valid: it gives no pins, and its text does not name
// sync. The structure is checked; null and wrong types never become an empty or missing entry.
func TestReadLockInvalid(t *testing.T) {
	entry := `{"name":"m","owner":"o","repo":"r","ref":"main","ref_kind":"branch","commit":"` + testCommit + `","digest":"d","dir":"x"}`
	bad := map[string]string{
		`{"version":1,"plugins":{}`:                                "unexpected EOF",
		`{"version":1,"plugins":{}} {}`:                            "data after the object",
		`{"version":1,"plugins":{},"extra":1}`:                     `unknown field "extra"`,
		`{"plugins":{}}`:                                           "version is missing",
		`{"version":2,"plugins":{}}`:                               "version 2 is not supported",
		`{"version":"1","plugins":{}}`:                             "cannot unmarshal string",
		`{"version":1}`:                                            "plugins must be an object",
		`{"version":1,"plugins":null}`:                             "plugins must be an object",
		`{"version":1,"plugins":[]}`:                               "plugins must be an object",
		`{"version":1,"plugins":{"github:o/r@main":null}}`:         "the entry for github:o/r@main must be an object",
		`{"version":1,"plugins":{"github:o/r@main":"x"}}`:          "the entry for github:o/r@main must be an object",
		`{"version":1,"plugins":{"github:o/r@main":{"commit":7}}}`: "cannot unmarshal number",
		`{"version":1,"plugins":{"github:o/r@main":` + strings.Replace(entry, `"dir":"x"`, `"dir":"x","more":1`, 1) + `}}`: `unknown field "more"`,
		`[]`: "cannot unmarshal array",
		``:   "EOF",
	}
	dir := t.TempDir()
	for body, want := range bad {
		p := filepath.Join(dir, "hub.lock.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		l, err := readLock(p)
		var inv *lockInvalidError
		if !errors.As(err, &inv) || l != nil {
			t.Errorf("%s: got %v, %v; want a lockInvalidError", body, l, err)
			continue
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "the lock file "+p+" is not valid: ") || !strings.Contains(msg, want) ||
			!strings.HasSuffix(msg, "; repair the file or restore it from a backup") || strings.Contains(msg, "sync") {
			t.Errorf("%s: text %q, want %q", body, msg, want)
		}
	}
}

// A lock file that exists but cannot be read is a read error, not an invalid lock.
func TestReadLockUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	p := filepath.Join(t.TempDir(), "hub.lock.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"plugins":{}}`), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := readLock(p)
	var re *lockReadError
	if !errors.As(err, &re) || err.Error() != "cannot read the lock file "+p+": permission denied" {
		t.Fatalf("got %v", err)
	}
}

// An empty, missing or null string field decodes to "": it is an agreement failure of the entry, so
// sync can repair it (section 3, "Repair"). A missing commit is untrusted: the text names update.
func TestLockDerivedFieldsMissing(t *testing.T) {
	src := "github:lazy-rex/Messaging@main"
	r, _ := parseGithub(src)
	for _, field := range []string{"name", "owner", "repo", "ref", "ref_kind", "commit", "digest", "dir"} {
		for _, mode := range []string{"empty", "missing", "null"} {
			fields := map[string]any{}
			raw, _ := json.Marshal(goodLockEntry())
			json.Unmarshal(raw, &fields)
			switch mode {
			case "empty":
				fields[field] = ""
			case "missing":
				delete(fields, field)
			default:
				fields[field] = nil // a JSON null in a string field is a missing value
			}
			body, _ := json.Marshal(map[string]any{"version": 1, "plugins": map[string]any{src: fields}})
			l, err := decodeLock(body)
			if err != nil {
				t.Fatalf("%s %s: %v\n%s", field, mode, err, body)
			}
			got := l.Plugins[src]
			if got.agree(r) != field {
				t.Errorf("%s %s: agree = %q", field, mode, got.agree(r))
			}
			err = lockMismatch("hub.json", src, got, r, got.agree(r))
			want := "the lock entry for " + src + " does not match: " + field + "; run lazyrex-hub plugin sync -config hub.json"
			if field == "commit" {
				want = "the locked commit for " + src + " cannot be trusted: commit is missing; run lazyrex-hub plugin update -config hub.json " + src
			}
			if err.Error() != want {
				t.Errorf("%s %s: %q, want %q", field, mode, err, want)
			}
		}
	}
}

// The agreement check names each field (section 3), and the trusted-commit rule picks the text.
func TestLockAgreement(t *testing.T) {
	cases := []struct {
		source string
		edit   func(*lockEntry)
		field  string
		why    string // the untrusted reason, "" when trusted
	}{
		{"github:lazy-rex/Messaging@main", func(*lockEntry) {}, "", ""},
		{"github:other/Messaging@main", func(*lockEntry) {}, "owner", ""},
		{"github:lazy-rex/Profiles@main", func(*lockEntry) {}, "repo", ""},
		{"github:lazy-rex/Messaging@v2", func(*lockEntry) {}, "ref", ""},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.RefKind = "commit" }, "ref_kind", ""},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.RefKind = "tag" }, "", ""},
		{"github:lazy-rex/Messaging@refs/heads/main", func(e *lockEntry) { e.Ref, e.RefKind = "refs/heads/main", "tag" }, "ref_kind", ""},
		{"github:lazy-rex/Messaging@refs/tags/main", func(e *lockEntry) { e.Ref, e.RefKind = "refs/tags/main", "tag" }, "", ""},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.Commit = strings.ToUpper(testCommit) }, "commit", "commit is not 40 lowercase hex"},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.Commit = testCommit[:39] }, "commit", "commit is not 40 lowercase hex"},
		{"github:lazy-rex/Messaging@" + testCommit2, func(e *lockEntry) { e.Ref, e.RefKind = testCommit2, "commit" }, "commit", "commit is not the commit of the ref"},
		{"github:lazy-rex/Messaging@" + testCommit, func(e *lockEntry) { e.Ref, e.RefKind = testCommit, "commit" }, "", ""},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.Dir = "github/lazy-rex/Messaging/" + testCommit2 + ".0123456789abcdef" }, "dir", ""},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.Dir = "github/lazy-rex/Messaging/" + testCommit + ".0123" }, "dir", ""},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.Dir = "github/lazy-rex/Messaging/.stage-0123456789abcdef" }, "dir", ""},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.Dir = "/abs/" + e.Dir }, "dir", ""},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.Dir += "/.." }, "dir", ""},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.Name = "Bad Name" }, "name", ""},
		{"github:lazy-rex/Messaging@main", func(e *lockEntry) { e.Digest = "sha256:abc" }, "digest", ""},
	}
	for _, c := range cases {
		r, ok := parseGithub(c.source)
		if !ok {
			t.Fatalf("parseGithub(%q)", c.source)
		}
		e := goodLockEntry()
		c.edit(e)
		if got := e.agree(r); got != c.field {
			t.Errorf("%s %+v: agree = %q, want %q", c.source, e, got, c.field)
		}
		if got := e.trust(r); got != c.why {
			t.Errorf("%s %+v: trust = %q, want %q", c.source, e, got, c.why)
		}
		if c.field == "" {
			continue
		}
		err := lockMismatch("cfg/hub.json", c.source, e, r, c.field)
		want := "the lock entry for " + c.source + " does not match: " + c.field + "; run lazyrex-hub plugin sync -config cfg/hub.json"
		if c.why != "" {
			want = "the locked commit for " + c.source + " cannot be trusted: " + c.why + "; run lazyrex-hub plugin update -config cfg/hub.json " + c.source
		}
		if err.Error() != want {
			t.Errorf("%s: %q, want %q", c.source, err, want)
		}
	}
}

// Equal lock values give equal bytes, in a fixed field order, with a final newline; the bytes
// decode to the same value.
func TestEncodeLockDeterministic(t *testing.T) {
	a := goodLockEntry()
	b := goodLockEntry()
	b.Name, b.Repo = "profiles", "Profiles"
	l1 := &pluginLock{Plugins: map[string]*lockEntry{"github:z/Z@main": a, "github:a/A@main": b}}
	l2 := &pluginLock{Plugins: map[string]*lockEntry{"github:a/A@main": b, "github:z/Z@main": a}}
	b1, err := encodeLock(l1)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := encodeLock(l2)
	if !bytes.Equal(b1, b2) || !bytes.HasSuffix(b1, []byte("}\n")) {
		t.Fatalf("not deterministic:\n%s\n%s", b1, b2)
	}
	order := []string{`"version"`, `"plugins"`, `"github:a/A@main"`, `"name"`, `"owner"`, `"repo"`, `"ref"`, `"ref_kind"`, `"commit"`, `"digest"`, `"dir"`, `"github:z/Z@main"`}
	at := 0
	for _, k := range order {
		i := bytes.Index(b1[at:], []byte(k))
		if i < 0 {
			t.Fatalf("%s is not after byte %d:\n%s", k, at, b1)
		}
		at += i + len(k)
	}
	got, err := decodeLock(b1)
	if err != nil || !reflect.DeepEqual(got.Plugins, l1.Plugins) {
		t.Fatalf("round trip: %v\n%+v", err, got)
	}
	empty, _ := encodeLock(&pluginLock{})
	if string(empty) != "{\n  \"version\": 1,\n  \"plugins\": {}\n}\n" {
		t.Fatalf("empty lock: %q", empty)
	}
}

// writeLock replaces the file by a rename. A reader during the writes reads the old lock or the
// new lock, never an error or a mix (acceptance matrix: "Hub start during a sync").
func TestWriteLockReadersSeeOldOrNew(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hub.lock.json")
	a := &pluginLock{Plugins: map[string]*lockEntry{"github:lazy-rex/Messaging@main": goodLockEntry()}}
	e := goodLockEntry()
	e.Commit, e.Dir = testCommit2, "github/lazy-rex/Messaging/"+testCommit2+".fedcba9876543210"
	b := &pluginLock{Plugins: map[string]*lockEntry{"github:lazy-rex/Messaging@main": e, "github:lazy-rex/Profiles@main": goodLockEntry()}}
	if err := writeLock(p, a); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				l, err := readLock(p)
				if err != nil {
					errs <- err
					return
				}
				if !reflect.DeepEqual(l.Plugins, a.Plugins) && !reflect.DeepEqual(l.Plugins, b.Plugins) {
					errs <- errors.New("a read gave a lock that is neither the old nor the new one")
					return
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		next := a
		if i%2 == 0 {
			next = b
		}
		if err := writeLock(p, next); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode: %v %v", fi.Mode(), err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".*tmp*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}
