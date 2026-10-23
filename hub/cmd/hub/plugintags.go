package main

// hub.plugins.tags (C7 plan r2 item 1): the release tags of a GitHub repository, for the manager's
// update form. A load lists the tags once with ls-remote and selects latest from that listing with
// the checks of resolveLatest (verifyLatest), so latest is the release that @latest would select.
// The other IDs are what ls-remote gives: they are not known to be commits.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// lsRemoteMax bounds the ls-remote output that a tags load reads (the hub lead's limit).
const lsRemoteMax = 1 << 20

// tagsList is the result of a tags load.
type tagsList struct {
	Tags    []map[string]string // {tag, object}, highest first
	Latest  *tagsLatest         // nil: no release tag points to a commit
	Ignored []string            // verifyLatest's notes: higher tags that point to a tree or a blob
}

type tagsLatest struct{ Tag, Commit string }

// loadTags lists the release tags of url and selects latest. dir is a new temporary directory.
func loadTags(ctx context.Context, git gitRunner, dir, url string) (tagsList, error) {
	var buf bytes.Buffer
	read := func(r io.Reader) error {
		n, err := io.Copy(&buf, io.LimitReader(r, lsRemoteMax+1))
		if err == nil && n > lsRemoteMax {
			err = fmt.Errorf("the repository has too many tags (over %d bytes of ls-remote output)", lsRemoteMax)
		}
		return err
	}
	if _, err := git(ctx, dir, read, "ls-remote", "--tags", "--", url, "refs/tags/v*"); err != nil {
		return tagsList{}, err
	}
	cands := listVersionTags(buf.Bytes())
	out := tagsList{Tags: []map[string]string{}}
	for _, c := range cands {
		out.Tags = append(out.Tags, map[string]string{"tag": c.tag, "object": c.object})
	}
	rel, err := verifyLatest(ctx, git, dir, url, cands)
	switch {
	case errors.Is(err, errNoRelease):
		out.Ignored = rel.Ignored
	case err != nil:
		return tagsList{}, err
	default:
		out.Latest, out.Ignored = &tagsLatest{Tag: rel.Tag, Commit: rel.Commit}, rel.Ignored
	}
	return out, nil
}

const (
	tagsFresh   = 5 * time.Minute  // a ready tags list stays this long
	tagsLoadMax = 30 * time.Second // the limit of one tags load
)

// tagsKey is the cache key of a GitHub source: its repository, without case, as the lock key
// compares it. The subdir and the ref do not change the tags.
func tagsKey(source string) (string, error) {
	e := &configEntry{Source: source}
	if !strings.HasPrefix(source, "github:") {
		return "", refuse("invalid_input", "source must be a GitHub source github:<owner>/<repo>[/<subdir>]@<ref>")
	}
	if err := e.checkSource(""); err != nil {
		return "", refuse("invalid_input", "%v", err)
	}
	return strings.ToLower(e.owner) + "/" + strings.ToLower(e.repo), nil
}

// runTagsLoad is the load of one repository key ("owner/repo"), in a fetch child, with the hub's
// limit, in a temporary directory of the plugin cache.
func (h *Hub) runTagsLoad(ctx context.Context, key string, _ any) (any, bool, error) {
	owner, repo, _ := strings.Cut(key, "/")
	o := h.pops.cmd
	if o.git == nil {
		o.git = h.fetchChildGit("", "tags")
	}
	tmpRoot := filepath.Join(h.cfg.State, "plugins", ".tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return nil, true, err
	}
	ctx, cancel := context.WithTimeout(ctx, tagsLoadMax)
	defer cancel()
	list, err := withTmp(tmpRoot, func(tmp string) (tagsList, error) {
		return loadTags(ctx, o.runner(), tmp, o.url(githubRef{Owner: owner, Repo: repo}))
	})
	if err != nil {
		return nil, true, err
	}
	return list, true, nil
}

// pluginTags answers hub.plugins.tags {source, refresh?}: the cache entry of the source's
// repository, at once.
func (h *Hub) pluginTags(args json.RawMessage) (any, error) {
	var in struct {
		Source  string `json:"source"`
		Refresh bool   `json:"refresh"`
	}
	if err := decodeManifest("the arguments", args, &in); err != nil {
		return nil, refuse("invalid_input", "%v", err)
	}
	key, err := tagsKey(in.Source)
	if err != nil {
		return nil, err
	}
	r, err := h.loadRequest(h.loads.tags, key, in.Refresh, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"source": in.Source, "state": r.State, "tags": []map[string]string{}, "latest": nil,
		"ignored": []string{}, "error": r.Error, "read_at": loadTime(r.ReadAt)}
	if list, ok := r.Value.(tagsList); ok {
		out["tags"] = list.Tags
		if list.Latest != nil {
			out["latest"] = map[string]string{"tag": list.Latest.Tag, "commit": list.Latest.Commit}
		}
		if list.Ignored != nil {
			out["ignored"] = list.Ignored
		}
	}
	return out, nil
}

// loadTime is a load's time in Unix milliseconds, as the op times are; 0: no load was kept.
func loadTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
