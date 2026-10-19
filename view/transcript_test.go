package main

import (
	"strconv"

	"context"
	"fmt"
	"github.com/lazy-rex/LazyRex/view/transcript"
	"strings"
	"testing"
	"time"
)

// writes records each Write call separately.
type writes []string

func (w *writes) Write(b []byte) (int, error) { *w = append(*w, string(b)); return len(b), nil }

func idFormat(m Message, _ *Message) string { return "[" + m.ID + ":" + m.Text + "]" }

func historySnapshot(hasMore bool, ids ...string) Snapshot {
	var msgs []Message
	for _, id := range ids {
		msgs = append(msgs, Message{ID: id, Author: "codex", Text: "t" + id})
	}
	return Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/thread/42", Revision: "1", Tree: Tree{Nodes: []Node{
		{ID: "history", Type: "history", Messages: msgs, HasMore: hasMore, PageSize: max(1, len(msgs))},
	}}}
}

func printerFor(t *testing.T) (*transcriptPrinter, *writes) {
	t.Helper()
	w := &writes{}
	return newTranscriptPrinter(w, idFormat), w
}

func TestTranscriptFirstSnapshotClearsThenPrintsInOneWrite(t *testing.T) {
	p, w := printerFor(t)
	if err := p.apply(historySnapshot(false, "1", "2")); err != nil {
		t.Fatal(err)
	}
	if len(*w) != 1 || (*w)[0] != clearScrollback+"[1:t1][2:t2]" {
		t.Fatalf("writes=%q", *w)
	}
}

func TestTranscriptRepeatedSnapshotAndReconnectPrintNothing(t *testing.T) {
	p, w := printerFor(t)
	p.apply(historySnapshot(false, "1", "2"))
	// The same snapshot arrives again, for example after a hub restart and reattach.
	p.apply(historySnapshot(false, "1", "2"))
	if len(*w) != 1 {
		t.Fatalf("A repeated snapshot printed again: %q", *w)
	}
}

func TestTranscriptAppendsOnlyNewMessages(t *testing.T) {
	p, w := printerFor(t)
	p.apply(historySnapshot(false, "1", "2"))
	p.apply(historySnapshot(false, "1", "2", "3", "4"))
	if len(*w) != 2 || (*w)[1] != "[3:t3][4:t4]" {
		t.Fatalf("writes=%q", *w)
	}
}

func TestTranscriptSlidingWindowAppendsWithoutRebuild(t *testing.T) {
	p, w := printerFor(t)
	p.apply(historySnapshot(true, "1", "2", "3"))
	// The window keeps three messages: 1 leaves, 4 arrives. The last printed (3) remains.
	p.apply(historySnapshot(true, "2", "3", "4"))
	if len(*w) != 2 || (*w)[1] != "[4:t4]" {
		t.Fatalf("writes=%q", *w)
	}
}

func TestTranscriptRebuildsWhenTheWindowMovedPastTheLastPrintedMessage(t *testing.T) {
	p, w := printerFor(t)
	p.apply(historySnapshot(true, "1", "2", "3"))
	// Messages 4..6 arrived between snapshots; 4 is outside the window. Continuity is unknown.
	p.apply(historySnapshot(true, "5", "6", "7"))
	if len(*w) != 2 || (*w)[1] != clearScrollback+olderMarker+"[5:t5][6:t6][7:t7]" {
		t.Fatalf("writes=%q", *w)
	}
}

func TestTranscriptRebuildsChronologicallyWhenOlderMessagesLoad(t *testing.T) {
	p, w := printerFor(t)
	p.apply(historySnapshot(true, "3", "4"))
	if !strings.Contains((*w)[0], olderMarker) {
		t.Fatalf("No older-messages marker: %q", (*w)[0])
	}
	p.apply(historySnapshot(false, "1", "2", "3", "4"))
	if len(*w) != 2 || (*w)[1] != clearScrollback+"[1:t1][2:t2][3:t3][4:t4]" {
		t.Fatalf("writes=%q", *w)
	}
}

func TestTranscriptIgnoresLoadingSnapshots(t *testing.T) {
	p, w := printerFor(t)
	p.apply(historySnapshot(false, "1"))
	loading := Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/thread/42", Revision: "2", Tree: Tree{Nodes: []Node{{ID: "loading", Type: "loading", Text: "Loading"}}}}
	p.apply(loading)
	p.apply(historySnapshot(false, "1", "2"))
	if len(*w) != 2 || (*w)[1] != "[2:t2]" {
		t.Fatalf("A loading snapshot changed the transcript: %q", *w)
	}
}

func TestTranscriptEmptyThreadThenFirstMessage(t *testing.T) {
	p, w := printerFor(t)
	p.apply(historySnapshot(false))
	p.apply(historySnapshot(false, "1"))
	if len(*w) != 2 || (*w)[0] != clearScrollback || (*w)[1] != "[1:t1]" {
		t.Fatalf("writes=%q", *w)
	}
}

func TestTranscriptRemovesTerminalControlsFromMessageText(t *testing.T) {
	p, w := printerFor(t)
	s := historySnapshot(false, "1")
	s.Tree.Nodes[0].Messages[0].Text = "a\x1b[3Jb\x07c"
	s.Tree.Nodes[0].Messages[0].Author = "x\x1b]0;title\x07"
	var author string
	p.format = func(m Message, _ *Message) string { author = m.Author; return m.Text }
	p.apply(s)
	if (*w)[0] != clearScrollback+"a[3Jbc" || strings.ContainsRune(author, 0x1b) {
		t.Fatalf("writes=%q author=%q", *w, author)
	}
}

func TestTranscriptPassesThePreviousPrintedMessage(t *testing.T) {
	p, w := printerFor(t)
	p.format = func(m Message, previous *Message) string {
		if previous == nil {
			return m.ID + "<-nil "
		}
		return fmt.Sprintf("%s<-%s ", m.ID, previous.ID)
	}
	p.apply(historySnapshot(false, "1", "2"))
	p.apply(historySnapshot(false, "1", "2", "3"))
	if got := strings.Join(*w, ""); got != clearScrollback+"1<-nil 2<-1 3<-2 " {
		t.Fatalf("got %q", got)
	}
}

func TestTranscriptRestyleReprintsTheWindow(t *testing.T) {
	p, w := printerFor(t)
	s := historySnapshot(false, "1", "2")
	p.apply(s)
	p.restyle(s, func(m Message, _ *Message) string { return "<" + m.ID + ">" })
	if len(*w) != 2 || (*w)[1] != clearScrollback+"<1><2>" {
		t.Fatalf("writes=%q", *w)
	}
}

func TestPrescrollMakesRoomBeforeWrappedBackgroundLines(t *testing.T) {
	bg := "\x1b[48;2;40;48;64m"
	long := bg + strings.Repeat("x", 25) + "\x1b[0m\n" // 25 cells: 3 rows at 10 columns
	short := bg + "short\x1b[0m\n"
	plainLong := strings.Repeat("y", 25) + "\n"
	got := string(prescroll([]byte("You\n"+long+short+plainLong), 10, 24))
	want := "You\n" + "\n\n\x1b[2A" + long + short + plainLong
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// A line taller than the pane cannot be prepared: write it unchanged.
	if got := string(prescroll([]byte(long), 10, 2)); got != long {
		t.Fatalf("A too-tall line changed: %q", got)
	}
	// Exactly one row wide: no extra rows.
	exact := bg + strings.Repeat("z", 10) + "\x1b[0m\n"
	if got := string(prescroll([]byte(exact), 10, 24)); got != exact {
		t.Fatalf("A full-width line changed: %q", got)
	}
}

func TestTranscriptWritePrescrollsWithThePaneSize(t *testing.T) {
	p, w := printerFor(t)
	p.format = func(m Message, _ *Message) string { return "\x1b[48;5;1m" + strings.Repeat("x", 15) + "\x1b[0m\n" }
	p.size = func() (int, int) { return 10, 24 }
	p.apply(historySnapshot(false, "1"))
	if want := clearScrollback + "\n\x1b[1A\x1b[48;5;1m" + strings.Repeat("x", 15) + "\x1b[0m\n"; (*w)[0] != want {
		t.Fatalf("got %q want %q", (*w)[0], want)
	}
}

// failing fails the next n writes.
type failing struct {
	writes
	fail int
}

func (f *failing) Write(b []byte) (int, error) {
	if f.fail > 0 {
		f.fail--
		return 0, fmt.Errorf("write failed")
	}
	return f.writes.Write(b)
}

func TestTranscriptFailedWriteKeepsMessagesUnprinted(t *testing.T) {
	f := &failing{}
	p := newTranscriptPrinter(f, idFormat)
	p.apply(historySnapshot(false, "1"))
	f.fail = 1
	if err := p.apply(historySnapshot(false, "1", "2")); err == nil {
		t.Fatal("No error from a failed write")
	}
	p.apply(historySnapshot(false, "1", "2"))
	if len(f.writes) != 2 || f.writes[1] != "[2:t2]" {
		t.Fatalf("An append after a failed write: %q", f.writes)
	}
	// A failed rebuild keeps the old ledger, so the next snapshot rebuilds again.
	f.fail = 1
	p.apply(historySnapshot(false, "0", "1", "2"))
	p.apply(historySnapshot(false, "0", "1", "2"))
	if len(f.writes) != 3 || f.writes[2] != clearScrollback+"[0:t0][1:t1][2:t2]" {
		t.Fatalf("A rebuild after a failed write: %q", f.writes)
	}
}

type short struct{ writes }

func (s *short) Write(b []byte) (int, error) { return len(b) - 1, nil }

func TestTranscriptShortWriteIsAnError(t *testing.T) {
	p := newTranscriptPrinter(&short{}, idFormat)
	if err := p.apply(historySnapshot(false, "1")); err == nil {
		t.Fatal("A short write was accepted")
	}
}

func TestTranscriptInputConsumesEscapeSequences(t *testing.T) {
	in := "a" + // a style key
		"\x1b[I" + // focus in
		"\x1b]11;rgb:0000/0000/0000\x07" + // OSC reply with letters
		"\x1b]52;c;Y2Rk\x1b\\" + // OSC with ST
		"\x1b[200~cd o\x1b[201~" + // bracketed paste with key letters
		"\x1b[1;5A" + // CSI with parameters
		"\x1bOd" + // SS3
		"\x1b[?997;2n" + // scheme report: light
		"\x1b[O" + // focus out
		"c"
	keys := make(chan byte, 32)
	focus := make(chan bool, 8)
	themes := make(chan themeReport, 8)
	readTranscriptInput(context.Background(), strings.NewReader(in), keys, focus, themes)
	close(keys)
	close(focus)
	close(themes)
	var gotThemes []themeReport
	for r := range themes {
		gotThemes = append(gotThemes, r)
	}
	if fmt.Sprint(gotThemes) != "[{true true} {false false}]" {
		t.Fatalf("themes=%v", gotThemes)
	}
	var gotKeys []byte
	for k := range keys {
		gotKeys = append(gotKeys, k)
	}
	var gotFocus []bool
	for f := range focus {
		gotFocus = append(gotFocus, f)
	}
	if string(gotKeys) != "ac" || fmt.Sprint(gotFocus) != "[true false]" {
		t.Fatalf("keys=%q focus=%v", gotKeys, gotFocus)
	}
}

func TestCRLFWriterReportsAShortWrite(t *testing.T) {
	if _, err := (crlfWriter{&short{}}).Write([]byte("a\nb")); err == nil {
		t.Fatal("A short write was accepted")
	}
	var w writes
	if n, err := (crlfWriter{&w}).Write([]byte("a\nb")); err != nil || n != 3 || w[0] != "a\r\nb" {
		t.Fatalf("n=%d err=%v writes=%q", n, err, w)
	}
}

func TestTranscriptBannerFollowsTheClear(t *testing.T) {
	p, w := printerFor(t)
	p.banner = "B\n"
	p.apply(historySnapshot(true, "1"))
	if (*w)[0] != clearScrollback+"B\n"+olderMarker+"[1:t1]" {
		t.Fatalf("writes=%q", *w)
	}
}

func TestOSC11Theme(t *testing.T) {
	for payload, want := range map[string]string{
		"11;rgb:1e1e/1e1e/1e1e": "dark",  // Rex with a dark theme (observed)
		"11;rgb:ffff/ffff/ffff": "light", // Rex with a light theme (observed)
		"11;rgb:00/00/00":       "dark",
		"11;rgb:f/f/f":          "light",
		"11;rgb:6666/6666/6666": "dark",  // dark gray: white text has more contrast
		"11;rgb:9999/9999/9999": "light", // light gray: black text has more contrast
		"11;rgb:zz/00/00":       "none",
		"11;rgb:00/00":          "none",
		"10;rgb:00/00/00":       "none", // OSC 10 is the foreground
		"11;?":                  "none",
	} {
		got := "none"
		if r, ok := parseOSC11(payload); ok {
			got = map[bool]string{true: "dark", false: "light"}[r.dark]
		}
		if got != want {
			t.Errorf("%q: got %s, want %s", payload, got, want)
		}
	}
}

func TestAwaitThemePrefersTheBackground(t *testing.T) {
	reports := make(chan themeReport, 4)
	// Rex with a light theme set: the OSC 11 answer, then the DSR 996 answer.
	reports <- themeReport{dark: false, osc11: true}
	reports <- themeReport{dark: true}
	if awaitTheme(reports, time.Second) {
		t.Fatal("the scheme answer replaced the background")
	}
	// Rex with no theme set (observed in the lab): only the scheme answer.
	reports <- themeReport{dark: true}
	if !awaitTheme(reports, time.Second) {
		t.Fatal("the scheme answer was ignored")
	}
	// No answer: light.
	if awaitTheme(reports, 10*time.Millisecond) {
		t.Fatal("no answer must give light")
	}
}

func TestGalleryBannerNamesTheTheme(t *testing.T) {
	if b := galleryBanner(8, 10, "9 header prompt", true, true); !strings.Contains(b, "Style 9 of 10: 9 header prompt · dark (forced) · ") {
		t.Fatalf("banner=%q", b)
	}
	if b := galleryBanner(0, 10, "x", false, false); !strings.Contains(b, " · light · s next") {
		t.Fatalf("banner=%q", b)
	}
}

func TestTranscriptStylePassesTheTheme(t *testing.T) {
	style, err := parseTranscriptStyle("9")
	if err != nil || style.gallery != 9 {
		t.Fatalf("style=%+v err=%v", style, err)
	}
	m := Message{ID: "1", Author: "You", Own: true, Text: "hello"}
	light, dark := formatTranscript(style, false)(m, nil), formatTranscript(style, true)(m, nil)
	if light == dark {
		t.Fatal("the dark palette is not used")
	}
	for _, bad := range []string{"0", strconv.Itoa(len(transcript.GalleryStyles) + 1), "e"} {
		if _, err := parseTranscriptStyle(bad); err == nil {
			t.Errorf("style %q was accepted", bad)
		}
	}
}
