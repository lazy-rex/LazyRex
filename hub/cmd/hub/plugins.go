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
	"sort"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

func (h *Hub) register(p perform, args map[string]any) (any, error) {
	name, _ := args["plugin"].(string)
	instance, _ := args["instance_id"].(string)
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
	pl := h.plugins[name]
	if pl == nil {
		return nil, bad("unknown plugin %q", name)
	}
	if instance == "" || instance != pl.InstanceID {
		return nil, bad("instance %q is not the current instance of %s", instance, name)
	}
	if !descendsFrom(p.Requester.Principal.PID, pl.ChildPID) {
		return nil, bad("caller pid %d is not in plugin %s's pane", p.Requester.Principal.PID, name)
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
	h.ev.emit("plugin_registered", map[string]any{"plugin": name, "instance_id": instance, "client_id": pl.ClientID})
	h.kickViews(name, true) // views rendered while the plugin was away hold a stale tree
	if h.watches != nil {
		h.watches.pluginRegistered(name) // rows may wait for it
	}
	go h.changed("plugins") // the home lists running plugins; go: register holds h.mu
	return map[string]any{"namespace": name, "generation": revString(h.generation)}, nil
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

// loadPluginRow returns the persisted desired state and instance, creating a "running" row if absent.
func (h *Hub) loadPluginRow(ctx context.Context, pl *Plugin) error {
	_, err := h.db.ExecContext(ctx, `INSERT OR IGNORE INTO plugins (name, desired) VALUES (?, 'running')`, pl.Name)
	if err != nil {
		return err
	}
	return h.db.QueryRowContext(ctx, `SELECT desired, instance_id, session_id, block_id, child_pid, child_start FROM plugins WHERE name = ?`, pl.Name).
		Scan(&pl.Desired, &pl.InstanceID, &pl.SessionID, &pl.BlockID, &pl.ChildPID, &pl.ChildStart)
}

func (h *Hub) savePlugin(ctx context.Context, pl *Plugin) error {
	_, err := h.db.ExecContext(ctx, `UPDATE plugins SET desired = ?, instance_id = ?, session_id = ?, block_id = ?, child_pid = ?, child_start = ? WHERE name = ?`,
		pl.Desired, pl.InstanceID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart, pl.Name)
	return err
}

// recoverPlugins recovers each resolved plugin in start order: dependencies first (sources.go
// startOrder).
func (h *Hub) recoverPlugins(ctx context.Context) {
	for _, name := range h.cfg.StartOrder {
		pl := h.plugins[name]
		pl.lifecycle.Lock()
		if err := h.recoverPlugin(ctx, pl); err != nil {
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
	pl.ClientID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart = "", "", "", 0, ""
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
			if err := h.recoverPlugin(ctx, pl); err != nil {
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

func (h *Hub) recoverPlugin(ctx context.Context, pl *Plugin) error {
	h.mu.Lock()
	err := h.loadPluginRow(ctx, pl)
	h.mu.Unlock()
	if err != nil {
		return err
	}
	// 1. Spawn intents from earlier generations.
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
	unresolved := false
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
			unresolved = true
			h.ev.emit("plugin_conflict", map[string]any{"plugin": pl.Name, "block_id": "", "reason": "unresolved spawn " + i.id})
			continue
		}
		h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'created', block_id = ? WHERE instance_id = ?`, block, i.id)
		h.mu.Lock()
		pl.InstanceID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart = i.id, found, block, pid, startTime(pid)
		h.savePlugin(ctx, pl)
		h.mu.Unlock()
	}
	if unresolved || pl.Desired != "running" {
		return nil
	}
	// 2. The recorded instance, if any.
	if pl.BlockID != "" {
		ok, err := h.retire(ctx, pl)
		if err != nil || !ok {
			return err
		}
	}
	return h.spawn(ctx, pl)
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
	pl.ClientID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart = "", "", "", 0, ""
	h.savePlugin(ctx, pl)
	h.mu.Unlock()
	if err := h.rex.Call(ctx, "block.close", map[string]any{"session_id": sess, "block_id": old}, nil); err != nil {
		log.Printf("close old plugin block %s: %v", old, err)
	}
	return true, nil
}

func (h *Hub) spawn(ctx context.Context, pl *Plugin) error {
	sess := h.workSession()
	if sess == "" {
		return fmt.Errorf("spawn %s: the LazyRex session is gone; the next UI pass creates it", pl.Name)
	}
	instance := newInstanceID(pl.Name, h.generation)
	if _, err := h.db.ExecContext(ctx, `INSERT INTO spawn_intents (instance_id, plugin, status, session_id) VALUES (?, ?, 'prepared', ?)`, instance, pl.Name, sess); err != nil {
		return err
	}
	h.mu.Lock()
	pl.InstanceID, pl.SessionID, pl.ClientID = instance, sess, ""
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
	command := []string{h.rexBinary, "-S", h.cfg.Server, "--autostart=false", "do", pl.Path, "-s", sess,
		"hub_client_id=" + h.clientID, "instance_id=" + instance}
	if pl.Events == "all" {
		// Rex refuses --all with -s (lab, Rex 1203). The plugin gets its session as an argument.
		command = []string{h.rexBinary, "-S", h.cfg.Server, "--autostart=false", "do", pl.Path, "--all",
			"plugin_session=" + sess, "hub_client_id=" + h.clientID, "instance_id=" + instance}
	}
	keys := make([]string, 0, len(pl.Args))
	for k := range pl.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		command = append(command, k+"="+pl.Args[k])
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
	if ok, err := h.retire(ctx, pl); err != nil || !ok {
		log.Printf("plugin %s: retire: %v", pl.Name, err)
		return
	}
	if err := h.spawn(ctx, pl); err != nil {
		log.Printf("plugin %s: %v", pl.Name, err)
	}
}

func (h *Hub) markStopped(ctx context.Context, pl *Plugin, reason string) {
	h.mu.Lock()
	block := pl.BlockID
	pl.Desired, pl.ClientID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart = "stopped", "", "", "", 0, ""
	h.savePlugin(ctx, pl)
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

func (h *Hub) adminAction(ctx context.Context, action string, args map[string]any) (any, error) {
	switch action {
	case "hub.admin.start", "hub.admin.stop":
		name, _ := args["plugin"].(string)
		pl := h.plugins[name]
		if pl == nil {
			return nil, bad("unknown plugin %q", name)
		}
		pl.lifecycle.Lock()
		defer pl.lifecycle.Unlock()
		if action == "hub.admin.stop" {
			if pl.BlockID != "" {
				if ok, err := h.retire(ctx, pl); err != nil || !ok {
					return nil, fmt.Errorf("cannot verify the running instance: %v", err)
				}
			}
			h.markStopped(ctx, pl, "admin stop")
			return map[string]any{"desired": "stopped"}, nil
		}
		// Start resolves unresolved spawn intents explicitly: the operator decided.
		h.db.ExecContext(ctx, `UPDATE spawn_intents SET status = 'abandoned' WHERE plugin = ? AND status IN ('prepared', 'sending')`, name)
		h.mu.Lock()
		pl.Desired = "running"
		h.savePlugin(ctx, pl)
		running := pl.ClientID != "" && alive(pl.ChildPID, pl.ChildStart)
		h.mu.Unlock()
		if !running {
			if pl.BlockID != "" {
				if ok, err := h.retire(ctx, pl); err != nil || !ok {
					return nil, fmt.Errorf("cannot verify the existing instance: %v", err)
				}
			}
			if err := h.spawn(ctx, pl); err != nil {
				return nil, err
			}
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
		a.Session, a.BlockID, a.ChildPID, a.ChildStart, a.ClientID, a.State = sess, block, child, startTime(child), "", state
		a.delivered = map[string]int64{}
		if err := h.saveBinding(ctx, id, sess, block, child, a.ChildStart); err != nil {
			return nil, err
		}
		h.kickNotices()
		return map[string]any{"agent": id, "block_id": block}, nil
	}
	return nil, bad("unknown admin action")
}

// noticed reports whether a plugin has a notice in its manifest.
func (h *Hub) noticed(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	pl := h.plugins[name]
	return pl != nil && pl.Notice != ""
}
