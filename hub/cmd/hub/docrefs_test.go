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
//   - a Markdown link to a local file names a file that exists, and its #anchor names a heading
//     (only a heading: a bold label has no anchor);
//   - a quoted section after a doc name (docs/hub.md, "Bindings"; plugin-sources.md, section 7,
//     "Selected run") names a section of that doc;
//   - see "X" names a section of the same doc;
//   - a section is a heading, a bold label (**Relaunch.**) or the lead of a list item
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
	var rels []string
	for _, d := range docs {
		rel, _ := filepath.Rel(root, d)
		rels = append(rels, rel)
	}
	for _, e := range docRefErrors(t, root, rels) {
		t.Error(e)
	}
}

// TestDocReferencesFixture gives the check one doc with each kind of good and bad reference.
func TestDocReferencesFixture(t *testing.T) {
	root := t.TempDir()
	doc := "# Fixture\n\n## Real heading\n\n**Bold label.** Text.\n\n- List lead (a note)\n\n" +
		"Good: [a](#real-heading), see \"Real heading\", see \"Bold label\", see \"List lead\",\n" +
		"(docs/a.md, \"Bold\nlabel\"), `docs/a.md`.\n\n" +
		"Bad: [b](#bold-label), [c](missing.md), see \"No section\", (docs/a.md, \"Nope\"),\n" +
		"`docs/missing.md`.\n"
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.md"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	got := docRefErrors(t, root, []string{"docs/a.md"})
	want := []string{
		"docs/a.md: the link #bold-label names no heading of docs/a.md",
		"docs/a.md: the link missing.md names a missing file",
		`docs/a.md: docs/a.md has no section "Nope"`,
		`docs/a.md: see "No section" names no section of this doc`,
		"docs/a.md: the path docs/missing.md does not exist",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("errors:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// docRefErrors checks the docs rels (paths relative to root) and returns one line for each
// broken reference, in the order of the checks, doc by doc.
func docRefErrors(t *testing.T, root string, rels []string) []string {
	t.Helper()
	heads := map[string][]string{}    // repository path -> its headings (anchors)
	sections := map[string][]string{} // repository path -> its headings and labels (section names)
	text := map[string]string{}
	for _, rel := range rels {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		text[rel] = docText(string(b))
		heads[rel] = docHeadings(string(b))
		sections[rel] = append(append([]string{}, heads[rel]...), docLabels(text[rel])...)
	}

	link := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	named := regexp.MustCompile(`((?:[a-z]+/)?[A-Za-z0-9_-]+\.md)\x60?,?(?: section [0-9]+[a-z]?,)? "([^"]+)"`)
	see := regexp.MustCompile(`[Ss]ee "([^"]+)"`)
	path := regexp.MustCompile("`((?:hub|view|sdk|lab|scripts|docs|plugins|hooks|\\.github)/[A-Za-z0-9_./-]+)`")
	doc := func(name string) string {
		if strings.Contains(name, "/") || name == "README.md" {
			return name
		}
		return "docs/" + name
	}
	var errs []string
	for _, rel := range rels {
		s := text[rel]
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
				errs = append(errs, rel+": the link "+target+" names a missing file")
				continue
			}
			if anchor != "" && !hasAnchor(heads[file], anchor) {
				errs = append(errs, rel+": the link "+target+" names no heading of "+file)
			}
		}
		for _, m := range named.FindAllStringSubmatch(s, -1) {
			file := doc(m[1])
			if hs, ok := sections[file]; !ok {
				errs = append(errs, rel+": "+`"`+m[2]+`" names `+m[1]+", which is not a doc of this repository")
			} else if !hasHeading(hs, oneLine(m[2])) {
				errs = append(errs, rel+": "+file+` has no section "`+oneLine(m[2])+`"`)
			}
		}
		for _, m := range see.FindAllStringSubmatch(s, -1) {
			if !hasHeading(sections[rel], oneLine(m[1])) {
				errs = append(errs, rel+`: see "`+oneLine(m[1])+`" names no section of this doc`)
			}
		}
		for _, m := range path.FindAllStringSubmatch(s, -1) {
			p := strings.TrimRight(m[1], ".")
			if strings.ContainsAny(p, "*<>") {
				continue // a pattern, not a path
			}
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				errs = append(errs, rel+": the path "+p+" does not exist")
			}
		}
	}
	return errs
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
