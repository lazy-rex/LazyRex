package main

import (
	"context"
	"time"
)

// performMargin is how much longer than a call's timeout_ms the hub waits for a client.perform to
// a client: the hub's wait is timeout_ms + performMargin. It does not bound Rex's queue. timeout_ms
// starts at delivery, so Rex can still deliver the call after the hub stopped waiting
// (rex-protocol.md), and the outcome of a call that the hub's wait cut is unknown.
const performMargin = 2 * time.Second

// performWait is the hub's wait for a client.perform with timeoutMS. A test shortens it.
var performWait = func(timeoutMS int) time.Duration {
	return time.Duration(timeoutMS)*time.Millisecond + performMargin
}

// performCtx is ctx bounded by the hub's wait for a call with timeoutMS.
func performCtx(ctx context.Context, timeoutMS int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, performWait(timeoutMS))
}

// unknownOutcome is the error that a caller gets when a call's wait ended without an answer: the
// outcome is unknown, and the request may still apply (outcome_unknown, as in sessions.go). No
// caller may treat it as a refusal. parent is the caller's context: when it ended first, the
// hub's own wait did not cut the call, so the text names no duration.
func unknownOutcome(parent context.Context, who string, timeoutMS int) error {
	if parent.Err() != nil {
		return refuse("outcome_unknown", "%s did not answer before the caller's deadline; the request may still apply", who)
	}
	return refuse("outcome_unknown", "%s did not answer within %g s; the request may still apply", who, performWait(timeoutMS).Seconds())
}
