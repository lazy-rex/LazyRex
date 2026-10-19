package main

import (
	"encoding/base64"
	"strings"
	"unicode"
)

// Status is the view's program status (Rex Program Status Protocol, OSC 7501). Rex shows it on
// the tab header when the tab is not focused, and in the session picker. The plugin decides the
// state and the text, for example {state: "done", msg: "3 unread from Codex"}.
type Status struct {
	State string `json:"state"`
	Kind  string `json:"kind,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

const statusClear = "\x1b]7501;state=clear\x1b\\"

// statusSequence returns the OSC 7501 report for s. No status, or an unknown state, clears the
// record. The protocol forbids control characters in msg; this removes them and keeps msg short.
func statusSequence(s *Status) string {
	if s == nil {
		return statusClear
	}
	switch s.State {
	case "idle", "working", "done", "blocked", "error":
	default:
		return statusClear
	}
	seq := "\x1b]7501;state=" + s.State + ":app=rex"
	if s.State == "blocked" {
		switch s.Kind {
		case "permission", "question", "auth":
			seq += ":kind=" + s.Kind
		}
	}
	if value := statusText(s.Msg); value != "" {
		seq += ":msg=" + base64.StdEncoding.EncodeToString([]byte(value))
	}
	return seq + "\x1b\\"
}

// statusText removes controls before text enters a terminal report.
func statusText(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, strings.TrimSpace(text))
	if r := []rune(text); len(r) > 200 {
		text = string(r[:200])
	}
	return text
}
