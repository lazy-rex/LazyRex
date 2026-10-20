package main

// Test mode (-test): the hub actions that the Lua test harness uses (docs/testing.md). The hub
// declares them only with -test, and refuses them at dispatch without it. -test needs a positive
// lab proof (checkLabProof); -real-server refuses it before any connection.
//
// No hub.test.* handler calls a plugin. Rex delivers the hub's requests one at a time, and a
// plugin's own calls to the hub wait in that queue (rex-protocol.md, "The hub has one request
// queue"). So each handler reads or writes hub state and answers at once.
//
// Every reply stays under testReplyLimit encoded bytes: a `rex do` script receives at most 64 KiB in
// one control message (rex-protocol.md, "Rex Lua runtime"). A value that does not fit comes back as
// too_large, and the test reads it in chunks (hub.test.chunk). Nothing is truncated.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
)

// testReplyLimit bounds the encoded result of a hub.test.* reply. The envelope (call_id, the
// client.reply frame) adds less than 1 KiB; the Lua limit is 65536 bytes for the whole message.
const testReplyLimit = 48 << 10

var testActions = []string{"hub.test.status", "hub.test.store", "hub.test.chunk", "hub.test.notice", "hub.test.stats", "hub.test.rev",
	"hub.test.tabs", "hub.test.views", "hub.test.operation", "hub.test.inject", "hub.test.inject.cancel", "hub.test.injection", "hub.test.view.render",
	"hub.test.view.result", "hub.test.parity", "hub.test.finish", "hub.test.commands", "hub.test.events", "hub.test.write", "hub.test.op.put",
	"hub.test.app", "hub.test.app.calls", "hub.test.app.close", "hub.test.fault", "hub.test.fault_state", "hub.test.fault_release", "hub.test.recover"}

type testState struct {
	mu          sync.Mutex
	recover     chan struct{} // closed by hub.test.recover (test_hold_recovery)
	recoverOnce sync.Once
	injections  []*injection
	batches     map[string]int                      // plugin -> hub.store.batch calls, including refused and conflicting ones
	invalidate  map[string]int                      // plugin -> hub.ui.invalidate calls
	renderReq   map[string]int64                    // view_id -> the latest requested render generation
	renderOut   map[string]map[int64]*renderOutcome // view_id -> requested generation -> its final outcome
	trees       map[string]*renderedTree            // plugin + "\x00" + route -> the descriptors of its last rendered tree
	apps        map[string]*testFakeApp             // fake Rex apps by name (testapp.go)
	faults      []*testFault                        // armed and fired faults (testfaults.go)
}

type injection struct {
	ID       int
	Plugin   string
	OpKey    string // match: the batch's op.key equals this
	OpPrefix string // match: the batch's op.key starts with this
	Writes   string // match: the batch writes or deletes this record key
	Updates  string // match: the batch writes this record key with an if_rev other than "0"
	// Reads: match a hub.store.get whose keys include this record key (test read hook). The hub
	// builds the get reply first, then commits Ops, then returns that reply: the next read sees
	// the change. It never matches a batch.
	Reads    string
	Ops      []any  // committed before the matching batch (no if_rev)
	Replay   bool   // commit a copy of the matching batch first
	Fail     string // then fail the matching batch with this message; it commits nothing
	FailCode string // the error code of that failure (default injected_failure)
	Times    int    // -1: always
	Skip     int    // the first Skip matching batches pass unchanged (Skipped counts them)
	Skipped  int
	Consumed int
	// A window hold (match {window = key}, hold = true) matches no batch. It holds one
	// hub.spawn_window call of the plugin with this key (and this caller, if set) after Rex
	// created the window and before the hub records it: the leaves stay sending, as after a
	// crash at that point. The held call waits until the injection is cancelled (then it goes on
	// as usual) or the hub process ends (hub:restart()). Rex delivers one request at a time to the
	// hub: while a call is held, the next request waits until the held call's caller times out. So
	// a test sees the hold through Rex (the new panes), and hub:restart() does not use the hub.
	Window  string
	Caller  string
	release chan struct{}
}

type renderOutcome struct {
	gen  int64
	ok   bool
	err  string
	rev  int64
	tree json.RawMessage
}

type renderedTree struct {
	plugin, route string
	descs         []actionDesc    // every distinct descriptor that a render of the route had
	seen          map[string]bool // descriptor identity -> in descs
}

func newTestState() *testState {
	return &testState{batches: map[string]int{}, invalidate: map[string]int{}, renderReq: map[string]int64{}, renderOut: map[string]map[int64]*renderOutcome{},
		trees: map[string]*renderedTree{}, apps: map[string]*testFakeApp{}, recover: make(chan struct{})}
}

// checkLabProof is the positive proof that -test points at a lab server that a harness (or
// lab/server.sh) owns: <dir>/rex.sock, <dir>/lab.json written by that owner, and a live server
// process whose argv names that socket and <dir>/state.
func checkLabProof(server string) error {
	if isRealServer(server) {
		return errors.New("-test refuses the real Rex server")
	}
	socket, err := rexSocket(server)
	if err != nil {
		return err
	}
	if filepath.Base(socket) != "rex.sock" {
		return fmt.Errorf("-test: the lab socket must be <lab dir>/rex.sock, not %s", socket)
	}
	dir := filepath.Dir(socket)
	path := filepath.Join(dir, "lab.json")
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("-test: no lab proof: %w (start the lab with lab/server.sh or lazyrex-hub test)", err)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("-test: %s does not belong to this user", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var proof struct {
		Socket    string `json:"socket"`
		ServerPID int    `json:"server_pid"`
		Nonce     string `json:"nonce"`
	}
	if err := json.Unmarshal(b, &proof); err != nil {
		return fmt.Errorf("-test: %s: %w", path, err)
	}
	if proof.Socket != "unix://"+socket {
		return fmt.Errorf("-test: %s names %s, not unix://%s", path, proof.Socket, socket)
	}
	if proof.ServerPID <= 0 || startTime(proof.ServerPID) == "" {
		return fmt.Errorf("-test: lab server pid %d is not running", proof.ServerPID)
	}
	if !argvHasPair(proof.ServerPID, "--listen", "unix://"+socket) || !argvHasPair(proof.ServerPID, "--state-dir", filepath.Join(dir, "state")) {
		return fmt.Errorf("-test: pid %d is not the lab server of %s", proof.ServerPID, dir)
	}
	return nil
}

func rexSocket(server string) (string, error) {
	if !strings.HasPrefix(server, "unix://") {
		return "", fmt.Errorf("-test needs a unix:// lab server, got %q", server)
	}
	return filepath.Clean(strings.TrimPrefix(server, "unix://")), nil
}

// argvHasPair reports whether the command line of pid has flag immediately followed by value.
func argvHasPair(pid int, flag, value string) bool {
	args := procArgs(pid)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// testDispatch handles hub.test.* in test mode.
func (h *Hub) testDispatch(ctx context.Context, p perform, args map[string]any) (any, error) {
	plugin, _ := args["plugin"].(string)
	needPlugin := func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.plugins[plugin] == nil {
			return bad("invalid: unknown plugin %q", plugin)
		}
		return nil
	}
	switch p.Action {
	case "hub.test.status":
		return h.testStatus(), nil
	case "hub.test.app":
		return h.testApp(ctx, args)
	case "hub.test.app.calls":
		return h.testAppCalls(args)
	case "hub.test.app.close":
		return h.testAppClose(args)
	case "hub.test.store":
		if err := needPlugin(); err != nil {
			return nil, err
		}
		return h.testStore(ctx, plugin, args)
	case "hub.test.chunk":
		return h.testChunk(ctx, args)
	case "hub.test.write":
		if err := needPlugin(); err != nil {
			return nil, err
		}
		return h.testWrite(ctx, plugin, args)
	case "hub.test.op.put":
		if err := needPlugin(); err != nil {
			return nil, err
		}
		return h.testOpPut(ctx, plugin, args)
	case "hub.test.notice":
		agent, _ := args["agent"].(string)
		var pending bool
		var intent int64
		err := h.db.QueryRowContext(ctx, `SELECT pending, intent_rev FROM notices WHERE ns = ? AND agent = ?`, plugin, agent).Scan(&pending, &intent)
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]any{"found": false}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"found": true, "pending": pending, "intent_rev": revString(intent)}, nil
	case "hub.test.stats":
		h.test.mu.Lock()
		defer h.test.mu.Unlock()
		return map[string]any{"batches": h.test.batches[plugin], "invalidates": h.test.invalidate[plugin]}, nil
	case "hub.test.rev":
		at, err := parseRevArg(args["at"])
		if err != nil {
			return nil, err
		}
		// Only forward: revisions never reset (store.go, nextRev).
		if _, err := h.db.ExecContext(ctx, `UPDATE meta SET v = ? WHERE k = 'rev' AND v < ?`, at, at); err != nil {
			return nil, err
		}
		var cur int64
		h.db.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = 'rev'`).Scan(&cur)
		return map[string]any{"rev": revString(cur)}, nil
	case "hub.test.tabs":
		rows, err := h.db.QueryContext(ctx, `SELECT tab, title, route, status, session_id, revision FROM ui_tabs WHERE plugin = ? ORDER BY pos`, plugin)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []any{}
		for rows.Next() {
			var key, title, route, status, sess, rev string
			if err := rows.Scan(&key, &title, &route, &status, &sess, &rev); err != nil {
				return nil, err
			}
			e := map[string]any{"key": key, "title": title, "route": route, "session_id": sess, "revision": rev}
			if status != "" {
				e["status"] = json.RawMessage(status)
			}
			list = append(list, e)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return boundReply(map[string]any{"tabs": list})
	case "hub.test.views":
		rows, err := h.db.QueryContext(ctx, `SELECT view_id, tab, route, attachment != '', context, session_id, window_id, block_id FROM views WHERE plugin = ? AND status = 'open' ORDER BY rowid`, plugin)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []any{}
		for rows.Next() {
			var id, tab, route, vctx, sess, win, block string
			var attached bool
			if err := rows.Scan(&id, &tab, &route, &attached, &vctx, &sess, &win, &block); err != nil {
				return nil, err
			}
			e := map[string]any{"view_id": id, "tab": tab, "route": route, "attached": attached, "session_id": sess, "window_id": win, "block_id": block}
			if vctx != "" {
				e["context"] = json.RawMessage(vctx)
			}
			list = append(list, e)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return boundReply(map[string]any{"views": list})
	case "hub.test.operation":
		// The durable record, independent of the renderer: a close_form effect can close the
		// renderer that admitted the operation.
		key, _ := args["operation_key"].(string)
		out, err := h.opState(ctx, plugin, key)
		if err != nil {
			return nil, err
		}
		return boundReply(out)
	case "hub.test.inject":
		if err := needPlugin(); err != nil {
			return nil, err
		}
		return h.testInject(args)
	case "hub.test.inject.cancel":
		// The injection matches no later batch. Its count is used up, so the file check passes.
		id, _ := args["id"].(float64)
		h.test.mu.Lock()
		defer h.test.mu.Unlock()
		for _, in := range h.test.injections {
			if float64(in.ID) == id { // exact: 1.5 is not injection 1
				in.Times = in.Consumed
				if in.release != nil {
					close(in.release) // a held window call goes on
					in.release = nil
				}
				return map[string]any{"consumed": in.Consumed}, nil
			}
		}
		return nil, bad("invalid: no injection %v", args["id"])
	case "hub.test.fault":
		if err := needPlugin(); err != nil {
			return nil, err
		}
		return h.testFaultArm(args, plugin)
	case "hub.test.recover":
		// Releases plugin recovery that the config holds (test_hold_recovery).
		h.test.recoverOnce.Do(func() { close(h.test.recover) })
		return map[string]any{"released": true}, nil
	case "hub.test.fault_release":
		id, _ := args["id"].(float64)
		return h.testFaultRelease(id)
	case "hub.test.fault_state":
		id, _ := args["id"].(float64)
		h.test.mu.Lock()
		defer h.test.mu.Unlock()
		for _, f := range h.test.faults {
			if float64(f.ID) == id { // exact: 1.5 is not fault 1
				return map[string]any{"fired": f.Fired, "seen": f.seen, "gated": f.Gated}, nil
			}
		}
		return nil, bad("invalid: no fault %v", args["id"])
	case "hub.test.injection":
		id, _ := args["id"].(float64)
		h.test.mu.Lock()
		defer h.test.mu.Unlock()
		for _, in := range h.test.injections {
			if float64(in.ID) == id { // exact: 1.5 is not injection 1
				return map[string]any{"consumed": in.Consumed, "remaining": in.remaining()}, nil
			}
		}
		return nil, bad("invalid: no injection %v", args["id"])
	case "hub.test.view.render":
		return h.testRender(ctx, args)
	case "hub.test.view.result":
		return h.testRenderResult(args)
	case "hub.test.events":
		// The hub's event log (events.jsonl) lines with this event name, oldest first, at most 50.
		name, _ := args["event"].(string)
		b, err := os.ReadFile(filepath.Join(h.cfg.State, "events.jsonl"))
		if err != nil {
			return nil, err
		}
		var list []any
		for _, line := range strings.Split(string(b), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil && m["event"] == name {
				list = append(list, m)
			}
		}
		if len(list) > 50 {
			list = list[len(list)-50:]
		}
		return boundReply(map[string]any{"events": orEmpty(list)})
	case "hub.test.commands":
		var raw string
		err := h.db.QueryRowContext(ctx, `SELECT commands FROM ui_commands WHERE plugin = ?`, plugin).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			raw = "[]"
		} else if err != nil {
			return nil, err
		}
		return boundReply(map[string]any{"commands": json.RawMessage(raw)})
	case "hub.test.parity":
		par, err := h.testParity(ctx)
		if err != nil {
			return nil, err
		}
		return boundReply(par)
	case "hub.test.finish":
		par, err := h.testParity(ctx)
		if err != nil {
			return nil, err
		}
		h.test.mu.Lock()
		var open []any
		for _, in := range h.test.injections {
			if in.Times > 0 && in.Consumed < in.Times {
				open = append(open, map[string]any{"id": in.ID, "plugin": in.Plugin, "consumed": in.Consumed, "times": in.Times})
			}
		}
		var unfired []any
		for _, f := range h.test.faults {
			if !f.Fired || ((f.Mode == "gate" || f.Mode == "hold") && !f.Released) {
				unfired = append(unfired, map[string]any{"id": f.ID, "point": f.Point, "mode": f.Mode, "action": f.Action, "when": f.When,
					"fired": f.Fired})
			}
		}
		h.test.mu.Unlock()
		return boundReply(map[string]any{"unconsumed": orEmpty(open), "unfired": orEmpty(unfired), "parity": par})
	}
	return nil, bad("unknown action %s", p.Action)
}

func orEmpty(l []any) []any {
	if l == nil {
		return []any{}
	}
	return l
}

func parseRevArg(v any) (int64, error) {
	switch t := v.(type) {
	case string:
		return parseRev(t)
	case float64:
		if t < 1 || t != float64(int64(t)) {
			return 0, bad("invalid: at must be a positive integer")
		}
		return int64(t), nil
	}
	return 0, bad("invalid: at must be a decimal string or an integer")
}

// boundReply refuses a reply that would not reach a Lua test client.
func boundReply(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > testReplyLimit {
		return nil, bad("too_large: the reply is %d bytes, the limit is %d", len(b), testReplyLimit)
	}
	return json.RawMessage(b), nil
}

func (h *Hub) testStatus() any {
	h.mu.Lock()
	defer h.mu.Unlock()
	plugins := map[string]any{}
	for name, pl := range h.plugins {
		face := []string{}
		for action, client := range h.agentForward {
			if pl.ClientID != "" && client == pl.ClientID {
				face = append(face, action)
			}
		}
		sort.Strings(face)
		plugins[name] = map[string]any{"registered": pl.ClientID != "", "client_id": pl.ClientID, "exports": orEmptyStrings(pl.Exports), "face": face,
			"state": pl.reportedState(), "error": pl.refusal, "instance_id": pl.InstanceID, "block_id": pl.BlockID, "child_pid": pl.ChildPID}
	}
	return map[string]any{"client_id": h.clientID, "plugins": plugins, "generation": revString(h.generation), "pid": os.Getpid()}
}

func orEmptyStrings(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// testStore reads records in key order until the encoded reply reaches the limit. A record that
// alone does not fit comes back in too_large with its size and revision.
func (h *Hub) testStore(ctx context.Context, plugin string, args map[string]any) (any, error) {
	keys, ok := asList(args["keys"])
	if !ok {
		return nil, bad("invalid: keys must be a list")
	}
	records := map[string]any{}
	tooLarge := map[string]any{}
	size := 64
	var rest []any
	for i, k := range keys {
		key, _ := k.(string)
		if key == "" {
			return nil, bad("invalid: keys must be nonempty strings")
		}
		value, rev, err := h.db.recordText(ctx, plugin, key)
		if err != nil {
			return nil, err
		}
		rec := map[string]any{"rev": revString(rev)}
		if rev != 0 {
			rec["value"] = json.RawMessage(value)
		}
		b, _ := json.Marshal(rec)
		n := len(b) + len(key)*6 + 8 // the key, escaped at worst, plus punctuation
		if n > testReplyLimit-256 {
			tooLarge[key] = map[string]any{"bytes": len(value), "rev": revString(rev)}
			continue
		}
		if size+n > testReplyLimit-256 {
			rest = keys[i:]
			break
		}
		size += n
		records[key] = rec
	}
	out := map[string]any{"records": records, "too_large": tooLarge}
	if len(rest) > 0 {
		out["rest"] = rest
	}
	return boundReply(out)
}

// recordText returns a record's stored JSON text and revision (0: absent).
func (db *DB) recordText(ctx context.Context, ns, key string) (string, int64, error) {
	var value string
	var rev int64
	err := db.QueryRowContext(ctx, `SELECT value, rev FROM records WHERE ns = ? AND key = ?`, ns, key).Scan(&value, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return value, rev, err
}

// testChunk returns a slice of a large JSON text: a record (source "record", with its revision) or
// a rendered tree (source "render", with the render generation). If the text changed since the
// first chunk, the read fails with changed: the test starts again. Slices end on UTF-8 boundaries,
// and each encoded reply stays under the limit, escaping included.
func (h *Hub) testChunk(ctx context.Context, args map[string]any) (any, error) {
	source, _ := args["source"].(string)
	offset, _ := args["offset"].(float64)
	var text string
	switch source {
	case "record":
		plugin, _ := args["plugin"].(string)
		key, _ := args["key"].(string)
		want, err := parseRevArg(args["rev"])
		if err != nil {
			return nil, err
		}
		value, rev, err := h.db.recordText(ctx, plugin, key)
		if err != nil {
			return nil, err
		}
		if rev != want {
			return nil, bad("changed: record %q is at revision %d, not %d; read it again", key, rev, want)
		}
		text = value
	case "render":
		view, _ := args["view_id"].(string)
		gen, _ := args["generation"].(float64)
		h.test.mu.Lock()
		out := h.test.renderOut[view][int64(gen)]
		h.test.mu.Unlock()
		if out == nil || !out.ok {
			return nil, bad("changed: view %q has no successful render of generation %v; render again", view, args["generation"])
		}
		text = string(out.tree)
	default:
		return nil, bad("invalid: source must be record or render")
	}
	start := int(offset)
	if start < 0 || start > len(text) || offset != float64(start) {
		return nil, bad("invalid: offset %v is outside the text (%d bytes)", args["offset"], len(text))
	}
	end := start + 32<<10
	for {
		if end >= len(text) {
			end = len(text)
		} else {
			for end > start && !utf8.RuneStart(text[end]) {
				end--
			}
		}
		b, _ := json.Marshal(text[start:end])
		if len(b) <= testReplyLimit-256 || end-start <= 4 {
			break
		}
		end = start + (end-start)/2
	}
	return map[string]any{"text": text[start:end], "next": end, "bytes": len(text), "done": end == len(text)}, nil
}

func (in *injection) remaining() any {
	if in.Times < 0 {
		return "always"
	}
	return in.Times - in.Consumed
}

func (in *injection) matches(plugin string, args map[string]any) bool {
	if in.Window != "" || in.Reads != "" || in.Plugin != plugin || in.Times >= 0 && in.Consumed >= in.Times {
		return false
	}
	opKey := ""
	if op, ok := args["op"].(map[string]any); ok {
		opKey, _ = op["key"].(string)
	}
	if in.OpKey != "" && opKey != in.OpKey {
		return false
	}
	if in.OpPrefix != "" && !strings.HasPrefix(opKey, in.OpPrefix) {
		return false
	}
	if in.Writes != "" {
		found := false
		ops, _ := asList(args["ops"])
		for _, o := range ops {
			if m, ok := o.(map[string]any); ok && m["key"] == in.Writes {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	if in.Updates != "" {
		found := false
		ops, _ := asList(args["ops"])
		for _, o := range ops {
			if m, ok := o.(map[string]any); ok && m["key"] == in.Updates {
				if rev, ok := m["if_rev"].(string); ok && rev != "0" {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (h *Hub) testInject(args map[string]any) (any, error) {
	plugin, _ := args["plugin"].(string)
	match, _ := args["match"].(map[string]any)
	in := &injection{Plugin: plugin, Times: 1}
	in.OpKey, _ = match["op_key"].(string)
	in.OpPrefix, _ = match["op_prefix"].(string)
	in.Writes, _ = match["writes"].(string)
	in.Updates, _ = match["updates"].(string)
	in.Reads, _ = match["reads"].(string)
	if _, ok := match["reads"]; ok && (in.Reads == "" || len(match) != 1) {
		return nil, bad("invalid: match.reads must be a nonempty record key, alone in match")
	}
	if _, ok := match["window"]; ok {
		return h.testWindowInject(in, match, args)
	}
	if in.OpKey == "" && in.OpPrefix == "" && in.Writes == "" && in.Updates == "" && in.Reads == "" {
		return nil, bad("invalid: match needs op_key, op_prefix, writes, updates, reads or window")
	}
	if _, ok := args["hold"]; ok {
		return nil, bad("invalid: hold needs match {window = key}")
	}
	if ops, ok := args["ops"]; ok {
		list, ok := asList(ops)
		if !ok {
			return nil, bad("invalid: ops must be a list")
		}
		in.Ops = list
	}
	if v, ok := args["skip"]; ok {
		n, ok := v.(float64)
		if !ok || n < 0 || n != float64(int(n)) {
			return nil, bad("invalid: skip must be a count")
		}
		in.Skip = int(n)
	}
	in.Replay, _ = args["replay"].(bool)
	in.Fail, _ = args["fail"].(string)
	if v, ok := args["fail_code"]; ok {
		code, _ := v.(string)
		if code == "" || in.Fail == "" {
			return nil, bad("invalid: fail_code must be nonempty text, and it needs fail")
		}
		in.FailCode = code
	}
	if len(in.Ops) == 0 && !in.Replay && in.Fail == "" {
		return nil, bad("invalid: an injection needs ops, replay or fail")
	}
	if in.Reads != "" && (len(in.Ops) == 0 || in.Replay || in.Fail != "") {
		return nil, bad("invalid: a reads injection needs ops, and has no replay or fail")
	}
	switch t := args["times"].(type) {
	case nil:
	case string:
		if t != "always" {
			return nil, bad("invalid: times must be a positive integer or \"always\"")
		}
		in.Times = -1
	case float64:
		if t < 1 || t != float64(int(t)) {
			return nil, bad("invalid: times must be a positive integer or \"always\"")
		}
		in.Times = int(t)
	default:
		return nil, bad("invalid: times must be a positive integer or \"always\"")
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	in.ID = len(h.test.injections) + 1
	h.test.injections = append(h.test.injections, in)
	return map[string]any{"id": in.ID}, nil
}

// testWindowInject arms a window hold: match {window = key, caller?} and hold = true, once.
func (h *Hub) testWindowInject(in *injection, match, args map[string]any) (any, error) {
	for k := range match {
		if k != "window" && k != "caller" {
			return nil, bad("invalid: a window hold matches only window and caller, not %s", k)
		}
	}
	in.Window, _ = match["window"].(string)
	if in.Window == "" {
		return nil, bad("invalid: match.window must be a spawn_window key")
	}
	if v, ok := match["caller"]; ok {
		if in.Caller, _ = v.(string); in.Caller == "" {
			return nil, bad("invalid: match.caller must be nonempty text")
		}
	}
	if hold, _ := args["hold"].(bool); !hold {
		return nil, bad("invalid: a window match needs hold = true")
	}
	for _, k := range []string{"ops", "replay", "fail", "fail_code", "skip", "times"} {
		if _, ok := args[k]; ok {
			return nil, bad("invalid: a window hold has no %s (it holds one call)", k)
		}
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	in.ID = len(h.test.injections) + 1
	h.test.injections = append(h.test.injections, in)
	return map[string]any{"id": in.ID}, nil
}

// testWindowHold takes the first window hold that matches this call, and returns the channel that
// releases the call (nil: no hold). spawnWindow calls it only after a valid success answer of Rex.
// The test mutex is not held while the call waits.
func (h *Hub) testWindowHold(p perform, caller, key string) <-chan struct{} {
	pl := h.pluginByClient(p.Requester.ClientID)
	if pl == nil {
		return nil
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	for _, in := range h.test.injections {
		if in.Window == key && in.Plugin == pl.Name && (in.Caller == "" || in.Caller == caller) && in.Consumed < in.Times {
			in.Consumed++
			in.release = make(chan struct{})
			h.ev.emit("test_injection", map[string]any{"id": in.ID, "plugin": pl.Name, "window": key})
			log.Printf("test: window hold %d holds spawn_window %q", in.ID, key)
			return in.release
		}
	}
	return nil
}

// testBeforeBatch counts a plugin's batch and commits the first matching injection before it.
// Injected commits call storeBatch directly, so they never match an injection themselves.
// testAfterGet runs a read hook after a plugin's hub.store.get built its reply. The caller keeps
// that reply and returns it after this commit, so the reply holds only the old state and the next
// request sees the new one: Rex delivers one hub request at a time. A get counts once for one
// injection, however many of its keys match. A commit that fails fails the get with
// injection_failed and is not counted.
func (h *Hub) testAfterGet(ctx context.Context, ns string, args map[string]any) error {
	keys, _ := asList(args["keys"])
	h.test.mu.Lock()
	var in *injection
	for _, c := range h.test.injections {
		if c.Reads == "" || c.Plugin != ns || c.Times >= 0 && c.Consumed >= c.Times {
			continue
		}
		found := false
		for _, k := range keys {
			found = found || k == c.Reads
		}
		if !found {
			continue
		}
		if c.Skipped < c.Skip {
			c.Skipped++
			break // this get passes; no other injection matches it
		}
		in = c
		break
	}
	h.test.mu.Unlock()
	if in == nil {
		return nil
	}
	b, _ := json.Marshal(in.Ops)
	var ops []any
	json.Unmarshal(b, &ops)
	out, err := h.db.storeBatch(ctx, ns, map[string]any{"ops": ops}, h.roster(), h.noticed(ns))
	if err == nil && !out.committed {
		err = fmt.Errorf("did not commit: %v", out.result)
	}
	if err != nil {
		return refuse("injection_failed", "injection %d: %v", in.ID, err)
	}
	h.test.mu.Lock()
	in.Consumed++
	h.test.mu.Unlock()
	h.afterCommit(ns, out)
	h.ev.emit("test_injection", map[string]any{"id": in.ID, "plugin": ns, "reads": in.Reads})
	return nil
}

func (h *Hub) testBeforeBatch(ctx context.Context, ns string, args map[string]any) error {
	h.test.mu.Lock()
	h.test.batches[ns]++
	var in *injection
	for _, c := range h.test.injections {
		if c.matches(ns, args) {
			if c.Skipped < c.Skip {
				c.Skipped++
				break // this batch passes; no other injection matches it
			}
			in = c
			c.Consumed++
			break
		}
	}
	h.test.mu.Unlock()
	if in == nil {
		return nil
	}
	if in.Replay {
		b, _ := json.Marshal(args)
		var dup map[string]any
		json.Unmarshal(b, &dup)
		out, err := h.db.storeBatch(ctx, ns, dup, h.roster(), h.noticed(ns))
		if err != nil {
			return fmt.Errorf("injection %d: replay: %w", in.ID, err)
		}
		h.afterCommit(ns, out)
	}
	if len(in.Ops) > 0 {
		b, _ := json.Marshal(in.Ops)
		var ops []any
		json.Unmarshal(b, &ops)
		out, err := h.db.storeBatch(ctx, ns, map[string]any{"ops": ops}, h.roster(), h.noticed(ns))
		if err != nil {
			return fmt.Errorf("injection %d: %w", in.ID, err)
		}
		if !out.committed {
			return fmt.Errorf("injection %d did not commit: %v", in.ID, out.result)
		}
		h.afterCommit(ns, out)
	}
	h.ev.emit("test_injection", map[string]any{"id": in.ID, "plugin": ns})
	if in.Fail != "" {
		code := in.FailCode
		if code == "" {
			code = "injected_failure"
		}
		return refuse(code, "%s", in.Fail)
	}
	return nil
}

// testWrite commits store ops in a plugin's namespace now, as another writer would: {key, value}
// or {key, delete = true}, without if_rev. It never matches an injection.
func (h *Hub) testWrite(ctx context.Context, plugin string, args map[string]any) (any, error) {
	list, ok := asList(args["ops"])
	if !ok || len(list) == 0 {
		return nil, bad("invalid: ops must be a nonempty list")
	}
	out, err := h.db.storeBatch(ctx, plugin, map[string]any{"ops": list}, h.roster(), h.noticed(plugin))
	if err != nil {
		return nil, err
	}
	if !out.committed {
		// A stale if_rev: a test that read a record and writes it back must not overwrite a newer
		// plugin commit, and it needs a code that it can tell from a malformed write.
		if c, _ := out.result.(map[string]any)["conflict"].(map[string]any); c["kind"] == "revision" {
			return nil, refuse("conflict", "the write did not commit: record %v is at rev %v", c["key"], c["rev"])
		}
		return nil, bad("invalid: the write did not commit: %v", out.result)
	}
	h.afterCommit(plugin, out)
	return out.result, nil
}

// testOpPut saves an operation record {key, fingerprint, result}, as an older plugin version
// saved it. It replaces a record with the same key.
func (h *Hub) testOpPut(ctx context.Context, plugin string, args map[string]any) (any, error) {
	key, _ := args["key"].(string)
	fp, _ := args["fingerprint"].(string)
	if key == "" || fp == "" {
		return nil, bad("invalid: key and fingerprint are required")
	}
	result, err := json.Marshal(args["result"])
	if err != nil || args["result"] == nil {
		return nil, bad("invalid: result is required")
	}
	if _, err := h.db.ExecContext(ctx, `INSERT OR REPLACE INTO ops VALUES (?, ?, ?, ?)`, plugin, key, fp, string(result)); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// testRender asks for a new render of one view and returns its generation. The production render
// path does the work (renderViews); the hub only records the outcome of that work.
func (h *Hub) testRender(ctx context.Context, args map[string]any) (any, error) {
	view, _ := args["view_id"].(string)
	var plugin string
	var attached bool
	err := h.db.QueryRowContext(ctx, `SELECT plugin, attachment != '' FROM views WHERE view_id = ? AND status = 'open'`, view).Scan(&plugin, &attached)
	if err != nil {
		return nil, bad("stale_view: view %q is not open", view)
	}
	if !attached {
		return nil, bad("not_ready: view %q has no renderer attached", view)
	}
	h.test.mu.Lock()
	h.test.renderReq[view]++
	gen := h.test.renderReq[view]
	h.test.mu.Unlock()
	h.kickViews(plugin, true)
	return map[string]any{"generation": gen}, nil
}

// testRenderStart returns the requested generation that a render starting now covers.
func (h *Hub) testRenderStart(view string) int64 {
	if h.test == nil {
		return 0
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	return h.test.renderReq[view]
}

// testRenderDone records a finished render that started when gen was the newest requested
// generation. It completes every requested generation up to gen that has no outcome yet. A
// completed generation keeps its outcome: a later render cannot change it.
func (h *Hub) testRenderDone(view string, gen int64, ok bool, errText string, rev int64, tree json.RawMessage) {
	if h.test == nil || gen < 1 {
		return
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	outs := h.test.renderOut[view]
	if outs == nil {
		outs = map[int64]*renderOutcome{}
		h.test.renderOut[view] = outs
	}
	out := &renderOutcome{gen: gen, ok: ok, err: errText, rev: rev, tree: tree}
	for g := gen; g >= 1 && g > gen-renderKeep; g-- {
		if outs[g] != nil {
			break // older generations completed before this one
		}
		outs[g] = out
	}
	for g := range outs {
		if g <= gen-renderKeep {
			delete(outs, g) // bounded memory; an older generation then reads as invalid
		}
	}
}

// renderKeep is the number of recent generations per view whose outcomes the hub keeps.
const renderKeep = 32

// testRenderResult answers for one requested generation: pending until a render that started
// after the request finished, then that render's outcome, unchanged by later renders.
func (h *Hub) testRenderResult(args map[string]any) (any, error) {
	view, _ := args["view_id"].(string)
	g, _ := args["generation"].(float64)
	gen := int64(g)
	h.test.mu.Lock()
	out := h.test.renderOut[view][gen]
	req := h.test.renderReq[view]
	h.test.mu.Unlock()
	if gen < 1 || gen > req || gen <= req-renderKeep {
		return nil, bad("invalid: view %q has no requested generation %v in the last %d", view, args["generation"], renderKeep)
	}
	if out == nil {
		return map[string]any{"status": "pending"}, nil
	}
	res := map[string]any{"generation": gen, "render_generation": out.gen}
	if !out.ok {
		res["status"], res["error"] = "failed", out.err
		return res, nil
	}
	res["status"], res["revision"], res["bytes"] = "ok", revString(out.rev), len(out.tree)
	if len(out.tree) <= testReplyLimit-1024 {
		res["tree"] = out.tree
	}
	return res, nil
}

// testRecordTree adds the action descriptors of a rendered tree for the parity check. Descriptors
// accumulate per route: a later render without an action does not hide its earlier violation.
func (h *Hub) testRecordTree(plugin, route string, tree map[string]any) {
	if h.test == nil {
		return
	}
	descs := treeActions(tree)
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	k := plugin + "\x00" + route
	t := h.test.trees[k]
	if t == nil {
		t = &renderedTree{plugin: plugin, route: route, seen: map[string]bool{}}
		h.test.trees[k] = t
	}
	for _, d := range descs {
		id := fmt.Sprintf("%s\x00%s\x00%s\x00%v%s\x00%v%s", d.Node, d.Field, d.Name, d.toolSet, d.Tool, d.userSet, d.UserOnly)
		if !t.seen[id] {
			t.seen[id] = true
			t.descs = append(t.descs, d)
		}
	}
}

// testParity checks every command and every rendered tree against the plugins' current exports
// and agent face. It also lists the required routes (declared tabs and command routes) that no
// render reached: those are unchecked, not passed.
func (h *Hub) testParity(ctx context.Context) (map[string]any, error) {
	h.mu.Lock()
	exports, face := map[string]map[string]bool{}, map[string]map[string]bool{}
	for name, pl := range h.plugins {
		exports[name], face[name] = map[string]bool{}, map[string]bool{}
		for _, e := range pl.Exports {
			exports[name][e] = true
		}
		for action, client := range h.agentForward {
			if pl.ClientID != "" && client == pl.ClientID {
				face[name][action] = true
			}
		}
	}
	names := make([]string, 0, len(h.plugins))
	for name := range h.plugins {
		names = append(names, name)
	}
	h.mu.Unlock()
	sort.Strings(names)
	hubFace, err := h.agentHubFace(ctx)
	if err != nil {
		return nil, err
	}
	var violations []any
	required := map[string]map[string]bool{} // plugin -> route -> rendered
	for _, name := range names {
		required[name] = map[string]bool{}
		var raw string
		err := h.db.QueryRowContext(ctx, `SELECT commands FROM ui_commands WHERE plugin = ?`, name).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var cmds []paletteCommand
		json.Unmarshal([]byte(raw), &cmds)
		for _, c := range cmds {
			required[name][c.Route] = false
			if msg := parityProblem(actionDesc{Name: "command:" + c.ID, Tool: c.Action, UserOnly: c.UserOnly, toolSet: c.Action != "", userSet: c.UserOnly != ""},
				exports[name], face[name], hubFace); msg != "" {
				violations = append(violations, map[string]any{"plugin": name, "command": c.ID, "problem": msg})
			}
		}
		tabs, err := h.loadTabs(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, t := range tabs {
			required[name][t.Route] = false
		}
	}
	h.test.mu.Lock()
	keys := make([]string, 0, len(h.test.trees))
	for k := range h.test.trees {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	checked := []any{}
	for _, k := range keys {
		t := h.test.trees[k]
		if required[t.plugin] != nil {
			if _, ok := required[t.plugin][t.route]; ok {
				required[t.plugin][t.route] = true
			}
		}
		bad := 0
		for _, d := range t.descs {
			if msg := parityProblem(d, exports[t.plugin], face[t.plugin], hubFace); msg != "" {
				bad++
				violations = append(violations, map[string]any{"plugin": t.plugin, "route": t.route, "node": d.Node, "action": d.Name, "problem": msg})
			}
		}
		checked = append(checked, map[string]any{"plugin": t.plugin, "route": t.route, "actions": len(t.descs), "violations": bad})
	}
	h.test.mu.Unlock()
	unrendered := []any{}
	for _, name := range names {
		var routes []string
		for r, done := range required[name] {
			if !done {
				routes = append(routes, r)
			}
		}
		sort.Strings(routes)
		for _, r := range routes {
			unrendered = append(unrendered, map[string]any{"plugin": name, "route": r})
		}
	}
	return map[string]any{"violations": orEmpty(violations), "checked": checked, "unrendered": unrendered}, nil
}

// --- The parity checker (shared by test mode and the production log) ---

// actionDesc is one user action in a view tree or a palette command.
type actionDesc struct {
	Node, Name, Tool, UserOnly string
	Field                      string // the tree field that holds it: action, submit_action, load_action, ...
	toolSet, userSet           bool   // the field is present (an empty string is a violation, not an absence)
}

// parityExemptNames are renderer mechanics with no domain effect: Back, a form's Cancel, and
// history paging (coordinator, 2026-10-19). The value is the descriptor field that the exemption
// needs ("" = any field): "load" is exempt only as a history node's load_action. This is the only
// exempt list.
var parityExemptNames = map[string]string{"$back": "", "$cancel": "", "load": "load_action"}

func parityExempt(d actionDesc) bool {
	field, ok := parityExemptNames[d.Name]
	return ok && (field == "" || field == d.Field)
}

// agentHubFace returns the hub actions (agentHubActions) that the agent face lists now, read back
// from Rex with client.inspect.
func (h *Hub) agentHubFace(ctx context.Context) (map[string]bool, error) {
	var w struct {
		Client struct {
			ID string `json:"id"`
		} `json:"client"`
	}
	if h.agentRex == nil || json.Unmarshal(h.agentRex.Welcome, &w) != nil || w.Client.ID == "" {
		return nil, fmt.Errorf("the agent face is not connected")
	}
	var out struct {
		Actions []map[string]any `json:"actions"`
	}
	if err := h.rex.Call(ctx, "client.inspect", map[string]any{"client_id": w.Client.ID}, &out); err != nil {
		return nil, err
	}
	face := map[string]bool{}
	for _, a := range out.Actions {
		if name, _ := a["name"].(string); agentHubActions[name] {
			face[name] = true
		}
	}
	return face, nil
}

// parityProblem returns "" when the action names an exported agent action that the agent face
// lists, or gives a user_only reason. The tool can also be a hub action (agentHubActions, for
// example hub.spawn) that the agent face lists (hubFace). This proves export and discovery, not
// that the view action and the agent action run the same handler: behavior tests on both paths
// check that.
func parityProblem(d actionDesc, exports, face, hubFace map[string]bool) string {
	switch {
	case parityExempt(d):
		return ""
	case d.toolSet && d.userSet:
		return "names both tool and user_only"
	case d.userSet && strings.TrimSpace(d.UserOnly) == "":
		return "user_only is empty: give the reason"
	case d.userSet:
		return ""
	case !d.toolSet:
		return "names no tool (an exported agent action) and no user_only reason"
	case agentHubActions[d.Tool] && !hubFace[d.Tool]:
		return fmt.Sprintf("hub tool %q is not on the agent face", d.Tool)
	case agentHubActions[d.Tool]:
		return ""
	case !exports[d.Tool]:
		return fmt.Sprintf("tool %q is not an export of the plugin", d.Tool)
	case !face[d.Tool]:
		return fmt.Sprintf("tool %q is not on the agent face (the plugin must declare hub.forward)", d.Tool)
	}
	return ""
}

// actionFields are the tree fields that hold an action descriptor.
var actionFields = map[string]bool{"action": true, "submit_action": true, "save_action": true, "send_action": true, "load_action": true,
	"retry_action": true, "back": true}

// treeActions returns every action descriptor in a tree, nested nodes and list items included.
func treeActions(tree map[string]any) []actionDesc {
	var out []actionDesc
	var walk func(v any, node string)
	walk = func(v any, node string) {
		switch t := v.(type) {
		case map[string]any:
			if id, ok := t["id"].(string); ok && id != "" {
				if node == "" {
					node = id
				} else {
					node = node + "/" + id
				}
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				child := t[k]
				if m, ok := child.(map[string]any); ok && actionFields[k] {
					if name, ok := m["name"].(string); ok {
						d := actionDesc{Node: node, Name: name, Field: k}
						if k == "back" && node == "" {
							d.Node = "$root"
						}
						d.Tool, d.toolSet = m["tool"].(string)
						if _, present := m["tool"]; present && !d.toolSet {
							d.toolSet = true // present but not a string: reported as not an export
						}
						d.UserOnly, d.userSet = m["user_only"].(string)
						if _, present := m["user_only"]; present && !d.userSet {
							d.userSet = true // present but not a string: reported as empty
						}
						out = append(out, d)
						continue
					}
				}
				walk(child, node)
			}
		case []any:
			for _, c := range t {
				walk(c, node)
			}
		}
	}
	walk(tree, "")
	return out
}

// parityLogged dedupes the production log of descriptors that name neither field.
var parityLogged sync.Map

func (h *Hub) parityLog(plugin, route string, tree map[string]any) {
	for _, d := range treeActions(tree) {
		if parityExempt(d) || d.toolSet || d.userSet {
			continue
		}
		key := plugin + "\x00" + route + "\x00" + d.Node + "\x00" + d.Name
		if _, seen := parityLogged.LoadOrStore(key, true); !seen {
			log.Printf("parity: %s %s node %s action %s names no tool and no user_only", plugin, route, d.Node, d.Name)
		}
	}
}

// procArgs returns the command line of pid, split on white space ("" fields dropped).
func procArgs(pid int) []string {
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}
