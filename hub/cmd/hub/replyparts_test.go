package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// partsOf cuts text into n slices at UTF-8 character boundaries, as the SDK does.
func partsOf(text string, n int) []string {
	var out []string
	for n > 1 {
		cut := len(text) / n
		for cut < len(text) && text[cut]&0xC0 == 0x80 {
			cut++
		}
		out = append(out, text[:cut])
		text, n = text[cut:], n-1
	}
	return append(out, text)
}

func sendParts(t *testing.T, ps *partSets, client, id string, parts []string) {
	t.Helper()
	for i, p := range parts {
		if _, err := ps.part(client, map[string]any{"id": id, "index": float64(i), "count": float64(len(parts)), "data": p}); err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
	}
}

func stubOf(id string, count, size int) map[string]any {
	return map[string]any{"id": id, "count": float64(count), "bytes": float64(size)}
}

func code(err error) string {
	var se *storeError
	if errors.As(err, &se) {
		return se.code
	}
	return ""
}

// A reply in parts joins back byte-exact, also with multibyte UTF-8 and escapes in the slices.
func TestReplyPartsJoin(t *testing.T) {
	var ps partSets
	text := `{"tree":{"title":"Ünïcode \"quoted\" \\ 日本語 ` + strings.Repeat("x😀", 5000) + `"}}`
	sendParts(t, &ps, "c1", "i-1", partsOf(text, 4))
	got, err := ps.join("c1", stubOf("i-1", 4, len(text)), false)
	if err != nil || string(got) != text {
		t.Fatalf("join: %v, equal %v", err, string(got) == text)
	}
	if ps.sets["c1"] != nil {
		t.Fatal("the set stays after its stub")
	}
	// One part: a reply between inline_max and one part's size (lead-chunking-a3-fix).
	one := `{"a":"` + strings.Repeat("a", 49992) + `"}`
	sendParts(t, &ps, "c1", "i-2", []string{one})
	if got, err := ps.join("c1", stubOf("i-2", 1, 50000), false); err != nil || string(got) != one {
		t.Fatalf("one part: %v", err)
	}
	// A late stub resolves only its own set: the next set stays.
	sendParts(t, &ps, "c1", "i-4", partsOf(text, 2)[:1])
	if _, err := ps.join("c1", stubOf("i-3", 2, len(text)), false); code(err) != "parts_invalid" {
		t.Fatalf("a late stub: %v", err)
	}
	if s := ps.sets["c1"]; s == nil || s.id != "i-4" || len(s.parts) != 1 {
		t.Fatalf("the late stub changed the next set: %+v", s)
	}
}

func TestReplyPartsRefused(t *testing.T) {
	text := `{"a":"` + strings.Repeat("y", 3000) + `"}`
	parts := partsOf(text, 3)
	part := func(ps *partSets, client, id string, index, count int, data string) error {
		_, err := ps.part(client, map[string]any{"id": id, "index": float64(index), "count": float64(count), "data": data})
		return err
	}
	for name, tc := range map[string]struct {
		run  func(ps *partSets) error
		want string
	}{
		"first index not 0": {func(ps *partSets) error { return part(ps, "c1", "i", 1, 3, parts[1]) }, "parts_invalid"},
		"a gap": {func(ps *partSets) error {
			part(ps, "c1", "i", 0, 3, parts[0])
			return part(ps, "c1", "i", 2, 3, parts[2])
		}, "parts_invalid"},
		"the same index, other data": {func(ps *partSets) error {
			part(ps, "c1", "i", 0, 3, parts[0])
			return part(ps, "c1", "i", 0, 3, parts[1])
		}, "parts_invalid"},
		"the same index, equal data": {func(ps *partSets) error {
			part(ps, "c1", "i", 0, 3, parts[0])
			return part(ps, "c1", "i", 0, 3, parts[0])
		}, "parts_invalid"},
		"count 0":          {func(ps *partSets) error { return part(ps, "c1", "i", 0, 0, "x") }, "parts_invalid"},
		"count 65":         {func(ps *partSets) error { return part(ps, "c1", "i", 0, 65, "x") }, "parts_invalid"},
		"a huge count":     {func(ps *partSets) error { return part(ps, "c1", "i", 0, 1<<40, "x") }, "parts_invalid"},
		"an id with a /":   {func(ps *partSets) error { return part(ps, "c1", "a/b", 0, 1, "x") }, "invalid_input"},
		"an id with space": {func(ps *partSets) error { return part(ps, "c1", "a b", 0, 1, "x") }, "invalid_input"},
		"stub bytes over max_total": {func(ps *partSets) error {
			sendParts(t, ps, "c1", "i", parts)
			_, err := ps.join("c1", stubOf("i", 3, partsMaxTotal+1), false)
			return err
		}, "parts_invalid"},
		"count changes": {func(ps *partSets) error {
			part(ps, "c1", "i", 0, 3, parts[0])
			return part(ps, "c1", "i", 1, 4, parts[1])
		}, "parts_invalid"},
		"index over count": {func(ps *partSets) error { return part(ps, "c1", "i", 3, 3, "x") }, "parts_invalid"},
		"an empty part":    {func(ps *partSets) error { return part(ps, "c1", "i", 0, 1, "") }, "parts_invalid"},
		"no id":            {func(ps *partSets) error { return part(ps, "c1", "", 0, 1, "x") }, "invalid_input"},
		"a long id":        {func(ps *partSets) error { return part(ps, "c1", strings.Repeat("i", 129), 0, 1, "x") }, "invalid_input"},
		"a fraction index": {func(ps *partSets) error {
			_, err := ps.part("c1", map[string]any{"id": "i", "index": 0.5, "count": float64(1), "data": "x"})
			return err
		}, "invalid_input"},
		"over max_total": {func(ps *partSets) error {
			big := strings.Repeat("z", partsMaxTotal/2+1)
			part(ps, "c1", "i", 0, 2, big)
			return part(ps, "c1", "i", 1, 2, big)
		}, "too_large"},
		"a missing part at the stub": {func(ps *partSets) error {
			sendParts(t, ps, "c1", "i", parts[:2])
			_, err := ps.join("c1", stubOf("i", 3, len(text)), false)
			return err
		}, "parts_invalid"},
		"a wrong id in the stub": {func(ps *partSets) error {
			sendParts(t, ps, "c1", "i", parts)
			_, err := ps.join("c1", stubOf("j", 3, len(text)), false)
			return err
		}, "parts_invalid"},
		"a count mismatch in the stub": {func(ps *partSets) error {
			sendParts(t, ps, "c1", "i", parts)
			_, err := ps.join("c1", stubOf("i", 2, len(text)), false)
			return err
		}, "parts_invalid"},
		"a bytes mismatch": {func(ps *partSets) error {
			sendParts(t, ps, "c1", "i", parts)
			_, err := ps.join("c1", stubOf("i", 3, len(text)-1), false)
			return err
		}, "parts_invalid"},
		"not one JSON value": {func(ps *partSets) error {
			sendParts(t, ps, "c1", "i", []string{`{"a":`, `1}{}`})
			_, err := ps.join("c1", stubOf("i", 2, 9), false)
			return err
		}, "parts_invalid"},
		"the set of another client": {func(ps *partSets) error {
			sendParts(t, ps, "c1", "i", parts)
			_, err := ps.join("c2", stubOf("i", 3, len(text)), false)
			return err
		}, "parts_invalid"},
		"a stub without fields": {func(ps *partSets) error {
			_, err := ps.join("c1", map[string]any{}, false)
			return err
		}, "parts_invalid"},
	} {
		var ps partSets
		if err := tc.run(&ps); code(err) != tc.want {
			t.Errorf("%s: want %s, got %v", name, tc.want, err)
		}
		// A stub that names no set of its client never touches the set (amendment 3).
		keeps := name == "the set of another client" || name == "a wrong id in the stub"
		if ps.sets["c1"] != nil && !keeps {
			t.Errorf("%s: the set stays after a refusal", name)
		}
	}
}

// A new id discards the old set; a timer removes a set 30 s after its last part; a drop ends it.
func TestReplyPartsLifetime(t *testing.T) {
	now := time.Unix(1000, 0)
	partsNow = func() time.Time { return now }
	var fire []func()
	partsAfter = func(d time.Duration, f func()) *time.Timer {
		fire = append(fire, f)
		return time.NewTimer(time.Hour)
	}
	t.Cleanup(func() { partsNow, partsAfter = time.Now, time.AfterFunc })
	var ps partSets
	sendParts(t, &ps, "c1", "old", []string{`{"a":`})
	sendParts(t, &ps, "c1", "new", []string{`{"b":1}`})
	if _, err := ps.join("c1", stubOf("old", 2, 0), false); code(err) != "parts_invalid" {
		t.Fatalf("the old set after a new id: %v", err)
	}
	if s := ps.sets["c1"]; s == nil || s.id != "new" {
		t.Fatalf("a stub of the old id changed the new set: %+v", s)
	}

	// The timer of a set that got a part meanwhile arms again; with no part for 30 s it removes
	// the set, with no later call.
	ps.drop("c1")
	fire = nil
	if _, err := ps.part("c1", map[string]any{"id": "slow", "index": float64(0), "count": float64(3), "data": `{"a":`}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	if _, err := ps.part("c1", map[string]any{"id": "slow", "index": float64(1), "count": float64(3), "data": `1,`}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	fire[0]() // 35 s after the first part, 15 s after the last
	if ps.sets["c1"] == nil || len(fire) != 2 {
		t.Fatalf("the timer removed a set that got a part 15 s ago (timers %d)", len(fire))
	}
	now = now.Add(16 * time.Second)
	fire[1]()
	if ps.sets["c1"] != nil {
		t.Fatal("the timer left an expired set")
	}

	// A stub after the expiry (before its timer) is refused and ends the set.
	sendParts(t, &ps, "c1", "late", []string{`{}`})
	now = now.Add(partsExpiry + time.Second)
	if _, err := ps.join("c1", stubOf("late", 1, 2), false); code(err) != "parts_invalid" || ps.sets["c1"] != nil {
		t.Fatalf("a stub after the expiry: %v, set %v", err, ps.sets["c1"])
	}
	sendParts(t, &ps, "c3", "y", []string{`{}`})
	ps.drop("c3")
	if ps.sets["c3"] != nil {
		t.Fatal("drop")
	}
}

// Each refusal of a part or a stub of an existing set's id ends that set: a later valid stub
// finds nothing (codex-29, render-e5c5d8b-refusal-lifetime-blocker).
func TestReplyPartsRefusalEndsSet(t *testing.T) {
	text := `{"a":"` + strings.Repeat("y", 300) + `"}`
	for name, refused := range map[string]func(ps *partSets) error{
		"a fraction index": func(ps *partSets) error {
			_, err := ps.part("c1", map[string]any{"id": "i", "index": 0.5, "count": float64(2), "data": "x"})
			return err
		},
		"data not a string": func(ps *partSets) error {
			_, err := ps.part("c1", map[string]any{"id": "i", "index": float64(2), "count": float64(2), "data": 1.0})
			return err
		},
		"a part after the last": func(ps *partSets) error {
			_, err := ps.part("c1", map[string]any{"id": "i", "index": float64(2), "count": float64(2), "data": "x"})
			return err
		},
		"stub bytes over max_total": func(ps *partSets) error {
			_, err := ps.join("c1", stubOf("i", 2, partsMaxTotal+1), false)
			return err
		},
		"stub bytes not a number": func(ps *partSets) error {
			_, err := ps.join("c1", map[string]any{"id": "i", "count": float64(2), "bytes": "many"}, false)
			return err
		},
		"a commit stub with a wrong count": func(ps *partSets) error {
			_, err := ps.join("c1", stubOf("i", 3, len(text)), true)
			return err
		},
	} {
		var ps partSets
		sendParts(t, &ps, "c1", "i", partsOf(text, 2))
		if err := refused(&ps); err == nil {
			t.Errorf("%s: not refused", name)
		}
		if _, err := ps.join("c1", stubOf("i", 2, len(text)), false); code(err) != "parts_invalid" {
			t.Errorf("%s: a later valid stub resolved the set: %v", name, err)
		}
	}
	// A refused stub that names another id leaves the set.
	var ps partSets
	sendParts(t, &ps, "c1", "i", partsOf(text, 2))
	ps.join("c1", map[string]any{"id": "j", "count": "x"}, false)
	if got, err := ps.join("c1", stubOf("i", 2, len(text)), false); err != nil || string(got) != text {
		t.Fatalf("after a stub of another id: %v", err)
	}
}

// One set serves hub.sql.commit reply_parts and the reply stub (lead v1.1, render-chunk-oneset).
func TestReplyPartsCommit(t *testing.T) {
	l := newSQLLab(t)
	var ps partSets
	l.s.replyParts = func(client string, stub map[string]any) ([]byte, error) { return ps.join(client, stub, true) }
	l.must("hub.sql.exec", `{"sql":"CREATE TABLE t (x)"}`)
	reply := `{"id":7,"text":"` + strings.Repeat("é", 40000) + `"}`
	commit := func(key string, parts map[string]any) error {
		tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
		l.must("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (1)", "tx":"`+tx+`"}`)
		_, err := l.call("c1", "hub.sql.commit", js(map[string]any{"tx": tx, "key": key, "fingerprint": "f", "reply_parts": parts}))
		if err != nil {
			l.call("c1", "hub.sql.rollback", `{"tx":"`+tx+`"}`) // as the SDK does after a failed commit
		}
		return err
	}

	sendParts(t, &ps, "c1", "i-1", partsOf(reply, 3))
	if err := commit("k1", stubOf("i-1", 3, len(reply))); err != nil {
		t.Fatalf("commit: %v", err)
	}
	r := l.must("hub.sql.replay", `{"key":"k1", "fingerprint":"f"}`)
	if b, _ := json.Marshal(r["reply"]); string(b) != reply {
		t.Fatalf("the saved reply is not the joined reply")
	}
	if got, err := ps.join("c1", stubOf("i-1", 3, len(reply)), false); err != nil || string(got) != reply {
		t.Fatalf("the stub after the commit: %v", err)
	}
	if ps.sets["c1"] != nil {
		t.Fatal("the set stays after its stub")
	}

	// A commit with a missing part is refused, and nothing is saved.
	sendParts(t, &ps, "c1", "i-2", partsOf(reply, 3)[:2])
	if err := commit("k2", stubOf("i-2", 3, len(reply))); code(err) != "parts_invalid" {
		t.Fatalf("a missing part: %v", err)
	}
	if r := l.must("hub.sql.replay", `{"key":"k2", "fingerprint":"f"}`); r["found"] != false {
		t.Fatalf("saved after a refused commit: %v", r)
	}

	// A commit that fails after it joined the set leaves the set for the stub.
	sendParts(t, &ps, "c1", "i-3", partsOf(reply, 2))
	_, err := l.call("c1", "hub.sql.commit", js(map[string]any{"tx": "t-gone", "key": "k3", "fingerprint": "f", "reply_parts": stubOf("i-3", 2, len(reply))}))
	if err == nil {
		t.Fatal("a commit of an unknown transaction succeeded")
	}
	// A second commit reads the same joined value (one stored value per set).
	if err := commit("k3", stubOf("i-3", 2, len(reply))); err != nil {
		t.Fatalf("a second commit read: %v", err)
	}
	if got, err := ps.join("c1", stubOf("i-3", 2, len(reply)), false); err != nil || string(got) != reply {
		t.Fatalf("the stub after a failed commit: %v", err)
	}

	tx := l.must("hub.sql.begin", `{}`)["tx"].(string)
	l.fails("invalid_input", "hub.sql.commit", js(map[string]any{"tx": tx, "key": "k4", "fingerprint": "f", "reply": 1, "reply_parts": stubOf("x", 1, 1)}))
	l.fails("invalid_input", "hub.sql.commit", js(map[string]any{"tx": tx, "key": "k4", "fingerprint": "f", "reply_parts": "x"}))
}

func TestPartsStub(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"lazyrex_parts":{"id":"i","count":2,"bytes":9}}`:             true,
		` { "lazyrex_parts" : {"id":"i"}}`:                             true,
		`{"lazyrex_parts":{"id":"i"},"tree":{}}`:                       false,
		`{"tree":{"lazyrex_parts":{"id":"i"}}}`:                        false,
		`{"tree":{"title":"x"}}`:                                       false,
		`[]`:                                                           false,
		`{"lazyrex_parts":1}`:                                          true, // a stub that join refuses
		"\n{\"lazyrex_parts\":{\"id\":\"i\",\"count\":1,\"bytes\":2}}": true,
	} {
		if _, ok := partsStub(json.RawMessage(raw)); ok != want {
			t.Errorf("%s: stub %v, want %v", raw, ok, want)
		}
	}
	// A refused stub is a plugin error answer: a render shows the Error tree.
	h := &Hub{}
	stub := json.RawMessage(`{"lazyrex_parts":{"id":"i","count":1,"bytes":2}}`)
	_, err := h.resolveReply("c1", stub)
	if re, definite := definiteFailure(err); !definite || re.Code != "parts_invalid" {
		t.Fatalf("a refused stub: %v", err)
	}
	if v, err := h.resolveReply("c1", json.RawMessage(`{"tree":{}}`)); err != nil || string(v) != `{"tree":{}}` {
		t.Fatalf("a plain reply: %s %v", v, err)
	}
}
