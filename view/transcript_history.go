package main

import (
	"context"
	"errors"
	"time"
)

// historyPage is one hub.view.history answer: messages older than a cursor, oldest first.
// Status "pending": the hub is still fetching the page; ask again.
type historyPage struct {
	Status   string    `json:"status"`
	Messages []Message `json:"messages"`
	HasMore  bool      `json:"has_more"`
}

// fullHistory keeps the whole thread for the transcript pane. A view tree holds only the newest
// messages that fit in one Rex control message (64 KiB); fullHistory fetches the older pages and
// gives the printer the complete thread. Messages never change after commit, so a message ID
// seen once stays valid.
type fullHistory struct {
	fetch    func(before string) (historyPage, error)
	messages []Message
	known    map[string]bool
	gap      bool // the last merge could not fetch the older pages
}

func newFullHistory(fetch func(before string) (historyPage, error)) *fullHistory {
	return &fullHistory{fetch: fetch, known: map[string]bool{}}
}

// merge returns s with its history node replaced by the whole thread.
//
//   - The tree's oldest message is known: append the new messages.
//   - The tree has no older messages: the tree is the whole thread.
//   - Otherwise (first snapshot, or a gap after a burst of messages): fetch every older page.
//
// A failed fetch returns s unchanged, so the printer shows the newest messages with the older
// marker; the next snapshot fetches again.
func (f *fullHistory) merge(s Snapshot) Snapshot {
	h := historyNode(s)
	if h == nil {
		return s
	}
	tail := h.Messages
	f.gap = false
	switch {
	case len(tail) == 0 || f.known[tail[0].ID]:
		for _, m := range tail {
			f.add(m)
		}
	case !h.HasMore:
		f.messages, f.known = nil, map[string]bool{}
		for _, m := range tail {
			f.add(m)
		}
	default:
		all, err := f.fetchOlder(tail)
		if err != nil {
			f.gap = true
			return s // stderr is the pane: the older marker reports the gap
		}
		f.messages, f.known = nil, map[string]bool{}
		for _, m := range all {
			f.add(m)
		}
	}
	out := s
	out.Tree.Nodes = append([]Node(nil), s.Tree.Nodes...)
	for i := range out.Tree.Nodes {
		if out.Tree.Nodes[i].Type == "history" {
			out.Tree.Nodes[i].Messages = f.messages
			out.Tree.Nodes[i].HasMore = false
		}
	}
	return out
}

func (f *fullHistory) add(m Message) {
	if !f.known[m.ID] {
		f.known[m.ID] = true
		f.messages = append(f.messages, m)
	}
}

// fetchOlder returns the thread from its first message through tail.
func (f *fullHistory) fetchOlder(tail []Message) ([]Message, error) {
	if f.fetch == nil {
		return nil, errors.New("no hub")
	}
	pages := [][]Message{tail}
	before := tail[0].ID
	for {
		page, err := f.fetch(before)
		if err != nil {
			return nil, err
		}
		if len(page.Messages) == 0 {
			break
		}
		pages = append(pages, page.Messages)
		if !page.HasMore {
			break
		}
		before = page.Messages[0].ID
	}
	var all []Message
	for i := len(pages) - 1; i >= 0; i-- {
		all = append(all, pages[i]...)
	}
	return all, nil
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
