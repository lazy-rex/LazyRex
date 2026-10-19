package main

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// themeReport is a report about the terminal's background from the pane's input: an OSC 11
// answer (the background color) or a color-scheme report (CSI ? 997 ; 1 n dark, ; 2 n light).
// A scheme report answers DSR 996, and with mode 2031 set it also comes on each change.
type themeReport struct {
	dark  bool
	osc11 bool // from an OSC 11 answer, not a scheme report
}

// themeQuery enables scheme change reports (mode 2031), then asks for the background (OSC 11)
// and the scheme (DSR 996). The terminal answers in order, so the scheme answer comes last.
// Observed on Rex 0.0.1792158932-g88fd41: OSC 11 has no answer until a client sets a theme;
// DSR 996 always has one.
const themeQuery = "\x1b[?2031h\x1b]11;?\x1b\\\x1b[?996n"

// themeQueryTimeout bounds the wait for the answers before the first print.
const themeQueryTimeout = 300 * time.Millisecond

// awaitTheme reads the answers to themeQuery. An OSC 11 answer wins over the scheme answer.
// With no answer before the timeout, the theme is light.
func awaitTheme(reports <-chan themeReport, timeout time.Duration) (dark bool) {
	deadline := time.After(timeout)
	var background *themeReport
	for {
		select {
		case r := <-reports:
			if r.osc11 {
				background = &r
				continue
			}
			if background != nil {
				return background.dark
			}
			return r.dark
		case <-deadline:
			return background != nil && background.dark
		}
	}
}

// parseSchemeReport reads the parameters of a CSI ... n report.
func parseSchemeReport(params string) (themeReport, bool) {
	switch params {
	case "?997;1":
		return themeReport{dark: true}, true
	case "?997;2":
		return themeReport{dark: false}, true
	}
	return themeReport{}, false
}

// parseOSC11 reads an OSC 11 answer payload, for example "11;rgb:1e1e/1e1e/1e1e". Each
// component has 1 to 4 hex digits. A background is dark when white text has more contrast on it
// than black text: relative luminance under sqrt(1.05 * 0.05) - 0.05, about 0.179.
func parseOSC11(payload string) (themeReport, bool) {
	spec, ok := strings.CutPrefix(payload, "11;rgb:")
	if !ok {
		return themeReport{}, false
	}
	parts := strings.Split(spec, "/")
	if len(parts) != 3 {
		return themeReport{}, false
	}
	var c [3]float64
	for i, p := range parts {
		if len(p) < 1 || len(p) > 4 {
			return themeReport{}, false
		}
		v, err := strconv.ParseUint(p, 16, 16)
		if err != nil {
			return themeReport{}, false
		}
		c[i] = float64(v) / float64(uint64(1)<<(4*len(p))-1)
	}
	return themeReport{dark: relativeLuminance(c[0], c[1], c[2]) < math.Sqrt(1.05*0.05)-0.05, osc11: true}, true
}

// relativeLuminance is the WCAG relative luminance of sRGB components in [0, 1].
func relativeLuminance(r, g, b float64) float64 {
	lin := func(c float64) float64 {
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}
