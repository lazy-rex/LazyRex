package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// opsAttemptRow inserts an attempt row of call, as opCall does before its call.
func opsAttemptRow(t *testing.T, h *Hub, attempt, call string) {
	t.Helper()
	if _, err := h.db.Exec(`INSERT INTO plugin_op_attempts (attempt, op_id, call, at) VALUES (?, 'op', ?, 1)`, attempt, call); err != nil {
		t.Fatal(err)
	}
}

type attemptState struct{ bound, sent, target, block, intent string }

func opsAttemptState(t *testing.T, h *Hub, attempt string) attemptState {
	t.Helper()
	var s attemptState
	err := h.db.QueryRow(`SELECT bound_at, sent_at, target_instance, target_block, intent_id FROM plugin_op_attempts WHERE attempt = ?`, attempt).
		Scan(&s.bound, &s.sent, &s.target, &s.block, &s.intent)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rowCount(t *testing.T, h *Hub, name string) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(`SELECT count(*) FROM plugins WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The stop handler binds an attempt once, with the record's (empty) instance, and commits sent_at.
// A replay, an attempt of another call and an unknown attempt refuse with attempt_bound and do
// nothing; the first attempt keeps its state.
func TestAdminAttemptBindsOnce(t *testing.T) {
	h, pl, _ := adminEntryHub(t)
	ctx := context.Background()
	opsAttemptRow(t, h, "a1", "stop")
	if _, err := h.adminAction(ctx, "hub.admin.stop", map[string]any{"plugin": "x", "attempt": "a1"}); err != nil {
		t.Fatal(err)
	}
	first := opsAttemptState(t, h, "a1")
	if first.bound == "" || first.sent == "" || first.target != "" || first.block != "" {
		t.Fatalf("the stop of a record without an instance: %+v", first)
	}
	if _, err := h.adminAction(ctx, "hub.admin.stop", map[string]any{"plugin": "x", "attempt": "a1"}); !isRefusal(err, "attempt_bound") || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("a replay: %v", err)
	}
	if got := opsAttemptState(t, h, "a1"); got != first {
		t.Fatalf("the replay changed the first attempt: %+v, was %+v", got, first)
	}

	pl.Desired = "running"
	if err := h.savePlugin(ctx, pl); err != nil {
		t.Fatal(err)
	}
	opsAttemptRow(t, h, "a2", "start")
	if _, err := h.adminAction(ctx, "hub.admin.stop", map[string]any{"plugin": "x", "attempt": "a2"}); !isRefusal(err, "attempt_bound") || !strings.Contains(err.Error(), "is for start") {
		t.Fatalf("a start attempt on a stop: %v", err)
	}
	if got := opsAttemptState(t, h, "a2"); got.bound != "" || got.sent != "" {
		t.Fatalf("the stop bound a start attempt: %+v", got)
	}
	if _, err := h.adminAction(ctx, "hub.admin.stop", map[string]any{"plugin": "x", "attempt": "nope"}); !isRefusal(err, "attempt_bound") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("an unknown attempt: %v", err)
	}
	if err := h.readPluginRow(ctx, pl); err != nil || pl.Desired != "running" {
		t.Fatalf("a refused stop changed the record: desired %s, %v", pl.Desired, err)
	}
	for _, v := range []any{"", 7.0, true} {
		if _, err := h.adminAction(ctx, "hub.admin.stop", map[string]any{"plugin": "x", "attempt": v}); !isRefusal(err, "invalid_input") {
			t.Errorf("attempt %#v: %v", v, err)
		}
	}
}

// A refusal before sent_at writes nothing: the row read inserts no row, and the attempt is bound
// with an empty sent_at, so the return proves not_applied.
func TestAdminAttemptRefusalBeforeEffect(t *testing.T) {
	h, pl, _ := adminEntryHub(t)
	ctx := context.Background()
	h.plugins["n"] = &Plugin{Name: "n", entry: pl.entry}
	opsAttemptRow(t, h, "a1", "stop")
	_, err := h.adminAction(ctx, "hub.admin.stop", map[string]any{"plugin": "n", "attempt": "a1", "expect_entry": `["managed:other","s"]`})
	if !isRefusal(err, "entry_changed") {
		t.Fatalf("stop: %v", err)
	}
	opsAttemptRow(t, h, "a2", "start")
	h.plugins["k"] = &Plugin{Name: "k"} // a kept record: no entry
	if _, err := h.adminAction(ctx, "hub.admin.start", map[string]any{"plugin": "k", "attempt": "a2"}); err == nil || !strings.Contains(err.Error(), "no config entry") {
		t.Fatalf("start of a kept record: %v", err)
	}
	// candidate_changed of a running plugin: the record runs another run than expect_run.
	r := &Plugin{Name: "r", entry: pl.entry}
	h.plugins["r"] = r
	if err := h.loadPluginRow(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.ClientID, r.ChildPID, r.ChildStart, r.run = "client:r", os.Getpid(), startTime(os.Getpid()), pluginRun{Name: "r", Source: "/a"}
	if err := h.savePlugin(ctx, r); err != nil {
		t.Fatal(err)
	}
	opsAttemptRow(t, h, "a3", "start")
	if _, err := h.adminAction(ctx, "hub.admin.start", map[string]any{"plugin": "r", "attempt": "a3", "expect_run": map[string]any{"source": "/b"}}); !isRefusal(err, "candidate_changed") {
		t.Fatalf("start of a running plugin with another run: %v", err)
	}
	if s := opsAttemptState(t, h, "a3"); s.bound == "" || s.sent != "" {
		t.Errorf("attempt a3: %+v", s)
	}
	// An argument error and an unknown plugin come before the binding: the attempt stays unbound.
	opsAttemptRow(t, h, "a4", "start")
	for _, args := range []map[string]any{
		{"plugin": "x", "attempt": "a4", "expect_run": "no object"},
		{"plugin": "zz", "attempt": "a4"},
	} {
		if _, err := h.adminAction(ctx, "hub.admin.start", args); err == nil {
			t.Fatalf("%v: no error", args)
		}
		if s := opsAttemptState(t, h, "a4"); s.bound != "" || s.sent != "" {
			t.Errorf("%v: attempt a4 %+v", args, s)
		}
	}
	for name, a := range map[string]string{"n": "a1", "k": "a2"} {
		if n := rowCount(t, h, name); n != 0 {
			t.Errorf("the refusal inserted the row of %s", name)
		}
		if s := opsAttemptState(t, h, a); s.bound == "" || s.sent != "" {
			t.Errorf("attempt %s: %+v", a, s)
		}
		proof, err := h.returnProof(ctx, "stop", a, errors.New("any return"))
		if err != nil || !strings.HasPrefix(proof, "not_applied") {
			t.Errorf("attempt %s: the proof %q, %v", a, proof, err)
		}
	}
}

// The non-inserting row read and loadPluginRow give the same values for an absent row.
func TestReadPluginRowDefaults(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	ctx := context.Background()
	read, loaded := &Plugin{Name: "d"}, &Plugin{Name: "d"}
	if err := h.readPluginRow(ctx, read); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, h, "d"); n != 0 {
		t.Fatalf("the read inserted a row")
	}
	if err := h.loadPluginRow(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	type row struct {
		desired, instance, session, block string
		pid                               int
		start                             string
		run                               pluginRun
	}
	r := row{read.Desired, read.InstanceID, read.SessionID, read.BlockID, read.ChildPID, read.ChildStart, read.run}
	l := row{loaded.Desired, loaded.InstanceID, loaded.SessionID, loaded.BlockID, loaded.ChildPID, loaded.ChildStart, loaded.run}
	if r.desired != defaultDesired || r.instance != l.instance || r.desired != l.desired || r.session != l.session || r.block != l.block || r.pid != l.pid || r.start != l.start || r.run.Name != l.run.Name || r.run.Source != l.run.Source {
		t.Fatalf("read %+v, loaded %+v", r, l)
	}
}

// A hub stop inside the real stop handler: between the binding and sent_at, the attempt is
// not_applied (the op failed, and the record is not stopped); between sent_at and the first
// effect, it stays unresolved (the op is unknown), though nothing happened.
func TestPluginOpAttemptFaults(t *testing.T) {
	for _, c := range []struct {
		point, phase, proof string
	}{
		{"admin-before-sent", "failed", "not_applied"},
		{"admin-after-sent", "unknown", ""},
	} {
		t.Run(c.point, func(t *testing.T) {
			h, pl, _ := adminEntryHub(t)
			ctx := context.Background()
			opAdmin = func(ctx context.Context, h *Hub, action string, args map[string]any) (any, error) {
				return h.adminAction(ctx, action, args)
			}
			id := opsMust(t, h, "stop", opRequest{Op: "stop", Entry: "managed:x"})
			h.pops.fault = func(p string) bool { return p == c.point || p == "plugin-op-after-stop" }
			h.opsRun(ctx)
			h.pops.fault = nil
			h.opsRun(ctx) // the next hub observes
			op := opsOf(t, h, id)
			proof := opsProof(t, h, id, "stop")
			if op.Phase != c.phase || !strings.HasPrefix(proof, c.proof) || (c.proof == "" && proof != "") {
				t.Fatalf("op %+v, proof %q", op, proof)
			}
			if err := h.readPluginRow(ctx, pl); err != nil || pl.Desired != "running" {
				t.Fatalf("the record: desired %s, %v", pl.Desired, err)
			}
		})
	}
}

// The proof of a return: a success is applied; after sent_at, a refusal of afterSentNone is
// not_applied, and any other error leaves the attempt unresolved (phase unknown).
func TestPluginOpReturnProof(t *testing.T) {
	h, c, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	h.plugins["x"] = &Plugin{Name: "x", entry: opsEntry(t, h, "managed:x")}
	for _, k := range []struct {
		op, call string
		reply    error
		phase    string
		proof    string
	}{
		{"stop", "stop", nil, "done", "applied"},
		{"start", "start", refuse("candidate_changed", "the candidate is b"), "conflict", "not_applied"},
		{"start", "start", refuse("dependency_not_ready", "d"), "failed", "not_applied"},
		{"stop", "stop", errors.New("cannot verify the running instance: timeout"), "unknown", ""},
		{"start", "start", refuse("spawn_unknown", "may still create a block"), "unknown", ""},
	} {
		c.reply = func(string, map[string]any) (any, error) { return nil, k.reply }
		id := opsMust(t, h, k.op+k.phase+k.proof, opRequest{Op: k.op, Entry: "managed:x"})
		h.opsRun(context.Background())
		op := opsOf(t, h, id)
		proof := opsProof(t, h, id, k.call)
		if op.Phase != k.phase || !strings.HasPrefix(proof, k.proof) || (k.proof == "" && proof != "") {
			t.Errorf("%s with %v: op %+v, proof %q", k.op, k.reply, op, proof)
		}
	}
}

// After a hub stop during a start: only the created intent of the attempt, with the record's
// identity and its registration, proves applied. An intent that is still open (its block was not
// found) proves nothing: the op stays unknown, not not_applied.
func TestPluginOpStartObservation(t *testing.T) {
	wait := registerWait
	t.Cleanup(func() { registerWait = wait })
	registerWait = 0 // no wait for a registration: the first observation decides the phase
	for _, created := range []bool{true, false} {
		h, c, dir := opsTestHub(t, "")
		ctx := context.Background()
		writeManaged(t, dir, opsLocal(t, t.TempDir(), "y"))
		opsLoadEntries(t, h)
		pl := &Plugin{Name: "y", entry: opsEntry(t, h, "managed:y")}
		h.plugins["y"] = pl
		if err := h.loadPluginRow(ctx, pl); err != nil {
			t.Fatal(err)
		}
		c.reply = func(_ string, args map[string]any) (any, error) {
			// The handler's effect: the intent with the attempt's intent_id, then the run.
			pl.opAttempt = args["attempt"].(string)
			defer func() { pl.opAttempt = "" }()
			if err := h.insertIntent(ctx, pl, "y-1-aaaaaaaa", "session:home"); err != nil {
				return nil, err
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			pl.InstanceID, pl.SessionID = "y-1-aaaaaaaa", "session:home"
			return nil, h.savePlugin(ctx, pl)
		}
		id := opsMust(t, h, "start", opRequest{Op: "start", Entry: "managed:y"})
		h.pops.fault = func(p string) bool { return p == "plugin-op-after-start" }
		h.opsRun(ctx)
		h.pops.fault = nil
		if s := opsAttemptStateOf(t, h, id, "start"); s.intent != "y-1-aaaaaaaa" {
			t.Fatalf("the intent of the attempt: %+v", s)
		}
		if created {
			// Recovery found the block with its marker, and the instance registered.
			if _, err := h.db.Exec(`UPDATE spawn_intents SET status = 'created', block_id = 'block:1' WHERE instance_id = 'y-1-aaaaaaaa'`); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			pl.BlockID, pl.ClientID, pl.ChildPID, pl.ChildStart = "block:1", "client:1", os.Getpid(), startTime(os.Getpid())
			err := h.savePlugin(ctx, pl)
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
		}
		h.opsRun(ctx)
		op := opsOf(t, h, id)
		proof := opsProof(t, h, id, "start")
		if created && (op.Phase != "done" || !strings.HasPrefix(proof, "applied")) {
			t.Errorf("a created and registered intent: op %+v, proof %q", op, proof)
		}
		if !created && (op.Phase != "unknown" || proof != "") {
			t.Errorf("an open intent: op %+v, proof %q", op, proof)
		}
	}
}

func opsAttemptStateOf(t *testing.T, h *Hub, id, call string) attemptState {
	t.Helper()
	var attempt string
	if err := h.db.QueryRow(`SELECT attempt FROM plugin_op_attempts WHERE op_id = ? AND call = ?`, id, call).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	return opsAttemptState(t, h, attempt)
}

// startOpHub is a hub with entry managed:y and a start op on it whose fake start inserts the
// attempt's intent and saves the run, then returns ret. The op ran once.
func startOpHub(t *testing.T, ret error, crash bool) (*Hub, *Plugin, string) {
	t.Helper()
	h, c, dir := opsTestHub(t, "")
	ctx := context.Background()
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "y"))
	opsLoadEntries(t, h)
	pl := &Plugin{Name: "y", entry: opsEntry(t, h, "managed:y")}
	h.plugins["y"] = pl
	if err := h.loadPluginRow(ctx, pl); err != nil {
		t.Fatal(err)
	}
	c.reply = func(_ string, args map[string]any) (any, error) {
		pl.opAttempt = args["attempt"].(string)
		defer func() { pl.opAttempt = "" }()
		if err := h.insertIntent(ctx, pl, "y-1-aaaaaaaa", "session:home"); err != nil {
			return nil, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		pl.InstanceID, pl.SessionID = "y-1-aaaaaaaa", "session:home"
		if err := h.savePlugin(ctx, pl); err != nil {
			return nil, err
		}
		return nil, ret
	}
	id := opsMust(t, h, "start", opRequest{Op: "start", Entry: "managed:y"})
	if crash {
		h.pops.fault = func(p string) bool { return p == "plugin-op-after-start" }
	}
	h.opsRun(ctx)
	h.pops.fault = nil
	return h, pl, id
}

// createAndRegister marks the intent created (its block was found) and registers its instance.
func createAndRegister(t *testing.T, h *Hub, pl *Plugin) {
	t.Helper()
	if _, err := h.db.Exec(`UPDATE spawn_intents SET status = 'created', block_id = 'block:1' WHERE instance_id = 'y-1-aaaaaaaa'`); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	pl.BlockID, pl.ClientID, pl.ChildPID, pl.ChildStart = "block:1", "client:1", os.Getpid(), startTime(os.Getpid())
	if err := h.savePlugin(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
}

// The registration deadline decides only the op's phase (lead-b8-late-obs):
//   - a registration before the deadline: proof applied, and the op ends done;
//   - after the deadline: the op ends unknown with no proof, and a later registration stores the
//     proof (later); the phase stays unknown, and nothing is called again;
//   - a start that returned spawn_unknown in this process (no crash): the same later proof;
//   - a resolved attempt (a proof that is not empty) does not change.
func TestPluginOpLateRegistration(t *testing.T) {
	wait := registerWait
	t.Cleanup(func() { registerWait = wait })
	ctx := context.Background()

	registerWait = time.Minute
	h, pl, id := startOpHub(t, nil, true)
	if w := h.opsRun(ctx); w <= 0 {
		t.Fatalf("the observation did not wait: %v; op %+v", w, opsOf(t, h, id))
	}
	if op := opsOf(t, h, id); op.Phase != "starting" {
		t.Fatalf("waiting: op %+v", op)
	}
	createAndRegister(t, h, pl)
	h.opsRun(ctx)
	if op, proof := opsOf(t, h, id), opsProof(t, h, id, "start"); op.Phase != "done" || !strings.HasPrefix(proof, "applied") {
		t.Fatalf("a registration before the deadline: op %+v, proof %q", op, proof)
	}

	registerWait = 0
	for _, c := range []struct {
		name  string
		ret   error
		crash bool
	}{
		{"after the deadline", nil, true},
		{"spawn_unknown", refuse("spawn_unknown", "may still create a block"), false},
	} {
		h, pl, id := startOpHub(t, c.ret, c.crash)
		h.opsRun(ctx)
		if op, proof := opsOf(t, h, id), opsProof(t, h, id, "start"); op.Phase != "unknown" || proof != "" {
			t.Fatalf("%s: op %+v, proof %q", c.name, op, proof)
		}
		createAndRegister(t, h, pl)
		h.opsRun(ctx)
		op, proof := opsOf(t, h, id), opsProof(t, h, id, "start")
		if op.Phase != "unknown" || !strings.HasPrefix(proof, "applied") || !strings.HasSuffix(proof, "(later)") {
			t.Errorf("%s, a later registration: op %+v, proof %q", c.name, op, proof)
		}
	}

	h, pl, id = startOpHub(t, refuse("spawn_unknown", "may still create a block"), false)
	// An operator resolution is its own field (resolved_by), and the proof stays empty.
	const by = `{"outcome":"not_applied","note":"the operator checked"}`
	if _, err := h.db.Exec(`UPDATE plugin_op_attempts SET resolved_by = ? WHERE op_id = ?`, by, id); err != nil {
		t.Fatal(err)
	}
	createAndRegister(t, h, pl)
	h.opsRun(ctx)
	var got string
	if err := h.db.QueryRow(`SELECT resolved_by FROM plugin_op_attempts WHERE op_id = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if proof := opsProof(t, h, id, "start"); proof != "" || got != by {
		t.Errorf("a resolved attempt changed: proof %q, resolved_by %q", proof, got)
	}
}

// A stop observation proves applied only from a complete read of the stored session without the
// bound block (codex-27, plugins-2d20d80-target-block-rejection-1): the stopped row alone is no
// proof, because retire saves it before block.close and ignores a close failure.
//   - the block is still in the session: unresolved;
//   - the read fails: unresolved, and a later pass reads again;
//   - the session does not exist (session_not_found): applied;
//   - a complete read without the block: applied.
func TestPluginOpStopObservationReadsTargetBlock(t *testing.T) {
	for _, c := range []struct {
		name   string
		blocks map[string]bool
		err    error
		phase  string
		proof  string
	}{
		{"the block is still there", map[string]bool{"block:original": true}, nil, "unknown", ""},
		{"a failed read", nil, errors.New("transport closed"), "unknown", ""},
		{"no session", nil, &rex.RexError{Code: "session_not_found"}, "done", "applied"},
		{"a complete read without the block", map[string]bool{"block:other": true}, nil, "done", "applied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, pl, _ := adminEntryHub(t)
			ctx := context.Background()
			h.mu.Lock()
			pl.InstanceID, pl.SessionID, pl.BlockID = "x-1-aaaaaaaa", "session:home", "block:original"
			err := h.savePlugin(ctx, pl)
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			var reads []string
			opSessionBlocks = func(_ context.Context, _ *Hub, sess string) (map[string]bool, error) {
				reads = append(reads, sess)
				return c.blocks, c.err
			}
			id := opsMust(t, h, "stop", opRequest{Op: "stop", Entry: "managed:x"})
			h.pops.fault = func(p string) bool { return p == "plugin-op-after-stop" }
			h.opsRun(ctx)
			h.pops.fault = nil
			// The stop's effect on the row (retire, then the stopped state): the block may remain.
			h.mu.Lock()
			pl.Desired, pl.BlockID, pl.SessionID = "stopped", "", ""
			err = h.savePlugin(ctx, pl)
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			h.opsRun(ctx)
			op, proof := opsOf(t, h, id), opsProof(t, h, id, "stop")
			if op.Phase != c.phase || !strings.HasPrefix(proof, c.proof) || (c.proof == "" && proof != "") {
				t.Fatalf("op %+v, proof %q", op, proof)
			}
			if len(reads) == 0 || reads[0] != "session:home" {
				t.Fatalf("the reads %v; want the bound session, not the row's", reads)
			}
		})
	}
}

// A replace refuses "stopped" on two paths: before its retire (the record is stopped; nothing was
// done) and after it (retire found the block gone and marked the record stopped: an effect). The
// code alone cannot tell them apart, so after sent_at it proves nothing (codex-27, 2d20d80).
func TestReplaceStoppedAfterSentIsUnresolved(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	opsAttemptRow(t, h, "r1", "replace")
	if err := h.bindAttempt(context.Background(), "r1", "replace", "a-1-x", "block:1", "session:home"); err != nil {
		t.Fatal(err)
	}
	if err := h.attemptSent(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	refusal := refuse("stopped", "plugin a: the block of instance a-1-x was closed; the plugin is stopped")
	if proof, err := h.returnProof(context.Background(), "replace", "r1", refusal); err != nil || proof != "" {
		t.Fatalf("the proof %q, %v; want unresolved", proof, err)
	}
}

// An operator resolution that commits after observeLater read the attempt and before its proof
// write: the conditional UPDATE changes no row, and proof and resolved_by stay as the operator
// wrote them (codex-27, plugins-b8-c2c-source-received-1).
func TestPluginOpLaterObservationResolutionRace(t *testing.T) {
	wait := registerWait
	t.Cleanup(func() { registerWait = wait })
	registerWait = 0
	ctx := context.Background()
	h, pl, id := startOpHub(t, refuse("spawn_unknown", "may still create a block"), false)
	createAndRegister(t, h, pl)
	const by = `{"outcome":"not_applied","note":"the operator checked"}`
	resolved := false
	h.pops.fault = func(p string) bool {
		if p == "observe-before-proof" && !resolved {
			resolved = true
			if _, err := h.db.Exec(`UPDATE plugin_op_attempts SET resolved_by = ? WHERE op_id = ?`, by, id); err != nil {
				t.Error(err)
			}
		}
		return false
	}
	h.opsRun(ctx)
	h.pops.fault = nil
	if !resolved {
		t.Fatal("the observation did not reach its proof write")
	}
	var proof, got string
	if err := h.db.QueryRow(`SELECT proof, resolved_by FROM plugin_op_attempts WHERE op_id = ?`, id).Scan(&proof, &got); err != nil {
		t.Fatal(err)
	}
	if proof != "" || got != by {
		t.Errorf("proof %q, resolved_by %q", proof, got)
	}
}

// fileProofs are the proofs of the op's file attempts, in order.
func fileProofs(t *testing.T, h *Hub, id string) []string {
	t.Helper()
	rows, err := h.db.Query(`SELECT proof FROM plugin_op_attempts WHERE op_id = ? AND call = 'write:managed' ORDER BY rowid`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		out = append(out, p)
	}
	return out
}

// Each managed-file write has its own attempt, with the intended hash, before the rename:
//   - a write in this process: applied when the rename returns;
//   - a crash before the rename (no sent_at): not_applied, and the next run writes with a new attempt;
//   - a crash after the rename, file unchanged: applied (the hash is the intended hash);
//   - a crash after the rename, and another writer changed the file: unresolved (the hash differs).
func TestPluginOpFileAttempts(t *testing.T) {
	for _, c := range []struct {
		point string
		edit  bool
		want  []string // proof prefixes; "" is unresolved
	}{
		{"", false, []string{"applied: the rename returned"}},
		{"plugin-op-before-rename", false, []string{"not_applied", "applied: the rename returned"}},
		{"plugin-op-after-rename", false, []string{"applied: the file is the intended output"}},
		{"plugin-op-after-rename", true, []string{""}},
	} {
		t.Run(fmt.Sprintf("%s edit=%v", c.point, c.edit), func(t *testing.T) {
			h, _, dir := opsTestHub(t, "")
			pd := t.TempDir()
			writeManaged(t, dir, opsLocal(t, pd, "x", "y"))
			opsLoadEntries(t, h)
			id := opsMust(t, h, "s", opRequest{Op: "settings", Entry: "managed:x", Input: json.RawMessage(`{"args":{"k":"v"}}`)})
			if c.point != "" {
				h.pops.fault = func(p string) bool { return p == c.point }
				h.opsRun(context.Background())
				h.pops.fault = nil
			}
			if c.edit {
				// Another writer changes y after the rename.
				list, _ := parseManaged(h.cfg.managedPath)
				var f struct {
					Plugins []managedFileEntry `json:"plugins"`
				}
				for _, e := range list {
					if e.managed == "y" {
						e.Args = map[string]string{"y": "hand"}
					}
					f.Plugins = append(f.Plugins, fileEntry(e))
				}
				b, _ := json.Marshal(f)
				writeManaged(t, dir, string(b))
			}
			h.opsRun(context.Background())
			if op := opsOf(t, h, id); op.Phase != "done" {
				t.Fatalf("op %+v", op)
			}
			got := fileProofs(t, h, id)
			if len(got) != len(c.want) {
				t.Fatalf("file proofs %q; want %q", got, c.want)
			}
			for i, w := range c.want {
				if !strings.HasPrefix(got[i], w) || (w == "" && got[i] != "") {
					t.Fatalf("file proofs %q; want %q", got, c.want)
				}
			}
		})
	}
}

// The proof of a reload's return: a success is applied; "invalid" changes no entry (not_applied);
// any other error is unresolved.
func TestReloadProof(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, "applied"},
		{refuse("invalid", "managed plugin file: bad"), "not_applied"},
		{errors.New("context canceled"), ""},
	} {
		if got := reloadProof(c.err); !strings.HasPrefix(got, c.want) || (c.want == "" && got != "") {
			t.Errorf("%v: %q; want %q", c.err, got, c.want)
		}
	}
}
