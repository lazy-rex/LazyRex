package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Rex Lua evaluates a plain multiple assignment wrongly: a later value that reads an earlier
// target sees the new value (`a, b = b, a` gives b, b; rex-protocol.md, "Lua swap assignment is
// wrong"). lazyrex-hub test warns about the pattern. It is a line-based check, so it can miss a
// case or report a false one; it never fails a run.

var (
	multiAssign = regexp.MustCompile(`(?:^|\bthen\b|\bdo\b|\belse\b|;)\s*([A-Za-z_][\w.\[\]'"]*(?:\s*,\s*[A-Za-z_][\w.\[\]'"]*)+)\s*=([^=].*)$`)
	plainName   = regexp.MustCompile(`^[A-Za-z_]\w*$`)
)

// splitTop splits s at commas outside brackets and strings.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	var quote rune
	for i, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

// multiAssignHazard reports whether one line has a multiple assignment where a later value reads
// an earlier plain variable target. A local declaration and table field targets are correct.
func multiAssignHazard(line string) bool {
	code, _, _ := strings.Cut(line, "--")
	m := multiAssign.FindStringSubmatch(code) // a statement start never matches "local a, b"
	if m == nil {
		return false
	}
	targets, values := splitTop(m[1]), splitTop(m[2])
	for i, t := range targets {
		if !plainName.MatchString(t) {
			continue
		}
		ref := regexp.MustCompile(`(^|[^\w.])` + regexp.QuoteMeta(t) + `($|\W)`)
		for _, v := range values[min(i+1, len(values)):] {
			if ref.MatchString(v) {
				return true
			}
		}
	}
	return false
}

// luaWarnings scans the .lua files under dir and returns one warning per hazard.
func luaWarnings(dir string) []string {
	var out []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || filepath.Ext(path) != ".lua" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for n := 1; sc.Scan(); n++ {
			if multiAssignHazard(sc.Text()) {
				rel, _ := filepath.Rel(dir, path)
				out = append(out, fmt.Sprintf("warning: %s:%d: a multiple assignment reads an earlier target; Rex Lua assigns it wrongly (use a temporary)", rel, n))
			}
		}
		return nil
	})
	return out
}
