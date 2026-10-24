package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

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

// latestRelease is the release that @latest selects: the tag and the commit that it peels to. The
// commit is resolved with Kind "latest" and the tag ref as the ref to fetch. Ignored names the
// higher release tags that point to a tree or a blob.
type latestRelease struct {
	resolved
	Tag     string
	Ignored []string
}

// resolveLatest selects the highest release tag of url that points to a commit (section 5,
// "@latest"). ls-remote lists the candidates; it gives no object types. So each candidate, highest
// first, is fetched into a bare repository in dir and peeled fully with ^{}: a commit is selected,
// and a tree or a blob is skipped. Every git failure and every other type fails the resolution: a
// failure never selects an older release.
func resolveLatest(ctx context.Context, git gitRunner, dir, url string) (latestRelease, error) {
	out, err := git(ctx, dir, nil, "ls-remote", "--tags", "--", url, "refs/tags/v*")
	if err != nil {
		return latestRelease{}, err
	}
	return verifyLatest(ctx, git, dir, url, listVersionTags(out))
}

// errNoRelease is the result of a repository with no release tag that points to a commit.
var errNoRelease = errors.New("the repository has no version tag vX.Y.Z that points to a commit")

// versionTagRef is a release tag of an ls-remote listing: its name, the ID that ls-remote gives
// (the peeled ID of an annotated tag, else the tag's own ID) and its version. ls-remote gives no
// object types, so the ID is not known to be a commit.
type versionTagRef struct {
	tag, object string
	v           tagVersion
}

// listVersionTags parses the output of "git ls-remote --tags": the release tags, highest first.
func listVersionTags(out []byte) []versionTagRef {
	var cands []versionTagRef
	peeled := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		id, name, ok := strings.Cut(line, "\t")
		tag, isTag := strings.CutPrefix(name, "refs/tags/")
		if !ok || !isTag {
			continue
		}
		if t, ok := strings.CutSuffix(tag, "^{}"); ok {
			peeled[t] = id
			continue
		}
		if v, ok := parseVersionTag(tag); ok {
			cands = append(cands, versionTagRef{tag: tag, object: id, v: v})
		}
	}
	for i, c := range cands {
		if id := peeled[c.tag]; id != "" {
			cands[i].object = id
		}
	}
	slices.SortFunc(cands, func(a, b versionTagRef) int { return compareVersions(b.v, a.v) })
	return cands
}

// verifyLatest selects the first candidate (highest first) that points to a commit: each one is
// fetched into a bare repository in dir and peeled fully with ^{}. A tree or a blob is skipped
// and named; every git failure and every other type fails.
func verifyLatest(ctx context.Context, git gitRunner, dir, url string, cands []versionTagRef) (latestRelease, error) {
	gitDir := filepath.Join(dir, "git")
	if len(cands) > 0 {
		if _, err := git(ctx, dir, nil, "init", "-q", "--bare", gitDir); err != nil {
			return latestRelease{}, err
		}
	}
	var ignored []string
	for _, c := range cands {
		ref := "refs/tags/" + c.tag
		if _, err := git(ctx, gitDir, nil, "fetch", "-q", "--no-tags", "--depth", "1", "--", url, "+"+ref+":"+ref); err != nil {
			return latestRelease{}, fmt.Errorf("fetch %s: %w", ref, err)
		}
		t, err := git(ctx, gitDir, nil, "cat-file", "-t", ref+"^{}")
		if err != nil {
			return latestRelease{}, fmt.Errorf("%s: %w", ref, err)
		}
		switch typ := strings.TrimSpace(string(t)); typ {
		case "commit":
			id, err := git(ctx, gitDir, nil, "rev-parse", "--verify", ref+"^{}")
			if err != nil {
				return latestRelease{}, fmt.Errorf("%s: %w", ref, err)
			}
			commit := strings.TrimSpace(string(id))
			if !commitID.MatchString(commit) {
				return latestRelease{}, fmt.Errorf("%s: rev-parse gives %q, not a commit ID", ref, commit)
			}
			return latestRelease{resolved: resolved{Commit: commit, Kind: "latest", FetchRef: ref}, Tag: c.tag, Ignored: ignored}, nil
		case "tree", "blob":
			ignored = append(ignored, fmt.Sprintf("%s points to a %s, not a commit", ref, typ))
		default:
			return latestRelease{}, fmt.Errorf("%s points to the object type %q", ref, typ)
		}
	}
	return latestRelease{}, errNoRelease
}
