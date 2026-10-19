// Package transcript formats messages for terminal scrollback without a pane width.
package transcript

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
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

type Style int

const (
	TextBackground Style = iota
	BarBackground
	Foreground
)

type Message struct {
	ID, Author, Text, At string
	Own, FYI             bool
}

const reset = "\x1b[0m"
const userColor = "\x1b[38;2;125;211;252m"
const agentColor = "\x1b[38;2;251;191;36m"
const muted = "\x1b[38;2;148;163;184m"

// Format returns one complete message, followed by one blank line.
// Each message keeps its header. Previous is reserved for caller compatibility.
// The function owns its renderer, so callers can prepare separate messages concurrently.
func Format(m Message, previous *Message, style Style) string {
	body := renderBody(m.Text, styles(m.Own, style))
	if m.Own && style == BarBackground && body != "" {
		body = userColor + "▌" + reset + body
	}
	color := agentColor
	if m.Own {
		color = userColor
	}
	header := "\x1b[1m" + color + oneLine(m.Author) + reset
	if m.At != "" {
		header += " " + muted + "· " + oneLine(m.At) + reset
	}
	if m.FYI {
		header += " " + muted + "· no reply needed" + reset
	}
	if body == "" {
		return header + "\n\n"
	}
	return header + "\n" + body + reset + "\n\n"
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

func styles(own bool, choice Style) ga.StyleConfig {
	zero, bold, italic, crossed := uint(0), true, true, true
	dim, sky, background := "#94A3B8", "#7DD3FC", "#283040"
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
		Code:           ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Color: &dim}},
		CodeBlock:      ga.StyleCodeBlock{StyleBlock: ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Color: &dim}, Margin: &zero}},
		BlockQuote:     ga.StyleBlock{StylePrimitive: ga.StylePrimitive{Prefix: "> "}},
		Link:           ga.StylePrimitive{Format: "{{.text}}"},
		Image:          ga.StylePrimitive{Format: "{{.text}}"},
		HorizontalRule: ga.StylePrimitive{Format: "---\n"},
	}
	if own {
		if choice == TextBackground || choice == BarBackground {
			s.Document.BackgroundColor = &background
		} else {
			s.Document.Color = &sky
		}
	}
	return s
}
