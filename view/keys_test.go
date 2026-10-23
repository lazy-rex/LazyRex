package main

import (
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// The composer's keys (keys.go) through Bubble Tea's own input decoder: the test writes a
// terminal's bytes into the program's input, as Rex does for a pane.

// typeInto starts a thread view with a composer, writes each chunk of bytes as one input write,
// and returns the texts of the hub.view.send requests (draft saves succeed).
func typeInto(t *testing.T, chunks ...string) []string {
	t.Helper()
	in, w := io.Pipe()
	r := startPrintInput(t, &testTerm{}, nil, in)
	t.Cleanup(func() { w.Close() })
	r.p.Send(page(msgs(1), false, 1))
	r.wait("the thread frame", func(s probeState) bool { return s.mode == viewThread && s.printed["m1"] })
	for _, c := range chunks {
		if _, err := w.Write([]byte(c)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond) // one chunk is one read: ESC CR stays one key
	}
	var sent []string
	rev := 1
	for quiet := time.After(time.Second); ; {
		select {
		case req := <-r.m.client.Requests:
			rev++
			result := map[string]any{"draft_revision": strconv.Itoa(rev)}
			if req.Method == "hub.view.send" {
				sent = append(sent, req.Args["text"].(string))
				result["message_id"] = "m9001"
			} else {
				result["text"] = req.Args["text"]
			}
			r.p.Send(completed{req, operationResult{Status: "succeeded", Result: result}})
			quiet = time.After(500 * time.Millisecond) // a second send would come within this time
		case <-quiet:
			r.stop()
			return sent
		}
	}
}

// TestNewlineKeysLegacyBytes (codex-29): without key disambiguation, Alt+Enter is ESC CR and Ctrl+J
// is LF; both insert a newline. ESC CR must not become an Enter send. CR (Enter) sends once.
func TestNewlineKeysLegacyBytes(t *testing.T) {
	sent := typeInto(t, "a", "\x1b\r", "b", "\n", "c", "\r")
	if !slices.Equal(sent, []string{"a\nb\nc"}) {
		t.Fatalf("legacy keys sent %q, want one send of %q", sent, "a\nb\nc")
	}
}

// TestNewlineKeysEnhancedBytes: with key disambiguation (Rex answers CSI ?1u), Shift+Enter is
// CSI 13;2u and Alt+Enter is CSI 13;3u; both insert a newline. CR sends once.
func TestNewlineKeysEnhancedBytes(t *testing.T) {
	sent := typeInto(t, "a", "\x1b[13;2u", "b", "\x1b[13;3u", "c", "\r")
	if !slices.Equal(sent, []string{"a\nb\nc"}) {
		t.Fatalf("enhanced keys sent %q, want one send of %q", sent, "a\nb\nc")
	}
}

// TestComposerHintComesFromTheKeyList: the hint names the first newline key, which the editor
// binds, and the frame keeps its accepted rows at 80 and at 40 columns.
func TestComposerHintComesFromTheKeyList(t *testing.T) {
	bound := newlineBinding().Keys()
	for _, k := range newlineKeys {
		if !slices.Contains(bound, k.key) {
			t.Fatalf("the editor does not bind %s", k.key)
		}
	}
	if hint, _ := composerHint(80); !strings.Contains(hint, newlineKeys[0].name) {
		t.Fatalf("the hint %q does not name %s", hint, newlineKeys[0].name)
	}
	m := testModel()
	for _, tc := range []struct {
		width int
		rows  []string
	}{
		{80, []string{"Enter send · Shift+Enter newline"}},
		{40, []string{"Enter send", "Shift+Enter ↵"}},
	} {
		m.width = tc.width
		frame := ansi.Strip(m.View().Content)
		for _, row := range tc.rows {
			if !strings.Contains(frame, row) {
				t.Fatalf("%d columns: the frame has no %q:\n%s", tc.width, row, frame)
			}
		}
	}
}
