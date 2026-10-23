package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"testing"
)

// The agent spawn address "spawn:<spawn_id>" (docs/hub.md, "Agent spawn addresses"): the first
// identity that the spawn's pane reports binds the address for good, a message or notice to an
// unbound address waits while the spawn has not ended, and hub.agents.resolve gives the bound
// identity. The lab drives the real panes; these tests drive the store rules.

// spawnAddrHub is identityFieldsHub with this test process as the child of the spawn's pane "b",
// so a report from this process comes from that pane. The pane has the spawn's label until a test
// stores another label (the pane of no spawn: "").
func spawnAddrHub(t *testing.T) (*Hub, *atomic.Value) {
	t.Helper()
	h := identityFieldsHub(t, nil, false)
	if err := h.workersSchemaStart(context.Background()); err != nil { // the report writes the child columns
		t.Fatal(err)
	}
	pid := os.Getpid()
	label := &atomic.Value{}
	label.Store("lazyrex-spawn:spawn-test")
	h.rex = replyRex(t, func(method string) (any, string) {
		switch method {
		case "session.list_blocks":
			return map[string]any{"blocks": []any{map[string]any{"block_id": "b", "window_id": "w", "label": label.Load()}}}, ""
		case "com.superlogical.terminal.process":
			return map[string]any{"child": map[string]any{"pid": pid}}, ""
		case "com.superlogical.terminal.program_status":
			return map[string]any{"records": []any{}}, ""
		}
		return map[string]any{}, ""
	})
	return h, label
}

func reportAs(t *testing.T, h *Hub, conv, source string) map[string]any {
	t.Helper()
	var p perform
	p.Requester.Principal.PID = os.Getpid()
	out, err := h.identityReport(context.Background(), p, map[string]any{"kind": "claude", "conversation_id": conv, "source": source, "block": "b"})
	if err != nil {
		t.Fatalf("report %s: %v", conv, err)
	}
	return out.(map[string]any)
}

func boundIdentity(t *testing.T, h *Hub) string {
	t.Helper()
	var id string
	if err := h.db.QueryRow(`SELECT identity FROM spawns WHERE spawn_id = 'spawn-test'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func resolveTo(t *testing.T, h *Hub, to string) (map[string]any, error) {
	t.Helper()
	out, err := h.resolveAgent(context.Background(), map[string]any{"to": to})
	if err != nil {
		return nil, err
	}
	return out.(map[string]any), nil
}

// The first report binds the address. A /clear (another conversation in the same pane) does not
// change it, and the binding stays after the spawn closes.
func TestSpawnAddressBindsFirstReport(t *testing.T) {
	h, _ := spawnAddrHub(t)
	ctx := context.Background()
	if r, err := resolveTo(t, h, "spawn:spawn-test"); err != nil || r["id"] != "spawn:spawn-test" || r["bound"] != false {
		t.Fatalf("before the report: %v %v", r, err)
	}
	if r := reportAs(t, h, "c1", "startup"); r["agent_id"] != "spawn:spawn-test" {
		t.Fatalf("the report's agent_id: %v", r["agent_id"])
	}
	if got := boundIdentity(t, h); got != "claude:c1" {
		t.Fatalf("after the first report: identity %q, want claude:c1", got)
	}
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM identity_log WHERE identity = 'claude:c1' AND event = 'bound'`).Scan(&n)
	if n != 1 {
		t.Fatalf("bound log rows: %d, want 1", n)
	}
	reportAs(t, h, "c2", "clear")
	if got := boundIdentity(t, h); got != "claude:c1" {
		t.Fatalf("after a /clear: identity %q, want claude:c1", got)
	}
	if r, err := resolveTo(t, h, "spawn:spawn-test"); err != nil || r["id"] != "claude:c1" || r["via"] != "spawn" {
		t.Fatalf("resolve after the /clear: %v %v", r, err)
	}
	s, err := h.spawnByID(ctx, "spawn-test")
	if err != nil {
		t.Fatal(err)
	}
	h.closeSpawn(ctx, s, "test")
	if r, err := resolveTo(t, h, "spawn:spawn-test"); err != nil || r["id"] != "claude:c1" {
		t.Fatalf("resolve after the close: %v %v", r, err)
	}
	h.mu.Lock()
	inRoster := h.roster()["spawn:spawn-test"]
	h.mu.Unlock()
	if !inRoster {
		t.Fatal("a bound address left the roster at the close")
	}
}

// A spawn that a pane's current identity adopts later binds at the next report of that same
// identity: the unchanged path also binds.
func TestSpawnAddressBindsUnchangedReport(t *testing.T) {
	h, label := spawnAddrHub(t)
	label.Store("")
	reportAs(t, h, "c1", "startup") // the pane has no spawn label yet
	label.Store("lazyrex-spawn:spawn-test")
	if got := boundIdentity(t, h); got != "" {
		t.Fatalf("before the second report: identity %q, want none", got)
	}
	reportAs(t, h, "c1", "resume")
	if got := boundIdentity(t, h); got != "claude:c1" {
		t.Fatalf("after the unchanged report: identity %q, want claude:c1", got)
	}
}

// hub.agents.resolve for each state of a spawn address that never bound.
func TestResolveSpawnAddress(t *testing.T) {
	h, _ := spawnAddrHub(t)
	mustExec(t, h, `INSERT INTO spawns (spawn_id,caller,key,request,profile,title,agent_id,parent,status,created)
		VALUES ('spawn-acc','user','k2','{}','shell','Shell','','user','open','x'),
		('spawn-gone','user','k3','{}','agent','Agent 2','spawn:spawn-gone','user','failed','x'),
		('spawn-shut','user','k4','{}','agent','Agent 3','spawn:spawn-shut','user','closed','x')`)
	if r, err := resolveTo(t, h, "spawn:spawn-test"); err != nil || r["id"] != "spawn:spawn-test" || r["via"] != "spawn" || r["bound"] != false {
		t.Errorf("unbound and open: %v %v", r, err)
	}
	if _, err := resolveTo(t, h, "spawn:spawn-acc"); code(err) != "invalid_target" {
		t.Errorf("an accessory: %v, want invalid_target", err)
	}
	for _, addr := range []string{"spawn:spawn-gone", "spawn:spawn-shut"} {
		if _, err := resolveTo(t, h, addr); code(err) != "not_delivered" {
			t.Errorf("%s ended unbound: %v, want not_delivered", addr, err)
		}
	}
	if _, err := resolveTo(t, h, "spawn:spawn-none"); err == nil || code(err) != "" {
		t.Errorf("an unknown spawn: %v, want a bad request", err)
	}
}

// A notice to an unbound address waits; after the bind it goes to the bound identity while that
// identity is current. The roster and hub.agents {all: true} list a waiting address; an address
// that ended unbound leaves both.
func TestSpawnAddressNoticesAndRoster(t *testing.T) {
	h, _ := spawnAddrHub(t)
	ctx := context.Background()
	if err := h.identitySync(ctx); err != nil {
		t.Fatal(err)
	}
	state := func() (target *Agent, inRoster bool, listed []string) {
		h.mu.Lock()
		target, inRoster = h.noticeTarget("spawn:spawn-test"), h.roster()["spawn:spawn-test"]
		h.mu.Unlock()
		for _, e := range h.agentsList(map[string]any{"all": true}).(map[string]any)["agents"].([]any) {
			m := e.(map[string]any)
			listed = append(listed, fmt.Sprint(m["id"], "=", m["state"]))
		}
		return
	}
	if target, inRoster, listed := state(); target != nil || !inRoster || !slices.Contains(listed, "spawn:spawn-test=starting") {
		t.Fatalf("unbound: target %v, roster %v, listed %v", target, inRoster, listed)
	}
	if slices.Contains(listedIDs(h, false), "spawn:spawn-test") {
		t.Fatal("hub.agents without all lists a waiting address")
	}

	reportAs(t, h, "c1", "startup")
	if target, inRoster, listed := state(); target == nil || target.ID != "claude:c1" || !inRoster || slices.Contains(listed, "spawn:spawn-test=starting") {
		t.Fatalf("bound: target %v, roster %v, listed %v", target, inRoster, listed)
	}
	reportAs(t, h, "c2", "clear")
	if target, _, _ := state(); target != nil {
		t.Fatalf("after a /clear: target %v, want none (the address names the first conversation)", target.ID)
	}

	// Another spawn that ends before its report leaves the roster.
	mustExec(t, h, `INSERT INTO spawns (spawn_id,caller,key,request,profile,title,agent_id,parent,status,created)
		VALUES ('spawn-late','user','k5','{}','agent','Agent 5','spawn:spawn-late','user','open','x')`)
	if err := h.spawnAddrsLoad(ctx); err != nil {
		t.Fatal(err)
	}
	late, err := h.spawnByID(ctx, "spawn-late")
	if err != nil {
		t.Fatal(err)
	}
	h.closeSpawn(ctx, late, "test")
	h.mu.Lock()
	inRoster := h.roster()["spawn:spawn-late"]
	h.mu.Unlock()
	if inRoster || slices.Contains(listedIDs(h, true), "spawn:spawn-late") {
		t.Fatal("an address that ended unbound is still a recipient")
	}
}

func listedIDs(h *Hub, all bool) []string {
	var ids []string
	for _, e := range h.agentsList(map[string]any{"all": all}).(map[string]any)["agents"].([]any) {
		ids = append(ids, e.(map[string]any)["id"].(string))
	}
	return ids
}
