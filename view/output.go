package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

var errPaneOutput = errors.New("the pane output failed")

// showCursor is DECTCEM set. The view draws its own caret, so the pane never shows the terminal
// cursor (paneStart hides it; inlineView parks it on the first live row).
var showCursor = []byte("\x1b[?25h")

// paneOut is the renderer's output boundary (thread view v1.2 item 3). The first failed or short
// write latches it: every later write is dropped and fails. It never calls Program.Send: it runs
// onFault (the client closes its seen gate at once) and closes fault; a watcher goroutine wakes
// the program. With hideCursor, it drops each showCursor.
type paneOut struct {
	w          io.Writer
	mu         sync.Mutex
	failed     atomic.Bool
	fault      chan struct{}
	onFault    func()
	hideCursor bool
}

func newPaneOut(w io.Writer) *paneOut {
	return &paneOut{w: w, fault: make(chan struct{})}
}

func (o *paneOut) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed.Load() {
		return 0, errPaneOutput
	}
	out := b
	if o.hideCursor && bytes.Contains(b, showCursor) {
		out = bytes.ReplaceAll(b, showCursor, nil)
	}
	n, err := o.w.Write(out)
	if err == nil && n < len(out) {
		err = io.ErrShortWrite
	}
	if err != nil {
		o.failed.Store(true)
		if o.onFault != nil {
			o.onFault()
		}
		close(o.fault)
		return n, err
	}
	return len(b), nil
}

func (o *paneOut) WriteString(s string) (int, error) { return o.Write([]byte(s)) }

// Failed reports a latched failure. It is safe from any goroutine.
func (o *paneOut) Failed() bool { return o.failed.Load() }

// paneFile is the boundary over the pane's terminal file. It keeps the term.File interface (Read,
// Close and Fd), so Bubble Tea still finds the terminal.
type paneFile struct {
	*paneOut
	f *os.File
}

func newPaneFile(f *os.File) *paneFile { return &paneFile{paneOut: newPaneOut(f), f: f} }

func (p *paneFile) Read(b []byte) (int, error) { return p.f.Read(b) }
func (p *paneFile) Close() error               { return p.f.Close() }
func (p *paneFile) Fd() uintptr                { return p.f.Fd() }
