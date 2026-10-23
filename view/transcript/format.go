// Package transcript formats messages for terminal scrollback without a pane width.
package transcript

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	ga "charm.land/glamour/v2/ansi"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	ea "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type Message struct {
	ID, Author, Text, At string
	Own, FYI             bool
}

const reset = "\x1b[0m"

// Format returns one message with a final newline. The header has a colored bar, the bold
// sender name, the time and the FYI note. User paragraphs and lists are bold; code keeps its
// color and normal weight. Format puts a blank line before the header when previous is not
// nil, so the caller passes the previous printed message and nil after a rebuild.
// The function owns its renderer, so callers can prepare separate messages concurrently.
func Format(m Message, previous *Message, dark bool) string {
	palette := newPalette(dark)
	color := palette.sender(m)
	config := styles()
	dim := palette.dim
	config.Document.Color = &palette.body
	config.Code.Color, config.CodeBlock.Color = &dim, &dim
	if m.Own {
		bold, normal := true, false
		config.Document.Bold = &bold
		config.List.Bold = &bold
		config.CodeBlock.Bold = &normal
	}
	body := renderBody(m.Text, config)
	gap := ""
	if previous != nil {
		gap = "\n"
	}
	header := rgb(color) + "▍ " + reset + "\x1b[1m" + rgb(color) + oneLine(m.Author) + reset
	if m.At != "" {
		header += " " + rgb(dim) + "· " + oneLine(m.At) + reset
	}
	if m.FYI {
		header += " " + rgb(dim) + "· no reply needed" + reset
	}
	if body == "" {
		return gap + header + "\n"
	}
	return gap + header + "\n" + body + reset + "\n"
}

type palette struct {
	body, dim                  string
	user, claude, codex, other string
}

func newPalette(dark bool) palette {
	if dark {
		return palette{"#F1F5F9", "#CBD5E1", "#60A5FA", "#FDBA74", "#6EE7B7", "#C4B5FD"}
	}
	return palette{"#0F172A", "#475569", "#2563EB", "#C2410C", "#047857", "#7C3AED"}
}

// sender returns the name color. Matching ignores case, and Own takes priority over the name.
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

// rgb returns the 24-bit foreground sequence for a #RRGGBB color.
func rgb(hex string) string {
	n, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 24)
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", n>>16, n>>8&255, n&255)
}

func renderBody(value string, config ga.StyleConfig) string {
	source := []byte(sanitize(value))
	md := goldmark.New(goldmark.WithExtensions(extension.GFM, extension.DefinitionList))
	doc := md.Parser().Parse(text.NewReader(source))
	source = prepare(doc, source)
	ar := ga.NewRenderer(ga.Options{WordWrap: 0, PreserveNewLines: true, Styles: config})
	r := renderer.NewRenderer(renderer.WithNodeRenderers(util.Prioritized(ar, 100)))
	var out bytes.Buffer
	if err := r.Render(&out, source, doc); err != nil {
		out.Reset()
		out.WriteString(sanitize(value))
	}
	return trailing.ReplaceAllString(strings.TrimLeft(out.String(), "\n"), "")
}

var entity = regexp.MustCompile(`&(?:#[xX][0-9a-fA-F]+|#[0-9]+|[A-Za-z]+);?`)
var trailing = regexp.MustCompile(`(?:\x1b\[[0-9;:]*m|\n)+$`)

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = entity.ReplaceAllStringFunc(s, func(token string) string {
		for _, r := range html.UnescapeString(token) {
			if unicode.IsControl(r) {
				return ""
			}
		}
		return token
	})
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

func oneLine(s string) string { return strings.Join(strings.Fields(sanitize(s)), " ") }

// prepare converts tables into labeled records and joins only Markdown soft breaks.
// Existing inline nodes retain links, emphasis, code, and escaped cell delimiters.
func prepare(doc ast.Node, source []byte) []byte {
	var tables []ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && n.Kind() == ea.KindTable {
			tables = append(tables, n)
		}
		return ast.WalkContinue, nil
	})
	appendText := func(parent ast.Node, value string) {
		start := len(source)
		source = append(source, value...)
		parent.AppendChild(parent, ast.NewTextSegment(text.NewSegment(start, len(source))))
	}
	for _, table := range tables {
		var labels []string
		header := table.FirstChild()
		if header == nil {
			continue
		}
		for cell := header.FirstChild(); cell != nil; cell = cell.NextSibling() {
			labels = append(labels, string(cell.Text(source)))
		}
		parent := table.Parent()
		for row := header.NextSibling(); row != nil; row = row.NextSibling() {
			p := ast.NewParagraph()
			column := 0
			for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
				if column > 0 {
					appendText(p, " · ")
				}
				label := fmt.Sprintf("Column %d", column+1)
				if column < len(labels) && strings.TrimSpace(labels[column]) != "" {
					label = labels[column]
				}
				appendText(p, label+": ")
				for cell.FirstChild() != nil {
					child := cell.FirstChild()
					cell.RemoveChild(cell, child)
					p.AppendChild(p, child)
				}
				column++
			}
			parent.InsertBefore(parent, table, p)
		}
		if header.NextSibling() == nil {
			p := ast.NewParagraph()
			appendText(p, strings.Join(labels, " · "))
			parent.InsertBefore(parent, table, p)
		}
		parent.RemoveChild(parent, table)
	}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if t, ok := n.(*ast.Text); entering && ok && t.SoftLineBreak() && !t.HardLineBreak() {
			value := append([]byte{}, t.Segment.Value(source)...)
			start := len(source)
			source = append(source, value...)
			source = append(source, ' ')
			t.Segment = text.NewSegment(start, len(source))
			t.SetSoftLineBreak(false)
		}
		return ast.WalkContinue, nil
	})
	return source
}

func styles() ga.StyleConfig {
	zero, bold, italic, crossed := uint(0), true, true, true
	s := ga.StyleConfig{
		Document:       ga.StyleBlock{Margin: &zero},
		Heading:        ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Bold: &bold, BlockSuffix: "\n"}},
		H1:             ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Prefix: "# "}},
		H2:             ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Prefix: "## "}},
		H3:             ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Prefix: "### "}},
		H4:             ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Prefix: "#### "}},
		H5:             ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Prefix: "##### "}},
		H6:             ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Prefix: "###### "}},
		Strong:         ga.StylePrimitive{Bold: &bold},
		Emph:           ga.StylePrimitive{Italic: &italic},
		Strikethrough:  ga.StylePrimitive{CrossedOut: &crossed},
		Item:           ga.StylePrimitive{BlockPrefix: "• "},
		Enumeration:    ga.StylePrimitive{BlockPrefix: ". "},
		Task:           ga.StyleTask{Ticked: "[x] ", Unticked: "[ ] "},
		List:           ga.StyleList{LevelIndent: 2},
		CodeBlock:      ga.StyleCodeBlock{StyleBlock: ga.StyleBlock{Margin: &zero}},
		BlockQuote:     ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Prefix: "> "}},
		Link:           ga.StylePrimitive{Format: "{{.text}}"},
		Image:          ga.StylePrimitive{Format: "{{.text}}"},
		HorizontalRule: ga.StylePrimitive{Format: "---\n"},
	}
	return s
}
