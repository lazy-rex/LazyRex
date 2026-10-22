package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	noHubText    = "No LazyRex hub is connected. To start one, use LazyRex: Start Hub in the palette; if the entry is missing, run lazyrex-hub setup again. This pane retries the connection automatically."
	twoHubsText  = "2 LazyRex hubs are connected. This pane needs exactly one."
	unavailable  = "Hub unavailable: "
	resolveEmpty = "Unexpected target.resolve reply"
)

// waitStatus waits for a connection message that is not ready and returns its text.
func waitStatus(t *testing.T, msgs <-chan tea.Msg) string {
	t.Helper()
	var text string
	waitMessage(t, msgs, func(msg tea.Msg) bool {
		s, ok := msg.(connectionMsg)
		text = s.Text
		return ok && !s.Ready
	})
	return text
}

func resolveResult(status string, ids ...string) map[string]any {
	r := map[string]any{"query": "lazyrex-hub", "status": status}
	if status == "resolved" {
		r["target"] = map[string]string{"client_id": ids[0]}
	} else if len(ids) > 0 {
		var c []any
		for _, id := range ids {
			c = append(c, map[string]string{"client_id": id})
		}
		r["candidates"] = c
	}
	return map[string]any{"results": []any{r}}
}

// A pane without a hub says so, and how to start one. It retries, and attaches when a hub comes.
// Rex answers not_found with no candidates when no client has the label (lab, 2026-10-22).
func TestNoHubSaysHowToStartOne(t *testing.T) {
	f := &protocolFixture{resolveAnswer: resolveResult("not_found")}
	_, msgs := startFixture(t, f)
	if got := waitStatus(t, msgs); got != noHubText {
		t.Fatalf("status %q, want %q", got, noHubText)
	}
	// A hub that is not connected is not a hub.
	f.mu.Lock()
	f.resolveAnswer = resolveResult("resolved", "old-hub")
	f.mu.Unlock()
	if got := waitStatus(t, msgs); got != noHubText {
		t.Fatalf("a lost hub: status %q", got)
	}
	f.mu.Lock()
	f.resolveAnswer = nil
	f.mu.Unlock()
	waitMessage(t, msgs, func(msg tea.Msg) bool { s, ok := msg.(connectionMsg); return ok && s.Ready && s.Text == "Connected" })
}

func TestTwoHubsSayThePaneNeedsOne(t *testing.T) {
	f := &protocolFixture{resolveAnswer: resolveResult("ambiguous", "hub", "hub2")}
	_, msgs := startFixture(t, f)
	if got := waitStatus(t, msgs); got != twoHubsText {
		t.Fatalf("status %q, want %q", got, twoHubsText)
	}
}

// Only a complete discovery gives a count. Any other failure keeps the ordinary text.
func TestDiscoveryFailuresAreNotACount(t *testing.T) {
	for _, c := range []struct {
		name    string
		resolve any
		inspect map[string]string
		want    string
	}{
		{"resolve error", &wireError{Code: "internal", Message: "resolve broke"}, nil, unavailable + "internal: resolve broke"},
		{"failed inspection", resolveResult("ambiguous", "hub", "hub2"), map[string]string{"hub2": "fail"}, unavailable + "Cannot inspect the hub client hub2: not_found: No client hub2"},
		{"failed inspection of the only one", resolveResult("resolved", "hub"), map[string]string{"hub": "fail"}, unavailable + "Cannot inspect the hub client hub: not_found: No client hub"},
		{"no result", map[string]any{"results": []any{}}, nil, unavailable + resolveEmpty + ": 0 results"},
		{"unknown status", resolveResult("pending"), nil, unavailable + resolveEmpty + `: status "pending"`},
		{"resolved without a client", map[string]any{"results": []any{map[string]any{"status": "resolved"}}}, nil, unavailable + resolveEmpty + ": a target without a client ID"},
		// codex-24's review of d5b4f4f: an incomplete reply is not a count of 0.
		{"ambiguous without candidates", resolveResult("ambiguous"), nil, unavailable + resolveEmpty + ": ambiguous without candidates"},
		{"a candidate without a client ID", map[string]any{"results": []any{map[string]any{"status": "ambiguous", "candidates": []any{map[string]string{"client_id": "hub"}, map[string]string{}}}}}, nil, unavailable + resolveEmpty + ": a candidate without a client ID"},
		{"not_found with candidates", resolveResult("not_found", "hub"), nil, unavailable + resolveEmpty + ": not_found with candidates"},
		{"an empty connection state", resolveResult("resolved", "hub"), map[string]string{"hub": ""}, unavailable + "Cannot inspect the hub client hub: the reply has no connection state"},
		{"an inspection without the client", resolveResult("ambiguous", "hub", "hub2"), map[string]string{"hub2": "no client"}, unavailable + "Cannot inspect the hub client hub2: the reply has no connection state"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &protocolFixture{resolveAnswer: c.resolve, inspect: c.inspect}
			_, msgs := startFixture(t, f)
			if got := waitStatus(t, msgs); got != c.want {
				t.Fatalf("status %q, want %q", got, c.want)
			}
		})
	}
}

// Error text from Rex reaches the pane only without control characters.
func TestConnectionStatusHasNoControls(t *testing.T) {
	f := &protocolFixture{resolveAnswer: &wireError{Code: "internal", Message: "a\x1b[2Jb\tc\u009bd\x7fe\nf"}}
	_, msgs := startFixture(t, f)
	if got, want := waitStatus(t, msgs), unavailable+"internal: a?[2Jb\tc?d?e?f"; got != want {
		t.Fatalf("status %q, want %q", got, want)
	}
}

func TestPaneSafe(t *testing.T) {
	if got := paneSafe("ok\t\x00\x1f\x7f\u0080\u009f é\xff"); got != "ok\t????? é?" {
		t.Fatalf("%q", got)
	}
}

// The footer notice is one row. In a 38-column pane its first sentence still shows whole.
func TestNoHubNoticeInANarrowPane(t *testing.T) {
	m := testModel()
	m.ready = false
	m.Update(connectionMsg{Text: noHubText})
	m.Update(tea.WindowSizeMsg{Width: 38, Height: 12})
	var notice string
	for _, line := range strings.Split(m.View().Content, "\n") {
		if plain := ansi.Strip(line); strings.HasPrefix(strings.TrimSpace(plain), "No LazyRex") {
			notice = plain
		}
	}
	if !strings.Contains(notice, "No LazyRex hub is connected.") || ansi.StringWidth(notice) > 38 {
		t.Fatalf("notice row %q (%d columns)", notice, ansi.StringWidth(notice))
	}
}
