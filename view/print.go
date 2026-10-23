package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/lazy-rex/LazyRex/view/transcript"
)

// The printed region of a thread view (v1.2 items 2 and 3). Thread mode prints each message of
// the current tree's history page once, above the live region, oldest first. The terminal owns the
// printed lines (its scrollback); the renderer never rewrites them.

// outputFailSaveTimeout bounds the draft saves after an output failure: one hub call per dirty
// draft, all under this one deadline, no retry. A timeout is an unknown outcome: the save carries
// its operation key, and the next view reads what the plugin committed.
const outputFailSaveTimeout = 5 * time.Second

// printer holds the ledger of printed message IDs. It belongs to the process and its pane, not to
// an attachment: a reattach or a new tree never clears it. One print runs at a time; later
// batches wait in model order.
type printer struct {
	printed, queued map[string]bool
	active          bool
	waiting         []printBatch
	last            *Message // the last printed message: the formatter puts a gap after it
	newest          string   // the current tree's newest message ID; "" when it has none
	out             interface{ Failed() bool }
	failed          bool // the output failed: no print, no commit, no seen
	themeTimer      bool // the theme timeout runs for the first print
}

type printBatch struct {
	messages []Message
	late     map[string]bool
}

// printDone comes from a print's Sequence after the write of its lines returned.
type printDone struct{ ids []string }

// outputFailed comes from the output watcher (main.go) after the first failed or short write.
type outputFailed struct{}

type draftSave struct {
	node string
	args map[string]any
}

// printTree reads the current tree in thread mode: it sets the newest ID and the seen gate, and
// queues the page's messages that are not printed or queued yet. A message is late when a printed
// message comes after it in the page; it is still printed at the bottom.
func (m *model) printTree() tea.Cmd {
	p := &m.print
	if m.mode != viewThread || p.failed {
		return nil
	}
	var history *Node
	for i := range m.snapshot.Tree.Nodes {
		if m.snapshot.Tree.Nodes[i].Type == "history" {
			history = &m.snapshot.Tree.Nodes[i]
			break
		}
	}
	if history == nil || len(history.Messages) == 0 {
		// No newest ID: no seen while this tree is current, also when a pending print completes.
		p.newest = ""
		m.client.blockSeen()
		return nil
	}
	if p.printed == nil {
		p.printed, p.queued = map[string]bool{}, map[string]bool{}
	}
	page := history.Messages
	p.newest = page[len(page)-1].ID
	if p.printed[p.newest] {
		m.client.allowSeen("", m.snapshot.gen)
	} else {
		m.client.blockSeen()
	}
	var batch printBatch
	printedAfter := false
	for i := len(page) - 1; i >= 0; i-- {
		msg := page[i]
		if p.printed[msg.ID] {
			printedAfter = true
			continue
		}
		if p.queued[msg.ID] {
			continue
		}
		p.queued[msg.ID] = true
		batch.messages = append(batch.messages, msg)
		if printedAfter {
			if batch.late == nil {
				batch.late = map[string]bool{}
			}
			batch.late[msg.ID] = true
		}
	}
	if len(batch.messages) == 0 {
		return nil
	}
	for i, j := 0, len(batch.messages)-1; i < j; i, j = i+1, j-1 {
		batch.messages[i], batch.messages[j] = batch.messages[j], batch.messages[i]
	}
	p.waiting = append(p.waiting, batch)
	return m.nextPrint()
}

// nextPrint starts the oldest waiting batch when no print runs and the theme is known. The text
// is formatted now, with the current theme. Before the theme reply, the first print waits for it
// or for the theme timeout.
func (m *model) nextPrint() tea.Cmd {
	p := &m.print
	if p.active || p.failed || len(p.waiting) == 0 {
		return nil
	}
	if !m.themeReady {
		if p.themeTimer {
			return nil
		}
		p.themeTimer = true
		return tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return formThemeTimeout{} })
	}
	b := p.waiting[0]
	p.waiting = p.waiting[1:]
	p.active = true
	var parts []string
	ids := make([]string, len(b.messages))
	for i, msg := range b.messages {
		ids[i] = msg.ID
		tm := transcriptMessage(msg)
		tm.Late = b.late[msg.ID]
		var previous *transcript.Message
		if p.last != nil {
			prev := transcriptMessage(*p.last)
			previous = &prev
		}
		parts = append(parts, transcript.Format(tm, previous, m.dark))
		last := msg
		p.last = &last
	}
	text := strings.TrimSuffix(strings.Join(parts, ""), "\n")
	return tea.Sequence(tea.Println(text), func() tea.Msg { return printDone{ids: ids} })
}

// printFinished commits a print's IDs at its printDone. It commits nothing after an output
// failure: the write may not have reached the pane. A print that holds the current tree's newest
// ID opens the seen gate.
func (m *model) printFinished(d printDone) tea.Cmd {
	p := &m.print
	p.active = false
	if p.failed || p.out != nil && p.out.Failed() {
		return nil
	}
	newest := false
	for _, id := range d.ids {
		delete(p.queued, id)
		p.printed[id] = true
		newest = newest || id == p.newest
	}
	if newest {
		m.client.allowSeen("content", m.snapshot.gen)
	}
	return m.nextPrint()
}

// failOutput handles an output failure inside the program: nothing more is printed, committed or
// sent as seen; the dirty drafts are saved under one bound; then the program ends (exit status 1,
// main.go).
func (m *model) failOutput() tea.Cmd {
	saves, first := m.beginFailure()
	if !first {
		return nil
	}
	client := m.client
	return func() tea.Msg {
		saveDrafts(client, saves)
		return tea.Quit()
	}
}

// finishFailure handles a failure that the output reports after the program stopped (a final
// flush). beginFailure owns the save, so it runs once whichever path sees the failure first.
func (m *model) finishFailure() {
	if saves, first := m.beginFailure(); first {
		saveDrafts(m.client, saves)
	}
}

func (m *model) beginFailure() ([]draftSave, bool) {
	p := &m.print
	if p.failed {
		return nil, false
	}
	p.failed = true
	p.waiting = nil
	m.client.closeSeen()
	return m.failureDrafts(), true
}

// failureDrafts lists the composer drafts whose current text is not saved, with new operation
// keys.
func (m *model) failureDrafts() []draftSave {
	var saves []draftSave
	for id, e := range m.editors {
		n := m.node(id)
		if e.Kind != "composer" || e.Conflict || n == nil || n.SaveAction == nil || !n.SaveAction.Enabled {
			continue
		}
		text := e.Area.Value() // the current local text, also while a send waits
		if text == e.Saved {
			continue
		}
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			continue
		}
		saves = append(saves, draftSave{node: id, args: map[string]any{"node_id": id, "operation_key": hex.EncodeToString(b), "base_revision": e.Revision, "text": text}})
	}
	sort.Slice(saves, func(i, j int) bool { return saves[i].node < saves[j].node })
	return saves
}

// saveDrafts makes one hub.view.draft.save call per draft, in order, all under one deadline, with
// no retry. A detached client fails each call at once. The result is not shown: the pane output
// failed, and stderr is the same pane.
func saveDrafts(client *viewClient, saves []draftSave) {
	if len(saves) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), outputFailSaveTimeout)
	defer cancel()
	for _, s := range saves {
		var out operationResult
		_ = client.call(ctx, "hub.view.draft.save", s.args, &out)
		if ctx.Err() != nil {
			return
		}
	}
}
