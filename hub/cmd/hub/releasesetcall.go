package main

// The calls of a release-set rehearsal: agent action calls from a lab pane (seed and probe items of
// kind "action"), and the expect checks of a result.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	relSeedAgent = "relseed"   // the lab-only roster agent that runs the action items
	relPeerAgent = "relpeer"   // a second lab-only roster agent that never joins (<PEER>)
	relLabInert  = "lab_inert" // the lab-only accessory profile (command true)
	relLabAgent  = "lab_agent" // the lab-only agent profile (command true)

	relSeedSession = "<SEED_SESSION_ID>" // the session of the plugin's seed action batch
	relSeedBlock   = "<SEED_BLOCK_ID>"   // the block of the plugin's seed action batch
)

// relCallOut is the result of one call: the rex exit code and its output.
type relCallOut struct {
	Code int
	Out  []byte
	Vals map[string]string // the placeholder values of the batch
}

// fillExpects returns expects with the lab placeholders replaced in each expected value. The set
// bytes stay as the owner reviewed them.
func fillExpects(expects []relSetExpect, vals map[string]string) ([]relSetExpect, error) {
	out := make([]relSetExpect, len(expects))
	for i, x := range expects {
		out[i] = x
		if len(x.Value) > 0 {
			v, err := fillPlaceholders(x.Value, vals)
			if err != nil {
				return nil, fmt.Errorf("the expected value of %q: %v", x.Pointer, err)
			}
			out[i].Value = v
		}
	}
	return out, nil
}

// checkFilled applies the expects to a result after it fills the lab placeholders in them.
func checkFilled(result []byte, expects []relSetExpect, vals map[string]string) error {
	filled, err := fillExpects(expects, vals)
	if err != nil {
		return err
	}
	return checkExpects(result, filled)
}

// fillPlaceholders replaces the lab placeholders in each string (value or member name) of a JSON
// text, and encodes it without HTML escapes. It works on the decoded strings, so an escaped form
// of a placeholder in the text (<SELF>) is replaced too.
func fillPlaceholders(raw []byte, vals map[string]string) ([]byte, error) {
	if err := noDupNames(raw); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	fill := func(s string) string {
		for k, x := range vals {
			s = strings.ReplaceAll(s, k, x)
		}
		return s
	}
	var walk func(any) (any, error)
	walk = func(v any) (any, error) {
		switch x := v.(type) {
		case string:
			return fill(x), nil
		case []any:
			for i := range x {
				w, err := walk(x[i])
				if err != nil {
					return nil, err
				}
				x[i] = w
			}
		case map[string]any:
			// Two member names can become one name after the fill: refuse, or one value is lost.
			m := make(map[string]any, len(x))
			for k, e := range x {
				fk := fill(k)
				if _, dup := m[fk]; dup {
					return nil, fmt.Errorf("two member names become %q after the placeholders are filled", fk)
				}
				w, err := walk(e)
				if err != nil {
					return nil, err
				}
				m[fk] = w
			}
			return m, nil
		}
		return v, nil
	}
	filled, err := walk(v)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(filled); err != nil {
		return nil, err
	}
	out := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	// A seed placeholder without a value: the plugin has no recorded seed action batch.
	for _, p := range []string{relSeedSession, relSeedBlock} {
		if _, ok := vals[p]; !ok && bytes.Contains(out, []byte(p)) {
			return nil, fmt.Errorf("%s has no value: no seed action batch of this plugin recorded it", p)
		}
	}
	return out, nil
}

// agentBatch runs calls as the lab agent relSeedAgent from the one pane of a new lab session: the
// pane joins, runs each call with the rex CLI through the agent face, and ends. The tool writes the
// args files only after it knows the pane's session and block (<SESSION_ID>, <BLOCK_ID>), and the
// pane waits for them. The seed batch of plugin records them as <SEED_SESSION_ID> and
// <SEED_BLOCK_ID> before it fills its args. After the batch the tool closes the pane, waits until
// the hub has no live binding of the agent, and destroys the session. The next batch rebinds the
// agent to its own pane before the join. A batch with no plugin (a chunking probe) has the name
// phase.
func (h *rehearsal) agentBatch(plugin, phase string, calls []relSetItem) ([]relCallOut, error) {
	name := phase
	if plugin != "" {
		name = plugin + "-" + phase
	}
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
	dir := filepath.Join(h.lab, "calls", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// hub.join goes to the hub client; the items go to the agent face (agentLabel), which carries
	// the plugin actions that agents may call.
	script := filepath.Join(dir, "batch.sh")
	if err := os.WriteFile(script, []byte(batchScript(dir, h.l.server, client, calls)), 0o700); err != nil {
		return nil, err
	}
	session, block, err := h.batchSession(name, dir, script)
	if err != nil {
		if session != "" {
			h.l.r.Call(context.Background(), "session.destroy", map[string]any{"session_id": session}, nil)
		}
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
	if plugin != "" && phase == "seed" {
		h.set.recordSeed(plugin, session, block)
	}
	vals := h.placeholders(plugin, session, block)
	for i, c := range calls {
		args := c.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		filled, err := fillPlaceholders(args, vals)
		if err != nil {
			return nil, relFail("the agent pane %s: the args of item %d: %v", name, i+1, err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("args-%d.json", i)), filled, 0o600); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o600); err != nil {
		return nil, err
	}
	untils := 0
	for _, c := range calls {
		if len(c.Until) > 0 {
			untils++
		}
	}
	deadline := time.Now().Add(time.Duration(30+60*len(calls))*time.Second + time.Duration(untils)*relUntilWindow)
	outs, err := serveBatch(dir, calls, vals, h.say, deadline)
	if err != nil {
		return nil, relFail("the agent pane %s: %v", name, err)
	}
	if err := h.closePane(session, block, relSeedAgent); err != nil {
		return nil, err
	}
	if err := h.l.r.Call(context.Background(), "session.destroy", map[string]any{"session_id": session}, nil); err != nil {
		return nil, relFail("the agent pane %s: session.destroy %s: %v", name, session, err)
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

// placeholders are the values of the lab placeholders of the set file for an item of plugin in the
// pane session and block. The seed placeholders have a value only after the plugin's seed action
// batch recorded them.
func (h *rehearsal) placeholders(plugin, session, block string) map[string]string {
	m := map[string]string{"<LAB>": h.lab, "<SELF>": relSeedAgent, "<PEER>": relPeerAgent, "<SESSION_ID>": session, "<BLOCK_ID>": block}
	if v, ok := h.set.seeds[plugin]; ok && plugin != "" {
		m[relSeedSession], m[relSeedBlock] = v[0], v[1]
	}
	return m
}

// recordSeed records the pane of plugin's seed action batch (<SEED_SESSION_ID>, <SEED_BLOCK_ID>).
// The probes replay keyed seed actions, which must name the seed's session and block again, after
// the tool destroyed that session.
func (s *relSetState) recordSeed(plugin, session, block string) {
	if s.seeds == nil {
		s.seeds = map[string][2]string{}
	}
	s.seeds[plugin] = [2]string{session, block}
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

// batchSession makes the lab session of one agent batch: one window with one pane, which runs the
// batch script in dir. A plugin that reads the caller's session (Templates saves each pane of it)
// sees only this pane. batchSession returns after the pane reported its directory (OSC 7), which
// is in the lab.
func (h *rehearsal) batchSession(name, dir, script string) (string, string, error) {
	ctx := context.Background()
	var s struct {
		ID string `json:"session_id"`
	}
	if err := h.l.r.Call(ctx, "session.create", map[string]any{"label": relAgentLabel + "-" + name, "initial_windows": []any{map[string]any{"window_label": name,
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell",
			"options": map[string]any{"command": []string{"/bin/sh", script}, "cwd": dir, "login": false, "exit": map[string]bool{"on_completion": false}}}}}}}, &s); err != nil {
		return "", "", fmt.Errorf("session.create: %v", err)
	}
	var out struct {
		Blocks []struct {
			BlockID string `json:"block_id"`
		} `json:"blocks"`
	}
	if s.ID == "" {
		return "", "", fmt.Errorf("session.create returned no session_id")
	}
	// Rex refuses list_blocks and block calls on a session that the connection has not attached
	// ("stream is not attached", lab), as in sessionBlocksIn.
	if err := h.l.r.Call(ctx, "session.attach", map[string]any{"session_id": s.ID, "event_scope": "session"}, nil); err != nil {
		return s.ID, "", fmt.Errorf("session.attach %s: %v", s.ID, err)
	}
	if err := h.l.r.Call(ctx, "session.list_blocks", map[string]any{"session_id": s.ID}, &out); err != nil {
		return s.ID, "", fmt.Errorf("session.list_blocks %s: %v", s.ID, err)
	}
	var ids []string
	for _, b := range out.Blocks {
		ids = append(ids, b.BlockID)
	}
	block, err := batchPaneIDs(s.ID, ids)
	if err != nil {
		return s.ID, "", err
	}
	var last string
	for i := 0; i < 75; i++ {
		var pwd struct {
			Pwd string `json:"pwd"`
		}
		err := h.l.r.BlockCall(ctx, "com.superlogical.terminal.pwd", s.ID, block, nil, &pwd)
		if err == nil {
			if err = batchPaneAt(pwd.Pwd, dir); err == nil {
				return s.ID, block, nil
			}
		}
		last = err.Error()
		time.Sleep(200 * time.Millisecond)
	}
	return s.ID, block, fmt.Errorf("the batch pane %s did not report the batch directory in 15 s: %s", block, last)
}

// batchPaneIDs checks the replies of session.create and session.list_blocks for one batch: a
// session id and exactly one block, with an id. It returns the block id.
func batchPaneIDs(session string, blocks []string) (string, error) {
	if session == "" {
		return "", fmt.Errorf("session.create returned no session_id")
	}
	if len(blocks) != 1 {
		return "", fmt.Errorf("the batch session %s has the blocks %q, not one", session, blocks)
	}
	if blocks[0] == "" {
		return "", fmt.Errorf("the batch session %s has a block with no block_id", session)
	}
	return blocks[0], nil
}

// batchPaneAt checks that raw, the terminal.pwd value of a batch pane (the OSC 7 file URL), names
// dir: a local file URL whose path is the same directory as dir after symlinks resolve.
func batchPaneAt(raw, dir string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("terminal.pwd %q: %v", raw, err)
	}
	if u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || !filepath.IsAbs(u.Path) {
		return fmt.Errorf("terminal.pwd %q is not a local file URL", raw)
	}
	got, err := filepath.EvalSymlinks(u.Path)
	if err != nil {
		return fmt.Errorf("terminal.pwd %q: %v", raw, err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("terminal.pwd %q is %s, not the batch directory %s", raw, got, want)
	}
	return nil
}
