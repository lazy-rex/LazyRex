package main

// Peer hubs, v1 (docs/hub.md, "Peer hubs"). Each hub has a name and a list of peers in its own
// config. Data moves only when the receiver pulls from a peer that its own config names, at the
// address that its own config gives: a hub never trusts a caller's identity from Rex. A plugin
// queues an envelope for a peer with hub.peer.send; the peer pulls it with hub.peer.pull and gives
// it to its own plugin of the same name ("peer.receive"). A nudge only makes the receiver pull now.

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
)

// peerSchema is in openDB. peer_outbox keeps a row after its ack (acked 1, body NULL), so a send
// with the same key keeps its answer: the first seq, or key_conflict. AUTOINCREMENT never reuses a
// seq, also after the outbox is empty. peer_inbox is the receiver's cursor for each peer.
const peerSchema = `
CREATE TABLE IF NOT EXISTS peer_outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT, peer TEXT NOT NULL, plugin TEXT NOT NULL,
  key TEXT NOT NULL, hash TEXT NOT NULL, body TEXT, created TEXT NOT NULL, acked INTEGER NOT NULL DEFAULT 0,
  UNIQUE (peer, plugin, key));
CREATE TABLE IF NOT EXISTS peer_inbox (peer TEXT PRIMARY KEY, after INTEGER NOT NULL);
`

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
	peerBlockMax    = 5 * time.Minute // the retries of one envelope for a plugin that does not run
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
	// blocked: the envelope blockSeq waits for blockPlugin, which is installed and desired running
	// but does not run, since blockSince. Only the peer loop writes it; mu guards it for status.
	blockSeq    int64
	blockPlugin string
	blockSince  time.Time
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

// peerSend is hub.peer.send: a plugin queues an envelope for a peer. The same (peer, plugin, key)
// with the same body returns the first seq, also after the ack; another body is key_conflict.
func (h *Hub) peerSend(ctx context.Context, p perform, args map[string]any) (any, error) {
	pl := h.pluginByClient(p.Requester.ClientID)
	if pl == nil {
		return nil, refuse("not_plugin", "hub.peer.send is for plugins")
	}
	peer, _ := args["hub"].(string)
	key, _ := args["key"].(string)
	body, ok := args["body"].(string)
	switch {
	case h.peerLink(peer) == nil:
		return nil, refuse("unknown_hub", "%q is not a configured peer", peer)
	case key == "" || len(key) > 512:
		return nil, bad("invalid: key must be 1-512 bytes")
	case !ok:
		return nil, bad("invalid: body must be a string")
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	// Each envelope must fit alone in one pull reply; else it would stop delivery to the peer.
	if enc, _ := json.Marshal(body); len(enc) > peerBodyMax {
		return nil, refuse("too_large", "body is %d bytes encoded; at most %d", len(enc), peerBodyMax)
	}
	if n, budget := peerEnvelopeBytes(math.MaxInt64, pl.Name, key, body, created), h.peerPullBudget(); n > budget {
		return nil, refuse("too_large", "the envelope is %d bytes encoded; one pull reply holds at most %d", n, budget)
	}
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])
	// A repeat finds the row. Else the insert does nothing if a send of the same key at the same
	// time inserted first, and the second read finds that row. (A conflict of the insert uses up
	// an AUTOINCREMENT value, so a repeat reads first.)
	for range 2 {
		var seq int64
		var old string
		err := h.db.QueryRowContext(ctx, `SELECT seq, hash FROM peer_outbox WHERE peer = ? AND plugin = ? AND key = ?`, peer, pl.Name, key).Scan(&seq, &old)
		switch {
		case err == nil && old == hash:
			return map[string]any{"seq": seq, "queued": true}, nil
		case err == nil:
			return nil, refuse("key_conflict", "key %q was sent to %s with another body", key, peer)
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		res, err := h.db.ExecContext(ctx, `INSERT INTO peer_outbox (peer, plugin, key, hash, body, created) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (peer, plugin, key) DO NOTHING`, peer, pl.Name, key, hash, body, created)
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

// peerEnvelope is one envelope of a hub.peer.pull reply.
func peerEnvelope(seq int64, plugin, key, body, created string) map[string]any {
	return map[string]any{"seq": seq, "plugin": plugin, "key": key, "body": body, "created": created}
}

// peerEnvelopeBytes is the encoded size of an envelope in a pull reply, with its comma.
func peerEnvelopeBytes(seq int64, plugin, key, body, created string) int {
	b, _ := json.Marshal(peerEnvelope(seq, plugin, key, body, created))
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
// up to N, then the reply has the next envelopes (at most 100, or peerPullBudget encoded bytes).
func (h *Hub) peerPullServe(ctx context.Context, args map[string]any) (any, error) {
	peer, _ := args["hub"].(string)
	after, ok := intArg(args["after"])
	if h.peerLink(peer) == nil {
		return nil, refuse("unknown_hub", "%q is not a configured peer", peer)
	}
	if !ok || after < 0 {
		return nil, bad("invalid: after must be a whole number >= 0")
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE peer_outbox SET acked = 1, body = NULL WHERE peer = ? AND seq <= ? AND acked = 0`, peer, after); err != nil {
		return nil, err
	}
	rows, err := h.db.QueryContext(ctx, `SELECT seq, plugin, key, body, created FROM peer_outbox WHERE peer = ? AND seq > ? AND acked = 0 ORDER BY seq LIMIT ?`,
		peer, after, peerPullMax+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	envs, size, more, budget := []any{}, 0, false, h.peerPullBudget()
	for rows.Next() {
		var seq int64
		var plugin, key, body, created string
		if err := rows.Scan(&seq, &plugin, &key, &body, &created); err != nil {
			return nil, err
		}
		n := peerEnvelopeBytes(seq, plugin, key, body, created)
		if len(envs) == peerPullMax || (len(envs) > 0 && size+n > budget) {
			more = true
			break
		}
		size += n
		envs = append(envs, peerEnvelope(seq, plugin, key, body, created))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"envelopes": envs, "more": more}, nil
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
			after, err := h.peerAfter(ctx, p.Name)
			if err != nil {
				return nil, err
			}
			l.mu.Lock()
			row := map[string]any{"name": p.Name, "server": p.Server, "state": l.state, "last_error": l.lastErr, "outbox": outbox,
				"after": after, "more": l.more, "last_ok": nil, "oldest_age_s": nil, "blocked_plugin": nil, "blocked_since": nil}
			if !l.lastOK.IsZero() {
				row["last_ok"] = l.lastOK.UTC().Format(time.RFC3339)
			}
			if l.blockSeq > after {
				row["blocked_plugin"], row["blocked_since"] = l.blockPlugin, l.blockSince.UTC().Format(time.RFC3339)
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

func (h *Hub) peerAfter(ctx context.Context, peer string) (int64, error) {
	var after int64
	err := h.db.QueryRowContext(ctx, `SELECT after FROM peer_inbox WHERE peer = ?`, peer).Scan(&after)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return after, err
}

// peersRun runs one pull loop for each peer. One loop per peer: one pull at a time, also for nudges.
func (h *Hub) peersRun(ctx context.Context) {
	if h.peers == nil {
		return
	}
	for _, l := range h.peers.links {
		go h.peerLoop(ctx, l)
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
			wait = min(max(2*wait, peerPollUp), peerPollDown)
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

// peerPluginClient returns the client of the plugin name, or "" and wait: true when the plugin is
// installed here (a config entry binds it) and desired running, but does not run now (starting,
// restarting, crashing). A plugin that is not installed (no record, or a kept record without an
// entry) or is desired stopped gives "" and false.
func (h *Hub) peerPluginClient(name string) (client string, wait bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pl := h.plugins[name]
	if pl == nil || pl.entry == nil || pl.Desired == "stopped" {
		return "", false
	}
	return pl.ClientID, pl.ClientID == ""
}

// peerBlocked records that envelope seq waits for plugin, and returns false when it has waited
// peerBlockMax: then the caller skips it.
func (l *peerLink) peerBlocked(seq int64, plugin string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.blockSeq != seq {
		l.blockSeq, l.blockPlugin, l.blockSince = seq, plugin, time.Now()
	}
	return time.Since(l.blockSince) < peerBlockMax
}

// peerPull pulls the envelopes that the peer holds for this hub and gives each, in seq order, to the
// local plugin of the same name. The cursor moves past an envelope only after the plugin answered:
// accepted, or refused with a result {"refused": code}. An envelope for a plugin that is not
// installed here or is desired stopped is logged and skipped; so is one that waited peerBlockMax
// for a desired-running plugin that does not run. Delivery is in seq order, so a wait blocks every
// later envelope from that peer. Any other error (the peer call, a failed perform) stops the pull,
// and the envelope comes again.
func (h *Hub) peerPull(ctx context.Context, l *peerLink) (more bool, err error) {
	after, err := h.peerAfter(ctx, l.cfg.Name)
	if err != nil {
		return false, err
	}
	out, err := h.peerCall(ctx, l.cfg, "hub.peer.pull", map[string]any{"hub": h.cfg.HubName, "after": after})
	if err != nil {
		return false, err
	}
	var r struct {
		Envelopes []struct {
			Seq    int64  `json:"seq"`
			Plugin string `json:"plugin"`
			Key    string `json:"key"`
			Body   string `json:"body"`
		} `json:"envelopes"`
		More bool `json:"more"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return false, fmt.Errorf("hub.peer.pull on %s: the reply is not JSON: %v", l.cfg.Name, err)
	}
	for _, e := range r.Envelopes {
		if e.Seq <= after {
			continue // a reply to an older cursor
		}
		client, wait := h.peerPluginClient(e.Plugin)
		switch {
		case wait && l.peerBlocked(e.Seq, e.Plugin):
			return false, fmt.Errorf("envelope %d: the plugin %q does not run here", e.Seq, e.Plugin)
		case wait:
			log.Printf("peer %s: envelope %d (%s, key %q) skipped: the plugin did not run for %s", l.cfg.Name, e.Seq, e.Plugin, e.Key, peerBlockMax)
		case client == "":
			log.Printf("peer %s: envelope %d (%s, key %q) skipped: the plugin is not installed or is stopped here", l.cfg.Name, e.Seq, e.Plugin, e.Key)
		default:
			res, err := h.performPlugin(ctx, client, "peer.receive", map[string]any{"from_hub": l.cfg.Name, "key": e.Key, "body": e.Body}, 10000)
			if err != nil {
				return false, fmt.Errorf("envelope %d: %s peer.receive: %v", e.Seq, e.Plugin, err)
			}
			var ans struct {
				Refused string `json:"refused"`
				Message string `json:"message"`
			}
			if json.Unmarshal(res, &ans) == nil && ans.Refused != "" {
				log.Printf("peer %s: envelope %d (%s, key %q) refused: %s: %s", l.cfg.Name, e.Seq, e.Plugin, e.Key, ans.Refused, ans.Message)
			}
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
