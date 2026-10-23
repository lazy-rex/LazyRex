package main

import (
	"charm.land/bubbles/v2/key"
)

// The composer's keys (thread view plan item 5). Enter sends (send_action). The newline keys are
// declared once: the editor binds every one of them, and the footer hint names the first one, so
// the hint never names a key that the editor does not bind. Alt+Enter is a renderer binding: the
// keys that a Mac or a hardware iOS keyboard sends for it are not observed. The iOS probe recorded
// no modified Return from the app's on-screen keyboard (docs/rex-protocol.md), and multi-line input
// there is an accepted limit. A hardware keyboard on iOS is not tested.

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

// composerHint returns the composer's hint row. Below 60 columns it names only Enter (lead-comp2):
// the full hint at 60 columns or more lists the newline key, and the thread menu reaches the rest.
func composerHint(width int) string {
	if width < 60 {
		return "Enter send"
	}
	return "Enter send · " + newlineKeys[0].name + " newline"
}
