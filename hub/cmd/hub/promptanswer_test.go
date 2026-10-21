package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// fakeWrite replaces promptWrite. answer gives the result of the n-th write (1-based); nil means
// every byte, answered.
type fakeWrite struct {
	mu     sync.Mutex
	data   []string
	answer func(n int, data string) (int, error)
}

func (f *fakeWrite) install(t *testing.T) {
	old := promptWrite
	t.Cleanup(func() { promptWrite = old })
	promptWrite = func(ctx context.Context, h *Hub, sess, block, data string) (int, error) {
		f.mu.Lock()
		f.data = append(f.data, data)
		n := len(f.data)
		f.mu.Unlock()
		if f.answer != nil {
			return f.answer(n, data)
		}
		return len(data), nil
	}
}

func (f *fakeWrite) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.data...)
}

// answerHub is promptHub with the answer table, a test state for faults, a fixed clock and an
// open prompt for alice.
func answerHub(t *testing.T) (*Hub, *Agent, *Plugin, *fakeWrite) {
	t.Helper()
	h, a := promptHub(t)
	if _, err := h.db.Exec(promptAnswerSchema); err != nil {
		t.Fatal(err)
	}
	h.test = newTestState()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	old := promptNow
	t.Cleanup(func() { promptNow = old })
	promptNow = func() time.Time { return now }
	setStatus(h, a, statusRecord{State: "blocked", Kind: "question", Msg: "Pick a color"})
	f := &fakeWrite{}
	f.install(t)
	return h, a, &Plugin{Name: "telegram", entry: &configEntry{index: 1}, run: pluginRun{Prompts: true}}, f
}

func answer(t *testing.T, h *Hub, pl *Plugin, id, key string, input map[string]any) (map[string]any, error) {
	t.Helper()
	out, err := h.promptAnswer(context.Background(), pl, map[string]any{"prompt_id": id, "key": key, "input": input})
	if err != nil {
		return nil, err
	}
	return out.(map[string]any), nil
}

func phaseOf(t *testing.T, out map[string]any, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return out["phase"].(string)
}

func reasonOf(out map[string]any) any {
	r, _ := out["result"].(map[string]any)
	return r["reason"]
}

func setClock(at time.Time) { promptNow = func() time.Time { return at } }

// keys: one write, no Enter. A repeat returns the first result and writes nothing; the same key
// with other input is operation_conflict.
func TestPromptAnswerKeysReplay(t *testing.T) {
	h, _, pl, f := answerHub(t)
	id := openPrompt(h, "alice").ID
	if ph := answerPhase(t, h, pl, id, "tg:1", map[string]any{"keys": "2"}); ph != "done" {
		t.Fatalf("phase %s", ph)
	}
	if ph := answerPhase(t, h, pl, id, "tg:1", map[string]any{"keys": "2"}); ph != "done" {
		t.Fatalf("repeat: %s", ph)
	}
	if _, err := answer(t, h, pl, id, "tg:1", map[string]any{"keys": "3"}); err == nil || !strings.Contains(err.Error(), "operation_conflict") && !strings.Contains(err.Error(), "another prompt_id or input") {
		t.Fatalf("other input: %v", err)
	}
	if w := f.writes(); len(w) != 1 || w[0] != "2" {
		t.Fatalf("writes %q", w)
	}
}

// text: a bracketed paste (newlines become spaces), then Enter.
func TestPromptAnswerText(t *testing.T) {
	h, _, pl, f := answerHub(t)
	id := openPrompt(h, "alice").ID
	if ph := answerPhase(t, h, pl, id, "tg:t", map[string]any{"text": "blue\nplease", "submit": true}); ph != "done" {
		t.Fatalf("phase %s", ph)
	}
	if w := f.writes(); len(w) != 2 || w[0] != "\x1b[200~blue please\x1b[201~" || w[1] != "\r" {
		t.Fatalf("writes %q", w)
	}
}

// Refusals: no grant, bad input, a closed prompt (zero bytes; the row is closed at once).
func TestPromptAnswerRefusals(t *testing.T) {
	h, a, pl, f := answerHub(t)
	id := openPrompt(h, "alice").ID
	if _, err := h.promptAnswer(context.Background(), &Plugin{Name: "other"}, map[string]any{"prompt_id": id, "key": "k", "input": map[string]any{"keys": "1"}}); err == nil || !strings.Contains(err.Error(), "no prompts grant") {
		t.Fatalf("no grant: %v", err)
	}
	for _, in := range []map[string]any{{}, {"keys": "1", "text": "x", "submit": true}, {"keys": ""}, {"keys": strings.Repeat("k", 65)},
		{"text": "x"}, {"text": strings.Repeat("t", 4097), "submit": true}, {"keys": "1", "submit": true}, {"keys": "1", "extra": 1}} {
		if _, err := answer(t, h, pl, id, "bad", in); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("input %v: %v", in, err)
		}
	}
	setStatus(h, a, statusRecord{State: "working"})
	out, err := answer(t, h, pl, id, "late", map[string]any{"keys": "1"})
	if phaseOf(t, out, err) != "refused" || reasonOf(out) != "prompt_closed" {
		t.Fatalf("closed: %+v", out)
	}
	var closed string
	h.db.QueryRow(`SELECT closed_at FROM prompt_answers WHERE op_key = 'late'`).Scan(&closed)
	if closed == "" || len(f.writes()) != 0 {
		t.Fatalf("closed_at %q, writes %q", closed, f.writes())
	}
}

// A late answer to prompt A after prompt B opened in the same block: zero bytes.
func TestPromptAnswerLateAfterNewPrompt(t *testing.T) {
	h, a, pl, f := answerHub(t)
	pa := openPrompt(h, "alice")
	setStatus(h, a, statusRecord{State: "blocked", Kind: "question", Msg: "Pick a size"})
	if pb := openPrompt(h, "alice"); pb == pa || pb.Gen != pa.Gen+1 {
		t.Fatal("B did not open")
	}
	out, err := answer(t, h, pl, pa.ID, "late", map[string]any{"keys": "1"})
	if phaseOf(t, out, err) != "refused" || len(f.writes()) != 0 {
		t.Fatalf("late answer: %+v, writes %q", out, f.writes())
	}
}

// The binding changes between the first check and the write (fault close, before-write): zero
// bytes. It changes after the paste (before-enter): partial, no Enter.
func TestPromptAnswerRecheck(t *testing.T) {
	h, _, pl, f := answerHub(t)
	if _, err := h.testFaultArm(map[string]any{"point": "prompt-answer", "mode": "close", "when": "before-write"}, "telegram"); err != nil {
		t.Fatal(err)
	}
	out, err := answer(t, h, pl, openPrompt(h, "alice").ID, "k1", map[string]any{"keys": "1"})
	if phaseOf(t, out, err) != "refused" || reasonOf(out) != "prompt_closed" || len(f.writes()) != 0 {
		t.Fatalf("before-write: %+v, writes %q", out, f.writes())
	}
	h.mu.Lock()
	h.syncPrompts() // the record is still blocked: a new prompt opens, as after a rebind
	h.mu.Unlock()
	if _, err := h.testFaultArm(map[string]any{"point": "prompt-answer", "mode": "close", "when": "before-enter"}, "telegram"); err != nil {
		t.Fatal(err)
	}
	out, err = answer(t, h, pl, openPrompt(h, "alice").ID, "k2", map[string]any{"text": "blue", "submit": true})
	r, _ := out["result"].(map[string]any)
	if phaseOf(t, out, err) != "partial" || r["enter"] != "not_sent" || r["text_written"] != true {
		t.Fatalf("before-enter: %+v", out)
	}
	if w := f.writes(); len(w) != 1 || !strings.HasPrefix(w[0], "\x1b[200~") {
		t.Fatalf("writes %q: the paste only", w)
	}
}

// Write errors: only not_found and session_not_found prove zero bytes. Every other error, and a
// short write, is unknown. An Enter that fails gives partial or unknown_after_text.
func TestPromptAnswerWriteErrors(t *testing.T) {
	cases := []struct {
		input  map[string]any
		answer func(n int, data string) (int, error)
		phase  string
	}{
		{map[string]any{"keys": "1"}, func(int, string) (int, error) { return 0, &rex.RexError{Code: "not_found"} }, "failed"},
		{map[string]any{"keys": "1"}, func(int, string) (int, error) { return 0, &rex.RexError{Code: "session_not_found"} }, "failed"},
		{map[string]any{"keys": "1"}, func(int, string) (int, error) { return 0, &rex.RexError{Code: "bad_request"} }, "unknown"},
		{map[string]any{"keys": "1"}, func(int, string) (int, error) { return 0, &rex.RexError{Code: "disconnected"} }, "unknown"},
		{map[string]any{"keys": "12"}, func(int, string) (int, error) { return 1, nil }, "unknown"},
		{map[string]any{"text": "x", "submit": true}, func(n int, d string) (int, error) {
			if n == 2 {
				return 0, &rex.RexError{Code: "not_found"}
			}
			return len(d), nil
		}, "partial"},
		{map[string]any{"text": "x", "submit": true}, func(n int, d string) (int, error) {
			if n == 2 {
				return 0, &rex.RexError{Code: "disconnected"}
			}
			return len(d), nil
		}, "unknown_after_text"},
		{map[string]any{"text": "x", "submit": true}, func(n int, d string) (int, error) { return 0, &rex.RexError{Code: "timeout"} }, "unknown"},
	}
	for i, c := range cases {
		h, _, pl, f := answerHub(t)
		f.answer = c.answer
		out, err := answer(t, h, pl, openPrompt(h, "alice").ID, "k", c.input)
		if ph := phaseOf(t, out, err); ph != c.phase {
			t.Errorf("case %d: phase %s, want %s (%+v)", i, ph, c.phase, out)
		}
		if c.phase == "unknown" && len(f.writes()) > 1 {
			t.Errorf("case %d: no Enter after an unknown paste: %q", i, f.writes())
		}
	}
}

// A crash during a write: the start makes the row unknown (or unknown_after_text); a repeat
// returns that result and writes nothing. After a restart, a repeat with the old prompt_id
// returns the recorded result (H1d), not prompt_closed.
func TestPromptAnswerRestart(t *testing.T) {
	h, _, pl, f := answerHub(t)
	id := openPrompt(h, "alice").ID
	answerPhase(t, h, pl, id, "done-key", map[string]any{"keys": "1"})
	ins := `INSERT INTO prompt_answers (plugin, op_key, fingerprint, prompt_id, agent, block_id, generation, hub_generation, phase, result, at)
		VALUES ('telegram', ?, ?, ?, 'alice', 'block:b', 1, 7, ?, '{}', '2026-10-21T12:00:00.000000000Z')`
	in := promptAnswerInput{Keys: new(string)}
	*in.Keys = "1"
	fp := func() string { // the fingerprint of {prompt_id: id, input: {keys: "1"}}
		var s string
		h.db.QueryRow(`SELECT fingerprint FROM prompt_answers WHERE op_key = 'done-key'`).Scan(&s)
		return s
	}()
	h.db.Exec(ins, "crash-1", fp, id, "writing")
	h.db.Exec(ins, "crash-2", fp, id, "text_written")
	// The restart: a new generation, no prompts.
	h.mu.Lock()
	h.generation = 8
	h.prompts = promptBook{}
	h.mu.Unlock()
	if err := h.promptAnswersStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(f.writes())
	for key, want := range map[string]string{"crash-1": "unknown", "crash-2": "unknown_after_text", "done-key": "done"} {
		if ph := answerPhase(t, h, pl, id, key, map[string]any{"keys": "1"}); ph != want {
			t.Errorf("%s: phase %s, want %s", key, ph, want)
		}
	}
	if len(f.writes()) != before {
		t.Fatalf("a repeat wrote: %q", f.writes())
	}
	var open int
	h.db.QueryRow(`SELECT COUNT(*) FROM prompt_answers WHERE closed_at IS NULL`).Scan(&open)
	if open != 0 {
		t.Fatalf("%d rows of the old run are not closed", open)
	}
}

// Retention: a row stays while its prompt is open (also past 7 days), and for 7 full days after
// the close. After the deletion, a repeat returns prompt_closed with zero bytes.
func TestPromptAnswerRetention(t *testing.T) {
	h, a, pl, f := answerHub(t)
	id := openPrompt(h, "alice").ID
	t0 := promptNow()
	answerPhase(t, h, pl, id, "k", map[string]any{"keys": "1"})
	count := func() (n int) {
		h.db.QueryRow(`SELECT COUNT(*) FROM prompt_answers WHERE op_key = 'k'`).Scan(&n)
		return
	}
	setClock(t0.Add(10 * 24 * time.Hour))
	h.sweepPromptAnswers(context.Background())
	if count() != 1 || answerPhase(t, h, pl, id, "k", map[string]any{"keys": "1"}) != "done" {
		t.Fatal("the row of an open prompt must stay")
	}
	setStatus(h, a, statusRecord{State: "working"})
	closeAt := t0.Add(11 * 24 * time.Hour)
	setClock(closeAt)
	h.sweepPromptAnswers(context.Background()) // sets closed_at
	setClock(closeAt.Add(promptAnswerKeep - time.Second))
	h.sweepPromptAnswers(context.Background())
	if count() != 1 {
		t.Fatal("deleted before 7 full days after the close")
	}
	setClock(closeAt.Add(promptAnswerKeep + time.Second))
	h.sweepPromptAnswers(context.Background())
	if count() != 0 {
		t.Fatal("not deleted 7 days after the close")
	}
	writes := len(f.writes())
	out, err := answer(t, h, pl, id, "k", map[string]any{"keys": "1"})
	if phaseOf(t, out, err) != "refused" || reasonOf(out) != "prompt_closed" || len(f.writes()) != writes {
		t.Fatalf("after deletion: %+v", out)
	}
}

// A repeat while the first request writes: in_progress, with no write. The first request then
// completes.
func TestPromptAnswerInProgress(t *testing.T) {
	h, _, pl, f := answerHub(t)
	id := openPrompt(h, "alice").ID
	started, release := make(chan struct{}), make(chan struct{})
	f.answer = func(n int, d string) (int, error) {
		close(started)
		<-release
		return len(d), nil
	}
	done := make(chan string)
	go func() {
		out, err := answer(t, h, pl, id, "k", map[string]any{"keys": "1"})
		if err != nil {
			done <- err.Error()
			return
		}
		done <- out["phase"].(string)
	}()
	<-started
	if ph := answerPhase(t, h, pl, id, "k", map[string]any{"keys": "1"}); ph != "in_progress" {
		t.Fatalf("repeat during the write: %s", ph)
	}
	close(release)
	if ph := <-done; ph != "done" {
		t.Fatalf("first: %s", ph)
	}
	if len(f.writes()) != 1 {
		t.Fatalf("writes %q", f.writes())
	}
}

func answerPhase(t *testing.T, h *Hub, pl *Plugin, id, key string, input map[string]any) string {
	t.Helper()
	out, err := answer(t, h, pl, id, key, input)
	return phaseOf(t, out, err)
}

// A close between the binding check and the insert: the row has no closed_at, and the next sweep
// closes it, because its prompt is not open. The row of an open prompt stays open.
func TestPromptAnswerCloseDuringAdmission(t *testing.T) {
	h, _, _, _ := answerHub(t)
	open := openPrompt(h, "alice").ID
	ins := `INSERT INTO prompt_answers (plugin, op_key, fingerprint, prompt_id, agent, block_id, generation, hub_generation, phase, result, at)
		VALUES ('telegram', ?, 'fp', ?, 'alice', 'block:b', 1, 7, 'done', '{}', '2026-10-21T12:00:00.000000000Z')`
	h.db.Exec(ins, "raced", "0123456789abcdef") // its prompt closed before the insert
	h.db.Exec(ins, "open", open)
	h.sweepPromptAnswers(context.Background())
	closed := map[string]bool{}
	rows, _ := h.db.Query(`SELECT op_key, closed_at IS NOT NULL FROM prompt_answers`)
	for rows.Next() {
		var k string
		var c bool
		rows.Scan(&k, &c)
		closed[k] = c
	}
	rows.Close()
	if !closed["raced"] || closed["open"] {
		t.Fatalf("closed: %v", closed)
	}
}
