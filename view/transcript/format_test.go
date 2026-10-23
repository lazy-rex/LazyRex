package transcript

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestLogicalLines(t *testing.T) {
	long := strings.Repeat("words ", 100) + "END"
	text := long + "\nsource soft break\n\nexplicit  \nbreak\n\n```text\n  code " + long + "\n```"
	got := ansi.Strip(Format(Message{Author: "Agent", Text: text}, nil, false))
	if !strings.Contains(got, long+" source soft break") {
		t.Fatalf("Soft line changed: %q", got)
	}
	if !strings.Contains(got, "explicit\nbreak") {
		t.Fatalf("Hard break lost: %q", got)
	}
	if !strings.Contains(got, "  code "+long) {
		t.Fatalf("Code line changed: %q", got)
	}
	if !strings.HasSuffix(got, "END\n") {
		t.Fatal("Expected one final newline")
	}
	for _, own := range []bool{false, true} {
		got := Format(Message{Author: "User", Own: own, At: "12:01", Text: text}, nil, false)
		if strings.Contains(got, "\x1b[K") || strings.Contains(got, "\x1b[2J") {
			t.Fatal("The formatter uses terminal clearing")
		}
		for _, line := range strings.Split(ansi.Strip(got), "\n") {
			if strings.HasSuffix(line, " ") {
				t.Fatalf("Padding: %q", line)
			}
		}
	}
}

func TestTableRecords(t *testing.T) {
	m := Message{Author: "Agent", Text: "Before\n\n| **Name** | Value |\n| --- | --- |\n| A\\|B | **bold** and [link](https://example.test) |\n| C | `code` |\n\nAfter"}
	got := Format(m, nil, false)
	plain := ansi.Strip(got)
	if !strings.Contains(plain, "Name: A|B · Value: bold and") || !strings.Contains(plain, "Name: C · Value: code") {
		t.Fatalf("Cells lost: %q", plain)
	}
	if !strings.Contains(plain, "https://example.test") {
		t.Fatalf("Link lost: %q", plain)
	}
	if strings.Index(plain, "Before") > strings.Index(plain, "Name: A") || strings.Index(plain, "After") < strings.Index(plain, "Name: C") {
		t.Fatal("Table order changed")
	}
	for _, line := range strings.Split(plain, "\n") {
		if strings.HasSuffix(line, " ") {
			t.Fatalf("Padding: %q", line)
		}
	}
	if strings.ContainsAny(plain, "│─┼") {
		t.Fatalf("Table box retained: %q", plain)
	}
}

func TestControlSanitization(t *testing.T) {
	m := Message{Author: "Name\x1b[2J\nNext", At: "now\x07", Text: "safe\x1b[2J\x1b]52;c;c2VjcmV0\a\rcontent &#27;[2J &#x9b;2J &#7; ~~strike~~"}
	got := Format(m, nil, false)
	sgr := regexp.MustCompile(`\x1b\[[0-9;:]*m`)
	clean := sgr.ReplaceAllString(got, "")
	if strings.ContainsAny(clean, "\x1b\a\r\u009b") {
		t.Fatalf("Terminal control survived: %q", got)
	}
	if !strings.Contains(clean, "safecontent") {
		t.Fatalf("Text lost: %q", clean)
	}
}

// TestFormatGap checks the blank line before a header that follows another message.
func TestFormatGap(t *testing.T) {
	m := Message{Author: "claude", At: "12:06", Text: "Second"}
	first, next := ansi.Strip(Format(m, nil, false)), ansi.Strip(Format(m, &m, false))
	if first != "▍ claude · 12:06\nSecond\n" || next != "\n"+first {
		t.Fatalf("first=%q next=%q", first, next)
	}
}

func TestSenderColors(t *testing.T) {
	for _, dark := range []bool{false, true} {
		p := newPalette(dark)
		seen := map[string]bool{}
		for _, m := range []Message{{Author: "Claude", Own: true}, {Author: "CLAUDE"}, {Author: "codex"}, {Author: "other"}} {
			if seen[p.sender(m)] {
				t.Fatalf("dark=%t: sender colors are not distinct", dark)
			}
			seen[p.sender(m)] = true
		}
		if p.sender(Message{Author: " You "}) != p.user {
			t.Fatal("A user name is not blue")
		}
	}
}
