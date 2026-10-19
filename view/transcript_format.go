package main

import (
	"fmt"
	"strconv"

	"github.com/lazy-rex/LazyRex/view/transcript"
)

// transcriptStyle is a printer style: a first demo style (a, c, d; one palette) or a gallery
// style (1 to len(transcript.GalleryStyles); a light and a dark palette).
type transcriptStyle struct {
	demo    transcript.Style
	gallery int // 1-based index in transcript.GalleryStyles; 0 for a demo style
}

func parseTranscriptStyle(s string) (transcriptStyle, error) {
	switch s {
	case "a", "text-background":
		return transcriptStyle{demo: transcript.TextBackground}, nil
	case "c", "bar-background":
		return transcriptStyle{demo: transcript.BarBackground}, nil
	case "d", "foreground":
		return transcriptStyle{demo: transcript.Foreground}, nil
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(transcript.GalleryStyles) {
		return transcriptStyle{gallery: n}, nil
	}
	return transcriptStyle{}, fmt.Errorf("unknown transcript style %q (use a, c, d, or 1 to %d)", s, len(transcript.GalleryStyles))
}

// transcriptMessage converts a view message for the formatter. The header shows the
// compact local time, as the renderer does.
func transcriptMessage(m Message) transcript.Message {
	author := m.Author
	if m.Own {
		author = "You"
	}
	return transcript.Message{ID: m.ID, Author: author, Text: m.Text, At: shortTime(m.At), Own: m.Own, FYI: m.FYI}
}

// formatTranscript adapts the view message to the width-independent formatter. A gallery style
// gets the palette for dark; a demo style has one palette.
func formatTranscript(style transcriptStyle, dark bool) func(Message, *Message) string {
	if style.gallery > 0 {
		return galleryEntries()[style.gallery-1].format(dark)
	}
	return func(m Message, previous *Message) string {
		if previous == nil {
			return transcript.Format(transcriptMessage(m), nil, style.demo)
		}
		p := transcriptMessage(*previous)
		return transcript.Format(transcriptMessage(m), &p, style.demo)
	}
}
