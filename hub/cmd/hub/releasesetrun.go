package main

// The set phase of a release-set rehearsal (plans r3 and r3.1). After the hub forward, with H
// running: the seeds (lab-made data that FROM writes), the reference snapshots, the probes on FROM;
// then each plugin forward in live order as a lab plugin run, with data checks at the step
// boundaries, an idempotent rerun and the probes on TO; then the rollbacks, newest first, with the
// probes on FROM again. The hub rollback and the full tier follow, with every plugin back at FROM.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// relSetState is the set of one rehearsal and what it found.
type relSetState struct {
	file   *relSetFile
	raw    []byte               // the set file bytes (set.json in the live run)
	ref    map[string]*relSnap  // the reference snapshot of each plugin
	runs   map[string]string    // the lab plugin run of each plugin
	checks []relSetCheck        // the record of every data check and probe
	steps  []relSetStep         // each plugin step as its lab freeze saw it, in set order
	faults []relSetFault        // the fault runs (evidence of recovery only)
	seeds  map[string][2]string // <SEED_SESSION_ID> and <SEED_BLOCK_ID>: the pane of each plugin's seed action batch
}

// relSetCheck is one record line: a phase, a plugin and what passed there.
type relSetCheck struct {
	Phase  string       `json:"phase"`
	Plugin string       `json:"plugin"`
	What   string       `json:"what"`
	Diff   *relSnapDiff `json:"diff,omitempty"`
}

func (s *relSetState) note(phase, plugin, what string, d *relSnapDiff) {
	s.checks = append(s.checks, relSetCheck{Phase: phase, Plugin: plugin, What: what, Diff: d})
}

// setPhase runs the set phase.
func (h *rehearsal) setPhase() error {
	s := h.set
	s.ref, s.runs = map[string]*relSnap{}, map[string]string{}
	if err := h.chunkProbe("after the hub forward", h.spDigest); err != nil {
		return err
	}
	for i := range s.file.Plugins {
		e := &s.file.Plugins[i]
		if err := h.runItems(e, e.Seed, "seed", false); err != nil {
			return relFail("the seed of %s: %v", e.Plugin, err)
		}
	}
	for i := range s.file.Plugins {
		e := &s.file.Plugins[i]
		ref, err := h.snapPlugin(e)
		if err != nil {
			return relFail("the reference snapshot of %s: %v", e.Plugin, err)
		}
		if err := checkKeyTables(ref, e.Keys); err != nil {
			return relFail("%s: %v", e.Plugin, err)
		}
		s.ref[e.Plugin] = ref
		s.note("seed", e.Plugin, fmt.Sprintf("reference: user_version %d, %d tables", ref.UserVersion, len(ref.Tables)), nil)
	}
	if err := h.setChecks("seeded"); err != nil {
		return err
	}
	for i := range s.file.Plugins {
		e := &s.file.Plugins[i]
		if err := h.runItems(e, e.Probe, "from-before", true); err != nil {
			return relFail("the probes of %s on FROM: %v", e.Plugin, err)
		}
	}
	if err := h.setChecks("probes on FROM"); err != nil {
		return err
	}
	if err := h.setFaults(&s.file.Plugins[0]); err != nil {
		return err
	}
	for i := range s.file.Plugins {
		if err := h.pluginForward(&s.file.Plugins[i]); err != nil {
			return err
		}
	}
	if err := h.chunkProbe("after the last plugin forward", h.spDigest); err != nil {
		return err
	}
	if err := h.setChecks("chunking probe"); err != nil {
		return err
	}
	for i := len(s.file.Plugins) - 1; i >= 0; i-- {
		if err := h.pluginRollback(&s.file.Plugins[i]); err != nil {
			return err
		}
	}
	return nil
}

// pluginForward moves one plugin to TO in a lab plugin run: freeze, precheck, backup, pull,
// replace, verify, with data checks after backup, replace and verify; then a rerun of replace and
// verify that must change nothing; then the probes on TO.
func (h *rehearsal) pluginForward(e *relSetEntry) error {
	run := filepath.Join(h.lab, "set", e.Plugin, "run")
	spec := map[string]any{"kind": "plugin", "plugin": e.Plugin, "to": e.To, "config": h.cfg, "server": h.l.server, "lab": true}
	specPath := filepath.Join(h.lab, "set", e.Plugin, "spec.json")
	if err := os.MkdirAll(filepath.Dir(specPath), 0o700); err != nil {
		return err
	}
	if err := writeJSON(specPath, spec); err != nil {
		return err
	}
	if err := h.child("freeze", nil, "-spec", specPath, "-run", run); err != nil {
		return relFail("%s: %v", e.Plugin, err)
	}
	h.set.runs[e.Plugin] = run
	if err := h.recordStep(e, run); err != nil {
		return err
	}
	step := func(name string) error {
		if err := h.child(name, nil, "-run", run); err != nil {
			return relFail("%s: %v", e.Plugin, err)
		}
		return nil
	}
	for _, st := range []string{"precheck", "backup", "pull", "replace", "verify"} {
		if err := step(st); err != nil {
			return err
		}
		if st == "backup" || st == "replace" || st == "verify" {
			if err := h.setChecks(e.Plugin + " " + st); err != nil {
				return err
			}
		}
	}
	// The rerun: the same pins and the same data. Each plugin's data after the rerun must equal its
	// snapshot right before the rerun exactly; the cumulative rules against the seed reference
	// still apply.
	pre := map[string]*relSnap{}
	for i := range h.set.file.Plugins {
		p := &h.set.file.Plugins[i]
		s, err := h.snapPlugin(p)
		if err != nil {
			return relFail("the snapshot of %s before the rerun: %v", p.Plugin, err)
		}
		pre[p.Plugin] = s
	}
	for _, st := range []string{"replace", "verify"} {
		if err := step(st); err != nil {
			return relFail("the rerun of %v", err)
		}
	}
	for i := range h.set.file.Plugins {
		p := &h.set.file.Plugins[i]
		s, err := h.snapPlugin(p)
		if err != nil {
			return relFail("the snapshot of %s after the rerun: %v", p.Plugin, err)
		}
		if err := sameSnap(pre[p.Plugin], s); err != nil {
			return relFail("the rerun of replace and verify of %s changed the data of %s: %v", e.Plugin, p.Plugin, err)
		}
	}
	if err := h.setChecks(e.Plugin + " rerun"); err != nil {
		return err
	}
	if err := h.runItems(e, e.Probe, "to", true); err != nil {
		return relFail("the probes of %s on TO: %v", e.Plugin, err)
	}
	return h.setChecks(e.Plugin + " probes on TO")
}

// pluginRollback rolls one plugin back to FROM in its lab plugin run, then runs its probes on FROM.
func (h *rehearsal) pluginRollback(e *relSetEntry) error {
	run := h.set.runs[e.Plugin]
	if err := h.child("rollback", nil, "-run", run); err != nil {
		return relFail("%s: %v", e.Plugin, err)
	}
	if err := h.setChecks(e.Plugin + " rollback"); err != nil {
		return err
	}
	if err := h.runItems(e, e.Probe, "from-after", true); err != nil {
		return relFail("the probes of %s on FROM after the rollback: %v", e.Plugin, err)
	}
	return h.setChecks(e.Plugin + " probes after the rollback")
}

// pluginDBPath is the SQL database of a plugin in the lab, "" when it has none.
func (h *rehearsal) pluginDBPath(plugin string) (string, error) {
	labCfg, err := readPluginCmdConfig(h.cfg)
	if err != nil {
		return "", err
	}
	p := filepath.Join(labCfg.state, "sql", plugin+".db")
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return p, nil
}

// openRO opens an SQLite file read-only.
func openRO(p string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+p+"?mode=ro&_pragma=busy_timeout(5000)")
}

// snapPlugin reads the current snapshot of a plugin.
func (h *rehearsal) snapPlugin(e *relSetEntry) (*relSnap, error) {
	hub, err := h.hubDB(true)
	if err != nil {
		return nil, err
	}
	defer hub.Close()
	p, err := h.pluginDBPath(e.Plugin)
	if err != nil {
		return nil, err
	}
	var pdb *sql.DB
	if p != "" {
		if pdb, err = openRO(p); err != nil {
			return nil, err
		}
		defer pdb.Close()
	}
	return newSnap(pdb, hub, e.Plugin, e.Keys)
}

// setChecks checks the data of every plugin of the set against its reference, and runs the
// invariants.
func (h *rehearsal) setChecks(phase string) error {
	for i := range h.set.file.Plugins {
		e := &h.set.file.Plugins[i]
		cur, err := h.snapPlugin(e)
		if err != nil {
			return relFail("%s, %s: %v", phase, e.Plugin, err)
		}
		p, err := h.pluginDBPath(e.Plugin)
		if err != nil {
			return err
		}
		var d *relSnapDiff
		check := func(pdb *sql.DB) error {
			eval := func(string, relCell, relCell) (bool, error) {
				return false, errors.New("the plugin has no database for a changes predicate")
			}
			if pdb != nil {
				eval = sqlPredicate(pdb)
			}
			if d, err = checkSnap(h.set.ref[e.Plugin], cur, e, eval); err != nil {
				return err
			}
			if pdb != nil {
				return checkInvariants(pdb, e)
			}
			return nil
		}
		if p != "" {
			pdb, err := openRO(p)
			if err != nil {
				return err
			}
			err = check(pdb)
			pdb.Close()
			if err != nil {
				return relFail("%s, %s: %v", phase, e.Plugin, err)
			}
		} else if err := check(nil); err != nil {
			return relFail("%s, %s: %v", phase, e.Plugin, err)
		}
		h.set.note(phase, e.Plugin, "data ok", d)
	}
	h.say("set data %s: the data of %d plugins passes", phase, len(h.set.file.Plugins))
	return nil
}

// runItems runs seed or probe items of one entry. The action items run in one agent batch; the
// view and sql items run one by one, in order after the batch. check: apply the expects.
func (h *rehearsal) runItems(e *relSetEntry, items []relSetItem, phase string, check bool) error {
	var acts []relSetItem
	var idx []int
	for i, it := range items {
		if it.Kind == "action" {
			acts, idx = append(acts, it), append(idx, i)
		}
	}
	if len(acts) > 0 {
		outs, err := h.agentBatch(e.Plugin, phase, acts)
		if err != nil {
			return err
		}
		for j, o := range outs {
			it := acts[j]
			if o.Code != 0 {
				return relFail("item %d (%s) exited %d: %s", idx[j]+1, it.Action, o.Code, strings.TrimSpace(string(o.Out)))
			}
			if check {
				if err := checkFilled(o.Out, it.Expect, o.Vals); err != nil {
					return relFail("item %d (%s): %v", idx[j]+1, it.Action, err)
				}
			}
			h.set.note(phase, e.Plugin, "action "+it.Action+" ok", nil)
		}
	}
	for i, it := range items {
		var err error
		switch it.Kind {
		case "view":
			err = h.viewItem(e, it, phase, check)
		case "sql":
			err = h.sqlItem(e, it)
		default:
			continue
		}
		if err != nil {
			return relFail("item %d (%s): %v", i+1, it.Kind, err)
		}
		h.set.note(phase, e.Plugin, it.Kind+" item ok", nil)
	}
	return nil
}

// sqlItem runs a sql probe on the plugin database (read-only) and compares the typed rows exactly.
func (h *rehearsal) sqlItem(e *relSetEntry, it relSetItem) error {
	p, err := h.pluginDBPath(e.Plugin)
	if err != nil {
		return err
	}
	if p == "" {
		return relFail("the plugin has no database")
	}
	db, err := openRO(p)
	if err != nil {
		return err
	}
	defer db.Close()
	var args []any
	for _, raw := range it.Params {
		c, err := cellOfJSON(raw)
		if err != nil {
			return err
		}
		args = append(args, c.sqlValue())
	}
	rows, err := db.Query(it.SQL, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	var got [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		var row []string
		for _, v := range vals {
			c, err := cellOf(v)
			if err != nil {
				return err
			}
			j, err := c.jsonValue()
			if err != nil {
				return err
			}
			row = append(row, j)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	want := *it.Rows
	if len(got) != len(want) {
		return relFail("%q gave %d rows, not %d: %v", it.SQL, len(got), len(want), got)
	}
	for i := range want {
		if len(want[i]) != len(got[i]) {
			return relFail("%q row %d has %d values, not %d", it.SQL, i+1, len(got[i]), len(want[i]))
		}
		for j, w := range want[i] {
			wc, err := canonicalJSON(w)
			if err != nil {
				return err
			}
			if string(wc) != got[i][j] {
				return relFail("%q row %d value %d is %s, not %s", it.SQL, i+1, j+1, got[i][j], wc)
			}
		}
	}
	return nil
}

// labViewWrapper writes the lab renderer: <lab>/view runs this tool's test renderer (testview) on
// the lab server. A lab transform: the lab has no user, and the tool drives views through it.
func labViewWrapper(lab, tool string) (relRewrite, error) {
	if err := writeLabView(lab, tool); err != nil {
		return relRewrite{}, err
	}
	return relRewrite{Key: "lab.view", Lab: filepath.Join(lab, "view") + " (testview of the tool, lab only)"}, nil
}

// writeLabView writes <lab>/view with a rename, as dev.sh installs a renderer. Each hub start
// (dev.sh) builds the real renderer over that path, so each view item writes the wrapper again.
func writeLabView(lab, tool string) error {
	p := filepath.Join(lab, "view")
	server := "unix://" + filepath.Join(lab, "rex.sock")
	script := fmt.Sprintf("#!/bin/sh\nREX_SERVER=%s exec %s testview \"$@\"\n", shQuote(server), shQuote(tool))
	if err := os.WriteFile(p+".new", []byte(script), 0o700); err != nil {
		return err
	}
	return os.Rename(p+".new", p)
}

// viewItem runs one view item as the user (plan r3.1, 2b): a source pane in pane_dir, the palette
// command through hub.palette.run, the lab renderer's view, the node action with the item's key
// (with the phase appended in a probe, so each phase has its own key), the wait for the final
// operation, the expects, a replay of the same operation, and then the close of the view and the
// pane.
func (h *rehearsal) viewItem(e *relSetEntry, it relSetItem, phase string, check bool) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := filepath.Join(h.lab, strings.TrimPrefix(it.PaneDir, "<LAB>/"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key := it.OperationKey
	if phase != "seed" {
		key += "-" + phase
	}
	// The hub start built the real renderer over <lab>/view; the hub execs that path for this view.
	if err := writeLabView(h.lab, h.tool); err != nil {
		return relFail("the lab renderer: %v", err)
	}
	var hubClient string
	if err := h.withLab(func(lr *releaser) error {
		hub, err := lr.currentHub()
		if err == nil {
			hubClient = hub.Client
		}
		return err
	}); err != nil {
		return relFail("the source pane: %v", err)
	}
	// The source pane: a new lab session whose one window shows it, in dir. The hub refuses a
	// palette command from a block that no window shows. The pane reports its directory with
	// OSC 7, as a shell does, and paneSession waits for that report.
	osc := `printf '\033]7;file://localhost%s\033\\' "$PWD"; exec sleep 3600`
	session, block, err := h.paneSession("source-"+key, dir, []string{"/bin/sh", "-c", osc})
	if err != nil {
		if session != "" {
			h.l.r.Call(context.Background(), "session.destroy", map[string]any{"session_id": session}, nil)
		}
		return relFail("the source pane: %v", err)
	}
	// A failed cleanup fails the item, unless the item already failed. The palette view (a form or
	// a panel) is in the side slot of the source window: the session close ends it, as a user's
	// close of its pane does. The hub refuses hub.view.close for a form. The renderer of the view
	// must then disconnect.
	c := &helperConn{r: h.l.r}
	tv := ""
	defer func() {
		cerr := h.closePane(session, block, "")
		if cerr == nil {
			cerr = h.l.r.Call(context.Background(), "session.destroy", map[string]any{"session_id": session}, nil)
		}
		if cerr == nil && tv != "" {
			cerr = waitClientGone(inspectClient(c, tv), tv, 10*time.Second)
		}
		if cerr != nil && err == nil {
			err = relFail("closing the source pane: %v", cerr)
		}
	}()
	self, err := h.runnerClient(ctx, c)
	if err != nil {
		return err
	}
	raw, err := c.perform(ctx, hubClient, "hub.palette.run", map[string]any{"command": it.Command,
		"ctx": map[string]any{"client_id": self, "session_id": session, "block_id": block}})
	if err != nil {
		return relFail("hub.palette.run %s: %v", it.Command, err)
	}
	var opened struct {
		ViewID string `json:"view_id"`
		Route  string `json:"route"`
	}
	if json.Unmarshal(raw, &opened) != nil || opened.ViewID == "" {
		return relFail("hub.palette.run %s gave no view: %s", it.Command, raw)
	}
	var rerr error
	for i := 0; i < 100 && tv == ""; i++ {
		tv, rerr = c.resolveClient(ctx, "lazyrex-testview:"+opened.ViewID)
		if tv == "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if tv == "" {
		return relFail("no lab renderer attached the view %s: %v", opened.ViewID, rerr)
	}
	snap, err := waitViewAction(ctx, func(ctx context.Context) (json.RawMessage, error) {
		return c.perform(ctx, tv, "testview.get", map[string]any{"route": it.Route})
	}, it.Route, it.NodeID, it.Action)
	if err != nil {
		return relFail("the view %s: %v", opened.ViewID, err)
	}
	input := it.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	filled, err := fillPlaceholders(input, h.placeholders(e.Plugin, session, block))
	if err != nil {
		return relFail("the input of the view action %s: %v", it.Action, err)
	}
	act := map[string]any{"node_id": it.NodeID, "action": it.Action, "revision": snap.Revision, "operation_key": key,
		"input": json.RawMessage(filled)}
	result, err := h.viewOperation(ctx, c, tv, act, key)
	if err != nil {
		return err
	}
	if check {
		if err := checkFilled(result, it.Expect, h.placeholders(e.Plugin, session, block)); err != nil {
			return err
		}
	}
	// The replay: the same key and arguments give the recorded result and no new effect. The data
	// after the replay must equal the data after the first operation exactly.
	before, err := h.snapPlugin(e)
	if err != nil {
		return relFail("the snapshot before the replay of %s: %v", key, err)
	}
	again, err := h.viewOperation(ctx, c, tv, act, key)
	if err != nil {
		return relFail("the replay of %s: %v", key, err)
	}
	ac, _ := canonicalJSON(result)
	bc, _ := canonicalJSON(again)
	if string(ac) != string(bc) {
		return relFail("the replay of %s gave %s, not %s", key, bc, ac)
	}
	after, err := h.snapPlugin(e)
	if err != nil {
		return relFail("the snapshot after the replay of %s: %v", key, err)
	}
	if err := sameSnap(before, after); err != nil {
		return relFail("the replay of %s changed data: %v", key, err)
	}
	return nil
}

// viewOperation sends one view action and waits for its final operation: succeeded, with every
// reported effect succeeded. It returns the result.
func (h *rehearsal) viewOperation(ctx context.Context, c *helperConn, tv string, act map[string]any, key string) (json.RawMessage, error) {
	if _, err := c.perform(ctx, tv, "testview.action", act); err != nil {
		return nil, relFail("hub.view.action %s: %v", key, err)
	}
	for {
		raw, err := c.perform(ctx, tv, "testview.operation", map[string]any{"operation_key": key})
		if err != nil {
			return nil, relFail("hub.view.operation %s: %v", key, err)
		}
		var op struct {
			Status  string            `json:"status"`
			Result  json.RawMessage   `json:"result"`
			Error   json.RawMessage   `json:"error"`
			Effects []json.RawMessage `json:"effects"`
		}
		if err := json.Unmarshal(raw, &op); err != nil {
			return nil, relFail("hub.view.operation %s: %s", key, raw)
		}
		switch op.Status {
		case "pending":
			select {
			case <-ctx.Done():
				return nil, relFail("the operation %s is still pending at the deadline", key)
			case <-time.After(200 * time.Millisecond):
			}
			continue
		case "succeeded":
			for _, ef := range op.Effects {
				var st struct {
					Status string `json:"status"`
				}
				if json.Unmarshal(ef, &st) != nil || st.Status != "succeeded" {
					return nil, relFail("the operation %s has an effect that did not succeed: %s", key, ef)
				}
			}
			if len(op.Result) == 0 {
				return nil, relFail("the operation %s succeeded with no result", key)
			}
			return op.Result, nil
		default:
			return nil, relFail("the operation %s is %s: %s", key, op.Status, raw)
		}
	}
}

// drainEvents reads and drops the events of the tool's lab connection. The tool attaches each
// batch and source session (event_scope session) and reads no event. The read loop of the Rex
// client blocks when its event buffer (256) is full, and then no reply arrives (LAB16).
func drainEvents(r *rex.Rex) {
	go func() {
		for range r.Events {
		}
	}()
}

// inspectClient gives the connection state of a client (client.inspect).
func inspectClient(c *helperConn, client string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		var ins struct {
			Client struct {
				ConnectionState string `json:"connection_state"`
			} `json:"client"`
		}
		err := c.r.Call(ctx, "client.inspect", map[string]any{"client_id": client}, &ins)
		return ins.Client.ConnectionState, err
	}
}

// waitClientGone waits until the client is not connected. Rex forgets a client after its
// disconnect: client.inspect then refuses with not_found, which also counts as gone.
func waitClientGone(inspect func(context.Context) (string, error), client string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	last := ""
	for {
		state, err := inspect(ctx)
		var re *rex.RexError
		switch {
		case errors.As(err, &re) && re.Code == "not_found":
			return nil
		case err != nil:
			last = err.Error()
		case state != "connected":
			return nil
		default:
			last = state
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the renderer %s did not disconnect in %s (the last state: %s)", client, wait, last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type relViewSnap struct {
	Route    string          `json:"route"`
	Revision json.RawMessage `json:"revision"`
	Tree     json.RawMessage `json:"tree"`
}

// waitViewAction gets the view until it is at route and its tree has the node id with the action.
// The hub gives a loading tree (revision 0) while the plugin snapshot is pending, so the first
// reply can be a success without the node. At the end of ctx, the error gives the last reply.
func waitViewAction(ctx context.Context, get func(context.Context) (json.RawMessage, error), route, id, action string) (relViewSnap, error) {
	var snap relViewSnap
	var last string
	for {
		snap = relViewSnap{}
		got, err := get(ctx)
		switch {
		case err != nil:
			last = err.Error()
		case json.Unmarshal(got, &snap) != nil:
			last = "not a view snapshot: " + string(got)
		case snap.Route != route:
			last = fmt.Sprintf("at %q, not %s", snap.Route, route)
		case !treeHasAction(snap.Tree, id, action):
			last = fmt.Sprintf("no node %s with the action %s: %s", id, action, got)
		default:
			return snap, nil
		}
		select {
		case <-ctx.Done():
			return relViewSnap{}, fmt.Errorf("%v; the last reply: %s", ctx.Err(), last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// treeHasAction reports whether a view tree has the node id with an action named action, on the
// node or on one of its items.
func treeHasAction(tree json.RawMessage, id, action string) bool {
	var v any
	if json.Unmarshal(tree, &v) != nil {
		return false
	}
	var walk func(x any, inNode bool) bool
	walk = func(x any, inNode bool) bool {
		switch t := x.(type) {
		case map[string]any:
			here := inNode || t["id"] == id
			if here {
				if a, ok := t["action"].(map[string]any); ok && a["name"] == action {
					return true
				}
			}
			for _, c := range t {
				if walk(c, here) {
					return true
				}
			}
		case []any:
			for _, c := range t {
				if walk(c, inNode) {
					return true
				}
			}
		}
		return false
	}
	return walk(v, false)
}

// runnerClient labels the tool's own lab connection and returns its client ID: the ctx.client_id
// of hub.palette.run must be the calling client.
func (h *rehearsal) runnerClient(ctx context.Context, c *helperConn) (string, error) {
	label := "lazyrex-release-runner-" + filepath.Base(h.lab)
	if err := c.r.Call(ctx, "client.set_label", map[string]any{"label": label}, nil); err != nil {
		return "", err
	}
	return c.resolveClient(ctx, label)
}

// loadSet reads the -set file into the live run as set.json, which the record binds. Without
// -set, a run that has a set.json is refused: the record would bind a set that did not run.
func (h *rehearsal) loadSet() error {
	p := filepath.Join(h.r.run, relSetJSON)
	if h.r.rehearseSet == "" {
		if _, err := os.Stat(p); err == nil {
			return relFail("the run has %s from an earlier set rehearsal; pass -set, or remove it", relSetJSON)
		}
		return nil
	}
	b, err := os.ReadFile(h.r.rehearseSet)
	if err != nil {
		return relFail("the set file: %v", err)
	}
	f, err := parseSet(b)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	h.set = &relSetState{file: f, raw: b}
	h.say("the set: %d plugin releases, bound as %s", len(f.Plugins), relSetJSON)
	return nil
}

// setNeed checks each set entry against the live lock (the plugin is in the lock, and FROM is its
// pin) and adds FROM and TO to the commits that the lab mirrors must hold.
func (h *rehearsal) setNeed(need map[string][]string, l *pluginLock) error {
	for _, e := range h.set.file.Plugins {
		var pin *lockEntry
		for _, le := range l.Plugins {
			if le.Name == e.Plugin {
				if pin != nil {
					return relFail("the live lock has two entries named %s", e.Plugin)
				}
				pin = le
			}
		}
		if pin == nil {
			return relFail("the set names %s, which the live lock does not have", e.Plugin)
		}
		if pin.Commit != e.From {
			return relFail("the set gives %s from %s, but the live lock pins %s", e.Plugin, e.From, pin.Commit)
		}
		k := pin.Owner + "/" + pin.Repo
		need[k] = append(need[k], e.From, e.To)
	}
	return nil
}

// setLabFiles adds the lab parts that the set items need, and returns them for the transform
// record: the seed agent and the peer agent in the lab config, the inert accessory and agent
// profiles (lab_inert, lab_agent: command true), the lab renderer (when a view item exists), the
// lab source directory and the lab Git repository.
func (h *rehearsal) setLabFiles(lab, cfgPath string) ([]relRewrite, error) {
	// It writes lab files only: the live config and profiles never get the lab agents or profiles.
	if err := checkPath(cfgPath, []string{lab}); err != nil {
		return nil, relFail("the set lab config: %v", err)
	}
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
		if a, _ := x.(map[string]any); a["id"] == relSeedAgent || a["id"] == relPeerAgent {
			return nil, relFail("the live config has the agent %v, which the set rehearsal adds", a["id"])
		}
	}
	cfg["agents"] = append(agents, map[string]any{"id": relSeedAgent, "name": "Release seed"}, map[string]any{"id": relPeerAgent, "name": "Release peer"})
	out := []relRewrite{{Key: "config.agents." + relSeedAgent, Lab: "added (lab only)"}, {Key: "config.agents." + relPeerAgent, Lab: "added (lab only)"}}
	p, _ := cfg["profiles"].(string)
	if err := checkPath(p, []string{lab}); err != nil {
		return nil, relFail("the set lab profiles: %v", err)
	}
	pf := map[string]any{}
	if pb, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(pb, &pf); err != nil {
			return nil, relFail("the lab profiles: %v", err)
		}
	} else {
		return nil, relFail("the lab profiles %s: %v", p, err)
	}
	list, _ := pf["profiles"].([]any)
	for _, x := range list {
		if q, _ := x.(map[string]any); q["id"] == relLabInert || q["id"] == relLabAgent {
			return nil, relFail("the live profiles have the profile %v, which the set rehearsal adds", q["id"])
		}
	}
	// An accessory profile (the Profiles view path) and an agent profile (an automation needs an
	// agent profile). Both run only "true".
	pf["profiles"] = append(list, map[string]any{"id": relLabInert, "name": "Release lab inert", "kind": "accessory", "command": []string{"true"}},
		map[string]any{"id": relLabAgent, "name": "Release lab agent", "kind": "agent", "command": []string{"true"}})
	out = append(out, relRewrite{Key: "profiles." + relLabInert, Lab: "added (lab only): command true"}, relRewrite{Key: "profiles." + relLabAgent, Lab: "added (lab only): kind agent, command true"})
	views := false
	for _, e := range h.set.file.Plugins {
		for _, it := range append(append([]relSetItem{}, e.Seed...), e.Probe...) {
			views = views || it.Kind == "view"
		}
	}
	if views {
		rw, err := labViewWrapper(lab, h.tool)
		if err != nil {
			return nil, err
		}
		cfg["view"] = filepath.Join(lab, "view")
		out = append(out, rw)
	}
	cb, _ := json.MarshalIndent(cfg, "", "  ")
	pb, _ := json.MarshalIndent(pf, "", "  ")
	if err := os.WriteFile(cfgPath, cb, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, pb, 0o600); err != nil {
		return nil, err
	}
	// The lab source directory and a lab Git repository (one commit, no remote).
	repo := filepath.Join(lab, "repos", "demo")
	if err := os.MkdirAll(filepath.Join(lab, "src"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(repo, 0o700); err != nil {
		return nil, err
	}
	for _, argv := range [][]string{{"git", "init", "-q", "-b", "main"}, {"git", "-c", "user.name=lab", "-c", "user.email=lab@lab", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "lab"}} {
		if _, err := h.cmd(repo, argv...); err != nil {
			return nil, err
		}
	}
	out = append(out, relRewrite{Key: "lab.repos.demo", Lab: repo + " (lab Git repository)"}, relRewrite{Key: "lab.src", Lab: filepath.Join(lab, "src")})
	return out, scanLab(lab, cfgPath, p)
}
