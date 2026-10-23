package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Clear the screen and the scrollback. ESC[2J alone keeps the scrollback in Rex, so a
// reprint after it duplicates history (spikes/04-scrollback, question 4).
const clearScrollback = "\x1b[H\x1b[2J\x1b[3J"

// olderMarker is printed above the first message when the plugin has older messages.
const olderMarker = "\x1b[2m· Earlier messages are not shown. Use Older messages in the composer to load them. ·\x1b[0m\n\n"

// transcriptPrinter writes the committed messages of one history node to the pane's
// scrollback. It never rewrites a printed line. It keeps a ledger of printed message IDs,
// so a reconnect or a repeated snapshot prints only new messages.
//
// Each apply formats everything first and then makes one Write. A rebuild therefore
// never clears the pane before its replacement text is ready, and new output cannot
// interleave with a rebuild.
type transcriptPrinter struct {
	out     io.Writer
	format  func(m Message, previous *Message) string
	printed map[string]bool
	last    *Message // the last printed message
	started bool
	added   []string // IDs printed by the current apply, removed again if the write fails
	rebuilt bool     // the current apply replaced the ledger
	// size returns the pane's columns and rows, or zeros when unknown. See prescroll.
	size func() (cols, rows int)
}

func newTranscriptPrinter(out io.Writer, format func(Message, *Message) string) *transcriptPrinter {
	return &transcriptPrinter{out: out, format: format, printed: map[string]bool{}}
}

// historyNode returns the snapshot's history node. A loading snapshot has none.
func historyNode(s Snapshot) *Node {
	for i := range s.Tree.Nodes {
		if s.Tree.Nodes[i].Type == "history" {
			return &s.Tree.Nodes[i]
		}
	}
	return nil
}

// apply prints what a snapshot adds. The plugin sends the latest window of messages,
// oldest first. Message bodies never change after commit.
//
//   - First snapshot (process start or reload): clear, then print the window.
//   - Messages after the last printed one: append them.
//   - An unprinted message before the last printed one (Older messages expanded the
//     window): rebuild. Terminal output cannot be inserted above printed lines.
//   - The last printed message is missing (the window moved past it, so messages may be
//     missing between the two): rebuild. Never skip messages silently.
//   - No history node (loading, or another route): no change. A loading snapshot is not
//     an empty thread.
func (p *transcriptPrinter) apply(s Snapshot) error {
	printed, last, started := p.printed, p.last, p.started
	p.added, p.rebuilt = p.added[:0], false
	if err := p.applyHistory(s); err != nil {
		// Nothing, or an unknown part, reached the terminal. Keep the ledger of what was
		// printed before, so the next snapshot prints these messages again rather than
		// skipping them. A partial write can then duplicate lines; it cannot lose them.
		if p.rebuilt {
			p.printed = printed
		} else {
			for _, id := range p.added {
				delete(p.printed, id)
			}
		}
		p.last, p.started = last, started
		return err
	}
	return nil
}

func (p *transcriptPrinter) applyHistory(s Snapshot) error {
	h := historyNode(s)
	if h == nil {
		return nil
	}
	messages := h.Messages
	if !p.started {
		return p.rebuild(messages, h.HasMore)
	}
	at := -1
	if p.last != nil {
		at = -2 // not found
		for i := range messages {
			if messages[i].ID == p.last.ID {
				at = i
				break
			}
		}
	}
	if at == -2 {
		for _, m := range messages {
			if !p.printed[m.ID] {
				return p.rebuild(messages, h.HasMore)
			}
		}
		return nil // nothing new; the window only lost old messages
	}
	for _, m := range messages[:max(0, at)] {
		if !p.printed[m.ID] {
			return p.rebuild(messages, h.HasMore)
		}
	}
	var buf bytes.Buffer
	for i := at + 1; i < len(messages); i++ {
		if !p.printed[messages[i].ID] {
			p.print(&buf, messages[i])
		}
	}
	return p.write(buf.Bytes())
}

// rebuild clears the pane and reprints the window. The caller loses its native scroll
// position (Rex has no API to keep it).
func (p *transcriptPrinter) rebuild(messages []Message, hasMore bool) error {
	var buf bytes.Buffer
	buf.WriteString(clearScrollback)
	if hasMore {
		buf.WriteString(olderMarker)
	}
	p.printed, p.last, p.rebuilt = map[string]bool{}, nil, true
	for _, m := range messages {
		p.print(&buf, m)
	}
	p.started = true
	return p.write(buf.Bytes())
}

func (p *transcriptPrinter) print(buf *bytes.Buffer, m Message) {
	m.Text, m.Author = plain(m.Text), plain(m.Author)
	buf.WriteString(p.format(m, p.last))
	p.printed[m.ID] = true
	p.added = append(p.added, m.ID)
	p.last = &m
}

func (p *transcriptPrinter) write(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if p.size != nil {
		if cols, rows := p.size(); cols > 0 && rows > 0 {
			b = prescroll(b, cols, rows)
		}
	}
	n, err := p.out.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

// prescroll makes room before each line that has a background color and wraps.
//
// When a soft wrap scrolls the screen while a background is set, the terminal fills the
// new row with that background (background color erase). The filled cells are content:
// they stay after a resize, as a block past the end of the text. Observed in the
// Demo B lab (spikes/04-scrollback). So the printer first writes the line's extra rows
// as newlines with the default background, moves the cursor back up, and then writes the
// line. The line then wraps into existing rows and never scrolls. A line taller than the
// pane cannot be prepared this way and is written as is.
func prescroll(b []byte, cols, rows int) []byte {
	if !bytes.Contains(b, []byte("\x1b[48")) {
		return b
	}
	lines := strings.SplitAfter(string(b), "\n")
	var out strings.Builder
	for _, line := range lines {
		if strings.Contains(line, "\x1b[48") {
			// A rebuild's first line starts with the clear. Make room after the clear, not before.
			if rest, ok := strings.CutPrefix(line, clearScrollback); ok {
				out.WriteString(clearScrollback)
				line = rest
			}
			if extra := (ansi.StringWidth(strings.TrimSuffix(line, "\n")) - 1) / cols; extra > 0 && extra < rows {
				fmt.Fprintf(&out, "%s\x1b[%dA", strings.Repeat("\n", extra), extra)
			}
		}
		out.WriteString(line)
	}
	return []byte(out.String())
}
