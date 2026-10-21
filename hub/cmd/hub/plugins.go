package main

// Plugin lifecycle (README section 6). For the messaging plugin all durable state is in the hub,
// so the hub replaces a plugin process instead of adopting it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

func (h *Hub) register(p perform, args map[string]any) (any, error) {
	name, _ := args["plugin"].(string)
	instance, _ := args["instance_id"].(string)
	h.mu.Lock()
	_, err := h.registerTarget(name, instance, p.Requester.Principal.PID)
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// The plugin declares each action, with its description and args schema, to Rex (client.keymap).
	// Rex validates client.perform args against that schema. The hub only checks that exports exist.
	declared, err := h.declaredActions(p.Requester.ClientID)
	if err != nil {
		return nil, err
	}
	cmds, err := parseCommands(name, args["commands"])
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	pl, err := h.registerTarget(name, instance, p.Requester.Principal.PID) // again: the record may have changed meanwhile
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(args["exports"])
	var exports []string
	if err := json.Unmarshal(raw, &exports); err != nil && string(raw) != "{}" {
		return nil, bad("exports must be a list of action names")
	}
	for _, e := range exports {
		if !declared[e] {
			return nil, bad("export %q is not an action the plugin declared to Rex", e)
		}
	}
	pl.ClientID, pl.Exports = p.Requester.ClientID, exports
	go h.saveCommands(context.Background(), name, cmds) // go: register holds h.mu
	h.kickAgentKeymap()
	h.kickWaiting() // a dependent can wait for this plugin
	h.ev.emit("plugin_registered", map[string]any{"plugin": name, "instance_id": instance, "client_id": pl.ClientID})
	h.kickViews(name, true) // views rendered while the plugin was away hold a stale tree
	if h.watches != nil {
		h.watches.pluginRegistered(name) // rows may wait for it
	}
	go h.changed("plugins") // the home lists running plugins; go: register holds h.mu
	return map[string]any{"namespace": name, "generation": revString(h.generation)}, nil
}

// registerTarget checks a hub.register caller against the record of its plugin: the current
// instance, not retained or kept, and a process in the instance's pane. h.mu must be held.
func (h *Hub) registerTarget(name, instance string, pid int) (*Plugin, error) {
	pl := h.plugins[name]
	if pl == nil {
		return nil, bad("unknown plugin %q", name)
	}
	if instance == "" || instance != pl.InstanceID {
		return nil, bad("instance %q is not the current instance of %s", instance, name)
	}
	if pl.entry == nil {
		return nil, refuse("retained", "instance %s of %s is retained: no config entry gives this name now", instance, name)
	}
	if pl.state == "retained" {
		return nil, refuse("retained", "instance %s of %s is retained after a refused start", instance, name)
	}
	if !descendsFrom(pid, pl.ChildPID) {
		return nil, bad("caller pid %d is not in plugin %s's pane", pid, name)
	}
	return pl, nil
}

// declaredActions returns the action names a client declared to Rex.
func (h *Hub) declaredActions(clientID string) (map[string]bool, error) {
	var out struct {
		Actions []struct {
			Name string `json:"name"`
		} `json:"actions"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.rex.Call(ctx, "client.inspect", map[string]any{"client_id": clientID}, &out); err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, a := range out.Actions {
		m[a.Name] = true
	}
	return m, nil
}

func newInstanceID(plugin string, gen int64) string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%s-%d-%s", plugin, gen, hex.EncodeToString(b))
}

// loadPluginRow returns the persisted desired state, instance and run, creating a "running" row if
// absent.
func (h *Hub) loadPluginRow(ctx context.Context, pl *Plugin) error {
	_, err := h.db.ExecContext(ctx, `INSERT OR IGNORE INTO plugins (name, desired) VALUES (?, 'running')`, pl.Name)
	if err != nil {
		return err
	}
	var run string
	err = h.db.QueryRowContext(ctx, `SELECT desired, instance_id, session_id, block_id, child_pid, child_start, run FROM plugins WHERE name = ?`, pl.Name).
		Scan(&pl.Desired, &pl.InstanceID, &pl.SessionID, &pl.BlockID, &pl.ChildPID, &pl.ChildStart, &run)
	pl.run = pluginRun{}
	if err == nil && run != "" {
		err = json.Unmarshal([]byte(run), &pl.run)
	}
	return err
}

func (h *Hub) savePlugin(ctx context.Context, pl *Plugin) error {
	run := ""
	if pl.run.Name != "" {
		b, _ := json.Marshal(pl.run)
		run = string(b)
	}
	// source changes only with a run: a spawn from a candidate that passed its checks. A later hub
	// start keeps a record of that source and name while the row has an instance or an intent
	// (loadKept).
	_, err := h.db.ExecContext(ctx, `UPDATE plugins SET desired = ?, instance_id = ?, session_id = ?, block_id = ?, child_pid = ?, child_start = ?, run = ?,
		source = CASE WHEN ? != '' THEN ? ELSE source END WHERE name = ?`,
		pl.Desired, pl.InstanceID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart, run, pl.run.Source, pl.run.Source, pl.Name)
	return err
}

// loadKept makes a kept record for each plugins row that has a recorded block or an open spawn
// intent: the instance or the intent has an owner even when no entry gives its name now
// (reviewer, hub-review-2d1-ownership-boundary). An entry adopts its kept record when it gives
// the same name from the same source (resolveLocked). It runs at hub start, before the first
// resolution. A row from before the source was recorded has source "": only an entry that gives
// the same name adopts it (migration rule).
func (h *Hub) loadKept(ctx context.Context) error {
	rows, err := h.db.QueryContext(ctx, `SELECT name, source FROM plugins WHERE block_id != ''
		OR name IN (SELECT plugin FROM spawn_intents WHERE status IN ('prepared', 'sending')) ORDER BY name`)
	if err != nil {
		return err
	}
	var kept []*Plugin
	for rows.Next() {
		pl := &Plugin{}
		if err := rows.Scan(&pl.Name, &pl.source); err != nil {
			rows.Close()
			return err
		}
		kept = append(kept, pl)
	}
	rows.Close() // the store has one connection: close the rows before the next query
	if err := rows.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, pl := range kept {
		if err := h.loadPluginRow(ctx, pl); err != nil { // its persisted instance, before any action sees it
			return err
		}
		h.plugins[pl.Name] = pl
	}
	return nil
}

// resolveLocked resolves every entry (resolveEntries) against the kept records. A name that an
// entry bound adopts its kept record (the same record and lifecycle lock), or gets a new record.
// It returns the named entries in recovery order. The caller holds h.entriesMu.
func (h *Hub) resolveLocked() []*configEntry {
	h.mu.Lock()
	var kept []keptName
	for _, pl := range h.plugins {
		if pl.entry == nil {
			kept = append(kept, keptName{Name: pl.Name, Source: pl.source})
		}
	}
	h.mu.Unlock()
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	order := resolveEntries(h.cfg.path, h.entries, kept)
	h.mu.Lock()
	for _, e := range h.entries {
		if e.name == "" {
			continue
		}
		if pl := h.plugins[e.name]; pl == nil {
			// The record loads its row before any action sees it: an action before recovery must not
			// take an empty record as proof that no instance exists.
			pl = &Plugin{Name: e.name, entry: e, source: e.Source}
			if err := h.loadPluginRow(context.Background(), pl); err != nil {
				log.Printf("plugin %s: load its row: %v", e.name, err) // startPlugin and the admin actions load it again
			}
			h.plugins[e.name] = pl
		} else if pl.entry == nil {
			pl.entry = e // the guard passed: the same name from the same source (or a legacy row)
		}
	}
	h.mu.Unlock()
	return order
}

// keptRecords returns the records without an entry, by name.
func (h *Hub) keptRecords() []*Plugin {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*Plugin
	for _, pl := range h.plugins {
		if pl.entry == nil {
			out = append(out, pl)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// dropKept forgets a kept record that has no recorded block and no open intent: nothing of it
// remains to own. Its row stays. h.mu must be held.
func (h *Hub) dropKept(pl *Plugin) {
	if pl.entry != nil || pl.BlockID != "" || h.plugins[pl.Name] != pl {
		return
	}
	var open int
	if err := h.db.QueryRow(`SELECT count(*) FROM spawn_intents WHERE plugin = ? AND status IN ('prepared', 'sending')`, pl.Name).Scan(&open); err != nil || open > 0 {
		return // an open intent keeps ownership
	}
	delete(h.plugins, pl.Name)
	h.ev.emit("plugin_kept_dropped", map[string]any{"plugin": pl.Name})
}

// pluginNames returns the bound plugin names in config order.
func (h *Hub) pluginNames() []string {
	h.entriesMu.Lock()
	defer h.entriesMu.Unlock()
	var names []string
	for _, e := range h.entries {
		if e.name != "" {
			names = append(names, e.name)
		}
	}
	return names
}

// recoverPlugins starts each named entry once: the entries that pass in start order (dependencies
// first), then the refused entries, so that a refused entry with a live instance is retained. Then it
// recovers each kept record (recoverKept), which starts nothing.
func (h *Hub) recoverPlugins(ctx context.Context) {
	if h.test != nil && h.cfg.TestHoldRecovery {
		select {
		case <-h.test.recover:
		case <-time.After(60 * time.Second):
		}
	}
	h.entriesMu.Lock()
	order := h.resolveLocked()
	h.entriesMu.Unlock()
	var records []*Plugin
	h.mu.Lock()
	for _, e := range order {
		records = append(records, h.plugins[e.name])
	}
	h.mu.Unlock()
	for _, pl := range append(records, h.keptRecords()...) {
		pl.lifecycle.Lock()
		if pl.attempted { // hub.admin.start started it before recovery came here
			pl.lifecycle.Unlock()
			continue
		}
		if err := h.startPlugin(ctx, pl); err != nil {
			log.Printf("plugin %s: %v", pl.Name, err)
		}
		pl.lifecycle.Unlock()
	}
}

// findInstanceBlock looks for a block in sess whose child runs the given plugin instance. An
// empty sess (an intent from before sessions were recorded) searches every session.
func (h *Hub) findInstanceBlock(ctx context.Context, sess, instance string) (found, block string, pid int, err error) {
	sessions := []string{sess}
	if sess == "" {
		if sessions, err = h.sessionIDs(ctx); err != nil {
			return "", "", 0, err
		}
	}
	for _, s := range sessions {
		blocks, err := h.sessionBlocksIn(ctx, s)
		if sessionGone(err) {
			continue
		} else if err != nil {
			return "", "", 0, err
		}
		for b := range blocks {
			c, err := h.childPIDIn(ctx, s, b)
			if err == nil && c > 0 && argvHas(c, "instance_id="+instance) {
				return s, b, c, nil
			}
		}
	}
	return "", "", 0, nil
}

// dropInstance forgets an instance whose session is gone. That is not a stop of the plugin:
// desired stays as it is, so the plugin starts again in the work session.
func (h *Hub) dropInstance(ctx context.Context, pl *Plugin, sess, reason string) {
	h.mu.Lock()
	old := pl.BlockID
	pl.clearInstance()
	h.savePlugin(ctx, pl)
	h.mu.Unlock()
	h.ev.emit("plugin_session_gone", map[string]any{"plugin": pl.Name, "block_id": old, "session_id": sess, "reason": reason})
}

// restartAfterSessionLoss starts a plugin again after its session closed while the hub ran. It
// waits until the UI pass has created a new work session: it wakes when a session is recorded, not
// on a timer, and gives up after 30 s.
func (h *Hub) restartAfterSessionLoss(pl *Plugin, lost string) {
	ctx := context.Background()
	deadline := time.After(30 * time.Second)
	for {
		recorded := h.sessionRecorded()
		if ws := h.workSession(); ws != "" && ws != lost {
			pl.lifecycle.Lock()
			defer pl.lifecycle.Unlock()
			if err := h.startPlugin(ctx, pl); err != nil {
				log.Printf("plugin %s: restart after session loss: %v", pl.Name, err)
			}
			return
		}
		h.kickUI("")
		select {
		case <-recorded:
		case <-deadline:
			log.Printf("plugin %s: no work session after the loss of %s; restart the hub", pl.Name, lost)
			return
		}
	}
}

// startPlugin is the one start path (docs/plugin-sources.md, section 7, "Lifecycle"): hub start
// recovery, hub.admin.start, a crash respawn and a restart after a session loss. The caller holds
// pl.lifecycle. The order:
//  1. Resolve the spawn intents. An unresolved one stops the start (code unresolved_intent).
//  2. Resolve every entry and check the graph. A refused entry keeps its recorded instance.
//  3. Only if step 2 passed: retire the recorded instance.
//  4. Spawn, or enter state waiting when a dependency is not registered (startOrWait).
func (h *Hub) startPlugin(ctx context.Context, pl *Plugin) error {
	pl.attempted = true
	h.mu.Lock()
	err := h.loadPluginRow(ctx, pl)
	before := pl.waiting
	h.mu.Unlock()
	defer func() {
		// Observers see each change of the waiting state or of its dependency names.
		h.mu.Lock()
		changed := !slices.Equal(before, pl.waiting)
		h.mu.Unlock()
		if changed {
			go h.changed("plugins")
		}
	}()
	if err != nil {
		return err
	}
	h.mu.Lock()
	kept := pl.entry == nil
	h.mu.Unlock()
	if kept {
		return h.recoverKept(ctx, pl)
	}
	if err := h.resolveIntents(ctx, pl); err != nil {
		return err
	}
	if pl.Desired != "running" {
		return nil
	}
	cand, err := h.checkStart(pl)
	if err != nil {
		return err
	}
	if pl.BlockID != "" {
		ok, err := h.retire(ctx, pl)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("plugin %s: the recorded instance is not retired (see the plugin_conflict or plugin_stopped event)", pl.Name)
		}
	}
	return h.startOrWait(ctx, pl, cand)
}

// registeredLocked reports whether the plugin name has an accepted hub.register for its current
// instance: a record with an entry, not retained, with a client and a live child. An unresolved
// spawn intent gives no client. h.mu must be held.
func (h *Hub) registeredLocked(name string) bool {
	d := h.plugins[name]
	return d != nil && d.entry != nil && d.state != "retained" && d.ClientID != "" && alive(d.ChildPID, d.ChildStart)
}

// startOrWait spawns from cand when each dependency is registered. Otherwise pl enters state
// waiting with the names, and a wake (kickWaiting) runs the whole start path again. The check and
// the state change share one h.mu section, and hub.register sets the client under h.mu before it
// kicks, so a register between them is not lost. Waiting creates no spawn intent. A dependency that
// stops after the check does not change this start.
func (h *Hub) startOrWait(ctx context.Context, pl *Plugin, cand *pluginRun) error {
	h.mu.Lock()
	var missing []string
	for _, d := range cand.Depends {
		if !h.registeredLocked(d) {
			missing = append(missing, d)
		}
	}
	changed := !slices.Equal(missing, pl.waiting)
	pl.waiting = missing
	pl.state = ""
	if missing != nil {
		pl.state = "waiting"
	}
	h.mu.Unlock()
	if missing == nil {
		return h.spawn(ctx, pl, cand)
	}
	if changed {
		h.ev.emit("plugin_waiting", map[string]any{"plugin": pl.Name, "waiting": missing})
	}
	return nil
}

// waitLoop starts the waiting plugins again on each kick (the wake rule, section 7): a register,
// a hub.admin.start or hub.admin.stop of any plugin, a refusal. A kick during a scan stays pending,
// so the loop scans again: a later entry can register after the scan passed it, and a refusal can
// refuse an earlier entry.
func (h *Hub) waitLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.waitKick:
		}
		h.wakeWaiting(ctx)
	}
}

// wakeWaiting runs the start path once for each plugin in state waiting, in config order.
func (h *Hub) wakeWaiting(ctx context.Context) {
	names := h.pluginNames()
	var waiting []*Plugin
	h.mu.Lock()
	for _, n := range names {
		if pl := h.plugins[n]; pl != nil && pl.state == "waiting" {
			waiting = append(waiting, pl)
		}
	}
	h.mu.Unlock()
	for _, pl := range waiting {
		if ctx.Err() != nil {
			return
		}
		pl.lifecycle.Lock()
		h.mu.Lock()
		still := pl.state == "waiting" && pl.Desired == "running" // a stop or a start can come first
		h.mu.Unlock()
		if still {
			if err := h.startPlugin(ctx, pl); err != nil {
				log.Printf("plugin %s: wake: %v", pl.Name, err)
			}
		}
		pl.lifecycle.Unlock()
	}
}

// kickWaiting wakes waitLoop. Repeated kicks coalesce. It never blocks, so a caller can hold h.mu.
func (h *Hub) kickWaiting() {
	if h.waitKick == nil {
		return
	}
	select {
	case h.waitKick <- struct{}{}:
	default:
	}
}

// keptError is the error of a kept record: no config entry gives its name now.
func keptError(pl *Plugin) string {
	return fmt.Sprintf("plugin %s: no config entry gives this name now (it ran from source %s); stop it with hub.admin.stop", pl.Name, pl.source)
}

// recoverKept resolves the spawn intents of a kept record and starts nothing (a kept record never
// starts or respawns). With a recorded block it is retained (the child lives) or refused. With an
// unresolved intent it is refused, and the intent error is returned. With neither, it is dropped.
// The caller holds pl.lifecycle.
func (h *Hub) recoverKept(ctx context.Context, pl *Plugin) error {
	ierr := h.resolveIntents(ctx, pl)
	h.mu.Lock()
	defer h.mu.Unlock()
	if ierr == nil && pl.BlockID == "" {
		h.dropKept(pl)
		return nil
	}
	pl.state, pl.refusal = "refused", keptError(pl)
	if pl.BlockID != "" && pl.ChildPID > 0 && alive(pl.ChildPID, pl.ChildStart) {
		pl.state = "retained"
	}
	h.ev.emit("plugin_refused", map[string]any{"plugin": pl.Name, "state": pl.state, "error": pl.refusal})
	h.kickWaiting()
	if ierr != nil {
		return ierr
	}
	return refuse("refused", "%s", pl.refusal)
}

// checkStart resolves every entry and returns the candidate of pl's entry. A refused entry gets
// state refused, or retained when its recorded instance still lives: a refused start never
// retires (section 7, "Retained instance").
func (h *Hub) checkStart(pl *Plugin) (*pluginRun, error) {
	h.entriesMu.Lock()
	h.resolveLocked()
	cand, err := pl.entry.cand, pl.entry.err
	h.entriesMu.Unlock()
	h.mu.Lock()
	pl.refusal = ""
	if pl.state != "waiting" {
		pl.state = "" // a waiting plugin stays waiting until startOrWait compares the names
	}
	if err != nil {
		pl.state, pl.refusal, pl.waiting = "refused", err.Error(), nil
		if pl.BlockID != "" && pl.ChildPID > 0 && alive(pl.ChildPID, pl.ChildStart) {
			pl.state = "retained"
		}
	}
	state := pl.state
	h.mu.Unlock()
	if err != nil {
		h.ev.emit("plugin_refused", map[string]any{"plugin": pl.Name, "state": state, "error": err.Error()})
		h.kickWaiting() // a dependent that waits for pl is refused now
		go h.changed("plugins")
		return nil, refuse("refused", "%v", err)
	}
	return cand, nil
}

// unresolvedStop is the error of a stop of a kept record that an unresolved spawn intent keeps
// (plan v2): the record stays.
func unresolvedStop(plugin, instance, sess string) error {
	return refuse("unresolved_intent", "plugin %s: the stop is not complete: spawn %s in session %s may still create a block. The record stays. Stop again after that block appears or that session closes.",
		plugin, instance, sess)
}

// unresolvedIntent is the error of a start that an unresolved spawn intent stops.
func unresolvedIntent(plugin, instance, sess string) error {
	return refuse("unresolved_intent", "plugin %s: spawn %s in session %s may still create a block, so the hub does not start a second instance. It resolves when that block appears or that session closes.",
		plugin, instance, sess)
}

// resolveIntents resolves the open spawn intents of pl (from this or an earlier hub process): a
// prepared intent was never sent; a sending intent in a gone session cannot create a block; a
// sending intent whose block is found is adopted as the recorded instance. Any other sending
// intent is unresolved: the request may still create a block, and absence is not permission to
// create again. The first unresolved intent is the error.
func (h *Hub) resolveIntents(ctx context.Context, pl *Plugin) error {
	return h.resolveIntentsFor(ctx, pl, unresolvedIntent)
}

// resolveIntentsFor is resolveIntents; mk makes the error of the first unresolved intent.
func (h *Hub) resolveIntentsFor(ctx context.Context, pl *Plugin, mk func(plugin, instance, sess string) error) error {
	rows, err := h.db.QueryContext(ctx, `SELECT instance_id, status, session_id FROM spawn_intents WHERE plugin = ? AND status IN ('prepared', 'sending')`, pl.Name)
	if err != nil {
		return err
	}
	type intent struct{ id, status, session string }
	var open []intent
	for rows.Next() {
		var i intent
		rows.Scan(&i.id, &i.status, &i.session)
		open = append(open, i)
	}
	rows.Close()
	var unresolved error
	for _, i := range open {
		if i.status == "prepared" {
			// The request was never sent: nothing can exist.
			h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'abandoned' WHERE instance_id = ?`, i.id)
			continue
		}
		if i.session != "" {
			live, err := h.sessionLive(ctx, i.session)
			if err != nil {
				return err
			}
			if !live {
				// The request named a session that is gone: no block can appear from it.
				h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'abandoned' WHERE instance_id = ?`, i.id)
				h.ev.emit("plugin_intent_abandoned", map[string]any{"plugin": pl.Name, "instance_id": i.id, "session_id": i.session})
				continue
			}
		}
		// A live session, the old work session included, is searched for the instance. An intent
		// from before sessions were recorded is searched for in every session.
		found, block, pid, err := h.findInstanceBlock(ctx, i.session, i.id)
		if err != nil {
			return err
		}
		if block == "" {
			// The earlier request may still complete. A timeout or absence is not permission to create again.
			if unresolved == nil {
				unresolved = mk(pl.Name, i.id, i.session)
			}
			h.ev.emit("plugin_conflict", map[string]any{"plugin": pl.Name, "block_id": "", "reason": "unresolved spawn " + i.id})
			continue
		}
		h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'created', block_id = ? WHERE instance_id = ?`, block, i.id)
		h.mu.Lock()
		pl.InstanceID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart = i.id, found, block, pid, startTime(pid)
		h.savePlugin(ctx, pl)
		h.mu.Unlock()
	}
	return unresolved
}

// retire stops the recorded instance after verifying it, and closes its block.
// It returns false if the block holds something the hub cannot verify; then it changes nothing.
func (h *Hub) retire(ctx context.Context, pl *Plugin) (bool, error) {
	sess := pl.SessionID
	if sess == "" {
		// A row from before the session was recorded: find the block in any session.
		found, err := h.findBlockSession(ctx, pl.BlockID)
		if err != nil {
			return false, err
		}
		if found == "" {
			// Migration decision: no listed session has the block, and desired=running means the hub
			// never recorded a stop. Start the plugin again.
			h.dropInstance(ctx, pl, "", "legacy block not found")
			return true, nil
		}
		sess = found
	}
	blocks, err := h.sessionBlocksIn(ctx, sess)
	if sessionGone(err) {
		// The session closed with the instance in it. That is not a stop of this plugin.
		h.dropInstance(ctx, pl, sess, "session gone")
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !blocks[pl.BlockID] {
		// The mapped block is gone from a live session: treat it as a native close.
		h.markStopped(ctx, pl, "block missing during recovery")
		return false, nil
	}
	child, err := h.childPIDIn(ctx, sess, pl.BlockID)
	if err != nil {
		return false, err
	}
	if child != 0 {
		if child != pl.ChildPID || !alive(child, pl.ChildStart) || !argvHas(child, "instance_id="+pl.InstanceID) {
			h.ev.emit("plugin_conflict", map[string]any{"plugin": pl.Name, "block_id": pl.BlockID, "reason": "unrecognized live child"})
			return false, nil
		}
		if err := stopProcess(child, pl.ChildStart); err != nil {
			return false, err
		}
	}
	h.mu.Lock()
	h.selfClosed[pl.BlockID] = true
	old := pl.BlockID
	pl.clearInstance()
	h.savePlugin(ctx, pl)
	h.mu.Unlock()
	if err := h.rex.Call(ctx, "block.close", map[string]any{"session_id": sess, "block_id": old}, nil); err != nil {
		log.Printf("close old plugin block %s: %v", old, err)
	}
	return true, nil
}

// spawn starts a new instance from the candidate cand, which becomes its run.
func (h *Hub) spawn(ctx context.Context, pl *Plugin, cand *pluginRun) error {
	sess := h.workSession()
	if sess == "" {
		return fmt.Errorf("spawn %s: the LazyRex session is gone; the next UI pass creates it", pl.Name)
	}
	instance := newInstanceID(pl.Name, h.generation)
	if _, err := h.db.ExecContext(ctx, `INSERT INTO spawn_intents (instance_id, plugin, status, session_id) VALUES (?, ?, 'prepared', ?)`, instance, pl.Name, sess); err != nil {
		return err
	}
	h.mu.Lock()
	pl.InstanceID, pl.SessionID, pl.ClientID, pl.run, pl.source = instance, sess, "", *cand, cand.Source
	run := pl.run
	h.savePlugin(ctx, pl)
	h.mu.Unlock()
	h.ev.emit("plugin_spawn_intent", map[string]any{"plugin": pl.Name, "instance_id": instance})
	if f := h.faults.take("block-create", "exit-before", ""); f != nil {
		h.ev.emit("fault", map[string]any{"point": f.point, "mode": f.mode, "instance_id": instance})
		exitNow()
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'sending' WHERE instance_id = ?`, instance); err != nil {
		return err
	}
	command := []string{h.rexBinary, "-S", h.cfg.Server, "--autostart=false", "do", run.Path, "-s", sess,
		"hub_client_id=" + h.clientID, "instance_id=" + instance}
	if run.Events == "all" {
		// Rex refuses --all with -s (lab, Rex 1203). The plugin gets its session as an argument.
		command = []string{h.rexBinary, "-S", h.cfg.Server, "--autostart=false", "do", run.Path, "--all",
			"plugin_session=" + sess, "hub_client_id=" + h.clientID, "instance_id=" + instance}
	}
	keys := make([]string, 0, len(run.Args))
	for k := range run.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		command = append(command, k+"="+run.Args[k])
	}
	// A detached block: callable and gets lifecycle events, but takes no tab or layout space.
	// `rex move` or session.move_block can show it.
	req := map[string]any{
		"session_id": sess,
		"flavor":     "com.superlogical.terminal.shell",
		"label":      "lazyrex-plugin:" + pl.Name,
		"options": map[string]any{
			"command": command,
			"login":   false,
			"exit":    map[string]any{"on_completion": false}, // keep the log after exit
		},
	}
	if h.exitAfterSendFault(ctx, req, instance) {
		exitNow()
	}
	var out struct {
		BlockID string `json:"block_id"`
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.rex.Call(cctx, "session.new_block", req, &out); err != nil {
		var re *rex.RexError
		if errors.As(err, &re) && re.Code != "disconnected" {
			// Rex answered with an error: the block was not created.
			h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'abandoned' WHERE instance_id = ?`, instance)
		}
		// Otherwise the outcome is unknown and the intent stays 'sending'.
		return fmt.Errorf("spawn %s: %w", pl.Name, err)
	}
	if out.BlockID == "" {
		return fmt.Errorf("spawn %s: no block id", pl.Name)
	}
	pid, err := h.childPIDIn(ctx, sess, out.BlockID)
	if err != nil {
		return err
	}
	h.mu.Lock()
	pl.BlockID, pl.ChildPID, pl.ChildStart = out.BlockID, pid, startTime(pid)
	h.savePlugin(ctx, pl)
	h.mu.Unlock()
	h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'created', block_id = ? WHERE instance_id = ?`, pl.BlockID, instance)
	h.ev.emit("plugin_spawned", map[string]any{"plugin": pl.Name, "instance_id": instance, "block_id": pl.BlockID})
	return nil
}

// exitAfterSendFault writes the block-create request frame, then reports the fault, if configured.
func (h *Hub) exitAfterSendFault(ctx context.Context, req map[string]any, instance string) bool {
	f := h.faults.take("block-create", "exit-after", "")
	if f == nil {
		return false
	}
	if err := h.rex.SendOnly(ctx, "session.new_block", req); err != nil {
		log.Printf("fault send: %v", err)
	}
	h.ev.emit("fault", map[string]any{"point": f.point, "mode": f.mode, "instance_id": instance})
	return true
}

func exitNow() {
	log.Printf("lab fault: exit %d", faultExitCode)
	os.Exit(faultExitCode)
}

// pluginExited handles child_exited for a plugin block: replace it while desired state is running,
// unless the block itself was closed (a user stop).
func (h *Hub) pluginExited(ctx context.Context, pl *Plugin, blockID string, payload json.RawMessage) {
	pl.lifecycle.Lock()
	defer pl.lifecycle.Unlock()
	h.mu.Lock()
	current := pl.BlockID == blockID
	instance := pl.InstanceID
	if current {
		pl.ClientID = ""
		h.kickAgentKeymap()
	}
	h.mu.Unlock()
	if !current {
		return
	}
	h.ev.emit("plugin_exited", map[string]any{"plugin": pl.Name, "instance_id": instance, "block_id": blockID})
	log.Printf("plugin %s exited: %s", pl.Name, payload)
	h.changed("plugins")
	// child_exited means the process ended in a block that is still open (a pane close sends
	// block_closed instead), so this is a crash or exit, not a user stop.
	if pl.Desired != "running" {
		return
	}
	if err := h.startPlugin(ctx, pl); err != nil {
		log.Printf("plugin %s: respawn: %v", pl.Name, err)
	}
}

func (h *Hub) markStopped(ctx context.Context, pl *Plugin, reason string) {
	h.mu.Lock()
	block := pl.BlockID
	pl.clearInstance()
	pl.Desired, pl.state, pl.refusal, pl.waiting = "stopped", "", "", nil
	h.savePlugin(ctx, pl)
	h.dropKept(pl) // a stopped kept record owns nothing more
	h.mu.Unlock()
	h.ev.emit("plugin_stopped", map[string]any{"plugin": pl.Name, "block_id": block, "reason": reason})
	h.changed("plugins")
}

// blockClosed treats the close of a mapped plugin block that the hub did not close itself as a user stop.
func (h *Hub) blockClosed(ctx context.Context, blockID string) {
	h.mu.Lock()
	self := h.selfClosed[blockID]
	delete(h.selfClosed, blockID)
	var pl *Plugin
	for _, p := range h.plugins {
		if p.BlockID == blockID {
			pl = p
		}
	}
	h.mu.Unlock()
	if h.uiBlockClosed(ctx, blockID, self) {
		return
	}
	if h.workerBlockClosed(ctx, blockID) {
		return
	}
	if self || pl == nil {
		return
	}
	pl.lifecycle.Lock()
	defer pl.lifecycle.Unlock()
	if pl.BlockID != blockID {
		return
	}
	// Rex closes every block of a destroyed session before session_destroyed, and the session is
	// already gone then (observed on Rex 1203, lab). A session loss is not a stop of this plugin.
	sess := pl.SessionID
	if sess == "" {
		h.markStopped(ctx, pl, "block closed")
		return
	}
	live, err := h.sessionLive(ctx, sess)
	if err != nil {
		// An error does not prove that the session is live: do not save a user stop. The saved
		// identity stays, so a hub restart classifies the close again (retire). Retry here too.
		h.mu.Lock()
		pl.ClientID = ""
		h.kickAgentKeymap()
		h.mu.Unlock()
		go h.classifyClose(pl, blockID, sess)
		return
	}
	if !live {
		h.dropInstance(ctx, pl, sess, "session closed")
		go h.restartAfterSessionLoss(pl, sess)
		return
	}
	h.markStopped(ctx, pl, "block closed")
}

// classifyClose retries the session check for a plugin close that blockClosed could not classify.
// Live session: a user stop. Gone: a session loss. Still unknown after about a minute: report it,
// keep the saved identity and start nothing; a hub restart classifies it again.
func (h *Hub) classifyClose(pl *Plugin, block, sess string) {
	ctx := context.Background()
	for wait := 500 * time.Millisecond; wait <= 16*time.Second; wait *= 2 {
		time.Sleep(wait)
		live, err := h.sessionLive(ctx, sess)
		if err != nil {
			continue
		}
		pl.lifecycle.Lock()
		current := pl.BlockID == block // else something replaced the instance meanwhile
		if current && live {
			h.markStopped(ctx, pl, "block closed")
		} else if current {
			h.dropInstance(ctx, pl, sess, "session closed")
		}
		pl.lifecycle.Unlock()
		if current && !live {
			h.restartAfterSessionLoss(pl, sess)
		}
		return
	}
	h.ev.emit("plugin_close_unclassified", map[string]any{"plugin": pl.Name, "session_id": sess})
	log.Printf("plugin %s: cannot tell whether its block closed alone or with session %s; not restarted. Use hub.admin.start or hub.admin.stop", pl.Name, sess)
}

// adminTarget finds the record that hub.admin.start or hub.admin.stop names with "plugin". A start
// can name an entry by its exact "source" instead (section 7, "Repair entry point"): an entry that
// has no name yet resolves again, and a name that it binds gets its record.
func (h *Hub) adminTarget(action string, args map[string]any) (*Plugin, error) {
	name, _ := args["plugin"].(string)
	source, _ := args["source"].(string)
	if source != "" {
		if action != "hub.admin.start" || name != "" {
			return nil, bad("give plugin, or (hub.admin.start only) source")
		}
		h.entriesMu.Lock()
		var e *configEntry
		for _, x := range h.entries {
			if x.Source == source {
				e = x
			}
		}
		if e != nil && e.name == "" {
			h.resolveLocked()
		}
		var err error
		if e != nil {
			name, err = e.name, e.err
		}
		h.entriesMu.Unlock()
		if e == nil {
			return nil, bad("no plugin entry has the source %q", source)
		}
		if name == "" {
			return nil, refuse("refused", "%v", err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	pl := h.plugins[name]
	if pl == nil {
		return nil, bad("unknown plugin %q", name)
	}
	return pl, nil
}

func (h *Hub) adminAction(ctx context.Context, action string, args map[string]any) (any, error) {
	switch action {
	case "hub.admin.start", "hub.admin.stop":
		pl, err := h.adminTarget(action, args)
		if err != nil {
			return nil, err
		}
		pl.lifecycle.Lock()
		defer pl.lifecycle.Unlock()
		defer h.kickWaiting() // the wake rule: a start or a stop of any plugin
		// Read the persisted instance under the lifecycle lock before any decision: an action may come
		// before recovery reaches this record.
		h.mu.Lock()
		err = h.loadPluginRow(ctx, pl)
		kept := pl.entry == nil
		h.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if action == "hub.admin.stop" {
			// A kept record resolves its intents first: a found block is adopted, then retired. An
			// unresolved intent keeps the record.
			if kept {
				if err := h.resolveIntentsFor(ctx, pl, unresolvedStop); err != nil {
					return nil, err
				}
			}
			if pl.BlockID != "" {
				if ok, err := h.retire(ctx, pl); err != nil || !ok {
					return nil, fmt.Errorf("cannot verify the running instance: %v", err)
				}
			}
			h.markStopped(ctx, pl, "admin stop")
			return map[string]any{"desired": "stopped"}, nil
		}
		if kept {
			return nil, bad("plugin %s has no config entry: stop it with hub.admin.stop, or start its entry by source", pl.Name)
		}
		// A start of a running plugin changes nothing. Any other start takes the one start path: an
		// unresolved spawn intent stops it (code unresolved_intent). There is no override.
		h.mu.Lock()
		running := pl.ClientID != "" && alive(pl.ChildPID, pl.ChildStart)
		if !running {
			pl.Desired = "running"
			h.savePlugin(ctx, pl)
		}
		h.mu.Unlock()
		if !running {
			if err := h.startPlugin(ctx, pl); err != nil {
				return nil, err
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if pl.state == "waiting" {
			return map[string]any{"desired": "running", "state": "waiting", "waiting": pl.waiting}, nil
		}
		return map[string]any{"desired": "running"}, nil
	case "hub.admin.rebind":
		id, _ := args["agent"].(string)
		block, _ := args["block_id"].(string)
		a := h.agents[id]
		if a == nil || block == "" {
			return nil, bad("rebind needs a roster agent and a block_id")
		}
		sess, err := h.findBlockSession(ctx, block)
		if err != nil {
			return nil, err
		}
		if sess == "" {
			return nil, bad("block %s is not in any session", block)
		}
		child, err := h.childPIDIn(ctx, sess, block)
		if err != nil || child == 0 {
			return nil, bad("block %s has no live child", block)
		}
		// Read the pane's current status as join does: a status reported before the rebind sends
		// no new event, and without it the agent never counts as ready for a notice.
		state := h.recordState(ctx, sess, block, a.StatusRecord)
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, other := range h.agents {
			if other != a && other.BlockID == block {
				return nil, bad("block %s is bound to agent %s", block, other.ID)
			}
		}
		a.Session, a.BlockID, a.ChildPID, a.ChildStart, a.ClientID = sess, block, child, startTime(child), ""
		a.setState(state)
		a.delivered = map[string]int64{}
		if err := h.saveBinding(ctx, id, sess, block, child, a.ChildStart); err != nil {
			return nil, err
		}
		h.kickNotices()
		return map[string]any{"agent": id, "block_id": block}, nil
	}
	return nil, bad("unknown admin action")
}

// noticed reports whether a plugin has a notice in its manifest, as its run records it.
func (h *Hub) noticed(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	pl := h.plugins[name]
	return pl != nil && pl.run.Notice != ""
}
