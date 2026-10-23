package main

// The calls of a release-set rehearsal: agent action calls from a lab pane (seed and probe items of
// kind "action"), and the expect checks of a result.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	relSeedAgent = "relseed" // the lab-only roster agent that runs the action items
	relPeerAgent = "relpeer" // a second lab-only roster agent that never joins (<PEER>)
)

// relCallOut is the result of one call: the rex exit code and its output.
type relCallOut struct {
	Code int
	Out  []byte
	Vals map[string]string // the placeholder values of the batch
}

// fillExpects returns expects with the lab placeholders replaced in each expected value. The set
// bytes stay as the owner reviewed them.
func fillExpects(expects []relSetExpect, vals map[string]string) []relSetExpect {
	out := make([]relSetExpect, len(expects))
	for i, x := range expects {
		out[i] = x
		if len(x.Value) > 0 {
			out[i].Value = json.RawMessage(fillPlaceholders(x.Value, vals))
		}
	}
	return out
}

// fillPlaceholders replaces the lab placeholders in a JSON text. Each value is JSON-escaped, so a
// placeholder must stand inside a JSON string.
func fillPlaceholders(raw []byte, vals map[string]string) []byte {
	s := string(raw)
	for k, v := range vals {
		b, _ := json.Marshal(v)
		s = strings.ReplaceAll(s, k, string(b[1:len(b)-1]))
	}
	return []byte(s)
}

// agentBatch runs calls as the lab agent relSeedAgent from one new pane of the lab session: the
// pane joins, runs each call with the rex CLI through the agent face, and ends. The tool writes the
// args files only after it knows the pane's session and block (<SESSION_ID>, <BLOCK_ID>), and the
// pane waits for them. After the batch the tool closes the pane and waits until the hub has no
// live binding of the agent. The next batch rebinds the agent to its own pane before the join.
func (h *rehearsal) agentBatch(name string, calls []relSetItem) ([]relCallOut, error) {
	var client string
	if err := h.withLab(func(lr *releaser) error {
		hub, err := lr.currentHub()
		if err == nil {
			client = hub.Client
		}
		return err
	}); err != nil {
		return nil, err
	}
	sb, err := os.ReadFile(filepath.Join(h.run, "session"))
	if err != nil {
		return nil, err
	}
	session := strings.TrimSpace(string(sb))
	dir := filepath.Join(h.lab, "calls", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// hub.join goes to the hub client; the items go to the agent face (agentLabel), which carries
	// the plugin actions that agents may call.
	rexDo := func(client string) string {
		return fmt.Sprintf("rex -S %s --autostart=false --timeout 60s -C %s do", shQuote(h.l.server), shQuote(client))
	}
	lines := []string{"#!/bin/sh",
		"cd " + shQuote(dir),
		"while [ ! -e go ]; do sleep 0.1; done",
		fmt.Sprintf(`%s hub.join --args '{"agent":"%s"}' > join.out 2>&1; echo $? > join.code`, rexDo(client), relSeedAgent)}
	for i, c := range calls {
		lines = append(lines, fmt.Sprintf(`%s %s --args @args-%d.json > out-%d 2>&1; echo $? > code-%d`, rexDo(agentLabel), shQuote(c.Action), i, i, i))
	}
	lines = append(lines, "echo done > done", "")
	script := filepath.Join(dir, "batch.sh")
	if err := os.WriteFile(script, []byte(strings.Join(lines, "\n")), 0o700); err != nil {
		return nil, err
	}
	arg, _ := json.Marshal(map[string]any{"session_id": session, "flavor": "com.superlogical.terminal.shell",
		"label": relAgentLabel + "-" + name, "options": map[string]any{"command": []string{"/bin/sh", script}, "login": false, "exit": map[string]bool{"on_completion": false}}})
	var block string
	if err := h.withLab(func(lr *releaser) error {
		out, err := lr.rex("api", "call", "session.new_block", string(arg))
		if err != nil {
			return err
		}
		var nb struct {
			BlockID string `json:"block_id"`
		}
		if json.Unmarshal(out, &nb) != nil || nb.BlockID == "" {
			return relFail("session.new_block gave no block_id: %s", out)
		}
		block = nb.BlockID
		return nil
	}); err != nil {
		return nil, relFail("the agent pane %s: %v", name, err)
	}
	// A closed pane does not free the agent name: a join from a new block is refused while the
	// binding names the old block. So bind the agent to the new pane first (its shell waits for
	// the go file, so the pane has a live child); its join is then a join from the bound block.
	bound, _, _, err := h.binding(relSeedAgent)
	if err != nil {
		return nil, err
	}
	if bound != "" && bound != block {
		if err := h.withLab(func(lr *releaser) error {
			_, err := lr.hubDo(client, "hub.admin.rebind", map[string]string{"agent": relSeedAgent, "block_id": block})
			return err
		}); err != nil {
			return nil, relFail("the agent pane %s: rebinding %s from %s: %v", name, relSeedAgent, bound, err)
		}
	}
	vals := h.placeholders(session, block)
	for i, c := range calls {
		args := c.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("args-%d.json", i)), fillPlaceholders(args, vals), 0o600); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o600); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Duration(30+60*len(calls)) * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, relFail("the agent pane %s did not finish; see %s", name, dir)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if code := readCode(filepath.Join(dir, "join.code")); code != 0 {
		return nil, relFail("the agent pane %s: hub.join exited %d: %s", name, code, bytes.TrimSpace(mustReadFile(filepath.Join(dir, "join.out"))))
	}
	outs := make([]relCallOut, len(calls))
	for i := range calls {
		outs[i] = relCallOut{Code: readCode(filepath.Join(dir, fmt.Sprintf("code-%d", i))), Out: mustReadFile(filepath.Join(dir, fmt.Sprintf("out-%d", i))), Vals: vals}
	}
	if err := h.closePane(session, block, relSeedAgent); err != nil {
		return nil, err
	}
	return outs, nil
}

// readCode reads an exit code file; a missing or bad file is -1.
func readCode(p string) int {
	n, err := strconv.Atoi(strings.TrimSpace(string(mustReadFile(p))))
	if err != nil {
		return -1
	}
	return n
}

// placeholders are the values of the lab placeholders of the set file.
func (h *rehearsal) placeholders(session, block string) map[string]string {
	return map[string]string{"<LAB>": h.lab, "<SELF>": relSeedAgent, "<PEER>": relPeerAgent, "<SESSION_ID>": session, "<BLOCK_ID>": block}
}

// closePane closes a lab pane and waits (15 s) until the lab hub.db has no binding of agent ("":
// no agent joined in it).
func (h *rehearsal) closePane(session, block, agent string) error {
	arg, _ := json.Marshal(map[string]string{"session_id": session, "block_id": block})
	if err := h.withLab(func(lr *releaser) error {
		_, err := lr.rex("api", "call", "block.close", string(arg))
		return err
	}); err != nil {
		return relFail("closing the pane %s: %v", block, err)
	}
	if agent == "" {
		return nil
	}
	// The hub keeps a binding row after its process ends ("bound but not live"); the next batch
	// rebinds the agent to its new pane. So wait until the binding is not live: no row, or the row
	// of the closed block whose process (pid and start time) has ended.
	for i := 0; i < 75; i++ {
		bound, pid, start, err := h.binding(agent)
		switch {
		case err != nil:
			return err
		case bound == "":
			return nil
		case bound != block:
			return relFail("the binding of %s names the block %s, not the closed pane %s", agent, bound, block)
		case startTime(pid) != start:
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return relFail("the bound process of %s still runs after its pane %s closed", agent, block)
}

// binding reads the binding row of agent from the lab hub.db: its block ("": no row), and the pid
// and start time of its process.
func (h *rehearsal) binding(agent string) (string, int, string, error) {
	db, err := h.hubDB(true)
	if err != nil {
		return "", 0, "", err
	}
	defer db.Close()
	var block, start string
	var pid int
	err = db.QueryRow("SELECT block_id, child_pid, child_start FROM bindings WHERE agent = ?", agent).Scan(&block, &pid, &start)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, "", nil
	}
	if err != nil {
		return "", 0, "", relFail("the binding of %s: %v", agent, err)
	}
	return block, pid, start, nil
}

// checkExpects applies the expects of an item to a JSON result. Value: the canonical JSON at the
// pointer equals the canonical expected value. Empty: the value is {} or [].
func checkExpects(result []byte, expects []relSetExpect) error {
	d := json.NewDecoder(bytes.NewReader(result))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return fmt.Errorf("the result is not JSON: %v", err)
	}
	for _, x := range expects {
		got, err := jsonPointer(v, x.Pointer)
		if err != nil {
			return err
		}
		if x.Empty {
			switch g := got.(type) {
			case map[string]any:
				if len(g) == 0 {
					continue
				}
			case []any:
				if len(g) == 0 {
					continue
				}
			}
			b, _ := json.Marshal(got)
			return fmt.Errorf("%q is %s, not empty", x.Pointer, b)
		}
		gb, err := json.Marshal(got)
		if err != nil {
			return err
		}
		gc, err := canonicalJSON(gb)
		if err != nil {
			return err
		}
		wc, err := canonicalJSON(x.Value)
		if err != nil {
			return fmt.Errorf("the expected value of %q: %v", x.Pointer, err)
		}
		if !bytes.Equal(gc, wc) {
			return fmt.Errorf("%q is %s, not %s", x.Pointer, gc, wc)
		}
	}
	return nil
}

// jsonPointer resolves an RFC 6901 pointer in a decoded JSON value.
func jsonPointer(v any, p string) (any, error) {
	if p == "" {
		return v, nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("%q is not a JSON pointer", p)
	}
	for _, tok := range strings.Split(p[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			n, ok := x[tok]
			if !ok {
				return nil, fmt.Errorf("%q: no member %q", p, tok)
			}
			v = n
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(x) || (len(tok) > 1 && tok[0] == '0') {
				return nil, fmt.Errorf("%q: no element %q", p, tok)
			}
			v = x[i]
		default:
			return nil, fmt.Errorf("%q: %q is below a value that is not an object or an array", p, tok)
		}
	}
	return v, nil
}
