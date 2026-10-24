package main

// Peer hubs, v1 (docs/hub.md, "Peer hubs"). Each hub has a name and a list of peers in its own
// config. Data moves only when the receiver pulls from a peer that its own config names, at the
// address that its own config gives: a hub never trusts a caller's identity from Rex. A plugin
// queues an envelope for a plugin on a peer with hub.peer.send (by default the plugin of its own
// name); the peer pulls it with hub.peer.pull and gives it to that plugin's "peer.receive", with the
// sender's name. A nudge only makes the receiver pull now.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// peerSchema is in openDB. peer_outbox keeps a row after its ack (acked 1, body NULL), so a send
// with the same key keeps its answer: the first seq, or operation_conflict. target is the plugin on
// the peer that gets the envelope; empty is the sender's own name. AUTOINCREMENT never reuses a
// seq, also after the outbox is empty. peer_meta's outbox_id is the identity of this hub.db's outbox:
// a new hub.db makes a new one, and its seqs start again at 1. peer_inbox is the receiver's cursor
// for each peer, with the identity of the peer's outbox that the cursor belongs to.
const peerSchema = `
CREATE TABLE IF NOT EXISTS peer_outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT, peer TEXT NOT NULL, plugin TEXT NOT NULL,
  key TEXT NOT NULL, hash TEXT NOT NULL, body TEXT, created TEXT NOT NULL, acked INTEGER NOT NULL DEFAULT 0,
  target TEXT NOT NULL DEFAULT '', UNIQUE (peer, plugin, key));
CREATE TABLE IF NOT EXISTS peer_meta (name TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT OR IGNORE INTO peer_meta (name, value) VALUES ('outbox_id', lower(hex(randomblob(16))));
CREATE TABLE IF NOT EXISTS peer_inbox (peer TEXT PRIMARY KEY, after INTEGER NOT NULL, outbox_id TEXT NOT NULL DEFAULT '');
`

// outboxID is the form of an outbox identity: 16 random bytes in lowercase hex.
var outboxID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// PeerConfig is one entry of the config field "peers".
type PeerConfig struct {
	Name        string `json:"name"`
	Server      string `json:"server"`       // a rex -S value: a Rex host label or a server address
	PluginsFrom bool   `json:"plugins_from"` // plugin sync reads this peer (at most one peer)
}

var hubName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

const (
	peerBodyMax     = 64 << 10 // hub.peer.send body, JSON-encoded
	peerPullMax     = 100      // envelopes in one pull
	peerPullBytes   = 1 << 20  // one pull reply, encoded (at least one envelope)
	peerReplyWrap   = 1 << 10  // the client.reply frame around a pull result: call_id, keys
	peerCallTimeout = 10 * time.Second
	peerPollUp      = 2 * time.Second
	peerPollDown    = 30 * time.Second
	peerBlockMax    = 5 * time.Minute  // the retries of one envelope for a plugin that does not run
	peerAgentsEvery = 30 * time.Second // a read of a peer's agents (hub.peer.agents)
)

// checkPeers checks hub_name and peers: peers need a name for this hub; each peer has a valid,
// unique name that is not this hub's name, and a server; at most one peer has plugins_from.
func checkPeers(c Config) error {
	if c.HubName != "" && !hubName.MatchString(c.HubName) {
		return fmt.Errorf("hub_name %q must match %s", c.HubName, hubName)
	}
	if len(c.Peers) > 0 && c.HubName == "" {
		return fmt.Errorf("peers need hub_name")
	}
	seen, from := map[string]bool{}, 0
	for _, p := range c.Peers {
		switch {
		case !hubName.MatchString(p.Name):
			return fmt.Errorf("peer name %q must match %s", p.Name, hubName)
		case p.Name == c.HubName:
			return fmt.Errorf("peer %q has this hub's name", p.Name)
		case seen[p.Name]:
			return fmt.Errorf("peer %q is listed twice", p.Name)
		case p.Server == "":
			return fmt.Errorf("peer %q has no server", p.Name)
		}
		seen[p.Name] = true
		if p.PluginsFrom {
			from++
		}
	}
	if from > 1 {
		return fmt.Errorf("plugins_from is set on %d peers; at most one", from)
	}
	return nil
}

// peerLink is the receiver's state of one peer.
type peerLink struct {
	cfg     PeerConfig
	kick    chan struct{}
	mu      sync.Mutex
	state   string // unknown, up or down
	lastOK  time.Time
	lastErr string
	more    bool // the last pull said that the peer holds more
	// blocked: the envelope blockSeq for blockPlugin was not processed (the plugin does not run, or
	// it replied with an error) since blockSince. Only the peer loop writes it; mu guards it for
	// status.
	blockSeq    int64
	blockPlugin string
	blockSince  time.Time
	// dropped: the envelopes from this peer that no plugin here could take (peerReceive), since the
	// hub started, and the last one.
	dropped  int
	lastDrop *peerDrop
	// agents: the peer's agents at its last successful hub.peer.agents read; nil before the first.
	// Only the agents loop writes it (a new value each time); mu guards it for status.
	agents    *peerAgents
	agentsErr string // the last read error, to log a new one once
	// sync: the plugin sync with a plugins_from peer (peersync.go); nil before its first try.
	sync *peerSync
}

type peerSet struct {
	links map[string]*peerLink
	// call runs a hub action on a peer's hub. nil: peerDo; a test connects two hubs directly.
	call func(ctx context.Context, peer PeerConfig, action string, args any) ([]byte, error)
}

func newPeerSet(c Config) *peerSet {
	s := &peerSet{links: map[string]*peerLink{}}
	for _, p := range c.Peers {
		s.links[p.Name] = &peerLink{cfg: p, kick: make(chan struct{}, 1), state: "unknown"}
	}
	return s
}

func (h *Hub) peerLink(name string) *peerLink {
	if h.peers == nil {
		return nil
	}
	return h.peers.links[name]
}

// peerCall runs action on the hub of peer with the rex CLI (the Go client dials only a unix socket;
// the CLI reaches a remote Rex server by its host label). The call has a 10 s bound.
func (h *Hub) peerCall(ctx context.Context, peer PeerConfig, action string, args any) ([]byte, error) {
	if h.peers != nil && h.peers.call != nil {
		return h.peers.call(ctx, peer, action, args)
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, peerCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.rexBinary, "-S", peer.Server, "--autostart=false", "--timeout", "10s",
		"-C", "lazyrex-hub", "do", action, "--args", string(b))
	cmd.WaitDelay = 100 * time.Millisecond
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s on %s: %v: %s", action, peer.Name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// resolveQualified handles an address with "@": "<x>@<this hub>" resolves "<x>" locally, and
// "<x>@<peer>" is a remote address with no local lookup. Local IDs never contain "@". ok false: to
// has no "@", and the local rules resolve it.
func (h *Hub) resolveQualified(to string) (local string, result map[string]any, ok bool, err error) {
	i := strings.LastIndex(to, "@")
	if i < 0 {
		return "", nil, false, nil
	}
	id, hub := to[:i], to[i+1:]
	switch {
	case id == "":
		return "", nil, true, bad("invalid: %q has no agent before @", to)
	case h.cfg.HubName != "" && hub == h.cfg.HubName:
		return id, nil, true, nil
	case h.peerLink(hub) != nil:
		return "", map[string]any{"id": to, "via": "remote", "hub": hub}, true, nil
	}
	return "", nil, true, refuse("unknown_hub", "%s is not a configured peer", hub)
}

// peerSend is hub.peer.send: a plugin queues an envelope for a peer, for the plugin "plugin" there
// (absent: the sender's own name). The sender is the calling plugin; a caller cannot name another.
// The same (peer, plugin, key) with the same body and target returns the first seq, also after the
// ack; another body or another target is operation_conflict.
func (h *Hub) peerSend(ctx context.Context, p perform, args map[string]any) (any, error) {
	pl := h.pluginByClient(p.Requester.ClientID)
	if pl == nil {
		return nil, refuse("not_plugin", "hub.peer.send is for plugins")
	}
	peer, _ := args["hub"].(string)
	key, _ := args["key"].(string)
	body, ok := args["body"].(string)
	target, _ := args["plugin"].(string)
	switch {
	case h.peerLink(peer) == nil:
		return nil, refuse("unknown_hub", "%q is not a configured peer", peer)
	case key == "" || len(key) > 512:
		return nil, bad("invalid: key must be 1-512 bytes")
	case !ok:
		return nil, bad("invalid: body must be a string")
	case args["plugin"] != nil && !pluginName.MatchString(target):
		return nil, bad("invalid: plugin must be a plugin name (%s)", pluginName)
	}
	if target == pl.Name {
		target = "" // the stored and sent form of the sender's own name
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	// Each envelope must fit alone in one pull reply; else it would stop delivery to the peer.
	if enc, _ := json.Marshal(body); len(enc) > peerBodyMax {
		return nil, refuse("too_large", "body is %d bytes encoded; at most %d", len(enc), peerBodyMax)
	}
	if n, budget := peerEnvelopeBytes(math.MaxInt64, pl.Name, target, key, body, created), h.peerPullBudget(); n > budget {
		return nil, refuse("too_large", "the envelope is %d bytes encoded; one pull reply holds at most %d", n, budget)
	}
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])
	// A repeat finds the row. Else the insert does nothing if a send of the same key at the same
	// time inserted first, and the second read finds that row. (A conflict of the insert uses up
	// an AUTOINCREMENT value, so a repeat reads first.)
	for range 2 {
		var seq int64
		var old, oldTarget string
		err := h.db.QueryRowContext(ctx, `SELECT seq, hash, target FROM peer_outbox WHERE peer = ? AND plugin = ? AND key = ?`, peer, pl.Name, key).Scan(&seq, &old, &oldTarget)
		switch {
		case err == nil && oldTarget != target:
			return nil, refuse("operation_conflict", "key %q was sent to %s for another plugin", key, peer)
		case err == nil && old == hash:
			return map[string]any{"seq": seq, "queued": true}, nil
		case err == nil:
			return nil, refuse("operation_conflict", "key %q was sent to %s with another body", key, peer)
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		res, err := h.db.ExecContext(ctx, `INSERT INTO peer_outbox (peer, plugin, target, key, hash, body, created) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (peer, plugin, key) DO NOTHING`, peer, pl.Name, target, key, hash, body, created)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			seq, _ = res.LastInsertId()
			go h.peerCall(context.Background(), h.peerLink(peer).cfg, "hub.peer.nudge", map[string]string{"hub": h.cfg.HubName})
			return map[string]any{"seq": seq, "queued": true}, nil
		}
	}
	return nil, fmt.Errorf("hub.peer.send: key %q: the row is gone after a conflict", key)
}

// peerEnvelope is one envelope of a hub.peer.pull reply. plugin is the sender; target, only when it
// is another plugin, is the plugin that gets it.
func peerEnvelope(seq int64, plugin, target, key, body, created string) map[string]any {
	e := map[string]any{"seq": seq, "plugin": plugin, "key": key, "body": body, "created": created}
	if target != "" {
		e["target"] = target
	}
	return e
}

// peerEnvelopeBytes is the encoded size of an envelope in a pull reply, with its comma.
func peerEnvelopeBytes(seq int64, plugin, target, key, body, created string) int {
	b, _ := json.Marshal(peerEnvelope(seq, plugin, target, key, body, created))
	return len(b) + 1
}

// peerPullBudget is the most bytes of envelopes in one pull reply: 1 MiB, or less if the hub's
// Rex connection takes smaller reply frames (the reply guard would refuse a larger reply).
func (h *Hub) peerPullBudget() int {
	limit := referenceMaxMessage - replyMargin
	if h.rex != nil {
		limit = replyLimit(h.rex)
	}
	return min(peerPullBytes, limit) - peerReplyWrap - len(`{"envelopes":[],"more":false}`)
}

// peerPullServe is hub.peer.pull, called by a peer for its envelopes: after N acks every envelope
// up to N, then the reply has the next envelopes (at most 100, or peerPullBudget encoded bytes) and
// this hub's outbox identity (outbox_id). The ack applies only when the caller names this outbox: a
// cursor of another outbox (this hub's hub.db was replaced) or none acks nothing and reads from
// zero, so a stale high cursor cannot ack the new outbox's envelopes.
func (h *Hub) peerPullServe(ctx context.Context, args map[string]any) (any, error) {
	peer, _ := args["hub"].(string)
	after, ok := intArg(args["after"])
	if h.peerLink(peer) == nil {
		return nil, refuse("unknown_hub", "%q is not a configured peer", peer)
	}
	if !ok || after < 0 {
		return nil, bad("invalid: after must be a whole number >= 0")
	}
	id, err := h.peerOutboxID(ctx)
	if err != nil {
		return nil, err
	}
	if outbox, _ := args["outbox_id"].(string); outbox != id {
		after = 0
	} else if _, err := h.db.ExecContext(ctx, `UPDATE peer_outbox SET acked = 1, body = NULL WHERE peer = ? AND seq <= ? AND acked = 0`, peer, after); err != nil {
		return nil, err
	}
	rows, err := h.db.QueryContext(ctx, `SELECT seq, plugin, target, key, body, created FROM peer_outbox WHERE peer = ? AND seq > ? AND acked = 0 ORDER BY seq LIMIT ?`,
		peer, after, peerPullMax+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	envs, size, more, budget := []any{}, 0, false, h.peerPullBudget()
	for rows.Next() {
		var seq int64
		var plugin, target, key, body, created string
		if err := rows.Scan(&seq, &plugin, &target, &key, &body, &created); err != nil {
			return nil, err
		}
		n := peerEnvelopeBytes(seq, plugin, target, key, body, created)
		if len(envs) == peerPullMax || (len(envs) > 0 && size+n > budget) {
			more = true
			break
		}
		size += n
		envs = append(envs, peerEnvelope(seq, plugin, target, key, body, created))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"envelopes": envs, "more": more, "outbox_id": id}, nil
}

// peerOutboxID is this hub's outbox identity (peerSchema).
func (h *Hub) peerOutboxID(ctx context.Context) (string, error) {
	var id string
	err := h.db.QueryRowContext(ctx, `SELECT value FROM peer_meta WHERE name = 'outbox_id'`).Scan(&id)
	return id, err
}

// peerNudge is hub.peer.nudge: pull from that peer now. It carries no data.
func (h *Hub) peerNudge(args map[string]any) (any, error) {
	peer, _ := args["hub"].(string)
	l := h.peerLink(peer)
	if l == nil {
		return nil, refuse("unknown_hub", "%q is not a configured peer", peer)
	}
	select {
	case l.kick <- struct{}{}:
	default:
	}
	return map[string]any{}, nil
}

// peerStatus is hub.peer.status.
func (h *Hub) peerStatus(ctx context.Context) (any, error) {
	list := []any{}
	if h.peers != nil {
		for _, p := range h.cfg.Peers {
			l := h.peers.links[p.Name]
			var outbox int
			var oldest sql.NullString
			if err := h.db.QueryRowContext(ctx, `SELECT count(*), min(created) FROM peer_outbox WHERE peer = ? AND acked = 0`, p.Name).Scan(&outbox, &oldest); err != nil {
				return nil, err
			}
			after, outboxID, err := h.peerCursor(ctx, p.Name)
			if err != nil {
				return nil, err
			}
			l.mu.Lock()
			row := map[string]any{"name": p.Name, "server": p.Server, "state": l.state, "last_error": l.lastErr, "outbox": outbox,
				"after": after, "outbox_id": outboxID, "more": l.more, "last_ok": nil, "oldest_age_s": nil, "blocked_plugin": nil, "blocked_since": nil,
				"dropped": map[string]any{"count": l.dropped, "last": l.lastDrop}, "agents": l.agents}
			if !l.lastOK.IsZero() {
				row["last_ok"] = l.lastOK.UTC().Format(time.RFC3339)
			}
			if l.blockSeq > after {
				row["blocked_plugin"], row["blocked_since"] = l.blockPlugin, l.blockSince.UTC().Format(time.RFC3339)
			}
			if p.PluginsFrom {
				var s *peerSync
				if l.sync != nil {
					c := *l.sync
					s = &c
				}
				row["plugin_sync"] = s // only on the plugins_from row; null before the first sync
			}
			l.mu.Unlock()
			if t, err := time.Parse(time.RFC3339Nano, oldest.String); oldest.Valid && err == nil {
				row["oldest_age_s"] = int(time.Since(t).Seconds())
			}
			list = append(list, row)
		}
	}
	return map[string]any{"hub_name": h.cfg.HubName, "peers": list}, nil
}

// peerCursor is this hub's cursor for a peer and the identity of the peer's outbox that it belongs
// to ("" before the first pull).
func (h *Hub) peerCursor(ctx context.Context, peer string) (after int64, outbox string, err error) {
	err = h.db.QueryRowContext(ctx, `SELECT after, outbox_id FROM peer_inbox WHERE peer = ?`, peer).Scan(&after, &outbox)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return after, outbox, err
}

// peersRun runs one pull loop for each peer. One loop per peer: one pull at a time, also for nudges.
func (h *Hub) peersRun(ctx context.Context) {
	if h.peers == nil {
		return
	}
	for _, l := range h.peers.links {
		go h.peerLoop(ctx, l)
		go h.peerAgentsLoop(ctx, l)
		if l.cfg.PluginsFrom {
			go h.peerPluginsLoop(ctx, l)
		}
	}
}

func (h *Hub) peerLoop(ctx context.Context, l *peerLink) {
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.kick:
		case <-time.After(wait):
		}
		more, err := h.peerPull(ctx, l)
		l.mu.Lock()
		switch {
		case err != nil:
			if l.state != "down" || l.lastErr != err.Error() {
				log.Printf("peer %s: %v", l.cfg.Name, err)
			}
			l.state, l.lastErr = "down", err.Error()
			wait = peerRetryWait(err, wait)
		default:
			l.state, l.lastErr, l.lastOK, l.more = "up", "", time.Now(), more
			wait = peerPollUp
			if more {
				wait = 0
			}
		}
		l.mu.Unlock()
	}
}

// peerRetryWait is the wait before the next pull after a failed one: it doubles from 2 s up to 30 s.
// An ambiguous peer hub is the exception: after the peer's hub restarts, Rex keeps the lost client
// of the old hub process for about a minute (docs/rex-protocol.md, "Lost clients and reconnect
// tokens"), and `-C lazyrex-hub` names two clients until it goes. That ends by itself, so the pull
// tries again every 2 s and takes the new hub as soon as Rex drops the old one. The loop logs an
// error only when it changes, so the episode logs once.
func peerRetryWait(err error, wait time.Duration) time.Duration {
	if peerAmbiguous(err) {
		return peerPollUp
	}
	return min(max(2*wait, peerPollUp), peerPollDown)
}

// peerAmbiguous: a peer call failed because the peer's lazyrex-hub label names two clients, the
// new hub and the lost client of the hub before its restart.
func peerAmbiguous(err error) bool {
	return strings.Contains(err.Error(), `client target "lazyrex-hub" is ambiguous`)
}

// peerAgents is hub.peer.status peers[].agents: the peer's agents and the time of the read.
type peerAgents struct {
	At   string `json:"at"`
	List []any  `json:"list"`
}

// peerAgentsServe is hub.peer.agents, called by a peer: this hub's current identities, with the
// fields id, name, kind, state and joined of hub.agents (the same values). No "user", spawn
// address, pane, session or binding field. The peer check is a name check ("Trust limit").
func (h *Hub) peerAgentsServe(args map[string]any) (any, error) {
	peer, _ := args["hub"].(string)
	if h.peerLink(peer) == nil {
		return nil, refuse("unknown_hub", "%q is not a configured peer", peer)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	list := []any{}
	for _, a := range identsOf(h.idents) {
		if a.IdState != "current" {
			continue
		}
		name := a.Name
		if name == "" {
			name = a.ID // as hub.agents
		}
		list = append(list, map[string]any{"id": a.ID, "name": name, "kind": a.Kind, "state": a.IdState, "joined": a.live()})
	}
	return map[string]any{"agents": list}, nil
}

// peerAgentsLoop reads the peer's agents at the start, then every 30 s (2 s after an ambiguous
// label, as the pull). A plugin reads the list from hub.peer.status, so no plugin request waits on
// a peer. A failed read keeps the last list and its time.
func (h *Hub) peerAgentsLoop(ctx context.Context, l *peerLink) {
	for {
		wait := peerAgentsEvery
		if err := h.peerAgentsRead(ctx, l); err != nil && peerAmbiguous(err) {
			wait = peerPollUp
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// peerAgentsRead runs one hub.peer.agents read of the peer and keeps the list.
func (h *Hub) peerAgentsRead(ctx context.Context, l *peerLink) error {
	out, err := h.peerCall(ctx, l.cfg, "hub.peer.agents", map[string]any{"hub": h.cfg.HubName})
	var r struct {
		Agents []any `json:"agents"`
	}
	if err == nil {
		if json.Unmarshal(out, &r) != nil || r.Agents == nil {
			err = fmt.Errorf("hub.peer.agents on %s: the reply has no agents list", l.cfg.Name)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		if l.agentsErr != err.Error() {
			log.Printf("peer %s: agents: %v", l.cfg.Name, err)
		}
		l.agentsErr = err.Error()
		return err
	}
	l.agentsErr = ""
	l.agents = &peerAgents{At: time.Now().UTC().Format(time.RFC3339), List: r.Agents}
	return nil
}

// peerPluginClient returns the client of the plugin name, or "" and wait: true when the plugin is
// installed here (a config entry binds it) and desired running, but does not run now (starting,
// restarting, crashing). A plugin that cannot take an envelope gives "" and the drop reason:
// not_installed (no record, or a kept record without an entry) or stopped (desired stopped).
func (h *Hub) peerPluginClient(name string) (client string, wait bool, drop string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pl := h.plugins[name]
	switch {
	case pl == nil || pl.entry == nil:
		return "", false, "not_installed"
	case pl.Desired == "stopped":
		return "", false, "stopped"
	}
	return pl.ClientID, pl.ClientID == "", ""
}

// peerDrop is an envelope from a peer that no plugin here could take (hub.peer.status "dropped").
type peerDrop struct {
	Plugin     string `json:"plugin"` // the target
	FromPlugin string `json:"from_plugin"`
	Key        string `json:"key"`
	Reason     string `json:"reason"` // not_installed, stopped, no_listener or blocked (skipped after peerBlockMax)
	At         string `json:"at"`
}

// peerDropped logs a drop and counts it on the link of peer fromHub (if fromHub is a configured
// peer: hub.test.peer.deliver may name another hub).
func (h *Hub) peerDropped(fromHub string, d peerDrop) {
	log.Printf("peer %s: %s key %q from %s dropped: %s", fromHub, d.Plugin, d.Key, d.FromPlugin, d.Reason)
	if l := h.peerLink(fromHub); l != nil {
		l.countDrop(d)
	}
}

// countDrop counts a drop in hub.peer.status "dropped".
func (l *peerLink) countDrop(d peerDrop) {
	d.At = time.Now().UTC().Format(time.RFC3339)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropped++
	l.lastDrop = &d
}

// peerReceive gives one envelope from plugin fromPlugin on peer fromHub to the local plugin's
// peer.receive, and returns the plugin's reply. It is the one delivery path: the peer pull and the
// test call hub.test.peer.deliver use it. A plugin that cannot take it gets nothing, and the hub
// counts the drop (peerDropped); the reply is nil. It cannot take it when it is not installed here,
// is desired stopped, or does not offer peer.receive (no_listener: the error not_supported, from Rex
// for an action that the plugin's keymap lacks, or from the plugin). A refusal (the result
// {"refused": "<code>", "message"}) is a reply, logged here; it is final. An error means "not
// processed": an installed, desired-running plugin that does not run now, or an error reply (the
// plugin's transaction rolled back), or a lost reply. The pull retries an error for at most
// peerBlockMax (lead-fed-sdk2).
func (h *Hub) peerReceive(ctx context.Context, fromHub, fromPlugin, plugin, key, body string) (json.RawMessage, error) {
	client, wait, drop := h.peerPluginClient(plugin)
	switch {
	case wait:
		return nil, fmt.Errorf("the plugin %q does not run here", plugin)
	case drop != "":
		h.peerDropped(fromHub, peerDrop{Plugin: plugin, FromPlugin: fromPlugin, Key: key, Reason: drop})
		return nil, nil
	}
	res, err := h.performPlugin(ctx, client, "peer.receive", map[string]any{"from_hub": fromHub, "from_plugin": fromPlugin, "key": key, "body": body}, 10000)
	if re := (*rex.RexError)(nil); errors.As(err, &re) && re.Code == "not_supported" {
		h.peerDropped(fromHub, peerDrop{Plugin: plugin, FromPlugin: fromPlugin, Key: key, Reason: "no_listener"})
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s peer.receive: %v", plugin, err)
	}
	var ans struct {
		Refused string `json:"refused"`
		Message string `json:"message"`
	}
	if json.Unmarshal(res, &ans) == nil && ans.Refused != "" {
		log.Printf("peer %s: %s key %q refused: %s: %s", fromHub, plugin, key, ans.Refused, ans.Message)
	}
	return res, nil
}

// peerBlocked records that envelope seq for plugin was not processed, and returns false when its
// retries began peerBlockMax ago: then the caller skips it.
func (l *peerLink) peerBlocked(seq int64, plugin string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.blockSeq != seq {
		l.blockSeq, l.blockPlugin, l.blockSince = seq, plugin, time.Now()
	}
	return time.Since(l.blockSince) < peerBlockMax
}

// peerPull pulls the envelopes that the peer holds for this hub and gives each, in seq order, to its
// target plugin here (an envelope without "target" is for the plugin of the sender's name). The
// cursor moves past an envelope only after the plugin answered: accepted, or refused with a result
// {"refused": code}. An envelope that no plugin here can take is dropped (peerReceive). An envelope that was not
// processed (peerReceive's error: the plugin does not run, an error reply, a lost reply) stops the
// pull and comes again, for at most peerBlockMax per hub process; then it is logged, skipped and
// counted as a drop (reason blocked).
// Delivery is in seq order, so a retry blocks every later envelope from that peer. A failed peer
// call stops the pull with no bound: nothing was delivered. A reply with another outbox identity
// (the peer's hub.db is new, so its seqs start again) resets the cursor to 0 and clears the block
// state, whose timer is keyed by seq; a reply without a valid identity is an error.
func (h *Hub) peerPull(ctx context.Context, l *peerLink) (more bool, err error) {
	after, outbox, err := h.peerCursor(ctx, l.cfg.Name)
	if err != nil {
		return false, err
	}
	out, err := h.peerCall(ctx, l.cfg, "hub.peer.pull", map[string]any{"hub": h.cfg.HubName, "after": after, "outbox_id": outbox})
	if err != nil {
		return false, err
	}
	var r struct {
		Envelopes []struct {
			Seq    int64  `json:"seq"`
			Plugin string `json:"plugin"`
			Target string `json:"target"`
			Key    string `json:"key"`
			Body   string `json:"body"`
		} `json:"envelopes"`
		More   bool   `json:"more"`
		Outbox string `json:"outbox_id"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return false, fmt.Errorf("hub.peer.pull on %s: the reply is not JSON: %v", l.cfg.Name, err)
	}
	if !outboxID.MatchString(r.Outbox) {
		return false, fmt.Errorf("hub.peer.pull on %s: the reply has no valid outbox identity (%q)", l.cfg.Name, r.Outbox)
	}
	if r.Outbox != outbox {
		// The identity and the cursor are saved in one write. The sender served this reply from zero.
		if _, err := h.db.ExecContext(ctx, `INSERT INTO peer_inbox (peer, after, outbox_id) VALUES (?, 0, ?)
			ON CONFLICT (peer) DO UPDATE SET after = 0, outbox_id = excluded.outbox_id`, l.cfg.Name, r.Outbox); err != nil {
			return false, err
		}
		if outbox != "" {
			log.Printf("peer %s: its outbox is new (outbox_id %s -> %s); cursor reset to 0", l.cfg.Name, outbox, r.Outbox)
		}
		l.mu.Lock()
		l.blockSeq, l.blockPlugin, l.blockSince = 0, "", time.Time{}
		l.mu.Unlock()
		after = 0
	}
	for _, e := range r.Envelopes {
		if e.Seq <= after {
			continue // a reply to an older cursor
		}
		target := e.Target
		if target == "" {
			target = e.Plugin
		}
		_, err := h.peerReceive(ctx, l.cfg.Name, e.Plugin, target, e.Key, e.Body)
		switch {
		case err != nil && l.peerBlocked(e.Seq, target):
			return false, fmt.Errorf("envelope %d: %v", e.Seq, err)
		case err != nil:
			log.Printf("peer %s: envelope %d (%s from %s, key %q) skipped after %s: %v", l.cfg.Name, e.Seq, target, e.Plugin, e.Key, peerBlockMax, err)
			l.countDrop(peerDrop{Plugin: target, FromPlugin: e.Plugin, Key: e.Key, Reason: "blocked"})
		}
		// The cursor never moves back: a late reply to an older pull cannot lower it.
		if _, err := h.db.ExecContext(ctx, `INSERT INTO peer_inbox (peer, after) VALUES (?, ?)
			ON CONFLICT (peer) DO UPDATE SET after = max(after, excluded.after)`, l.cfg.Name, e.Seq); err != nil {
			return false, err
		}
		after = e.Seq
	}
	return r.More, nil
}
