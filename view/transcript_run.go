package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

// crlfWriter turns "\n" into "\r\n". Raw mode turns off the terminal's output processing.
type crlfWriter struct{ w io.Writer }

// A short write of the converted bytes is an error; the caller cannot map it back.
func (c crlfWriter) Write(b []byte) (int, error) {
	converted := bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
	n, err := c.w.Write(converted)
	if err != nil {
		return 0, err
	}
	if n != len(converted) {
		return 0, io.ErrShortWrite
	}
	return len(b), nil
}

type transcriptOptions struct {
	style   transcriptStyle
	gallery bool           // s and S cycle the gallery styles instead of a, c, d
	client  *viewClient    // hub mode
	thread  *fixtureThread // fixture mode
}

// runTranscript prints a thread into the pane's scrollback until ctx ends, Ctrl+Q, or a
// reload. It returns reload=true when the hub asked it to execute itself again.
//
// The pane gets no echo and no mouse modes, so the wheel, selection and copy stay native.
// Keys: a, c, d switch the user-message style (a rebuild); o loads 50 older messages
// (fixture mode); Ctrl+Q quits. In gallery mode, s and S cycle the styles and t forces the
// other theme. Focus reports (DECSET 1004) go to the hub as read notices.
//
// The theme comes from the terminal (themeQuery) before the first print. A later scheme
// report with the other theme changes the palette of new messages; in the gallery it rebuilds.
func runTranscript(ctx context.Context, opts transcriptOptions) (reload bool, err error) {
	fd := os.Stdin.Fd()
	if state, err := term.MakeRaw(fd); err == nil {
		defer term.Restore(fd, state)
	}
	out := crlfWriter{os.Stdout}
	// Focus reports and bracketed paste: a paste arrives wrapped, so its letters are not keys.
	os.Stdout.WriteString("\x1b[?25l\x1b[?1004h\x1b[?2004h" + themeQuery)
	defer os.Stdout.WriteString("\x1b[?2031l\x1b[?2004l\x1b[?1004l\x1b[?25h")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	keys := make(chan byte, 16)
	focus := make(chan bool, 4)
	themes := make(chan themeReport, 4)
	go readTranscriptInput(ctx, os.Stdin, keys, focus, themes)
	dark, forced := awaitTheme(themes, themeQueryTimeout), false

	snapshots := make(chan Snapshot, 4)
	events := make(chan tea.Msg, 16)
	switch {
	case opts.thread != nil:
		go opts.thread.watch(ctx, snapshots)
	case opts.client != nil:
		opts.client.emit = func(msg tea.Msg) {
			select {
			case events <- msg:
			case <-ctx.Done():
			}
		}
		go opts.client.run(ctx)
	}

	entries, shown := galleryEntries(), 0
	if opts.style.gallery > 0 {
		shown = opts.style.gallery - 1 // the gallery opens on the selected style
	}
	format := func() func(Message, *Message) string {
		if opts.gallery {
			return entries[shown].format(dark)
		}
		return formatTranscript(opts.style, dark)
	}
	p := newTranscriptPrinter(out, format())
	if opts.gallery {
		p.banner = galleryBanner(shown, len(entries), entries[shown].name, dark, forced)
	}
	p.size = func() (int, int) {
		cols, rows, err := term.GetSize(os.Stdout.Fd())
		if err != nil {
			return 0, 0
		}
		return cols, rows
	}
	// The view tree holds the newest messages; full fetches the rest of the thread (64 KiB limit).
	var full *fullHistory
	if opts.client != nil {
		full = newFullHistory(func(before string) (historyPage, error) { return opts.client.history(ctx, before) })
	}
	var current, raw Snapshot
	have := false
	// retry fires while older history is missing (a failed fetch, for example during a plugin
	// restart): an unchanged thread sends no new snapshot that would fetch again.
	var retry <-chan time.Time
	merge := func(s Snapshot) Snapshot {
		raw, retry = s, nil
		s = full.merge(s)
		if full.gap {
			retry = time.After(2 * time.Second)
		}
		return s
	}
	for {
		select {
		case <-ctx.Done():
			return false, nil
		case <-retry:
			current = merge(raw)
			if err := p.apply(current); err != nil {
				return false, err
			}
		case s := <-snapshots:
			current, have = s, true
			if err := p.apply(s); err != nil {
				return false, err
			}
		case msg := <-events:
			switch msg := msg.(type) {
			case Snapshot:
				if full != nil {
					msg = merge(msg)
				}
				current, have = msg, true
				if err := p.apply(msg); err != nil {
					return false, err
				}
			case reloadRequested:
				return true, nil
			case retireRequested:
				return false, nil
			case replacementRequested:
				// A transcript is never a panel. Refuse a panel replacement.
				if cmd := msg.reply("refused"); cmd != nil {
					go cmd()
				}
			}
		case t := <-themes:
			if forced || t.dark == dark {
				continue
			}
			dark = t.dark
			if opts.gallery {
				p.banner = galleryBanner(shown, len(entries), entries[shown].name, dark, forced)
			}
			if !opts.gallery || !have {
				// Outside the gallery the new palette applies to new messages only; the next
				// rebuild (Older messages, reload) prints all of them in it. A full rebuild of a
				// long thread is not bounded yet (coordinator, 2026-10-19).
				p.format = format()
				continue
			}
			if err := p.restyle(current, format()); err != nil {
				return false, err
			}
		case f := <-focus:
			if opts.client != nil {
				opts.client.focus(f, "focus")
			}
		case k := <-keys:
			switch k {
			case 0x11, 0x03: // Ctrl+Q, Ctrl+C
				// In a pair, the user's close: the hub closes this pane and asks the composer to
				// close through its gate. The printer runs until its pane closes.
				if opts.client != nil && opts.client.closeView() == nil {
					continue
				}
				return false, nil
			case 's', 'S':
				if !opts.gallery || !have {
					continue
				}
				step := 1
				if k == 'S' {
					step = len(entries) - 1
				}
				shown = (shown + step) % len(entries)
				p.banner = galleryBanner(shown, len(entries), entries[shown].name, dark, forced)
				if err := p.restyle(current, format()); err != nil {
					return false, err
				}
			case 't':
				if !opts.gallery || !have {
					continue
				}
				dark, forced = !dark, true
				p.banner = galleryBanner(shown, len(entries), entries[shown].name, dark, forced)
				if err := p.restyle(current, format()); err != nil {
					return false, err
				}
			case 'a', 'c', 'd':
				if opts.gallery {
					continue
				}
				style, _ := parseTranscriptStyle(string(k))
				if style != opts.style && have {
					opts.style = style
					if err := p.restyle(current, formatTranscript(style, dark)); err != nil {
						return false, err
					}
				}
			case 'o':
				if opts.thread != nil {
					opts.thread.older()
				}
			}
		}
	}
}

// readTranscriptInput reports plain keys, focus reports (ESC [ I, ESC [ O) and theme reports
// (OSC 11 answers, CSI ? 997 ; Ps n). It consumes every other escape sequence whole, so its
// payload never reaches the keys: CSI (ESC [ ... final), SS3 (ESC O x), OSC, DCS, APC and PM
// (to BEL or ESC \), and a bracketed paste (ESC [ 200 ~ ... ESC [ 201 ~).
func readTranscriptInput(ctx context.Context, in io.Reader, keys chan<- byte, focus chan<- bool, theme chan<- themeReport) {
	r := bufio.NewReader(in)
	for ctx.Err() == nil {
		b, err := r.ReadByte()
		if err != nil {
			return
		}
		if b != 0x1b {
			keys <- b
			continue
		}
		kind, err := r.ReadByte()
		if err != nil {
			return
		}
		switch kind {
		case '[':
			params, final, err := readCSI(r)
			if err != nil {
				return
			}
			switch {
			case final == 'I' && params == "":
				focus <- true
			case final == 'O' && params == "":
				focus <- false
			case final == '~' && params == "200":
				if skipPaste(r) != nil {
					return
				}
			case final == 'n':
				if t, ok := parseSchemeReport(params); ok {
					theme <- t
				}
			}
		case 'O':
			if _, err := r.ReadByte(); err != nil {
				return
			}
		case ']':
			payload, err := readString(r)
			if err != nil {
				return
			}
			if t, ok := parseOSC11(payload); ok {
				theme <- t
			}
		case 'P', '_', '^':
			if skipString(r) != nil {
				return
			}
		}
	}
}

func readCSI(r *bufio.Reader) (params string, final byte, err error) {
	var p []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", 0, err
		}
		if b >= 0x40 && b <= 0x7e {
			return string(p), b, nil
		}
		p = append(p, b)
	}
}

// readString consumes a control string up to BEL or ST (ESC \) and returns its first 256 bytes.
func readString(r *bufio.Reader) (string, error) {
	var s []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == 0x07 {
			return string(s), nil
		}
		if b == 0x1b {
			next, err := r.ReadByte()
			if err != nil {
				return "", err
			}
			if next == '\\' {
				return string(s), nil
			}
			continue
		}
		if len(s) < 256 {
			s = append(s, b)
		}
	}
}

// skipString consumes a control string up to BEL or ST (ESC \).
func skipString(r *bufio.Reader) error {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		if b == 0x07 {
			return nil
		}
		if b == 0x1b {
			if next, err := r.ReadByte(); err != nil || next == '\\' {
				return err
			}
		}
	}
}

// skipPaste consumes a bracketed paste up to ESC [ 201 ~.
func skipPaste(r *bufio.Reader) error {
	const end = "\x1b[201~"
	matched := 0
	for matched < len(end) {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch {
		case b == end[matched]:
			matched++
		case b == end[0]:
			matched = 1
		default:
			matched = 0
		}
	}
	return nil
}

// fixtureThread is a demo thread in a JSON Lines file of Message values. The transcript
// polls it; the demo composer appends sends to it; a timer appends agent messages. It
// produces the same snapshots the messaging plugin sends: the latest window, oldest first.
type fixtureThread struct {
	path   string
	every  time.Duration
	mu     sync.Mutex
	window int
	bump   chan struct{}
}

func newFixtureThread(path string, window int, every time.Duration) *fixtureThread {
	return &fixtureThread{path: path, window: window, every: every, bump: make(chan struct{}, 1)}
}

func (f *fixtureThread) older() {
	f.mu.Lock()
	f.window += 50
	f.mu.Unlock()
	select {
	case f.bump <- struct{}{}:
	default:
	}
}

func (f *fixtureThread) snapshot() (Snapshot, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return Snapshot{}, err
	}
	var all []Message
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m Message
		if json.Unmarshal([]byte(line), &m) == nil && m.ID != "" {
			all = append(all, m)
		}
	}
	f.mu.Lock()
	window := f.window
	f.mu.Unlock()
	first := max(0, len(all)-window)
	return Snapshot{SchemaVersion: 1, ViewID: "demo", Route: "/demo/thread", Revision: "1", Tree: Tree{Title: "Demo thread", Nodes: []Node{
		{ID: "history", Type: "history", Messages: all[first:], HasMore: first > 0, PageSize: max(1, window)},
	}}}, nil
}

// watch sends a snapshot when the file changes, and appends a timed agent message.
func (f *fixtureThread) watch(ctx context.Context, out chan<- Snapshot) {
	poll := time.NewTicker(150 * time.Millisecond)
	defer poll.Stop()
	var tick <-chan time.Time
	if f.every > 0 {
		t := time.NewTicker(f.every)
		defer t.Stop()
		tick = t.C
	}
	var size int64 = -1
	n := 0
	send := func() {
		if s, err := f.snapshot(); err == nil {
			select {
			case out <- s:
			case <-ctx.Done():
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.bump:
			send()
		case <-tick:
			n++
			_ = appendFixtureMessage(f.path, timedMessage(n))
		case <-poll.C:
			if info, err := os.Stat(f.path); err == nil && info.Size() != size {
				size = info.Size()
				send()
			}
		}
	}
}

// appendFixtureMessage appends one message as one write, so a reader never sees half a line.
func appendFixtureMessage(path string, m Message) error {
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(line, '\n'))
	return err
}

var timedBodies = []string{
	"Lab check done. The **reflow** probe passed at 100, 50 and 120 columns, and no message was printed twice.",
	"Next step:\n\n- run the printer against the fixture\n- resize the pane\n- select a few lines and copy them",
	"Here is the call:\n\n```go\nprinter.apply(snapshot) // one Write per snapshot\n```",
	"| Check | Result |\n| --- | --- |\n| wheel | native |\n| copy | native |",
	"A long line to show soft wrapping: the terminal wraps this paragraph at any width and joins it again when the pane grows wider, because the printer never inserts line breaks of its own.",
}

func timedMessage(n int) Message {
	author := []string{"codex", "claude"}[n%2]
	return Message{ID: fmt.Sprintf("timed-%d-%d", time.Now().UnixNano(), n), Author: author,
		Text: timedBodies[(n-1)%len(timedBodies)], At: time.Now().UTC().Format(time.RFC3339Nano)}
}
