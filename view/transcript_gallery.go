package main

import (
	"fmt"

	"github.com/lazy-rex/LazyRex/view/transcript"
)

// galleryEntry is one message style in the printer's gallery mode. format returns the
// formatter for the light or the dark palette.
type galleryEntry struct {
	name   string
	format func(dark bool) func(Message, *Message) string
}

// galleryEntries lists the formatter's gallery styles, in the order s and S cycle them.
func galleryEntries() []galleryEntry {
	var entries []galleryEntry
	for _, style := range transcript.GalleryStyles {
		entries = append(entries, galleryEntry{name: style.String(), format: func(dark bool) func(Message, *Message) string {
			return func(m Message, previous *Message) string {
				if previous == nil {
					return transcript.FormatGallery(transcriptMessage(m), nil, style, dark)
				}
				p := transcriptMessage(*previous)
				return transcript.FormatGallery(transcriptMessage(m), &p, style, dark)
			}
		}})
	}
	return entries
}

// galleryBanner is the first line of every gallery rebuild. It names the palette, and says
// "forced" after t replaced the terminal's theme. FormatGallery adds the gap before each
// header, so the banner ends with one newline.
func galleryBanner(i, n int, name string, dark, forced bool) string {
	theme := "light"
	if dark {
		theme = "dark"
	}
	if forced {
		theme += " (forced)"
	}
	return fmt.Sprintf("\x1b[2m── Style %d of %d: %s · %s · s next · S previous · t theme ──\x1b[0m\n", i+1, n, name, theme)
}
