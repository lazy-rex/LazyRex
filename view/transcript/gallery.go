package transcript

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// GalleryStyle identifies one fixture style. It is separate from the original demo styles.
type GalleryStyle int

const (
	NameChip GalleryStyle = iota
	TintedChip
	ChatMarkers
	GroupedRule
	CodePrompt
	GroupedNames
	UserChip
	HeaderMarker
	HeaderPrompt
	HeaderPromptBackground
	BoldUser
)

var GalleryStyles = []GalleryStyle{NameChip, TintedChip, ChatMarkers, GroupedRule, CodePrompt, GroupedNames, UserChip, HeaderMarker, HeaderPrompt, HeaderPromptBackground, BoldUser}

func (s GalleryStyle) String() string {
	names := [...]string{"1 name chip", "2 chip + user tint", "3 chat markers", "4 grouped rule", "5 code prompt", "6 grouped names", "7 user-only chip", "8 header marker", "9 header + user prompt", "10 header + user prompt background", "11 header + bold user"}
	if s < 0 || int(s) >= len(names) {
		return "unknown style"
	}
	return names[s]
}

// FormatGallery returns one message with a final newline. It puts gaps before headers,
// so grouped messages can follow each other without an extra blank line.
// The caller supplies the same style and the previous message from this transcript.
func FormatGallery(m Message, previous *Message, choice GalleryStyle, dark bool) string {
	palette := galleryPalette(dark)
	color := palette.sender(m)
	config := styles(false, Foreground)
	dim := palette.dim
	config.Document.Color = &palette.body
	config.Code.Color, config.CodeBlock.Color = &dim, &dim
	bold := true
	if m.Own {
		switch choice {
		case TintedChip:
			config.Document.Color = &color
		case ChatMarkers, BoldUser:
			config.Document.Bold = &bold
			if choice == BoldUser {
				normal := false
				config.List.Bold = &bold
				config.CodeBlock.Bold = &normal
			}
		case CodePrompt, HeaderPromptBackground:
			fg, bg := palette.body, palette.userBackground
			config.Document.Color, config.Document.BackgroundColor = &fg, &bg
		}
	}
	body := renderBody(m.Text, config)
	grouped := (choice == GroupedRule || choice == GroupedNames) && sameGroup(m, previous)
	gap := ""
	if previous != nil && !grouped {
		gap = "\n"
	}
	if grouped {
		if m.FYI {
			body = rgb(dim, false) + "no reply needed" + reset + "\n" + body
		}
		return body + reset + "\n"
	}
	name := oneLine(m.Author)
	header := "\x1b[1m" + rgb(color, false) + name + reset
	chip := "\x1b[1m" + rgb("#FFFFFF", false) + rgb(senderColor(m), true) + name + reset
	switch choice {
	case NameChip, TintedChip:
		header = chip
	case ChatMarkers:
		marker := "●"
		if m.Own {
			marker = "›"
		}
		header = rgb(color, false) + marker + " " + reset + header
	case GroupedRule:
		gap += rgb(dim, false) + "────" + reset + "\n"
	case CodePrompt:
		if !m.Own {
			header = rgb(color, false) + "● " + reset + header
		}
	case UserChip:
		if m.Own {
			header = chip
		}
	case HeaderMarker, HeaderPrompt, HeaderPromptBackground, BoldUser:
		header = rgb(color, false) + "▍ " + reset + header
	}
	if m.At != "" {
		header += " " + rgb(dim, false) + "· " + oneLine(m.At) + reset
	}
	if m.FYI {
		header += " " + rgb(dim, false) + "· no reply needed" + reset
	}
	if body == "" {
		return gap + header + "\n"
	}
	if choice == CodePrompt && m.Own {
		body = rgb(color, false) + "> " + reset + body
	}
	if m.Own && (choice == HeaderPrompt || choice == HeaderPromptBackground) {
		prefix := rgb(color, false) + "> " + reset
		body = prefix + strings.ReplaceAll(body, "\n", "\n"+prefix)
	}
	return gap + header + "\n" + body + reset + "\n"
}

// Chip colors stay dark in both palettes. Sender foregrounds change with the theme.
type palette struct {
	body, dim, background, userBackground string
	user, claude, codex, other            string
}

func galleryPalette(dark bool) palette {
	if dark {
		return palette{"#F1F5F9", "#CBD5E1", "#1E1E1E", "#334155", "#60A5FA", "#FDBA74", "#6EE7B7", "#C4B5FD"}
	}
	return palette{"#0F172A", "#475569", "#FFFFFF", "#E2E8F0", "#2563EB", "#C2410C", "#047857", "#7C3AED"}
}

func (p palette) sender(m Message) string {
	if m.Own {
		return p.user
	}
	switch strings.ToLower(strings.TrimSpace(m.Author)) {
	case "user", "you":
		return p.user
	case "claude":
		return p.claude
	case "codex":
		return p.codex
	default:
		return p.other
	}
}

func senderColor(m Message) string {
	if m.Own {
		return "#2563EB"
	}
	switch strings.ToLower(strings.TrimSpace(m.Author)) {
	case "user", "you":
		return "#2563EB"
	case "claude":
		return "#C2410C"
	case "codex":
		return "#047857"
	default:
		return "#7C3AED"
	}
}

func rgb(hex string, background bool) string {
	n, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 24)
	channel := 38
	if background {
		channel = 48
	}
	return fmt.Sprintf("\x1b[%d;2;%d;%d;%dm", channel, n>>16, n>>8&255, n&255)
}

func sameGroup(m Message, previous *Message) bool {
	if previous == nil || m.Own != previous.Own || m.Author != previous.Author {
		return false
	}
	for _, layout := range []string{time.RFC3339Nano, "Jan 2 · 15:04", "15:04:05", "15:04"} {
		now, err := time.Parse(layout, m.At)
		if err != nil {
			continue
		}
		before, err := time.Parse(layout, previous.At)
		if err != nil {
			continue
		}
		delta := now.Sub(before)
		return delta >= 0 && delta <= 5*time.Minute
	}
	return false
}
