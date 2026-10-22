package main

// Plugin lifecycle (README section 6). For the messaging plugin all durable state is in the hub,
// so the hub replaces a plugin process instead of adopting it.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

func (h *Hub) register(ctx context.Context, p perform, args map[string]any) (any, error) {
	name, _ := args["plugin"].(string)
	instance, _ := args["instance_id"].(string)
	if name == restartFormOwner {
		return nil, bad("%s is reserved for the hub's own form", name) // also no manifest name (pluginName)
	}
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
	h.parts.drop(pl.ClientID) // the parts of the previous instance end
	h.parts.drop(p.Requester.ClientID)
	pl.ClientID, pl.Exports = p.Requester.ClientID, exports
	if pl.promptsGranted() {
		h.startPromptSender(pl)
	}
	go h.admitCommands(ctx, pl, cmds)() // register holds h.mu; the save runs without it
	h.kickAgentKeymap()
	h.kickWaiting() // a dependent can wait for this plugin
	h.kickOps()     // an unresolved start attempt can wait for this registration (observeLater)
	h.ev.emit("plugin_registered", map[string]any{"plugin": name, "instance_id": instance, "client_id": pl.ClientID})
	h.clearDeclarations(name)        // a new instance declares again
	h.kickViews(name, causeRegister) // views rendered while the plugin was away hold a stale tree
	if h.watches != nil {
		h.watches.pluginRegistered(name) // rows may wait for it
	}
	return map[string]any{"namespace": name, "generation": revString(h.generation), "limits": partsLimits}, nil
}

// commandState is a plugin's admitted palette commands (hub.register). h.mu guards it.
type commandState struct {
	list  []paletteCommand // the admitted set; never nil once admitted (an empty set is registered)
	of    string           // the instance whose registration admitted list; "" before any
	retry bool             // a retry of the save is pending (one per plugin)
	delay time.Duration    // the delay of the next retry; 0 after a successful save
}

// commandsRetryBase is the first retry delay of a failed command save (tests shorten it).
var commandsRetryBase = time.Second

// admitCommands records cmds as the admitted set of pl's current instance and returns its save.
// h.mu must be held: admission order is h.mu order. The save captures only the name, so it never
// writes for a record that another instance took over. ctx is the hub's run context: a pending
// retry ends at the hub stop.
func (h *Hub) admitCommands(ctx context.Context, pl *Plugin, cmds []paletteCommand) func() {
	if cmds == nil {
		cmds = []paletteCommand{}
	}
	pl.commands.list, pl.commands.of = cmds, pl.InstanceID
	name := pl.Name
	return func() { h.persistCommands(ctx, name) }
}

// persistCommands saves the latest admitted set of the plugin's current instance. Saves run one at
// a time (h.cmdMu), and each reads the set when it runs, so the last save to finish writes the
// latest set admitted before it read. A stopped instance, or one that has not registered, saves
// nothing. A failed save keeps one retry pending; hub.changed (plugins) follows only a save that
// succeeded, so the home plugin reads the stored commands (its Launch Profile… entry).
func (h *Hub) persistCommands(ctx context.Context, name string) {
	h.cmdMu.Lock()
	h.mu.Lock()
	pl := h.plugins[name]
	if ctx.Err() != nil || pl == nil || pl.ClientID == "" || pl.commands.of == "" || pl.commands.of != pl.InstanceID {
		h.mu.Unlock()
		h.cmdMu.Unlock()
		return
	}
	cmds := pl.commands.list
	h.mu.Unlock()
	err := h.saveCommands(ctx, name, cmds)
	h.cmdMu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			return // the hub stops
		}
		log.Printf("palette: save %s: %v", name, err)
		h.ev.emit("commands_save_failed", map[string]any{"plugin": name, "error": err.Error()})
		h.retryCommands(ctx, name)
		return
	}
	h.mu.Lock()
	if pl := h.plugins[name]; pl != nil {
		pl.commands.delay = 0
	}
	h.mu.Unlock()
	commandsSaved(h)
}

// commandsAfter schedules a retry (tests collect it without a timer).
var commandsAfter = func(d time.Duration, f func()) { time.AfterFunc(d, f) }

// commandsSaved runs after a successful command save (tests replace it to count).
var commandsSaved = func(h *Hub) { h.changed("plugins") }

// retryCommands schedules one save of the plugin's commands after a failed one: 1 s, doubling to
// 30 s. A plugin has at most one pending retry; the retry saves the latest set. The retry ends at
// the hub stop, or when the registration is no longer current (persistCommands saves nothing).
func (h *Hub) retryCommands(ctx context.Context, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pl := h.plugins[name]
	if pl == nil || pl.commands.retry {
		return
	}
	d := pl.commands.delay
	if d == 0 {
		d = commandsRetryBase
	}
	pl.commands.retry, pl.commands.delay = true, min(2*d, 30*time.Second)
	commandsAfter(d, func() {
		h.mu.Lock()
		if pl := h.plugins[name]; pl != nil {
			pl.commands.retry = false
		}
		h.mu.Unlock()
		h.persistCommands(ctx, name)
	})
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
		return nil, refuse("retained", "instance %s of %s is retained: no entry gives this name now", instance, name)
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
	if err := h.ensurePluginRow(ctx, pl.Name); err != nil {
		return err
	}
	return h.readPluginRow(ctx, pl)
}

// defaultDesired is the desired state of a plugin that has no row: the row that ensurePluginRow
// inserts, and the value that readPluginRow gives for an absent row.
const defaultDesired = "running"

// ensurePluginRow inserts the default row of name when it has none. The other columns take their
// schema defaults, which are the zero values that readPluginRow gives for an absent row.
func (h *Hub) ensurePluginRow(ctx context.Context, name string) error {
	_, err := h.db.ExecContext(ctx, `INSERT OR IGNORE INTO plugins (name, desired) VALUES (?, ?)`, name, defaultDesired)
	return err
}

// readPluginRow is loadPluginRow without the insert: an absent row reads as the default row. An
// admin call reads with it before its first effect (adminattempt.go).
func (h *Hub) readPluginRow(ctx context.Context, pl *Plugin) error {
	var run, selected string
	err := h.db.QueryRowContext(ctx, `SELECT desired, instance_id, session_id, block_id, child_pid, child_start, run, selected_run FROM plugins WHERE name = ?`, pl.Name).
		Scan(&pl.Desired, &pl.InstanceID, &pl.SessionID, &pl.BlockID, &pl.ChildPID, &pl.ChildStart, &run, &selected)
	if errors.Is(err, sql.ErrNoRows) {
		pl.Desired, pl.InstanceID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart, run, selected, err = defaultDesired, "", "", "", 0, "", "", "", nil
	}
	pl.run, pl.selected = pluginRun{}, nil
	if err == nil && run != "" {
		err = json.Unmarshal([]byte(run), &pl.run)
	}
	if err == nil && selected != "" {
		pl.selected = &selectedRun{}
		err = json.Unmarshal([]byte(selected), pl.selected)
	}
	return err
}

// entryIdentity names one entry: its key (managed:<id>, or the source of a config entry) and its
// source, as a JSON array, so that no two entries give the same text. An op stores it at admission,
// and hub.admin.start and hub.admin.stop check it (expect_entry): a later entry that binds the same
// name, or the same source, is another entry.
func entryIdentity(e *configEntry) string {
	b, _ := json.Marshal([]string{opEntryKey(e), e.Source})
	return string(b)
}

// entryStamp is the hash of the plugins row values that every hub writes (also a rollback hub,
// whose savePlugin names these columns and not entry_identity). entry_identity counts only while
// entry_stamp is the stamp of the row: a write by a hub that does not know entry_identity changes
// the values and so makes it invalid.
func entryStamp(desired, instance, session, block string, pid int, start, run string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{desired, instance, session, block, strconv.Itoa(pid), start, run}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (h *Hub) savePlugin(ctx context.Context, pl *Plugin) error {
	run := ""
	// entry_identity is the entry that this save happens under, with a fresh entry_stamp. A kept
	// record (no entry) keeps the identity of its last entry but clears the stamp: a rollback hub can
	// have bound that entry's name to another entry, and this save cannot see it.
	identity := ""
	if pl.entry != nil {
		identity = entryIdentity(pl.entry)
	}
	if pl.run.Name != "" {
		b, _ := json.Marshal(pl.run)
		run = string(b)
	}
	// A kept record never starts, so it has no selected run (the entry's removal clears it).
	if pl.entry == nil {
		pl.selected = nil
	}
	selected := ""
	if pl.selected != nil {
		b, _ := json.Marshal(pl.selected)
		selected = string(b)
	}
	stamp := ""
	if identity != "" {
		stamp = entryStamp(pl.Desired, pl.InstanceID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart, run)
	}
	// source changes only with a run: a spawn from a candidate that passed its checks. A later hub
	// start keeps a record of that source and name while the row has an instance or an intent
	// (loadKept).
	_, err := h.db.ExecContext(ctx, `UPDATE plugins SET desired = ?, instance_id = ?, session_id = ?, block_id = ?, child_pid = ?, child_start = ?, run = ?,
		source = CASE WHEN ? != '' THEN ? ELSE source END,
		entry_identity = CASE WHEN ? != '' THEN ? ELSE entry_identity END, entry_stamp = ?, selected_run = ? WHERE name = ?`,
		pl.Desired, pl.InstanceID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart, run, pl.run.Source, pl.run.Source, identity, identity, stamp, selected, pl.Name)
	return err
}

// loadKept clears every selected run, then makes a kept record for each plugins row that has a recorded block or an open spawn
// intent: the instance or the intent has an owner even when no entry gives its name now
// (reviewer, hub-review-2d1-ownership-boundary). An entry adopts its kept record when it gives
// the same name from the same source (resolveLocked). It runs at hub start, before the first
// resolution. A row from before the source was recorded has source "": only an entry that gives
// the same name adopts it (migration rule).
func (h *Hub) loadKept(ctx context.Context) error {
	// Hub start: no selected run survives a hub stop. A value that this hub stored before, or that a
	// rollback hub kept (it does not know the column), never reaches an automatic start: only an
	// explicit start of this process selects a run (selectedrun.go).
	if _, err := h.db.ExecContext(ctx, `UPDATE plugins SET selected_run = '' WHERE selected_run != ''`); err != nil {
		return err
	}
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
	order := resolveEntries(readLockSnapshot(&h.cfg, h.entries), h.entries, kept)
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
	h.reloadMu.Lock() // a reload waits until recovery has visited every record
	defer h.reloadMu.Unlock()
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
	h.parts.drop(pl.ClientID)
	pl.clearInstance()
	h.savePlugin(ctx, pl)
	h.mu.Unlock()
	h.fenceStoreWrites(pl)
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
	return h.startPluginWith(ctx, pl, false)
}

// startPluginWith is startPlugin. With keepLive (a re-add by reload), a passing check keeps a
// registered live instance unchanged instead of replacing it: the check always runs first, so a
// failing candidate stays refused or retained.
func (h *Hub) startPluginWith(ctx context.Context, pl *Plugin, keepLive bool) error {
	return h.startPluginExpect(ctx, pl, keepLive, nil)
}

// startPluginExpect is startPluginWith. With an expected run x (hub.admin.start expect_run), the
// desired state of the record does not decide: the start runs, and only a candidate that is x,
// with every dependency registered, makes the record desired running and spawns. Otherwise it
// refuses (candidate_changed, dependency_not_ready) and changes no desired state and no instance.
// It never waits: a later wake would start whatever the candidate is then.
func (h *Hub) startPluginExpect(ctx context.Context, pl *Plugin, keepLive bool, x *expectRun) error {
	pl.attempted = true
	h.startFault(pl.Name, "hold-before")
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
	if x == nil && pl.Desired != "running" {
		return nil
	}
	// The prune lock (prunelock.go): from before the resolution until the run is saved.
	lease, err := h.takeStartLease()
	if err != nil {
		return fmt.Errorf("plugin %s: %w", pl.Name, err)
	}
	defer lease.release()
	e, cand, err := h.checkStart(pl)
	if err == errKept {
		return h.recoverKept(ctx, pl) // removed before admission: spawn nothing
	}
	if err != nil {
		return err
	}
	h.startFault(pl.Name, "hold-after")
	if x != nil {
		return h.startExpected(ctx, pl, keepLive, x, e, cand, lease)
	}
	h.mu.Lock()
	pl.cancelRespawnLocked() // the start passed its refusals: it owns the plugin now (respawn.go)
	h.mu.Unlock()
	if keepLive {
		h.mu.Lock()
		live := pl.entry == e && pl.ClientID != "" && alive(pl.ChildPID, pl.ChildStart)
		if live {
			pl.state, pl.refusal, pl.waiting = "", "", nil // the check passed: ready again
		}
		h.mu.Unlock()
		if live {
			h.kickWaiting()
			go h.changed("plugins")
			return nil
		}
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
	return h.startOrWait(ctx, pl, e, cand, lease)
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
// stops after the check does not change this start. e is the admitted entry: after a removal
// (pl.entry != e) the start still completes, but it writes no state, so the kept state stays.
func (h *Hub) startOrWait(ctx context.Context, pl *Plugin, e *configEntry, cand *pluginRun, lease *startLease) error {
	h.mu.Lock()
	var missing []string
	for _, d := range cand.Depends {
		if !h.registeredLocked(d) {
			missing = append(missing, d)
		}
	}
	member := pl.entry == e
	changed := member && !slices.Equal(missing, pl.waiting)
	if member {
		pl.waiting = missing
		pl.state = ""
		if missing != nil {
			pl.state = "waiting"
		}
	}
	if member && missing != nil {
		// The wake starts this run (an automatic start), so it is selected before the lease ends.
		pl.selected = &selectedRun{Entry: entryIdentity(e), Run: *cand}
		if err := h.savePlugin(ctx, pl); err != nil {
			h.mu.Unlock()
			return fmt.Errorf("plugin %s: the selected run is not saved: %w", pl.Name, err)
		}
	}
	h.mu.Unlock()
	if missing == nil {
		return h.spawn(ctx, pl, cand, lease)
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

// keptError is the error of a kept record: no entry (config or managed) gives its name now.
func keptError(pl *Plugin) string {
	return fmt.Sprintf("plugin %s: no entry gives this name now (it ran from source %s); stop it with hub.admin.stop", pl.Name, pl.source)
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
func (h *Hub) checkStart(pl *Plugin) (*configEntry, *pluginRun, error) {
	// Admission: the entry read and the state write share one entriesMu + h.mu section, and a
	// reload swaps the entries under the same locks. So the start is admitted with entry e, or it
	// sees the removal (e nil: a kept record).
	h.entriesMu.Lock()
	h.resolveLocked()
	h.mu.Lock()
	e := pl.entry
	if e == nil {
		h.mu.Unlock()
		h.entriesMu.Unlock()
		return nil, nil, errKept
	}
	cand, err := e.cand, e.err
	if err == nil && cand != nil && cand.SDK != "" && h.cfg.SDK == "" {
		err = errors.New("the manifest declares sdk, and the hub has no SDK directory (-sdk, with boot.lua)")
	}
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
	h.entriesMu.Unlock()
	if err != nil {
		h.ev.emit("plugin_refused", map[string]any{"plugin": pl.Name, "state": state, "error": err.Error()})
		h.kickWaiting() // a dependent that waits for pl is refused now
		go h.changed("plugins")
		return nil, nil, refuse("refused", "%v", err)
	}
	return e, cand, nil
}

// errKept is checkStart's result for a record whose entry is gone at admission: the start takes
// the kept path and spawns nothing.
var errKept = errors.New("the record has no entry")

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
		pl.respawn.spawned = h.clock() // adopted now: the base of the backoff reset
		h.savePlugin(ctx, pl)
		h.mu.Unlock()
		h.checkMapped(ctx, pl, found, block) // the child may have exited after findInstanceBlock read it
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
	h.parts.drop(pl.ClientID)
	pl.clearInstance()
	h.savePlugin(ctx, pl)
	h.mu.Unlock()
	h.fenceStoreWrites(pl)
	h.sql.fence(pl.Name) // the old instance's SQL transaction rolls back before the retirement returns
	if err := h.rex.Call(ctx, "block.close", map[string]any{"session_id": sess, "block_id": old}, nil); err != nil {
		log.Printf("close old plugin block %s: %v", old, err)
	}
	return true, nil
}

// spawnError is a spawn failure and its stage (session, intent, sending, create, child). known:
// no instance runs, because no request went to Rex or Rex refused it. open is the status of an
// intent that stays open after a known failure (the hub could not abandon it), else "". Otherwise
// the outcome is unknown, and the intent stays open for resolveIntents.
type spawnError struct {
	stage string
	known bool
	open  string
	err   error
}

func (e *spawnError) Error() string { return e.err.Error() }
func (e *spawnError) Unwrap() error { return e.err }

// knownFailure is a known spawn failure at stage. It abandons the intent instance, which has the
// status status. If the abandonment fails, the intent stays open, and the error says so.
func (h *Hub) knownFailure(ctx context.Context, stage, instance, status string, err error) *spawnError {
	if aerr := h.abandonIntent(ctx, instance); aerr != nil {
		return &spawnError{stage: stage, known: true, open: status, err: fmt.Errorf("%w; the intent %s stays %s: %v", err, instance, status, aerr)}
	}
	return &spawnError{stage: stage, known: true, err: err}
}

// abandonIntent sets the status of the intent instance to abandoned, and checks that one row
// changed.
func (h *Hub) abandonIntent(ctx context.Context, instance string) error {
	res, err := h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'abandoned' WHERE instance_id = ?`, instance)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%d intent rows changed, not 1", n)
	}
	return nil
}

// pluginChildRead bounds each read of a new plugin block's child (spawn, checkMapped): an
// unanswered Rex read must not hold the start, and pl.lifecycle, for the life of the hub.
var pluginChildRead = 10 * time.Second

// spawn starts a new instance from the candidate cand, which becomes its run. A failure is a
// *spawnError. It releases lease when the run is saved in hub.db: from then on, prune sees the run.
// The caller releases it on the other paths.
func (h *Hub) spawn(ctx context.Context, pl *Plugin, cand *pluginRun, lease *startLease) error {
	sess := h.workSession()
	if sess == "" {
		return &spawnError{stage: "session", known: true, err: fmt.Errorf("spawn %s: the LazyRex session is gone; the next UI pass creates it", pl.Name)}
	}
	instance := newInstanceID(pl.Name, h.generation)
	if err := h.insertIntent(ctx, pl, instance, sess); err != nil {
		return &spawnError{stage: "intent", known: true, err: err}
	}
	h.mu.Lock()
	wasInstance, wasSession, wasClient, wasRun, wasSource, wasSelected := pl.InstanceID, pl.SessionID, pl.ClientID, pl.run, pl.source, pl.selected
	pl.InstanceID, pl.SessionID, pl.ClientID, pl.run, pl.source = instance, sess, "", *cand, cand.Source
	if pl.entry != nil {
		pl.selected = &selectedRun{Entry: entryIdentity(pl.entry), Run: *cand}
	}
	run := pl.run
	if err := h.savePlugin(ctx, pl); err != nil {
		// The run is not durable: prune could not see it, so nothing is sent. The record keeps its
		// state from before this spawn. The intent was never sent, so it is abandoned now, or by
		// the next intent resolution if this update fails too.
		pl.InstanceID, pl.SessionID, pl.ClientID, pl.run, pl.source, pl.selected = wasInstance, wasSession, wasClient, wasRun, wasSource, wasSelected
		h.mu.Unlock()
		return h.knownFailure(ctx, "save", instance, "prepared", fmt.Errorf("spawn %s: %w; nothing was sent", pl.Name, err))
	}
	h.mu.Unlock()
	lease.release()
	h.ev.emit("plugin_spawn_intent", map[string]any{"plugin": pl.Name, "instance_id": instance})
	if f := h.faults.take("block-create", "exit-before", ""); f != nil {
		h.ev.emit("fault", map[string]any{"point": f.point, "mode": f.mode, "instance_id": instance})
		exitNow()
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'sending' WHERE instance_id = ?`, instance); err != nil {
		return h.knownFailure(ctx, "sending", instance, "prepared", err) // nothing was sent
	}
	script, launch := run.Path, []string(nil)
	if run.SDK != "" {
		// The SDK launcher sets package.path for the SDK and the plugin directory, then runs main.
		script, launch = filepath.Join(h.cfg.SDK, "boot.lua"), []string{"main=" + run.Path}
	}
	command := []string{h.rexBinary, "-S", h.cfg.Server, "--autostart=false", "do", script, "-s", sess,
		"hub_client_id=" + h.clientID, "instance_id=" + instance}
	if run.Events == "all" {
		// Rex refuses --all with -s (lab, Rex 1203). The plugin gets its session as an argument.
		command = []string{h.rexBinary, "-S", h.cfg.Server, "--autostart=false", "do", script, "--all",
			"plugin_session=" + sess, "hub_client_id=" + h.clientID, "instance_id=" + instance}
	}
	command = append(command, launch...)
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
	if err := h.createBlock(cctx, pl.Name, req, &out); err != nil {
		var re *rex.RexError
		if errors.As(err, &re) && re.Code != "disconnected" {
			// Rex answered with an error: the block was not created.
			return h.knownFailure(ctx, "create", instance, "sending", fmt.Errorf("spawn %s: %w", pl.Name, err))
		}
		// Otherwise the outcome is unknown and the intent stays 'sending'.
		return &spawnError{stage: "create", err: fmt.Errorf("spawn %s: %w", pl.Name, err)}
	}
	if out.BlockID == "" {
		return &spawnError{stage: "create", err: fmt.Errorf("spawn %s: no block id", pl.Name)}
	}
	rctx, rcancel := context.WithTimeout(ctx, pluginChildRead)
	pid, err := h.childPIDIn(rctx, sess, out.BlockID)
	rcancel()
	if err != nil {
		// The block exists; its child is not verified. The outcome is unknown, not a failed create:
		// the intent stays sending, and resolveIntents adopts a live child by its instance argv.
		log.Printf("plugin %s: cannot read the child of block %s after its create: %v", pl.Name, out.BlockID, err)
		return &spawnError{stage: "child", err: err}
	}
	h.mapFault(pl.Name, out.BlockID)
	h.mu.Lock()
	pl.BlockID, pl.ChildPID, pl.ChildStart = out.BlockID, pid, startTime(pid)
	pl.respawn.spawned = h.clock() // the base of the backoff reset
	h.savePlugin(ctx, pl)
	h.mu.Unlock()
	h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'created', block_id = ? WHERE instance_id = ?`, pl.BlockID, instance)
	h.ev.emit("plugin_spawned", map[string]any{"plugin": pl.Name, "instance_id": instance, "block_id": pl.BlockID})
	h.checkMapped(ctx, pl, sess, out.BlockID)
	return nil
}

// checkMapped reads the child of the block that pl just recorded. A child_exited that came before
// the record found no plugin in blockEvent and was dropped. After that exit, Rex's read shows no
// child and gives last_exit in the event's form (lab, Rex 0.0.1792593189-g3f4a0a), so the exit
// takes the pluginExited path now; its dedupe counts a later child_exited of the block once. A
// failed read, or no child without a last_exit, never counts as an exit (deferred: roadmap.md,
// "Cleanup and test debt"). The caller holds pl.lifecycle, so pluginExited runs on its own.
func (h *Hub) checkMapped(ctx context.Context, pl *Plugin, sess, block string) {
	rctx, cancel := context.WithTimeout(ctx, pluginChildRead)
	c, err := h.childProcIn(rctx, sess, block)
	cancel()
	if err != nil {
		log.Printf("plugin %s: cannot read the child of block %s after recording it: %v", pl.Name, block, err)
		return
	}
	if c.pid != 0 || len(c.lastExit) == 0 || string(c.lastExit) == "null" {
		return
	}
	log.Printf("plugin %s: the child exited before the block was recorded", pl.Name)
	go h.pluginExited(ctx, pl, block, c.lastExit)
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
// unless the block itself was closed (a user stop). An entry's instance respawns after the crash
// backoff (respawn.go). A kept, refused or retained record takes the start path at once, as
// before: it spawns nothing, or it starts after a repair.
func (h *Hub) pluginExited(ctx context.Context, pl *Plugin, blockID string, payload json.RawMessage) {
	pl.lifecycle.Lock()
	defer pl.lifecycle.Unlock()
	h.mu.Lock()
	current := pl.BlockID == blockID
	instance := pl.InstanceID
	if current && instance != "" && pl.respawn.exited == instance {
		current = false // a second child_exited for the same instance counts once
	}
	if current {
		h.parts.drop(pl.ClientID)
		pl.ClientID = ""
		pl.respawn.exited = instance
		h.kickAgentKeymap()
	}
	h.mu.Unlock()
	if !current {
		return
	}
	h.fenceStoreWrites(pl)
	h.ev.emit("plugin_exited", map[string]any{"plugin": pl.Name, "instance_id": instance, "block_id": blockID})
	log.Printf("plugin %s exited: %s", pl.Name, payload)
	// child_exited means the process ended in a block that is still open (a pane close sends
	// block_closed instead), so this is a crash or exit, not a user stop.
	h.mu.Lock()
	backoff := pl.Desired == "running" && pl.entry != nil && pl.state == ""
	if backoff {
		h.crashedLocked(ctx, pl, exitDetail(payload))
	}
	delay := pl.respawn.delay
	h.mu.Unlock()
	if backoff {
		h.ev.emit("plugin_respawn_scheduled", map[string]any{"plugin": pl.Name, "instance_id": instance, "delay_ms": delay.Milliseconds()})
	}
	h.changed("plugins")
	if backoff || pl.Desired != "running" {
		return
	}
	if err := h.startPlugin(ctx, pl); err != nil {
		log.Printf("plugin %s: respawn: %v", pl.Name, err)
	}
}

func (h *Hub) markStopped(ctx context.Context, pl *Plugin, reason string) {
	h.mu.Lock()
	// Before the stop is saved or reported: a respawn callback that waits for pl.lifecycle sees a
	// new generation and does nothing. A later start begins the delays again.
	pl.cancelRespawnLocked()
	pl.respawn.delay = 0
	block := pl.BlockID
	h.parts.drop(pl.ClientID)
	pl.clearInstance()
	pl.Desired, pl.state, pl.refusal, pl.waiting, pl.selected = "stopped", "", "", nil, nil
	h.savePlugin(ctx, pl)
	h.dropKept(pl) // a stopped kept record owns nothing more
	h.mu.Unlock()
	h.fenceStoreWrites(pl)
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
		h.parts.drop(pl.ClientID)
		pl.ClientID = ""
		h.kickAgentKeymap()
		h.mu.Unlock()
		h.fenceStoreWrites(pl)
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
		// The first entry with the source: config entries come first, and a config entry owns its
		// source over a managed duplicate (section 13), which is refused.
		var e *configEntry
		for _, x := range h.entries {
			if x.Source == source {
				e = x
				break
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
	case "hub.admin.replace":
		return h.adminReplace(ctx, args)
	case "hub.admin.start", "hub.admin.stop":
		x, err := parseExpectRun(args)
		if err != nil {
			return nil, err
		}
		if x != nil && action == "hub.admin.stop" {
			return nil, bad("invalid: hub.admin.stop has no expect_run")
		}
		expect, guarded := args["expect_entry"]
		want, _ := expect.(string)
		if guarded && want == "" {
			return nil, refuse("invalid_input", "expect_entry must be a non-empty string")
		}
		attempt, err := parseAttempt(args)
		if err != nil {
			return nil, err
		}
		if guarded || attempt != "" {
			// A reload changes the entry of a bound record (a removal, an in-place change of a managed
			// entry) under reloadMu, and a hub start binds them under it too. With reloadMu and then the
			// lifecycle lock (the order of replace.go), the entry check and the action are one step.
			// An entry can still bind a kept record without reloadMu (resolveLocked), but a kept record
			// has no entry, so it never matches.
			h.reloadMu.Lock()
			defer h.reloadMu.Unlock()
		}
		pl, err := h.adminTarget(action, args)
		if err != nil {
			return nil, err
		}
		pl.lifecycle.Lock()
		defer pl.lifecycle.Unlock()
		defer h.kickWaiting() // the wake rule: a start or a stop of any plugin
		// Read the persisted instance under the lifecycle lock before any decision: an action may come
		// before recovery reaches this record. The read inserts no row: up to sent_at (attemptSent)
		// the call has no effect (adminattempt.go).
		h.mu.Lock()
		err = h.readPluginRow(ctx, pl)
		kept := pl.entry == nil
		now := "none"
		if !kept {
			now = entryIdentity(pl.entry)
		}
		instance, block, session := pl.InstanceID, pl.BlockID, pl.SessionID
		h.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if attempt != "" {
			if err := h.bindAttempt(ctx, attempt, strings.TrimPrefix(action, "hub.admin."), instance, block, session); err != nil {
				return nil, err
			}
			pl.opAttempt = attempt // the spawn of this call writes its intent_id (spawn)
			defer func() { pl.opAttempt = "" }()
		}
		if guarded && now != want {
			return nil, refuse("entry_changed", "plugin %s is bound to entry %s, not %s; nothing was started or stopped", pl.Name, now, want)
		}
		if action == "hub.admin.stop" {
			// The first effect: the insert of an absent row (markStopped updates the row), the intent
			// resolution of a kept record, or the retire.
			if err := h.attemptSent(ctx, attempt); err != nil {
				return nil, err
			}
			if attempt != "" {
				h.opCallFault(pl.Name) // a lab test: the hub stops between sent_at and the first effect
			}
			if err := h.ensurePluginRow(ctx, pl.Name); err != nil {
				return nil, err
			}
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
		// unresolved spawn intent stops it (code unresolved_intent). There is no override. A retained
		// instance is not running: its start runs the check again, and a pass keeps a registered live
		// instance (a reload re-add, section 13).
		h.mu.Lock()
		retained := pl.state == "retained"
		running := pl.ClientID != "" && alive(pl.ChildPID, pl.ChildStart) && !retained
		// With expect_run, the start itself sets the desired state, and only when the candidate is the
		// expected run (startPluginExpect). A running plugin must run the expected run.
		if running && x != nil && !x.matches(&pl.run) {
			h.mu.Unlock()
			return nil, refuse("candidate_changed", "plugin %s runs %s, not %s; nothing was started", pl.Name, runText(&pl.run), x)
		}
		if !running {
			// The first effect: the row insert and the desired state (no expect_run), or the start path.
			if err := h.attemptSent(ctx, attempt); err != nil {
				h.mu.Unlock()
				return nil, err
			}
			if err := h.ensurePluginRow(ctx, pl.Name); err != nil {
				h.mu.Unlock()
				return nil, err
			}
			if x == nil {
				pl.Desired = "running"
				h.savePlugin(ctx, pl)
			}
		}
		h.mu.Unlock()
		if !running {
			if err := h.startPluginExpect(ctx, pl, retained, x); err != nil {
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
		h.mu.Lock()
		a := h.agents[id] // a join or a spawn writes the map under h.mu
		h.mu.Unlock()
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
		rec := h.recordState(ctx, sess, block, a.StatusRecord)
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, other := range h.agents {
			if other != a && other.BlockID == block {
				return nil, bad("block %s is bound to agent %s", block, other.ID)
			}
		}
		a.Session, a.BlockID, a.ChildPID, a.ChildStart, a.ClientID = sess, block, child, startTime(child), ""
		a.setRecord(rec)
		h.syncPrompts()
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
