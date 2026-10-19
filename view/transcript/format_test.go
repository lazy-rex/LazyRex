package transcript

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestLogicalLines(t *testing.T) {
	long := strings.Repeat("words ", 100) + "END"
	text := long + "\nsource soft break\n\nexplicit  \nbreak\n\n```text\n  code " + long + "\n```"
	got := ansi.Strip(Format(Message{Author: "Agent", Text: text}, nil, Foreground))
	if !strings.Contains(got, long+" source soft break") {
		t.Fatalf("Soft line changed: %q", got)
	}
	if !strings.Contains(got, "explicit\nbreak") {
		t.Fatalf("Hard break lost: %q", got)
	}
	if !strings.Contains(got, "  code "+long) {
		t.Fatalf("Code line changed: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") || strings.HasSuffix(got, "\n\n\n") {
		t.Fatal("Expected one message gap")
	}
}

func TestTableRecords(t *testing.T) {
	m := Message{Author: "Agent", Text: "Before\n\n| **Name** | Value |\n| --- | --- |\n| A\\|B | **bold** and [link](https://example.test) |\n| C | `code` |\n\nAfter"}
	got := Format(m, nil, Foreground)
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
	got := Format(m, nil, TextBackground)
	sgr := regexp.MustCompile(`\x1b\[[0-9;:]*m`)
	clean := sgr.ReplaceAllString(got, "")
	if strings.ContainsAny(clean, "\x1b\a\r\u009b") {
		t.Fatalf("Terminal control survived: %q", got)
	}
	if !strings.Contains(clean, "safecontent") {
		t.Fatalf("Text lost: %q", clean)
	}
}

func TestUserStyles(t *testing.T) {
	for _, style := range []Style{TextBackground, BarBackground, Foreground} {
		got := Format(Message{ID: "1", Author: "User", At: "12:01", Own: true, Text: "plain **bold** after\n\n`code`"}, nil, style)
		if strings.Contains(got, "\x1b[K") {
			t.Fatal("Erase-to-end fill")
		}
		if (strings.Count(got, "▌") == 1) != (style == BarBackground) {
			t.Fatal("First-row bar mismatch")
		}
		if strings.Contains(got, "48;2;40;48;64") != (style != Foreground) {
			t.Fatalf("Background mismatch: %q", got)
		}
		if !strings.Contains(ansi.Strip(got), "plain bold after") {
			t.Fatalf("Body lost: %q", got)
		}
	}
}

func TestFixture(t *testing.T) {
	text := "# Review\n\n" + strings.Repeat("One logical paragraph with **emphasis** and `code`. ", 6) + "END\n\n- First\n- Second\n\n> Quote\n\n```go\n  fmt.Println(\"long code that remains a single logical line and wraps only in the terminal\")\n```\n\n| Mode | Wheel |\n| --- | --- |\n| None | native |\n| Cell | captured |"
	var out strings.Builder
	for _, style := range []Style{TextBackground, BarBackground, Foreground} {
		out.WriteString(Format(Message{Author: "User", At: "12:01", Text: text, Own: true}, nil, style))
		out.WriteString(Format(Message{Author: "Codex", At: "12:02", Text: text}, nil, style))
	}
	if path := os.Getenv("TRANSCRIPT_FIXTURE"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/mixed.ansi")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Fatal("Mixed ANSI fixture changed")
	}
}
