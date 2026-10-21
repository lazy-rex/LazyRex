package main

// hub.digest (docs/hub.md, "Digests"): the SHA-256 of each text, for a plugin that cannot hash a
// body in Lua. A Lua plugin sends a body over 64 KiB in chunks: it digests each chunk, then the
// joined chunk digests. No state, no grant.

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	digestMaxTexts = 16
	digestMaxText  = 65536  // bytes of one text
	digestMaxTotal = 262144 // bytes of all texts
)

// digest answers hub.digest for the registered current instance of an entry-backed plugin.
func (h *Hub) digest(clientID string, args map[string]any) (any, error) {
	h.mu.Lock()
	var ok bool
	for name, pl := range h.plugins {
		if clientID != "" && pl.ClientID == clientID && h.registeredLocked(name) {
			ok = true
		}
	}
	h.mu.Unlock()
	if !ok {
		return nil, refuse("forbidden", "hub.digest needs the registered current instance of a configured plugin")
	}
	for k := range args {
		if k != "texts" {
			return nil, refuse("invalid_input", "unknown field %s", k)
		}
	}
	list, isList := args["texts"].([]any)
	if !isList || len(list) == 0 || len(list) > digestMaxTexts {
		return nil, refuse("invalid_input", "texts must be a list of 1 to %d strings", digestMaxTexts)
	}
	texts := make([]string, len(list))
	total := 0
	for i, v := range list {
		s, isText := v.(string)
		if !isText {
			return nil, refuse("invalid_input", "texts[%d] must be a string", i+1)
		}
		if len(s) > digestMaxText {
			return nil, refuse("too_large", "texts[%d] is %d bytes, over the %d-byte limit", i+1, len(s), digestMaxText)
		}
		total += len(s)
		texts[i] = s
	}
	if total > digestMaxTotal {
		return nil, refuse("too_large", "the texts are %d bytes, over the %d-byte limit", total, digestMaxTotal)
	}
	out := make([]string, len(texts))
	for i, s := range texts {
		sum := sha256.Sum256([]byte(s))
		out[i] = hex.EncodeToString(sum[:])
	}
	return map[string]any{"digests": out}, nil
}
