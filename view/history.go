package main

import (
	"context"
	"errors"
	"time"
)

// Clear the screen and the scrollback. ESC[2J alone keeps the scrollback in Rex, so a
// reprint after it duplicates history (spikes/04-scrollback, question 4).
const clearScrollback = "\x1b[H\x1b[2J\x1b[3J"

// paneStart is the process's first write: the clear, then the cursor hide (ESC[?25l). The view
// draws its own caret and parks the hidden cursor on the first live row (inlineView); paneOut
// drops every later show-cursor sequence. The pane closes with the process, so nothing shows the
// cursor again.
const paneStart = clearScrollback + "\x1b[?25l"

// historyNode returns the snapshot's history node. A loading snapshot has none.
func historyNode(s Snapshot) *Node {
	for i := range s.Tree.Nodes {
		if s.Tree.Nodes[i].Type == "history" {
			return &s.Tree.Nodes[i]
		}
	}
	return nil
}

// historyPage is one hub.view.history answer: messages older than a cursor, oldest first.
// Status "pending": the hub is still fetching the page; ask again.
type historyPage struct {
	Status   string    `json:"status"`
	Messages []Message `json:"messages"`
	HasMore  bool      `json:"has_more"`
}

// history fetches one page of older messages through the hub. The hub answers "pending" until
// the plugin has the page (a hub handler never waits for a plugin).
func (v *viewClient) history(ctx context.Context, before string) (historyPage, error) {
	deadline := time.Now().Add(20 * time.Second)
	for {
		var page historyPage
		if err := v.call(ctx, "hub.view.history", map[string]any{"before": before}, &page); err != nil {
			return page, err
		}
		if page.Status != "pending" {
			return page, nil
		}
		if time.Now().After(deadline) {
			return page, errors.New("the older history did not arrive")
		}
		select {
		case <-ctx.Done():
			return page, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
