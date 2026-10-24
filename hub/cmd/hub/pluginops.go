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
	"io"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
)

const pluginOpsSchema = `
-- phase: queued, then the phases of the kind (pluginOpPhases), then done, failed, conflict or unknown.
-- base: the entry at admission (canonical JSON, or "absent"); output: the hash of the bytes that the
-- write renames, stored before the rename.
-- entry_identity: entryIdentity of the entry at admission; the start and stop calls send it as expect_entry.
-- pin: add and update: the fetched lock entry (JSON), stored before the lock write.
-- lock_base: add and update: the lock entry of each key that the target can use, at admission
-- (JSON: key -> lockBase); the lock write compares against it.
-- row_prior: add: the record of its plugins row write (JSON opRowPrior, pluginopsrow.go), '' before it.
-- lock_old: update, remove: the record of unpinning (JSON opLockOld, pluginopsunpin.go), '' before it.
CREATE TABLE IF NOT EXISTS plugin_ops (op_id TEXT PRIMARY KEY, scope TEXT NOT NULL, key TEXT NOT NULL,
  fingerprint TEXT NOT NULL, origin TEXT NOT NULL, kind TEXT NOT NULL, entry TEXT NOT NULL, name TEXT NOT NULL,
  expect TEXT NOT NULL, input TEXT NOT NULL, target TEXT NOT NULL, phase TEXT NOT NULL, rev INTEGER NOT NULL DEFAULT 1,
  confirmed TEXT NOT NULL DEFAULT '', base TEXT NOT NULL DEFAULT '', output TEXT NOT NULL DEFAULT '', entry_identity TEXT NOT NULL DEFAULT '',
  result TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL, updated INTEGER NOT NULL,
  pin TEXT NOT NULL DEFAULT '', lock_base TEXT NOT NULL DEFAULT '', row_prior TEXT NOT NULL DEFAULT '', lock_old TEXT NOT NULL DEFAULT '', UNIQUE (scope, key));
-- One row for each lifecycle call of an op, stored before the call. outcome '': the call may have
-- run; the manager observes its result and never calls again.
-- bound_at: the handler bound the attempt (with target_instance, target_block, target_session) before its first
-- effect. sent_at: the handler committed it before its first effect; empty sent_at means that no
-- effect began. intent_id: the spawn intent of a start or a replace. proof: how the outcome is
-- known (separate from the outcome text). An outcome that starts with "unproven:" ends the later
-- observation without a proof (observeLater).
-- A file write (call write:managed or write:lock) has its own attempt: intended is the hash of the bytes that it
-- renames; bound_at is the insert, sent_at the moment just before the rename.
CREATE TABLE IF NOT EXISTS plugin_op_attempts (attempt TEXT PRIMARY KEY, op_id TEXT NOT NULL, call TEXT NOT NULL,
  expect_run TEXT NOT NULL DEFAULT '', at INTEGER NOT NULL,
  outcome TEXT NOT NULL DEFAULT '', bound_at TEXT NOT NULL DEFAULT '', target_instance TEXT NOT NULL DEFAULT '',
  target_block TEXT NOT NULL DEFAULT '', target_session TEXT NOT NULL DEFAULT '', intent_id TEXT NOT NULL DEFAULT '', sent_at TEXT NOT NULL DEFAULT '',
  proof TEXT NOT NULL DEFAULT '', intended TEXT NOT NULL DEFAULT '');
`

// pluginOpPhases are the working phases of each kind, in order.
var pluginOpPhases = map[string][]string{
	"start":    {"starting"},
	"stop":     {"stopping"},
	"settings": {"writing", "reloading"},
	// checking: the graph check before the stop, so a refused remove stops nothing.
	// unpinning: the op removes the lock entry of the source that it stopped naming (C5f).
	"remove": {"checking", "stopping", "writing", "reloading", "unpinning"},
	// add and update (B5): each phase is stored before its effect, and a replay uses only stored
	// values. An add owns the first start of its plugin (starting); an update and a restart replace.
	"add":     {"resolving", "fetching", "locking", "writing", "reloading", "starting"},
	"update":  {"resolving", "fetching", "locking", "writing", "reloading", "unpinning", "replacing"},
	"restart": {"replacing"},
	// install (C2, lead-c2i2..5): the hub puts the code of a managed entry into the lock and the
	// cache, at start and at each reload (pluginopsinstall.go). It writes no managed entry and
	// starts nothing: the hub's normal start runs after the installs of a scan.
	"install": {"resolving", "fetching", "locking"},
}

// opFinal are the phases that end an op.
var opFinal = map[string]bool{"done": true, "failed": true, "conflict": true, "unknown": true}

// opBusyRetry is the wait before the next try when another plugin command holds the flocks;
// opBusyNote is the wait after which the op reports that command.
const (
	opBusyRetry = 2 * time.Second
	opBusyNote  = 10 * time.Minute
	// opRegisterPoll is the interval of the checks while a start observation waits for a
	// registration (a registration also kicks the manager).
	opRegisterPoll = time.Second
	// opObservePoll is the interval of the later observation while an attempt stays unresolved.
	opObservePoll = 30 * time.Second
	// opObservePasses bounds the later observation: an attempt that gets no proof in this many
	// passes, at least opObservePoll apart (5 minutes), ends unproven (observeLater).
	opObservePasses = 10
)

// opRegisterWait bounds the wait of a start observation for the registration of its intent's
// instance; after it, the op ends unknown (docs/hub.md, "attempt"). The SDK registers before a
// plugin's start functions run, so 60 s is a wide margin, and observeLater still proves a later
// registration (the hub lead, lead-b8-c2b).
const opRegisterWait = 60 * time.Second

// registerWait is opRegisterWait; a test shortens it.
var registerWait = opRegisterWait

type pluginOpsSvc struct {
	kick chan struct{}
	busy map[string]time.Time // op ID -> the first busy try (the manager goroutine only)
	// observing: attempt -> the first observation that waits for a registration (the manager goroutine only)
	observing map[string]time.Time
	// later: attempt -> its counted passes of the later observation (the manager goroutine only).
	// The count is per hub process.
	later map[string]laterPasses
	// fault is a test hook: true at a point stops the manager there, as a crash would.
	fault func(point string) bool
	// cmd are the options of the git commands of the resolving and fetching phases (a test sets the
	// remote URL).
	cmd pluginCmdOpts
	// resolve is a test hook in place of opResolveTarget; nil in production.
	resolve func(ctx context.Context, op *pluginOp) (opResolved, error)
	// installs are the install ops of the scans that have not all ended (pluginopsinstall.go).
	installs pluginInstalls
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
	Kind string `json:"kind"` // "user", "agent" or "peer" (a plugin sync from a peer hub, peersync.go)
	ID   string `json:"id"`   // the view ID of the user's operation, the agent ID, or the peer hub name
}

// opRequest is the request of an op: the same fields from an agent and from the panel.
type opRequest struct {
	Op     string          `json:"op"`
	Entry  string          `json:"entry"`  // "managed:<id>", or the exact source of a config entry
	Expect json.RawMessage `json:"expect"` // start from the panel: the run that the panel showed
	Input  json.RawMessage `json:"input"`  // settings: {args, watch_roots}; add: {source, id?, args?, watch_roots?}; update: {to}
	// Target: a peer update (peersync.go): the peer's pin of the source, a pin document entry. A
	// user's add of an offer: the complete offer {peer, id, source, pin} (hub.plugins "offered").
	// The pin is the stored target, so the op does not resolve; the fetch must give its digest.
	Target json.RawMessage `json:"target"`
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
	Target                       string // start: the expect_run JSON; restart: the run and instance; add, update: the resolved target
	Pin                          string // add, update: the fetched lock entry (JSON)
	LockBase                     string // add, update: the admission snapshot of the lock (lock_base)
	RowPrior                     string // add: the record of its plugins row write (row_prior), '' before it
	LockOld                      string // update, remove: the record of unpinning (lock_old), '' before it
	Base, Output                 string
	EntryIdentity                string // entryIdentity of the entry at admission ("" in no admitted op)
	Result, Error                string
	Created, Updated             int64
}

// pluginOpsSchemaUp creates the manager's tables. No release had them, so there is no migration:
// every column is in its CREATE TABLE.
func (h *Hub) pluginOpsSchemaUp(ctx context.Context) error {
	_, err := h.db.ExecContext(ctx, pluginOpsSchema)
	return err
}

func (h *Hub) pluginOpsStart(ctx context.Context) error {
	if err := h.pluginOpsSchemaUp(ctx); err != nil {
		return err
	}
	h.pops.kick = make(chan struct{}, 1)
	h.loadsStart(ctx)
	if err := h.installsRecover(ctx); err != nil { // before the first pass (pluginopsinstall.go)
		return err
	}
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
	b, err := readManagedBytes(path)
	if err != nil || b == nil {
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

// readBack is the hash of the file at path after a failed rename of call: the managed file is read
// within its limit (managedFileLimit+1 bytes; a larger file is not the op's output), the lock as a
// stream. A file that cannot be read gives "".
func readBack(call, path string) string {
	if call != "write:managed" {
		return hashFile(path)
	}
	b, err := readManagedBytes(path)
	if err != nil || b == nil {
		return ""
	}
	return hashBytes(b)
}

// hashFile is hashBytes of the file at path, read as a stream, so its memory does not grow with
// the file. A file that cannot be read gives "".
func hashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
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
// current conversation (agentOfPID): a plugin process has none, so it is refused.
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
		return h.pluginOpsList(ctx, args, "")
	}
	key, req, err := agentOpRequest(args)
	if err != nil {
		return nil, err
	}
	return h.admitOp(ctx, opOrigin{"agent", agent}, "agent:"+agent, key, req)
}

// agentOpRequest reads the key and the request of an agent's hub.plugins.op. An agent gives expect
// only for settings: only the user's panel names the run that it showed.
func agentOpRequest(args map[string]json.RawMessage) (string, opRequest, error) {
	var key string
	if json.Unmarshal(args["key"], &key) != nil || key == "" || len(key) > 128 {
		return "", opRequest{}, bad("invalid: key must be a string of 1 to 128 characters")
	}
	rest := map[string]json.RawMessage{}
	for k, v := range args {
		if k != "key" {
			rest[k] = v
		}
	}
	b, _ := json.Marshal(rest)
	req, err := decodeOpRequest(b)
	if err == nil && len(req.Expect) > 0 && req.Op != "settings" {
		return "", opRequest{}, bad("invalid: expect is set only by the user's panel, except for settings")
	}
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
	m := map[string]any{"op": req.Op, "entry": req.Entry, "expect": canon(req.Expect), "input": canon(req.Input)}
	if len(req.Target) > 0 {
		m["target"] = canon(req.Target)
	}
	b, _ := json.Marshal(m)
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
	if op.Phase == "" {
		op.Phase = "queued"
	}
	// OR IGNORE: a second admission of the same key in a race finds the first one.
	if _, err := h.db.ExecContext(ctx, `INSERT OR IGNORE INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, result, base, entry_identity, lock_base, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, scope, key, fp, string(ob), op.Kind, op.Entry, op.Name, string(req.Expect), op.Input, op.Target, op.Phase, op.Result, op.Base, op.EntryIdentity, op.LockBase, now, now); err != nil {
		return nil, err
	}
	if id, err = find(); err != nil {
		return nil, err
	}
	h.ev.emit("plugin_op", map[string]any{"op_id": id, "kind": op.Kind, "entry": op.Entry, "origin": origin, "phase": op.Phase})
	h.kickOps()
	h.changed("plugins")
	return h.opView(ctx, id)
}

// opAdmission checks a request and builds its op (without ID).
func (h *Hub) opAdmission(origin opOrigin, req opRequest) (*pluginOp, error) {
	op := &pluginOp{Kind: req.Op, Entry: req.Entry, Origin: origin}
	if _, ok := pluginOpPhases[req.Op]; !ok {
		return nil, bad("invalid: op must be start, stop, settings, remove, add, update or restart")
	}
	if len(req.Expect) > 0 && req.Op != "start" && req.Op != "settings" {
		return nil, bad("invalid: only start and settings have expect")
	}
	if len(req.Input) > 0 && req.Op != "settings" && req.Op != "add" && req.Op != "update" {
		return nil, bad("invalid: only settings, add and update have input")
	}
	// A peer makes only updates with a target and no input (peersync.go). The only other target is
	// the offer of a user's add: the complete offer that the panel showed (addAdmission).
	// An install is the hub's own op, with no input: the target is the entry's lock entry, or none
	// (then it resolves the entry's ref).
	peerUpdate := origin.Kind == "peer" && req.Op == "update" && len(req.Input) == 0 && len(req.Target) > 0
	offerAdd := origin.Kind == "user" && req.Op == "add" && len(req.Target) > 0
	install := origin.Kind == "hub" && req.Op == "install" && len(req.Input) == 0
	if (origin.Kind == "hub") != (req.Op == "install") || (origin.Kind == "hub" && !install) {
		return nil, bad("invalid: only the hub admits an install")
	}
	if (origin.Kind == "peer" && !peerUpdate) || (len(req.Target) > 0 && !peerUpdate && !offerAdd && !install) {
		return nil, bad("invalid: only a peer sync gives target (an update with no input), and a user's add of an offer")
	}
	// An agent add is user_only: the user confirms a new source, and the text names what the agent
	// can do (P1; docs/hub.md). An agent update is admitted (B9 removed for update by the user's
	// decision of 2026-10-23).
	if origin.Kind == "agent" && req.Op == "add" {
		return nil, refuse("user_only", "add needs the user: ask the user to open LazyRex: Plugins…")
	}
	if req.Op == "add" {
		return h.addAdmission(op, req)
	}
	h.entriesMu.Lock()
	var e *configEntry
	for _, x := range h.entries {
		if opEntryKey(x) == req.Entry {
			e = x
			break
		}
	}
	// Every field that admission uses is read here, under entriesMu: a reload changes an entry in
	// place (updateManaged), so e is not read after the unlock.
	var name, source, managed, identity string
	var github bool
	var cand *pluginRun
	if e != nil {
		name, source, managed, identity, github = e.name, e.Source, e.managed, entryIdentity(e), e.github
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
	op.EntryIdentity = identity
	if (req.Op == "settings" || req.Op == "remove" || req.Op == "update" || req.Op == "install") && managed == "" {
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
	case "update":
		if !github {
			return nil, bad("invalid: update needs a GitHub source; %s is %s", req.Entry, source)
		}
		if len(req.Target) > 0 {
			t, err := peerTarget(req.Target, source)
			if err != nil {
				return nil, err
			}
			op.Target = t
			break
		}
		var in opUpdateInput
		if err := decodeManifest("input", req.Input, &in); err != nil {
			return nil, bad("invalid: %v", err)
		}
		if !updateTarget(in.To) {
			return nil, bad(`invalid: to must be "@", "latest" or a tag name; a branch or a commit is not accepted`)
		}
		b, _ := json.Marshal(in)
		op.Input = string(b)
	case "install":
		if !github {
			return nil, bad("invalid: install needs a GitHub source; %s is %s", req.Entry, source)
		}
		if len(req.Target) > 0 { // the lock entry: fetched by its commit ID, with its digest
			t, err := peerTarget(req.Target, source)
			if err != nil {
				return nil, err
			}
			op.Target = t
		}
	case "restart":
		x, err := h.restartTarget(name)
		if err != nil {
			return nil, err
		}
		if cand != nil && !x.run().matches(cand) {
			return nil, refuse("candidate_changed", "%s runs %s, and its candidate is %s; a restart runs the current run only: use update", req.Entry, x.run(), runText(cand))
		}
		b, _ := json.Marshal(x)
		op.Target = string(b)
	}
	if req.Op == "settings" || req.Op == "remove" || req.Op == "update" || req.Op == "install" {
		_, list, err := readManaged(h.cfg.managedPath)
		if err != nil {
			return nil, refuse("refused", "managed plugin file %s: %v", h.cfg.managedPath, err)
		}
		m := managedByID(list, managed)
		if m == nil {
			return nil, refuse("refused", "%s is not in the managed file now; reload first", req.Entry)
		}
		op.Base = canonicalEntry(m)
		if req.Op == "settings" {
			if err := settingsAdmission(op, req, m); err != nil {
				return nil, err
			}
		}
	}
	if req.Op == "update" {
		// The keys that the resolve can choose (opResolveTarget): latest, or the two spellings of a
		// tag; a refresh ("@") keeps the source, so its key is the old key.
		var in opUpdateInput
		var base managedFileEntry
		json.Unmarshal([]byte(op.Input), &in)
		json.Unmarshal([]byte(op.Base), &base)
		var keys []string
		switch {
		case op.Target != "":
			var t opResolved
			json.Unmarshal([]byte(op.Target), &t)
			keys = []string{t.Source}
		case in.To == "@":
		default:
			keys = []string{opNewSource(base.Source, in.To, false)}
			if in.To != "latest" {
				keys = append(keys, opNewSource(base.Source, in.To, true))
			}
		}
		if !slices.Contains(keys, base.Source) {
			keys = append(keys, base.Source) // the old key: unpinning compares against it (C5f)
		}
		snap, err := h.lockSnapshot(keys)
		if err != nil {
			return nil, err
		}
		op.LockBase = snap
	}
	if req.Op == "install" {
		// An install keeps the source: its one key is the entry's source.
		var base managedFileEntry
		json.Unmarshal([]byte(op.Base), &base)
		snap, err := h.lockSnapshot([]string{base.Source})
		if err != nil {
			return nil, err
		}
		op.LockBase = snap
	}
	if req.Op == "remove" {
		// The old key of a GitHub entry: unpinning compares against it (C5f).
		if old := op.oldKey(); old != "" {
			snap, err := h.lockSnapshot([]string{old})
			if err != nil {
				return nil, err
			}
			op.LockBase = snap
		}
	}
	return op, nil
}

// settingsAdmission checks the settings op's expect against the managed entry m: a difference is a
// conflict. An input that equals m's values ends the op at admission: done, "no change". A missing
// field and an empty one are equal.
func settingsAdmission(op *pluginOp, req opRequest, m *configEntry) error {
	same := func(s opSettings) bool {
		return maps.Equal(s.Args, m.Args) && slices.Equal(s.WatchRoots, m.WatchRoots)
	}
	if len(req.Expect) > 0 {
		var x opSettings
		if err := decodeManifest("expect", req.Expect, &x); err != nil {
			return bad("invalid: %v", err)
		}
		if !same(x) {
			return refuse("conflict", "the settings changed since they were shown")
		}
	}
	var in opSettings
	json.Unmarshal([]byte(op.Input), &in)
	if same(in) {
		op.Phase, op.Result = "done", "no change"
	}
	return nil
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
	err := h.db.QueryRowContext(ctx, `SELECT kind, entry, name, origin, input, target, phase, base, output, entry_identity, result, error, created, updated, pin, lock_base, row_prior, lock_old FROM plugin_ops WHERE op_id = ?`, id).
		Scan(&op.Kind, &op.Entry, &op.Name, &origin, &op.Input, &op.Target, &op.Phase, &op.Base, &op.Output, &op.EntryIdentity, &op.Result, &op.Error, &op.Created, &op.Updated, &op.Pin, &op.LockBase, &op.RowPrior, &op.LockOld)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(origin), &op.Origin)
	return op, nil
}

func (op *pluginOp) view() map[string]any {
	v := map[string]any{"op_id": op.ID, "op": op.Kind, "entry": op.Entry, "origin": op.Origin, "phase": op.Phase,
		"result": op.Result, "error": op.Error, "created": op.Created, "updated": op.Updated}
	var r opLockOld
	if op.LockOld != "" && json.Unmarshal([]byte(op.LockOld), &r) == nil {
		v["lock_old"] = r // the record of unpinning (C5f), when there is one
	}
	return v
}

func (h *Hub) opView(ctx context.Context, id string) (any, error) {
	op, err := h.loadOp(ctx, id)
	if err != nil {
		return nil, err
	}
	v := op.view()
	// attempts: each attempt record, oldest first.
	attempts := []map[string]any{}
	rows, err := h.db.QueryContext(ctx, `SELECT attempt, call, at, outcome, proof FROM plugin_op_attempts WHERE op_id = ? ORDER BY at, rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var attempt, call, outcome, proof string
		var at int64
		if err := rows.Scan(&attempt, &call, &at, &outcome, &proof); err != nil {
			return nil, err
		}
		attempts = append(attempts, map[string]any{"attempt": attempt, "call": call, "at": at, "outcome": outcome, "proof": proof})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	v["attempts"] = attempts
	return v, nil
}

// pluginOpsList answers hub.plugins.ops: one op by op_id, or the 50 newest.
// plugin: the calling plugin on the plugin face; "" on the agent face.
func (h *Hub) pluginOpsList(ctx context.Context, args map[string]json.RawMessage, plugin string) (any, error) {
	for k := range args {
		if k != "op_id" && (k != "origin_view" || plugin == "") {
			return nil, bad("invalid: unknown field %s", k)
		}
	}
	query, qargs := `SELECT op_id FROM plugin_ops ORDER BY created DESC, rowid DESC LIMIT 50`, []any{}
	if raw, ok := args["origin_view"]; ok {
		// origin_view (C8): the ops of the user's operations of one view of the calling plugin.
		var view, owner string
		if _, both := args["op_id"]; both {
			return nil, bad("invalid: give op_id or origin_view, not both")
		}
		if json.Unmarshal(raw, &view) != nil || view == "" {
			return nil, bad("invalid: origin_view must be a string")
		}
		err := h.db.QueryRowContext(ctx, `SELECT plugin FROM views WHERE view_id = ?`, view).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || err == nil && owner != plugin {
			return nil, refuse("not_found", "no view %s of plugin %s", view, plugin)
		} else if err != nil {
			return nil, err
		}
		query = `SELECT op_id FROM plugin_ops WHERE json_extract(origin, '$.kind') = 'user' AND json_extract(origin, '$.id') = ?
			ORDER BY created DESC, rowid DESC LIMIT 50`
		qargs = append(qargs, view)
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
	rows, err := h.db.QueryContext(ctx, query, qargs...)
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
	left := h.observeLater(ctx)
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
	h.installsEnded(ctx)
	// An op of this pass can end with an unproved attempt: the observation then needs a next pass,
	// also on an idle hub.
	if left == 0 {
		if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) `+laterAttempts).Scan(&left); err != nil {
			return opObservePoll
		}
	}
	if left > 0 {
		return opObservePoll
	}
	return 0
}

// attemptTarget is what the handler bound to an attempt: the instance, block and session that it
// acted on (stop, replace), and the spawn intent that it created (start, replace).
type attemptTarget struct{ instance, block, session, intent string }

// opSessionBlocks lists the blocks of a session for an observation (a test replaces it).
var opSessionBlocks = func(ctx context.Context, h *Hub, sess string) (map[string]bool, error) {
	return h.sessionBlocksIn(ctx, sess)
}

func (h *Hub) opFault(point string) bool {
	return h.pops.fault != nil && h.pops.fault(point)
}

// setPhase stores the op's next phase, with its result or error text, then tells observers. An
// add that wrote its plugins row and ends without done ends through endAddRow (pluginopsrow.go).
func (h *Hub) setOpPhase(ctx context.Context, op *pluginOp, phase, result, errText string) error {
	if opFinal[phase] && phase != "done" && op.Kind == "add" && op.RowPrior != "" {
		return h.endAddRow(ctx, op, phase, result, errText)
	}
	lockOld, note := op.endRecord(phase)
	result, errText = withEndNote(phase, result, errText, note)
	if _, err := h.db.ExecContext(ctx, `UPDATE plugin_ops SET phase = ?, result = ?, error = ?, lock_old = ?, updated = ? WHERE op_id = ?`,
		phase, result, errText, lockOld, time.Now().UnixMilli(), op.ID); err != nil {
		return err
	}
	op.LockOld = lockOld
	h.opPhaseStored(op, phase, result, errText)
	return nil
}

// opPhaseStored sets the op's stored phase and texts in op, then tells observers.
func (h *Hub) opPhaseStored(op *pluginOp, phase, result, errText string) {
	op.Phase, op.Result, op.Error = phase, result, errText
	h.ev.emit("plugin_op", map[string]any{"op_id": op.ID, "kind": op.Kind, "entry": op.Entry, "phase": phase, "result": result, "error": errText})
	h.changed("plugins")
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
		case "resolving":
			err = h.opResolve(ctx, op)
		case "fetching":
			err = h.opFetch(ctx, op)
		case "locking":
			err = h.opLock(ctx, op)
		case "writing":
			err = h.opWrite(ctx, op)
		case "reloading":
			err = h.opCall(ctx, op, "reload")
		case "unpinning":
			err = h.opUnpin(ctx, op)
		case "replacing":
			err = h.opReplacing(ctx, op)
		case "fallback":
			err = h.opFallback(ctx, op)
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
	switch op.Kind {
	case "remove":
		return "absent", nil
	case "add", "update":
		var t opResolved
		if err := json.Unmarshal([]byte(op.Target), &t); err != nil {
			return "", fmt.Errorf("the stored target: %v", err)
		}
		m, err := op.newManaged(t)
		if err != nil {
			return "", err
		}
		return canonicalEntry(&configEntry{managed: m.ID, Source: m.Source, Args: m.Args, WatchRoots: m.WatchRoots}), nil
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
				owner: e.owner, repo: e.repo, subdir: e.subdir, gitRef: e.gitRef, static: e.static}
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
// For an add or an update, "written" goes on only after the writing checks (writeChecks).
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
	if err := h.settleFileAttempt(ctx, op, "write:managed", b); err != nil {
		return err
	}
	if err != nil {
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("managed plugin file %s: %v", path, err))
	}
	intended, err := op.intended()
	if err != nil {
		return h.setOpPhase(ctx, op, "failed", "", err.Error())
	}
	id := op.managedID()
	cur := canonicalEntry(managedByID(list, id))
	// written: the rename happened before a crash, or the file needs no change (an update that
	// keeps its source string).
	written := op.Output != "" && b != nil && hashBytes(b) == op.Output || cur == intended
	checks := op.Kind == "add" || op.Kind == "update"
	switch {
	case written && !checks:
		return h.setOpPhase(ctx, op, op.nextPhase("writing"), "", "")
	case !written && cur != op.Base:
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
	// An add or an update runs its writing checks on every path, also when the file needs no write
	// (hub lead, lead-c4e 2): only "already at" (fetching) ends an update early.
	if checks {
		if stop, err := h.writeChecks(ctx, op); stop || err != nil {
			return err
		}
		if written {
			// An add goes on only while it owns its row (lead-c4e 3).
			if op.Kind == "add" {
				if stop, err := h.entryRow(ctx, op, intended); stop || err != nil {
					return err
				}
			}
			return h.setOpPhase(ctx, op, op.nextPhase("writing"), "", "")
		}
	}
	// The new file: every other entry as the locked read gave it, in its order. An add's entry
	// comes last.
	plugins := []managedFileEntry{}
	var m managedFileEntry
	json.Unmarshal([]byte(intended), &m)
	for _, e := range list {
		if e.managed != id {
			plugins = append(plugins, fileEntry(e))
			continue
		}
		if op.Kind == "settings" || op.Kind == "update" {
			plugins = append(plugins, m)
		}
	}
	if op.Kind == "add" {
		plugins = append(plugins, m)
	}
	nb, err := encodeManaged(plugins)
	if err != nil {
		return h.setOpPhase(ctx, op, "conflict", "", fmt.Sprintf("the new managed file is not valid: %v; nothing was written", err))
	}
	if op.Kind == "add" {
		if stop, err := h.entryRow(ctx, op, intended); stop || err != nil {
			return err
		}
	}
	if err := h.renameAttempt(ctx, op, "write:managed", path, nb, "rename"); err != nil {
		return err
	}
	return h.setOpPhase(ctx, op, op.nextPhase("writing"), "", "")
}

// renameAttempt replaces path with nb by one rename, with a file attempt of call (write:managed or
// write:lock) whose intended hash is the hash of nb. The caller holds both plugin command flocks.
// The fault points are plugin-op-before-<fault> and plugin-op-after-<fault>.
func (h *Hub) renameAttempt(ctx context.Context, op *pluginOp, call, path string, nb []byte, fault string) error {
	out := hashBytes(nb)
	attempt, err := h.beginFileAttempt(ctx, op, call, out)
	if err != nil {
		return err
	}
	if call == "write:managed" {
		op.Output = out
	}
	if h.opFault("plugin-op-before-" + fault) {
		return errOpCrash
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE plugin_op_attempts SET sent_at = ? WHERE attempt = ?`, time.Now().UTC().Format(time.RFC3339Nano), attempt); err != nil {
		return err // no rename without sent_at
	}
	if err := replaceFile(path, nb); err != nil {
		var re *replacedError
		if !errors.As(err, &re) {
			// The writer returned, under both flocks: the file shows whether the rename happened.
			proof, outcome := "not_applied: the writer returned, and the file is not its output", "not written: "+err.Error()
			if readBack(call, path) == out {
				proof, outcome = "applied: the writer returned, and the file is its output", "written"
			}
			if e := h.finishFileAttempt(ctx, attempt, outcome, proof); e != nil {
				return e
			}
			return err
		}
		log.Printf("plugin op %s: %v (the new file is in place)", op.ID, err)
	}
	if h.opFault("plugin-op-after-" + fault) {
		return errOpCrash
	}
	return h.finishFileAttempt(ctx, attempt, "written", "applied: the rename returned")
}

// beginFileAttempt stores a new file attempt of call with the intended hash out, before the
// rename. For write:managed, the op's output hash is stored in the same transaction (opWrite's
// recovery reads it).
func (h *Hub) beginFileAttempt(ctx context.Context, op *pluginOp, call, out string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	attempt := hex.EncodeToString(b)
	if !slices.Contains(opAttemptCalls, call) {
		return "", fmt.Errorf("the attempt call %q is not in opAttemptCalls", call)
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := time.Now()
	if call == "write:managed" {
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_ops SET output = ?, updated = ? WHERE op_id = ?`, out, now.UnixMilli(), op.ID); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_op_attempts (attempt, op_id, call, at, bound_at, intended) VALUES (?, ?, ?, ?, ?, ?)`,
		attempt, op.ID, call, now.UnixMilli(), now.UTC().Format(time.RFC3339Nano), out); err != nil {
		return "", err
	}
	return attempt, tx.Commit()
}

func (h *Hub) finishFileAttempt(ctx context.Context, attempt, outcome, proof string) error {
	_, err := h.db.ExecContext(ctx, `UPDATE plugin_op_attempts SET outcome = ?, proof = ? WHERE attempt = ? AND outcome = ''`, outcome, proof, attempt)
	return err
}

// settleFileAttempt ends the op's open file attempt of call, which a hub stop interrupted (the writer cannot
// act any more), from the file b that the caller read under both flocks:
//   - no sent_at: the rename never ran: not_applied;
//   - the file hash is the intended hash: applied;
//   - else unresolved: the rename may have run, and another writer changed the file later.
//
// The op's own recovery (opWrite) then decides its phase, as before.
func (h *Hub) settleFileAttempt(ctx context.Context, op *pluginOp, call string, b []byte) error {
	var attempt, sent, intended string
	err := h.db.QueryRowContext(ctx, `SELECT attempt, sent_at, intended FROM plugin_op_attempts WHERE op_id = ? AND call = ? AND outcome = ''`, op.ID, call).
		Scan(&attempt, &sent, &intended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case sent == "":
		return h.finishFileAttempt(ctx, attempt, "not written: the hub stopped before the rename", "not_applied: no rename was sent")
	case b != nil && hashBytes(b) == intended:
		return h.finishFileAttempt(ctx, attempt, "written (observed after the hub restarted)", "applied: the file is the intended output")
	}
	return h.finishFileAttempt(ctx, attempt, "unknown: the file is not the intended output", "")
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
	var attempt, outcome, stored string
	err := h.db.QueryRowContext(ctx, `SELECT attempt, outcome, proof FROM plugin_op_attempts WHERE op_id = ? AND call = ?`, op.ID, call).Scan(&attempt, &outcome, &stored)
	switch {
	case err == nil && outcome == "":
		return h.opObserve(ctx, op, attempt, call)
	case err == nil && call == "replace" && !strings.HasPrefix(stored, "applied"):
		// finishCall stores a call's outcome and the op's phase in one transaction, so this is a
		// lost phase write. Only an applied replace goes on: any other stored outcome is unknown,
		// never a fallback or a second replace (C5 plan r2 B).
		return h.setOpPhase(ctx, op, "unknown", "", "the replace's outcome is stored without its phase: "+outcome)
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
	// The attempt stores the expect_run that a start or a replace sends (opRun); the other calls
	// store the op's target.
	expectRun := op.Target
	if call == "start" || call == "replace" {
		x, err := opRun(op)
		if err != nil {
			return h.setOpPhase(ctx, op, "failed", "", call+": "+err.Error())
		}
		b, _ := json.Marshal(x)
		expectRun = string(b)
	}
	// The start and stop handlers bind the attempt (target_instance, target_block, intent_id) and
	// commit sent_at before their first effect (adminattempt.go).
	if !slices.Contains(opAttemptCalls, call) {
		return fmt.Errorf("the attempt call %q is not in opAttemptCalls", call)
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO plugin_op_attempts (attempt, op_id, call, expect_run, at) VALUES (?, ?, ?, ?, ?)`,
		attempt, op.ID, call, expectRun, time.Now().UnixMilli()); err != nil {
		return err
	}
	var res any
	var callErr error
	switch call {
	case "start":
		res, callErr = h.opStart(ctx, op, attempt)
	case "stop":
		res, callErr = h.opStop(ctx, op, attempt)
	case "replace":
		res, callErr = h.opReplace(ctx, op, attempt)
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
	proof := ""
	if call == "reload" {
		proof = reloadProof(callErr)
	} else {
		var err error
		if proof, err = h.returnProof(ctx, call, attempt, callErr); err != nil {
			return err
		}
	}
	if callErr != nil {
		phase, result, errText = "failed", "", callErr.Error()
		if isRefusal(callErr, "candidate_changed") || isRefusal(callErr, "entry_changed") || isRefusal(callErr, "instance_changed") {
			phase = "conflict"
		}
		if proof == "" {
			phase = "unknown" // the call can have had its effect
		}
		// An update's replace that refused no_instance before its own effect: the op picks once
		// more, a start or no call (C5 plan r2 B). The phase is stored with the outcome.
		if call == "replace" && op.Kind == "update" && isRefusal(callErr, "no_instance") && strings.HasPrefix(proof, "not_applied") {
			phase = "fallback"
		}
	}
	if call == "reload" && callErr == nil {
		if msg := h.reloadEntryError(res, op); msg != "" {
			errText = msg
		}
	}
	return h.finishCall(ctx, op, attempt, phase, result, errText, proof)
}

// reloadProof is the proof of a reload's return: a success is applied; "invalid" (a malformed
// file) changes no entry, so it is not_applied; any other error is unresolved.
func reloadProof(callErr error) string {
	switch {
	case callErr == nil:
		return "applied: the reload returned"
	case isRefusal(callErr, "invalid"):
		return "not_applied: the reload refused the file, and the managed entries are unchanged"
	}
	return ""
}

// afterSentNone are the refusals of each call that come after sent_at and before the call's own
// effect: the call retired, spawned and saved nothing. For replace, the resolution of earlier
// intents can come before them; it belongs to those intents, not to this attempt.
// A code that the call can also return after its own effect is not in the list: replace returns
// "stopped" before its retire and after it (retire marked the record stopped).
var afterSentNone = map[string][]string{
	"start":   {"refused", "candidate_changed", "dependency_not_ready", "unresolved_intent"},
	"stop":    {},
	"replace": {"unresolved_intent", "no_instance", "instance_changed", "refused", "candidate_changed", "dependency_not_ready"},
}

// returnProof is the proof that the return of a bound call gives (docs/hub.md, "attempt"):
//   - a success: applied;
//   - any return while sent_at is empty: not_applied (no effect began);
//   - after sent_at, a refusal of afterSentNone: not_applied;
//   - else empty: unresolved, until an observation or an operator.
//
// attempt_bound proves nothing: the call did nothing, and the attempt keeps its own state.
func (h *Hub) returnProof(ctx context.Context, call, attempt string, callErr error) (string, error) {
	if callErr == nil {
		return "applied: the call returned", nil
	}
	if isRefusal(callErr, "attempt_bound") {
		return "", nil
	}
	var sent string
	if err := h.db.QueryRowContext(ctx, `SELECT sent_at FROM plugin_op_attempts WHERE attempt = ?`, attempt).Scan(&sent); err != nil {
		return "", err
	}
	if sent == "" {
		return "not_applied: the call returned before its first effect", nil
	}
	for _, code := range afterSentNone[call] {
		if isRefusal(callErr, code) {
			return "not_applied: the call refused (" + code + ") before its own effect", nil
		}
	}
	return "", nil
}

// finishCall stores the outcome of the op's open attempt and the op's next phase in one
// transaction.
func (h *Hub) finishCall(ctx context.Context, op *pluginOp, attempt, phase, result, errText, proof string) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored := result
	if errText != "" {
		stored = "error: " + errText
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_op_attempts SET outcome = ?, proof = ? WHERE attempt = ?`, stored, proof, attempt); err != nil {
		return err
	}
	lockOld, note := op.endRecord(phase)
	result, errText = withEndNote(phase, result, errText, note)
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_ops SET phase = ?, result = ?, error = ?, lock_old = ?, updated = ? WHERE op_id = ?`,
		phase, result, errText, lockOld, time.Now().UnixMilli(), op.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if h.opFault("plugin-op-after-finish") {
		return errOpCrash // a test: the hub stops right after the outcome and the phase are stored
	}
	op.Phase, op.Result, op.Error, op.LockOld = phase, result, errText, lockOld
	h.ev.emit("plugin_op", map[string]any{"op_id": op.ID, "kind": op.Kind, "entry": op.Entry, "phase": phase, "result": result, "error": errText})
	h.changed("plugins")
	return nil
}

func opResultText(call string, res any) string {
	if res == nil {
		return call + ": done"
	}
	if s, ok := res.(opText); ok {
		return string(s)
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

// entryChanged is the op's conflict when its target is bound to another entry than expect (the
// op's opExpectEntry). This check only saves a call: the admin handler checks expect_entry again
// under its locks.
func entryChanged(op *pluginOp, expect, now string) error {
	return refuse("entry_changed", "%s expects entry %s; the target is bound to %s now; nothing was called", op.Entry, expect, now)
}

// opExpectEntry is the entry identity that the op's lifecycle calls expect (C5 plan r1 item 1).
// An add or an update calls after its managed write, so it expects the intended entry (for an add,
// that is also the entry at admission). Every other op expects the entry at admission.
func opExpectEntry(op *pluginOp) (string, error) {
	if op.Kind != "add" && op.Kind != "update" {
		return op.EntryIdentity, nil
	}
	var t opResolved
	if err := json.Unmarshal([]byte(op.Target), &t); err != nil {
		return "", fmt.Errorf("the stored target: %v", err)
	}
	m, err := op.newManaged(t)
	if err != nil {
		return "", err
	}
	return entryIdentity(&configEntry{managed: m.ID, Source: m.Source}), nil
}

// opRun is the expect_run of the op's start and replace calls (C5 plan r1 item 1): for an add or
// an update, the pin at the target source (source, commit and digest); for a restart, its stored
// run; for a start, its stored expect_run. A local source has no commit and no digest: a local
// restart drops the snapshot digest of its stored run.
func opRun(op *pluginOp) (map[string]any, error) {
	var x map[string]any
	switch op.Kind {
	case "add", "update", "install":
		var t opResolved
		var pin lockEntry
		if json.Unmarshal([]byte(op.Target), &t) != nil || json.Unmarshal([]byte(op.Pin), &pin) != nil {
			return nil, errors.New("the stored target or pin does not read")
		}
		x = map[string]any{"source": t.Source, "commit": pin.Commit, "digest": pin.Digest}
	case "restart":
		var r opRestartTarget
		if err := json.Unmarshal([]byte(op.Target), &r); err != nil {
			return nil, fmt.Errorf("the stored target: %v", err)
		}
		x = map[string]any{"source": r.Source, "commit": r.Commit, "digest": r.Digest}
	default:
		if err := json.Unmarshal([]byte(op.Target), &x); err != nil {
			return nil, fmt.Errorf("the stored target: %v", err)
		}
	}
	for k, v := range x {
		if v == "" {
			delete(x, k)
		}
	}
	if s, _ := x["source"].(string); !strings.HasPrefix(s, "github:") {
		// A local run stores its snapshot digest (P1), but the expect_run of a local source is the
		// source only (parseExpectRun).
		delete(x, "commit")
		delete(x, "digest")
	}
	return x, nil
}

// opStart starts the op's entry with the op's expect_run and expect_entry: nothing else than that
// run of that entry starts.
func (h *Hub) opStart(ctx context.Context, op *pluginOp, attempt string) (any, error) {
	expect, err := opExpectEntry(op)
	if err != nil {
		return nil, err
	}
	if _, now := h.opEntryNow(op); now != expect {
		return nil, entryChanged(op, expect, now)
	}
	x, err := opRun(op)
	if err != nil {
		return nil, err
	}
	args := map[string]any{"expect_run": x, "expect_entry": expect, "attempt": attempt}
	if op.Name != "" {
		args["plugin"] = op.Name
	} else {
		args["source"] = x["source"]
	}
	return opAdmin(ctx, h, "hub.admin.start", args)
}

// opReplace replaces the instance of the op's plugin with the op's run (expect_run) of the op's
// entry (expect_entry). A restart names the instance that it stored at admission; an update names
// the instance that the record owns now (the handler checks it again under its locks:
// instance_changed). A record with no owned instance is no_instance, before the attempt is bound.
func (h *Hub) opReplace(ctx context.Context, op *pluginOp, attempt string) (any, error) {
	expect, err := opExpectEntry(op)
	if err != nil {
		return nil, err
	}
	pl, now := h.opEntryNow(op)
	if now != expect {
		return nil, entryChanged(op, expect, now)
	}
	x, err := opRun(op)
	if err != nil {
		return nil, err
	}
	var instance string
	if op.Kind == "restart" {
		var r opRestartTarget
		if err := json.Unmarshal([]byte(op.Target), &r); err != nil {
			return nil, fmt.Errorf("the stored target: %v", err)
		}
		instance = r.Instance
	}
	var same bool // the instance runs the pin's code under another source (hub lead, lead-caseb)
	if op.Kind != "restart" && pl != nil {
		h.mu.Lock()
		owned, pending := h.ownedInstance(pl)
		run := pl.run
		h.mu.Unlock()
		if !pending {
			instance = owned
		}
		same = run.Commit != "" && run.Commit == x["commit"] && run.Digest == x["digest"] && run.Source != x["source"]
	}
	if instance == "" {
		return nil, refuse("no_instance", "plugin %s has no instance; nothing was called", op.Name)
	}
	res, err := opAdmin(ctx, h, "hub.admin.replace", map[string]any{"plugin": op.Name, "expect_instance": instance, "expect_run": x, "expect_entry": expect, "attempt": attempt})
	if err == nil && same {
		// A run belongs to its entry: the same code runs again under the new source.
		return opText(fmt.Sprintf("replaced: the same commit %s now runs as %s", x["commit"], x["source"])), nil
	}
	return res, err
}

// opText is a call result that is its own end text (opResultText).
type opText string

// opStop stops the op's plugin, only while it is bound to the op's entry. An entry without a record
// has nothing to stop.
func (h *Hub) opStop(ctx context.Context, op *pluginOp, attempt string) (any, error) {
	pl, now := h.opEntryNow(op)
	if op.Name == "" || pl == nil {
		return map[string]any{"desired": "stopped", "note": "no instance"}, nil
	}
	expect, err := opExpectEntry(op)
	if err != nil {
		return nil, err
	}
	if now != expect {
		return nil, entryChanged(op, expect, now)
	}
	return opAdmin(ctx, h, "hub.admin.stop", map[string]any{"plugin": op.Name, "expect_entry": expect, "attempt": attempt})
}

// opObserve ends an op whose call has an attempt without an outcome. The manager does not call
// again: it reads what the hub knows. It runs only when the call cannot act any more: the call
// returned in this process (a test fault), or the hub restarted. The proof follows the binding
// of the start and stop handlers (adminattempt.go):
//   - bound_at is empty (the handler never bound it) or sent_at is empty (no effect began): not_applied;
//     the op failed, and nothing was changed by it.
//   - stop: the stored record is stopped with no instance, its saved entry identity is the op's,
//     and its instance_id is the target_instance that the handler bound -> applied (the same
//     effect from any caller); else unresolved. A row without an identity (from before it was
//     recorded), with another entry's, or with a later instance (a start) is never a match.
//   - start: the spawn intent intent_id is created (its block has the instance marker), the record
//     has that instance with the op's entry identity, and the instance registered ->
//     applied; else unresolved. An absent block never proves not_applied: Rex can complete a
//     create after its client is gone.
//   - reload: the hub's entries have the op's intended entry -> done (the hub start or the call read
//     the file); else unknown. Its proof comes with the file attempts.
func (h *Hub) opObserve(ctx context.Context, op *pluginOp, attempt, call string) error {
	if call == "reload" {
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
			return h.finishCall(ctx, op, attempt, op.nextPhase(op.Phase), "reload: the hub has the new entry (observed after the hub restarted)", "", "applied: the hub's entries have the op's entry (the start-up load of the file)")
		}
		return h.finishCall(ctx, op, attempt, "unknown", "", fmt.Sprintf("the reload's outcome is unknown: the hub has %s", now), "")
	}
	var bound, sent string
	var t attemptTarget
	if err := h.db.QueryRowContext(ctx, `SELECT bound_at, sent_at, target_instance, target_block, target_session, intent_id FROM plugin_op_attempts WHERE attempt = ?`, attempt).
		Scan(&bound, &sent, &t.instance, &t.block, &t.session, &t.intent); err != nil {
		return err
	}
	if bound == "" || sent == "" {
		how := "the call never bound the attempt"
		if bound != "" {
			how = "the call stopped before its first effect"
		}
		return h.finishCall(ctx, op, attempt, "failed", "", "the "+call+" did not run: "+how+" (observed after the hub restarted)", "not_applied: "+how)
	}
	proof, detail, waiting, err := h.attemptEvidence(ctx, op, call, t)
	if err != nil {
		return err
	}
	if proof == "" && waiting {
		// The registration deadline decides only the op's phase: until it, the op waits for the
		// registration of its intent's instance; after it, the op ends unknown, and observeLater
		// keeps the observation of the attempt.
		if h.pops.observing == nil {
			h.pops.observing = map[string]time.Time{}
		}
		first, ok := h.pops.observing[attempt]
		if !ok {
			first = time.Now()
			h.pops.observing[attempt] = first
		}
		if time.Since(first) < registerWait {
			return &opWait{after: opRegisterPoll, why: "waiting for the registration of " + t.intent}
		}
		detail += fmt.Sprintf(" The hub waited %v for the registration; it keeps the observation.", registerWait)
	}
	delete(h.pops.observing, attempt)
	if proof != "" {
		return h.finishCall(ctx, op, attempt, op.nextPhase(op.Phase), call+": "+detail+" (observed after the hub restarted)", "", proof)
	}
	return h.finishCall(ctx, op, attempt, "unknown", "", "the "+call+"'s outcome is unknown: "+detail, "")
}

// attemptEvidence is the observation of a sent start or stop attempt from stored identity only
// (opObserve): an applied proof with what it saw, or "" with what it saw instead. The caller makes
// sure that the call cannot act any more.
//
// waiting: a start whose intent can still register (not abandoned); the observation can wait.
func (h *Hub) attemptEvidence(ctx context.Context, op *pluginOp, call string, t attemptTarget) (proof, detail string, waiting bool, err error) {
	target, intent := t.instance, t.intent
	var desired, block, identity, instance string
	err = h.db.QueryRowContext(ctx, `SELECT desired, block_id, entry_identity, instance_id FROM plugins WHERE name = ?`, op.Name).
		Scan(&desired, &block, &identity, &instance)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "the plugin has no record", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	// The entry that the call expected (opExpectEntry): an update's call names the entry after its
	// write, not the entry at admission.
	expect, err := opExpectEntry(op)
	if err != nil {
		return "", "", false, err
	}
	mine := expect != "" && identity == expect
	if call == "stop" {
		if mine && instance == target && desired == "stopped" && block == "" {
			// The row alone is no proof: retire saves the cleared row before block.close, and it
			// ignores a close failure. Here a missing block is the goal of the call, so a complete
			// read of the stored session without target_block proves the stop.
			if t.block == "" {
				return "applied: observed the stored record (no block was bound)", "stopped", false, nil
			}
			if t.session == "" {
				return "", fmt.Sprintf("the stop's block %s has no stored session, so the hub cannot read it", t.block), false, nil
			}
			blocks, err := opSessionBlocks(ctx, h, t.session)
			switch {
			case sessionGone(err):
				return "applied: observed the stored record, and session " + t.session + " does not exist", "stopped", false, nil
			case err != nil:
				return "", fmt.Sprintf("the read of session %s failed (%v); the hub reads it again", t.session, err), false, nil
			case blocks[t.block]:
				return "", fmt.Sprintf("block %s is still in session %s", t.block, t.session), false, nil
			}
			return "applied: observed the stored record, and block " + t.block + " is not in session " + t.session, "stopped", false, nil
		}
		return "", fmt.Sprintf("the record is %s with block %q and instance %q, saved with entry %q; the stop was for instance %q of entry %s", desired, block, instance, identity, target, expect), false, nil
	}
	var status string
	if intent != "" {
		err := h.db.QueryRowContext(ctx, `SELECT status FROM spawn_intents WHERE instance_id = ?`, intent).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", "", false, err
		}
	}
	h.mu.Lock()
	pl := h.plugins[op.Name]
	registered := pl != nil && pl.InstanceID == intent && h.registeredLocked(op.Name)
	h.mu.Unlock()
	if intent != "" && status == "created" && mine && instance == intent && registered {
		return "applied: observed the created intent and its registration", "running", false, nil
	}
	return "", fmt.Sprintf("the start's intent is %q with status %q, and it has not registered. A new start runs only after the hub resolves the spawn intent (unresolved_intent).", intent, status), intent != "" && status != "abandoned", nil
}

// opAttemptCalls are the calls of plugin_op_attempts: the lifecycle calls (opCall) and the file
// writes (renameAttempt). An attempt of another call is not stored. observeLater looks for Rex
// evidence of the sent ones of opEvidenceCalls; it counts the passes of every call.
var (
	opAttemptCalls  = []string{"start", "stop", "replace", "reload", "write:managed", "write:lock", "write:lock-old"}
	opEvidenceCalls = []string{"start", "stop", "replace"}
)

// observeLater observes the unproved attempts of every ended op, of every call. It proves a sent
// start, stop or replace attempt when exact evidence appears later: for example, the instance of
// a start's created intent registers after the op ended unknown (a lost answer, or a hub restart
// before the registration). The op has ended, so its calls cannot act any more. It never calls
// again and never changes the op's phase: only the attempt's proof and outcome (docs/hub.md,
// "attempt").
//
// The observation is bounded. An attempt without a proof after opObservePasses passes, at least
// opObservePoll apart, ends unproven: its outcome gets the prefix "unproven:" in the one UPDATE that
// ends the observation. A reload or a file write has nothing to observe in Rex, and ends unproven
// after the same passes. addRow counts an unproven attempt as settled, so the plugin name is free
// again. A late effect of an old start is a recorded instance, and the next start retires it. A
// reload and a file write run in the hub process and end before their outcome is stored, so
// they have no late effect. An open bound attempt (bound_at, no outcome) is not observed: its
// handler can still act.
//
// It returns the number of attempts that stay unresolved: the manager passes again after
// opObservePoll while there are any.
func (h *Hub) observeLater(ctx context.Context) (left int) {
	rows, err := h.db.QueryContext(ctx, `SELECT a.attempt, a.op_id, a.call, a.sent_at, a.target_instance, a.target_block, a.target_session, a.intent_id `+laterAttempts)
	if err != nil {
		log.Printf("plugin ops: the later observation: %v", err)
		return 0
	}
	type open struct {
		attempt, op, call, sent string
		t                       attemptTarget
	}
	var list []open
	for rows.Next() {
		var a open
		if rows.Scan(&a.attempt, &a.op, &a.call, &a.sent, &a.t.instance, &a.t.block, &a.t.session, &a.t.intent) == nil {
			list = append(list, a)
		}
	}
	rows.Close()
	if h.pops.later == nil {
		h.pops.later = map[string]laterPasses{}
	}
	now := time.Now()
	listed := map[string]bool{}
	for _, a := range list {
		listed[a.attempt] = true
		op, err := h.loadOp(ctx, a.op)
		if err != nil {
			continue
		}
		proof := ""
		if a.sent != "" && slices.Contains(opEvidenceCalls, a.call) {
			if proof, _, _, err = h.attemptEvidence(ctx, op, a.call, a.t); err != nil {
				proof = ""
			}
		}
		if proof != "" {
			res, err := h.db.ExecContext(ctx, `UPDATE plugin_op_attempts SET proof = ? WHERE attempt = ? AND proof = ''`, proof+" (later)", a.attempt)
			if err != nil {
				left++
				continue
			}
			if n, _ := res.RowsAffected(); n == 1 {
				h.ev.emit("plugin_op_attempt", map[string]any{"op_id": a.op, "attempt": a.attempt, "call": a.call, "proof": proof + " (later)"})
				h.changed("plugins")
			}
			continue
		}
		p := h.pops.later[a.attempt]
		if now.Sub(p.last) >= opObservePoll {
			p.n, p.last = p.n+1, now
		}
		h.pops.later[a.attempt] = p
		if p.n < opObservePasses {
			left++
			continue
		}
		res, err := h.db.ExecContext(ctx, `UPDATE plugin_op_attempts SET outcome = ? || outcome WHERE attempt = ? AND proof = '' AND outcome NOT LIKE 'unproven:%' AND (outcome != '' OR bound_at = '')`,
			unprovenPrefix, a.attempt)
		if err != nil {
			left++
			continue
		}
		delete(h.pops.later, a.attempt)
		if n, _ := res.RowsAffected(); n == 1 {
			h.ev.emit("plugin_op_attempt", map[string]any{"op_id": a.op, "attempt": a.attempt, "call": a.call, "outcome": "unproven"})
			h.changed("plugins")
		}
	}
	for k := range h.pops.later {
		if !listed[k] {
			delete(h.pops.later, k)
		}
	}
	return left
}

// laterAttempts selects the attempts of the later observation: no proof, not unproven, not an open
// bound attempt, of an ended op.
const laterAttempts = `FROM plugin_op_attempts a JOIN plugin_ops o ON o.op_id = a.op_id
	WHERE a.proof = '' AND a.outcome NOT LIKE 'unproven:%' AND (a.outcome != '' OR a.bound_at = '') AND o.phase IN ('done', 'failed', 'conflict', 'unknown')`

// laterPasses counts the passes of the later observation of one attempt.
type laterPasses struct {
	n    int
	last time.Time
}

// unprovenPrefix starts the outcome of an attempt whose later observation ended without a proof.
var unprovenPrefix = fmt.Sprintf("unproven: observation ended after %d passes; ", opObservePasses)

// settingsSchema is the agent-face schema of the settings input and of its expect.
var settingsSchema = map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
	"args":        map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
	"watch_roots": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}

// opInputSchema is the agent-face schema of the input: settings {args, watch_roots}, or update
// {to}. Admission decodes the input of each op strictly.
var opInputSchema = map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
	"args":        settingsSchema["properties"].(map[string]any)["args"],
	"watch_roots": settingsSchema["properties"].(map[string]any)["watch_roots"],
	"to":          map[string]any{"type": "string", "minLength": 1}}}
