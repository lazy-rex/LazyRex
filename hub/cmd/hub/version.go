package main

import "runtime/debug"

// version is the release version. The release build sets it (scripts/release-build.sh:
// -X main.version=vX.Y.Z); any other build is "dev".
var version = "dev"

// versionText is the version and the commit that the Go toolchain stamped, for example
// "v0.4.0 (commit 1a2b...)" or "dev (commit 1a2b..., modified)". A build outside a git checkout has
// no commit.
func versionText() string {
	s := version
	if rev, modified := buildCommit(); rev != "" {
		s += " (commit " + rev
		if modified {
			s += ", modified"
		}
		s += ")"
	}
	return s
}

// buildCommit is the commit that the Go toolchain stamped (vcs.revision) and whether the checkout
// had changes (vcs.modified). A build outside a git checkout has no commit: "".
func buildCommit() (rev string, modified bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, kv := range bi.Settings {
		switch kv.Key {
		case "vcs.revision":
			rev = kv.Value
		case "vcs.modified":
			modified = kv.Value == "true"
		}
	}
	return rev, modified
}
