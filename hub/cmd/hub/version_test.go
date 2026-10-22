package main

import (
	"strings"
	"testing"
)

// The release build sets version; the text starts with it. A test binary has no VCS stamp, so the
// commit part is not checked here: scripts/release-build.sh and the release workflow check it.
func TestVersionText(t *testing.T) {
	defer func(v string) { version = v }(version)
	if got := versionText(); !strings.HasPrefix(got, "dev") {
		t.Errorf("default: %q", got)
	}
	version = "v1.2.3"
	if got := versionText(); !strings.HasPrefix(got, "v1.2.3") || strings.Contains(got, "dev") {
		t.Errorf("release: %q", got)
	}
}
