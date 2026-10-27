package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

type mutation struct {
	Method, Key, Node string
	Args              map[string]any
}
type operationResult struct {
	Status       string         `json:"status"`
	Effects      string         `json:"effects"`
	EffectsError string         `json:"effects_error"`
	OperationKey string         `json:"operation_key"`
	Result       map[string]any `json:"result"`
	Error        *wireError     `json:"error"`
}
type completed struct {
	Request mutation
	Outcome operationResult
}
type connectionMsg struct {
	Ready bool
	Text  string
}

type attachRefused struct{ error }

func attachNotReady(err error) bool {
	var e *wireError
	return errors.As(err, &e) && (e.Code == "not_ready" || strings.HasPrefix(e.Message, "not_ready"))
}

type viewClient struct {
	Socket, Session, ViewID, Plugin, Route string
	Requests                               chan mutation
	emit                                   func(tea.Msg)
	seen                                   chan struct{}
	focused                                bool
	seenReason                             string // the reason for the next hub.view.seen; "focus" wins
	mu                                     sync.Mutex
	w                                      *wire
	hub, attachment                        string
	replacementID                          string
	changed                                chan struct{}
	lost                                   bool
	available                              bool
	// The seen gate (thread view v1.2 item 3): seenClosed until the renderer decides its mode;
	// seenAlt sends as before; seenThread sends only while seenOK (the current tree's newest ID
	// is printed). outFailed is the output boundary's latch: after a fault no seen goes out.
	// gen counts the attachments; each emitted Snapshot carries the gen of the attachment that
	// fetched it, and only a tree of the current attachment opens the gate.
	seenGate  int
	seenOK    bool
	outFailed func() bool
	gen       uint64
	// became is the view.become that this process accepted: a replay of it answers ok and changes
	// nothing; any other view.become is refused.
	became *becomeRecord
}

// becomeRecord is an accepted view.become: the replacement, the pair it replaced, the next view.
type becomeRecord struct{ repl, oldView, oldAttachment, next string }

// becomeRequested asks the model to end this process so that main re-executes the renderer as
// the view ViewID (plugin, route) in the same pane and process: an in-place replacement, or an
// attach that the hub answered with the block's current view.
type becomeRequested struct{ ViewID, Plugin, Route string }

// errBecame ends the connection loop: the process re-executes as another view.
var errBecame = errors.New("this pane shows another view now")

const (
	seenClosed = iota
	seenAlt
	seenThread
)

// decideSeen opens the seen gate when the renderer decides its mode. Alt mode sends one seen at
// once (reason "connect" unless one waits), then as before. Thread mode waits for a printed newest
// ID (allowSeen).
func (v *viewClient) decideSeen(thread bool) {
	v.mu.Lock()
	v.seenGate, v.seenOK = seenAlt, false
	if thread {
		v.seenGate = seenThread
	}
	v.mu.Unlock()
	if !thread {
		v.wakeSeen()
	}
}

// allowSeen opens a thread view's gate: the current tree's newest ID is printed. gen is the
// current tree's attachment generation: a tree of an earlier attachment opens nothing. reason is
// "content" after the print of new messages; "" keeps a waiting reason (else "connect"). It wakes
// the run loop only when the gate opens or a reason is new, so a draft-only tree sends nothing.
func (v *viewClient) allowSeen(reason string, gen uint64) {
	v.mu.Lock()
	if gen != v.gen {
		v.mu.Unlock()
		return
	}
	wake := !v.seenOK || reason != ""
	v.seenOK = true
	if reason != "" && v.seenReason != "focus" {
		v.seenReason = reason
	}
	v.mu.Unlock()
	if wake {
		v.wakeSeen()
	}
}

// currentGen is the current attachment generation.
func (v *viewClient) currentGen() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.gen
}

// blockSeen closes a thread view's gate: the current tree has no newest ID, or it is not printed.
func (v *viewClient) blockSeen() {
	v.mu.Lock()
	v.seenOK = false
	v.mu.Unlock()
}

// closeSeen closes the gate for good (an output failure). The output boundary calls it from its
// writer, before the model hears of the failure.
func (v *viewClient) closeSeen() {
	v.mu.Lock()
	v.seenGate, v.seenOK = seenClosed, false
	v.outFailed = func() bool { return true }
	v.mu.Unlock()
}

func (v *viewClient) wakeSeen() {
	select {
	case v.seen <- struct{}{}:
	default:
	}
}

// focus records the current terminal focus before the broker sends a read notice. reason says
// why the user sees the view now: "focus" (the pane got focus) or "content" (new messages arrived
// while it had focus). A plugin can refresh on "focus" without a refresh/render loop.
func (v *viewClient) focus(focused bool, reason string) {
	v.mu.Lock()
	v.focused = focused
	if focused && v.seenReason != "focus" {
		v.seenReason = reason
	}
	v.mu.Unlock()
	if focused {
		v.wakeSeen()
	}
}

// markSeen sends hub.view.seen when the view has focus and the gate is open. A closed gate keeps
// the reason for later.
func (v *viewClient) markSeen(ctx context.Context) {
	v.mu.Lock()
	open := v.seenGate == seenAlt || v.seenGate == seenThread && v.seenOK
	failed := v.outFailed != nil && v.outFailed()
	if !open || failed {
		v.mu.Unlock()
		return
	}
	focused, reason := v.focused, v.seenReason
	v.seenReason = ""
	v.mu.Unlock()
	if focused {
		if reason == "" {
			reason = "connect"
		}
		var out any
		_ = v.call(ctx, "hub.view.seen", map[string]any{"reason": reason}, &out)
	}
}

func (v *viewClient) pulse() {
	select {
	case v.changed <- struct{}{}:
	default:
	}
}

// event records notifications without blocking the Rex response reader.
func (v *viewClient) event(raw json.RawMessage) {
	var ev struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	if ev.Type == "session_event" {
		var se struct {
			Event struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			} `json:"event"`
		}
		if json.Unmarshal(ev.Payload, &se) != nil {
			return
		}
		ev.Type, ev.Payload = se.Event.Type, se.Event.Payload
	}
	if ev.Type == "client_perform_requested" {
		var p struct {
			CallID    string `json:"call_id"`
			Action    string `json:"action"`
			Requester struct {
				ClientID string `json:"client_id"`
			} `json:"requester"`
			Args struct {
				ViewID        string `json:"view_id"`
				Attachment    string `json:"attachment"`
				Revision      string `json:"revision"`
				ReplacementID string `json:"replacement_id"`
				NextTitle     string `json:"next_title"`
				NextViewID    string `json:"next_view_id"`
				NextPlugin    string `json:"next_plugin"`
				NextRoute     string `json:"next_route"`
			} `json:"args"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			return
		}
		if p.Action == "view.become" {
			a := p.Args
			v.becomeCall(p.CallID, p.Requester.ClientID, becomeRecord{repl: a.ReplacementID, oldView: a.ViewID, oldAttachment: a.Attachment, next: a.NextViewID}, a.NextPlugin, a.NextRoute)
			return
		}
		v.mu.Lock()
		valid := (p.Action == "view.changed" || p.Action == "view.reload" || p.Action == "view.release" || p.Action == "view.resume") && p.Requester.ClientID == v.hub && p.Args.ViewID == v.ViewID && p.Args.Attachment == v.attachment
		w := v.w
		v.mu.Unlock()
		if w == nil {
			return
		}
		if valid && (p.Action == "view.release" || p.Action == "view.resume") {
			v.emit(replacementRequested{Action: p.Action, ID: p.Args.ReplacementID, Title: p.Args.NextTitle,
				valid: func() bool {
					v.mu.Lock()
					defer v.mu.Unlock()
					return v.w == w && v.hub == p.Requester.ClientID && v.attachment == p.Args.Attachment
				},
				reply: func(status string) tea.Cmd {
					return func() tea.Msg {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						_ = w.call(ctx, "client.reply", map[string]any{"call_id": p.CallID, "result": map[string]any{"status": status}}, nil)
						return nil
					}
				}})
			return
		}
		reply := map[string]any{"call_id": p.CallID}
		if valid {
			reply["result"] = map[string]any{"received": true}
			if p.Action == "view.changed" {
				v.pulse()
			}
		} else {
			reply["error"] = &wireError{Code: "refused", Message: "The notification does not match the current view attachment"}
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := w.call(ctx, "client.reply", reply, nil); err == nil && valid {
				if p.Action == "view.reload" {
					v.emit(reloadRequested{})
				}
			}
		}()
		return
	}
	if ev.Type == "client_left" || ev.Type == "client_changed" || ev.Type == "client_joined" || ev.Type == "client_connection_changed" || ev.Type == "client_disconnected" {
		var p struct {
			ClientID string `json:"client_id"`
			State    string `json:"connection_state"`
			Client   struct {
				ID    string `json:"client_id"`
				State string `json:"connection_state"`
				Info  struct {
					Label string `json:"label"`
				} `json:"info"`
			} `json:"client"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			return
		}
		v.mu.Lock()
		gone := (p.ClientID == v.hub && p.State != "connected") || (p.Client.ID == v.hub && p.Client.State != "" && p.Client.State != "connected")
		if gone {
			v.lost = true
		}
		available := p.Client.Info.Label == "lazyrex-hub" && p.Client.State != "lost" && (p.Client.ID != v.hub || v.lost)
		if available {
			v.available = true
		}
		v.mu.Unlock()
		if gone || available {
			v.pulse()
		}
	}
}

// becomeCall answers view.become (an in-place replacement, hub inplace.go). The checks run in this
// order: the requester is the hub; a replay of the accepted request answers ok and changes
// nothing; a second, different request is refused; the request names this view and attachment.
// An accepted request answers at once, before the switch: the hub has one request queue, so the
// renderer must not wait on the hub inside a request from it. The attach of the next view is the
// proof that the hub waits for.
func (v *viewClient) becomeCall(callID, requester string, rec becomeRecord, plugin, route string) {
	v.mu.Lock()
	w := v.w
	accept := false
	var refusal *wireError
	switch {
	case requester == "" || requester != v.hub:
		refusal = &wireError{Code: "refused", Message: "view.become comes only from the hub"}
	case v.became != nil && *v.became == rec:
		// A replay: the answer was lost. The switch is under way.
	case v.became != nil:
		refusal = &wireError{Code: "conflict", Message: "this renderer already becomes another view"}
	case rec.oldView != v.ViewID || rec.oldAttachment != v.attachment || rec.next == "" || plugin == "" || route == "":
		refusal = &wireError{Code: "refused", Message: "The notification does not match the current view attachment"}
	default:
		v.became, accept = &rec, true
	}
	v.mu.Unlock()
	if w == nil {
		return
	}
	reply := map[string]any{"call_id": callID}
	if refusal != nil {
		reply["error"] = refusal
	} else {
		reply["result"] = map[string]any{"status": "ok"}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = w.call(ctx, "client.reply", reply, nil)
		if accept {
			v.emit(becomeRequested{ViewID: rec.next, Plugin: plugin, Route: route})
		}
	}()
}

func (v *viewClient) call(ctx context.Context, action string, args map[string]any, out any) error {
	v.mu.Lock()
	w, hub, attachment := v.w, v.hub, v.attachment
	v.mu.Unlock()
	if w == nil || hub == "" {
		return errNotAttached
	}
	input := map[string]any{"view_id": v.ViewID, "attachment": attachment}
	for k, val := range args {
		input[k] = val
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return w.perform(callCtx, hub, action, input, out)
}

func (v *viewClient) connect(ctx context.Context) error {
	v.mu.Lock()
	w := v.w
	v.mu.Unlock()
	if w != nil {
		select {
		case <-w.done:
			w = nil
		default:
		}
	}
	if w == nil {
		var err error
		w, err = dialWire(ctx, v.Socket, v.event)
		if err != nil {
			return err
		}
		v.mu.Lock()
		v.w = w
		v.mu.Unlock()
		if err = w.call(ctx, "client.keymap", map[string]any{"actions": []any{map[string]any{"name": "view.changed", "kind": "client", "title": "Refresh view"}, map[string]any{"name": "view.reload", "kind": "client", "title": "Reload renderer"}, map[string]any{"name": "view.release", "kind": "client", "title": "Replace panel"}, map[string]any{"name": "view.resume", "kind": "client", "title": "Resume panel"}, map[string]any{"name": "view.become", "kind": "client", "title": "Become the next view"}}}, nil); err != nil {
			w.conn.CloseNow()
			return err
		}
		if err = w.call(ctx, "session.attach_all", map[string]any{"event_scope": "all"}, nil); err != nil {
			w.conn.CloseNow()
			return err
		}
	}
	hub, err := w.resolve(ctx)
	if err != nil {
		return err
	}
	var attached struct {
		Status        string            `json:"status"`
		CurrentViewID string            `json:"current_view_id"`
		ViewID        string            `json:"view_id"`
		Plugin        string            `json:"plugin"`
		Route         string            `json:"route"`
		Attachment    string            `json:"attachment"`
		Generation    string            `json:"hub_generation"`
		Replacement   *replacementState `json:"replacement"`
	}
	v.mu.Lock()
	replacementID := v.replacementID
	v.mu.Unlock()
	attachArgs := map[string]any{"view_id": v.ViewID, "schema_version": 1}
	if replacementID != "" {
		attachArgs["replacement_id"] = replacementID
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		err = w.perform(ctx, hub, "hub.view.attach", attachArgs, &attached)
		if err == nil {
			break
		}
		if !attachNotReady(err) {
			if uncertain(err) {
				return err
			}
			return &attachRefused{err}
		}
		if time.Now().After(deadline) {
			return &attachRefused{fmt.Errorf("The hub did not record this pane within three seconds: %w", err)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if attached.Status == "replaced" {
		// An in-place replacement gave this pane to a newer view (a lost view.become, or a reload
		// with an older argv): become it. No attachment was made.
		if attached.CurrentViewID == "" || attached.Plugin == "" || attached.Route == "" {
			return fmt.Errorf("The hub named no current view for this pane")
		}
		v.emit(becomeRequested{ViewID: attached.CurrentViewID, Plugin: attached.Plugin, Route: attached.Route})
		return errBecame
	}
	if attached.ViewID != v.ViewID || attached.Plugin != v.Plugin || attached.Route != v.Route || attached.Attachment == "" {
		return fmt.Errorf("The hub attachment does not match the requested route")
	}
	v.mu.Lock()
	v.hub = hub
	v.attachment = attached.Attachment
	v.seenOK = false // a new attachment sends seen at its first thread frame with a printed newest ID
	v.gen++
	gen := v.gen
	v.lost = false
	v.available = false
	v.mu.Unlock()
	if attached.Replacement != nil {
		id := attached.Replacement.ID
		if id == "" {
			id = replacementID
		}
		v.emit(replacementRecovered{ID: id, State: *attached.Replacement, Initial: replacementID == "", FromAttach: true})
	}
	var snapshot Snapshot
	if err = v.call(ctx, "hub.view.subscribe", nil, &snapshot); err != nil {
		return err
	}
	if err = snapshot.Validate(v.ViewID, v.Route); err != nil {
		return err
	}
	snapshot.gen = gen
	v.emit(snapshot)
	return nil
}

func uncertain(err error) bool {
	var e *wireError
	if !errors.As(err, &e) {
		return true
	}
	if strings.HasPrefix(e.Message, "stale_attachment:") || strings.HasPrefix(e.Message, "unavailable:") {
		return true
	}
	switch e.Code {
	case "timeout", "client_unavailable", "disconnected", "unavailable", "not_found", "stale_attachment":
		return true
	}
	return false
}

func (v *viewClient) admit(ctx context.Context, req mutation) error {
	var result operationResult
	err := v.call(ctx, req.Method, req.Args, &result)
	if err != nil {
		if uncertain(err) {
			return err
		}
		v.emit(completed{req, operationResult{Status: "failed", Error: &wireError{Code: "refused", Message: err.Error()}}})
		return errFinal
	}
	switch result.Status {
	case "accepted", "pending":
		return nil
	case "succeeded", "failed":
		if result.Status == "succeeded" && result.Effects == "pending" {
			return nil
		}
		v.emit(completed{req, result})
		return errFinal
	default:
		return fmt.Errorf("Invalid operation admission status %q", result.Status)
	}
}

var errFinal = errors.New("operation completed")

var errNotAttached = errors.New("the view is not attached")

func (v *viewClient) check(ctx context.Context, pending map[string]mutation) error {
	for key, req := range pending {
		var result operationResult
		if err := v.call(ctx, "hub.view.operation", map[string]any{"operation_key": key}, &result); err != nil {
			return err
		}
		switch result.Status {
		case "not_found":
			if err := v.admit(ctx, req); errors.Is(err, errFinal) {
				delete(pending, key)
			} else if err != nil {
				return err
			}
		case "pending":
		case "succeeded", "failed":
			if result.Status == "succeeded" && result.Effects == "pending" {
				continue
			}
			v.emit(completed{req, result})
			delete(pending, key)
		default:
			return fmt.Errorf("Invalid operation result status %q", result.Status)
		}
	}
	return nil
}

// run serializes requests. Healthy admitted operations wait for notifications.
func (v *viewClient) run(ctx context.Context) {
	pending := map[string]mutation{}
	ready := false
	delay := 250 * time.Millisecond
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer func() {
		v.mu.Lock()
		w := v.w
		v.mu.Unlock()
		if w != nil {
			w.conn.CloseNow()
		}
	}()
	retry := func(err error) {
		ready = false
		text := "Hub unavailable: " + err.Error()
		var count *hubCount
		if errors.As(err, &count) {
			text = count.Error()
		}
		v.emit(connectionMsg{Text: paneSafe(text)})
		timer.Reset(delay + time.Duration(rand.Int64N(int64(delay))))
		delay = min(delay*2, 2*time.Second)
	}
	for {
		v.mu.Lock()
		w := v.w
		v.mu.Unlock()
		var done <-chan struct{}
		if ready && w != nil {
			done = w.done
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			setup, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := v.connect(setup)
			cancel()
			if err != nil {
				if errors.Is(err, errBecame) {
					return // the model ends the process; main re-executes it as the current view
				}
				var refused *attachRefused
				if errors.As(err, &refused) {
					v.emit(connectionMsg{Text: paneSafe("Cannot attach this view: " + err.Error())})
					return
				}
				retry(err)
				continue
			}
			if err = v.check(ctx, pending); err != nil {
				retry(err)
				continue
			}
			ready = true
			delay = 250 * time.Millisecond
			v.emit(connectionMsg{Ready: true, Text: "Connected"})
			v.markSeen(ctx)
		case <-done:
			retry(fmt.Errorf("Rex disconnected"))
		case <-v.seen:
			if ready {
				v.markSeen(ctx)
			}
		case req := <-v.Requests:
			pending[req.Key] = req
			if !ready {
				continue
			}
			err := v.admit(ctx, req)
			if errors.Is(err, errFinal) {
				delete(pending, req.Key)
			} else if err != nil {
				retry(err)
			}
		case <-v.changed:
			v.mu.Lock()
			available := v.available
			v.available = false
			v.mu.Unlock()
			if !ready {
				if available {
					delay = 250 * time.Millisecond
					timer.Reset(time.Duration(rand.Int64N(int64(delay))))
				}
				continue
			}
			v.mu.Lock()
			lost := v.lost
			v.mu.Unlock()
			if lost || available {
				retry(fmt.Errorf("The hub disconnected"))
				continue
			}
			v.mu.Lock()
			gen := v.gen // the attachment that makes this call, read before it
			v.mu.Unlock()
			var snapshot Snapshot
			err := v.call(ctx, "hub.view.get", map[string]any{"route": v.Route}, &snapshot)
			if err == nil {
				err = snapshot.Validate(v.ViewID, v.Route)
			}
			if err != nil {
				retry(err)
				continue
			}
			snapshot.gen = gen
			v.emit(snapshot)
			if err = v.check(ctx, pending); err != nil {
				retry(err)
			}
		}
	}
}

// closeView records an intentional close before the renderer exits.
func (v *viewClient) closeView() error {
	v.mu.Lock()
	connected := v.w != nil
	v.mu.Unlock()
	if !connected {
		return errors.New("The hub is unavailable")
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := v.call(context.Background(), "hub.view.close", nil, &result); err != nil {
		return err
	}
	if result.Status != "closed" {
		return fmt.Errorf("Unexpected close status %q", result.Status)
	}
	return nil
}

type closeFinished struct{ err error }
