package main

// A test-only prototype of the thread view's print path (thread view v1.2, before the printed
// region). It runs a real tea.Program (Bubble Tea v2.1.0) on a test output and proves:
//   - receipt: tea.Sequence(tea.Println(text), printDone) delivers printDone only after the
//     renderer's insertAbove write returned;
//   - the mode is decided once per process (v1.2 item 1): neutral until a tree with content; a
//     history node decides thread mode, which never uses the alt screen; any other content, a
//     history node beside a list or form, or the paired layout decides alt mode, which prints
//     nothing. A print released on a return from alt is written before the alt-off frame and is
//     lost, so there is no return from alt (the failed v1.1 switching design);
//   - the guard (lead-tv-altprint): no print is queued outside thread mode, and alt is never
//     chosen while a print is pending;
//   - a reattach: printDone commits its IDs whatever the attachment; seen uses only the
//     attachment that is current at printDone;
//   - the output failure policy (v1.2 item 3): the first failed or short write is fatal; no
//     ledger entry, no seen, no further output bytes; the draft is saved within a bound; exit
//     status 1; an idle program is woken by a watcher goroutine, never by a Send from the writer.
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
	// shortAt: the terminal takes only half of these bytes and reports no error (n < len(b),
	// err == nil). The wrapper treats it as a failure, as for an error.
	shortAt func(b []byte) bool
	failed  bool
	fault   chan struct{}
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
	if o.shortAt != nil && o.shortAt(b) {
		n, _ := o.buf.Write(b[:len(b)/2]) // the inner write: a partial count, no error
		o.failed = true
		close(o.fault)
		return n, nil
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

type protoMode int

const (
	modeNeutral protoMode = iota
	modeThread
	modeAlt
)

// protoMsg is one message of a history node.
type protoMsg struct{ id, text string }

// protoTree is a complete tree: its node kinds in order ("loading", "error", "error-action",
// "history", "composer", "menu", "widget", "list", "form"), the history messages (oldest first),
// the snapshot layout, and the text that the live region shows for it.
type protoTree struct {
	nodes    []string
	messages []protoMsg
	layout   string
	frame    string
	gate     chan struct{} // test only: the tree's print waits until the test closes it
}

// decideMode reads the complete tree. Only loading and error nodes keep the renderer neutral.
// History decides thread, also after a menu, unless a list or form stands beside it or the
// layout is the paired "transcript"; every other content node decides alt.
func decideMode(t protoTree) (mode protoMode, log string) {
	history, listOrForm, content := false, false, false
	for _, n := range t.nodes {
		switch n {
		case "loading", "error", "error-action":
		case "history":
			history = true
		case "list", "form":
			listOrForm, content = true, true
		default:
			content = true
		}
	}
	switch {
	case history && listOrForm:
		return modeAlt, "history beside list/form: alt mode"
	case history && t.layout == "transcript":
		return modeAlt, ""
	case history:
		return modeThread, ""
	case content:
		return modeAlt, ""
	}
	return modeNeutral, ""
}

func (t protoTree) hasHistory() bool {
	for _, n := range t.nodes {
		if n == "history" {
			return true
		}
	}
	return false
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
	protoReattach struct{ attach string }
	protoNewest   struct{ id string } // a frame whose newest ID is id
	outputFailed  struct{}
	draftSaved    struct{ err error }
)

type protoModel struct {
	out            *protoOut
	mode           protoMode
	frame          string // the live region's text for the current tree
	attach         string
	newest         string
	ledger         map[string]bool
	queued         map[string]bool
	pending        int
	seen           []string // attachment:newest, in order
	receipts       []bool   // at each printDone: the output holds the print's text
	logs           []string
	loggedHistory  bool     // the one log line of a history node in alt mode is written
	guard          []string // lead-tv-altprint violations; each test expects none
	failed         bool
	exitCode       int
	saveDraft      func(ctx context.Context) error
	saveCalled     bool
	saveBound      time.Duration
	quitAfterPrint bool
}

func newProtoModel(out *protoOut) *protoModel {
	return &protoModel{out: out, attach: "A", ledger: map[string]bool{}, queued: map[string]bool{}, saveBound: 200 * time.Millisecond}
}

// newThreadModel is a model whose mode a history tree already decided. The tests of the print
// path send protoPrint (the IDs that the renderer found unprinted) without a tree.
func newThreadModel(out *protoOut) *protoModel {
	m := newProtoModel(out)
	m.mode = modeThread
	return m
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
	case protoTree:
		return m, m.tree(msg)
	case protoPrint:
		return m, m.print(msg)
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
		if m.pending == 0 && m.quitAfterPrint {
			return m, tea.Quit
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

// tree draws a tree and, in thread mode, prints the unprinted messages of its history node.
func (m *protoModel) tree(t protoTree) tea.Cmd {
	m.frame = t.frame
	if m.mode == modeNeutral {
		mode, log := decideMode(t)
		if mode == modeAlt && m.pending > 0 {
			m.guard = append(m.guard, "alt chosen while a print is pending")
		}
		m.mode = mode
		if log != "" {
			m.logs = append(m.logs, log)
			m.loggedHistory = true
		}
	}
	if !t.hasHistory() {
		return nil
	}
	if m.mode == modeAlt {
		if t.layout != "transcript" && !m.loggedHistory {
			m.logs = append(m.logs, "history in alt mode: no print")
			m.loggedHistory = true
		}
		return nil
	}
	newest := t.messages[len(t.messages)-1].id
	m.newest = newest
	if m.ledger[newest] {
		m.seen = append(m.seen, m.attach+":"+newest)
	}
	var ids, lines []string
	for _, msg := range t.messages {
		if !m.ledger[msg.id] && !m.queued[msg.id] {
			ids, lines = append(ids, msg.id), append(lines, msg.text)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return m.print(protoPrint{ids: ids, text: strings.Join(lines, "\n"), gate: t.gate})
}

func (m *protoModel) print(p protoPrint) tea.Cmd {
	if m.mode != modeThread {
		m.guard = append(m.guard, "a print queued outside thread mode")
		return nil
	}
	for _, id := range p.ids {
		m.queued[id] = true
	}
	m.pending++
	attach, newest, ids, text, gate := m.attach, p.ids[len(p.ids)-1], p.ids, p.text, p.gate
	return tea.Sequence(
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
}

func (m *protoModel) View() tea.View {
	v := tea.NewView("LIVE " + m.attach + " " + m.frame)
	v.AltScreen = m.mode == modeAlt
	return v
}

// runProto runs the model on out and returns the final model. drive sends the messages; the
// watcher goroutine turns an output fault into outputFailed.
func runProto(t *testing.T, m *protoModel, drive func(p *tea.Program)) *protoModel {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(m.out), tea.WithWindowSize(60, 10), tea.WithoutSignals())
	fault := m.out.fault
	go func() {
		select {
		case <-fault:
			p.Send(outputFailed{})
		case <-ctx.Done():
		}
	}()
	go drive(p)
	final, err := p.Run()
	if ctx.Err() != nil {
		t.Fatalf("the program did not end: %v", err)
	}
	done := final.(*protoModel)
	if len(done.guard) != 0 {
		t.Fatalf("guard: %v", done.guard)
	}
	return done
}

const (
	altOn  = "\x1b[?1049h"
	altOff = "\x1b[?1049l"
)

// noAlt fails the test if the output holds an alt screen sequence.
func noAlt(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, altOn) || strings.Contains(out, altOff) {
		t.Fatalf("alt screen bytes in the output %q", out)
	}
}

func TestProtoPrintReceiptCommitsAfterTheWrite(t *testing.T) {
	m := newThreadModel(newProtoOut(nil))
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

func TestProtoDecideMode(t *testing.T) {
	cases := []struct {
		name   string
		tree   protoTree
		mode   protoMode
		logged bool
	}{
		{"loading", protoTree{nodes: []string{"loading"}}, modeNeutral, false},
		{"error with an action", protoTree{nodes: []string{"error-action"}}, modeNeutral, false},
		{"thread", protoTree{nodes: []string{"history", "composer", "menu"}}, modeThread, false},
		{"menu before history", protoTree{nodes: []string{"menu", "history", "composer"}}, modeThread, false},
		{"widget beside history", protoTree{nodes: []string{"widget", "history", "error"}}, modeThread, false},
		{"read-only thread", protoTree{nodes: []string{"widget", "history"}}, modeThread, false},
		{"paired transcript layout", protoTree{nodes: []string{"history", "composer"}, layout: "transcript"}, modeAlt, false},
		{"Gallery: list, form, history, composer", protoTree{nodes: []string{"list", "form", "history", "composer"}}, modeAlt, true},
		{"form beside history", protoTree{nodes: []string{"history", "form"}}, modeAlt, true},
		{"list", protoTree{nodes: []string{"list"}}, modeAlt, false},
		{"form", protoTree{nodes: []string{"form"}}, modeAlt, false},
		{"menu without history", protoTree{nodes: []string{"menu"}}, modeAlt, false},
		{"composer without history", protoTree{nodes: []string{"composer"}}, modeAlt, false},
	}
	for _, c := range cases {
		mode, log := decideMode(c.tree)
		if mode != c.mode || (log != "") != c.logged {
			t.Errorf("%s: mode=%d log=%q, want mode=%d logged=%t", c.name, mode, log, c.mode, c.logged)
		}
	}
}

// thread is a thread tree with these messages.
func thread(messages ...protoMsg) protoTree {
	return protoTree{nodes: []string{"history", "composer"}, messages: messages, frame: "COMPOSER"}
}

// TestProtoErrorFirstThenHistory (v1.2 test i): an error tree, with or without an action, comes
// first and keeps the renderer neutral; the history tree after the recovery decides thread mode.
func TestProtoErrorFirstThenHistory(t *testing.T) {
	for _, kind := range []string{"error", "error-action", "loading"} {
		m := newProtoModel(newProtoOut(nil))
		m = runProto(t, m, func(p *tea.Program) {
			p.Send(protoTree{nodes: []string{kind}, frame: "ERROR-FRAME"})
			time.Sleep(50 * time.Millisecond)
			p.Send(thread(protoMsg{"m1", "FIRST-MSG"}, protoMsg{"m2", "SECOND-MSG"}))
			time.Sleep(100 * time.Millisecond)
			p.Quit()
		})
		out := m.out.String()
		t.Logf("%s: raw output %q", kind, out)
		noAlt(t, out)
		if m.mode != modeThread {
			t.Fatalf("%s: mode=%d", kind, m.mode)
		}
		frame, first, second := strings.Index(out, "ERROR-FRAME"), strings.Index(out, "FIRST-MSG"), strings.Index(out, "SECOND-MSG")
		if frame < 0 || first < frame || second < first {
			t.Fatalf("%s: error frame=%d first=%d second=%d", kind, frame, first, second)
		}
		if strings.Count(out, "FIRST-MSG") != 1 || strings.Count(out, "SECOND-MSG") != 1 {
			t.Fatalf("%s: a message was not printed once", kind)
		}
		if !m.ledger["m1"] || !m.ledger["m2"] || strings.Join(m.seen, ",") != "A:m2" || len(m.logs) != 0 {
			t.Fatalf("%s: ledger=%v seen=%v logs=%v", kind, m.ledger, m.seen, m.logs)
		}
	}
}

// TestProtoErrorTreeInThreadMode (v1.2 test ii): a thread frame, then an error tree, then a thread
// frame with a new message. The error tree is drawn in the live region; no alt bytes; the new
// message is printed once, after the error frame. This replaces the failed v1.1 tests of the
// return from alt.
func TestProtoErrorTreeInThreadMode(t *testing.T) {
	m := newProtoModel(newProtoOut(nil))
	m = runProto(t, m, func(p *tea.Program) {
		p.Send(thread(protoMsg{"m1", "OLD-MSG"}))
		time.Sleep(50 * time.Millisecond)
		p.Send(protoTree{nodes: []string{"error-action"}, frame: "ERROR-FRAME"})
		time.Sleep(50 * time.Millisecond)
		p.Send(thread(protoMsg{"m1", "OLD-MSG"}, protoMsg{"m2", "NEW-MSG"}))
		time.Sleep(100 * time.Millisecond)
		p.Quit()
	})
	out := m.out.String()
	t.Logf("raw output %q", out)
	noAlt(t, out)
	frame, text := strings.Index(out, "ERROR-FRAME"), strings.Index(out, "NEW-MSG")
	if frame < 0 || text < frame {
		t.Fatalf("the new message is at %d and the error frame at %d", text, frame)
	}
	if strings.Count(out, "OLD-MSG") != 1 || strings.Count(out, "NEW-MSG") != 1 || !m.ledger["m2"] {
		t.Fatalf("counts or ledger wrong: ledger=%v", m.ledger)
	}
	if strings.Join(m.seen, ",") != "A:m1,A:m2" {
		t.Fatalf("seen=%v", m.seen)
	}
}

// TestProtoQueuedPrintThenErrorTree: an error tree arrives while a print is still pending. The
// mode stays thread, so the text is written once on the primary screen. (In v1.1 this needed an
// inline hold before the alt switch; in v1.2 no switch exists.)
func TestProtoQueuedPrintThenErrorTree(t *testing.T) {
	m := newProtoModel(newProtoOut(nil))
	gate := make(chan struct{})
	m = runProto(t, m, func(p *tea.Program) {
		tree := thread(protoMsg{"m1", "QUEUED-TEXT"})
		tree.gate = gate
		p.Send(tree)
		p.Send(protoTree{nodes: []string{"error"}, frame: "ERROR-FRAME"})
		time.Sleep(100 * time.Millisecond) // frames render while the print waits
		close(gate)
		time.Sleep(100 * time.Millisecond)
		p.Quit()
	})
	out := m.out.String()
	t.Logf("raw output %q", out)
	noAlt(t, out)
	if !m.ledger["m1"] || strings.Count(out, "QUEUED-TEXT") != 1 || strings.Join(m.seen, ",") != "A:m1" {
		t.Fatalf("ledger=%v count=%d seen=%v", m.ledger, strings.Count(out, "QUEUED-TEXT"), m.seen)
	}
}

// TestProtoListFirstThenHistory (v1.2 test iii): a list tree decides alt mode. Later history
// trees are drawn without a print and without a ledger, and the renderer logs one line.
func TestProtoListFirstThenHistory(t *testing.T) {
	m := newProtoModel(newProtoOut(nil))
	m = runProto(t, m, func(p *tea.Program) {
		p.Send(protoTree{nodes: []string{"list"}, frame: "LIST-FRAME"})
		time.Sleep(50 * time.Millisecond)
		p.Send(thread(protoMsg{"m1", "HIDDEN-ONE"}))
		p.Send(thread(protoMsg{"m1", "HIDDEN-ONE"}, protoMsg{"m2", "HIDDEN-TWO"}))
		time.Sleep(100 * time.Millisecond)
		p.Quit()
	})
	out := m.out.String()
	t.Logf("raw output %q", out)
	if strings.Count(out, altOn) != 1 || strings.Contains(out, "HIDDEN-") {
		t.Fatalf("alt on %d times; output %q", strings.Count(out, altOn), out)
	}
	if m.mode != modeAlt || len(m.ledger) != 0 || len(m.seen) != 0 {
		t.Fatalf("mode=%d ledger=%v seen=%v", m.mode, m.ledger, m.seen)
	}
	if strings.Join(m.logs, "|") != "history in alt mode: no print" {
		t.Fatalf("logs=%v", m.logs)
	}
}

// TestProtoGalleryTreeDecidesAlt (lead-tv-gallery): the current Gallery tree (list, form,
// history, composer) decides alt, prints nothing, keeps no ledger, and logs one line.
func TestProtoGalleryTreeDecidesAlt(t *testing.T) {
	m := newProtoModel(newProtoOut(nil))
	gallery := protoTree{nodes: []string{"list", "form", "history", "composer"}, messages: []protoMsg{{"m1", "GALLERY-MSG"}}, frame: "GALLERY-FRAME"}
	m = runProto(t, m, func(p *tea.Program) {
		p.Send(gallery)
		p.Send(gallery)
		time.Sleep(100 * time.Millisecond)
		p.Quit()
	})
	out := m.out.String()
	t.Logf("raw output %q", out)
	if !strings.Contains(out, altOn) || strings.Contains(out, "GALLERY-MSG") {
		t.Fatalf("output %q", out)
	}
	if m.mode != modeAlt || len(m.ledger) != 0 || strings.Join(m.logs, "|") != "history beside list/form: alt mode" {
		t.Fatalf("mode=%d ledger=%v logs=%v", m.mode, m.ledger, m.logs)
	}
}

// TestProtoPairedTreeDecidesAlt (v1.2 test v): a paired composer tree (layout "transcript") with
// a history node decides alt and prints nothing; the paired printer process prints.
func TestProtoPairedTreeDecidesAlt(t *testing.T) {
	m := newProtoModel(newProtoOut(nil))
	paired := thread(protoMsg{"m1", "PAIRED-MSG"})
	paired.layout = "transcript"
	m = runProto(t, m, func(p *tea.Program) {
		p.Send(paired)
		p.Send(paired)
		time.Sleep(100 * time.Millisecond)
		p.Quit()
	})
	out := m.out.String()
	if !strings.Contains(out, altOn) || strings.Contains(out, "PAIRED-MSG") {
		t.Fatalf("output %q", out)
	}
	if m.mode != modeAlt || len(m.ledger) != 0 || len(m.logs) != 0 {
		t.Fatalf("mode=%d ledger=%v logs=%v", m.mode, m.ledger, m.logs)
	}
}

// TestProtoQueuedPrintThenReattach: the print was queued for attachment A and is written after
// the reattach to B. The ledger keeps it (it is in this pane), A sends no seen, and B sends seen
// at its first frame whose newest ID is printed.
func TestProtoQueuedPrintThenReattach(t *testing.T) {
	m := newThreadModel(newProtoOut(nil))
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
	m := newThreadModel(out)
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
	m := newThreadModel(out)
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
	m := newThreadModel(out)
	m.saveDraft = func(ctx context.Context) error { select {} }
	start := time.Now()
	m = runProto(t, m, func(p *tea.Program) { p.Send(protoReattach{attach: "B"}) })
	if m.exitCode != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("exit=%d after %v", m.exitCode, time.Since(start))
	}
}

// TestProtoShortWriteIsFatal: the terminal takes half of the print's bytes and reports no error.
// The wrapper detects n < len(b): the same fatal policy as a failed write. Bubble Tea gets the
// short count (it may report io.ErrShortWrite itself; the policy does not depend on that).
func TestProtoShortWriteIsFatal(t *testing.T) {
	out := newProtoOut(nil)
	out.shortAt = func(b []byte) bool { return bytes.Contains(b, []byte("SHORT-PRINT")) }
	m := newThreadModel(out)
	m.saveDraft = func(context.Context) error { return nil }
	m = runProto(t, m, func(p *tea.Program) {
		p.Send(protoNewest{id: "m1"})
		p.Send(protoPrint{ids: []string{"m1"}, text: "SHORT-PRINT"})
	})
	if m.ledger["m1"] || len(m.seen) != 0 || m.exitCode != 1 || !m.saveCalled {
		t.Fatalf("ledger=%v seen=%v exit=%d save=%t", m.ledger, m.seen, m.exitCode, m.saveCalled)
	}
	before := len(out.String())
	time.Sleep(50 * time.Millisecond)
	if len(out.String()) != before {
		t.Fatal("bytes reached the output after the short write")
	}
	t.Logf("%d bytes reached the output (the half write included); %d bytes dropped after it", before, out.dropped)
}
