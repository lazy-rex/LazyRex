package main

// The restart wrapper's log (docs/hub.md, "Restart wrapper", "Log"). Under -restart the wrapper is
// the only writer of <state>/hub.log:
//   - One reader for each run moves the hub's output (stdout and stderr are one pipe) into a byte
//     queue of fixed capacity in chunks of at most 32 KiB. A chunk that does not fit is dropped and
//     counted. The reader never waits for the disk or the pane.
//   - One writer moves chunks from the queue to hub.log, rotated at 16 MiB to hub.log.1 to .3.
//     A failed open, rotation or write puts the writer in drop mode: no file grows, chunks are
//     counted as dropped, and the writer tries again every 60 s. When it writes again, it first
//     writes "dropped N bytes" (best effort: a disk that stays stalled gets no such line).
//   - Pane lines (the wrapper's own lines, drop notices) go through a bounded channel to one pane
//     goroutine. A full channel drops the line.
//   - At open, a hub.log at the size is rotated first.
//   - The reader also keeps the last 2 KiB of its run. A hub that fails before its first ready
//     gets its last lines in the pane (lastLines), so a refusal is readable there.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	logChunk    = 32 << 10
	logQueueMax = 1 << 20
	logFileMax  = 16 << 20
	logOldFiles = 3
	logRetry    = 60 * time.Second
	logEndWait  = 2 * time.Second // the writer's and the pane's last flush at the wrapper's exit
	paneLines   = 64
	tailMax     = 2 << 10 // the end of a run that lastLines reads
)

type restartLog struct {
	path    string // <state>/hub.log
	fileMax int64
	queMax  int
	now     func() time.Time
	retry   time.Duration
	// writeFile writes to the open log; a test replaces it (a stalled or failing disk).
	writeFile func(f *os.File, b []byte) (int, error)

	mu        sync.Mutex
	queue     [][]byte
	queued    int
	dropped   int64
	dropSince time.Time
	noticed   bool   // the pane has the notice of the current drop
	tail      []byte // the end of the current run's output (tailMax)
	tailCut   bool   // tail lost its start: its first line is incomplete
	closed    bool
	busy      bool // the writer writes chunks that it took from the queue
	kick      chan struct{}

	paneCh   chan string
	paneStop chan struct{} // closed by close; paneCh is never closed, so a late line cannot panic
	paneDone chan struct{}
	done     chan struct{}

	// The writer's own state: only the writer goroutine uses it after start.
	f       *os.File
	size    int64
	dropMod bool
	retryAt time.Time
}

func newRestartLog(state string, pane io.Writer) *restartLog {
	l := &restartLog{path: filepath.Join(state, "hub.log"), fileMax: logFileMax, queMax: logQueueMax,
		now: time.Now, retry: logRetry, writeFile: func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		kick: make(chan struct{}, 1), paneCh: make(chan string, paneLines), paneStop: make(chan struct{}), paneDone: make(chan struct{}),
		done: make(chan struct{})}
	go func() {
		defer close(l.paneDone)
		for {
			select {
			case s := <-l.paneCh:
				io.WriteString(pane, s)
			case <-l.paneStop:
				for {
					select {
					case s := <-l.paneCh:
						io.WriteString(pane, s)
					default:
						return
					}
				}
			}
		}
	}()
	return l
}

// paneLine sends a line to the pane without waiting; a full channel drops it.
func (l *restartLog) paneLine(s string) {
	select {
	case l.paneCh <- s:
	default:
	}
}

// Line is a wrapper line: to the pane and into the queue.
func (l *restartLog) Write(p []byte) (int, error) {
	l.paneLine(string(p))
	l.push(p)
	return len(p), nil
}

// push copies b into the queue, or counts it as dropped. It never waits.
func (l *restartLog) push(b []byte) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	if l.queued+len(b) > l.queMax {
		l.dropLocked(int64(len(b)))
		l.mu.Unlock()
		return
	}
	l.queue = append(l.queue, append([]byte(nil), b...))
	l.queued += len(b)
	l.mu.Unlock()
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// dropLocked counts dropped bytes and sends one pane notice for each drop period.
func (l *restartLog) dropLocked(n int64) {
	if l.dropped == 0 {
		l.dropSince = l.now()
	}
	l.dropped += n
	if !l.noticed {
		l.noticed = true
		l.paneLine(fmt.Sprintf("restart: hub output is dropped (the log queue is full, or the log is in drop mode): %s\n", l.path))
	}
}

// readRun moves one run's output into the queue until EOF or a closed read end.
func (l *restartLog) readRun(r io.Reader) {
	l.mu.Lock()
	l.tail, l.tailCut = l.tail[:0], false
	l.mu.Unlock()
	buf := make([]byte, logChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			l.push(buf[:n])
			l.mu.Lock()
			l.tail = append(l.tail, buf[:n]...)
			if len(l.tail) > tailMax {
				l.tail = append(l.tail[:0], l.tail[len(l.tail)-tailMax:]...)
				l.tailCut = true
			}
			l.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// lastLines is at most n of the last complete lines of the last run's output, for the pane. The
// first line of a cut tail is incomplete and left out; an empty line is left out. Each control
// character except tab becomes "?" (C0, DEL, C1, and a byte that is not UTF-8, which a terminal can
// read as C1): the output can hold text from other sources, and the pane is the user's terminal.
func (l *restartLog) lastLines(n int) []string {
	l.mu.Lock()
	s := string(l.tail)
	cut := l.tailCut
	l.mu.Unlock()
	lines := strings.Split(s, "\n")
	if cut && len(lines) > 0 {
		lines = lines[1:]
	}
	var out []string
	for _, line := range lines {
		if line = strings.TrimRight(line, "\r"); line != "" {
			out = append(out, paneSafe(line))
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// paneSafe replaces each control character except tab with "?".
func paneSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\t' && (r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f || r == utf8.RuneError) {
			return '?'
		}
		return r
	}, s)
}

// start opens the log, then runs the writer.
func (l *restartLog) start() {
	l.open()
	go l.writer()
}

func (l *restartLog) files() []string {
	out := []string{l.path}
	for i := 1; i <= logOldFiles; i++ {
		out = append(out, fmt.Sprintf("%s.%d", l.path, i))
	}
	return out
}

// open opens hub.log for appending; a file at the size is rotated first. A failure enters drop mode.
func (l *restartLog) open() bool {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err == nil {
		var st os.FileInfo
		if st, err = f.Stat(); err == nil {
			l.f, l.size = f, st.Size()
			if l.size >= l.fileMax {
				return l.rotate()
			}
			return true
		}
		f.Close()
	}
	l.enterDrop(err)
	return false
}

// rotate closes the log, then renames .2 to .3, .1 to .2 and hub.log to .1, then opens a new log.
// No rename touches an open file.
func (l *restartLog) rotate() bool {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	files := l.files()
	for i := len(files) - 1; i > 0; i-- {
		if err := os.Rename(files[i-1], files[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			l.enterDrop(err)
			return false
		}
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		l.enterDrop(err)
		return false
	}
	l.f, l.size = f, 0
	return true
}

func (l *restartLog) enterDrop(err error) {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	if !l.dropMod {
		l.paneLine(fmt.Sprintf("restart: the log %s cannot be written (%v); output is dropped, next try in %s\n", l.path, err, l.retry))
	}
	l.dropMod = true
	l.retryAt = l.now().Add(l.retry)
}

// writer moves chunks to the file until the log is closed and empty.
func (l *restartLog) writer() {
	defer close(l.done)
	tick := time.NewTicker(time.Second) // the drop-mode retry needs a clock check without new output
	defer tick.Stop()
	for {
		l.mu.Lock()
		chunks, closed := l.queue, l.closed
		l.queue, l.queued, l.busy = nil, 0, len(chunks) > 0
		l.mu.Unlock()
		for _, c := range chunks {
			l.writeChunk(c)
		}
		l.mu.Lock()
		l.busy = false
		l.mu.Unlock()
		if closed && len(chunks) == 0 {
			if l.f != nil {
				l.f.Close()
			}
			return
		}
		if len(chunks) > 0 {
			continue
		}
		select {
		case <-l.kick:
		case <-tick.C:
			if l.dropMod && !l.now().Before(l.retryAt) {
				l.paneLine(fmt.Sprintf("restart: trying the log %s again\n", l.path))
				l.dropMod = false
				l.open()
			}
		}
	}
}

// writeChunk writes the drop line first when bytes were dropped, then the chunk.
func (l *restartLog) writeChunk(c []byte) {
	if l.dropMod {
		if l.now().Before(l.retryAt) {
			l.mu.Lock()
			l.dropLocked(int64(len(c)))
			l.mu.Unlock()
			return
		}
		l.paneLine(fmt.Sprintf("restart: trying the log %s again\n", l.path))
		l.dropMod = false
		if !l.open() {
			l.mu.Lock()
			l.dropLocked(int64(len(c)))
			l.mu.Unlock()
			return
		}
	}
	l.mu.Lock()
	n, since := l.dropped, l.dropSince
	l.mu.Unlock()
	if n > 0 {
		line := fmt.Sprintf("restart: dropped %d bytes of hub output since %s\n", n, since.Format(time.RFC3339))
		if _, ok := l.put([]byte(line)); !ok {
			l.mu.Lock()
			l.dropLocked(int64(len(c)))
			l.mu.Unlock()
			return
		}
		l.mu.Lock()
		l.dropped -= n
		if l.dropped == 0 {
			l.noticed = false
		}
		l.mu.Unlock()
	}
	if n, ok := l.put(c); !ok && n < len(c) {
		l.mu.Lock()
		l.dropLocked(int64(len(c) - n)) // only the bytes that did not reach the file
		l.mu.Unlock()
	}
}

// put writes b to the file, rotating first when b would pass the size. It returns the bytes that
// reached the file. A failed write, or a short write without an error, enters drop mode.
func (l *restartLog) put(b []byte) (int, bool) {
	if l.f == nil {
		return 0, false
	}
	if l.size+int64(len(b)) > l.fileMax && l.size > 0 && !l.rotate() {
		return 0, false
	}
	n, err := l.writeFile(l.f, b)
	l.size += int64(n)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		l.enterDrop(err)
		return n, false
	}
	return n, true
}

// close ends the log: the writer gets at most wait to write the queue, the pane the same.
// flush waits at most wait until the writer wrote every queued chunk and the pane took every
// queued line. The log stays open: a relaunch flushes before its exec (the exec closes the file),
// and after a failed exec the same writer goes on. It reports whether everything was written.
func (l *restartLog) flush(wait time.Duration) bool {
	select {
	case l.kick <- struct{}{}:
	default:
	}
	deadline := time.Now().Add(wait)
	for {
		l.mu.Lock()
		idle := len(l.queue) == 0 && !l.busy
		l.mu.Unlock()
		if idle && len(l.paneCh) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (l *restartLog) close(wait time.Duration) {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	select {
	case l.kick <- struct{}{}:
	default:
	}
	deadline := time.After(wait)
	select {
	case <-l.done:
	case <-deadline:
	}
	close(l.paneStop)
	select {
	case <-l.paneDone:
	case <-time.After(wait):
	}
}
