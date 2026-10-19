package transcript

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func luminance(hex string) float64 {
	n, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 24)
	linear := func(v uint64) float64 {
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(n>>16) + 0.7152*linear(n>>8&255) + 0.0722*linear(n&255)
}

func TestGalleryContrast(t *testing.T) {
	for _, dark := range []bool{false, true} {
		p := galleryPalette(dark)
		check := func(label, fg, bg string, minimum float64) {
			a, b := luminance(fg), luminance(bg)
			if a < b {
				a, b = b, a
			}
			ratio := (a + 0.05) / (b + 0.05)
			t.Logf("dark=%t %s %s on %s: %.2f:1 (minimum %.1f)", dark, label, fg, bg, ratio, minimum)
			if ratio < minimum {
				t.Errorf("Insufficient contrast: %s %.2f", label, ratio)
			}
		}
		check("body", p.body, p.background, 4.5)
		check("style 11 bold user body and lists", p.body, p.background, 4.5)
		check("time and code", p.dim, p.background, 4.5)
		check("user background body", p.body, p.userBackground, 4.5)
		check("user background code", p.dim, p.userBackground, 4.5)
		for _, m := range []Message{{Own: true}, {Author: "claude"}, {Author: "codex"}, {Author: "other"}} {
			check("bold name", p.sender(m), p.background, 3)
			check("chip name", "#FFFFFF", senderColor(m), 3)
		}
		check("user tint and prompt", p.user, p.background, 4.5)
	}
}

func TestGalleryPrompts(t *testing.T) {
	text := "First paragraph\n\nSecond paragraph\n\n```go\n  one()\n  two()\n```"
	for _, dark := range []bool{false, true} {
		for _, style := range []GalleryStyle{HeaderPrompt, HeaderPromptBackground} {
			m := Message{Author: "You", Own: true, Text: text}
			got := FormatGallery(m, nil, style, dark)
			lines := strings.Split(strings.TrimSuffix(ansi.Strip(got), "\n"), "\n")
			for _, line := range lines[1:] {
				if !strings.HasPrefix(line, "> ") {
					t.Fatalf("Unprefixed logical line: %q", line)
				}
			}
			if !strings.Contains(ansi.Strip(got), ">   one()\n>   two()") {
				t.Fatal("Code indentation changed")
			}
			if strings.Contains(got, "48;2;") != (style == HeaderPromptBackground) {
				t.Fatal("Unexpected body background")
			}
			m.Own = false
			m.Author = "claude"
			if FormatGallery(m, nil, style, dark) != FormatGallery(m, nil, HeaderMarker, dark) {
				t.Fatal("Agent output changed from style 8")
			}
		}
	}
}
