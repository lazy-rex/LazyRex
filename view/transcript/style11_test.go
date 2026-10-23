package transcript

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

var updateStyle11 = flag.Bool("update-style11", false, "rewrite testdata/style11/*.golden")

// style11 is the renderer's transcript style (style 11, "header + bold user"). It is the only call
// of the formatter in this test, so a rename changes one line and the golden files stay.
func style11(m Message, previous *Message, dark bool) string {
	return Format(m, previous, dark)
}

// TestStyle11Golden pins the exact bytes of style 11 for both palettes: a first message and a
// following one (the gap), a grouped follow-up, own and agent headers with each sender color, FYI,
// an empty body, and Markdown (emphasis, inline code, a list and a code block).
func TestStyle11Golden(t *testing.T) {
	markdown := "Some *emphasis*, **strong** and `code`.\n\n- one\n- two\n\n```go\nfmt.Println(\"x\")\n```"
	thread := []Message{
		{ID: "1", Author: "User", Own: true, At: "12:01", Text: "First message from me."},
		{ID: "2", Author: "Claude", At: "12:02", Text: markdown},
		{ID: "3", Author: "Claude", At: "12:03", Text: "A grouped follow-up."},
		{ID: "4", Author: "Codex", At: "12:04", Text: "Noted.", FYI: true},
		{ID: "5", Author: "Helper", At: "12:05", Text: ""},
		{ID: "6", Author: "User", Own: true, At: "12:06", Text: markdown},
		{ID: "7", Author: "you", At: "12:07", Text: "A user message that is not mine."},
	}
	for _, dark := range []bool{false, true} {
		var out []byte
		for i, m := range thread {
			var previous *Message
			if i > 0 {
				previous = &thread[i-1]
			}
			out = append(out, "--- message "+m.ID+" previous "+strconv.FormatBool(previous != nil)+"\n"...)
			out = append(out, style11(m, previous, dark)...)
		}
		name := "light.golden"
		if dark {
			name = "dark.golden"
		}
		path := filepath.Join("testdata", "style11", name)
		if *updateStyle11 {
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, out, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(want) {
			t.Fatalf("style 11 (%s) differs from %s:\n%q\nwant\n%q", name, path, out, want)
		}
	}
}
