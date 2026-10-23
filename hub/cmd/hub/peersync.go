package main

// Plugin sync between hubs (multi-hub v1, lead-fed-r1 item 4). A hub pulls the pins of its one
// "plugins_from" peer and applies them through the plugin manager: an update op of origin peer
// with the peer's pin as its stored target. Only GitHub sources sync; a local source names a path
// on the other host. The peer wins for each source that it lists. A source that this hub does not
// have is only offered (hub.peer.status); the user adds it. A config entry of this hub is skipped.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	peerSyncEvery = time.Minute     // a sync of the plugins_from peer
	peerSyncRetry = 5 * time.Minute // the wait after an op of the same target ended
)

// peerTarget checks a peer target (a pin document entry, pinsync.go) for the entry whose source is
// source, and gives the op's stored target. The target is the same repository and subdir; its ref
// can differ (the peer updated to another tag). The release tag of a latest target is the peer's
// claim: the fetch checks only that the commit's tree gives the peer's digest.
func peerTarget(raw json.RawMessage, source string) (string, error) {
	var p pinEntry
	if err := decodeStrict(raw, &p); err != nil {
		return "", bad("invalid: target: %v", err)
	}
	if err := p.check(); err != nil {
		return "", bad("invalid: target %s: %v", p.Source, err)
	}
	cur, next := &configEntry{Source: source}, &configEntry{Source: p.Source}
	if cur.checkSource("") != nil || next.checkSource("") != nil || !sameRepo(cur, next) {
		return "", bad("invalid: target %s is not the repository of %s", p.Source, source)
	}
	b, _ := json.Marshal(opResolved{Source: p.Source, RefKind: p.RefKind, Tag: p.Tag, Commit: p.Commit,
		FetchRef: p.Commit, Digest: p.Digest, ByID: true})
	return string(b), nil
}

// peerOrigin is the op origin of a sync from the peer hub name.
func peerOrigin(name string) opOrigin { return opOrigin{Kind: "peer", ID: strings.ToLower(name)} }

// peerManaged is a managed entry of the source hub: the receiver offers a new source with its id.
type peerManaged struct {
	ID     string `json:"id"`
	Source string `json:"source"`
}

// peerPluginsServe is hub.peer.plugins, called by a peer hub: the pin document of this hub
// (lazyrex-hub plugin export) and the id and source of each managed GitHub entry.
func (h *Hub) peerPluginsServe(args map[string]any) (any, error) {
	peer, _ := args["hub"].(string)
	if h.peerLink(peer) == nil {
		return nil, refuse("unknown_hub", "%q is not a configured peer", peer)
	}
	var doc bytes.Buffer
	if err := pluginExport(h.cfg.path, &doc); err != nil {
		return nil, refuse("refused", "%v", err)
	}
	managed := []peerManaged{}
	h.entriesMu.Lock()
	for _, e := range h.entries {
		if e.managed != "" && e.github {
			managed = append(managed, peerManaged{ID: e.managed, Source: e.Source})
		}
	}
	h.entriesMu.Unlock()
	return map[string]any{"pins": json.RawMessage(doc.Bytes()), "managed": managed}, nil
}

// peerSync is the state of the plugin sync with the plugins_from peer (hub.peer.status,
// "plugin_sync").
type peerSync struct {
	LastOK    *string          `json:"last_ok"`    // RFC 3339 time of the last sync with no error
	LastError string           `json:"last_error"` // the last sync's error, "" for none
	Offered   []peerManaged    `json:"offered"`    // the peer's sources that this hub does not have
	Skipped   []peerSkip       `json:"skipped"`    // the peer's sources that this hub does not sync
	Updating  []map[string]any `json:"updating"`   // {entry, op_id, phase}: the ops of the last sync
	logged    map[string]bool  // the skipped sources already logged
}

type peerSkip struct {
	Source string `json:"source"`
	Entry  string `json:"entry"`
	Why    string `json:"why"`
}

// peerPluginsLoop syncs the plugins of the plugins_from peer: at the start, then every minute.
func (h *Hub) peerPluginsLoop(ctx context.Context, l *peerLink) {
	for {
		s, err := h.peerPluginsSync(ctx, l)
		l.mu.Lock()
		if l.sync == nil {
			l.sync = &peerSync{logged: map[string]bool{}}
		}
		if err != nil {
			if l.sync.LastError != err.Error() {
				log.Printf("peer %s: plugin sync: %v", l.cfg.Name, err)
			}
			l.sync.LastError = err.Error()
		} else {
			now := time.Now().UTC().Format(time.RFC3339)
			l.sync.LastOK, l.sync.LastError = &now, ""
			l.sync.Offered, l.sync.Updating = s.Offered, s.Updating
			l.sync.Skipped = s.Skipped
			for _, k := range s.Skipped {
				if !l.sync.logged[k.Source] {
					log.Printf("peer %s: plugin sync skips %s: %s", l.cfg.Name, k.Source, k.Why)
					l.sync.logged[k.Source] = true
				}
			}
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(peerSyncEvery):
		}
	}
}

// peerPluginsSync runs one sync: it reads the peer's pins, and admits a peer update op for each
// managed GitHub entry of this hub (the same repository and subdir) whose pin is not the peer's.
func (h *Hub) peerPluginsSync(ctx context.Context, l *peerLink) (*peerSync, error) {
	out, err := h.peerCall(ctx, l.cfg, "hub.peer.plugins", map[string]any{"hub": h.cfg.HubName})
	if err != nil {
		return nil, err
	}
	var r struct {
		Pins    json.RawMessage `json:"pins"`
		Managed []peerManaged   `json:"managed"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("hub.peer.plugins on %s: the reply is not JSON: %v", l.cfg.Name, err)
	}
	doc, err := decodePinDoc(r.Pins)
	if err != nil {
		return nil, fmt.Errorf("hub.peer.plugins on %s: the pin document: %v", l.cfg.Name, err)
	}
	lock, err := readLock(lockPath(h.cfg.path, h.cfg.Lock))
	if err != nil {
		return nil, err
	}
	// Copies, read under entriesMu: a reload changes an entry in place (updateManaged).
	h.entriesMu.Lock()
	var mine []configEntry
	for _, e := range h.entries {
		if e.github {
			mine = append(mine, *e)
		}
	}
	h.entriesMu.Unlock()
	s := &peerSync{Offered: []peerManaged{}, Skipped: []peerSkip{}, Updating: []map[string]any{}}
	var errs []string
	for _, p := range doc.Entries {
		pe := &configEntry{Source: p.Source}
		if pe.checkSource("") != nil {
			continue // decodePinDoc checked it
		}
		var match *configEntry
		for i := range mine {
			if sameRepo(&mine[i], pe) {
				match = &mine[i]
				break
			}
		}
		switch {
		case match == nil:
			o := peerManaged{Source: p.Source}
			for _, m := range r.Managed {
				if m.Source == p.Source {
					o.ID = m.ID
				}
			}
			s.Offered = append(s.Offered, o)
			continue
		case match.managed == "":
			s.Skipped = append(s.Skipped, peerSkip{Source: p.Source, Entry: opEntryKey(match), Why: "a config entry of this hub"})
			continue
		}
		// The whole pin counts: the same commit of a latest source can have another release tag.
		if cur, ok := lock.Plugins[p.Source]; ok && match.Source == p.Source && cur.lockPin == p.lockPin {
			continue // in sync
		}
		op, err := h.peerUpdate(ctx, l.cfg.Name, opEntryKey(match), p)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", opEntryKey(match), err))
			continue
		}
		if op != nil {
			s.Updating = append(s.Updating, op)
		}
	}
	if len(errs) > 0 {
		return s, errors.New(strings.Join(errs, "; "))
	}
	return s, nil
}

// peerUpdate admits a peer update of entry to the pin p, unless an op of the peer on that entry
// runs, or an op of the same target ended less than peerSyncRetry ago. The key is the target's
// hash and a count, so a later try of the same target is a new op. It gives {entry, op_id, phase}
// of the running or new op, or nil.
func (h *Hub) peerUpdate(ctx context.Context, peer, entry string, p pinEntry) (map[string]any, error) {
	target, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	scope, base := "peer:"+strings.ToLower(peer), hashBytes(target)
	var id, key, phase string
	var updated int64
	err = h.db.QueryRowContext(ctx, `SELECT op_id, key, phase, updated FROM plugin_ops WHERE scope = ? AND entry = ? ORDER BY created DESC, rowid DESC LIMIT 1`,
		scope, entry).Scan(&id, &key, &phase, &updated)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, err
	case err == nil && !opFinal[phase]:
		return map[string]any{"entry": entry, "op_id": id, "phase": phase}, nil
	case err == nil && strings.HasPrefix(key, base+"#") && time.Since(time.UnixMilli(updated)) < peerSyncRetry:
		return nil, nil
	}
	var n int
	if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM plugin_ops WHERE scope = ? AND key LIKE ?`, scope, base+"#%").Scan(&n); err != nil {
		return nil, err
	}
	v, err := h.admitOp(ctx, peerOrigin(peer), scope, fmt.Sprintf("%s#%d", base, n+1), opRequest{Op: "update", Entry: entry, Target: target})
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return map[string]any{"entry": entry, "op_id": m["op_id"], "phase": m["phase"]}, nil
}
