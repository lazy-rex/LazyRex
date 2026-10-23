package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The review of 8437532 (codex-26): placeholders in expected values, an exact data check around a
// replay or a rerun, and the hub log window of the chunking probe.

func TestReleaseSetExpectPlaceholders(t *testing.T) {
	var x []relSetExpect
	json.Unmarshal([]byte(`[{"pointer":"/thread","value":"<SELF>"},{"pointer":"/path","value":"<LAB>/src"},{"pointer":"/p","empty":true}]`), &x)
	vals := map[string]string{"<SELF>": "relseed", "<LAB>": `/tmp/lab "q"`}
	got, err := fillExpects(x, vals)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkExpects([]byte(`{"thread":"relseed","path":"/tmp/lab \"q\"/src","p":{}}`), got); err != nil {
		t.Fatal(err)
	}
	if err := checkExpects([]byte(`{"thread":"<SELF>","path":"/tmp/lab \"q\"/src","p":{}}`), got); err == nil {
		t.Fatal("the literal placeholder passed")
	}
	if string(x[0].Value) != `"<SELF>"` {
		t.Fatalf("fillExpects changed the reviewed expects: %s", x[0].Value)
	}
}

// TestReleaseSetRerunExact: an allowed change of the first forward stays allowed against the seed
// reference, and a rerun that changes nothing passes the exact check; a rerun that inserts a row or
// changes a value fails it.
func TestReleaseSetRerunExact(t *testing.T) {
	keys := map[string][]string{"clock": {"one"}}
	p, h := snapDBs(t, snapSchema+`INSERT INTO drafts VALUES ('a', 'd', 0, 1.0, 0);`, "")
	ref := mustSnap(t, p, h, keys)
	var e relSetEntry
	json.Unmarshal([]byte(`{"changes":[{"table":"drafts","key":{"view_id":"a"},"column":"v","expect":1}],
		"inserts":[{"table":"drafts","max":1,"invariants":[]}]}`), &e)
	p.Exec(`UPDATE drafts SET v = 1`) // the first forward
	if _, err := checkSnap(ref, mustSnap(t, p, h, keys), &e, sqlPredicate(p)); err != nil {
		t.Fatalf("the first forward: %v", err)
	}
	pre := mustSnap(t, p, h, keys)
	// The rerun changes nothing.
	post := mustSnap(t, p, h, keys)
	if err := sameSnap(pre, post); err != nil {
		t.Fatalf("an unchanged rerun: %v", err)
	}
	if d, err := checkSnap(ref, post, &e, sqlPredicate(p)); err != nil || len(d.Changed) != 1 {
		t.Fatalf("the cumulative check after the rerun: %v %+v", err, d)
	}
	// A rerun that inserts an allowed row, or changes a value, is not exact.
	p.Exec(`INSERT INTO drafts VALUES ('b', 'd', 0, 1.0, 0)`)
	if err := sameSnap(pre, mustSnap(t, p, h, keys)); err == nil || !strings.Contains(err.Error(), "only one snapshot") {
		t.Fatalf("an insert: %v", err)
	}
	p.Exec(`DELETE FROM drafts WHERE view_id = 'b'; UPDATE drafts SET dir = 'e'`)
	if err := sameSnap(pre, mustSnap(t, p, h, keys)); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a changed value: %v", err)
	}
	p.Exec(`UPDATE drafts SET dir = 'd'; PRAGMA user_version = 3`)
	if err := sameSnap(pre, mustSnap(t, p, h, keys)); err == nil || !strings.Contains(err.Error(), "user_version") {
		t.Fatalf("a version change: %v", err)
	}
}

// failingReader returns its data, then err.
type failingReader struct {
	data []byte
	err  error
}

func (f failingReader) ReadAt(b []byte, off int64) (int, error) {
	if off >= int64(len(f.data)) {
		return 0, f.err
	}
	return copy(b, f.data[off:]), nil
}

func TestReleaseChunkLogWindow(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "hub.log")
	part := "2026/10/22 hub.reply.part from client:x 1/3\n"
	setup := func(t *testing.T) *relLogMark {
		t.Helper()
		os.WriteFile(logp, []byte("before the window: hub.reply.part from an earlier probe\n"), 0o600)
		m, err := markLog(logp)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.f.Close() })
		return m
	}
	appendLog := func(s string) {
		f, _ := os.OpenFile(logp, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString(s)
		f.Close()
	}
	t.Run("parts", func(t *testing.T) {
		m := setup(t)
		appendLog(part + part)
		var rec relChunkCheck
		if err := checkChunkLog(m, logp, &rec); err != nil || rec.PartLines != 2 {
			t.Fatalf("%v, %d part lines", err, rec.PartLines)
		}
	})
	t.Run("only before the window", func(t *testing.T) {
		m := setup(t)
		var rec relChunkCheck
		if err := checkChunkLog(m, logp, &rec); err == nil || !strings.Contains(err.Error(), "no hub.reply.part") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("refusal", func(t *testing.T) {
		m := setup(t)
		appendLog(part + "refused: parts_invalid\n")
		var rec relChunkCheck
		if err := checkChunkLog(m, logp, &rec); err == nil || !strings.Contains(err.Error(), "refusal") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("replaced, same size", func(t *testing.T) {
		m := setup(t)
		os.Remove(logp)
		os.WriteFile(logp, []byte(strings.Repeat("x", int(m.offset))+part), 0o600)
		var rec relChunkCheck
		if err := checkChunkLog(m, logp, &rec); err == nil || !strings.Contains(err.Error(), "replaced") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("cut", func(t *testing.T) {
		m := setup(t)
		os.Truncate(logp, 3)
		var rec relChunkCheck
		if err := checkChunkLog(m, logp, &rec); err == nil || !strings.Contains(err.Error(), "was cut") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("gone", func(t *testing.T) {
		m := setup(t)
		os.Remove(logp)
		var rec relChunkCheck
		if err := checkChunkLog(m, logp, &rec); err == nil {
			t.Fatal("a removed log passed")
		}
	})
	// A part line, then a read error that is not the end: the read fails, so the window fails.
	t.Run("read error after a part line", func(t *testing.T) {
		if _, err := readFrom(failingReader{[]byte(part), errors.New("disk error")}, 0); err == nil || !strings.Contains(err.Error(), "disk error") {
			t.Fatalf("got %v", err)
		}
		b, err := readFrom(failingReader{[]byte(part), io.EOF}, 0)
		if err != nil || string(b) != part {
			t.Fatalf("the end: %q %v", b, err)
		}
	})
}
