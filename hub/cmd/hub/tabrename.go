package main

// Tab rename: the user renames a plugin view's Rex tab, and the plugin gets view.rename
// {route, user, title} with the new label, exactly as typed. The plugin stores the title and declares its tabs again; the hub
// then writes the declared title, which matches the new label.
//
// Loop suppression: each label the hub writes to a window expects exactly one window_label_changed
// echo, and that echo is consumed (a failed write cancels it). So a user rename back to an earlier
// title (A -> B -> A) is still a rename. A label equal to the tab's declared title, Rex's generated
// label ("Window " + the last 8 characters of the window ID), and an empty label are not renames.
// Label events run one at a time, in arrival order (one worker), so rapid renames reach the plugin
// in order. While a rename waits for the plugin, orderTabs leaves that window's label alone; only
// the rename that set the hold releases it.

import (
	"context"
	"log"
	"time"
)

const renameHold = 10 * time.Second // the longest a pending rename keeps orderTabs off a window

type labelEvent struct{ sess, window, label string }

// expectLabel records that the hub is about to write label to window: its echo is not a rename.
func (h *Hub) expectLabel(window, label string) {
	h.ui.mu.Lock()
	defer h.ui.mu.Unlock()
	if h.ui.expect == nil {
		h.ui.expect = map[string]map[string]int{}
	}
	if h.ui.expect[window] == nil {
		h.ui.expect[window] = map[string]int{}
	}
	h.ui.expect[window][label]++
}

// unexpectLabel cancels an expectation: the write failed, so no echo comes. It reports whether an
// expectation was there.
func (h *Hub) unexpectLabel(window, label string) bool {
	h.ui.mu.Lock()
	defer h.ui.mu.Unlock()
	if h.ui.expect[window][label] == 0 {
		return false
	}
	h.ui.expect[window][label]--
	if h.ui.expect[window][label] == 0 {
		delete(h.ui.expect[window], label)
		if len(h.ui.expect[window]) == 0 {
			delete(h.ui.expect, window)
		}
	}
	return true
}

// renamePending reports whether a user rename of window waits for its plugin.
func (h *Hub) renamePending(window string) bool {
	h.ui.mu.Lock()
	defer h.ui.mu.Unlock()
	p, ok := h.ui.renaming[window]
	return ok && time.Since(p.at) < renameHold
}

// queueLabelEvent admits a window_label_changed event, in event order. The echo of a hub write is
// consumed here, before any other check, and goes no further. Other events go to the label worker
// through an unbounded FIFO: no event is dropped.
func (h *Hub) queueLabelEvent(ctx context.Context, sess, window, label string) {
	if window == "" || h.unexpectLabel(window, label) {
		return
	}
	h.ui.mu.Lock()
	h.ui.labelQ = append(h.ui.labelQ, labelEvent{sess, window, label})
	if h.ui.labelKick == nil {
		h.ui.labelKick = make(chan struct{}, 1)
		go h.labelWorker(ctx, h.ui.labelKick)
	}
	kick := h.ui.labelKick
	h.ui.mu.Unlock()
	select {
	case kick <- struct{}{}:
	default:
	}
}

func (h *Hub) labelWorker(ctx context.Context, kick chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		}
		for {
			h.ui.mu.Lock()
			if len(h.ui.labelQ) == 0 {
				h.ui.mu.Unlock()
				break
			}
			e := h.ui.labelQ[0]
			h.ui.labelQ = h.ui.labelQ[1:]
			h.ui.mu.Unlock()
			h.uiTabRenamed(ctx, e.sess, e.window, e.label)
		}
	}
}

// forgetWindowLabels drops the label state of a closed window.
func (h *Hub) forgetWindowLabels(window string) {
	h.ui.mu.Lock()
	defer h.ui.mu.Unlock()
	delete(h.ui.expect, window)
	delete(h.ui.renaming, window)
}

// uiTabRenamed handles window_label_changed {window_id, label}: if the window shows a plugin view
// and the label is a user rename, the plugin gets view.rename. Only that window is checked: another
// window's label can lag behind its declared title (an unread count) without any rename.
func (h *Hub) uiTabRenamed(ctx context.Context, sess, window, newLabel string) {
	rows, err := h.db.QueryContext(ctx, `SELECT v.window_id, v.plugin, v.route, COALESCE(t.title, '') FROM views v
		LEFT JOIN ui_tabs t ON t.plugin = v.plugin AND t.tab = v.tab
		WHERE v.session_id = ? AND v.window_id = ? AND v.status = 'open' AND v.tab != ?`, sess, window, formTab)
	if err != nil {
		log.Printf("tab rename: %v", err)
		return
	}
	type viewTab struct{ window, plugin, route, declared string }
	byWindow := map[string]viewTab{}
	for rows.Next() {
		var v viewTab
		if rows.Scan(&v.window, &v.plugin, &v.route, &v.declared) == nil {
			byWindow[v.window] = v
		}
	}
	rows.Close()
	if len(byWindow) == 0 {
		return
	}
	for _, w := range []struct{ WindowID, Label string }{{window, newLabel}} {
		v, ok := byWindow[w.WindowID]
		if !ok || w.Label == v.declared || w.Label == "" {
			continue
		}
		if len(w.WindowID) >= 8 && w.Label == "Window "+w.WindowID[len(w.WindowID)-8:] {
			continue
		}
		h.ui.mu.Lock()
		if h.ui.renaming == nil {
			h.ui.renaming = map[string]pendingRename{}
		}
		h.ui.renaming[w.WindowID] = pendingRename{label: w.Label, at: time.Now()}
		h.ui.mu.Unlock()
		err := h.callPlugin(ctx, v.plugin, "view.rename", map[string]any{"route": v.route, "user": viewUser, "title": w.Label}, nil)
		h.ev.emit("tab_rename", map[string]any{"plugin": v.plugin, "route": v.route, "window_id": w.WindowID, "title": w.Label, "ok": err == nil})
		if err != nil {
			// Refused (not a thread, blank) or no handler: the next UI pass restores the declared title.
			log.Printf("tab rename %s %s: %v", v.plugin, v.route, err)
		}
		h.ui.mu.Lock()
		if h.ui.renaming[w.WindowID].label == w.Label {
			delete(h.ui.renaming, w.WindowID) // only the rename that set the hold releases it
		}
		h.ui.mu.Unlock()
		h.kickUI("")
	}
}

type pendingRename struct {
	label string
	at    time.Time
}
