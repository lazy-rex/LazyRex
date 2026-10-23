package main

// The full tier of release rehearse. It runs after the default phases, on the same lab.
// Each phase starts and ends with OLD running and the home pin at OLD:
//   spawn        OLD writes a spawns row; forward; H reads the row exactly; the rows of the
//                spec probe file stay exact; rollback.
//   pull-stop    pull stops after its mutation (exit 3); pull again; rollback.
//   to-from      a pull that stopped after the lock write: the backup base put back.
//   from-to      a rollback pull that stopped after the lock write: the backup lock put back.
//   build-fails  pull with a go command whose build fails: no H binary, "from from".
//   start-fails  start with that go command: dev.sh cannot build, the dev pane stays.
//   resume       backup stops before the stamp; resume starts OLD again.
//   h-ended      after forward, the dev pane closes outside the tool; rollback.
// After each phase: the schema, the file probe rows, the pin state "from from",
// the OLD checkout and the back verify list.

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	relProbeAgent    = "relprobe"   // the lab-only roster agent that writes the spawns row
	relProbeProfile  = "relprobe"   // the lab-only accessory profile that it spawns
	relProbeSpawnKey = "relprobe-1" // the key of that spawn
	relAgentLabel    = "release-rehearse-agent"
)

// labProbeAgent adds the lab-only probe agent to the lab config and the probe profile to the lab
// profile file (a new lab file when the live config has none). The rehearsal never joins or spawns
// a live agent or profile: a live profile can start an agent CLI. The additions go into the
// transform record.
func labProbeAgent(lab, cfgPath string) ([]relRewrite, error) {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	cfg := map[string]any{}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, relFail("the lab config: %v", err)
	}
	agents, _ := cfg["agents"].([]any)
	for _, x := range agents {
		if a, _ := x.(map[string]any); a["id"] == relProbeAgent {
			return nil, relFail("the live config has the agent %q, which the rehearsal adds", relProbeAgent)
		}
	}
	cfg["agents"] = append(agents, map[string]any{"id": relProbeAgent, "name": "Release probe"})
	out := []relRewrite{{Key: "config.agents." + relProbeAgent, Lab: "added (lab only)"}}
	p, ok := cfg["profiles"].(string)
	if !ok {
		p = filepath.Join(lab, "home", "profiles.json")
		cfg["profiles"] = p
		out = append(out, relRewrite{Key: "config.profiles", Lab: p})
	}
	pf := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &pf); err != nil {
			return nil, relFail("the lab profiles: %v", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	list, _ := pf["profiles"].([]any)
	for _, x := range list {
		if q, _ := x.(map[string]any); q["id"] == relProbeProfile {
			return nil, relFail("the live profiles have the profile %q, which the rehearsal adds", relProbeProfile)
		}
	}
	pf["profiles"] = append(list, map[string]any{"id": relProbeProfile, "name": "Release probe", "kind": "accessory",
		"command": []string{"/bin/sh", "-c", "read x"}})
	out = append(out, relRewrite{Key: "profiles." + relProbeProfile, Lab: "added (lab only)"})
	cb, _ := json.MarshalIndent(cfg, "", "  ")
	pb, _ := json.MarshalIndent(pf, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(cfgPath, cb, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, pb, 0o600); err != nil {
		return nil, err
	}
	return out, scanLab(lab, cfgPath, p)
}

// full runs the phases of the full tier, in order. A failed phase stops the rehearsal.
func (h *rehearsal) full() error {
	for _, p := range []struct {
		name string
		fn   func() error
	}{
		{"spawn", h.spawnPhase},
		{"pull-stop", h.pullStopPhase},
		{"to-from", func() error { return h.partialPhase("to from") }},
		{"from-to", func() error { return h.partialPhase("from to") }},
		{"build-fails", h.buildFailsPhase},
		{"start-fails", h.startFailsPhase},
		{"resume", h.resumePhase},
		{"h-ended", h.hEndedPhase},
	} {
		if err := h.phase(p.name, p.fn); err != nil {
			return err
		}
	}
	return nil
}

// steps runs child steps of the lab run with the lab environment; each must exit 0.
func (h *rehearsal) steps(names ...string) error {
	for _, s := range names {
		if err := h.child(s, nil); err != nil {
			return err
		}
	}
	return nil
}

// stepCode runs one child step with extra variables and checks its exit status.
func (h *rehearsal) stepCode(step string, extra []string, ok func(int) bool, want string) error {
	code, logp, err := h.childCode(step, extra)
	if err != nil {
		return err
	}
	if !ok(code) {
		return relFail("step %s exited %d, not %s; see %s", step, code, want, logp)
	}
	return nil
}

// pinIs checks the home pin state of the lab run.
func (h *rehearsal) pinIs(want string) error {
	return h.withLab(func(lr *releaser) error {
		c, err := lr.readCandidate()
		if err != nil {
			return err
		}
		st, err := lr.pinState(c)
		if err != nil {
			return err
		}
		if st != want {
			return relFail("the home pin state is %q, not %q", st, want)
		}
		return nil
	})
}

// back checks a phase that ended with a rollback or a resume: the SDK bytes of OLD, the schema, the file probe rows, the pins at OLD, the OLD checkout and the back verify list.
func (h *rehearsal) back(name string) error {
	if err := h.checkSDK(name, "", h.c.From.Commit); err != nil {
		return err
	}
	if err := h.schema(name); err != nil {
		return err
	}
	if err := h.checkFileProbes(name); err != nil {
		return err
	}
	if err := h.pinIs("from from"); err != nil {
		return relFail("phase %s: %v", name, err)
	}
	return h.withLab(func(lr *releaser) error {
		if err := lr.needHead(h.c.From.Commit, "OLD"); err != nil {
			return err
		}
		return lr.verifyList(relVerifyBack)
	})
}

func (h *rehearsal) rollbackAs(name string) error {
	if err := h.child("rollback", nil); err != nil {
		return err
	}
	return h.back(name)
}

// spawnPhase: the spawns row and the file probe rows. The probe agent joins in a lab pane under
// OLD and spawns the probe profile; OLD stores the spawns row. The file probe rows are written
// while the hub is stopped in the forward run. Under H the row is the same in every OLD column,
// and hub.workers lists the spawn once. After the rollback, the row is the same again.
func (h *rehearsal) spawnPhase() error {
	sp, row, err := h.spawnUnderOld()
	if err != nil {
		return err
	}
	if err := h.steps("precheck", "stop"); err != nil {
		return err
	}
	if err := h.writeFileProbes(); err != nil {
		return err
	}
	if err := h.steps("backup", "checkout", "pull", "start", "replace", "verify"); err != nil {
		return err
	}
	if err := h.forwardChecks("spawn-forward"); err != nil {
		return err
	}
	if err := h.checkFileProbes("spawn-forward"); err != nil {
		return err
	}
	if err := h.checkSpawn(sp, row, "under H"); err != nil {
		return err
	}
	if err := h.withLab(func(lr *releaser) error {
		hub, err := lr.currentHub()
		if err != nil {
			return err
		}
		out, err := lr.hubDo(hub.Client, "hub.workers", map[string]any{})
		if err != nil {
			return relFail("hub.workers under H: %v", err)
		}
		var v any
		if err := json.Unmarshal(out, &v); err != nil {
			return relFail("hub.workers under H: %v", err)
		}
		if n := countSpawn(v, sp); n != 1 {
			return relFail("hub.workers under H lists the spawn %s %d times, not once", sp, n)
		}
		return nil
	}); err != nil {
		return err
	}
	h.say("spawn: H reads the spawns row %s of OLD exactly, and hub.workers lists it once", sp)
	if err := h.rollbackAs("spawn-rollback"); err != nil {
		return err
	}
	return h.checkSpawn(sp, row, "under OLD after the rollback")
}

// countSpawn counts the objects in v whose spawn_id is sp.
func countSpawn(v any, sp string) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		if x["spawn_id"] == sp {
			n++
		}
		for _, y := range x {
			n += countSpawn(y, sp)
		}
	case []any:
		for _, y := range x {
			n += countSpawn(y, sp)
		}
	}
	return n
}

// spawnUnderOld starts a lab pane that joins as the probe agent and spawns the probe profile
// (detached), and returns the spawn ID and the spawns row that OLD wrote.
func (h *rehearsal) spawnUnderOld() (string, map[string]string, error) {
	var client string
	if err := h.withLab(func(lr *releaser) error {
		hub, err := lr.currentHub()
		if err == nil {
			client = hub.Client
		}
		return err
	}); err != nil {
		return "", nil, err
	}
	sb, err := os.ReadFile(filepath.Join(h.run, "session"))
	if err != nil {
		return "", nil, err
	}
	L := h.lab
	R := fmt.Sprintf("rex -S %s --autostart=false --timeout 30s -C %s do", shQuote(h.l.server), shQuote(client))
	script := strings.Join([]string{"#!/bin/sh",
		fmt.Sprintf(`%s hub.join --args '{"agent":"%s"}' > %s 2>&1`, R, relProbeAgent, shQuote(filepath.Join(L, "agent.join"))),
		fmt.Sprintf(`%s hub.spawn --args '{"profile":"%s","key":"%s","placement":{"detached":true}}' > %s 2>&1`, R, relProbeProfile, relProbeSpawnKey, shQuote(filepath.Join(L, "agent.spawn"))),
		"echo done > " + shQuote(filepath.Join(L, "agent.done")),
		"read x", ""}, "\n")
	agentSh := filepath.Join(L, "agent.sh")
	if err := os.WriteFile(agentSh, []byte(script), 0o700); err != nil {
		return "", nil, err
	}
	// The rex CLI, as devStart: a session.new_block of the Go client needs a stream attached to the
	// session.
	arg, _ := json.Marshal(map[string]any{"session_id": strings.TrimSpace(string(sb)), "flavor": "com.superlogical.terminal.shell",
		"label": relAgentLabel, "options": map[string]any{"command": []string{"/bin/sh", agentSh}, "login": false, "exit": map[string]bool{"on_completion": false}}})
	if err := h.withLab(func(lr *releaser) error {
		_, err := lr.rex("api", "call", "session.new_block", string(arg))
		return err
	}); err != nil {
		return "", nil, relFail("the agent pane: %v", err)
	}
	for i := 0; i < 30; i++ {
		if _, err := os.Stat(filepath.Join(L, "agent.done")); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	join, spawn := mustReadFile(filepath.Join(L, "agent.join")), mustReadFile(filepath.Join(L, "agent.spawn"))
	h.say("agent pane: join %s; spawn %s", strings.TrimSpace(string(join)), strings.TrimSpace(string(spawn)))
	var s struct {
		SpawnID string `json:"spawn_id"`
	}
	if json.Unmarshal(spawn, &s) != nil || s.SpawnID == "" {
		return "", nil, relFail("no spawn ID from OLD (join %q, spawn %q)", join, spawn)
	}
	row, err := h.spawnRow(s.SpawnID)
	if err != nil {
		return "", nil, err
	}
	if row == nil || row["status"] != typedSQL("open") {
		return "", nil, relFail("the spawns row of %s by OLD is %v, not an open row", s.SpawnID, row)
	}
	return s.SpawnID, row, nil
}

func (h *rehearsal) spawnRow(sp string) (map[string]string, error) {
	db, err := h.hubDB(true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := queryRows(db, "SELECT * FROM spawns WHERE spawn_id = ?", sp)
	if err != nil {
		return nil, relFail("the spawns row of %s: %v", sp, err)
	}
	if len(rows) != 1 {
		return nil, relFail("%d spawns rows have the spawn ID %s", len(rows), sp)
	}
	return rows[0], nil
}

// checkSpawn checks that each column of the row that OLD wrote has the same value now.
func (h *rehearsal) checkSpawn(sp string, old map[string]string, when string) error {
	now, err := h.spawnRow(sp)
	if err != nil {
		return err
	}
	var diff, added []string
	for k, v := range old {
		if now[k] != v {
			diff = append(diff, fmt.Sprintf("%s %q -> %q", k, v, now[k]))
		}
	}
	for k, v := range now {
		if _, ok := old[k]; !ok {
			added = append(added, k+"="+v)
		}
	}
	sort.Strings(diff)
	sort.Strings(added)
	if len(diff) > 0 {
		return relFail("the spawns row %s %s differs from the row of OLD: %s", sp, when, strings.Join(diff, "; "))
	}
	if len(added) > 0 {
		h.say("spawns row %s %s: the columns that OLD did not write: %s", sp, when, strings.Join(added, ", "))
	}
	return nil
}

// queryRows reads every row of a query as column -> typedSQL: each value keeps its SQL type, so a
// NULL, a text, an integer, a real and a blob never compare equal.
func queryRows(db *sql.DB, q string, args ...any) ([]map[string]string, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]string{}
		for i, c := range cols {
			m[c] = typedSQL(vals[i])
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// typedSQL is a scanned SQL value with its type: null, integer:<n>, real:<x>, text:<quoted> or
// blob:<hex>. Another driver type keeps its Go type name.
func typedSQL(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case int64:
		return "integer:" + strconv.FormatInt(x, 10)
	case float64:
		return "real:" + strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return "text:" + strconv.Quote(x)
	case []byte:
		return "blob:" + hex.EncodeToString(x)
	case time.Time:
		return "time:" + x.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%T:%v", v, v)
}

// probeRow is one row of the spec probe file: its rowid and its values (queryRows form).
type probeRow struct {
	rowid int64
	vals  map[string]string
}

// tableRows reads every row of table t by rowid. A table without rowid fails: its rows cannot be
// found again by rowid.
func tableRows(db *sql.DB, t string) (map[int64]map[string]string, error) {
	rows, err := queryRows(db, `SELECT rowid AS relprobe_rowid, * FROM "`+t+`"`)
	if err != nil {
		return nil, err
	}
	out := map[int64]map[string]string{}
	for _, r := range rows {
		s, ok := strings.CutPrefix(r["relprobe_rowid"], "integer:")
		id, err := strconv.ParseInt(s, 10, 64)
		if !ok || err != nil {
			return nil, relFail("the table %s: the rowid %s", t, r["relprobe_rowid"])
		}
		out[id] = r
	}
	return out, nil
}

// dbSnapshot is the content of a hub.db: the schema objects, the rows of each rowid table by
// rowid, and the sorted rows of each table without rowid.
type dbSnapshot struct {
	schema []string
	rowid  map[string]map[int64]map[string]string
	other  map[string][]string
}

// snapshotDB reads every table of db (the sqlite_ internal tables excepted).
func snapshotDB(db *sql.DB) (*dbSnapshot, error) {
	s := &dbSnapshot{rowid: map[string]map[int64]map[string]string{}, other: map[string][]string{}}
	objs, err := queryRows(db, `SELECT type, name, ifnull(sql, '') AS sql FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		s.schema = append(s.schema, o["type"]+" "+o["name"]+" "+o["sql"])
		name, _ := strconv.Unquote(strings.TrimPrefix(o["name"], "text:"))
		if o["type"] != `text:"table"` || strings.HasPrefix(name, "sqlite_") {
			continue
		}
		if strings.Contains(strings.ToUpper(o["sql"]), "WITHOUT ROWID") {
			rows, err := queryRows(db, `SELECT * FROM "`+name+`"`)
			if err != nil {
				return nil, relFail("the table %s: %v", name, err)
			}
			var enc []string
			for _, r := range rows {
				b, _ := json.Marshal(r)
				enc = append(enc, string(b))
			}
			sort.Strings(enc)
			s.other[name] = enc
			continue
		}
		if s.rowid[name], err = tableRows(db, name); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// writeFileProbes runs the spec probe file (the SQL rows for the tables that H adds) in the lab
// hub.db, while the hub is stopped. It reads every table before and after the file, and tracks
// every row that is new or changed, in any table and at any rowid. checkFileProbes compares them
// after each later phase. A file that deletes a row, changes the schema or a table without rowid,
// or writes no row refuses: the check could not see all of its effects.
func (h *rehearsal) writeFileProbes() error {
	if h.r.spec.Probes == "" {
		h.say("the spec has no probe file: no file probe rows to check")
		return nil
	}
	b, err := os.ReadFile(filepath.Join(h.lab, "probes.sql"))
	if err != nil {
		return err
	}
	db, err := h.hubDB(false)
	if err != nil {
		return err
	}
	defer db.Close()
	before, err := snapshotDB(db)
	if err != nil {
		return relFail("the lab hub.db before the probe file: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(string(b)); err != nil {
		tx.Rollback()
		return relFail("the probe file: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	after, err := snapshotDB(db)
	if err != nil {
		return relFail("the lab hub.db after the probe file: %v", err)
	}
	if strings.Join(before.schema, "\n") != strings.Join(after.schema, "\n") {
		return relFail("the probe file changed the schema; a probe file may only insert or change rows")
	}
	for t, rows := range before.other {
		if strings.Join(rows, "\n") != strings.Join(after.other[t], "\n") {
			return relFail("the probe file changed the table %s, which has no rowid; the check cannot track its rows", t)
		}
	}
	h.fileProbes = map[string][]probeRow{}
	n := 0
	for t, rows := range after.rowid {
		for id := range before.rowid[t] {
			if _, ok := rows[id]; !ok {
				return relFail("the probe file deleted the row %s rowid %d; a probe file may only insert or change rows", t, id)
			}
		}
		for id, r := range rows {
			if old, ok := before.rowid[t][id]; !ok || !sameRow(old, r) {
				h.fileProbes[t] = append(h.fileProbes[t], probeRow{id, r})
			}
		}
		sort.Slice(h.fileProbes[t], func(i, j int) bool { return h.fileProbes[t][i].rowid < h.fileProbes[t][j].rowid })
		n += len(h.fileProbes[t])
	}
	if n == 0 {
		return relFail("the probe file wrote no row")
	}
	h.say("the probe file wrote %d rows in %d tables", n, len(h.fileProbes))
	return nil
}

// checkFileProbes checks that each row of the probe file is the same, by rowid, with each value's
// SQL type.
func (h *rehearsal) checkFileProbes(phase string) error {
	if len(h.fileProbes) == 0 {
		return nil
	}
	db, err := h.hubDB(true)
	if err != nil {
		return err
	}
	defer db.Close()
	n := 0
	for t, rows := range h.fileProbes {
		for _, want := range rows {
			got, err := queryRows(db, `SELECT rowid AS relprobe_rowid, * FROM "`+t+`" WHERE rowid = ?`, want.rowid)
			if err != nil {
				return relFail("phase %s: the probe table %s: %v", phase, t, err)
			}
			if len(got) != 1 || !sameRow(got[0], want.vals) {
				return relFail("phase %s: the probe row %s rowid %d is %v, not %v", phase, t, want.rowid, got, want.vals)
			}
			n++
		}
	}
	h.say("file probes %s: %d rows are exact", phase, n)
	return nil
}

func sameRow(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// pullStopPhase: pull stops after its mutation (before the end record), with the pins at
// H; a second pull finds "to to" and passes; then rollback.
func (h *rehearsal) pullStopPhase() error {
	if err := h.steps("precheck", "stop", "backup", "checkout"); err != nil {
		return err
	}
	if err := h.stepCode("pull", []string{"RELEASE_TEST_STOP=pull-fwd:after"}, func(c int) bool { return c == relTestStop }, "the test stop"); err != nil {
		return err
	}
	if err := h.pinIs("to to"); err != nil {
		return err
	}
	if err := h.steps("pull"); err != nil {
		return err
	}
	return h.rollbackAs("pull-stop")
}

// partialPhase: after pull, the backup base ("to from") or the backup lock ("from to")
// is put back, as a stop between the lock write and the base write leaves it. Then rollback.
func (h *rehearsal) partialPhase(state string) error {
	if err := h.steps("precheck", "stop", "backup", "checkout", "pull"); err != nil {
		return err
	}
	if err := h.pinIs("to to"); err != nil {
		return err
	}
	if err := h.withLab(func(lr *releaser) error {
		b, err := lr.currentBackup()
		if err != nil {
			return err
		}
		src, dst := filepath.Join(b, bakLock), lr.cfg.lock
		if state == "to from" {
			src, dst = filepath.Join(b, bakBase()), peersPath(lr.cfg.lock)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return replaceFile(dst, data)
	}); err != nil {
		return err
	}
	if err := h.pinIs(state); err != nil {
		return err
	}
	h.say("partial pull: the pin state is %q", state)
	return h.rollbackAs("partial " + state)
}

// failGo writes <lab>/failgo/go: it logs each call (cwd and arguments) to <lab>/failgo/calls,
// exits 1 for "go build", and runs the real go for every other call. It returns the extra
// variable that puts it first on the child PATH.
func (h *rehearsal) failGo() ([]string, string, error) {
	path := ""
	for _, kv := range h.env {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = p
		}
	}
	real := ""
	for _, d := range filepath.SplitList(path) {
		if st, err := os.Stat(filepath.Join(d, "go")); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			real = filepath.Join(d, "go")
			break
		}
	}
	if real == "" {
		return nil, "", relFail("no go command on the lab PATH")
	}
	dir := filepath.Join(h.lab, "failgo")
	calls := filepath.Join(dir, "calls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(calls, nil, 0o600); err != nil {
		return nil, "", err
	}
	script := failGoScript(calls, real)
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o700); err != nil {
		return nil, "", err
	}
	return []string{"PATH=" + dir + string(filepath.ListSeparator) + path}, calls, nil
}

func failGoScript(calls, real string) string {
	return "#!/bin/sh\n" +
		"printf '%s\\t%s\\n' \"$PWD\" \"$*\" >> " + shQuote(calls) + "\n" +
		"case \"$1\" in build) exit 1 ;; esac\n" +
		"exec " + shQuote(real) + " \"$@\"\n"
}

// goBuilds lists the cwd of each "go build" call in the calls log.
func goBuilds(calls string) []string {
	var out []string
	for _, line := range strings.Split(string(mustReadFile(calls)), "\n") {
		if cwd, args, ok := strings.Cut(line, "\t"); ok && strings.HasPrefix(args, "build") {
			out = append(out, cwd)
		}
	}
	return out
}

// buildFailsPhase: pull with the failing go command fails at the build, before any pin
// moves; no H binary stays (the one of the earlier phases is removed first). Then rollback with
// the real PATH.
func (h *rehearsal) buildFailsPhase() error {
	if err := h.steps("precheck", "stop", "backup", "checkout"); err != nil {
		return err
	}
	extra, calls, err := h.failGo()
	if err != nil {
		return err
	}
	if err := h.stepCode("pull", extra, func(c int) bool { return c != relOK && c != relTestStop }, "a failure"); err != nil {
		return err
	}
	if len(goBuilds(calls)) == 0 {
		return relFail("the calls log %s has no go build", calls)
	}
	if err := h.withLab(func(lr *releaser) error {
		if _, err := os.Stat(lr.hubBin()); !errors.Is(err, fs.ErrNotExist) {
			return relFail("after the failed build, the H binary %s exists (%v)", lr.hubBin(), err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := h.pinIs("from from"); err != nil {
		return err
	}
	h.say("build fails: no H binary, the pins at OLD")
	return h.rollbackAs("build-fails")
}

// startFailsPhase: start with the failing go command. dev.sh in the dev pane runs the
// go of the child PATH, so its build fails and no new hub starts; the step fails and the dev pane
// stays. Then rollback with the real PATH closes it.
func (h *rehearsal) startFailsPhase() error {
	if err := h.steps("precheck", "stop", "backup", "checkout", "pull"); err != nil {
		return err
	}
	extra, calls, err := h.failGo()
	if err != nil {
		return err
	}
	if err := h.stepCode("start", extra, func(c int) bool { return c != relOK && c != relTestStop }, "a failure"); err != nil {
		return err
	}
	clone := filepath.Join(h.lab, "release", "LazyRex")
	inClone := false
	for _, cwd := range goBuilds(calls) {
		if cwd == clone || strings.HasPrefix(cwd, clone+"/") {
			inClone = true
		}
	}
	if !inClone {
		return relFail("the calls log %s has no go build in the lab clone %s: dev.sh did not run the failing go", calls, clone)
	}
	if err := h.withLab(func(lr *releaser) error {
		panes, err := lr.devPanes()
		if err != nil {
			return err
		}
		if len(panes) != 1 {
			return relFail("after the failed start, %d dev panes run, not 1", len(panes))
		}
		return nil
	}); err != nil {
		return err
	}
	h.say("start fails: dev.sh ran the failing go in the lab clone; the dev pane stays")
	return h.rollbackAs("start-fails")
}

// resumePhase: backup stops after its begin record of the stamp, so no stamp exists;
// resume starts OLD again.
func (h *rehearsal) resumePhase() error {
	if err := h.steps("precheck", "stop"); err != nil {
		return err
	}
	if err := h.stepCode("backup", []string{"RELEASE_TEST_STOP=stamp:begin"}, func(c int) bool { return c == relTestStop }, "the test stop"); err != nil {
		return err
	}
	if err := h.withLab(func(lr *releaser) error {
		if s, _ := lr.stamp(); s != "" {
			return relFail("the backup stopped before the stamp, but the stamp %s exists", s)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := h.steps("resume"); err != nil {
		return err
	}
	return h.back("resume")
}

// hEndedPhase: after a forward run, the dev pane of H closes outside the release steps;
// rollback then makes no hub call to H.
func (h *rehearsal) hEndedPhase() error {
	if err := h.forwardAs("h-ended-forward"); err != nil {
		return err
	}
	if err := h.withLab(func(lr *releaser) error {
		panes, err := lr.devPanes()
		if err != nil {
			return err
		}
		if len(panes) != 1 {
			return relFail("%d dev panes run, not 1", len(panes))
		}
		hub, err := lr.currentHub()
		if err != nil {
			return err
		}
		return lr.devStop(relPane{Session: panes[0][0], Block: panes[0][1], relHub: *hub})
	}); err != nil {
		return err
	}
	h.say("H ended: the dev pane of H is closed")
	return h.rollbackAs("h-ended")
}
