package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

var _ term.File = (*paneFile)(nil) // Bubble Tea finds the terminal through the boundary

var errTestTerm = errors.New("test terminal failed")

// testTerm is the terminal under a paneOut in tests. failAt fails a write (an error), and shortAt
// takes half of it (n < len, no error).
type testTerm struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	failAt  func(b []byte) bool
	shortAt func(b []byte) bool
}

func newTestOut(failAt func([]byte) bool) (*paneOut, *testTerm) {
	t := &testTerm{failAt: failAt}
	return newPaneOut(t), t
}

func (t *testTerm) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failAt != nil && t.failAt(b) {
		return 0, errTestTerm
	}
	if t.shortAt != nil && t.shortAt(b) {
		return t.buf.Write(b[:len(b)/2])
	}
	return t.buf.Write(b)
}

func (t *testTerm) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

func (t *testTerm) set(f func(t *testTerm)) {
	t.mu.Lock()
	f(t)
	t.mu.Unlock()
}

// probeModel runs the production model and answers probes from Update, so a test reads the model
// without a data race. At each printDone it records whether the output already holds the text of
// the print's messages (the receipt). A held print: the commands of the Update that started a
// print wait until the test closes the gate (a write that blocks would block the event loop).
type probeModel struct {
	*model
	term     *testTerm
	texts    map[string]string
	receipts *[]bool
	hold     *printHold
}

type printHold struct {
	mu   sync.Mutex
	next chan struct{}
}

type probe struct{ reply chan probeState }

type probeState struct {
	printed  map[string]bool
	seenOK   bool
	gate     int
	failed   bool
	mode     viewMode
	waiting  int
	reasonOK bool
	fetching bool
	opened   bool
	excluded int
	notice   string
	composer string // the composer's text and draft revision
	revision string
}

func (p probeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case probe:
		s := probeState{printed: map[string]bool{}, failed: p.print.failed, mode: p.mode, waiting: len(p.print.waiting),
			fetching: p.print.fetch != nil, opened: p.print.opened, excluded: len(p.print.excluded), notice: p.notice()}
		for id := range p.print.printed {
			s.printed[id] = true
		}
		if e := p.editors["composer"]; e != nil {
			s.composer, s.revision = e.text(), e.Revision
		}
		p.client.mu.Lock()
		s.seenOK, s.gate = p.client.seenOK, p.client.seenGate
		p.client.mu.Unlock()
		msg.reply <- s
		return p, nil
	case printDone:
		written := true
		for _, id := range msg.ids {
			written = written && strings.Contains(p.term.String(), p.texts[id])
		}
		*p.receipts = append(*p.receipts, written)
	}
	active := p.print.active
	_, cmd := p.model.Update(msg)
	if !active && p.print.active && cmd != nil {
		p.hold.mu.Lock()
		gate := p.hold.next
		p.hold.next = nil
		p.hold.mu.Unlock()
		if gate != nil {
			inner := cmd
			cmd = func() tea.Msg { <-gate; return inner() }
		}
	}
	return p, cmd
}

// printRun is one renderer on a test terminal, with the output watcher of main.go.
type printRun struct {
	t        *testing.T
	p        *tea.Program
	m        *model
	out      *paneOut
	term     *testTerm
	receipts []bool
	done     chan struct{}
	err      error
	hold     *printHold
}

// holdNextPrint holds the next print that starts until the returned gate is closed.
func (r *printRun) holdNextPrint() chan struct{} {
	gate := make(chan struct{})
	r.hold.mu.Lock()
	r.hold.next = gate
	r.hold.mu.Unlock()
	return gate
}

func startPrint(t *testing.T, term *testTerm) *printRun {
	t.Helper()
	return startPrintWith(t, term, nil)
}

// startPrintWith is startPrint with a setup of the model before the program starts.
func startPrintWith(t *testing.T, term *testTerm, setup func(*model)) *printRun {
	t.Helper()
	return startPrintInput(t, term, setup, nil)
}

// startPrintInput is startPrintWith with a program input: Bubble Tea decodes its bytes as a
// terminal's input (keys_test.go).
func startPrintInput(t *testing.T, term *testTerm, setup func(*model), input io.Reader) *printRun {
	t.Helper()
	out := newPaneOut(term)
	client := &viewClient{ViewID: "v1", Plugin: "messaging", Route: "/thread/42", Requests: make(chan mutation, 16), seen: make(chan struct{}, 1)}
	client.outFailed = out.Failed
	out.onFault = client.closeSeen
	m := newModel(client)
	m.ready = true
	m.print.out = out
	if setup != nil {
		setup(m)
	}
	r := &printRun{t: t, m: m, out: out, term: term, done: make(chan struct{}), hold: &printHold{}}
	pm := probeModel{model: m, term: term, texts: map[string]string{}, receipts: &r.receipts, hold: r.hold}
	for i := 1; i <= 9; i++ {
		pm.texts[fmt.Sprintf("m%d", i)] = fmt.Sprintf("TEXT-%d", i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	r.p = tea.NewProgram(pm, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(out), tea.WithWindowSize(80, 20), tea.WithoutSignals())
	go func() {
		select {
		case <-out.fault:
			r.p.Send(outputFailed{})
		case <-ctx.Done():
		}
	}()
	go func() {
		_, r.err = r.p.Run()
		close(r.done)
	}()
	r.p.Send(tea.BackgroundColorMsg{}) // the theme is known: prints start at once
	return r
}

// wait polls the model through probes until ok holds.
func (r *printRun) wait(what string, ok func(probeState) bool) probeState {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		reply := make(chan probeState, 1)
		r.p.Send(probe{reply})
		select {
		case s := <-reply:
			if ok(s) {
				return s
			}
		case <-r.done:
			r.t.Fatalf("%s: the program ended", what)
		case <-time.After(time.Second):
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s: timed out; output %q", what, r.term.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *printRun) stop() {
	r.t.Helper()
	r.p.Quit()
	r.ended()
}

func (r *printRun) ended() {
	r.t.Helper()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		r.t.Fatal("the program did not end")
	}
}

func msgs(ids ...int) []Message {
	var out []Message
	for _, i := range ids {
		out = append(out, Message{ID: fmt.Sprintf("m%d", i), Author: "codex", Text: fmt.Sprintf("TEXT-%d", i)})
	}
	return out
}

func threadTree(messages []Message) Snapshot {
	return treeOf(Node{ID: "history", Type: "history", PageSize: 500, Messages: messages}, tvComposer)
}

func printedAll(ids ...int) func(probeState) bool {
	return func(s probeState) bool {
		for _, i := range ids {
			if !s.printed[fmt.Sprintf("m%d", i)] {
				return false
			}
		}
		return true
	}
}

// once checks that each text is in the output exactly once, in this order.
func once(t *testing.T, out string, texts ...string) {
	t.Helper()
	at := -1
	for _, text := range texts {
		if n := strings.Count(out, text); n != 1 {
			t.Fatalf("%q is in the output %d times: %q", text, n, out)
		}
		i := strings.Index(out, text)
		if i < at {
			t.Fatalf("%q is out of order: %q", text, out)
		}
		at = i
	}
}

// TestPrintReceiptCommitsAfterTheWrite: printDone comes after the write of the print's lines, and
// the ledger commits at printDone; the print of the newest ID opens the seen gate.
func TestPrintReceiptCommitsAfterTheWrite(t *testing.T) {
	r := startPrint(t, &testTerm{})
	r.p.Send(threadTree(msgs(1, 2)))
	s := r.wait("m1 and m2 printed", printedAll(1, 2))
	if !s.seenOK || s.gate != seenThread || s.mode != viewThread {
		t.Fatalf("after the print: %+v", s)
	}
	r.stop()
	if len(r.receipts) != 1 || !r.receipts[0] {
		t.Fatalf("receipts %v", r.receipts)
	}
	once(t, r.term.String(), "TEXT-1", "TEXT-2")
	noAlt(t, r.term.String())
}

// TestPrintBatchesRunInOrder: a second tree's batch waits behind the active print and prints after
// it (one print Sequence at a time).
func TestPrintBatchesRunInOrder(t *testing.T) {
	tt := &testTerm{}
	r := startPrint(t, tt)
	gate := r.holdNextPrint()
	r.p.Send(threadTree(msgs(1)))
	r.p.Send(threadTree(msgs(1, 2)))
	r.p.Send(threadTree(msgs(1, 2, 3)))
	r.wait("two batches wait", func(s probeState) bool { return s.waiting == 2 })
	close(gate)
	r.wait("all printed", printedAll(1, 2, 3))
	r.stop()
	once(t, r.term.String(), "TEXT-1", "TEXT-2", "TEXT-3")
}

// TestPrintLedgerSurvivesReattach: a reattach closes the gate until the first thread frame whose
// newest ID is printed; nothing is printed again.
func TestPrintLedgerSurvivesReattach(t *testing.T) {
	r := startPrint(t, &testTerm{})
	r.p.Send(threadTree(msgs(1, 2)))
	r.wait("printed", printedAll(1, 2))
	reattach(r.m.client, "B")
	r.wait("gate closed", func(s probeState) bool { return !s.seenOK })
	r.p.Send(stamp(r.m.client, threadTree(msgs(1, 2))))
	r.wait("the first frame of the new attachment opens the gate", func(s probeState) bool { return s.seenOK })
	r.stop()
	once(t, r.term.String(), "TEXT-1", "TEXT-2")
}

// TestPrintReceiptBeforeNewAttachTree (record item 3): a print queued for attachment A commits
// its IDs at printDone after the reattach to B, but it opens no gate: the current tree is A's. B's
// first thread frame, whose newest ID is printed, opens it.
func TestPrintReceiptBeforeNewAttachTree(t *testing.T) {
	tt := &testTerm{}
	r := startPrint(t, tt)
	gate := r.holdNextPrint()
	r.p.Send(threadTree(msgs(1)))
	r.wait("queued", func(s probeState) bool { return !s.printed["m1"] && !s.seenOK })
	reattach(r.m.client, "B")
	close(gate)
	s := r.wait("committed", printedAll(1))
	if s.seenOK {
		t.Fatal("A's print receipt opened the gate before B's first tree")
	}
	r.p.Send(stamp(r.m.client, threadTree(msgs(1))))
	r.wait("B's first thread frame opens the gate", func(s probeState) bool { return s.seenOK })
	r.stop()
	once(t, tt.String(), "TEXT-1")
}

// TestPrintReceiptAfterNewAttachTree: B's frame (newest m1) comes before the receipt of A's print.
// m1 is queued, so it is not printed again; the receipt opens the gate, because the current tree
// is B's and holds m1.
func TestPrintReceiptAfterNewAttachTree(t *testing.T) {
	tt := &testTerm{}
	r := startPrint(t, tt)
	gate := r.holdNextPrint()
	r.p.Send(threadTree(msgs(1)))
	r.wait("queued", func(s probeState) bool { return !s.printed["m1"] && !s.seenOK })
	reattach(r.m.client, "B")
	r.p.Send(stamp(r.m.client, threadTree(msgs(1))))
	r.wait("B's frame before the receipt", func(s probeState) bool { return !s.printed["m1"] && !s.seenOK && s.waiting == 0 })
	close(gate)
	s := r.wait("committed", printedAll(1))
	if !s.seenOK {
		t.Fatal("the print of the newest ID did not open the gate")
	}
	r.stop()
	once(t, tt.String(), "TEXT-1")
}

// TestPrintWaitingBatchSurvivesReattach: m1 prints behind a gate and m2 waits when the view
// reattaches. The new attachment's frame queues nothing again; m2 prints after m1, once, and its
// receipt opens the gate.
func TestPrintWaitingBatchSurvivesReattach(t *testing.T) {
	tt := &testTerm{}
	r := startPrint(t, tt)
	gate := r.holdNextPrint()
	r.p.Send(threadTree(msgs(1)))
	r.p.Send(threadTree(msgs(1, 2)))
	r.wait("m2 waits", func(s probeState) bool { return s.waiting == 1 })
	reattach(r.m.client, "B")
	r.p.Send(stamp(r.m.client, threadTree(msgs(1, 2))))
	r.wait("B's frame", func(s probeState) bool { return s.waiting == 1 && !s.seenOK })
	close(gate)
	s := r.wait("all printed", printedAll(1, 2))
	if !s.seenOK {
		t.Fatal("the print of the newest ID did not open the gate")
	}
	r.stop()
	once(t, tt.String(), "TEXT-1", "TEXT-2")
	if strings.Index(tt.String(), "TEXT-1") > strings.Index(tt.String(), "TEXT-2") {
		t.Fatal("m2 printed before m1")
	}
}

// TestPrintPendingThenEmptyHistory: an empty page comes while the print of m1 is pending. The
// current tree has no newest ID, so m1's receipt commits m1 and opens no gate.
func TestPrintPendingThenEmptyHistory(t *testing.T) {
	tt := &testTerm{}
	r := startPrint(t, tt)
	gate := r.holdNextPrint()
	r.p.Send(threadTree(msgs(1)))
	r.wait("queued", func(s probeState) bool { return !s.printed["m1"] })
	r.p.Send(threadTree(nil))
	r.wait("the empty page", func(s probeState) bool { return !s.printed["m1"] && !s.seenOK })
	close(gate)
	s := r.wait("committed", printedAll(1))
	if s.seenOK {
		t.Fatal("m1's receipt opened the gate while the current page is empty")
	}
	r.stop()
	once(t, tt.String(), "TEXT-1")
}

// TestPrintThenErrorTreeSendsNoSeen (lead-tv-seencur): a print finishes while an error tree is
// current: its IDs commit, the gate stays closed; the next thread tree opens it.
func TestPrintThenErrorTreeSendsNoSeen(t *testing.T) {
	tt := &testTerm{}
	r := startPrint(t, tt)
	gate := r.holdNextPrint()
	r.p.Send(threadTree(msgs(1)))
	r.wait("queued", func(s probeState) bool { return s.mode == viewThread })
	r.p.Send(treeOf(tvError("e", "ERROR-TEXT", false)))
	close(gate)
	s := r.wait("committed", printedAll(1))
	if s.seenOK {
		t.Fatal("seen opened while an error tree is current")
	}
	r.p.Send(threadTree(msgs(1)))
	r.wait("the next thread tree opens the gate", func(s probeState) bool { return s.seenOK })
	r.stop()
	once(t, r.term.String(), "TEXT-1")
	noAlt(t, r.term.String())
}

// TestPrintEmptyHistory: an empty page has no newest ID: nothing prints and the gate stays closed;
// after a print, an empty page closes the gate again.
func TestPrintEmptyHistory(t *testing.T) {
	r := startPrint(t, &testTerm{})
	r.p.Send(threadTree(nil))
	s := r.wait("thread mode", func(s probeState) bool { return s.mode == viewThread })
	if s.seenOK || len(s.printed) != 0 {
		t.Fatalf("an empty first page: %+v", s)
	}
	r.p.Send(threadTree(msgs(1)))
	r.wait("printed", func(s probeState) bool { return s.printed["m1"] && s.seenOK })
	r.p.Send(threadTree(nil))
	r.wait("an empty page closes the gate", func(s probeState) bool { return !s.seenOK })
	r.stop()
}

// TestPrintLateMessage (v1.2 item 2): a message that arrives before a printed one in the page is
// printed at the bottom, with "(late)" after its author.
func TestPrintLateMessage(t *testing.T) {
	r := startPrint(t, &testTerm{})
	r.p.Send(threadTree(msgs(1, 3)))
	r.wait("printed", printedAll(1, 3))
	r.p.Send(threadTree(msgs(1, 2, 3)))
	r.wait("the late message", printedAll(2))
	r.stop()
	out := r.term.String()
	once(t, out, "TEXT-1", "TEXT-3", "TEXT-2")
	late := strings.Index(out, "(late)")
	if late < 0 || late < strings.Index(out, "TEXT-3") || late > strings.Index(out, "TEXT-2") {
		t.Fatalf("no (late) in the header of m2: %q", out)
	}
	if strings.Count(out, "(late)") != 1 {
		t.Fatalf("(late) on another message: %q", out)
	}
}

// TestPrintModes (v1.2 item 1 tests i, ii, ii-b, iii, v) on raw bytes.
func TestPrintModes(t *testing.T) {
	t.Run("i: an error tree, then history", func(t *testing.T) {
		r := startPrint(t, &testTerm{})
		r.p.Send(treeOf(tvError("e", "ERROR-TEXT", true)))
		r.p.Send(threadTree(msgs(1, 2)))
		r.wait("printed", printedAll(1, 2))
		r.stop()
		noAlt(t, r.term.String())
		once(t, r.term.String(), "TEXT-1", "TEXT-2")
	})
	t.Run("ii: thread, an error tree, thread with a new message", func(t *testing.T) {
		r := startPrint(t, &testTerm{})
		r.p.Send(threadTree(msgs(1)))
		r.wait("printed", printedAll(1))
		r.p.Send(treeOf(tvError("e", "ERROR-TEXT", false)))
		r.p.Send(threadTree(msgs(1, 2)))
		r.wait("printed", printedAll(2))
		r.stop()
		out := r.term.String()
		noAlt(t, out)
		once(t, out, "TEXT-1", "TEXT-2")
		if strings.Index(out, "ERROR-TEXT") > strings.Index(out, "TEXT-2") {
			t.Fatalf("m2 printed before the error frame: %q", out)
		}
	})
	t.Run("ii-b: a print pending when the error tree arrives", func(t *testing.T) {
		tt := &testTerm{}
		r := startPrint(t, tt)
		gate := r.holdNextPrint()
		r.p.Send(threadTree(msgs(1)))
		r.wait("thread", func(s probeState) bool { return s.mode == viewThread })
		r.p.Send(treeOf(tvError("e", "ERROR-TEXT", false)))
		close(gate)
		r.wait("printed", printedAll(1))
		r.stop()
		noAlt(t, r.term.String())
		once(t, r.term.String(), "TEXT-1")
	})
	t.Run("iii: a list tree, then history", func(t *testing.T) {
		r := startPrint(t, &testTerm{})
		r.p.Send(treeOf(tvList))
		r.p.Send(threadTree(msgs(1)))
		r.p.Send(threadTree(msgs(1, 2)))
		r.wait("alt", func(s probeState) bool { return s.mode == viewAlt })
		time.Sleep(100 * time.Millisecond) // the history trees are applied
		s := r.wait("alt", func(probeState) bool { return true })
		r.stop()
		// The alt screen draws the history as before; nothing is printed or kept in the ledger.
		if len(s.printed) != 0 || r.m.print.active || len(r.m.print.waiting) != 0 {
			t.Fatalf("alt mode printed: %+v", s)
		}
		if !strings.Contains(r.term.String(), altOn) {
			t.Fatalf("no alt screen: %q", r.term.String())
		}
	})
}

const (
	altOn  = "\x1b[?1049h"
	altOff = "\x1b[?1049l"
)

func noAlt(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, altOn) || strings.Contains(out, altOff) {
		t.Fatalf("alt screen bytes in thread mode: %q", out)
	}
}

// failRun checks the end of a run after an output failure: the program ended, nothing more reached
// the terminal, and the gate is closed for good.
func failRun(t *testing.T, r *printRun) {
	t.Helper()
	r.ended()
	if !r.out.Failed() || !r.m.print.failed {
		t.Fatalf("out failed %t, model failed %t", r.out.Failed(), r.m.print.failed)
	}
	r.m.client.mu.Lock()
	gate, ok := r.m.client.seenGate, r.m.client.seenOK
	r.m.client.mu.Unlock()
	if gate != seenClosed || ok {
		t.Fatalf("the seen gate after the failure: %d %t", gate, ok)
	}
	if n, _ := r.out.Write([]byte("x")); n != 0 {
		t.Fatal("the boundary forwards bytes after the failure")
	}
}

// TestPrintWriteFailureIsFatal: the write of a print fails: no ledger entry, no seen, the program
// ends.
func TestPrintWriteFailureIsFatal(t *testing.T) {
	tt := &testTerm{failAt: func(b []byte) bool { return bytes.Contains(b, []byte("TEXT-2")) }}
	r := startPrint(t, tt)
	r.p.Send(threadTree(msgs(1)))
	r.wait("m1", printedAll(1))
	r.p.Send(threadTree(msgs(1, 2)))
	failRun(t, r)
	if r.m.print.printed["m2"] {
		t.Fatal("m2 committed after a failed write")
	}
	if strings.Contains(tt.String(), "TEXT-2") {
		t.Fatal("m2 reached the terminal")
	}
}

// TestPrintShortWriteIsFatal: a short write (n < len, no error) is a failure too.
func TestPrintShortWriteIsFatal(t *testing.T) {
	tt := &testTerm{shortAt: func(b []byte) bool { return bytes.Contains(b, []byte("TEXT-1")) }}
	r := startPrint(t, tt)
	r.p.Send(threadTree(msgs(1)))
	failRun(t, r)
	if r.m.print.printed["m1"] {
		t.Fatal("m1 committed after a short write")
	}
}

// TestIdleFrameFailureEndsTheProgram: a frame write fails while no print runs; the watcher wakes
// the program.
func TestIdleFrameFailureEndsTheProgram(t *testing.T) {
	tt := &testTerm{}
	r := startPrint(t, tt)
	r.p.Send(threadTree(msgs(1)))
	r.wait("printed", printedAll(1))
	tt.set(func(t *testTerm) { t.failAt = func([]byte) bool { return true } })
	r.p.Send(tea.WindowSizeMsg{Width: 70, Height: 20}) // a new frame
	failRun(t, r)
}

// TestFailureDropsWaitingBatches: batches that wait behind a failed print never start.
func TestFailureDropsWaitingBatches(t *testing.T) {
	tt := &testTerm{failAt: func(b []byte) bool { return bytes.Contains(b, []byte("TEXT-1")) }}
	r := startPrint(t, tt)
	gate := r.holdNextPrint()
	r.p.Send(threadTree(msgs(1)))
	r.p.Send(threadTree(msgs(1, 2)))
	r.wait("a batch waits", func(s probeState) bool { return s.waiting == 1 })
	close(gate)
	failRun(t, r)
	if strings.Contains(tt.String(), "TEXT-2") || len(r.m.print.printed) != 0 {
		t.Fatalf("a waiting batch ran: %v %q", r.m.print.printed, tt.String())
	}
}

// TestFinalFlushFailure: the output fails only in the final flush after Quit. afterRun finds it,
// runs the draft save once and returns 1, and writes no status clear.
func TestFinalFlushFailure(t *testing.T) {
	f := &protocolFixture{}
	client, _ := startFixture(t, f)
	waitAttached(t, f)
	tt := &testTerm{}
	out := newPaneOut(tt)
	client.mu.Lock() // client.run reads it
	client.outFailed = out.Failed
	client.mu.Unlock()
	out.onFault = client.closeSeen
	m := newModel(client)
	m.ready = true
	m.print.out = out
	m.apply(threadTree(msgs(1)))
	m.editors["composer"].Area.SetValue("UNSAVED")
	m.programStatus = "\x1b]7501;busy\x07"
	tt.set(func(t *testTerm) { t.failAt = func([]byte) bool { return true } })
	out.Write([]byte("the final flush"))
	if code := afterRun(m, out, nil, false); code != 1 {
		t.Fatalf("exit status %d", code)
	}
	if code := afterRun(m, out, nil, false); code != 1 {
		t.Fatalf("second exit status %d", code)
	}
	f.mu.Lock()
	admissions := f.admissions
	f.mu.Unlock()
	if admissions != 1 {
		t.Fatalf("%d draft saves, want 1", admissions)
	}
	if strings.Contains(tt.String(), statusClear) {
		t.Fatal("a status clear after the failure")
	}
}

// TestFailureSaveAnswersAtOnce: a save that answers ends the failure at once.
func TestFailureSaveAnswersAtOnce(t *testing.T) {
	f := &protocolFixture{}
	client, _ := startFixture(t, f)
	waitAttached(t, f)
	m := newModel(client)
	m.ready = true
	m.apply(threadTree(msgs(1)))
	m.editors["composer"].Area.SetValue("UNSAVED")
	start := time.Now()
	m.finishFailure()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the save took %v", d)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.admissions != 1 {
		t.Fatalf("%d draft saves", f.admissions)
	}
}

// TestFailureSaveIsBounded: two dirty composers whose saves never answer share one 5 s deadline;
// there is no retry.
func TestFailureSaveIsBounded(t *testing.T) {
	f := &protocolFixture{holdSaves: true}
	client, _ := startFixture(t, f)
	waitAttached(t, f)
	m := newModel(client)
	m.ready = true
	second := tvComposer
	second.ID = "composer2"
	m.apply(treeOf(Node{ID: "history", Type: "history", PageSize: 500, Messages: msgs(1)}, tvComposer, second))
	m.editors["composer"].Area.SetValue("UNSAVED-1")
	m.editors["composer2"].Area.SetValue("UNSAVED-2")
	start := time.Now()
	m.finishFailure()
	d := time.Since(start)
	if d < outputFailSaveTimeout-100*time.Millisecond || d > outputFailSaveTimeout+2*time.Second {
		t.Fatalf("the saves ended after %v, want about %v", d, outputFailSaveTimeout)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.admissions != 1 {
		t.Fatalf("%d save calls, want 1 (the first one holds the deadline; no retry)", f.admissions)
	}
}

// TestFailureSaveDetachedClient: a client without a hub connection ends at once, without a panic.
func TestFailureSaveDetachedClient(t *testing.T) {
	m := newModel(&viewClient{ViewID: "v1", Plugin: "messaging", Route: "/thread/42", Requests: make(chan mutation, 16)})
	m.ready = true
	m.apply(threadTree(msgs(1)))
	m.editors["composer"].Area.SetValue("UNSAVED")
	start := time.Now()
	m.finishFailure()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the detached save took %v", d)
	}
}

func waitAttached(t *testing.T, f *protocolFixture) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		ok := f.attachment != ""
		f.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no attach")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPaneOutHidesTheCursor: with hideCursor, each DECTCEM set is dropped and the write reports
// the full length.
func TestPaneOutHidesTheCursor(t *testing.T) {
	tt := &testTerm{}
	out := newPaneOut(tt)
	out.hideCursor = true
	in := "a\x1b[?25hb\x1b[?25lc\x1b[?25h"
	if n, err := out.Write([]byte(in)); err != nil || n != len(in) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if got, want := tt.String(), "ab\x1b[?25lc"; got != want {
		t.Fatalf("the pane got %q, want %q", got, want)
	}
}

// TestPaneOutLatchesTheFirstFailure: the first failure runs onFault once and closes fault; later
// writes are dropped.
func TestPaneOutLatchesTheFirstFailure(t *testing.T) {
	tt := &testTerm{}
	out := newPaneOut(tt)
	faults := 0
	out.onFault = func() { faults++ }
	if _, err := out.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	tt.set(func(t *testTerm) { t.shortAt = func([]byte) bool { return true } })
	if _, err := out.Write([]byte("short")); err == nil {
		t.Fatal("a short write reported no error")
	}
	tt.set(func(t *testTerm) { t.shortAt = nil })
	if _, err := out.Write([]byte("later")); err == nil {
		t.Fatal("a write after the failure succeeded")
	}
	select {
	case <-out.fault:
	default:
		t.Fatal("fault is open")
	}
	if faults != 1 || strings.Contains(tt.String(), "later") {
		t.Fatalf("faults %d, output %q", faults, tt.String())
	}
}

// TestFailureSavesCurrentEditorText (codex-29's probe on bbe841b3): a send waits with earlier
// text, and the editor holds newer text. The failure save keeps the current local text.
func TestFailureSavesCurrentEditorText(t *testing.T) {
	m := modeModel(20)
	m.apply(threadTree(msgs(1)))
	e := m.editors["composer"]
	e.Area.SetValue("CURRENT UNSAVED TEXT")
	e.SendText = "EARLIER SEND TEXT"
	e.WantSend = true
	saves := m.failureDrafts()
	if len(saves) != 1 || saves[0].args["text"] != "CURRENT UNSAVED TEXT" {
		t.Fatalf("the failure save does not hold the current editor text: %#v", saves)
	}
}
