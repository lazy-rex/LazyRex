package main

import (
	"context"
	"errors"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// view.changed delivery. Rex delivers client.perform requests to a client one at a time, and
// timeout_ms starts at the delivery (rex-protocol.md). So a renderer that is busy holds every
// request sent to it. One request per render filled its queue: 20 renders while the renderer
// was busy were 20 requests (timeout audit, row 10, 2026-10-22).
//
// The hub keeps at most one view.changed request outstanding for each view, role (composer or
// printer) and attachment. A notification during that request marks the sender pending: when the
// request ends, one more request tells the latest revision, however many notifications came. A
// notification counts even with an unchanged revision: an operation completion asks for one with
// an unchanged tree (vs.notify; Codex review of c2e9a78).
//
// timeout_ms is the only bound: the hub sets no shorter local deadline. A failed request is tried
// again (three attempts in total), because the renderer can refuse a request just before it
// records its attachment (requestClose). A success, or a new notification after a failure, starts
// a fresh count.
//
// A view close (forgetView), a new attachment of the role, a render pass that no longer finds the
// attachment, and the hub stop cancel the notifier: its outstanding call returns at once and it
// sends nothing more. Rex can still deliver a queued request later; its reply has no receiver.

const (
	roleComposer = "composer"
	rolePrinter  = "printer"
)

type notifyKey struct{ view, role, attachment string }

// viewNotifier is the delivery of one key. h.vs.mu guards want, pending and attempts.
type viewNotifier struct {
	plugin, client string
	want           int64 // the latest revision to tell
	pending        bool  // a notification came after the current request was sent
	attempts       int   // failed attempts since the last notification
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{} // closed when the sender has ended
}

// viewChangedRetry is the wait after each failed attempt.
var viewChangedRetry = 300 * time.Millisecond

// viewChangedAttempts is the number of attempts for one revision.
const viewChangedAttempts = 3

// tellViewChanged tells the renderer of the role that the view has a new revision. It starts the
// sender of the key, or raises the revision of the running one.
func (h *Hub) tellViewChanged(plugin, role, client, viewID, attachment string, rev int64) {
	k := notifyKey{viewID, role, attachment}
	vs := h.vs
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if n, ok := vs.senders[k]; ok {
		if rev > n.want {
			n.want = rev
		}
		n.pending = true
		return
	}
	// A new attachment of the role replaces the old one: its request no longer waits for the old.
	for old, n := range vs.senders {
		if old.view == viewID && old.role == role {
			vs.retireLocked(old, n)
		}
	}
	if vs.senders == nil {
		vs.senders = map[notifyKey]*viewNotifier{}
	}
	parent := vs.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	n := &viewNotifier{plugin: plugin, client: client, want: rev, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	// The sender owns the key before it starts: a second tellViewChanged marks it pending.
	vs.senders[k] = n
	go h.notifySender(k, n)
}

// retireLocked cancels the notifier and removes it. h.vs.mu must be held.
func (vs *viewSvc) retireLocked(k notifyKey, n *viewNotifier) {
	n.cancel()
	if vs.senders[k] == n {
		delete(vs.senders, k)
	}
}

// retireNotifiers cancels every notifier of the view: the view closed.
func (vs *viewSvc) retireNotifiers(viewID string) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for k, n := range vs.senders {
		if k.view == viewID {
			vs.retireLocked(k, n)
		}
	}
}

// pruneNotifiers cancels the notifiers of the plugin whose attachment the render pass did not
// find: composers lists the view's attachment, printers the printer's.
func (vs *viewSvc) pruneNotifiers(plugin string, composers map[string]string, printers map[string][2]string) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for k, n := range vs.senders {
		if n.plugin != plugin {
			continue
		}
		current := composers[k.view]
		if k.role == rolePrinter {
			current = printers[k.view][1]
		}
		if current != k.attachment {
			vs.retireLocked(k, n)
		}
	}
}

// notifySender sends the requests of one key, one at a time, until no newer revision is waiting.
// Each exit decides to stop and removes the sender under one h.vs.mu lock. So tellViewChanged
// either marks it pending before that lock, and the sender sends again, or finds no sender after it and
// starts a new one (Codex review of 8025d6b: a gap between the two lost a revision).
func (h *Hub) notifySender(k notifyKey, n *viewNotifier) {
	vs := h.vs
	defer func() {
		if hook := senderEndHook; hook != nil {
			hook(k)
		}
		close(n.done)
	}()
	// stopLocked ends the sender. h.vs.mu must be held; it is released.
	stopLocked := func() {
		vs.retireLocked(k, n)
		vs.mu.Unlock()
	}
	current := h.composerAttachment
	if k.role == rolePrinter {
		current = h.transcriptAttachment
	}
	for {
		// The attachment is read outside h.vs.mu: the database has one connection.
		moved := current(n.ctx, k.view) != k.attachment
		vs.mu.Lock()
		if moved || n.ctx.Err() != nil {
			stopLocked()
			return
		}
		n.pending = false
		rev := n.want
		vs.mu.Unlock()
		err := viewChangedCall(n.ctx, h, n.client, k.view, k.attachment, rev)
		vs.mu.Lock()
		switch {
		case n.ctx.Err() != nil, err != nil && targetGone(err):
			stopLocked()
			return
		case n.pending:
			n.attempts = 0 // a new notification: after a success, or a fresh count after a failure
			vs.mu.Unlock()
			continue
		case err == nil:
			stopLocked()
			return
		}
		n.attempts++
		attempts := n.attempts
		if attempts >= viewChangedAttempts {
			stopLocked()
			return
		}
		vs.mu.Unlock()
		select {
		case <-n.ctx.Done():
		case <-time.After(time.Duration(attempts) * viewChangedRetry):
		}
	}
}

// senderEndHook is a test hook: it runs after a sender has ended, before its done channel closes.
var senderEndHook func(notifyKey)

// targetGone reports whether Rex says that the renderer's client is gone.
func targetGone(err error) bool {
	var re *rex.RexError
	return errors.As(err, &re) && re.Code == "not_found"
}

// viewChangedCall sends one view.changed. A test replaces it.
var viewChangedCall = func(ctx context.Context, h *Hub, client, viewID, attachment string, rev int64) error {
	return h.rex.Call(ctx, "client.perform", map[string]any{"client_id": client, "action": "view.changed",
		"args": map[string]any{"view_id": viewID, "attachment": attachment, "revision": revString(rev)}, "timeout_ms": 5000}, nil)
}
