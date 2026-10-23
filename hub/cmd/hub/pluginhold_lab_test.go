package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The C3 lifecycle lab tests (the hub lead's lead-c3v2; codex-26 and codex-27 approved plan v2):
// H -> OLD -> H with an active release hold and an unfinished attempt. Lab only: HARNESS_LAB=1.

// b8RollbackOld is OLD: the release that a rollback from the first B8 release goes to, the frozen
// 4.1 kit (coord-r41-hold-0). It changes when the release order changes. B8_OLD_COMMIT overrides
// it, and B8_OLD_REPO names the repository that has it (default: the repository of this test).
const b8RollbackOld = "25f8c7db5b41d7024db050d434204e1444228404"

type b8Life struct {
	*startLab
	hBin, oldBin    string
	evidence        string
	hHash, oldHash  string
	summary         []string
	agent, probeDir string
}

func newB8Life(t *testing.T, name string) *b8Life {
	s := newStartLab(t)
	b := &b8Life{startLab: s, hBin: s.bin}
	var c map[string]any
	raw, _ := os.ReadFile(s.cfg)
	json.Unmarshal(raw, &c)
	c["agents"] = []any{map[string]any{"id": "alice", "name": "alice"}}
	raw, _ = json.MarshalIndent(c, "", " ")
	os.WriteFile(s.cfg, raw, 0o600)
	s.writeSource("probe", `{"name":"probe","main":"main.lua"}`, "probe")
	s.writeManagedIDs("probe=probe")

	ev := os.Getenv("B8_EVIDENCE")
	if ev == "" {
		ev = filepath.Join(os.TempDir(), "b8-c3-evidence")
	}
	b.evidence = filepath.Join(ev, fmt.Sprintf("%s-%d", name, time.Now().UnixMilli()))
	if err := os.MkdirAll(b.evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.finish)

	repo := os.Getenv("B8_OLD_REPO")
	if repo == "" {
		root, _ := filepath.Abs("../../..")
		repo = root
	}
	old := os.Getenv("B8_OLD_COMMIT")
	if old == "" {
		old = b8RollbackOld
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", old+"^{commit}").Output()
	if err != nil {
		t.Fatalf("OLD %s in %s: %v", old, repo, err)
	}
	b.oldHash = strings.TrimSpace(string(out))
	b.hHash = os.Getenv("B8_H_COMMIT")
	if b.hHash == "" {
		if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
			b.hHash = strings.TrimSpace(string(out))
			if st, _ := exec.Command("git", "status", "--porcelain", "--", "../..").Output(); len(st) > 0 {
				b.hHash += " (with uncommitted changes)"
			}
		} else {
			b.hHash = "unknown: no git; set B8_H_COMMIT"
		}
	}
	b.note("H %s; OLD %s (repository %s)", b.hHash, b.oldHash, repo)

	// OLD from the local repository only: git archive, then a build with no module download.
	src := filepath.Join(s.l.dir, "old")
	os.MkdirAll(src, 0o700)
	archive := exec.Command("sh", "-c", `git -C "$1" archive "$2" hub | tar -x -C "$3"`, "sh", repo, b.oldHash, src)
	if out, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("the OLD archive: %v\n%s", err, out)
	}
	b.oldBin = filepath.Join(s.l.dir, "hub-old")
	build := exec.Command("go", "build", "-o", b.oldBin, "./cmd/hub")
	build.Dir = filepath.Join(src, "hub")
	build.Env = append(os.Environ(), "GOFLAGS=-mod=readonly", "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the OLD build: %v\n%s", err, out)
	}
	return b
}

func (b *b8Life) note(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	b.summary = append(b.summary, line)
	b.t.Log(line)
}

// finish keeps the evidence outside the lab: the summary and the hub log of each start.
func (b *b8Life) finish() {
	for i := 1; i <= b.starts; i++ {
		name := fmt.Sprintf("hub.%d.log", i)
		if raw, err := os.ReadFile(filepath.Join(b.l.dir, name)); err == nil {
			os.WriteFile(filepath.Join(b.evidence, name), raw, 0o600)
		}
	}
	result := "PASS"
	if b.t.Failed() {
		result = "FAIL (lab kept: " + b.l.dir + ")"
	}
	os.WriteFile(filepath.Join(b.evidence, "summary.txt"), []byte(strings.Join(append(b.summary, result), "\n")+"\n"), 0o600)
	fmt.Fprintf(os.Stderr, "C3 evidence: %s\n", b.evidence)
}

func (b *b8Life) save(name string, v any) {
	raw, _ := json.MarshalIndent(v, "", " ")
	os.WriteFile(filepath.Join(b.evidence, name+".json"), raw, 0o600)
}

func (b *b8Life) db() *sql.DB {
	b.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(b.l.dir, "hub", "hub.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		b.t.Fatal(err)
	}
	return db
}

// rows reads every row of a query as column -> value text.
func (b *b8Life) rows(query string, args ...any) []map[string]string {
	b.t.Helper()
	db := b.db()
	defer db.Close()
	r, err := db.Query(query, args...)
	if err != nil {
		b.t.Fatalf("%s: %v", query, err)
	}
	defer r.Close()
	cols, _ := r.Columns()
	out := []map[string]string{}
	for r.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			b.t.Fatal(err)
		}
		m := map[string]string{}
		for i, c := range cols {
			if bs, ok := vals[i].([]byte); ok {
				m[c] = string(bs)
			} else {
				m[c] = fmt.Sprint(vals[i])
			}
		}
		out = append(out, m)
	}
	return out
}

type b8Snapshot struct {
	B8      map[string][]map[string]string // plugin_ops, plugin_op_attempts, plugin_hold
	Plugins []map[string]string
	Intents []map[string]string
	Schema  []map[string]string // sqlite_master, then every column
}

func (b *b8Life) snapshot(name string) b8Snapshot {
	b.t.Helper()
	s := b8Snapshot{B8: map[string][]map[string]string{}}
	for _, t := range []string{"plugin_ops", "plugin_op_attempts", "plugin_hold"} {
		s.B8[t] = b.rows("SELECT * FROM " + t + " ORDER BY rowid")
	}
	s.Plugins = b.rows("SELECT * FROM plugins ORDER BY name")
	s.Intents = b.rows("SELECT * FROM spawn_intents ORDER BY rowid")
	s.Schema = b.rows(`SELECT type, name, tbl_name, ifnull(sql, '') AS sql FROM sqlite_master ORDER BY type, name`)
	s.Schema = append(s.Schema, b.rows(`SELECT m.name AS tbl, p.name, p.type, p."notnull", ifnull(p.dflt_value, '-') AS dflt, p.pk
		FROM sqlite_master m, pragma_table_info(m.name) p WHERE m.type = 'table' ORDER BY m.name, p.cid`)...)
	b.save(name, s)
	return s
}

func (b *b8Life) waitFor(what string, d time.Duration, ok func() bool) {
	b.t.Helper()
	for deadline := time.Now().Add(d); !ok(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			b.t.Fatalf("timeout after %v: %s", d, what)
		}
	}
}

// kill stops the hub with SIGKILL: no shutdown path runs.
func (b *b8Life) kill() {
	b.t.Helper()
	b.l.snapshot()
	if p := b.l.hub.kill(); len(p) > 0 { // SIGKILL to the hub's group, through its keeper
		b.l.unproved = append(b.l.unproved, p...)
		b.t.Fatalf("the hub did not end after SIGKILL: %v", p)
	}
}

// runOld starts OLD on the same config, state and lab server, runs fn, and stops it with SIGTERM.
// OLD must answer and must exit with status 0 (lead-c3-plan B: OLD runs on the H schema).
func (b *b8Life) runOld(fn func()) {
	b.t.Helper()
	b.bin = b.oldBin
	b.start()
	b.must("hub.plugins", nil)
	fn()
	b.l.snapshot()
	// SIGTERM through the keeper. kept.stop proves the keeper and its group ended; keptExit proves
	// OLD's own exit status 0, within the 10 s OLD stop bound.
	deadline := time.Now().Add(10 * time.Second)
	if p := b.l.hub.stop(); len(p) > 0 {
		b.l.unproved = append(b.l.unproved, p...)
		b.t.Fatalf("OLD did not stop cleanly: %v", p)
	}
	if err := keptExit(b.l.hub, deadline); err != nil {
		b.t.Fatalf("OLD did not exit with status 0 within 10 s: %v", err)
	}
	b.bin = b.hBin
	b.note("OLD started and stopped cleanly (exit status 0)")
}

func (b *b8Life) hold(action, id string) map[string]any {
	b.t.Helper()
	args := map[string]any{}
	if id != "" {
		args["release_id"] = id
	}
	out := b.must("hub.plugins.hold."+action, args)
	b.save(fmt.Sprintf("hold-%d-%s", len(b.summary), action), out)
	return out
}

func (b *b8Life) attempt(call string) map[string]string {
	b.t.Helper()
	list := b.rows("SELECT * FROM plugin_op_attempts WHERE call = ? ORDER BY rowid", call)
	if len(list) == 0 {
		return nil
	}
	return list[len(list)-1]
}

func (b *b8Life) opPhase(id string) string {
	b.t.Helper()
	list := b.rows("SELECT phase FROM plugin_ops WHERE op_id = ?", id)
	if len(list) == 0 {
		return ""
	}
	return list[0]["phase"]
}

func (b *b8Life) op(key string, args map[string]any) string {
	b.t.Helper()
	args["key"] = key
	op, err := b.agentDo(b.agent, "hub.plugins.op", args)
	if err != nil {
		b.t.Fatalf("op %s: %v", key, err)
	}
	return op["op_id"].(string)
}

// oldStep is one schema step of OLD's own migrations on a table that OLD creates. OLD is frozen,
// so its steps are fixed. A new retireSchema step (store.go) on a table that OLD migrates adds one
// entry to oldSteps, in the same commit (the hub lead's lead-c3v3).
type oldStep struct {
	table, column, def string // ALTER TABLE table ADD COLUMN column def
	typ, notnull, dflt string // the column as pragma_table_info shows it
}

// oldSteps: main a1d13d7c ("ui_tabs.rev_known goes through retireSchema") drops ui_tabs.rev_known
// at each open, and OLD's ui.go migration adds it again.
var oldSteps = []oldStep{
}

// afterOld is the schema s with oldSteps applied as SQLite stores an ADD COLUMN: the definition
// goes right after the last column definition of the CREATE text (before a table constraint such
// as PRIMARY KEY (...)), and the column comes last in its table.
func afterOld(s []map[string]string) []map[string]string {
	out := append([]map[string]string{}, s...)
	for _, st := range oldSteps {
		last := -1
		for i, e := range out {
			if e["type"] == "table" && e["name"] == st.table {
				sql := e["sql"]
				k := strings.LastIndex(sql, ")")
				for _, c := range []string{", PRIMARY KEY", ", UNIQUE", ", CHECK", ", FOREIGN KEY", ", CONSTRAINT"} {
					if i := strings.Index(sql, c); i >= 0 && i < k {
						k = i
					}
				}
				if k >= 0 {
					sql = sql[:k] + ", " + st.column + " " + st.def + sql[k:]
				}
				c := map[string]string{}
				for k, v := range e {
					c[k] = v
				}
				c["sql"] = sql
				out[i] = c
			}
			if e["tbl"] == st.table {
				last = i
			}
		}
		col := map[string]string{"tbl": st.table, "name": st.column, "type": st.typ, "notnull": st.notnull, "dflt": st.dflt, "pk": "0"}
		out = append(out[:last+1], append([]map[string]string{col}, out[last+1:]...)...)
	}
	return out
}

// sameB8 asserts what OLD may change (the hub lead's lead-c3v3): the B8 rows and the B8 tables'
// schema stay byte-equal, because OLD does not know them; every other schema change is exactly
// oldSteps, no change missing and none more.
func (b *b8Life) sameB8(s1, s2 b8Snapshot) {
	b.t.Helper()
	j := func(v any) string { raw, _ := json.Marshal(v); return string(raw) }
	if j(s1.B8) != j(s2.B8) {
		b.t.Errorf("OLD changed a B8 row:\nS1 %s\nS2 %s", j(s1.B8), j(s2.B8))
	}
	want := afterOld(s1.Schema)
	if j(want) != j(s2.Schema) {
		b.t.Errorf("the schema through OLD is not S1 with exactly OLD's own steps %v:\nwant %s\nS2   %s", oldSteps, j(want), j(s2.Schema))
	}
	b.note("S1 and S2: the B8 rows are byte-equal: %v; the schema is S1 with exactly OLD's own steps: %v", j(s1.B8) == j(s2.B8), j(want) == j(s2.Schema))
	b.note("OLD effects, plugins: S1 %s; S2 %s", j(s1.Plugins), j(s2.Plugins))
	b.note("OLD effects, spawn_intents: S1 %s; S2 %s", j(s1.Intents), j(s2.Intents))
}

// h2Schema asserts that H2's open restores the whole schema of S1 byte for byte: retireSchema
// (store.go) runs each step whose check still finds its column, at every open (lead-c3v3).
func (b *b8Life) h2Schema(s1 b8Snapshot) {
	b.t.Helper()
	j := func(v any) string { raw, _ := json.Marshal(v); return string(raw) }
	s3 := b.snapshot("S3")
	if j(s1.Schema) != j(s3.Schema) {
		b.t.Errorf("H2's open does not restore the schema of S1:\nS1 %s\nS3 %s", j(s1.Schema), j(s3.Schema))
	}
	b.note("S1 and S3 (after H2's open): the schema is byte-equal: %v", j(s1.Schema) == j(s3.Schema))
}

func (b *b8Life) state() string {
	b.t.Helper()
	st, _ := b.must("hub.plugins.hold.status", map[string]any{})["state"].(string)
	return st
}

// resolveAndRelease resolves the attempt (when it has no proof), requires held, releases the hold,
// and requires that a new write op is admitted.
func (b *b8Life) resolveAndRelease(release, opID string, a map[string]string, outcome string) {
	b.t.Helper()
	if a["proof"] == "" {
		out := b.must("hub.plugins.attempt.resolve", map[string]any{"op_id": opID, "attempt": a["attempt"], "outcome": outcome, "note": "C3 lab: the operator read Rex"})
		b.save("resolve", out)
		b.note("resolved %s %s", a["attempt"], outcome)
	}
	if st := b.hold("status", ""); st["state"] != "held" {
		b.t.Fatalf("the hold after the resolution: %v", st)
	}
	if st := b.hold("release", release); st["state"] != "none" {
		b.t.Fatalf("the hold after the release: %v", st)
	}
	b.op("c3-after-release", map[string]any{"op": "settings", "entry": "managed:probe", "input": map[string]any{"args": map[string]any{"k": "v"}}})
	b.note("after the release: a new write op is admitted")
}

func (b *b8Life) arm(spec map[string]any) float64 {
	b.t.Helper()
	return b.must("hub.test.fault", spec)["id"].(float64)
}

func (b *b8Life) faultState(id float64) map[string]any {
	b.t.Helper()
	return b.must("hub.test.fault_state", map[string]any{"id": id})
}

// The start case: a start attempt whose create the hub never sent (plugin-create hold-send, then
// SIGKILL), with an active hold, through OLD and back. The B8 rows and the schema stay exact
// through OLD; the hold stays and is never held until an operator resolves the attempt.
func TestB8LifecycleStartLab(t *testing.T) {
	b := newB8Life(t, "start")
	b.start()
	b.registered("probe")
	b.agent = b.opsAgent("alice")
	// At setup the row is desired=stopped, so no startup recovery makes an intent of its own.
	stop := b.op("c3-setup-stop", map[string]any{"op": "stop", "entry": "managed:probe"})
	b.waitFor("the setup stop", 20*time.Second, func() bool { return b.opPhase(stop) == "done" })

	// H1.
	fault := b.arm(map[string]any{"point": "plugin-create", "mode": "hold-send", "key": "probe", "plugin": "probe", "hold_ms": float64(60000)})
	id := b.op("c3-start", map[string]any{"op": "start", "entry": "managed:probe"})
	var a map[string]string
	b.waitFor("the start attempt is bound and sent with its intent", 20*time.Second, func() bool {
		a = b.attempt("start")
		ph := b.opPhase(id)
		return a != nil && a["bound_at"] != "" && a["sent_at"] != "" && a["intent_id"] != "" && (ph == "starting" || ph == "unknown" && a["outcome"] != "")
	})
	if a["proof"] != "" || b.faultState(fault)["fired"] != true {
		t.Fatalf("before the kill: attempt %v, fault %v", a, b.faultState(fault))
	}
	if in := b.rows("SELECT status FROM spawn_intents WHERE instance_id = ?", a["intent_id"]); len(in) != 1 || in[0]["status"] == "created" {
		t.Fatalf("the intent: %v", in)
	}
	acq := b.hold("acquire", "c3-start")
	if acq["state"] != "draining" && acq["state"] != "unresolved" {
		t.Fatalf("acquire: %v", acq)
	}
	b.note("H1: op %s phase %s; attempt %s bound, sent, intent %s; acquire state %v", id, b.opPhase(id), a["attempt"], a["intent_id"], acq["state"])
	b.kill()
	sess := b.rows("SELECT session_id FROM spawn_intents WHERE instance_id = ?", a["intent_id"])[0]["session_id"]
	if n := b.pluginBlocks(sess, "probe"); n != 0 {
		t.Fatalf("Rex has %d probe blocks after the kill", n)
	}
	b.note("after the kill: no probe block in session %s", sess)
	s1 := b.snapshot("S1")
	if len(s1.B8["plugin_hold"]) != 1 {
		t.Fatalf("S1 has no hold row: %v", s1.B8["plugin_hold"])
	}

	// OLD.
	b.runOld(func() {})
	s2 := b.snapshot("S2")
	b.sameB8(s1, s2)

	// H2.
	b.start()
	b.h2Schema(s1)
	first := b.hold("status", "")
	if first["release_id"] != "c3-start" || first["acquired_at"] != s1.B8["plugin_hold"][0]["acquired_at"] || (first["state"] != "draining" && first["state"] != "unresolved") {
		t.Fatalf("H2 first status: %v", first)
	}
	b.note("H2 first status: %v", first["state"])
	b.waitFor("the op ends unknown", 80*time.Second, func() bool {
		if st := b.state(); st == "held" {
			t.Fatalf("held before the resolution")
		}
		return b.opPhase(id) == "unknown"
	})
	a = b.attempt("start")
	if a["proof"] != "" || a["outcome"] == "" || b.state() != "unresolved" {
		t.Fatalf("H2: attempt %v, state %v", a, b.state())
	}
	b.note("H2: op unknown; attempt proof empty, outcome %q; state unresolved", a["outcome"])
	b.snapshot("S3")

	// H3: the same state after a restart.
	b.stop()
	b.start()
	if st := b.state(); st != "unresolved" {
		t.Fatalf("H3: %v", st)
	}
	b.note("H3: state unresolved")
	b.resolveAndRelease("c3-start", id, a, "not_applied")
}

// The stop case (the hole check): a stop attempt sent and not run (op-call hold-after-sent, then
// SIGKILL), with an active hold; OLD then stops probe by itself. OLD must change a stamped value of
// the row. At H2 the attempt stays unresolved while the row's stamp is invalid; if H2 saved the row
// again, an applied proof must name the same target instance and entry.
// This lab does not prove the stamp rule: OLD replaces the plugin process at its start
// (plugins.go), so it never stops the attempt's own instance, and the instance check refuses the
// proof first. TestPluginOpStopObservationAfterOldWriter covers the same-instance OLD write, where
// only the stamp rule can refuse it.
func TestB8LifecycleStopLab(t *testing.T) {
	b := newB8Life(t, "stop")
	b.start()
	b.registered("probe")
	b.agent = b.opsAgent("alice")
	run := b.status("probe")

	// H1.
	fault := b.arm(map[string]any{"point": "op-call", "mode": "hold-after-sent", "key": "probe", "plugin": "probe", "hold_ms": float64(60000)})
	id := b.op("c3-stop", map[string]any{"op": "stop", "entry": "managed:probe"})
	b.waitFor("the stop handler holds after sent_at", 20*time.Second, func() bool { return b.faultState(fault)["held"] == true })
	if len(b.events("fault_held", "probe")) != 1 {
		t.Fatalf("no fault_held event")
	}
	a := b.attempt("stop")
	if a == nil || a["bound_at"] == "" || a["sent_at"] == "" || a["outcome"] != "" || a["target_instance"] != run.InstanceID ||
		a["target_block"] != run.BlockID || a["target_session"] == "" || b.opPhase(id) != "stopping" {
		t.Fatalf("before the kill: attempt %v, phase %s, run %+v", a, b.opPhase(id), run)
	}
	if st := b.status("probe"); !st.Registered || st.BlockID != run.BlockID {
		t.Fatalf("probe before the kill: %+v", st)
	}
	acq := b.hold("acquire", "c3-stop")
	if acq["state"] != "draining" {
		t.Fatalf("acquire: %v", acq)
	}
	b.note("H1: op %s stopping; attempt %s bound and sent for instance %s block %s; acquire draining", id, a["attempt"], a["target_instance"], a["target_block"])
	b.kill()
	s1 := b.snapshot("S1")

	// OLD stops probe by itself, as a rollback hub would.
	var oldRun startStatus
	b.runOld(func() {
		b.waitFor("OLD has probe registered", 30*time.Second, func() bool { oldRun = b.status("probe"); return oldRun.Registered })
		b.must("hub.admin.stop", map[string]any{"plugin": "probe"})
		b.waitFor("OLD stopped probe", 20*time.Second, func() bool { return !b.status("probe").Registered })
	})
	s2 := b.snapshot("S2")
	b.sameB8(s1, s2)
	stamped := []string{"desired", "instance_id", "session_id", "block_id", "child_pid", "child_start", "run"}
	row := func(s b8Snapshot) map[string]string {
		for _, p := range s.Plugins {
			if p["name"] == "probe" {
				return p
			}
		}
		t.Fatalf("no probe row: %v", s.Plugins)
		return nil
	}
	p1, p2 := row(s1), row(s2)
	var changed []string
	for _, c := range stamped {
		if p1[c] != p2[c] {
			changed = append(changed, c)
		}
	}
	valid := func(p map[string]string) bool {
		var pid int
		fmt.Sscan(p["child_pid"], &pid)
		return p["entry_stamp"] == entryStamp(p["desired"], p["instance_id"], p["session_id"], p["block_id"], pid, p["child_start"], p["run"])
	}
	if len(changed) == 0 {
		t.Fatalf("OLD changed no stamped value of probe: the run does not cover the rollback boundary: %v", p2)
	}
	b.note("OLD changed the stamped values %v; the stamp is valid after OLD: %v", changed, valid(p2))
	same := p2["instance_id"] == a["target_instance"]
	b.note("OLD stopped instance %s (registered with OLD: %s); the attempt's target is %s. Same instance, so only the stamp rule can refuse the proof: %v", p2["instance_id"], oldRun.InstanceID, a["target_instance"], same)

	// H2.
	b.start()
	b.h2Schema(s1)
	first := b.hold("status", "")
	if first["release_id"] != "c3-stop" || (first["state"] != "draining" && first["state"] != "unresolved") {
		t.Fatalf("H2 first status: %v", first)
	}
	b.note("H2 first status: %v", first["state"])
	b.waitFor("the op ends unknown", 80*time.Second, func() bool { return b.opPhase(id) == "unknown" })
	time.Sleep(opObservePoll + 5*time.Second) // one later-observation pass more
	s3 := b.snapshot("S3")
	p3 := row(s3)
	a = b.attempt("stop")
	resaved := p3["entry_stamp"] != p2["entry_stamp"]
	b.note("H2: saved the row again: %v (stamp valid: %v, entry %s); attempt proof %q", resaved, valid(p3), p3["entry_identity"], a["proof"])
	switch {
	case !valid(p3):
		if a["proof"] != "" || b.state() != "unresolved" {
			t.Fatalf("THE HOLE: an OLD write made the stamp invalid, and the attempt has proof %q (state %v)", a["proof"], b.state())
		}
	case a["proof"] != "":
		var ident string
		b.rowsInto(&ident, "SELECT entry_identity FROM plugin_ops WHERE op_id = ?", id)
		if p3["instance_id"] != a["target_instance"] || p3["entry_identity"] != ident {
			t.Fatalf("applied for another instance or entry: row %v, attempt %v, op entry %s", p3, a, ident)
		}
		b.note("H2 saved the row with a valid stamp; applied as an effect for the same instance and entry (lead-c3v2)")
	}
	b.resolveAndRelease("c3-stop", id, a, "applied")
}

// pluginBlocks counts the blocks of the plugin (label lazyrex-plugin:<name>) in the session.
func (b *b8Life) pluginBlocks(session, name string) int {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.l.r.Call(ctx, "session.attach", map[string]any{"session_id": session, "event_scope": "session"}, nil); err != nil {
		b.t.Fatal(err)
	}
	var out struct {
		Blocks []struct {
			Label string `json:"label"`
		} `json:"blocks"`
	}
	if err := b.l.r.Call(ctx, "session.list_blocks", map[string]any{"session_id": session}, &out); err != nil {
		b.t.Fatal(err)
	}
	n := 0
	for _, bl := range out.Blocks {
		if bl.Label == "lazyrex-plugin:"+name {
			n++
		}
	}
	return n
}

func (b *b8Life) rowsInto(dst *string, query string, args ...any) {
	b.t.Helper()
	list := b.rows(query, args...)
	for _, v := range list[0] {
		*dst = v
	}
}

// keptExit is the exit of a kept command after its stop: its status from the keeper's "exit"
// line, read after the done signal, by deadline. kept.stop checks the keeper and its group, not the
// command's status, so OLD's exit status is checked here.
func keptExit(k *kept, deadline time.Time) error {
	select {
	case <-k.done:
	case <-time.After(time.Until(deadline)):
		return fmt.Errorf("no exit status by the deadline")
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("the exit came after the deadline")
	}
	return k.status
}

// keptExit gives the command's own status: an exit other than 0 is an error, also when the keeper
// stopped cleanly (codex-27, plugins-c2-source-findings-1).
// OLD starts under the H binary's keeper, answers, and stops with exit status 0. Before the fix,
// startLab.start used OLD's binary as the keeper too, and OLD (which has no keeper role) printed its
// usage: "hub keeper: no command report".
func TestB8OldStartLab(t *testing.T) {
	b := newB8Life(t, "old-start")
	b.runOld(func() {})
}

func TestKeptExit(t *testing.T) {
	k := &kept{done: make(chan struct{})}
	k.status = exitStatus("7")
	close(k.done)
	if err := keptExit(k, time.Now().Add(time.Second)); err == nil {
		t.Fatal("exit 7 is accepted")
	}
	k = &kept{done: make(chan struct{})}
	close(k.done)
	if err := keptExit(k, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("exit 0: %v", err)
	}
	if err := keptExit(&kept{done: make(chan struct{})}, time.Now().Add(50*time.Millisecond)); err == nil {
		t.Fatal("no status is accepted")
	}
}
