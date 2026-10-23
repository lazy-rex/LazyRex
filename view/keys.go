package main

import (
	"charm.land/bubbles/v2/key"
)

// The composer's keys (thread view plan item 5). Enter sends (send_action). The newline keys are
// declared once: the editor binds every one of them, and the footer hint names the first one, so
// the hint never names a key that the editor does not bind. Alt+Enter is a renderer binding: the
// keys that a Mac or a hardware iOS keyboard sends for it are not observed. The on-screen keyboard
// of the iOS app sends no modified Return (docs/rex-protocol.md), so multi-line input there is an
// accepted limit.

type newlineKey struct {
	key  string // the Bubble Tea key string
	name string // the key's name in the hint
}

var newlineKeys = []newlineKey{
	{key: "shift+enter", name: "Shift+Enter"}, // the hint names this one
	{key: "alt+enter", name: "Alt+Enter"},
	{key: "ctrl+j", name: "Ctrl+J"},
}

// newlineBinding is the editor's InsertNewline binding.
func newlineBinding() key.Binding {
	keys := make([]string, len(newlineKeys))
	for i, k := range newlineKeys {
		keys[i] = k.key
	}
	return key.NewBinding(key.WithKeys(keys...))
}

// composerHint returns the composer's hint row and, below 60 columns, a second hint row.
func composerHint(width int) (hint, second string) {
	name := newlineKeys[0].name
	if width < 60 {
		return "Enter send", name + " ↵"
	}
	return "Enter send · " + name + " newline", ""
}
