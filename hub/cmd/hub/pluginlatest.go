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
func resolveLatest(ctx context.Context, dir, url string) (latestRelease, error) {
	out, err := runGit(ctx, dir, "ls-remote", "--tags", "--", url, "refs/tags/v*")
	if err != nil {
		return latestRelease{}, err
	}
	type candidate struct {
		tag string
		v   tagVersion
	}
	var cands []candidate
	for _, line := range strings.Split(string(out), "\n") {
		_, name, ok := strings.Cut(line, "\t")
		tag, isTag := strings.CutPrefix(name, "refs/tags/")
		if !ok || !isTag {
			continue
		}
		if v, ok := parseVersionTag(tag); ok {
			cands = append(cands, candidate{tag, v})
		}
	}
	slices.SortFunc(cands, func(a, b candidate) int { return compareVersions(b.v, a.v) })

	gitDir := filepath.Join(dir, "git")
	if len(cands) > 0 {
		if _, err := runGit(ctx, dir, "init", "-q", "--bare", gitDir); err != nil {
			return latestRelease{}, err
		}
	}
	var ignored []string
	for _, c := range cands {
		ref := "refs/tags/" + c.tag
		if _, err := runGit(ctx, gitDir, "fetch", "-q", "--no-tags", "--depth", "1", "--", url, "+"+ref+":"+ref); err != nil {
			return latestRelease{}, fmt.Errorf("fetch %s: %w", ref, err)
		}
		t, err := runGit(ctx, gitDir, "cat-file", "-t", ref+"^{}")
		if err != nil {
			return latestRelease{}, fmt.Errorf("%s: %w", ref, err)
		}
		switch typ := strings.TrimSpace(string(t)); typ {
		case "commit":
			id, err := runGit(ctx, gitDir, "rev-parse", "--verify", ref+"^{}")
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
	return latestRelease{}, errors.New("the repository has no version tag vX.Y.Z that points to a commit")
}

// legacyLatest reports whether e is a lock entry of the source ref latest from before @latest: a
// branch or a tag named latest.
func (e *lockEntry) legacyLatest(r githubRef) bool {
	return r.Ref == "latest" && e.Ref == "latest" && (e.RefKind == "branch" || e.RefKind == "tag")
}

// legacyLatestError is the refusal of a legacy entry (section 6). Only plugin update converts it.
func legacyLatestError(cfgPath, source string, e *lockEntry) error {
	full := "refs/heads/latest"
	if e.RefKind == "tag" {
		full = "refs/tags/latest"
	}
	return fmt.Errorf("@latest now means the highest version tag vX.Y.Z. To convert this entry (it replaces the pinned %s commit %s), run: lazyrex-hub plugin update -config %s %s. To keep following the %s named latest, change the source to @%s",
		e.RefKind, e.Commit, cfgPath, source, e.RefKind, full)
}

// selectLatest resolves the highest release of the latest entry e. Each skipped tag goes to note.
func selectLatest(ctx context.Context, o pluginCmdOpts, tmpRoot string, e *configEntry, r githubRef, note func(string)) (latestRelease, error) {
	rel, err := withTmp(tmpRoot, func(tmp string) (latestRelease, error) {
		return resolveLatest(ctx, tmp, o.url(r))
	})
	if err != nil {
		return rel, err
	}
	for _, why := range rel.Ignored {
		note(fmt.Sprintf("ignored %s: %s", e.Source, why))
	}
	return rel, nil
}

// fetchRelease fetches the selected release by its tag ref, so a tag that moved after the
// selection is refused.
func fetchRelease(ctx context.Context, cfg *pluginCmdConfig, o pluginCmdOpts, tmpRoot string, e *configEntry, r githubRef, rel latestRelease) (*lockEntry, error) {
	if o.afterResolve != nil {
		o.afterResolve(e.Source)
	}
	return fetchAt(ctx, cfg, o, tmpRoot, r, rel.FetchRef, rel.Commit, "latest", rel.Tag, false)
}

// installLatest is the first install of a latest entry: it selects the highest release.
func installLatest(ctx context.Context, cfg *pluginCmdConfig, o pluginCmdOpts, tmpRoot string, e *configEntry, r githubRef, note func(string)) (*lockEntry, error) {
	rel, err := selectLatest(ctx, o, tmpRoot, e, r, note)
	if err != nil {
		return nil, err
	}
	return fetchRelease(ctx, cfg, o, tmpRoot, e, r, rel)
}

// updateLatest is plugin update of a latest entry (section 6): it selects the highest release
// again. The same tag at the same commit, with a good cache entry, is unchanged; a higher tag is
// updated. A tag that moved, and a highest tag lower than the locked one, are refused: a release
// tag must not move, and update never downgrades. A legacy entry is converted, with its own
// output line. An entry that cannot be trusted is replaced.
func updateLatest(ctx context.Context, cfg *pluginCmdConfig, o pluginCmdOpts, tmpRoot string, e *configEntry, r githubRef, le *lockEntry, note func(string)) (*lockEntry, string, error) {
	rel, err := selectLatest(ctx, o, tmpRoot, e, r, note)
	if err != nil {
		return nil, "", err
	}
	if le != nil && le.RefKind == "latest" && le.trust(r) == "" {
		locked, _ := parseVersionTag(le.Tag)
		sel, _ := parseVersionTag(rel.Tag)
		switch c := compareVersions(sel, locked); {
		case c < 0:
			return nil, "", fmt.Errorf("the newest version tag is %s, lower than the locked %s", rel.Tag, le.Tag)
		case c == 0 && rel.Commit != le.Commit:
			return nil, "", fmt.Errorf("the tag %s moved from %s to %s; a release tag must not move: check the repository", rel.Tag, le.Commit, rel.Commit)
		case c == 0:
			if _, _, err := lockedDir(cfg.path, cfg.state, &pluginLock{Plugins: map[string]*lockEntry{e.Source: le}}, e); err == nil {
				return le, "", nil
			}
		}
	}
	got, err := fetchRelease(ctx, cfg, o, tmpRoot, e, r, rel)
	switch {
	case err != nil:
		return nil, "", err
	case le == nil:
		return got, "installed", nil
	case le.legacyLatest(r):
		note(fmt.Sprintf("converted %s: %s latest %s -> %s %s", e.Source, le.RefKind, le.Commit, rel.Tag, rel.Commit))
		return got, "", nil
	}
	return got, "updated", nil
}
