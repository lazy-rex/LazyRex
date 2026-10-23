package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The history fetch of a thread view (v1.2 item 2). At open, the renderer pages back until it
// holds the newest 500 messages (full mode: every page) and prints them once, after a count line.
// After a gap (the current page holds no printed message), it pages back until a page holds a
// printed message and prints what is newer. The model owns all fetch state; a Cmd fetches one
// page and returns it with its sequence number and attachment generation.

const (
	openWindow      = 500
	pageRetryDelay  = 2 * time.Second
	loadingHistory  = "Loading history…"
	fullHistoryHint = "Thread Settings… > Show full history"
	fetchOpen       = "open"
	fetchGap        = "gap"
)

// fetch is one running history fetch: a contiguous run of messages, oldest first, that ends at the
// current page.
type fetch struct {
	kind   string
	seq    int
	gen    uint64
	ctx    context.Context // cancelled when the fetch stops: its call and its retry end
	cancel context.CancelFunc
	cursor string          // the oldest ID held: the next page ends before it
	run    []Message       // the first page, the fetched pages and the later current pages, oldest first
	held   map[string]bool // the IDs in run
	more   bool            // the last page's has_more
}

// historyPageMsg is one page of a fetch. A result whose seq or gen is not current is dropped.
type historyPageMsg struct {
	seq  int
	gen  uint64
	page historyPage
	err  error
}

// retryPage asks again for the cursor of fetch seq, after a failed page.
type retryPage struct{ seq int }

func fetchPage(ctx context.Context, pages func(context.Context, string) (historyPage, error), before string, seq int, gen uint64) tea.Cmd {
	return func() tea.Msg {
		page, err := pages(ctx, before)
		return historyPageMsg{seq: seq, gen: gen, page: page, err: err}
	}
}

func (m *model) historyNode() *Node {
	for i := range m.snapshot.Tree.Nodes {
		if m.snapshot.Tree.Nodes[i].Type == "history" {
			return &m.snapshot.Tree.Nodes[i]
		}
	}
	return nil
}

func (m *model) full() bool { return m.snapshot.Open != nil && m.snapshot.Open.Full }

// ending reports a close, reload or retirement that waits or runs. A failed close ends it: the
// view stays, and its next tree can fetch again.
func (m *model) ending() bool {
	return m.closePending || m.closeRunning || m.closeReady || m.reloadPending || m.reloadReady || m.retirePending || m.retireReady
}

// startFetch starts a fetch of kind back from the current page. An ending view starts none
// (codex-29): it prints nothing from this tree, and seen stays closed.
func (m *model) startFetch(kind string, page []Message) tea.Cmd {
	p := &m.print
	if m.ending() {
		m.updateSeen("")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.seq++
	f := &fetch{kind: kind, seq: p.seq, gen: m.snapshot.gen, ctx: ctx, cancel: cancel, cursor: page[0].ID, held: map[string]bool{}}
	m.extend(f, page)
	p.fetch = f
	m.updateSeen("")
	return fetchPage(f.ctx, p.pages, f.cursor, f.seq, f.gen)
}

// cancelFetch stops the running fetch: its call or its retry tick ends, and its result is dropped.
func (m *model) cancelFetch() {
	if f := m.print.fetch; f != nil {
		f.cancel()
		m.print.fetch = nil
	}
}

// followFetch takes a new current page during a fetch. A page that continues the run (its oldest
// ID is held) joins it; any other page, or a new attachment, restarts the fetch from that page.
func (m *model) followFetch(page []Message) bool {
	f := m.print.fetch
	if f.gen != m.snapshot.gen || !f.held[page[0].ID] {
		m.cancelFetch()
		return false
	}
	m.extend(f, page)
	return true
}

// extend takes a current page into the run. The page is the thread's order for its range: a new
// message can come inside it (codex-29). So the page replaces the run from the page's oldest ID
// on; a run without that ID (a new fetch) is the page.
func (m *model) extend(f *fetch, page []Message) {
	at := slices.IndexFunc(f.run, func(x Message) bool { return x.ID == page[0].ID })
	if at < 0 {
		at = len(f.run)
	}
	f.run = append(f.run[:at:at], page...)
	f.held = map[string]bool{}
	for _, x := range f.run {
		f.held[x.ID] = true
	}
	if f.kind == fetchOpen {
		for _, x := range page {
			m.print.openSeen[x.ID] = true
		}
	}
}

// pageDone takes one fetched page.
func (m *model) pageDone(msg historyPageMsg) tea.Cmd {
	p := &m.print
	f := p.fetch
	if f == nil || msg.seq != f.seq || msg.gen != m.client.currentGen() {
		return nil
	}
	page := msg.page.Messages
	fresh := 0
	for _, x := range page {
		if !f.held[x.ID] {
			fresh++
		}
	}
	// A page with has_more and nothing new, or the cursor again, is a failed page.
	if msg.err != nil || msg.page.HasMore && (fresh == 0 || page[0].ID == f.cursor) {
		seq, ctx := f.seq, f.ctx
		return func() tea.Msg {
			select {
			case <-time.After(pageRetryDelay):
				return retryPage{seq: seq}
			case <-ctx.Done(): // a stopped fetch ends its wait
				return nil
			}
		}
	}
	var older []Message
	for _, x := range page {
		if !f.held[x.ID] {
			f.held[x.ID] = true
			if f.kind == fetchOpen {
				p.openSeen[x.ID] = true
			}
			older = append(older, x)
		}
	}
	f.run = append(older, f.run...)
	f.more = msg.page.HasMore
	if len(page) > 0 {
		f.cursor = page[0].ID
	}
	done := !f.more
	if f.kind == fetchOpen && !m.full() {
		done = done || len(f.held) >= openWindow
	}
	if f.kind == fetchGap {
		for _, x := range page {
			done = done || p.printed[x.ID] || p.queued[x.ID]
		}
	}
	if !done {
		return fetchPage(f.ctx, p.pages, f.cursor, f.seq, f.gen)
	}
	return m.finishFetch()
}

func (m *model) retry(msg retryPage) tea.Cmd {
	f := m.print.fetch
	if f == nil || msg.seq != f.seq {
		return nil
	}
	return fetchPage(f.ctx, m.print.pages, f.cursor, f.seq, f.gen)
}

// finishFetch ends a fetch. Its run is contiguous and ends at the current page (followFetch).
func (m *model) finishFetch() tea.Cmd {
	f := m.print.fetch
	m.cancelFetch()
	var cmd tea.Cmd
	if f.kind == fetchOpen {
		cmd = m.finishOpen(f.run, f.more)
	} else {
		cmd = m.finishGap(f.run)
	}
	return tea.Batch(cmd, m.printTree())
}

// finishOpen ends the open: it keeps the newest 500 (full mode: all) as one batch after the count
// line, and every other ID that the open saw is excluded for the life of the process
// (lead-tv-excl). It runs once per process.
func (m *model) finishOpen(all []Message, more bool) tea.Cmd {
	p := &m.print
	p.opened = true
	keep := all
	if !m.full() && len(keep) > openWindow {
		keep, more = keep[len(keep)-openWindow:], true
	}
	kept := map[string]bool{}
	for _, x := range keep {
		kept[x.ID] = true
	}
	if !m.full() {
		for id := range p.openSeen {
			if !kept[id] {
				p.excluded[id] = true
			}
		}
	}
	p.openSeen = nil
	var head string
	if h := m.historyNode(); h != nil && !m.full() {
		switch {
		case h.Total != nil && *h.Total > len(keep):
			head = fmt.Sprintf("%d earlier messages. %s", *h.Total-len(keep), fullHistoryHint)
		case h.Total == nil && more:
			head = "Earlier messages exist. " + fullHistoryHint
		}
	}
	return m.queue(keep, nil, head)
}

// finishGap prints the messages after the last printed ID of the run, in page order, not late.
// A fetched message before that anchor is never printed by the fill, and an excluded ID is never
// an anchor. A run without a printed ID reached has_more = false: it is the whole thread, and the
// fill prints every message in it that is not printed or excluded, oldest first, not late
// (lead-tv-noanchor). Their IDs are then queued, so the same page does not start the fill again.
func (m *model) finishGap(all []Message) tea.Cmd {
	p := &m.print
	anchor := -1
	for i, x := range all {
		if p.printed[x.ID] || p.queued[x.ID] {
			anchor = i
		}
	}
	return m.queue(all[anchor+1:], nil, "")
}

// queue adds a batch of the messages that are not printed, queued or excluded.
func (m *model) queue(messages []Message, late map[string]bool, head string) tea.Cmd {
	p := &m.print
	batch := printBatch{late: late, head: head}
	for _, x := range messages {
		if p.printed[x.ID] || p.queued[x.ID] || p.excluded[x.ID] {
			continue
		}
		p.queued[x.ID] = true
		batch.messages = append(batch.messages, x)
	}
	if len(batch.messages) == 0 && head == "" {
		return nil
	}
	p.waiting = append(p.waiting, batch)
	return m.nextPrint()
}

// updateSeen is the one seen guard of thread mode: seen opens only when no fetch runs and the
// current tree's newest ID is printed (record item 3).
func (m *model) updateSeen(reason string) {
	p := &m.print
	if p.fetch != nil || p.newest == "" || !p.printed[p.newest] {
		m.client.blockSeen()
		return
	}
	m.client.allowSeen(reason, m.snapshot.gen)
}
