package main

import (
	"errors"
	"fmt"
	"testing"
)

// fakeThread serves pages of at most size messages, like the plugin's view.history.
func fakeThread(n, size int, calls *int) func(string) (historyPage, error) {
	return func(before string) (historyPage, error) {
		*calls++
		end := -1
		for i := 0; i < n; i++ {
			if fmt.Sprint(i) == before {
				end = i
			}
		}
		if end < 0 {
			return historyPage{}, errors.New("unknown cursor")
		}
		start := max(0, end-size)
		var page historyPage
		for i := start; i < end; i++ {
			page.Messages = append(page.Messages, Message{ID: fmt.Sprint(i), Text: "m"})
		}
		page.HasMore = start > 0
		return page, nil
	}
}

func tailSnapshot(from, to int, hasMore bool) Snapshot {
	s := Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/thread/x", Revision: "1"}
	h := Node{ID: "history", Type: "history", HasMore: hasMore}
	for i := from; i < to; i++ {
		h.Messages = append(h.Messages, Message{ID: fmt.Sprint(i), Text: "m"})
	}
	s.Tree.Nodes = []Node{h}
	return s
}

func ids(s Snapshot) []string {
	var out []string
	for _, m := range historyNode(s).Messages {
		out = append(out, m.ID)
	}
	return out
}

func TestFullHistoryFetchesEveryOlderPage(t *testing.T) {
	calls := 0
	f := newFullHistory(fakeThread(100, 30, &calls))
	got := f.merge(tailSnapshot(90, 100, true))
	if h := historyNode(got); len(h.Messages) != 100 || h.HasMore || h.Messages[0].ID != "0" || h.Messages[99].ID != "99" {
		t.Fatalf("whole thread not merged: %v", ids(got))
	}
	for i, id := range ids(got) {
		if id != fmt.Sprint(i) {
			t.Fatalf("message %d is %s", i, id)
		}
	}
	if calls != 3 {
		t.Fatalf("fetched %d pages, want 3", calls)
	}
}

func TestFullHistoryAppendsWithoutFetching(t *testing.T) {
	calls := 0
	f := newFullHistory(fakeThread(102, 30, &calls))
	f.merge(tailSnapshot(90, 100, true))
	calls = 0
	// The tree's window moved: its oldest message is known, so only the new ones are added.
	got := f.merge(tailSnapshot(92, 102, true))
	if calls != 0 || len(historyNode(got).Messages) != 102 {
		t.Fatalf("calls %d, messages %d", calls, len(historyNode(got).Messages))
	}
}

func TestFullHistoryRefetchesAfterAGap(t *testing.T) {
	calls := 0
	f := newFullHistory(fakeThread(150, 30, &calls))
	f.merge(tailSnapshot(90, 100, true))
	calls = 0
	// A burst moved the window past every known message: fetch again, never skip.
	got := f.merge(tailSnapshot(140, 150, true))
	if calls == 0 || len(historyNode(got).Messages) != 150 {
		t.Fatalf("calls %d, messages %d", calls, len(historyNode(got).Messages))
	}
}

func TestFullHistoryKeepsTheTreeOnFailure(t *testing.T) {
	f := newFullHistory(func(string) (historyPage, error) { return historyPage{}, errors.New("hub down") })
	s := tailSnapshot(90, 100, true)
	got := f.merge(s)
	if h := historyNode(got); !h.HasMore || len(h.Messages) != 10 {
		t.Fatal("a failed fetch changed the tree")
	}
	// The next snapshot fetches again: nothing was recorded as known.
	if len(f.known) != 0 || !f.gap {
		t.Fatal("a failed fetch recorded messages or no gap")
	}
	// The retry with the same snapshot fetches again once the hub answers.
	calls := 0
	f.fetch = fakeThread(100, 30, &calls)
	if got := f.merge(s); f.gap || len(historyNode(got).Messages) != 100 {
		t.Fatal("the retry did not fill the gap")
	}
}

func TestFullHistoryShortThread(t *testing.T) {
	calls := 0
	f := newFullHistory(fakeThread(5, 30, &calls))
	got := f.merge(tailSnapshot(0, 5, false))
	if calls != 0 || len(historyNode(got).Messages) != 5 {
		t.Fatal("a whole-thread tree fetched pages")
	}
}

// The printer gets the merged thread: a long thread prints every message once.
func TestTranscriptPrintsTheWholeThread(t *testing.T) {
	calls := 0
	f := newFullHistory(fakeThread(100, 30, &calls))
	var out []string
	p := newTranscriptPrinter(nil, func(m Message, _ *Message) string { out = append(out, m.ID); return "" })
	p.out = discard{}
	if err := p.apply(f.merge(tailSnapshot(90, 100, true))); err != nil || len(out) != 100 {
		t.Fatalf("printed %d messages, err %v", len(out), err)
	}
	// One new message: the printer appends it and prints nothing again.
	if err := p.apply(f.merge(tailSnapshot(91, 101, true))); err != nil || len(out) != 101 || out[100] != "100" {
		t.Fatalf("printed %d messages, err %v", len(out), err)
	}
}

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }
