package main

// expect_run (docs/plugin-sources.md, section 14): hub.admin.replace and hub.admin.start can name
// the exact run that the caller expects to start: the source string (owner, repo, subdir and ref
// for a GitHub source), and for a GitHub source the commit and the digest of its cache entry. The
// check runs inside admission, on the candidate that the spawn uses, after the resolution and
// before any retirement or spawn. A different candidate is refused with code candidate_changed,
// and nothing changes. A caller that writes the lock (plugin use, the plugin manager) so cannot
// start a candidate that another writer put in the lock after its write.

import (
	"fmt"
	"strings"
)

type expectRun struct {
	Source, Commit, Digest string
}

func (x *expectRun) String() string {
	if x.Commit == "" {
		return x.Source
	}
	return fmt.Sprintf("%s at %s (%s)", x.Source, x.Commit, x.Digest)
}

// runText names a run in the expect_run terms.
func runText(r *pluginRun) string {
	return (&expectRun{r.Source, r.Commit, r.Digest}).String()
}

// matches reports whether the run r is the expected run.
func (x *expectRun) matches(r *pluginRun) bool {
	return r != nil && r.Source == x.Source && r.Commit == x.Commit && r.Digest == x.Digest
}

// parseExpectRun reads the optional "expect_run" field: nil when it is absent. A GitHub source
// needs its commit (40 lowercase hex) and digest (sha256:<64 hex>); a local source has neither.
func parseExpectRun(args map[string]any) (*expectRun, error) {
	v, ok := args["expect_run"]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, bad("invalid: expect_run must be an object")
	}
	for k, f := range m {
		if k != "source" && k != "commit" && k != "digest" {
			return nil, bad("invalid: expect_run: unknown field %s", k)
		}
		if _, ok := f.(string); !ok {
			return nil, bad("invalid: expect_run.%s must be a string", k)
		}
	}
	x := &expectRun{}
	x.Source, _ = m["source"].(string)
	x.Commit, _ = m["commit"].(string)
	x.Digest, _ = m["digest"].(string)
	if x.Source == "" {
		return nil, bad("invalid: expect_run needs a source")
	}
	if strings.HasPrefix(x.Source, "github:") {
		if !commitID.MatchString(x.Commit) || !lockDigest.MatchString(x.Digest) {
			return nil, bad("invalid: expect_run of a GitHub source needs commit (40 lowercase hex) and digest (sha256:<64 hex>)")
		}
	} else if x.Commit != "" || x.Digest != "" {
		return nil, bad("invalid: expect_run of a local source has no commit and no digest")
	}
	return x, nil
}

// candidateChanged is the refusal of a candidate that is not the expected run.
func candidateChanged(name string, cand *pluginRun, x *expectRun, unchanged string) error {
	return refuse("candidate_changed", "plugin %s: the candidate is %s, not %s; %s", name, runText(cand), x, unchanged)
}
