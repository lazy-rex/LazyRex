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
	"hub.test.tabs", "hub.test.views", "hub.test.operation", "hub.test.inject", "hub.test.injection", "hub.test.view.render",
	"hub.test.view.result", "hub.test.parity", "hub.test.finish", "hub.test.commands", "hub.test.events"}

type testState struct {
	mu         sync.Mutex
	injections []*injection
	batches    map[string]int            // plugin -> hub.store.batch calls, including refused and conflicting ones
	renderReq  map[string]int64          // view_id -> the latest requested render generation
	renderOut  map[string]*renderOutcome // view_id -> the outcome of the latest finished render
	trees      map[string]*renderedTree  // plugin + "\x00" + route -> the descriptors of its last rendered tree
}

type injection struct {
	ID       int
	Plugin   string
	OpKey    string // match: the batch's op.key equals this
	OpPrefix string // match: the batch's op.key starts with this
	Writes   string // match: the batch writes or deletes this record key
	Ops      []any  // committed before the matching batch (no if_rev)
	Replay   bool   // commit a copy of the matching batch first
	Times    int    // -1: always
	Consumed int
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
	descs         []actionDesc
}

func newTestState() *testState {
	return &testState{batches: map[string]int{}, renderReq: map[string]int64{}, renderOut: map[string]*renderOutcome{},
		trees: map[string]*renderedTree{}}
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
	case "hub.test.store":
		if err := needPlugin(); err != nil {
			return nil, err
		}
		return h.testStore(ctx, plugin, args)
	case "hub.test.chunk":
		return h.testChunk(ctx, args)
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
		return map[string]any{"batches": h.test.batches[plugin]}, nil
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
		rows, err := h.db.QueryContext(ctx, `SELECT tab, title, route, status, session_id FROM ui_tabs WHERE plugin = ? ORDER BY pos`, plugin)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []any{}
		for rows.Next() {
			var key, title, route, status, sess string
			if err := rows.Scan(&key, &title, &route, &status, &sess); err != nil {
				return nil, err
			}
			e := map[string]any{"key": key, "title": title, "route": route, "session_id": sess}
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
		rows, err := h.db.QueryContext(ctx, `SELECT view_id, tab, route, attachment != '', context FROM views WHERE plugin = ? AND status = 'open' ORDER BY rowid`, plugin)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []any{}
		for rows.Next() {
			var id, tab, route, vctx string
			var attached bool
			if err := rows.Scan(&id, &tab, &route, &attached, &vctx); err != nil {
				return nil, err
			}
			e := map[string]any{"view_id": id, "tab": tab, "route": route, "attached": attached}
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
	case "hub.test.injection":
		id, _ := args["id"].(float64)
		h.test.mu.Lock()
		defer h.test.mu.Unlock()
		for _, in := range h.test.injections {
			if in.ID == int(id) {
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
		h.test.mu.Unlock()
		return boundReply(map[string]any{"unconsumed": orEmpty(open), "parity": par})
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
		plugins[name] = map[string]any{"registered": pl.ClientID != "", "client_id": pl.ClientID, "exports": orEmptyStrings(pl.Exports), "face": face}
	}
	return map[string]any{"client_id": h.clientID, "plugins": plugins, "generation": revString(h.generation)}
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
		out := h.test.renderOut[view]
		h.test.mu.Unlock()
		if out == nil || out.gen != int64(gen) || !out.ok {
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
	if in.Plugin != plugin || in.Times >= 0 && in.Consumed >= in.Times {
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
	return true
}

func (h *Hub) testInject(args map[string]any) (any, error) {
	plugin, _ := args["plugin"].(string)
	match, _ := args["match"].(map[string]any)
	in := &injection{Plugin: plugin, Times: 1}
	in.OpKey, _ = match["op_key"].(string)
	in.OpPrefix, _ = match["op_prefix"].(string)
	in.Writes, _ = match["writes"].(string)
	if in.OpKey == "" && in.OpPrefix == "" && in.Writes == "" {
		return nil, bad("invalid: match needs op_key, op_prefix or writes")
	}
	if ops, ok := args["ops"]; ok {
		list, ok := asList(ops)
		if !ok {
			return nil, bad("invalid: ops must be a list")
		}
		in.Ops = list
	}
	in.Replay, _ = args["replay"].(bool)
	if len(in.Ops) == 0 && !in.Replay {
		return nil, bad("invalid: an injection needs ops or replay")
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

// testBeforeBatch counts a plugin's batch and commits the first matching injection before it.
// Injected commits call storeBatch directly, so they never match an injection themselves.
func (h *Hub) testBeforeBatch(ctx context.Context, ns string, args map[string]any) error {
	h.test.mu.Lock()
	h.test.batches[ns]++
	var in *injection
	for _, c := range h.test.injections {
		if c.matches(ns, args) {
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
		out, err := h.db.storeBatch(ctx, ns, dup, h.roster())
		if err != nil {
			return fmt.Errorf("injection %d: replay: %w", in.ID, err)
		}
		h.afterCommit(ns, out)
	}
	if len(in.Ops) > 0 {
		b, _ := json.Marshal(in.Ops)
		var ops []any
		json.Unmarshal(b, &ops)
		out, err := h.db.storeBatch(ctx, ns, map[string]any{"ops": ops}, h.roster())
		if err != nil {
			return fmt.Errorf("injection %d: %w", in.ID, err)
		}
		if !out.committed {
			return fmt.Errorf("injection %d did not commit: %v", in.ID, out.result)
		}
		h.afterCommit(ns, out)
	}
	h.ev.emit("test_injection", map[string]any{"id": in.ID, "plugin": ns})
	return nil
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

func (h *Hub) testRenderDone(view string, gen int64, ok bool, errText string, rev int64, tree json.RawMessage) {
	if h.test == nil {
		return
	}
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	if cur := h.test.renderOut[view]; cur != nil && cur.gen > gen {
		return
	}
	h.test.renderOut[view] = &renderOutcome{gen: gen, ok: ok, err: errText, rev: rev, tree: tree}
}

// testRenderResult answers for one requested generation: pending until a render that started after
// the request finished. A later render answers for an earlier request too.
func (h *Hub) testRenderResult(args map[string]any) (any, error) {
	view, _ := args["view_id"].(string)
	g, _ := args["generation"].(float64)
	gen := int64(g)
	h.test.mu.Lock()
	out := h.test.renderOut[view]
	req := h.test.renderReq[view]
	h.test.mu.Unlock()
	if gen < 1 || gen > req {
		return nil, bad("invalid: view %q has no requested generation %v", view, args["generation"])
	}
	if out == nil || out.gen < gen {
		return map[string]any{"status": "pending"}, nil
	}
	res := map[string]any{"generation": out.gen}
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

// testRecordTree keeps the action descriptors of a rendered tree for the parity check.
func (h *Hub) testRecordTree(plugin, route string, tree map[string]any) {
	if h.test == nil {
		return
	}
	descs := treeActions(tree)
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	h.test.trees[plugin+"\x00"+route] = &renderedTree{plugin: plugin, route: route, descs: descs}
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
				exports[name], face[name]); msg != "" {
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
			if msg := parityProblem(d, exports[t.plugin], face[t.plugin]); msg != "" {
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
	toolSet, userSet           bool // the field is present (an empty string is a violation, not an absence)
}

// parityExemptNames are renderer mechanics with no domain effect: Back, a form's Cancel, and
// history paging (coordinator, 2026-10-19). This is the only exempt list.
var parityExemptNames = map[string]bool{"$back": true, "$cancel": true, "load": true}

func parityExempt(d actionDesc) bool { return parityExemptNames[d.Name] }

// parityProblem returns "" when the action names an exported agent action that the agent face
// lists, or gives a user_only reason. This proves export and discovery, not that the view action
// and the agent action run the same handler: behavior tests on both paths check that.
func parityProblem(d actionDesc, exports, face map[string]bool) string {
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
						d := actionDesc{Node: node, Name: name}
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
