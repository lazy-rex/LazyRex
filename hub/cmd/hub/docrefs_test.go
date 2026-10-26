package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocReferences checks the references between the docs (README.md, docs/*.md and the
// README.md of each top directory):
//   - a Markdown link to a local file names a file that exists, and its #anchor names a heading;
//   - a quoted section after a doc name (docs/hub.md, "Bindings"; plugin-sources.md, section 7,
//     "Selected run") names a heading of that doc;
//   - see "X" names a heading of the same doc;
//   - a section name is a heading, a bold label (**Relaunch.**) or the lead of a list item
//     (- The admin guard (...)), and a name wrapped over two lines counts as one line;
//   - a backtick path under a directory of this repository (hub/, view/, sdk/, lab/, scripts/,
//     docs/, plugins/, hooks/, .github/) exists. tests/ is not checked: a plugin repository has a
//     tests/ directory too, and the docs name paths in it.
func TestDocReferences(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	docs, _ := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	readmes, _ := filepath.Glob(filepath.Join(root, "*", "README.md"))
	docs = append(append(docs, filepath.Join(root, "README.md")), readmes...)
	heads := map[string][]string{} // repository path -> its headings
	text := map[string]string{}
	for _, d := range docs {
		b, err := os.ReadFile(d)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, d)
		text[rel] = docText(string(b))
		heads[rel] = append(docHeadings(string(b)), docLabels(text[rel])...)
	}

	link := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	named := regexp.MustCompile(`((?:[a-z]+/)?[A-Za-z0-9_-]+\.md)\x60?,?(?: section [0-9]+[a-z]?,)? "([^"]+)"`)
	see := regexp.MustCompile(`[Ss]ee "([^"]+)"`)
	path := regexp.MustCompile("`((?:hub|view|sdk|lab|scripts|docs|plugins|hooks|\\.github)/[A-Za-z0-9_./-]+)`")
	for rel, s := range text {
		doc := func(name string) string {
			if strings.Contains(name, "/") || name == "README.md" {
				return name
			}
			return "docs/" + name
		}
		for _, m := range link.FindAllStringSubmatch(s, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			file, anchor, _ := strings.Cut(target, "#")
			if file == "" {
				file = rel
			} else {
				file = filepath.ToSlash(filepath.Join(filepath.Dir(rel), file))
			}
			if _, err := os.Stat(filepath.Join(root, file)); err != nil {
				t.Errorf("%s: the link %s names a missing file", rel, target)
				continue
			}
			if anchor != "" && !hasAnchor(heads[file], anchor) {
				t.Errorf("%s: the link %s names no heading of %s", rel, target, file)
			}
		}
		for _, m := range named.FindAllStringSubmatch(s, -1) {
			file := doc(m[1])
			if hs, ok := heads[file]; ok && !hasHeading(hs, oneLine(m[2])) {
				t.Errorf("%s: %s has no section %q", rel, file, m[2])
			} else if !ok {
				t.Errorf("%s: %q names %s, which is not a doc of this repository", rel, m[2], m[1])
			}
		}
		for _, m := range see.FindAllStringSubmatch(s, -1) {
			if !hasHeading(heads[rel], oneLine(m[1])) {
				t.Errorf("%s: see %q names no heading of this doc", rel, m[1])
			}
		}
		for _, m := range path.FindAllStringSubmatch(s, -1) {
			p := strings.TrimRight(m[1], ".")
			if strings.ContainsAny(p, "*<>") {
				continue // a pattern, not a path
			}
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				t.Errorf("%s: the path %s does not exist", rel, p)
			}
		}
	}
}

// docText is the text of a Markdown doc without its fenced code blocks.
func docText(s string) string {
	var out []string
	fence := false
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
			continue
		}
		if !fence {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// docHeadings are the heading texts of a Markdown doc, outside fenced code blocks.
func docHeadings(s string) []string {
	var hs []string
	fence := false
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
			continue
		}
		if !fence && strings.HasPrefix(l, "#") {
			hs = append(hs, strings.TrimSpace(strings.TrimLeft(l, "#")))
		}
	}
	return hs
}

var (
	boldLabel = regexp.MustCompile(`\*\*([^*]+?)\.?\*\*`)
	itemLead  = regexp.MustCompile(`(?m)^\s*- ([^(.:\n]+?)(?: \(|\.|:)`)
)

// docLabels are the bold labels and the list item leads of a doc's text.
func docLabels(s string) []string {
	var ls []string
	for _, re := range []*regexp.Regexp{boldLabel, itemLead} {
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			ls = append(ls, strings.TrimSpace(m[1]))
		}
	}
	return ls
}

// oneLine joins a name that the doc wrapped over lines.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

var sectionNumber = regexp.MustCompile(`^[0-9]+[a-z]?\.\s+`)

// hasHeading reports whether a heading is name: the same text, without a section number, or with
// a qualifier in parentheses after it ("Peer hubs" for "Peer hubs (federation v1, 2026-10-23)").
// Inline code marks do not count.
func hasHeading(hs []string, name string) bool {
	name = strings.ReplaceAll(name, "`", "")
	for _, h := range hs {
		h = strings.ReplaceAll(h, "`", "")
		h = sectionNumber.ReplaceAllString(h, "")
		if h == name || strings.HasPrefix(h, name+" (") || strings.HasPrefix(h, name+": ") {
			return true
		}
	}
	return false
}

// hasAnchor reports whether anchor is the GitHub anchor of a heading: lower case, spaces as
// hyphens, and no punctuation but hyphens and underscores.
func hasAnchor(hs []string, anchor string) bool {
	for _, h := range hs {
		var b strings.Builder
		for _, r := range strings.ToLower(h) {
			switch {
			case r == ' ':
				b.WriteRune('-')
			case r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127:
				b.WriteRune(r)
			}
		}
		if b.String() == anchor {
			return true
		}
	}
	return false
}
