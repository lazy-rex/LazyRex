package transcript

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// boldCells reads the generated SGR attributes at each visible byte.
func boldCells(value string) (string, []bool) {
	escape := regexp.MustCompile(`\x1b\[([0-9;]*)m`)
	var text strings.Builder
	var cells []bool
	bold, at := false, 0
	appendText := func(s string) {
		text.WriteString(s)
		for range []byte(s) {
			cells = append(cells, bold)
		}
	}
	for _, m := range escape.FindAllStringSubmatchIndex(value, -1) {
		appendText(value[at:m[0]])
		params := strings.Split(value[m[2]:m[3]], ";")
		for i := 0; i < len(params); i++ {
			n, _ := strconv.Atoi(params[i])
			switch n {
			case 0, 22:
				bold = false
			case 1:
				bold = true
			case 38, 48:
				if i+1 < len(params) {
					if params[i+1] == "2" {
						i += 4
					} else if params[i+1] == "5" {
						i += 2
					}
				}
			}
		}
		at = m[1]
	}
	appendText(value[at:])
	return text.String(), cells
}

func TestBoldUserBodyAndListsKeepCodeNormal(t *testing.T) {
	m := Message{Author: "You", Own: true, Text: "ParagraphWord\n\n- ListWord\n  - NestedWord\n\n1. OrderedWord\n\n```go\n  CodeWord()\n```\n\nAfterWord"}
	for _, dark := range []bool{false, true} {
		got := Format(m, nil, dark)
		plain, cells := boldCells(got)
		for word, want := range map[string]bool{"ParagraphWord": true, "ListWord": true, "NestedWord": true, "OrderedWord": true, "AfterWord": true, "CodeWord": false} {
			at := strings.Index(plain, word)
			if at < 0 {
				t.Fatalf("Missing %s", word)
			}
			for _, bold := range cells[at : at+len(word)] {
				if bold != want {
					t.Fatalf("dark=%t %s bold=%t, want %t; output=%q", dark, word, bold, want, got)
				}
			}
		}
		if strings.Contains(got, "48;2;") || strings.Contains(ansi.Strip(got), "> ") {
			t.Fatal("A user body has a background or prompt")
		}
		m.Own = false
		if plain, cells := boldCells(Format(m, nil, dark)); cells[strings.Index(plain, "ParagraphWord")] {
			t.Fatal("An agent body is bold")
		}
		m.Own = true
	}
}
