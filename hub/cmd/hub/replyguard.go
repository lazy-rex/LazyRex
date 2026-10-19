package main

// The reply guard. Rex closes a client's connection when one frame from it is larger than the
// welcome's limits.max_message_bytes (rex-protocol.md, "Rex Lua runtime"). A result that is too
// large would disconnect the hub for every caller, so each client.reply is measured before it is
// sent. A reply over the limit becomes the error reply_too_large: the caller gets an answer at
// once, and the hub stays connected.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"unicode/utf8"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const (
	// replyMargin keeps each reply frame this far below the server's limit. The guard measures the
	// exact frame (with the longest request ID); the margin is only for a server that counts a
	// frame differently.
	replyMargin = 1 << 10
	// referenceMaxMessage is the Control v1 reference limit, for a welcome without limits.
	referenceMaxMessage = 64 << 10
)

// replyLimit is the largest client.reply frame that the guard sends on r.
func replyLimit(r *rex.Rex) int {
	max := r.MaxMessage()
	if max <= 0 {
		max = referenceMaxMessage
	}
	return max - replyMargin
}

// guardReply encodes reply ({call_id, result} or {call_id, error}). When its client.reply frame
// is over limit, it returns the encoded reply_too_large error instead, with the attempted size.
// The guard knows only that the handler returned a result or an error. A result can be an
// operation that is pending or failed, and an error can follow partial changes. So the message
// states no outcome; it tells the caller to keep the operation key and check the status.
func guardReply(action string, reply map[string]any, limit int) (json.RawMessage, bool) {
	raw, err := json.Marshal(reply)
	if err != nil {
		raw, _ = json.Marshal(map[string]any{"call_id": reply["call_id"], "error": &rex.RexError{Code: "failed",
			Message: "the hub could not encode the reply of " + shortName(action) + ": " + err.Error()}})
		return raw, false
	}
	n := rex.RequestFrameBytes("client.reply", raw)
	if n <= limit {
		return raw, true
	}
	what := "a result"
	if _, failed := reply["error"]; failed {
		what = "an error"
	}
	msg := fmt.Sprintf("the handler of %s returned %s that was too large to send (%d bytes, limit %d). This error does not undo changes and does not tell the operation's final state. Before you retry a change, keep its operation key and check its status; ask for less at a time.",
		shortName(action), what, n, limit)
	raw, _ = json.Marshal(map[string]any{"call_id": reply["call_id"], "error": &rex.RexError{Code: "reply_too_large", Message: msg}})
	return raw, false
}

// shortName quotes an action name of at most 64 bytes.
func shortName(action string) string {
	if len(action) <= 64 {
		return strconv.Quote(action)
	}
	cut := 64
	for cut > 0 && !utf8.RuneStart(action[cut]) {
		cut--
	}
	return strconv.Quote(action[:cut]) + "…"
}

// sendReply sends reply on r through the guard.
func sendReply(ctx context.Context, r *rex.Rex, action string, reply map[string]any) error {
	raw, ok := guardReply(action, reply, replyLimit(r))
	if !ok {
		log.Printf("reply %s %v: not sent as is: %s", action, reply["call_id"], raw)
	}
	return r.Call(ctx, "client.reply", raw, nil)
}
