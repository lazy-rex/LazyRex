package main

// Plugin sync between hubs (multi-hub v1, lead-fed-r1 item 4). A hub pulls the pins of its one
// "plugins_from" peer and applies them through the plugin manager: an update op of origin peer
// with the peer's pin as its stored target. Only GitHub sources sync; a local source names a path
// on the other host.

import (
	"encoding/json"
	"strings"
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
