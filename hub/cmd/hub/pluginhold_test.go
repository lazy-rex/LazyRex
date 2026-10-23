package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func holdCall(t *testing.T, h *Hub, action string, args map[string]any) (map[string]any, error) {
	t.Helper()
	raw := map[string]json.RawMessage{}
	for k, v := range args {
		b, _ := json.Marshal(v)
		raw[k] = b
	}
	res, err := h.holdAction(context.Background(), action, raw)
	if err != nil {
		return nil, err
	}
	return res.(map[string]any), nil
}

func holdState(t *testing.T, h *Hub) map[string]any {
	t.Helper()
	res, err := holdCall(t, h, "hub.plugins.hold.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Acquire, release and status: the arguments, the idempotent repeats, another ID, and the
// status with no row.
func TestPluginHoldActions(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	if s := holdState(t, h); s["release_id"] != "" || s["state"] != "none" || s["acquired_at"] != "" {
		t.Fatalf("no row: %v", s)
	}
	for _, args := range []map[string]any{{}, {"release_id": ""}, {"release_id": "Upper"}, {"release_id": 7}, {"release_id": "r1", "x": 1}} {
		if _, err := holdCall(t, h, "hub.plugins.hold.acquire", args); !isRefusal(err, "invalid_input") {
			t.Errorf("acquire %v: %v", args, err)
		}
	}
	s, err := holdCall(t, h, "hub.plugins.hold.acquire", map[string]any{"release_id": "r1"})
	if err != nil || s["release_id"] != "r1" || s["state"] != "held" || s["acquired_at"] == "" {
		t.Fatalf("acquire: %v, %v", s, err)
	}
	again, err := holdCall(t, h, "hub.plugins.hold.acquire", map[string]any{"release_id": "r1"})
	if err != nil || again["acquired_at"] != s["acquired_at"] {
		t.Fatalf("a repeated acquire: %v, %v", again, err)
	}
	for _, action := range []string{"hub.plugins.hold.acquire", "hub.plugins.hold.release"} {
		if _, err := holdCall(t, h, action, map[string]any{"release_id": "r2"}); !isRefusal(err, "held_by_other") || !strings.Contains(err.Error(), "r1") {
			t.Errorf("%s with another ID: %v", action, err)
		}
	}
	if s, err := holdCall(t, h, "hub.plugins.hold.release", map[string]any{"release_id": "r1"}); err != nil || s["state"] != "none" {
		t.Fatalf("release: %v, %v", s, err)
	}
	if s, err := holdCall(t, h, "hub.plugins.hold.release", map[string]any{"release_id": "r1"}); err != nil || s["state"] != "none" {
		t.Fatalf("a repeated release: %v, %v", s, err)
	}
}

// Two acquires with two IDs in a race: one holds, the other is held_by_other with the holder's ID.
func TestPluginHoldAcquireRace(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = holdCall(t, h, "hub.plugins.hold.acquire", map[string]any{"release_id": fmt.Sprintf("r%d", i)})
		}()
	}
	wg.Wait()
	holder := holdState(t, h)["release_id"]
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
			if holder != fmt.Sprintf("r%d", i) {
				t.Errorf("r%d won, the holder is %v", i, holder)
			}
		case !isRefusal(err, "held_by_other") || !strings.Contains(err.Error(), fmt.Sprint(holder)):
			t.Errorf("r%d: %v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d holders: %v", won, errs)
	}
}

// Admission under a hold is refused (held), and nothing is inserted. Admissions in a race with an
// acquire are each refused or listed in the state that the acquire reports.
func TestPluginHoldAdmission(t *testing.T) {
	h, _, dir := opsTestHub(t, "")
	writeManaged(t, dir, opsLocal(t, t.TempDir(), "x"))
	opsLoadEntries(t, h)
	h.plugins["x"] = &Plugin{Name: "x", entry: opsEntry(t, h, "managed:x")}
	if _, err := holdCall(t, h, "hub.plugins.hold.acquire", map[string]any{"release_id": "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admitOp(context.Background(), opOrigin{Kind: "agent", ID: "a1"}, "agent:a1", "k", opRequest{Op: "stop", Entry: "managed:x"}); !isRefusal(err, "held") {
		t.Fatalf("admission under the hold: %v", err)
	}
	var n int
	h.db.QueryRow(`SELECT count(*) FROM plugin_ops`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d ops inserted under the hold", n)
	}
	if _, err := holdCall(t, h, "hub.plugins.hold.release", map[string]any{"release_id": "r1"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	admitted := make([]bool, 8)
	var state map[string]any
	for i := range admitted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.admitOp(context.Background(), opOrigin{Kind: "agent", ID: "a1"}, "agent:a1", fmt.Sprintf("k%d", i), opRequest{Op: "stop", Entry: "managed:x"})
			switch {
			case err == nil:
				admitted[i] = true
			case !isRefusal(err, "held"):
				t.Errorf("k%d: %v", i, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s, err := holdCall(t, h, "hub.plugins.hold.acquire", map[string]any{"release_id": "r2"})
		if err != nil {
			t.Error(err)
		}
		state = s
	}()
	wg.Wait()
	listed := map[string]bool{}
	for _, f := range state["inflight"].([]map[string]any) {
		listed[f["op_id"].(string)] = true
	}
	for i, ok := range admitted {
		var id string
		err := h.db.QueryRow(`SELECT op_id FROM plugin_ops WHERE key = ?`, fmt.Sprintf("k%d", i)).Scan(&id)
		if ok != (err == nil) {
			t.Errorf("k%d: admitted %v, row %v", i, ok, err)
		}
		// An op that the manager has not run yet is queued, so it is listed.
		if ok && !listed[id] {
			var phase string
			h.db.QueryRow(`SELECT phase FROM plugin_ops WHERE op_id = ?`, id).Scan(&phase)
			if phase != "done" && phase != "failed" && phase != "conflict" && phase != "unknown" {
				t.Errorf("k%d (%s) is admitted and open, and the acquire's state does not list it", i, phase)
			}
		}
	}
}

// The state follows the ops and attempts:
//   - a queued op before its first attempt: draining, never held;
//   - an unconfirmed agent request (add, update) counts for nothing;
//   - an op that ended unknown keeps unresolved; a later observation that proves its attempt moves
//     the state to held; the state is the same for a new hub on the same store (a restart).
func TestPluginHoldStates(t *testing.T) {
	wait := registerWait
	t.Cleanup(func() { registerWait = wait })
	registerWait = 0
	ctx := context.Background()
	h, pl, id := startOpHub(t, refuse("spawn_unknown", "may still create a block"), false)
	if _, err := holdCall(t, h, "hub.plugins.hold.acquire", map[string]any{"release_id": "r1"}); err != nil {
		t.Fatal(err)
	}
	s := holdState(t, h)
	if s["state"] != "unresolved" || len(s["unresolved"].([]map[string]any)) != 1 || s["unresolved"].([]map[string]any)[0]["op_id"] != id {
		t.Fatalf("an unknown start: %v", s)
	}
	if s := holdState(t, &Hub{db: h.db}); s["state"] != "unresolved" {
		t.Fatalf("after a restart: %v", s)
	}
	createAndRegister(t, h, pl)
	h.opsRun(ctx)
	if s := holdState(t, h); s["state"] != "held" {
		t.Fatalf("after the later proof: %v", s)
	}

	now := time.Now().UnixMilli()
	if _, err := h.db.Exec(`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated)
		VALUES ('req', 'agent:a1', 'req', 'f', '{"kind":"agent","id":"a1"}', 'add', 'managed:z', '', '', '', '', 'awaiting_user', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if s := holdState(t, h); s["state"] != "held" {
		t.Fatalf("an unconfirmed agent request: %v", s)
	}
	if _, err := h.db.Exec(`INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, created, updated)
		VALUES ('q', 'agent:a1', 'q', 'f', '{"kind":"agent","id":"a1"}', 'stop', 'managed:y', 'y', '', '', '', 'queued', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	s = holdState(t, h)
	if s["state"] != "draining" || len(s["inflight"].([]map[string]any)) != 1 || s["inflight"].([]map[string]any)[0]["op_id"] != "q" {
		t.Fatalf("a queued op before its first attempt: %v", s)
	}
}

func resolveCall(h *Hub, args map[string]any) (map[string]any, error) {
	raw := map[string]json.RawMessage{}
	for k, v := range args {
		b, _ := json.Marshal(v)
		raw[k] = b
	}
	res, err := h.attemptResolve(context.Background(), raw)
	if err != nil {
		return nil, err
	}
	return res.(map[string]any), nil
}

// hub.plugins.attempt.resolve: the arguments; an unknown op or attempt; an open attempt; a proved
// attempt; the resolution of an unresolved attempt (resolved_by only: outcome and proof stay, and
// the hold moves to held); the same resolution again returns the same record; another one is
// conflict.
func TestPluginAttemptResolve(t *testing.T) {
	wait := registerWait
	t.Cleanup(func() { registerWait = wait })
	registerWait = 0
	h, _, id := startOpHub(t, refuse("spawn_unknown", "may still create a block"), false)
	var attempt, outcome string
	if err := h.db.QueryRow(`SELECT attempt, outcome FROM plugin_op_attempts WHERE op_id = ?`, id).Scan(&attempt, &outcome); err != nil {
		t.Fatal(err)
	}
	ok := map[string]any{"op_id": id, "attempt": attempt, "outcome": "not_applied", "note": "no block in session:home"}
	for _, bad := range []map[string]any{
		{"op_id": id, "attempt": attempt, "outcome": "maybe", "note": "n"},
		{"op_id": id, "attempt": attempt, "outcome": "applied", "note": ""},
		{"op_id": id, "attempt": attempt, "outcome": "applied", "note": strings.Repeat("n", 1001)},
		{"op_id": id, "outcome": "applied", "note": "n"},
		{"op_id": id, "attempt": attempt, "outcome": "applied", "note": "n", "retry": true},
	} {
		if _, err := resolveCall(h, bad); !isRefusal(err, "invalid_input") {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if _, err := resolveCall(h, map[string]any{"op_id": id, "attempt": "nope", "outcome": "applied", "note": "n"}); !isRefusal(err, "not_found") {
		t.Errorf("an unknown attempt: %v", err)
	}
	if _, err := holdCall(t, h, "hub.plugins.hold.acquire", map[string]any{"release_id": "r1"}); err != nil {
		t.Fatal(err)
	}
	first, err := resolveCall(h, ok)
	if err != nil {
		t.Fatal(err)
	}
	var by, proof, after string
	h.db.QueryRow(`SELECT resolved_by, proof, outcome FROM plugin_op_attempts WHERE attempt = ?`, attempt).Scan(&by, &proof, &after)
	if by == "" || proof != "" || after != outcome {
		t.Fatalf("resolved_by %q, proof %q, outcome %q (was %q)", by, proof, after, outcome)
	}
	if s := holdState(t, h); s["state"] != "held" {
		t.Fatalf("the hold after the resolution: %v", s)
	}
	again, err := resolveCall(h, ok)
	if err != nil || fmt.Sprint(again["resolved_by"]) != fmt.Sprint(first["resolved_by"]) {
		t.Fatalf("the same resolution again: %v, %v", again, err)
	}
	other := map[string]any{"op_id": id, "attempt": attempt, "outcome": "applied", "note": "no block in session:home"}
	if _, err := resolveCall(h, other); !isRefusal(err, "conflict") {
		t.Fatalf("another outcome: %v", err)
	}
	var now string
	h.db.QueryRow(`SELECT resolved_by FROM plugin_op_attempts WHERE attempt = ?`, attempt).Scan(&now)
	if now != by {
		t.Fatalf("resolved_by changed: %q, was %q", now, by)
	}

	// A proved attempt and an open attempt cannot be resolved.
	opsAttemptRow(t, h, "p1", "stop")
	h.db.Exec(`UPDATE plugin_op_attempts SET op_id = ?, outcome = 'stop: done', proof = 'applied: the call returned' WHERE attempt = 'p1'`, id)
	opsAttemptRow(t, h, "o1", "stop")
	h.db.Exec(`UPDATE plugin_op_attempts SET op_id = ? WHERE attempt = 'o1'`, id)
	for _, a := range []string{"p1", "o1"} {
		if _, err := resolveCall(h, map[string]any{"op_id": id, "attempt": a, "outcome": "applied", "note": "n"}); !isRefusal(err, "conflict") {
			t.Errorf("attempt %s: %v", a, err)
		}
	}
}

// An operator resolution stays visible (lead-b8-resolved): every hold reply lists it in resolved,
// never in unresolved, and hub.plugins.ops shows it on the op's attempt, also for a new hub on the
// same store (a restart). A proved attempt has no resolved_by.
func TestPluginResolutionVisible(t *testing.T) {
	wait := registerWait
	t.Cleanup(func() { registerWait = wait })
	registerWait = 0
	ctx := context.Background()
	h, _, id := startOpHub(t, refuse("spawn_unknown", "may still create a block"), false)
	var attempt string
	if err := h.db.QueryRow(`SELECT attempt FROM plugin_op_attempts WHERE op_id = ?`, id).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	opsAttemptRow(t, h, "p1", "stop")
	h.db.Exec(`UPDATE plugin_op_attempts SET op_id = ?, at = 9999999999999, outcome = 'stop: done', proof = 'applied: the call returned' WHERE attempt = 'p1'`, id)
	if _, err := resolveCall(h, map[string]any{"op_id": id, "attempt": attempt, "outcome": "not_applied", "note": "no block in session:home"}); err != nil {
		t.Fatal(err)
	}
	if _, err := holdCall(t, h, "hub.plugins.hold.acquire", map[string]any{"release_id": "r1"}); err != nil {
		t.Fatal(err)
	}
	for _, hub := range []*Hub{h, {db: h.db}} {
		s := holdState(t, hub)
		resolved, _ := s["resolved"].([]map[string]any)
		if s["state"] != "held" || len(s["unresolved"].([]map[string]any)) != 0 || len(resolved) != 1 {
			t.Fatalf("the hold status: %v", s)
		}
		r := resolved[0]
		by, _ := r["resolved_by"].(map[string]any)
		if r["op_id"] != id || r["attempt"] != attempt || r["call"] != "start" || r["note"] != "resolved by operator (attribution, not proof)" ||
			by["by"] != "operator" || by["outcome"] != "not_applied" || by["note"] != "no block in session:home" {
			t.Fatalf("the resolved entry: %v", r)
		}
		for _, args := range []map[string]any{{"op_id": id}, nil} {
			raw := map[string]json.RawMessage{}
			for k, v := range args {
				b, _ := json.Marshal(v)
				raw[k] = b
			}
			v, err := hub.pluginOpsList(ctx, raw, "")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(v)
			var ops []map[string]any
			if args == nil {
				var list struct{ Ops []map[string]any }
				json.Unmarshal(b, &list)
				ops = list.Ops
			} else {
				var op map[string]any
				json.Unmarshal(b, &op)
				ops = []map[string]any{op}
			}
			if len(ops) != 1 {
				t.Fatalf("hub.plugins.ops %v: %s", args, b)
			}
			atts, _ := ops[0]["attempts"].([]any)
			if len(atts) != 2 {
				t.Fatalf("the attempts %v: %s", args, b)
			}
			a, p := atts[0].(map[string]any), atts[1].(map[string]any)
			ab, _ := a["resolved_by"].(map[string]any)
			if a["attempt"] != attempt || a["call"] != "start" || a["proof"] != "" || a["outcome"] == "" ||
				ab["outcome"] != "not_applied" || ab["by"] != "operator" || a["note"] != "resolved by operator (attribution, not proof)" {
				t.Fatalf("the resolved attempt %v: %s", args, b)
			}
			if _, ok := p["resolved_by"]; ok || p["attempt"] != "p1" || p["proof"] != "applied: the call returned" {
				t.Fatalf("the proved attempt %v: %s", args, b)
			}
		}
	}
}
