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
//   - At open, the first file above 16 MiB is renamed once to hub.log.pre-restart if that name is
//     free. Normalization deletes no bytes; that archive is never written again.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	logChunk    = 32 << 10
	logQueueMax = 1 << 20
	logFileMax  = 16 << 20
	logOldFiles = 3
	logRetry    = 60 * time.Second
	logEndWait  = 2 * time.Second // the writer's and the pane's last flush at the wrapper's exit
	paneLines   = 64
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
	noticed   bool // the pane has the notice of the current drop
	closed    bool
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
	buf := make([]byte, logChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			l.push(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// start normalizes and opens the log, then runs the writer.
func (l *restartLog) start() {
	l.normalize()
	l.open()
	go l.writer()
}

// normalize renames the first file above the size to hub.log.pre-restart, once; any other file
// above the size gets a pane line. It deletes no bytes.
func (l *restartLog) normalize() {
	archive := l.path + ".pre-restart"
	_, err := os.Lstat(archive)
	free := errors.Is(err, fs.ErrNotExist)
	for _, p := range l.files() {
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || st.Size() <= l.fileMax {
			continue // only a regular file is renamed; a directory in the way is an open failure
		}
		if free {
			if err := os.Rename(p, archive); err == nil {
				free = false
				l.paneLine(fmt.Sprintf("restart: %s was %d bytes; renamed to %s, which is never written again\n", p, st.Size(), archive))
				continue
			} else {
				l.paneLine(fmt.Sprintf("restart: %s cannot be renamed: %v\n", p, err))
			}
		}
		l.paneLine(fmt.Sprintf("restart: %s is %d bytes, above the %d-byte file size: the size bound does not hold until it leaves through rotation, or until you delete it\n", p, st.Size(), l.fileMax))
	}
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
		l.queue, l.queued = nil, 0
		l.mu.Unlock()
		for _, c := range chunks {
			l.writeChunk(c)
		}
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
