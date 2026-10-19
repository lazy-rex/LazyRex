package transcript

import (
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestGalleryLogicalLines(t *testing.T) {
	long := strings.Repeat("long logical line ", 60) + "END"
	for _, style := range GalleryStyles {
		for _, own := range []bool{false, true} {
			got := FormatGallery(Message{Author: "User", Own: own, At: "12:01", Text: long + "\n\n```go\n  " + long + "\n```"}, nil, style, false)
			plain := ansi.Strip(got)
			if !strings.Contains(plain, long) || !strings.Contains(plain, "  "+long) {
				t.Fatalf("%s changed a logical line", style)
			}
			if strings.Contains(got, "\x1b[K") || strings.Contains(got, "\x1b[2J") {
				t.Fatalf("%s uses terminal clearing", style)
			}
			for _, line := range strings.Split(plain, "\n") {
				if strings.HasSuffix(line, " ") && line != "> " {
					t.Fatalf("%s adds padding", style)
				}
			}
		}
	}
}

func TestGalleryGrouping(t *testing.T) {
	previous := Message{Author: "claude", At: "12:01", Text: "First"}
	for _, style := range []GalleryStyle{GroupedRule, GroupedNames} {
		next := Message{Author: "claude", At: "12:06", Text: "Second"}
		got := ansi.Strip(FormatGallery(next, &previous, style, false))
		if got != "Second\n" {
			t.Fatalf("%s failed grouping: %q", style, got)
		}
		for _, at := range []string{"12:07", "12:00", "unknown"} {
			next.At = at
			if !strings.Contains(ansi.Strip(FormatGallery(next, &previous, style, false)), "claude") {
				t.Fatalf("%s grouped outside the time window", style)
			}
		}
		next.At = "12:02"
		next.Own = true
		if !strings.Contains(ansi.Strip(FormatGallery(next, &previous, style, false)), "claude") {
			t.Fatal("Grouped different sender kinds")
		}
	}
}

func TestGallerySenderColors(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range []Message{{Author: "User", Own: true}, {Author: "Claude"}, {Author: "CODEX"}, {Author: "other"}} {
		color := senderColor(m)
		if seen[color] {
			t.Fatal("Sender colors are not distinct")
		}
		seen[color] = true
		got := FormatGallery(Message{Author: m.Author, Own: m.Own, Text: "body"}, nil, NameChip, false)
		if !strings.Contains(got, rgb(color, true)+m.Author+reset) {
			t.Fatalf("Chip pads or changes its name: %q", got)
		}
	}
}

func TestGalleryFixture(t *testing.T) {
	messages := []Message{
		{Author: "You", Own: true, At: "12:01", Text: "Can we make **each sender** easy to recognize? Keep this long paragraph readable when the pane becomes narrow and then becomes wide again."},
		{Author: "claude", At: "12:02", Text: "## Review\n\nI checked the **Markdown** and *spacing*.\n\n- Keep native wrapping.\n- Keep sender names visible."},
		{Author: "claude", At: "12:03", Text: "This follow-up shares the previous header in the grouped styles."},
		{Author: "codex", At: "12:04", Text: "Here is the call:\n\n```go\n  print(message) // the terminal wraps this source line without adding spaces to its end\n```"},
		{Author: "You", Own: true, At: "12:05", Text: "Good. I prefer a clear name and a quiet message body."},
	}
	var out strings.Builder
	for _, style := range GalleryStyles {
		out.WriteString(style.String() + "\n\n")
		for i, m := range messages {
			var previous *Message
			if i > 0 {
				previous = &messages[i-1]
			}
			out.WriteString(FormatGallery(m, previous, style, false))
		}
		out.WriteString("\n")
	}
	if path := os.Getenv("GALLERY_FIXTURE"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(out.String(), "8 header marker") {
		t.Fatal("Missing gallery style")
	}
}
