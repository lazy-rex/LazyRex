package main

// A test-only prototype of the thread view's print path (thread view v1.1, before the printed
// region). It runs a real tea.Program (Bubble Tea v2.1.0) on a test output and proves:
//   - receipt: tea.Sequence(tea.Println(text), printDone) delivers printDone only after the
//     renderer's insertAbove write returned;
//   - the inline hold: a tree that needs the alt screen waits until queued prints are done;
//     without the hold, the printed text goes to the alt screen (observed on iOS: L34);
//   - a reattach: printDone commits its IDs whatever the attachment; seen uses only the
//     attachment that is current at printDone;
//   - the output failure policy (lead-tv-print2): the first failed write is fatal; no ledger
//     entry, no seen, no further output bytes; the draft is saved within a bound; exit status 1;
//     an idle program is woken by a watcher goroutine, never by a Send from the writer.
// The production code takes these rules, not this model.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

var errProtoWrite = errors.New("test output failed")

// protoOut is the output boundary. After the first failure it forwards no byte. It never calls
// Program.Send: it only closes fault, and a watcher goroutine wakes the program.
type protoOut struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	failAt func(b []byte) bool
	failed bool
	fault  chan struct{}
	// dropped counts the bytes that callers wrote after the failure.
	dropped int
}

func newProtoOut(failAt func([]byte) bool) *protoOut {
	return &protoOut{failAt: failAt, fault: make(chan struct{})}
}

func (o *protoOut) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed {
		o.dropped += len(b)
		return 0, errProtoWrite
	}
	if o.failAt != nil && o.failAt(b) {
		o.failed = true
		close(o.fault)
		return 0, errProtoWrite
	}
	return o.buf.Write(b)
}

func (o *protoOut) Failed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.failed
}

func (o *protoOut) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// Messages of the prototype model.
type (
	protoPrint struct {
		ids  []string
		text string
		gate chan struct{} // test only: the print waits until the test closes it
	}
	printDone struct {
		ids            []string
		attach, newest string
		text           string
	}
	protoTree     struct{ alt bool } // a tree: alt = an error tree
	protoReattach struct{ attach string }
	protoNewest   struct{ id string } // a frame whose newest ID is id
	outputFailed  struct{}
	draftSaved    struct{ err error }
)

type protoModel struct {
	out            *protoOut
	hold           bool // the inline hold; false only for the control test
	attach         string
	newest         string
	ledger         map[string]bool
	queued         map[string]bool
	pending        int
	alt, wantAlt   bool
	seen           []string // attachment:newest, in order
	receipts       []bool   // at each printDone: the output holds the print's text
	failed         bool
	exitCode       int
	saveDraft      func(ctx context.Context) error
	saveCalled     bool
	saveBound      time.Duration
	quitAfterPrint bool
}

func newProtoModel(out *protoOut) *protoModel {
	return &protoModel{out: out, hold: true, attach: "A", ledger: map[string]bool{}, queued: map[string]bool{}, saveBound: 200 * time.Millisecond}
}

func (m *protoModel) Init() tea.Cmd { return nil }

func (m *protoModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.failed {
		if _, ok := msg.(draftSaved); ok {
			return m, tea.Quit
		}
		return m, nil
	}
	switch msg := msg.(type) {
	case protoPrint:
		for _, id := range msg.ids {
			m.queued[id] = true
		}
		m.pending++
		attach, newest, ids, text := m.attach, msg.ids[len(msg.ids)-1], msg.ids, msg.text
		gate := msg.gate
		return m, tea.Sequence(
			func() tea.Msg {
				if gate != nil {
					<-gate
				}
				return nil
			},
			tea.Println(text),
			func() tea.Msg {
				return printDone{ids: ids, attach: attach, newest: newest, text: text}
			},
		)
	case printDone:
		m.pending--
		// The event loop wrote the line (insertAbove) before it delivered this message: both came
		// from one Sequence, in order. The check is here, in Update, not in the Cmd's goroutine:
		// p.Send returns before the loop has handled the print.
		// insertAbove writes each line, then erase-to-end and CR LF, so each line is checked.
		written := true
		for _, line := range strings.Split(msg.text, "\n") {
			written = written && strings.Contains(m.out.String(), line)
		}
		m.receipts = append(m.receipts, written)
		// The print's write may have failed. insertAbove returned before this message, so the
		// output already knows; the watcher's outputFailed may still be on its way.
		if m.out.Failed() {
			return m, nil
		}
		for _, id := range msg.ids {
			delete(m.queued, id)
			m.ledger[id] = true
		}
		if msg.attach == m.attach && m.ledger[m.newest] {
			m.seen = append(m.seen, m.attach+":"+m.newest)
		}
		if m.pending == 0 && m.wantAlt {
			m.alt, m.wantAlt = true, false
		}
		if m.pending == 0 && m.quitAfterPrint {
			return m, tea.Quit
		}
	case protoTree:
		if !msg.alt {
			m.alt, m.wantAlt = false, false
		} else if m.hold && m.pending > 0 {
			m.wantAlt = true
		} else {
			m.alt = true
		}
	case protoReattach:
		m.attach = msg.attach
	case protoNewest:
		m.newest = msg.id
		if m.ledger[msg.id] {
			m.seen = append(m.seen, m.attach+":"+msg.id)
		}
	case outputFailed:
		m.failed, m.exitCode = true, 1
		save, bound := m.saveDraft, m.saveBound
		m.saveCalled = save != nil
		return m, func() tea.Msg {
			if save == nil {
				return draftSaved{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), bound)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- save(ctx) }()
			select {
			case err := <-done:
				return draftSaved{err: err}
			case <-ctx.Done():
				return draftSaved{err: ctx.Err()}
			}
		}
	}
	return m, nil
}

func (m *protoModel) View() tea.View {
	if m.alt {
		v := tea.NewView("ERROR TREE")
		v.AltScreen = true
		return v
	}
	return tea.NewView("LIVE " + m.attach)
}

// runProto runs the model on out and returns the final model. drive sends the messages; the
// watcher goroutine turns an output fault into outputFailed.
func runProto(t *testing.T, m *protoModel, drive func(p *tea.Program)) *protoModel {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(m.out), tea.WithWindowSize(60, 10), tea.WithoutSignals())
	go func() {
		select {
		case <-m.out.fault:
			p.Send(outputFailed{})
		case <-ctx.Done():
		}
	}()
	go drive(p)
	final, err := p.Run()
	if ctx.Err() != nil {
		t.Fatalf("the program did not end: %v", err)
	}
	return final.(*protoModel)
}

const altOn = "\x1b[?1049h"

func TestProtoPrintReceiptCommitsAfterTheWrite(t *testing.T) {
	m := newProtoModel(newProtoOut(nil))
	m.quitAfterPrint = true
	m = runProto(t, m, func(p *tea.Program) {
		p.Send(protoNewest{id: "m2"})
		p.Send(protoPrint{ids: []string{"m1", "m2"}, text: "M1-TEXT\nM2-TEXT"})
	})
	if len(m.receipts) != 1 || !m.receipts[0] {
		t.Fatalf("printDone came before the write: %v", m.receipts)
	}
	if !m.ledger["m1"] || !m.ledger["m2"] || len(m.queued) != 0 {
		t.Fatalf("ledger=%v queued=%v", m.ledger, m.queued)
	}
	if strings.Join(m.seen, ",") != "A:m2" {
		t.Fatalf("seen=%v", m.seen)
	}
	if strings.Count(m.out.String(), "M1-TEXT") != 1 {
		t.Fatalf("output=%q", m.out.String())
	}
}

// TestProtoQueuedPrintThenErrorTree: an error tree arrives while a print is queued. With the hold
// the printed text reaches the primary screen before the alt screen starts; without it (the
// control), the text is written after the alt screen started, so it is lost to scrollback.
func TestProtoQueuedPrintThenErrorTree(t *testing.T) {
	for _, hold := range []bool{true, false} {
		m := newProtoModel(newProtoOut(nil))
		m.hold = hold
		gate := make(chan struct{})
		m = runProto(t, m, func(p *tea.Program) {
			p.Send(protoNewest{id: "m1"})
			p.Send(protoPrint{ids: []string{"m1"}, text: "QUEUED-TEXT", gate: gate})
			p.Send(protoTree{alt: true})
			time.Sleep(100 * time.Millisecond) // frames render while the print waits
			close(gate)
			time.Sleep(100 * time.Millisecond)
			p.Send(protoTree{alt: false}) // recovery
			time.Sleep(100 * time.Millisecond)
			p.Quit()
		})
		out := m.out.String()
		text, alt := strings.Index(out, "QUEUED-TEXT"), strings.Index(out, altOn)
		if text < 0 || alt < 0 {
			t.Fatalf("hold=%t: text=%d alt=%d output=%q", hold, text, alt, out)
		}
		t.Logf("hold=%t: text at %d, alt on at %d; raw output %q", hold, text, alt, out)
		if hold != (text < alt) {
			t.Fatalf("hold=%t: the text is at %d and the alt screen starts at %d", hold, text, alt)
		}
		if !m.ledger["m1"] || strings.Count(out, "QUEUED-TEXT") != 1 {
			t.Fatalf("hold=%t: ledger=%v count=%d", hold, m.ledger, strings.Count(out, "QUEUED-TEXT"))
		}
		if hold && strings.Join(m.seen, ",") != "A:m1" {
			t.Fatalf("seen=%v", m.seen)
		}
	}
}

// TestProtoQueuedPrintThenReattach: the print was queued for attachment A and is written after
// the reattach to B. The ledger keeps it (it is in this pane), A sends no seen, and B sends seen
// at its first frame whose newest ID is printed.
func TestProtoQueuedPrintThenReattach(t *testing.T) {
	m := newProtoModel(newProtoOut(nil))
	gate := make(chan struct{})
	m = runProto(t, m, func(p *tea.Program) {
		p.Send(protoNewest{id: "m1"})
		p.Send(protoPrint{ids: []string{"m1"}, text: "BEFORE-REATTACH", gate: gate})
		p.Send(protoReattach{attach: "B"})
		close(gate)
		time.Sleep(100 * time.Millisecond)
		p.Send(protoNewest{id: "m1"}) // B's first frame: the newest ID is already printed
		time.Sleep(50 * time.Millisecond)
		p.Quit()
	})
	if !m.ledger["m1"] {
		t.Fatal("the reattach dropped a written print")
	}
	if strings.Join(m.seen, ",") != "B:m1" {
		t.Fatalf("seen=%v, want only B:m1", m.seen)
	}
	if strings.Count(m.out.String(), "BEFORE-REATTACH") != 1 {
		t.Fatal("the print was written twice or not at all")
	}
}

// TestProtoInsertAboveWriteFailureIsFatal: the print's own write fails. No ledger entry, no seen,
// the draft is saved, the exit status is 1, and no byte reaches the output after the failure.
func TestProtoInsertAboveWriteFailureIsFatal(t *testing.T) {
	out := newProtoOut(func(b []byte) bool { return bytes.Contains(b, []byte("FAILING-PRINT")) })
	m := newProtoModel(out)
	m.saveDraft = func(context.Context) error { return nil }
	m = runProto(t, m, func(p *tea.Program) {
		p.Send(protoNewest{id: "m1"})
		p.Send(protoPrint{ids: []string{"m1"}, text: "FAILING-PRINT"})
	})
	if m.ledger["m1"] || len(m.seen) != 0 || m.exitCode != 1 || !m.saveCalled {
		t.Fatalf("ledger=%v seen=%v exit=%d save=%t", m.ledger, m.seen, m.exitCode, m.saveCalled)
	}
	if strings.Contains(out.String(), "FAILING-PRINT") {
		t.Fatal("the failed print reached the output")
	}
	before := len(out.String())
	time.Sleep(50 * time.Millisecond)
	if len(out.String()) != before {
		t.Fatal("bytes reached the output after the failure")
	}
}

// TestProtoIdleFrameFailureWakesTheProgram: the last frame write fails, and then nothing is sent:
// no key, no timer, no message. The watcher wakes the program, which saves and exits with 1.
func TestProtoIdleFrameFailureWakesTheProgram(t *testing.T) {
	out := newProtoOut(func(b []byte) bool { return bytes.Contains(b, []byte("LIVE B")) })
	m := newProtoModel(out)
	m.saveDraft = func(context.Context) error { return nil }
	start := time.Now()
	m = runProto(t, m, func(p *tea.Program) {
		p.Send(protoReattach{attach: "B"}) // the next frame shows "LIVE B" and fails
	})
	if m.exitCode != 1 || !m.saveCalled {
		t.Fatalf("exit=%d save=%t", m.exitCode, m.saveCalled)
	}
	if strings.Contains(out.String(), "LIVE B") {
		t.Fatal("the failed frame reached the output")
	}
	t.Logf("exit after %v; %d bytes written by callers after the failure were dropped", time.Since(start), out.dropped)
}

// TestProtoHangingDraftSaveDoesNotBlockTheExit: the save never returns; the bound ends it.
func TestProtoHangingDraftSaveDoesNotBlockTheExit(t *testing.T) {
	out := newProtoOut(func(b []byte) bool { return bytes.Contains(b, []byte("LIVE B")) })
	m := newProtoModel(out)
	m.saveDraft = func(ctx context.Context) error { select {} }
	start := time.Now()
	m = runProto(t, m, func(p *tea.Program) { p.Send(protoReattach{attach: "B"}) })
	if m.exitCode != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("exit=%d after %v", m.exitCode, time.Since(start))
	}
}
