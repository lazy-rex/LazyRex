package transcript

import (
	"math"
	"strconv"
	"strings"
	"testing"
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

// TestContrast checks the palettes against the assumed terminal backgrounds.
func TestContrast(t *testing.T) {
	for _, dark := range []bool{false, true} {
		p, background := newPalette(dark), "#FFFFFF"
		if dark {
			background = "#1E1E1E"
		}
		check := func(label, fg string, minimum float64) {
			a, b := luminance(fg), luminance(background)
			if a < b {
				a, b = b, a
			}
			ratio := (a + 0.05) / (b + 0.05)
			t.Logf("dark=%t %s %s on %s: %.2f:1 (minimum %.1f)", dark, label, fg, background, ratio, minimum)
			if ratio < minimum {
				t.Errorf("Insufficient contrast: %s %.2f", label, ratio)
			}
		}
		check("body", p.body, 4.5)
		check("time and code", p.dim, 4.5)
		for _, m := range []Message{{Own: true}, {Author: "claude"}, {Author: "codex"}, {Author: "other"}} {
			check("bold name", p.sender(m), 3)
		}
	}
}
