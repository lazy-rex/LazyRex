package main

// Reply parts (docs/hub.md "Reply parts"). A Lua plugin sends at most 64 KiB in one control
// message, but the hub and the renderer take about 1 MiB. So a plugin reply to a hub request that
// is larger than inline_max comes in parts: the plugin sends hub.reply.part {id, index, count,
// data} for each slice of the encoded reply, in order, and then replies with the stub
// {"lazyrex_parts": {id, count, bytes}}. The hub joins the slices as bytes and decodes the result
// once: the plugin's value is used whole or not at all.
//
// "lazyrex_parts" is a reserved result key: the SDK refuses a plugin result of that shape. The SDK
// ships in the hub release and takes its limits from the hub.register reply (partsLimits).
//
// Each plugin client has at most one part set (Rex delivers one client.perform at a time to a
// client). A new id discards the old set. A stub resolves only the set with its own id: a late
// stub never touches the next set. A set ends at its stub, at a refused part, when the plugin
// registers again or its instance ends, and 30 s after its last part.
//
// Saved replies: hub.sql.commit with reply_parts {id, count, bytes} joins the set and saves the
// value. The set stays, resolved: the stub with the same id then gets the same value.

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const (
	partsInlineMax = 45056  // a reply up to this size goes in one message
	partsPartBytes = 57344  // the escaped length of one data string (the SDK cuts to fit)
	partsMaxTotal  = 786432 // the joined reply; hub.view.get carries it to the renderer under the 1 MiB guard
	partsMaxID     = 128
	partsMaxCount  = 64 // max_total / 12 KiB: a smaller part is of no use
	partsExpiry    = 30 * time.Second
)

// partsLimits is the "limits" field of the hub.register reply: the one source of these numbers
// for the SDK (configuration, not negotiation).
var partsLimits = map[string]any{"inline_max": partsInlineMax, "part_bytes": partsPartBytes, "max_total": partsMaxTotal}

var partsIDForm = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// partsNow is the clock of the expiry. A test replaces it.
var partsNow = time.Now

// partSet is the part set of one plugin client.
type partSet struct {
	id     string
	count  int
	parts  []string
	size   int
	last   time.Time
	joined []byte // set when hub.sql.commit resolved the set
}

type partSets struct {
	mu   sync.Mutex
	sets map[string]*partSet // by plugin client ID
}

// sweepLocked discards the expired sets. ps.mu must be held.
func (ps *partSets) sweepLocked(now time.Time) {
	for c, s := range ps.sets {
		if now.Sub(s.last) > partsExpiry {
			delete(ps.sets, c)
		}
	}
}

// drop discards the set of a client: the client registered again or ended.
func (ps *partSets) drop(client string) {
	ps.mu.Lock()
	delete(ps.sets, client)
	ps.mu.Unlock()
}

// part handles hub.reply.part from a registered plugin client. A refused part discards the set.
func (ps *partSets) part(client string, args map[string]any) (any, error) {
	id, _ := args["id"].(string)
	index, iok := intArg(args["index"])
	count, cok := intArg(args["count"])
	data, dok := args["data"].(string)
	if len(id) > partsMaxID || !partsIDForm.MatchString(id) || !iok || !cok || !dok {
		return nil, refuse("invalid_input", "hub.reply.part takes id (at most %d bytes of [A-Za-z0-9._:-]), index, count and data (a string)", partsMaxID)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := partsNow()
	if ps.sets == nil {
		ps.sets = map[string]*partSet{}
	}
	s := ps.sets[client]
	if s != nil && (s.id != id || now.Sub(s.last) > partsExpiry) {
		delete(ps.sets, client) // a new id discards the old set
		s = nil
	}
	fail := func(code, format string, a ...any) (any, error) {
		delete(ps.sets, client)
		return nil, refuse(code, "part set %s: "+format, append([]any{id}, a...)...)
	}
	if count < 1 || count > partsMaxCount || index < 0 || index >= count {
		return fail("parts_invalid", "index %d of count %d", index, count)
	}
	if s == nil {
		if index != 0 {
			return fail("parts_invalid", "the first part has index %d, not 0", index)
		}
		ps.sweepLocked(now)
		s = &partSet{id: id, count: count}
		ps.sets[client] = s
	}
	switch {
	case s.joined != nil:
		return fail("parts_invalid", "the set is already joined")
	case count != s.count:
		return fail("parts_invalid", "count %d, but the set has count %d", count, s.count)
	case index != len(s.parts): // also a repeated index
		return fail("parts_invalid", "index %d, but the next index is %d", index, len(s.parts))
	case data == "":
		return fail("parts_invalid", "part %d is empty", index)
	case s.size+len(data) > partsMaxTotal:
		return fail("too_large", "more than %d bytes", partsMaxTotal)
	}
	s.parts = append(s.parts, data)
	s.size += len(data)
	s.last = now
	return map[string]any{}, nil
}

// join checks a stub's id, count and bytes against the client's set and returns the joined value.
// keep: the set stays, resolved (hub.sql.commit); else the set ends, also after a failure.
func (ps *partSets) join(client string, stub map[string]any, keep bool) ([]byte, error) {
	id, _ := stub["id"].(string)
	count, cok := intArg(stub["count"])
	size, bok := intArg(stub["bytes"])
	if id == "" || !cok || !bok || size < 0 || size > partsMaxTotal {
		return nil, refuse("parts_invalid", "the parts stub takes id, count and bytes (at most %d)", partsMaxTotal)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	s := ps.sets[client]
	if s == nil || s.id != id || partsNow().Sub(s.last) > partsExpiry {
		return nil, refuse("parts_invalid", "no part set %s", id)
	}
	if !keep {
		delete(ps.sets, client)
	}
	if s.joined == nil {
		if count != s.count || len(s.parts) != s.count {
			delete(ps.sets, client)
			return nil, refuse("parts_invalid", "part set %s: %d of %d parts, the stub says %d", id, len(s.parts), s.count, count)
		}
		var b strings.Builder
		b.Grow(s.size)
		for _, p := range s.parts {
			b.WriteString(p)
		}
		joined := []byte(b.String())
		if !json.Valid(joined) {
			delete(ps.sets, client)
			return nil, refuse("parts_invalid", "part set %s: the joined %d bytes are not one JSON value", id, len(joined))
		}
		s.joined, s.parts = joined, nil
	}
	if count != s.count || len(s.joined) != size {
		delete(ps.sets, client)
		return nil, refuse("parts_invalid", "part set %s: %d parts and %d bytes, the stub says %d and %d", id, s.count, len(s.joined), count, size)
	}
	return s.joined, nil
}

// partsStub returns the stub fields when a reply is exactly {"lazyrex_parts": {...}}. A reply
// that does not start with the key pays one prefix check.
func partsStub(raw json.RawMessage) (map[string]any, bool) {
	t := bytes.TrimLeft(raw, " \t\r\n")
	if len(t) == 0 || t[0] != '{' || !bytes.HasPrefix(bytes.TrimLeft(t[1:], " \t\r\n"), []byte(`"lazyrex_parts"`)) {
		return nil, false
	}
	var v map[string]json.RawMessage
	if json.Unmarshal(raw, &v) != nil || len(v) != 1 || v["lazyrex_parts"] == nil {
		return nil, false
	}
	var stub map[string]any
	if json.Unmarshal(v["lazyrex_parts"], &stub) != nil || stub == nil {
		return map[string]any{}, true // a stub that join refuses
	}
	return stub, true
}

// resolveReply returns a plugin reply: the joined value for a parts stub, else the reply itself.
func (h *Hub) resolveReply(client string, raw json.RawMessage) (json.RawMessage, error) {
	stub, ok := partsStub(raw)
	if !ok {
		return raw, nil
	}
	v, err := h.parts.join(client, stub, false)
	if se, isStore := err.(*storeError); isStore {
		// A plugin error answer (definiteFailure): a render shows the Error tree.
		return nil, &rex.RexError{Code: se.code, Message: se.msg}
	}
	return v, err
}

// performPlugin calls an action on a plugin client and returns its reply, with the reply parts
// resolved. Every hub path that reads a plugin reply uses it.
func (h *Hub) performPlugin(ctx context.Context, client, action string, args any, timeoutMS int) (json.RawMessage, error) {
	var res struct {
		Result json.RawMessage `json:"result"`
	}
	if err := h.rex.Call(ctx, "client.perform", map[string]any{"client_id": client, "action": action, "args": args, "timeout_ms": timeoutMS}, &res); err != nil {
		return nil, err
	}
	return h.resolveReply(client, res.Result)
}

// intArg reads a JSON number that is a whole number.
func intArg(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}
