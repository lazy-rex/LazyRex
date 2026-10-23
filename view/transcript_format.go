package main

import "github.com/lazy-rex/LazyRex/view/transcript"

// transcriptMessage converts a view message for the formatter. The header shows the
// compact local time, as the renderer does.
func transcriptMessage(m Message) transcript.Message {
	author := m.Author
	if m.Own {
		author = "You"
	}
	return transcript.Message{ID: m.ID, Author: author, Text: m.Text, At: shortTime(m.At), Own: m.Own, FYI: m.FYI}
}

// formatTranscript adapts the view message to the width-independent formatter, with the
// palette for dark.
func formatTranscript(dark bool) func(Message, *Message) string {
	return func(m Message, previous *Message) string {
		if previous == nil {
			return transcript.Format(transcriptMessage(m), nil, dark)
		}
		p := transcriptMessage(*previous)
		return transcript.Format(transcriptMessage(m), &p, dark)
	}
}
