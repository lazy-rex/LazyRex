package main

import "regexp"

// versionTag is the form of a release tag that @latest can select (section 5): v, then three
// decimal numbers without a leading zero. A pre-release, build metadata, an upper-case V and every
// other name are not release tags.
var versionTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// tagVersion is the major, minor and patch numbers of a release tag, as decimal strings. A string
// has no length limit, so no tag is too large to compare.
type tagVersion [3]string

// parseVersionTag gives the version of a release tag, and false for every other name.
func parseVersionTag(tag string) (tagVersion, bool) {
	m := versionTag.FindStringSubmatch(tag)
	if m == nil {
		return tagVersion{}, false
	}
	return tagVersion{m[1], m[2], m[3]}, true
}

// compareVersions gives -1, 0 or 1 when a is lower than, equal to or higher than b.
func compareVersions(a, b tagVersion) int {
	for i := range a {
		if c := compareDecimal(a[i], b[i]); c != 0 {
			return c
		}
	}
	return 0
}

// compareDecimal compares two decimal strings without a leading zero: the longer string is
// higher, and strings of equal length compare as strings.
func compareDecimal(a, b string) int {
	switch {
	case len(a) != len(b):
		if len(a) < len(b) {
			return -1
		}
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
