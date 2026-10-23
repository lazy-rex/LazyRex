package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The history fetch (fetch.go; record v1.2 items 2 and 3, lead-tv-excl) on the production model
// in a Bubble Tea program. fakePages answers hub.view.history from a thread.

type fakePages struct {
	mu        sync.Mutex
	thread    []Message
	size      int
	calls     int
	cancelled int
	hold      chan struct{} // the next call waits until it is closed (or its fetch stops)
	answer    func(before string, call int) (historyPage, bool)
}

func (f *fakePages) get(ctx context.Context, before string) (historyPage, error) {
	f.mu.Lock()
	f.calls++
	call, hold, answer := f.calls, f.hold, f.answer
	f.hold = nil
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			f.mu.Lock()
			f.cancelled++
			f.mu.Unlock()
			return historyPage{}, ctx.Err()
		}
	}
	if answer != nil {
		if page, ok := answer(before, call); ok {
			return page, nil
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	end := slices.IndexFunc(f.thread, func(x Message) bool { return x.ID == before })
	if end < 0 {
		return historyPage{}, fmt.Errorf("unknown cursor %s", before)
	}
	start := max(0, end-f.size)
	return historyPage{Status: "ready", Messages: append([]Message(nil), f.thread[start:end]...), HasMore: start > 0}, nil
}

func (f *fakePages) holdNext() chan struct{} {
	gate := make(chan struct{})
	f.mu.Lock()
	f.hold = gate
	f.mu.Unlock()
	return gate
}

func (f *fakePages) counts() (calls, cancelled int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.cancelled
}

func span(from, to int) []int {
	var ids []int
	for i := from; i <= to; i++ {
		ids = append(ids, i)
	}
	return ids
}

// page is a thread tree whose history node holds the messages, with has_more and total.
func page(messages []Message, more bool, total int) Snapshot {
	h := Node{ID: "history", Type: "history", PageSize: 500, Messages: messages, HasMore: more}
	if total >= 0 {
		h.Total = &total
	}
	return treeOf(h, tvComposer)
}

// startFetchRun starts a print run whose pages come from f, attached: the status is "Connected".
func startFetchRun(t *testing.T, f *fakePages) *printRun {
	t.Helper()
	r := startPrintWith(t, &testTerm{}, func(m *model) { m.print.pages = f.get })
	r.p.Send(connectionMsg{Ready: true, Text: "Connected"})
	return r
}

var printedText = regexp.MustCompile(`TEXT-(\d+)\x1b`)

// sequence returns the message numbers in the output, in print order.
func sequence(out string) []int {
	var seq []int
	for _, m := range printedText.FindAllStringSubmatch(out, -1) {
		n, _ := strconv.Atoi(m[1])
		seq = append(seq, n)
	}
	return seq
}

func wantSequence(t *testing.T, out string, want []int) {
	t.Helper()
	if got := sequence(out); !slices.Equal(got, want) {
		t.Fatalf("printed %d messages %v…, want %d %v…", len(got), got[:min(len(got), 8)], len(want), want[:min(len(want), 8)])
	}
}

func idle(s probeState) bool { return !s.fetching && s.waiting == 0 }

// TestFetchOpenTwoPages: the first tree holds 301..600 of 600 with has_more. The open fetches
// 1..300 and prints exactly the newest 500 once, oldest first, after the count line.
func TestFetchOpenTwoPages(t *testing.T) {
	f := &fakePages{thread: msgs(span(1, 600)...), size: 300}
	r := startFetchRun(t, f)
	r.p.Send(page(msgs(span(301, 600)...), true, 600))
	s := r.wait("the newest 500 printed", func(s probeState) bool { return idle(s) && s.printed["m600"] })
	r.stop()
	out := r.term.String()
	wantSequence(t, out, span(101, 600))
	if calls, _ := f.counts(); calls != 1 || s.excluded != 100 {
		t.Fatalf("%d page calls, %d excluded; want 1 and 100", calls, s.excluded)
	}
	head := strings.Index(out, "100 earlier messages. Thread Settings… > Show full history")
	if head < 0 || head > strings.Index(out, "TEXT-101\x1b") || strings.Count(out, "earlier messages") != 1 {
		t.Fatalf("the count line is missing, repeated, or not first: %q", out[:min(len(out), 300)])
	}
}

// TestFetchOpenFirstPageOver500 (lead-tv-excl): a first page of 1..520 needs no fetch. 21..520
// print; 1..20 are excluded, also when the same page comes again. A new message inside the window
// prints once, late.
func TestFetchOpenFirstPageOver500(t *testing.T) {
	f := &fakePages{thread: msgs(span(1, 520)...), size: 300}
	r := startFetchRun(t, f)
	r.p.Send(page(msgs(span(1, 520)...), false, 520))
	r.wait("printed", func(s probeState) bool { return idle(s) && s.printed["m520"] })
	r.p.Send(page(msgs(span(1, 520)...), false, 520))
	inside := append(append(msgs(span(1, 300)...), msgs(9001)...), msgs(span(301, 520)...)...)
	r.p.Send(page(inside, false, 521))
	r.wait("the late message", func(s probeState) bool { return idle(s) && s.printed["m9001"] })
	r.stop()
	out := r.term.String()
	wantSequence(t, out, append(span(21, 520), 9001))
	if calls, _ := f.counts(); calls != 0 {
		t.Fatalf("%d page calls for a first page over 500", calls)
	}
	if !strings.Contains(out, "20 earlier messages.") || strings.Count(out, "(late)") != 1 {
		t.Fatalf("the count line or the one late mark is missing: %q", out[:min(len(out), 300)])
	}
}

// TestFetchOpenCountLine: a thread of exactly 500 has no count line; a cut without total says
// that earlier messages exist.
func TestFetchOpenCountLine(t *testing.T) {
	r := startFetchRun(t, &fakePages{})
	r.p.Send(page(msgs(span(1, 500)...), false, 500))
	r.wait("printed", func(s probeState) bool { return idle(s) && s.printed["m500"] })
	r.stop()
	if out := r.term.String(); strings.Contains(out, "earlier messages") || strings.Contains(out, "Earlier messages") {
		t.Fatalf("a count line for a whole thread: %q", out[:300])
	}
	r = startFetchRun(t, &fakePages{})
	r.p.Send(page(msgs(span(1, 520)...), false, -1))
	r.wait("printed", func(s probeState) bool { return idle(s) && s.printed["m520"] })
	r.stop()
	if out := r.term.String(); strings.Count(out, "Earlier messages exist. Thread Settings… > Show full history") != 1 {
		t.Fatalf("no 'Earlier messages exist' line without total: %q", out[:300])
	}
}

// TestFetchFullMode: in full mode the open fetches every page and prints the whole thread, with
// no count line and no exclusion.
func TestFetchFullMode(t *testing.T) {
	f := &fakePages{thread: msgs(span(1, 700)...), size: 300}
	r := startFetchRun(t, f)
	tree := page(msgs(span(401, 700)...), true, 700)
	tree.Open = &OpenArgs{Full: true}
	r.p.Send(tree)
	s := r.wait("printed", func(s probeState) bool { return idle(s) && s.printed["m700"] })
	r.stop()
	out := r.term.String()
	wantSequence(t, out, span(1, 700))
	if calls, _ := f.counts(); calls != 2 || s.excluded != 0 || strings.Contains(out, "earlier messages") {
		t.Fatalf("full mode: %d calls, %d excluded, count line %v", calls, s.excluded, strings.Contains(out, "earlier messages"))
	}
}

// TestFetchOpenRestartsWhenThePageMoves (codex-29): the open holds 601..900 and waits for a page;
// the current page moves to 1501..1800, which does not continue the run. The fetch restarts from
// the new page (the held call stops), and the print is exactly 1301..1800. 601..900 stay
// excluded: a later frame with them prints nothing.
func TestFetchOpenRestartsWhenThePageMoves(t *testing.T) {
	f := &fakePages{thread: msgs(span(1, 1800)...), size: 300}
	gate := f.holdNext()
	r := startFetchRun(t, f)
	r.p.Send(page(msgs(span(601, 900)...), true, 1800))
	r.wait("the open fetch waits", func(s probeState) bool { return s.fetching && s.notice == loadingHistory })
	r.p.Send(page(msgs(span(1501, 1800)...), true, 1800))
	s := r.wait("printed", func(s probeState) bool { return idle(s) && s.printed["m1800"] })
	close(gate)
	if calls, cancelled := f.counts(); calls != 2 || cancelled != 1 {
		t.Fatalf("%d page calls, %d cancelled; want 2 and 1", calls, cancelled)
	}
	if s.excluded != 300+100 {
		t.Fatalf("%d excluded, want 400 (601..900 and 1201..1300)", s.excluded)
	}
	r.p.Send(page(msgs(span(601, 900)...), true, 1800))
	r.wait("the old frame is done", idle)
	r.stop()
	wantSequence(t, r.term.String(), span(1301, 1800))
	if calls, _ := f.counts(); calls != 2 {
		t.Fatalf("the old frame of excluded IDs started a fill: %d page calls, want 2", calls)
	}
}

// TestFetchOpenFollowsAnOverlappingPage: the current page moves to 651..950, which continues the
// run: no restart, and the print is exactly 451..950.
func TestFetchOpenFollowsAnOverlappingPage(t *testing.T) {
	f := &fakePages{thread: msgs(span(1, 950)...), size: 300}
	gate := f.holdNext()
	r := startFetchRun(t, f)
	r.p.Send(page(msgs(span(601, 900)...), true, 950))
	r.wait("the open fetch waits", func(s probeState) bool { return s.fetching })
	r.p.Send(page(msgs(span(651, 950)...), true, 950))
	r.wait("the moved page", func(s probeState) bool { return s.fetching })
	close(gate)
	r.wait("printed", func(s probeState) bool { return idle(s) && s.printed["m950"] })
	r.stop()
	wantSequence(t, r.term.String(), span(451, 950))
	if calls, cancelled := f.counts(); calls != 1 || cancelled != 0 {
		t.Fatalf("%d page calls, %d cancelled; want 1 and 0", calls, cancelled)
	}
}

// TestFetchGap (record item 2): 1..500 printed; after a reattach the page holds only
// 1001..1100. The fill pages back until a page holds a printed ID and prints 501..1100 in order,
// not late. A page whose oldest ID is printed starts no fill.
func TestFetchGap(t *testing.T) {
	f := &fakePages{thread: msgs(span(1, 1100)...), size: 300}
	r := startFetchRun(t, f)
	r.p.Send(page(msgs(span(1, 500)...), false, 500))
	r.wait("1..500", func(s probeState) bool { return idle(s) && s.printed["m500"] })
	reattach(r.m.client, "B")
	r.p.Send(stamp(r.m.client, page(msgs(span(1001, 1100)...), true, 1100)))
	r.wait("the fill", func(s probeState) bool { return idle(s) && s.printed["m1100"] })
	r.p.Send(stamp(r.m.client, page(msgs(span(1001, 1101)...), true, 1101)))
	r.wait("the next message", func(s probeState) bool { return idle(s) && s.printed["m1101"] })
	r.stop()
	out := r.term.String()
	wantSequence(t, out, span(1, 1101))
	if calls, _ := f.counts(); calls != 2 || strings.Contains(out, "(late)") {
		t.Fatalf("%d page calls (want 2), late marks %d", calls, strings.Count(out, "(late)"))
	}
}

// TestFetchGapAnchorByPageOrder (codex-29): 1 and 5 printed, then 3 late. The fill's run is the
// fetched 3, 4, 5 and the current 6..9. The anchor is 5, the last printed ID in page order (not
// 3, the last print): 6..9 print, and the fetched 4 never prints.
func TestFetchGapAnchorByPageOrder(t *testing.T) {
	f := &fakePages{thread: msgs(1, 3, 4, 5, 6, 7, 8, 9), size: 3}
	r := startFetchRun(t, f)
	r.p.Send(page(msgs(1, 5), false, 2))
	r.wait("1 and 5", func(s probeState) bool { return idle(s) && s.printed["m5"] })
	r.p.Send(page(msgs(1, 3, 5), false, 3))
	r.wait("3, late", func(s probeState) bool { return idle(s) && s.printed["m3"] })
	r.p.Send(page(msgs(6, 7, 8, 9), true, 8))
	r.wait("the fill", func(s probeState) bool { return idle(s) && s.printed["m9"] })
	r.stop()
	wantSequence(t, r.term.String(), []int{1, 5, 3, 6, 7, 8, 9})
}

// TestFetchNoProgressPageRetries (codex-29 item 7): a page with has_more and nothing new is a
// failed page. The fetch asks again after 2 s and finishes. This test takes the real delay.
func TestFetchNoProgressPageRetries(t *testing.T) {
	f := &fakePages{thread: msgs(span(1, 600)...), size: 300}
	f.answer = func(before string, call int) (historyPage, bool) {
		if call == 1 {
			return historyPage{Status: "ready", Messages: msgs(301), HasMore: true}, true // held already
		}
		return historyPage{}, false
	}
	r := startFetchRun(t, f)
	start := time.Now()
	r.p.Send(page(msgs(span(301, 600)...), true, 600))
	deadline := time.Now().Add(6 * time.Second)
	for {
		reply := make(chan probeState, 1)
		r.p.Send(probe{reply})
		if s := <-reply; idle(s) && s.printed["m600"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the retried fetch did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	took := time.Since(start)
	r.stop()
	wantSequence(t, r.term.String(), span(101, 600))
	if calls, _ := f.counts(); calls != 2 || took < pageRetryDelay {
		t.Fatalf("%d page calls in %v; want 2 after at least %v", calls, took, pageRetryDelay)
	}
}

// TestFetchStopsOnReattach: the open waits for a page when the hub goes away. The fetch stops
// (its call ends), and the new attachment's first tree opens again: one count line, one copy.
func TestFetchStopsOnReattach(t *testing.T) {
	f := &fakePages{thread: msgs(span(1, 600)...), size: 300}
	gate := f.holdNext()
	r := startFetchRun(t, f)
	r.p.Send(page(msgs(span(301, 600)...), true, 600))
	r.wait("the open fetch waits", func(s probeState) bool { return s.fetching })
	r.p.Send(connectionMsg{Ready: false, Text: "Reconnecting"})
	r.wait("the fetch stopped", func(s probeState) bool { return !s.fetching && !s.opened })
	close(gate)
	reattach(r.m.client, "B")
	r.p.Send(connectionMsg{Ready: true, Text: "Connected"})
	r.p.Send(stamp(r.m.client, page(msgs(span(301, 600)...), true, 600)))
	r.wait("printed", func(s probeState) bool { return idle(s) && s.printed["m600"] })
	r.stop()
	out := r.term.String()
	wantSequence(t, out, span(101, 600))
	if calls, cancelled := f.counts(); calls != 2 || cancelled != 1 || strings.Count(out, "earlier messages") != 1 {
		t.Fatalf("%d calls, %d cancelled, %d count lines; want 2, 1, 1", calls, cancelled, strings.Count(out, "earlier messages"))
	}
}

// TestFetchNoSeenWhileFetching (record item 3): the receipt of a pending print arrives while a gap
// fill runs: no seen. Seen opens after the fill's print of the newest.
func TestFetchNoSeenWhileFetching(t *testing.T) {
	f := &fakePages{thread: msgs(span(1, 1100)...), size: 300}
	r := startFetchRun(t, f)
	r.p.Send(page(msgs(span(1, 500)...), false, 500))
	r.wait("1..500 and seen", func(s probeState) bool { return idle(s) && s.printed["m500"] && s.seenOK })
	printGate := r.holdNextPrint()
	r.p.Send(page(msgs(span(1, 501)...), false, 501))
	r.wait("the print of 501 waits", func(s probeState) bool { return !s.printed["m501"] && !s.seenOK })
	pageGate := f.holdNext()
	r.p.Send(page(msgs(span(1001, 1100)...), true, 1100))
	r.wait("the fill waits", func(s probeState) bool { return s.fetching })
	close(printGate)
	s := r.wait("the receipt of 501", func(s probeState) bool { return s.printed["m501"] })
	if s.seenOK || !s.fetching {
		t.Fatalf("seen opened during the fill (seen %v, fetching %v)", s.seenOK, s.fetching)
	}
	close(pageGate)
	r.wait("the fill's print opens seen", func(s probeState) bool { return idle(s) && s.printed["m1100"] && s.seenOK })
	r.stop()
	wantSequence(t, r.term.String(), span(1, 1100))
}

// TestFetchGapWithoutAnchor: the thread no longer holds a printed message. The fill pages back to
// the start and finds no anchor. It prints the current page only (11..13), never the fetched
// 7..10, and the same page again starts no fill.
func TestFetchGapWithoutAnchor(t *testing.T) {
	f := &fakePages{thread: msgs(span(7, 13)...), size: 3}
	r := startFetchRun(t, f)
	r.p.Send(page(msgs(1, 2, 3), false, 3))
	r.wait("1..3", func(s probeState) bool { return idle(s) && s.printed["m3"] })
	r.p.Send(page(msgs(11, 12, 13), true, 7))
	r.wait("the fill", func(s probeState) bool { return idle(s) && s.printed["m13"] })
	r.p.Send(page(msgs(11, 12, 13), true, 7))
	r.wait("the same page", idle)
	r.stop()
	wantSequence(t, r.term.String(), []int{1, 2, 3, 11, 12, 13})
	if calls, _ := f.counts(); calls != 2 {
		t.Fatalf("%d page calls, want 2", calls)
	}
}

// TestFetchComposerWorksWhileAPageIsHeld (codex-29): the open waits for a page. The user types and
// sends; the send and its result finish while the page is still held. Then the page comes, and
// the print is the newest 500.
func TestFetchComposerWorksWhileAPageIsHeld(t *testing.T) {
	f := &fakePages{thread: msgs(span(1, 600)...), size: 300}
	gate := f.holdNext()
	r := startFetchRun(t, f)
	r.p.Send(page(msgs(span(301, 600)...), true, 600))
	r.wait("the open fetch waits", func(s probeState) bool { return s.fetching })
	for _, c := range "hi" {
		r.p.Send(tea.KeyPressMsg{Code: c, Text: string(c)})
	}
	r.p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	sent := false
	for deadline := time.After(5 * time.Second); !sent; {
		select {
		case req := <-r.m.client.Requests:
			switch req.Method {
			case "hub.view.save":
				r.p.Send(completed{req, operationResult{Status: "succeeded", Result: map[string]any{"draft_revision": "2", "text": req.Args["text"]}}})
			case "hub.view.send":
				if req.Args["text"] != "hi" {
					t.Fatalf("sent %q, want hi", req.Args["text"])
				}
				r.p.Send(completed{req, operationResult{Status: "succeeded", Result: map[string]any{"draft_revision": "3", "message_id": "m9001"}}})
				sent = true
			}
		case <-deadline:
			t.Fatal("no send request while the page is held")
		}
	}
	s := r.wait("the send finished", func(s probeState) bool { return s.composer == "" && s.revision == "3" })
	if !s.fetching {
		t.Fatal("the page was released before the send finished")
	}
	close(gate)
	r.wait("printed", func(s probeState) bool { return idle(s) && s.printed["m600"] })
	r.stop()
	wantSequence(t, r.term.String(), span(101, 600))
}
