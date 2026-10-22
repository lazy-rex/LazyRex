package main

// The plugin manager's operations (plugin manager plan v5, step 2: the op core). An operation
// ("op") is a durable request to start, stop, change the settings of, or remove one plugin entry.
// Agents create ops with hub.plugins.op (the agent face only); the home plugin's panel creates
// them with the view effect "plugins" (forms.go), from a user's view operation. One manager
// goroutine runs the ops in order, one phase at a time. Each phase is stored before its effect,
// and each lifecycle call has an attempt row before the call: after a crash the manager observes
// the result of an open attempt and never makes the call again.
//
// Authorization (user decisions 2026-10-19 and 2026-10-21): an agent may start, stop, remove and
// change settings without a user click (option C), but it never starts new GitHub code. Every
// start passes expect_run (expectrun.go). An agent's start of a GitHub entry is admitted only when
// the candidate is the record's last run; else the user must confirm (user_confirmation_required).
// A user's start names the run that the panel showed, and the start runs only that run.
//
// The managed file (section 13): the manager changes only the entry of its op. At admission it
// stores the entry as the loader parsed it (base). At the write, under both plugin command flocks,
// the current entry must still be base, or the op ends in conflict. Before the rename it stores
// the hash of the bytes that it writes (output): after a crash, that hash proves the write, and an
// entry that already has the intended value is the write too. It never writes old bytes again.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"time"
)

const pluginOpsSchema = `
-- phase: queued, then the phases of the kind (pluginOpPhases), then done, failed, conflict or unknown.
-- base: the entry at admission (canonical JSON, or "absent"); output: the hash of the bytes that the
-- write renames, stored before the rename.
-- entry_identity: entryIdentity of the entry at admission; the start and stop calls send it as expect_entry.
CREATE TABLE IF NOT EXISTS plugin_ops (op_id TEXT PRIMARY KEY, scope TEXT NOT NULL, key TEXT NOT NULL,
  fingerprint TEXT NOT NULL, origin TEXT NOT NULL, kind TEXT NOT NULL, entry TEXT NOT NULL, name TEXT NOT NULL,
  expect TEXT NOT NULL, input TEXT NOT NULL, target TEXT NOT NULL, phase TEXT NOT NULL, rev INTEGER NOT NULL DEFAULT 1,
  confirmed TEXT NOT NULL DEFAULT '', base TEXT NOT NULL DEFAULT '', output TEXT NOT NULL DEFAULT '', entry_identity TEXT NOT NULL DEFAULT '',
  result TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL, updated INTEGER NOT NULL,
  UNIQUE (scope, key));
-- One row for each lifecycle call of an op, stored before the call. outcome '': the call may have
-- run; the manager observes its result and never calls again.
CREATE TABLE IF NOT EXISTS plugin_op_attempts (attempt TEXT PRIMARY KEY, op_id TEXT NOT NULL, call TEXT NOT NULL,
  expect_instance TEXT NOT NULL DEFAULT '', expect_run TEXT NOT NULL DEFAULT '', at INTEGER NOT NULL,
  outcome TEXT NOT NULL DEFAULT '');
`

// pluginOpPhases are the working phases of each kind, in order.
var pluginOpPhases = map[string][]string{
	"start":    {"starting"},
	"stop":     {"stopping"},
	"settings": {"writing", "reloading"},
	// checking: the graph check before the stop, so a refused remove stops nothing.
	"remove": {"checking", "stopping", "writing", "reloading"},
}

// opFinal are the phases that end an op.
var opFinal = map[string]bool{"done": true, "failed": true, "conflict": true, "unknown": true}

// opBusyRetry is the wait before the next try when another plugin command holds the flocks;
// opBusyNote is the wait after which the op reports that command.
const (
	opBusyRetry = 2 * time.Second
	opBusyNote  = 10 * time.Minute
)

type pluginOpsSvc struct {
	kick chan struct{}
	busy map[string]time.Time // op ID -> the first busy try (the manager goroutine only)
	// fault is a test hook: true at a point stops the manager there, as a crash would.
	fault func(point string) bool
}

// errOpCrash ends a manager run at a test fault point.
var errOpCrash = errors.New("plugin op: test fault point")

// opWait is a phase that must run again later.
type opWait struct {
	after time.Duration
	why   string
}

func (w *opWait) Error() string { return w.why }

type opOrigin struct {
	Kind string `json:"kind"` // "user" or "agent"
	ID   string `json:"id"`   // the view ID of the user's operation, or the agent ID
}

// opRequest is the request of an op: the same fields from an agent and from the panel.
type opRequest struct {
	Op     string          `json:"op"`
	Entry  string          `json:"entry"`  // "managed:<id>", or the exact source of a config entry
	Expect json.RawMessage `json:"expect"` // start from the panel: the run that the panel showed
	Input  json.RawMessage `json:"input"`  // settings: {args, watch_roots}
}

type opSettings struct {
	Args       map[string]string `json:"args"`
	WatchRoots []string          `json:"watch_roots"`
}

// pluginOp is one plugin_ops row.
type pluginOp struct {
	ID, Kind, Entry, Name, Phase string
	Origin                       opOrigin
	Input                        string
	Target                       string // start: the expect_run JSON
	Base, Output                 string
	EntryIdentity                string // entryIdentity of the entry at admission ("" in no admitted op)
	Result, Error                string
	Created, Updated             int64
}

func (h *Hub) pluginOpsStart(ctx context.Context) error {
	if _, err := h.db.ExecContext(ctx, pluginOpsSchema); err != nil {
		return err
	}
	h.pops.kick = make(chan struct{}, 1)
	go h.opsLoop(ctx)
	h.kickOps() // ops that a stop or a crash interrupted
	return nil
}

func (h *Hub) kickOps() {
	select {
	case h.pops.kick <- struct{}{}:
	default:
	}
}

// opEntryKey is the identity of an entry in an op: "managed:<id>" or the exact source.
func opEntryKey(e *configEntry) string {
	if e.managed != "" {
		return "managed:" + e.managed
	}
	return e.Source
}

// canonicalEntry is the managed entry as the loader parsed it, in canonical JSON: a reformat of the
// file by hand gives the same text, and every field of managedFileEntry is in it.
func canonicalEntry(e *configEntry) string {
	if e == nil {
		return "absent"
	}
	b, _ := json.Marshal(fileEntry(e))
	return string(b)
}

// fileEntry is the file form of a parsed managed entry. Empty args and roots are written as {} and
// [], so an absent field and an empty one are the same entry.
func fileEntry(e *configEntry) managedFileEntry {
	m := managedFileEntry{ID: e.managed, Source: e.Source, Args: e.Args, WatchRoots: e.WatchRoots}
	if m.Args == nil {
		m.Args = map[string]string{}
	}
	if m.WatchRoots == nil {
		m.WatchRoots = []string{}
	}
	return m
}

// readManaged reads and parses the managed file. A missing file has no entries and no bytes.
func readManaged(path string) ([]byte, []*configEntry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	list, err := parseManagedBytes(b)
	return b, list, err
}

func managedByID(list []*configEntry, id string) *configEntry {
	for _, e := range list {
		if e.managed == id {
			return e
		}
	}
	return nil
}

func hashBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// homeRecordLocked reports whether name is the record of the configured home entry, with an entry
// (not kept) and a registered instance. h.mu must be held. The "plugins" effect and
// hub.plugins.ops use it.
func (h *Hub) homeRecordLocked(name string) bool {
	home := h.homePlugin()
	pl := h.plugins[home]
	return name == home && pl != nil && pl.entry != nil && pl.ClientID != ""
}

// pluginOpAgent answers hub.plugins.op and hub.plugins.ops on the agent face. The caller must be a
// joined agent or a current conversation (agentOfPID): a plugin process has none, so it is refused.
func (h *Hub) pluginOpAgent(ctx context.Context, p perform) (any, error) {
	var args map[string]json.RawMessage
	if len(p.Args) > 0 && string(p.Args) != "null" {
		if err := json.Unmarshal(p.Args, &args); err != nil {
			return nil, bad("args must be an object")
		}
	}
	agent, err := h.agentOfPID(ctx, p.Requester.Principal.PID)
	if err != nil {
		return nil, err
	}
	if p.Action == "hub.plugins.ops" {
		return h.pluginOpsList(ctx, args)
	}
	key, req, err := agentOpRequest(args)
	if err != nil {
		return nil, err
	}
	return h.admitOp(ctx, opOrigin{"agent", agent}, "agent:"+agent, key, req)
}

// agentOpRequest reads the key and the request of an agent's hub.plugins.op. An agent never gives
// expect: only the user's panel names the run that it showed.
func agentOpRequest(args map[string]json.RawMessage) (string, opRequest, error) {
	var key string
	if json.Unmarshal(args["key"], &key) != nil || key == "" || len(key) > 128 {
		return "", opRequest{}, bad("invalid: key must be a string of 1 to 128 characters")
	}
	if _, ok := args["expect"]; ok {
		return "", opRequest{}, bad("invalid: expect is set only by the user's panel")
	}
	rest := map[string]json.RawMessage{}
	for k, v := range args {
		if k != "key" {
			rest[k] = v
		}
	}
	b, _ := json.Marshal(rest)
	req, err := decodeOpRequest(b)
	return key, req, err
}

func decodeOpRequest(b []byte) (opRequest, error) {
	var req opRequest
	if err := decodeManifest("the operation", b, &req); err != nil {
		return req, bad("invalid: %v", err)
	}
	return req, nil
}

// pluginOpEffect admits the op of a "plugins" view effect. Only a user's view operation of the
// home plugin's current registered instance has one: the view_ops row is the user's attested
// action (the renderer admitted it, viewAdmit), and the op is bound to that row by its key.
// retry: the error is transient, and the effect runs again later.
func (h *Hub) pluginOpEffect(ctx context.Context, plugin, key, viewID string, raw json.RawMessage) (retry bool, err error) {
	h.mu.Lock()
	home := h.homeRecordLocked(plugin)
	h.mu.Unlock()
	if !home {
		return false, errors.New("plugins: only the home plugin manages plugins")
	}
	var kind string
	err = h.db.QueryRowContext(ctx, `SELECT kind FROM view_ops WHERE plugin = ? AND user = ? AND op_key = ?`, plugin, viewUser, key).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && kind != "action") {
		return false, errors.New("plugins: the effect has no user view action")
	}
	if err != nil {
		return true, err
	}
	req, err := decodeOpRequest(raw)
	if err != nil {
		return false, fmt.Errorf("plugins: %w", err)
	}
	_, err = h.admitOp(ctx, opOrigin{"user", viewID}, "user", "view:"+plugin+":"+key, req)
	var se *storeError
	if err != nil && !errors.As(err, &se) {
		return true, err
	}
	return false, err
}

// opFingerprint is the sha256 of the canonical request.
func opFingerprint(req opRequest) string {
	canon := func(r json.RawMessage) any {
		var v any
		if len(r) > 0 {
			json.Unmarshal(r, &v)
		}
		return v
	}
	b, _ := json.Marshal(map[string]any{"op": req.Op, "entry": req.Entry, "expect": canon(req.Expect), "input": canon(req.Input)})
	return hashBytes(b)
}

// admitOp stores a new op, or returns the op of the same scope and key. The same key with another
// request is operation_conflict. Admission checks the request, the entry and the authorization; it
// never waits for a flock or a lifecycle call (the manager goroutine runs the op).
func (h *Hub) admitOp(ctx context.Context, origin opOrigin, scope, key string, req opRequest) (any, error) {
	fp := opFingerprint(req)
	find := func() (string, error) {
		var id, old string
		err := h.db.QueryRowContext(ctx, `SELECT op_id, fingerprint FROM plugin_ops WHERE scope = ? AND key = ?`, scope, key).Scan(&id, &old)
		if err != nil {
			return "", err
		}
		if old != fp {
			return "", refuse("operation_conflict", "key %q has a different operation", key)
		}
		return id, nil
	}
	id, err := find()
	if err == nil {
		return h.opView(ctx, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	op, err := h.opAdmission(origin, req)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	op.ID = hex.EncodeToString(b)
	ob, _ := json.Marshal(origin)
	now := time.Now().UnixMilli()
	// OR IGNORE: a second admission of the same key in a race finds the first one.
	if _, err := h.db.ExecContext(ctx, `INSERT OR IGNORE INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, base, entry_identity, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?)`,
		op.ID, scope, key, fp, string(ob), op.Kind, op.Entry, op.Name, string(req.Expect), op.Input, op.Target, op.Base, op.EntryIdentity, now, now); err != nil {
		return nil, err
	}
	if id, err = find(); err != nil {
		return nil, err
	}
	h.ev.emit("plugin_op", map[string]any{"op_id": id, "kind": op.Kind, "entry": op.Entry, "origin": origin, "phase": "queued"})
	h.kickOps()
	h.changed("plugins")
	return h.opView(ctx, id)
}

// opAdmission checks a request and builds its op (without ID).
func (h *Hub) opAdmission(origin opOrigin, req opRequest) (*pluginOp, error) {
	op := &pluginOp{Kind: req.Op, Entry: req.Entry, Origin: origin}
	if _, ok := pluginOpPhases[req.Op]; !ok {
		return nil, bad("invalid: op must be start, stop, settings or remove")
	}
	if len(req.Expect) > 0 && req.Op != "start" {
		return nil, bad("invalid: only start has expect")
	}
	if len(req.Input) > 0 && req.Op != "settings" {
		return nil, bad("invalid: only settings has input")
	}
	h.entriesMu.Lock()
	var e *configEntry
	for _, x := range h.entries {
		if opEntryKey(x) == req.Entry {
			e = x
			break
		}
	}
	var name, source string
	var github bool
	var cand *pluginRun
	if e != nil {
		name, source, github = e.name, e.Source, e.github
		if e.cand != nil {
			c := *e.cand
			cand = &c
		}
	}
	h.entriesMu.Unlock()
	if e == nil {
		return nil, bad("no plugin entry %q (give managed:<id> or the exact source of a config entry)", req.Entry)
	}
	op.Name = name
	op.EntryIdentity = entryIdentity(e)
	if (req.Op == "settings" || req.Op == "remove") && e.managed == "" {
		return nil, bad("%s changes only managed entries; %s is in the config file", req.Op, req.Entry)
	}
	if (req.Op == "stop" || req.Op == "remove") && name != "" && name == h.homePlugin() {
		return nil, refuse("refused", "%s is the home plugin entry; it cannot be stopped or removed here", req.Entry)
	}
	switch req.Op {
	case "start":
		x, err := h.startTarget(origin, req, name, source, github, cand)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(map[string]string{"source": x.Source, "commit": x.Commit, "digest": x.Digest})
		op.Target = string(b)
	case "settings":
		if len(req.Input) == 0 {
			return nil, bad("invalid: settings needs input {args, watch_roots}")
		}
		var in opSettings
		if err := decodeManifest("input", req.Input, &in); err != nil {
			return nil, bad("invalid: %v", err)
		}
		if err := (&configEntry{Args: in.Args, WatchRoots: in.WatchRoots}).checkFields(); err != nil {
			return nil, bad("invalid: %v", err)
		}
		b, _ := json.Marshal(in)
		op.Input = string(b)
	}
	if req.Op == "settings" || req.Op == "remove" {
		_, list, err := readManaged(h.cfg.managedPath)
		if err != nil {
			return nil, refuse("refused", "managed plugin file %s: %v", h.cfg.managedPath, err)
		}
		m := managedByID(list, e.managed)
		if m == nil {
			return nil, refuse("refused", "%s is not in the managed file now; reload first", req.Entry)
		}
		op.Base = canonicalEntry(m)
	}
	return op, nil
}

// startTarget is the run that a start op may start. An agent: a local entry's source, or a GitHub
// entry's last run when the candidate is that run. The user: the run that the panel showed.
func (h *Hub) startTarget(origin opOrigin, req opRequest, name, source string, github bool, cand *pluginRun) (*expectRun, error) {
	if origin.Kind == "user" {
		if len(req.Expect) == 0 {
			return nil, bad("invalid: a start from the panel needs expect, the run that the panel showed")
		}
		var m map[string]any
		if err := json.Unmarshal(req.Expect, &m); err != nil {
			return nil, bad("invalid: expect must be an object")
		}
		x, err := parseExpectRun(map[string]any{"expect_run": m})
		if err != nil {
			return nil, err
		}
		if x.Source != source {
			return nil, bad("invalid: expect names source %s, not %s", x.Source, source)
		}
		return x, nil
	}
	if !github {
		return &expectRun{Source: source}, nil
	}
	var last pluginRun
	h.mu.Lock()
	if pl := h.plugins[name]; name != "" && pl != nil {
		last = pl.run
	}
	h.mu.Unlock()
	x := &expectRun{Source: last.Source, Commit: last.Commit, Digest: last.Digest}
	switch {
	case last.Commit == "" || last.Source != source:
		return nil, refuse("user_confirmation_required", "%s has no earlier run of this source; the user must confirm new GitHub code in the plugins panel", source)
	case cand == nil:
		return nil, refuse("user_confirmation_required", "%s has no resolved candidate; the user must confirm the start in the plugins panel", source)
	case !x.matches(cand):
		return nil, refuse("user_confirmation_required", "the candidate %s is not the last run %s; the user must confirm new GitHub code in the plugins panel", runText(cand), x)
	}
	return x, nil
}

func (h *Hub) loadOp(ctx context.Context, id string) (*pluginOp, error) {
	op := &pluginOp{ID: id}
	var origin string
	err := h.db.QueryRowContext(ctx, `SELECT kind, entry, name, origin, input, target, phase, base, output, entry_identity, result, error, created, updated FROM plugin_ops WHERE op_id = ?`, id).
		Scan(&op.Kind, &op.Entry, &op.Name, &origin, &op.Input, &op.Target, &op.Phase, &op.Base, &op.Output, &op.EntryIdentity, &op.Result, &op.Error, &op.Created, &op.Updated)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(origin), &op.Origin)
	return op, nil
}

func (op *pluginOp) view() map[string]any {
	return map[string]any{"op_id": op.ID, "op": op.Kind, "entry": op.Entry, "origin": op.Origin, "phase": op.Phase,
		"result": op.Result, "error": op.Error, "created": op.Created, "updated": op.Updated}
}

func (h *Hub) opView(ctx context.Context, id string) (any, error) {
	op, err := h.loadOp(ctx, id)
	if err != nil {
		return nil, err
	}
	return op.view(), nil
}

// pluginOpsList answers hub.plugins.ops: one op by op_id, or the 50 newest.
func (h *Hub) pluginOpsList(ctx context.Context, args map[string]json.RawMessage) (any, error) {
	for k := range args {
		if k != "op_id" {
			return nil, bad("invalid: unknown field %s", k)
		}
	}
	var id string
	if raw, ok := args["op_id"]; ok {
		if json.Unmarshal(raw, &id) != nil || id == "" {
			return nil, bad("invalid: op_id must be a string")
		}
		v, err := h.opView(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, bad("no op %s", id)
		}
		return v, err
	}
	rows, err := h.db.QueryContext(ctx, `SELECT op_id FROM plugin_ops ORDER BY created DESC, rowid DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, s)
	}
	rows.Close()
	out := []any{}
	for _, s := range ids {
		v, err := h.opView(ctx, s)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return map[string]any{"ops": out}, nil
}

// opsLoop is the manager goroutine. It ends with ctx (the hub's stop), also during a busy wait.
func (h *Hub) opsLoop(ctx context.Context) {
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.pops.kick:
		case <-retry:
		}
		retry = nil
		if wait := h.opsRun(ctx); wait > 0 {
			retry = time.After(wait)
		}
	}
}

// opsRun runs the open ops in admission order. An op that must wait stops the run: the later ops
// wait for it. It returns the wait (0: nothing to wait for).
func (h *Hub) opsRun(ctx context.Context) time.Duration {
	rows, err := h.db.QueryContext(ctx, `SELECT op_id FROM plugin_ops WHERE phase NOT IN ('done', 'failed', 'conflict', 'unknown') ORDER BY created, rowid`)
	if err != nil {
		log.Printf("plugin ops: %v", err)
		return opBusyRetry
	}
	var ids []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			ids = append(ids, s)
		}
	}
	rows.Close()
	for _, id := range ids {
		op, err := h.loadOp(ctx, id)
		if err == nil {
			err = h.runOp(ctx, op)
		}
		var w *opWait
		switch {
		case err == nil:
		case errors.As(err, &w):
			return w.after
		case errors.Is(err, errOpCrash) || ctx.Err() != nil:
			return 0
		default:
			log.Printf("plugin op %s: %v (retry)", id, err)
			return opBusyRetry
		}
	}
	return 0
}

func (h *Hub) opFault(point string) bool {
	return h.pops.fault != nil && h.pops.fault(point)
}

// setPhase stores the op's next phase, with its result or error text, then tells observers.
func (h *Hub) setOpPhase(ctx context.Context, op *pluginOp, phase, result, errText string) error {
	if _, err := h.db.ExecContext(ctx, `UPDATE plugin_ops SET phase = ?, result = ?, error = ?, updated = ? WHERE op_id = ?`,
		phase, result, errText, time.Now().UnixMilli(), op.ID); err != nil {
		return err
	}
	op.Phase, op.Result, op.Error = phase, result, errText
	h.ev.emit("plugin_op", map[string]any{"op_id": op.ID, "kind": op.Kind, "entry": op.Entry, "phase": phase, "result": result, "error": errText})
	h.changed("plugins")
	return nil
}

// nextPhase is the phase after cur for the op's kind: done after the last one.
func (op *pluginOp) nextPhase(cur string) string {
	list := pluginOpPhases[op.Kind]
	if cur == "queued" {
		return list[0]
	}
	for i, p := range list {
		if p == cur && i+1 < len(list) {
			return list[i+1]
		}
	}
	return "done"
}

// runOp runs the op's phases until it ends or must wait.
func (h *Hub) runOp(ctx context.Context, op *pluginOp) error {
	for !opFinal[op.Phase] {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var err error
		switch op.Phase {
		case "queued":
			err = h.setOpPhase(ctx, op, op.nextPhase("queued"), "", "")
		case "checking":
			err = h.opCheck(ctx, op)
		case "starting":
			err = h.opCall(ctx, op, "start")
		case "stopping":
			err = h.opCall(ctx, op, "stop")
		case "writing":
			err = h.opWrite(ctx, op)
		case "reloading":
			err = h.opCall(ctx, op, "reload")
		default:
			err = h.setOpPhase(ctx, op, "failed", "", "unknown phase "+op.Phase)
		}
		if err != nil {
			return err
		}
	}
	delete(h.pops.busy, op.ID)
	return nil
}

// opManagedID is the managed id of the op's entry.
func (op *pluginOp) managedID() string { return strings.TrimPrefix(op.Entry, "managed:") }

// opIntended is the entry that the op's write leaves in the file: the canonical entry, or "absent".
func (op *pluginOp) intended() (string, error) {
	if op.Kind == "remove" {
		return "absent", nil
	}
	var m managedFileEntry
	if err := json.Unmarshal([]byte(op.Base), &m); err != nil {
		return "", fmt.Errorf("the stored base: %v", err)
	}
	var in opSettings
	json.Unmarshal([]byte(op.Input), &in)
	e := &configEntry{managed: m.ID, Source: m.Source, Args: in.Args, WatchRoots: in.WatchRoots}
	return canonicalEntry(e), nil
}

// opCheck is the remove's graph check before its stop: a remove that leaves a dependent without
// its dependency stops nothing.
func (h *Hub) opCheck(ctx context.Context, op *pluginOp) error {
	_, list, err := readManaged(h.cfg.managedPath)
	if err != nil {
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("managed plugin file %s: %v", h.cfg.managedPath, err))
	}
	if cur := canonicalEntry(managedByID(list, op.managedID())); cur != op.Base {
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("%s changed since the operation was admitted: %s, now %s", op.Entry, op.Base, cur))
	}
	if msg, err := h.removeRefusal(list, op.managedID()); err != nil || msg != "" {
		if err != nil {
			msg = err.Error()
		}
		return h.setOpPhase(ctx, op, "conflict", "", msg)
	}
	return h.setOpPhase(ctx, op, op.nextPhase("checking"), "", "")
}

// removeRefusal checks the graph without the managed entry id. An entry that passes the check
// with the entry and is refused without it is a dependent: the remove is refused with its text.
// An entry that is refused already is not the remove's concern: it does not block the remove.
func (h *Hub) removeRefusal(list []*configEntry, id string) (string, error) {
	lockFile := lockPath(h.cfg.path, h.cfg.Lock)
	lock, err := readLock(lockFile)
	if err != nil {
		return "", err
	}
	var rest []*configEntry
	for _, e := range list {
		if e.managed != id {
			rest = append(rest, e)
		}
	}
	before := h.opGraph(lockFile, list, lock)
	after := h.opGraph(lockFile, rest, lock)
	for ident, err := range after {
		if before[ident] == nil {
			return fmt.Sprintf("removing managed:%s refuses %s: %v", id, ident, err), nil
		}
	}
	return "", nil
}

// opGraph runs the graph check of the plugin commands (graphRefusals) on the config entries and
// the managed entries list, with lock: the refusals by entry identity.
func (h *Hub) opGraph(lockFile string, list []*configEntry, lock *pluginLock) map[string]error {
	h.entriesMu.Lock()
	var config []*configEntry
	for _, e := range h.entries {
		if e.managed == "" {
			c := &configEntry{Source: e.Source, Args: e.Args, WatchRoots: e.WatchRoots, index: e.index, dir: e.dir, github: e.github,
				owner: e.owner, repo: e.repo, subdir: e.subdir, gitRef: e.gitRef}
			config = append(config, c)
		}
	}
	h.entriesMu.Unlock()
	var managed []*configEntry
	for _, e := range list {
		c := *e
		c.static = nil
		managed = append(managed, &c)
	}
	cfg := &pluginCmdConfig{path: h.cfg.path, state: h.cfg.State, lock: lockFile, managed: h.cfg.managedPath, config: config}
	cfg.entries = addManaged(config, managed)
	out := map[string]error{}
	for e, err := range graphRefusals(cfg, lock) {
		out[e.identity()] = err
	}
	return out
}

// opWrite changes the op's entry in the managed file, under both plugin command flocks. Its
// recovery rule, after a crash at any point: the file hash is the stored output -> written; the
// entry is already the intended entry -> written; the entry is still base -> write; else conflict.
func (h *Hub) opWrite(ctx context.Context, op *pluginOp) error {
	release, err := lockCommands(h.cfg.State, lockPath(h.cfg.path, h.cfg.Lock))
	if err != nil {
		if strings.HasPrefix(err.Error(), "another plugin command runs") {
			return h.opBusy(ctx, op, err)
		}
		return err
	}
	defer release()
	delete(h.pops.busy, op.ID)
	path := h.cfg.managedPath
	b, list, err := readManaged(path)
	if err != nil {
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("managed plugin file %s: %v", path, err))
	}
	intended, err := op.intended()
	if err != nil {
		return h.setOpPhase(ctx, op, "failed", "", err.Error())
	}
	id := op.managedID()
	cur := canonicalEntry(managedByID(list, id))
	switch {
	case op.Output != "" && b != nil && hashBytes(b) == op.Output, cur == intended:
		return h.setOpPhase(ctx, op, op.nextPhase("writing"), "", "") // written (before a crash, or no change)
	case cur != op.Base:
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("%s changed since the operation was admitted: %s, now %s", op.Entry, op.Base, cur))
	}
	if op.Kind == "remove" {
		if msg, err := h.removeRefusal(list, id); err != nil || msg != "" {
			if err != nil {
				msg = err.Error()
			}
			return h.setOpPhase(ctx, op, "conflict", "", msg+"; the plugin is stopped")
		}
	}
	// The new file: every other entry as the locked read gave it, in its order.
	var file struct {
		Plugins []managedFileEntry `json:"plugins"`
	}
	file.Plugins = []managedFileEntry{}
	for _, e := range list {
		if e.managed != id {
			file.Plugins = append(file.Plugins, fileEntry(e))
			continue
		}
		if op.Kind == "settings" {
			var m managedFileEntry
			json.Unmarshal([]byte(intended), &m)
			file.Plugins = append(file.Plugins, m)
		}
	}
	nb, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	nb = append(nb, '\n')
	// The strict loader checks the whole new file (ids, sources, fields, the size bound).
	if _, err := parseManagedBytes(nb); err != nil {
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("the new managed file is not valid: %v; nothing was written", err))
	}
	out := hashBytes(nb)
	if _, err := h.db.ExecContext(ctx, `UPDATE plugin_ops SET output = ?, updated = ? WHERE op_id = ?`, out, time.Now().UnixMilli(), op.ID); err != nil {
		return err
	}
	op.Output = out
	if h.opFault("plugin-op-before-rename") {
		return errOpCrash
	}
	if err := replaceFile(path, nb); err != nil {
		var re *replacedError
		if !errors.As(err, &re) {
			return err
		}
		log.Printf("plugin op %s: %v (the new file is in place)", op.ID, err)
	}
	if h.opFault("plugin-op-after-rename") {
		return errOpCrash
	}
	return h.setOpPhase(ctx, op, op.nextPhase("writing"), "", "")
}

// opBusy keeps the phase while another plugin command holds the flocks. After opBusyNote the op
// reports that command, and still tries again.
func (h *Hub) opBusy(ctx context.Context, op *pluginOp, err error) error {
	if h.pops.busy == nil {
		h.pops.busy = map[string]time.Time{}
	}
	first, ok := h.pops.busy[op.ID]
	if !ok {
		h.pops.busy[op.ID] = time.Now()
	} else if time.Since(first) >= opBusyNote && op.Result != err.Error() {
		if _, e := h.db.ExecContext(ctx, `UPDATE plugin_ops SET result = ?, updated = ? WHERE op_id = ?`, err.Error(), time.Now().UnixMilli(), op.ID); e == nil {
			op.Result = err.Error()
			h.changed("plugins")
		}
	}
	return &opWait{after: opBusyRetry, why: err.Error()}
}

// The lifecycle calls of the manager. A test replaces them.
var (
	opAdmin = func(ctx context.Context, h *Hub, action string, args map[string]any) (any, error) {
		return h.adminAction(ctx, action, args)
	}
	opReload = func(ctx context.Context, h *Hub) (any, error) { return h.reload(ctx, nil, "") }
)

// opCall makes the lifecycle call of the op's phase (start, stop or reload), once. The attempt
// row comes before the call; its outcome and the next phase are stored in one transaction. An
// attempt without an outcome (a crash or a stop during the call) is observed, never called again.
func (h *Hub) opCall(ctx context.Context, op *pluginOp, call string) error {
	var attempt, outcome string
	err := h.db.QueryRowContext(ctx, `SELECT attempt, outcome FROM plugin_op_attempts WHERE op_id = ? AND call = ?`, op.ID, call).Scan(&attempt, &outcome)
	switch {
	case err == nil && outcome == "":
		return h.opObserve(ctx, op, attempt, call)
	case err == nil:
		return h.setOpPhase(ctx, op, op.nextPhase(op.Phase), outcome, "") // the outcome is stored; the phase follows it
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	attempt = hex.EncodeToString(b)
	if _, err := h.db.ExecContext(ctx, `INSERT INTO plugin_op_attempts (attempt, op_id, call, expect_run, at) VALUES (?, ?, ?, ?, ?)`,
		attempt, op.ID, call, op.Target, time.Now().UnixMilli()); err != nil {
		return err
	}
	var res any
	var callErr error
	switch call {
	case "start":
		res, callErr = h.opStart(ctx, op)
	case "stop":
		res, callErr = h.opStop(ctx, op)
	case "reload":
		res, callErr = opReload(ctx, h)
	}
	if h.opFault("plugin-op-after-" + call) {
		return errOpCrash
	}
	if ctx.Err() != nil {
		return ctx.Err() // the outcome is unknown: the next hub observes it
	}
	phase, result, errText := op.nextPhase(op.Phase), opResultText(call, res), ""
	if callErr != nil {
		phase, result, errText = "failed", "", callErr.Error()
		if isRefusal(callErr, "candidate_changed") || isRefusal(callErr, "entry_changed") {
			phase = "conflict"
		}
	}
	if call == "reload" && callErr == nil {
		if msg := h.reloadEntryError(res, op); msg != "" {
			errText = msg
		}
	}
	return h.finishCall(ctx, op, attempt, phase, result, errText)
}

// finishCall stores the outcome of the op's open attempt and the op's next phase in one
// transaction.
func (h *Hub) finishCall(ctx context.Context, op *pluginOp, attempt, phase, result, errText string) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored := result
	if errText != "" {
		stored = "error: " + errText
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_op_attempts SET outcome = ? WHERE attempt = ?`, stored, attempt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_ops SET phase = ?, result = ?, error = ?, updated = ? WHERE op_id = ?`,
		phase, result, errText, time.Now().UnixMilli(), op.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	op.Phase, op.Result, op.Error = phase, result, errText
	h.ev.emit("plugin_op", map[string]any{"op_id": op.ID, "kind": op.Kind, "entry": op.Entry, "phase": phase, "result": result, "error": errText})
	h.changed("plugins")
	return nil
}

func opResultText(call string, res any) string {
	if res == nil {
		return call + ": done"
	}
	b, _ := json.Marshal(res)
	return call + ": " + string(b)
}

// reloadEntryError is the reload's error for the op's plugin, if the reload reported one.
func (h *Hub) reloadEntryError(res any, op *pluginOp) string {
	m, _ := res.(map[string]any)
	errs, _ := m["errors"].(map[string]string)
	if op.Name != "" && errs[op.Name] != "" {
		return errs[op.Name]
	}
	return ""
}

// opEntryNow is the entry identity of the op's target now: the record of the op's name, or (an
// entry without a name) the entry of the op's key. "none" when there is no such entry.
func (h *Hub) opEntryNow(op *pluginOp) (pl *Plugin, now string) {
	now = "none"
	if op.Name == "" {
		h.entriesMu.Lock()
		defer h.entriesMu.Unlock()
		for _, e := range h.entries {
			if opEntryKey(e) == op.Entry {
				return nil, entryIdentity(e)
			}
		}
		return nil, now
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	pl = h.plugins[op.Name]
	if pl != nil && pl.entry != nil {
		now = entryIdentity(pl.entry)
	}
	return pl, now
}

// entryChanged is the op's conflict when its target is bound to another entry than at admission.
// This check only saves a call: the admin handler checks expect_entry again under its locks.
func entryChanged(op *pluginOp, now string) error {
	return refuse("entry_changed", "%s was admitted for entry %s; the target is bound to %s now; nothing was called", op.Entry, op.EntryIdentity, now)
}

// opStart starts the op's entry with the op's expect_run and expect_entry: nothing else than that
// run of that entry starts.
func (h *Hub) opStart(ctx context.Context, op *pluginOp) (any, error) {
	if _, now := h.opEntryNow(op); now != op.EntryIdentity {
		return nil, entryChanged(op, now)
	}
	var x map[string]any
	if err := json.Unmarshal([]byte(op.Target), &x); err != nil {
		return nil, fmt.Errorf("the stored target: %v", err)
	}
	for k, v := range x {
		if v == "" {
			delete(x, k) // a local source has no commit and no digest
		}
	}
	args := map[string]any{"expect_run": x, "expect_entry": op.EntryIdentity}
	if op.Name != "" {
		args["plugin"] = op.Name
	} else {
		args["source"] = x["source"]
	}
	return opAdmin(ctx, h, "hub.admin.start", args)
}

// opStop stops the op's plugin, only while it is bound to the op's entry. An entry without a record
// has nothing to stop.
func (h *Hub) opStop(ctx context.Context, op *pluginOp) (any, error) {
	pl, now := h.opEntryNow(op)
	if op.Name == "" || pl == nil {
		return map[string]any{"desired": "stopped", "note": "no instance"}, nil
	}
	if now != op.EntryIdentity {
		return nil, entryChanged(op, now)
	}
	return opAdmin(ctx, h, "hub.admin.stop", map[string]any{"plugin": op.Name, "expect_entry": op.EntryIdentity})
}

// opObserve ends an op whose call has an attempt without an outcome. The manager does not call
// again: it reads what the hub knows.
//   - stop: the stored record is stopped with no instance, and its saved entry identity is the op's
//     -> the stop happened (the same effect from any caller); else unknown. A row without an
//     identity (from before it was recorded), or with another entry's, is never a match.
//   - reload: the hub's entries have the op's intended entry -> done (the hub start or the call read
//     the file); else unknown.
//   - start: unknown. A new start goes through the unresolved-intent guard of the start path.
func (h *Hub) opObserve(ctx context.Context, op *pluginOp, attempt, call string) error {
	switch call {
	case "stop":
		var desired, block, identity string
		err := h.db.QueryRowContext(ctx, `SELECT desired, block_id, entry_identity FROM plugins WHERE name = ?`, op.Name).Scan(&desired, &block, &identity)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && op.EntryIdentity != "" && identity == op.EntryIdentity && desired == "stopped" && block == "" {
			return h.finishCall(ctx, op, attempt, op.nextPhase(op.Phase), "stop: stopped (observed after the hub restarted)", "")
		}
		if err != nil {
			return h.finishCall(ctx, op, attempt, "unknown", "", "the stop's outcome is unknown: the plugin has no record")
		}
		return h.finishCall(ctx, op, attempt, "unknown", "", fmt.Sprintf("the stop's outcome is unknown: the record is %s with block %q, saved with entry %q; the op's entry is %s", desired, block, identity, op.EntryIdentity))
	case "reload":
		intended, err := op.intended()
		if err != nil {
			return err
		}
		h.entriesMu.Lock()
		var cur *configEntry
		for _, e := range h.entries {
			if e.managed == op.managedID() {
				cur = e
			}
		}
		now := canonicalEntry(cur)
		h.entriesMu.Unlock()
		if now == intended {
			return h.finishCall(ctx, op, attempt, op.nextPhase(op.Phase), "reload: the hub has the new entry (observed after the hub restarted)", "")
		}
		return h.finishCall(ctx, op, attempt, "unknown", "", fmt.Sprintf("the reload's outcome is unknown: the hub has %s", now))
	}
	return h.finishCall(ctx, op, attempt, "unknown", "", "the start's outcome is unknown: the hub stopped during the start. A new start runs only after the hub resolves the spawn intent (unresolved_intent).")
}

// pluginOpActions are hub.plugins.op and hub.plugins.ops on the agent face (agentKeymap).
var pluginOpActions = []map[string]any{
	{"name": "hub.plugins.op", "kind": "client", "title": "Run a plugin operation",
		"description": "Start, stop, remove or change the settings of a plugin entry. entry is \"managed:<id>\" (a managed entry) or the exact source of a config entry; settings and remove change only managed entries. settings takes input {args, watch_roots}; they apply at the next start. The hub runs the operation in the background: read its phase with hub.plugins.ops op_id=<op_id>. Choose a key; after an unknown outcome, retry with the same key. A start of a GitHub entry runs only the plugin's last run; new GitHub code needs the user's confirm in the plugins panel (user_confirmation_required). The home plugin cannot be stopped or removed.",
		"args": map[string]any{"type": "object", "properties": map[string]any{
			"key":   map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"op":    map[string]any{"type": "string", "enum": []string{"start", "stop", "settings", "remove"}},
			"entry": map[string]any{"type": "string", "minLength": 1},
			"input": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"args":        map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"watch_roots": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}},
		}, "required": []string{"key", "op", "entry"}, "additionalProperties": false}},
	{"name": "hub.plugins.ops", "kind": "client", "title": "List plugin operations",
		"description": "The plugin operations: one by op_id, or the 50 newest. Each has op, entry, origin, phase (queued, a working phase, then done, failed, conflict or unknown), result and error.",
		"args":        map[string]any{"type": "object", "properties": map[string]any{"op_id": map[string]any{"type": "string", "minLength": 1}}, "additionalProperties": false}},
}
