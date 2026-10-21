package main

// hub.prompt.answer (docs/hub.md "Prompt answers"): a plugin with the prompts grant writes the
// user's answer to the pane of an open prompt. Each answer has a durable row in prompt_answers,
// keyed by (plugin, op_key). The key check comes first, so a repeat returns the recorded result
// and writes nothing, also after a hub restart. The hub checks the prompt binding before each
// write and never repeats a write by itself.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// Phases: writing and text_written exist only while a request runs (the start turns them into
// unknown and unknown_after_text). The others are final.
const promptAnswerSchema = `
CREATE TABLE IF NOT EXISTS prompt_answers (plugin TEXT NOT NULL, op_key TEXT NOT NULL, fingerprint TEXT NOT NULL,
	prompt_id TEXT NOT NULL, agent TEXT NOT NULL, block_id TEXT NOT NULL, generation INTEGER NOT NULL,
	hub_generation INTEGER NOT NULL, phase TEXT NOT NULL, result TEXT NOT NULL, at TEXT NOT NULL, closed_at TEXT,
	PRIMARY KEY (plugin, op_key));
`

const (
	promptKeysMax = 64
	promptTextMax = 4096
	promptKeyMax  = 256 // an operation key
)

// promptAnswerKeep is how long a row stays after its prompt closed.
const promptAnswerKeep = 7 * 24 * time.Hour

// promptNow is the clock of answer rows. Tests replace it.
var promptNow = func() time.Time { return time.Now().UTC() }

// promptWrite writes data to a pane and returns bytes_written. Tests replace it.
var promptWrite = func(ctx context.Context, h *Hub, sess, block, data string) (int, error) {
	var out struct {
		BytesWritten int `json:"bytes_written"`
	}
	err := h.rex.BlockCall(ctx, "com.superlogical.terminal.write", sess, block, map[string]any{"data": data}, &out)
	return out.BytesWritten, err
}

// promptAnswerInput is the input of an answer: exactly one of keys and text.
type promptAnswerInput struct {
	Keys   *string `json:"keys,omitempty"`
	Text   *string `json:"text,omitempty"`
	Submit bool    `json:"submit,omitempty"`
}

// promptAnswerLocks makes the answers for one agent run one at a time.
type promptAnswerLocks struct {
	mu    sync.Mutex
	agent map[string]*sync.Mutex
}

func (l *promptAnswerLocks) of(agent string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.agent == nil {
		l.agent = map[string]*sync.Mutex{}
	}
	if l.agent[agent] == nil {
		l.agent[agent] = &sync.Mutex{}
	}
	return l.agent[agent]
}

// promptAnswersStart creates the table and ends the rows of an earlier hub run: a row still
// writing becomes unknown (the hub may have died during the write), and every open row gets
// closed_at (a restart closes every prompt).
func (h *Hub) promptAnswersStart(ctx context.Context) error {
	if _, err := h.db.ExecContext(ctx, promptAnswerSchema); err != nil {
		return err
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE prompt_answers SET phase = 'unknown', result = ? WHERE phase = 'writing'`,
		resultJSON(map[string]any{"reason": "hub_restarted"})); err != nil {
		return err
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE prompt_answers SET phase = 'unknown_after_text', result = ? WHERE phase = 'text_written'`,
		resultJSON(map[string]any{"text_written": true, "enter": "unknown", "reason": "hub_restarted"})); err != nil {
		return err
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE prompt_answers SET closed_at = ? WHERE closed_at IS NULL AND hub_generation != ?`,
		stamp(promptNow()), h.generation); err != nil {
		return err
	}
	h.sweepPromptAnswers(ctx)
	go func() {
		for range time.Tick(time.Hour) {
			h.sweepPromptAnswers(context.Background())
		}
	}()
	return nil
}

// sweepPromptAnswers sets closed_at of rows whose prompt is closed, and deletes rows that closed
// more than promptAnswerKeep ago. A row of an open prompt stays. closed_at is the first sweep
// that saw the prompt closed, so it is at most one sweep interval late: retention only grows.
// A prompt id never opens again after its close, so a prompt that is not open in the snapshot
// stays closed: the writes need no h.mu (the store has one connection, and a transaction can
// wait for h.mu, so no store call runs under h.mu here).
func (h *Hub) sweepPromptAnswers(ctx context.Context) {
	rows, err := h.db.QueryContext(ctx, `SELECT DISTINCT prompt_id FROM prompt_answers WHERE closed_at IS NULL`)
	if err != nil {
		log.Printf("prompt answers: sweep: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	h.mu.Lock()
	h.syncPrompts()
	open := map[string]bool{}
	for _, p := range h.prompts.open {
		open[p.ID] = true
	}
	h.mu.Unlock()
	now := promptNow()
	for _, id := range ids {
		if !open[id] {
			h.db.ExecContext(ctx, `UPDATE prompt_answers SET closed_at = ? WHERE prompt_id = ? AND closed_at IS NULL`, stamp(now), id)
		}
	}
	if _, err := h.db.ExecContext(ctx, `DELETE FROM prompt_answers WHERE closed_at IS NOT NULL AND closed_at < ?`,
		stamp(now.Add(-promptAnswerKeep))); err != nil {
		log.Printf("prompt answers: delete: %v", err)
	}
}

// stamp is a fixed-width UTC time, so stamps compare as text.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func resultJSON(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// promptAnswer is the handler of hub.prompt.answer.
func (h *Hub) promptAnswer(ctx context.Context, pl *Plugin, args map[string]any) (any, error) {
	if !pl.run.Prompts {
		return nil, refuse("not_granted", "the hub config gives plugin %s no prompts grant", pl.Name)
	}
	id, _ := args["prompt_id"].(string)
	key, _ := args["key"].(string)
	if id == "" || key == "" || len(key) > promptKeyMax {
		return nil, bad("invalid: prompt_id and key (at most %d bytes) are required", promptKeyMax)
	}
	for k := range args {
		if k != "prompt_id" && k != "key" && k != "input" {
			return nil, bad("invalid: unknown field %s", k)
		}
	}
	in, err := parsePromptInput(args["input"])
	if err != nil {
		return nil, err
	}
	canon, _ := json.Marshal(struct {
		PromptID string            `json:"prompt_id"`
		Input    promptAnswerInput `json:"input"`
	}{id, in})
	sum := sha256.Sum256(canon)
	fp := hex.EncodeToString(sum[:])

	// 1. The key check.
	if out, done, err := h.promptAnswerRecorded(ctx, pl.Name, key, fp); done || err != nil {
		return out, err
	}
	// 2. The binding check. The agent's answers run one at a time.
	h.mu.Lock()
	p := h.promptOpen(id)
	h.mu.Unlock()
	if p == nil {
		return h.promptAnswerClosed(ctx, pl.Name, key, fp, id)
	}
	lock := h.promptLocks.of(p.Agent)
	lock.Lock()
	defer lock.Unlock()
	// 3. The row, in phase writing, before any byte. A close after the check leaves closed_at
	// unset only until the next sweep: the sweep closes every row whose prompt is not open.
	h.mu.Lock()
	p = h.promptOpen(id)
	h.mu.Unlock()
	if p == nil {
		return h.promptAnswerClosed(ctx, pl.Name, key, fp, id)
	}
	res, err := h.db.ExecContext(ctx, `INSERT INTO prompt_answers (plugin, op_key, fingerprint, prompt_id, agent, block_id, generation,
		hub_generation, phase, result, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'writing', '{}', ?) ON CONFLICT DO NOTHING`,
		pl.Name, key, fp, id, p.Agent, p.Block, p.Gen, h.generation, stamp(promptNow()))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 { // a request with the same key came first
		out, _, err := h.promptAnswerRecorded(ctx, pl.Name, key, fp)
		return out, err
	}
	h.ev.emit("prompt_answer", map[string]any{"plugin": pl.Name, "key": key, "prompt_id": id, "agent": p.Agent, "phase": "writing"})
	return h.promptAnswerWrite(ctx, pl.Name, key, p, in)
}

func parsePromptInput(v any) (promptAnswerInput, error) {
	var in promptAnswerInput
	raw, _ := json.Marshal(v)
	if err := decodeManifest("input", raw, &in); err != nil {
		return in, bad("invalid: input must be {keys} or {text, submit: true}: %v", err)
	}
	switch {
	case (in.Keys == nil) == (in.Text == nil):
		return in, bad("invalid: input needs exactly one of keys and text")
	case in.Keys != nil && (len(*in.Keys) == 0 || len(*in.Keys) > promptKeysMax || in.Submit):
		return in, bad("invalid: keys must be 1 to %d bytes, without submit", promptKeysMax)
	case in.Text != nil && (len(*in.Text) == 0 || len(*in.Text) > promptTextMax || !in.Submit):
		return in, bad("invalid: text must be 1 to %d bytes, with submit: true", promptTextMax)
	}
	return in, nil
}

// promptAnswerRecorded is the key check. done reports that the key has a row: its result, or
// operation_conflict for other input. A row still writing gives in_progress, which is not final.
func (h *Hub) promptAnswerRecorded(ctx context.Context, plugin, key, fp string) (any, bool, error) {
	var rowFP, phase, result string
	err := h.db.QueryRowContext(ctx, `SELECT fingerprint, phase, result FROM prompt_answers WHERE plugin = ? AND op_key = ?`,
		plugin, key).Scan(&rowFP, &phase, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if rowFP != fp {
		return nil, true, refuse("operation_conflict", "key %s was used with another prompt_id or input", key)
	}
	if phase == "writing" || phase == "text_written" {
		return map[string]any{"phase": "in_progress"}, true, nil
	}
	return answerReply(phase, result), true, nil
}

func answerReply(phase, result string) map[string]any {
	var r any
	json.Unmarshal([]byte(result), &r)
	return map[string]any{"phase": phase, "result": r}
}

// promptAnswerClosed records a refused answer: its prompt is not open. The row is closed at once.
func (h *Hub) promptAnswerClosed(ctx context.Context, plugin, key, fp, id string) (any, error) {
	result := resultJSON(map[string]any{"reason": "prompt_closed"})
	now := stamp(promptNow())
	res, err := h.db.ExecContext(ctx, `INSERT INTO prompt_answers (plugin, op_key, fingerprint, prompt_id, agent, block_id, generation,
		hub_generation, phase, result, at, closed_at) VALUES (?, ?, ?, ?, '', '', 0, ?, 'refused', ?, ?, ?) ON CONFLICT DO NOTHING`,
		plugin, key, fp, id, h.generation, result, now, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		out, _, err := h.promptAnswerRecorded(ctx, plugin, key, fp)
		return out, err
	}
	h.ev.emit("prompt_answer", map[string]any{"plugin": plugin, "key": key, "prompt_id": id, "phase": "refused"})
	return answerReply("refused", result), nil
}

// writeOutcome classifies a write: ok (every byte, answered), none (Rex refused it before any
// byte: block or session not found) or unknown.
func writeOutcome(data string, n int, err error) (string, map[string]any) {
	if err == nil && n == len(data) {
		return "ok", nil
	}
	if err == nil {
		return "unknown", map[string]any{"bytes_written": n, "length": len(data)}
	}
	var re *rex.RexError
	if errors.As(err, &re) && (re.Code == "not_found" || re.Code == "session_not_found") {
		return "none", map[string]any{"code": re.Code}
	}
	return "unknown", map[string]any{"error": err.Error()}
}

// promptAnswerWrite runs the checks and writes of one answer, and records each phase.
func (h *Hub) promptAnswerWrite(ctx context.Context, plugin, key string, p *prompt, in promptAnswerInput) (any, error) {
	finish := func(phase string, result map[string]any) (any, error) {
		if result == nil {
			result = map[string]any{}
		}
		r := resultJSON(result)
		if _, err := h.db.ExecContext(context.Background(), `UPDATE prompt_answers SET phase = ?, result = ? WHERE plugin = ? AND op_key = ?`,
			phase, r, plugin, key); err != nil {
			return nil, err // the row stays writing: the next start makes it unknown
		}
		h.ev.emit("prompt_answer", map[string]any{"plugin": plugin, "key": key, "prompt_id": p.ID, "agent": p.Agent, "phase": phase})
		return answerReply(phase, r), nil
	}
	// still reports whether p is still open (h.mu held only for the check).
	still := func(when string) bool {
		h.promptAnswerFault(when, p)
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.promptOpen(p.ID) == p
	}
	write := func(when, data string) (string, map[string]any) {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := h.promptWriteFaulted(wctx, when, p, data)
		return writeOutcome(data, n, err)
	}

	// 4. The binding check right before the write.
	if !still("before-write") {
		return finish("refused", map[string]any{"reason": "prompt_closed"})
	}
	if in.Keys != nil {
		switch o, info := write("write", *in.Keys); o {
		case "ok":
			return finish("done", nil)
		case "none":
			return finish("failed", info)
		default:
			return finish("unknown", info)
		}
	}
	// text: the paste, then Enter. Newlines become spaces, as in notices.
	paste := "\x1b[200~" + strings.ReplaceAll(*in.Text, "\n", " ") + "\x1b[201~"
	switch o, info := write("write", paste); o {
	case "none":
		return finish("failed", info)
	case "unknown":
		return finish("unknown", info)
	}
	if _, err := h.db.ExecContext(context.Background(), `UPDATE prompt_answers SET phase = 'text_written' WHERE plugin = ? AND op_key = ?`, plugin, key); err != nil {
		return nil, err
	}
	// The paste can change the agent's state: check again before Enter.
	if !still("before-enter") {
		return finish("partial", map[string]any{"text_written": true, "enter": "not_sent", "reason": "prompt_closed"})
	}
	switch o, info := write("enter", "\r"); o {
	case "ok":
		return finish("done", nil)
	case "none":
		info["text_written"], info["enter"] = true, "failed"
		return finish("partial", info)
	default:
		info["text_written"], info["enter"] = true, "unknown"
		return finish("unknown_after_text", info)
	}
}
