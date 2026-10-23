package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
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

// runTranscript prints a thread into the pane's scrollback until ctx ends, Ctrl+Q, or a
// reload. It returns reload=true when the hub asked it to execute itself again.
//
// The pane gets no echo and no mouse modes, so the wheel, selection and copy stay native.
// Ctrl+Q quits. Focus reports (DECSET 1004) go to the hub as read notices.
//
// The theme comes from the terminal (themeQuery) before the first print. A later scheme
// report with the other theme changes the palette of new messages.
func runTranscript(ctx context.Context, client *viewClient) (reload bool, err error) {
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
	dark := awaitTheme(themes, themeQueryTimeout)

	events := make(chan tea.Msg, 16)
	client.emit = func(msg tea.Msg) {
		select {
		case events <- msg:
		case <-ctx.Done():
		}
	}
	go client.run(ctx)

	p := newTranscriptPrinter(out, formatTranscript(dark))
	p.size = func() (int, int) {
		cols, rows, err := term.GetSize(os.Stdout.Fd())
		if err != nil {
			return 0, 0
		}
		return cols, rows
	}
	// The view tree holds the newest messages; full fetches the rest of the thread (64 KiB limit).
	full := newFullHistory(func(before string) (historyPage, error) { return client.history(ctx, before) })
	var raw Snapshot
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
			if err := p.apply(merge(raw)); err != nil {
				return false, err
			}
		case msg := <-events:
			switch msg := msg.(type) {
			case Snapshot:
				if err := p.apply(merge(msg)); err != nil {
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
			if t.dark == dark {
				continue
			}
			// The new palette applies to new messages only; the next rebuild (Older messages,
			// reload) prints all of them in it. A full rebuild of a long thread is not bounded
			// yet (coordinator, 2026-10-19).
			dark = t.dark
			p.format = formatTranscript(dark)
		case f := <-focus:
			client.focus(f, "focus")
		case k := <-keys:
			switch k {
			case 0x11, 0x03: // Ctrl+Q, Ctrl+C
				// In a pair, the user's close: the hub closes this pane and asks the composer to
				// close through its gate. The printer runs until its pane closes.
				if client.closeView() == nil {
					continue
				}
				return false, nil
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
