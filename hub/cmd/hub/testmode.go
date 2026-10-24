package main

// Test mode (-test): the hub actions that the Lua test harness uses (docs/testing.md). The hub
// declares them only with -test, and refuses them at dispatch without it. -test needs a positive
// lab proof (checkLabProof); -real-server refuses it before any connection.
//
// No hub.test.* handler calls a plugin. Rex delivers the hub's requests one at a time, and a
// plugin's own calls to the hub wait in that queue (rex-protocol.md, "The hub has one request
// queue"). So each handler reads or writes hub state and answers at once.
//
// Every reply stays under testReplyLimit encoded bytes, the harness's own bound. A value that does
// not fit comes back as too_large, and the test reads it in chunks (hub.test.chunk). Nothing is
// truncated.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/lazy-rex/LazyRex/hub/internal/rexreal"
)

// testReplyLimit bounds the encoded result of a hub.test.* reply. It is the harness's bound, not a
// Rex limit: a `rex do` script sends at most 65536 bytes in one control message but receives more
// (rex-protocol.md, "Rex Lua runtime").
const testReplyLimit = 48 << 10

type testState struct {
	mu           sync.Mutex
	recover      chan struct{} // closed by hub.test.recover (test_hold_recovery)
	recoverOnce  sync.Once
	injections   []*injection
	invalidate   map[string]int                      // plugin -> hub.ui.invalidate calls
	renderReq    map[string]int64                    // view_id -> the latest requested render generation
	renderOut    map[string]map[int64]*renderOutcome // view_id -> requested generation -> its final outcome
	trees        map[string]*renderedTree            // plugin + "\x00" + route -> the descriptors of its last rendered tree
	apps         map[string]*testFakeApp             // fake Rex apps by name (testapp.go)
	faults       []*testFault                        // armed and fired faults (testfaults.go)
	calls        map[string][]testCall               // plugin -> its last hub service calls (testcalls.go)
	callsDropped map[string]int64                    // plugin -> the highest seq of a call that the bound dropped
	callSeq      int64                               // the seq of the last recorded call, for any plugin
	renders      map[string]*renderCount             // view_id -> its render counts (renderdeps.go)
	deliveries   []*testDelivery                     // hub.test.peer.deliver, by id - 1
}

// testDelivery is one hub.test.peer.deliver: its outcome, when done.
type testDelivery struct {
	done    bool
	reply   json.RawMessage
	skipped bool
	err     string
}

type injection struct {
	ID       int
	Plugin   string
	Fail     string // fail the matching commit with this message
	FailCode string // the error code of that failure (default injected_failure)
	Times    int    // -1: always
	Skip     int    // the first Skip matching commits pass unchanged (Skipped counts them)
	Skipped  int
	Consumed int
	// A window hold (match {window = key}, hold = true) matches no commit. It holds one
	// hub.spawn_window call of the plugin with this key (and this caller, if set) after Rex
	// created the window and before the hub records it: the leaves stay sending, as after a
	// crash at that point. The held call waits until the injection is cancelled (then it goes on
	// as usual) or the hub process ends (hub:restart()). Rex delivers one request at a time to the
	// hub: while a call is held, the next request waits until the held call's caller times out. So
	// a test sees the hold through Rex (the new panes), and hub:restart() does not use the hub.
	Window  string
	Caller  string
	release chan struct{}

	// SQLCommitWrites: match a hub.sql commit (a handler commit, an exec or a batch alone) that
	// changed this table (lower case). Only Fail applies; with After the commit happens first and
	// the caller gets the error, as after a lost reply.
	SQLCommitWrites string
	After           bool
	// SQLParam: with SQLCommitWrites, match only a commit whose transaction ran a statement with
	// a parameter equal to this (sqlParamsHave).
	SQLParam string
	// SQLBegin: match the plugin's next hub.sql.begin. The hub first commits SQL (statements
	// {sql, params}) as another writer, then begins the plugin transaction.
	SQLBegin bool
	SQL      []map[string]any
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
	return &testState{invalidate: map[string]int{}, renderReq: map[string]int64{}, renderOut: map[string]map[int64]*renderOutcome{},
		trees: map[string]*renderedTree{}, apps: map[string]*testFakeApp{}, recover: make(chan struct{}),
		calls: map[string][]testCall{}, callsDropped: map[string]int64{}, renders: map[string]*renderCount{}}
}

// checkLabProof is the positive proof that -test points at a lab server that a harness (or
// lab/server.sh) owns: <dir>/rex.sock, <dir>/lab.json written by that owner, and a live server
// process whose argv names that socket and <dir>/state.
func checkLabProof(server string) error {
	if rexreal.KnownDefault(server) {
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
	case "hub.test.calls":
		return h.testCalls(args)
	case "hub.test.renders":
		if err := needPlugin(); err != nil {
			return nil, err
		}
		return h.testRenders(plugin), nil
	case "hub.test.app.close":
		return h.testAppClose(args)
	case "hub.test.sql":
		if err := needPlugin(); err != nil {
			return nil, err
		}
		return h.sql.test(ctx, plugin, args)
	case "hub.test.chunk":
		return h.testChunk(args)
	case "hub.test.peer.deliver":
		// One envelope to the plugin through peerReceive, the path of a pulled envelope. It runs in a
		// goroutine, and the request returns without waiting for it, as the pull loop runs outside
		// any request: Rex gives the hub one request at a time, so a plugin's hub call during
		// peer.receive would wait behind this request.
		// hub.test.peer.delivery returns the outcome.
		if err := needPlugin(); err != nil {
			return nil, err
		}
		from, _ := args["from_hub"].(string)
		key, _ := args["key"].(string)
		body, ok := args["body"].(string)
		fromPlugin, _ := args["from_plugin"].(string)
		if args["from_plugin"] == nil {
			fromPlugin = plugin
		}
		if !hubName.MatchString(from) || key == "" || !ok || !pluginName.MatchString(fromPlugin) {
			return nil, bad("invalid: peer.deliver needs from_hub (a hub name), key, a string body and from_plugin (a plugin name) if given")
		}
		d := &testDelivery{}
		h.test.mu.Lock()
		h.test.deliveries = append(h.test.deliveries, d)
		id := len(h.test.deliveries)
		h.test.mu.Unlock()
		go func() {
			res, err := h.peerReceive(context.Background(), from, fromPlugin, plugin, key, body)
			h.test.mu.Lock()
			defer h.test.mu.Unlock()
			d.done, d.reply, d.skipped = true, res, err == nil && res == nil
			if err != nil {
				d.err = err.Error()
			}
		}()
		return map[string]any{"id": id}, nil
	case "hub.test.peer.delivery":
		id, _ := intArg(args["id"])
		h.test.mu.Lock()
		defer h.test.mu.Unlock()
		if id < 1 || int(id) > len(h.test.deliveries) {
			return nil, bad("invalid: unknown delivery %v", args["id"])
		}
		d := h.test.deliveries[id-1]
		out := map[string]any{"done": d.done}
		switch {
		case !d.done:
		case d.err != "":
			out["error"] = map[string]any{"code": "not_processed", "message": d.err}
		case d.skipped:
			out["skipped"] = true
		default:
			out["reply"] = d.reply
		}
		return out, nil
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
	case "hub.test.notice_renudge":
		// The time to the one repeat of an unread nudge, for this hub run (default 30 minutes).
		ms, ok := args["ms"].(float64)
		if !ok || ms < 1 || ms != math.Trunc(ms) {
			return nil, bad("invalid: notice_renudge needs a whole ms of at least 1")
		}
		h.renudgeAfter.Store(int64(time.Duration(ms) * time.Millisecond))
		h.kickNotices()
		return map[string]any{"ms": ms}, nil
	case "hub.test.stats":
		h.test.mu.Lock()
		defer h.test.mu.Unlock()
		return map[string]any{"invalidates": h.test.invalidate[plugin]}, nil
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
		return h.testFaultState(id)
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
			"state": pl.reportedState(), "error": pl.refusal, "instance_id": pl.InstanceID, "block_id": pl.BlockID, "child_pid": pl.ChildPID, "waiting": orEmptyStrings(pl.waiting)}
	}
	return map[string]any{"client_id": h.clientID, "plugins": plugins, "generation": revString(h.generation), "pid": os.Getpid()}
}

func orEmptyStrings(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// testChunk returns a slice of a large JSON text: a rendered tree (source "render", with the render
// generation). If the text changed since the first chunk, the read fails with changed: the test
// starts again. Slices end on UTF-8 boundaries,
// and each encoded reply stays under the limit, escaping included.
func (h *Hub) testChunk(args map[string]any) (any, error) {
	if source, _ := args["source"].(string); source != "render" {
		return nil, bad("invalid: source must be render")
	}
	offset, _ := args["offset"].(float64)
	view, _ := args["view_id"].(string)
	gen, _ := args["generation"].(float64)
	h.test.mu.Lock()
	out := h.test.renderOut[view][int64(gen)]
	h.test.mu.Unlock()
	if out == nil || !out.ok {
		return nil, bad("changed: view %q has no successful render of generation %v; render again", view, args["generation"])
	}
	text := string(out.tree)
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

func (h *Hub) testInject(args map[string]any) (any, error) {
	plugin, _ := args["plugin"].(string)
	match, _ := args["match"].(map[string]any)
	in := &injection{Plugin: plugin, Times: 1}
	in.SQLCommitWrites, _ = match["sql_commit_writes"].(string)
	keys := 1 // sql_commit_writes, and sql_param if set
	if v, ok := match["sql_param"]; ok {
		text, isText := sqlParamText(v)
		if !isText || text == "" || in.SQLCommitWrites == "" {
			return nil, bad("invalid: match.sql_param must be a nonempty string or an integer, with sql_commit_writes")
		}
		in.SQLParam, keys = text, 2
	}
	if _, ok := match["sql_commit_writes"]; ok && (in.SQLCommitWrites == "" || len(match) != keys) {
		return nil, bad("invalid: match.sql_commit_writes must be a nonempty table name, alone in match (or with sql_param)")
	}
	if v, ok := match["sql_begin"]; ok {
		if v != true || len(match) != 1 {
			return nil, bad("invalid: match.sql_begin must be true, alone in match")
		}
		return h.testSQLBeginInject(in, args)
	}
	in.SQLCommitWrites = strings.ToLower(in.SQLCommitWrites)
	in.After, _ = args["after"].(bool)
	if in.After && in.SQLCommitWrites == "" {
		return nil, bad("invalid: after needs match {sql_commit_writes = table}")
	}
	if _, ok := match["window"]; ok {
		return h.testWindowInject(in, match, args)
	}
	if in.SQLCommitWrites == "" {
		return nil, bad("invalid: match needs sql_commit_writes, sql_begin or window")
	}
	if _, ok := args["hold"]; ok {
		return nil, bad("invalid: hold needs match {window = key}")
	}
	if v, ok := args["skip"]; ok {
		n, ok := v.(float64)
		if !ok || n < 0 || n != float64(int(n)) {
			return nil, bad("invalid: skip must be a count")
		}
		in.Skip = int(n)
	}
	in.Fail, _ = args["fail"].(string)
	if v, ok := args["fail_code"]; ok {
		code, _ := v.(string)
		if code == "" || in.Fail == "" {
			return nil, bad("invalid: fail_code must be nonempty text, and it needs fail")
		}
		in.FailCode = code
	}
	for _, k := range []string{"ops", "replay"} {
		if _, ok := args[k]; ok {
			return nil, bad("invalid: a sql_commit_writes injection has no %s", k)
		}
	}
	if in.Fail == "" {
		return nil, bad("invalid: a sql_commit_writes injection needs fail")
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
	h.markView(plugin, view, causeTest)
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

// procArgs returns the command line of pid (procArgv: ps words on macOS, the exact arguments on
// Linux), or nil.
func procArgs(pid int) []string {
	argv, err := procArgv(pid)
	if err != nil {
		return nil
	}
	return argv
}

// testBeforeSQLCommit is the hub.sql commit fault point: the first live injection with
// sql_commit_writes for a changed table fails the commit (or, with after, its reply).
func (h *Hub) testBeforeSQLCommit(plugin string, marks []string, params []any) (error, bool) {
	h.test.mu.Lock()
	var in *injection
	for _, c := range h.test.injections {
		if c.SQLCommitWrites == "" || c.Plugin != plugin || c.Times >= 0 && c.Consumed >= c.Times {
			continue
		}
		if !slices.Contains(marks, c.SQLCommitWrites) || c.SQLParam != "" && !sqlParamsHave(params, c.SQLParam) {
			continue
		}
		if c.Skipped < c.Skip {
			c.Skipped++
			break
		}
		in = c
		c.Consumed++
		break
	}
	h.test.mu.Unlock()
	if in == nil {
		return nil, false
	}
	h.ev.emit("test_injection", map[string]any{"id": in.ID, "plugin": plugin, "after": in.After})
	code := in.FailCode
	if code == "" {
		code = "injected_failure"
	}
	return refuse(code, "%s", in.Fail), in.After
}

// testSQLBeginInject arms an injection at the plugin's next hub.sql.begin: the entries of sql
// ({sql = ..., params = ...?}) commit first, as another writer. skip and times apply.
func (h *Hub) testSQLBeginInject(in *injection, args map[string]any) (any, error) {
	for k := range args {
		if k != "plugin" && k != "match" && k != "sql" && k != "skip" && k != "times" {
			return nil, bad("invalid: field %s does not apply to a sql_begin injection", k)
		}
	}
	list, _ := args["sql"].([]any)
	if len(list) == 0 {
		return nil, bad("invalid: a sql_begin injection needs sql = {{sql = ..., params = ...?}, ...}")
	}
	for i, x := range list {
		m, ok := x.(map[string]any)
		if q, _ := m["sql"].(string); !ok || q == "" {
			return nil, bad("invalid: sql[%d] must be {sql, params?}", i+1)
		}
		in.SQL = append(in.SQL, m)
	}
	in.SQLBegin = true
	if v, ok := args["skip"]; ok {
		n, ok := v.(float64)
		if !ok || n < 0 || n != float64(int(n)) {
			return nil, bad("invalid: skip must be a count")
		}
		in.Skip = int(n)
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

// testBeforeSQLBegin is the hub.sql.begin test point: the statements of the first live sql_begin
// injection of the plugin.
func (h *Hub) testBeforeSQLBegin(plugin string) []map[string]any {
	h.test.mu.Lock()
	defer h.test.mu.Unlock()
	for _, c := range h.test.injections {
		if !c.SQLBegin || c.Plugin != plugin || c.Times >= 0 && c.Consumed >= c.Times {
			continue
		}
		if c.Skipped < c.Skip {
			c.Skipped++
			return nil
		}
		c.Consumed++
		h.ev.emit("test_injection", map[string]any{"id": c.ID, "plugin": plugin, "sql_begin": true})
		return c.SQL
	}
	return nil
}

// sqlParamText is the text of a parameter value for a match: a string, an integer, or an int tag.
func sqlParamText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10), true
		}
	case map[string]any:
		if s, ok := t["int"].(string); ok && len(t) == 1 {
			return s, true
		}
	}
	return "", false
}

// sqlParamsHave reports a parameter equal to want in any of the params values (an array or an
// object of parameters, as in hub.sql args).
func sqlParamsHave(params []any, want string) bool {
	for _, p := range params {
		var values []any
		switch t := p.(type) {
		case []any:
			values = t
		case map[string]any:
			for _, v := range t {
				values = append(values, v)
			}
		}
		for _, v := range values {
			if text, ok := sqlParamText(v); ok && text == want {
				return true
			}
		}
	}
	return false
}
