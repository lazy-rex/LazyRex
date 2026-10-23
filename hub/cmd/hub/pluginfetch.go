package main

// Refs and fetch (docs/plugin-sources.md, section 5). This file has all git use of the hub: the
// plugin commands and the manager's ops (add, update, install) run it; a plugin start never runs
// git.
//
// The fetched tree must be the commit's tree exactly: the archive has the same file paths as
// ls-tree, and each file's content has the blob ID that ls-tree gives. So .gitattributes
// export-ignore and export-subst cannot change what is published, and the digest depends only on
// the commit. Symlinks and submodules are refused.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// gitErrLimit bounds the git error output that a text quotes.
const gitErrLimit = 4 << 10

// gitRunner runs one git command in dir. With out nil, it collects git's stdout and returns it.
// With out set, it streams git's stdout into out, which reads to EOF, and returns no bytes. The
// error is out's own error first, then git's standard error (at most gitErrLimit), else the exit
// error. Every git call of the commands and of the plugin manager goes through a runner: execGit
// for the commands, the fetch child for the manager.
type gitRunner func(ctx context.Context, dir string, out func(io.Reader) error, args ...string) ([]byte, error)

// execGit is the runner of the commands: git as a child of this process. Git uses the user's
// credential helper and url.insteadOf, and never asks for a password (GIT_TERMINAL_PROMPT=0).
func execGit(ctx context.Context, dir string, out func(io.Reader) error, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	stderr := &limitedBuffer{max: gitErrLimit}
	cmd.Stderr = stderr
	if out == nil {
		var b bytes.Buffer
		cmd.Stdout = &b
		if err := cmd.Run(); err != nil {
			return nil, gitError(err, stderr)
		}
		return b.Bytes(), nil
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	xerr := out(pipe)
	io.Copy(io.Discard, pipe)
	werr := cmd.Wait()
	if xerr != nil {
		return nil, xerr
	}
	if werr != nil {
		return nil, gitError(werr, stderr)
	}
	return nil, nil
}

// gitError is the error of a git command: its trimmed standard error, else the exit error.
func gitError(err error, stderr *limitedBuffer) error {
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return errors.New(msg)
	}
	return err
}

// limitedBuffer keeps the first max bytes written to it, and accepts (drops) the rest, so a child
// never blocks on a full stderr pipe. The buffer is a named field, not embedded: an embedded
// bytes.Buffer would give limitedBuffer its ReadFrom, and io.Copy (used by os/exec) would call
// that and skip the bound.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

// checkRef checks the form of a ref (section 5, "Ref forms"): 40 lowercase hex (a commit),
// refs/heads/<name>, refs/tags/<name>, or a name. A short hex is a name. git check-ref-format
// checks the full ref; a name is checked as refs/heads/<name>.
func checkRef(ctx context.Context, git gitRunner, dir, ref string) error {
	invalid := fmt.Errorf("%s is not a valid ref", ref)
	full := ref
	switch {
	case commitID.MatchString(ref):
		return nil
	case strings.HasPrefix(ref, "refs/heads/"), strings.HasPrefix(ref, "refs/tags/"):
	case strings.HasPrefix(ref, "refs/"):
		return invalid
	default:
		full = "refs/heads/" + ref
	}
	if _, err := git(ctx, dir, nil, "check-ref-format", full); err != nil {
		return invalid
	}
	return nil
}

// resolved is a ref that ls-remote resolved: the commit, the ref kind, and the exact ref to fetch.
type resolved struct {
	Commit, Kind, FetchRef string
}

// resolveRef resolves ref at url with ls-remote (section 5, "Resolve"). Only lines whose name
// equals one of the asked refs count; ls-remote also gives suffix matches such as
// refs/heads/team/<name>, which are dropped. A name that is both a branch and a tag is refused. A
// tag resolves to its peeled commit when it is annotated. A commit ID is not resolved.
func resolveRef(ctx context.Context, git gitRunner, dir, url, ref string) (resolved, error) {
	if commitID.MatchString(ref) {
		return resolved{Commit: ref, Kind: "commit", FetchRef: ref}, nil
	}
	head, tag := "refs/heads/"+ref, "refs/tags/"+ref
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		head, tag = ref, ""
	case strings.HasPrefix(ref, "refs/tags/"):
		head, tag = "", ref
	}
	args := []string{"ls-remote", "--", url}
	for _, r := range []string{head, tag} {
		if r != "" {
			args = append(args, r)
		}
	}
	if tag != "" {
		args = append(args, tag+"^{}")
	}
	out, err := git(ctx, dir, nil, args...)
	if err != nil {
		return resolved{}, err
	}
	found := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		id, name, ok := strings.Cut(line, "\t")
		if !ok || !commitID.MatchString(id) {
			continue
		}
		if name == head || name == tag || name == tag+"^{}" {
			found[name] = id
		}
	}
	hc, isHead := found[head]
	tc, isTag := found[tag]
	if p, ok := found[tag+"^{}"]; ok && isTag {
		tc = p
	}
	switch {
	case head != "" && tag != "" && isHead && isTag:
		return resolved{}, fmt.Errorf("ref %s is both a branch and a tag: write refs/heads/%s or refs/tags/%s", ref, ref, ref)
	case isHead:
		return resolved{Commit: hc, Kind: "branch", FetchRef: head}, nil
	case isTag:
		return resolved{Commit: tc, Kind: "tag", FetchRef: tag}, nil
	}
	return resolved{}, fmt.Errorf("ref %s is not found", ref)
}

// taggedRef is a tag that resolveTag resolved. Branch tells that a branch has the same name, so a
// source must spell the tag as refs/tags/<name> (resolveRef refuses the short name then).
type taggedRef struct {
	resolved
	Branch bool
}

// resolveTag resolves the tag name at url with one ls-remote: refs/tags/<name>, its peeled line,
// and refs/heads/<name>. Only the exact tag ref counts, so a name that only a branch has is not a
// tag. An annotated tag resolves to its peeled commit.
func resolveTag(ctx context.Context, git gitRunner, dir, url, name string) (taggedRef, error) {
	tag, head := "refs/tags/"+name, "refs/heads/"+name
	out, err := git(ctx, dir, nil, "ls-remote", "--", url, tag, tag+"^{}", head)
	if err != nil {
		return taggedRef{}, err
	}
	found := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		id, ref, ok := strings.Cut(line, "\t")
		if ok && commitID.MatchString(id) && (ref == tag || ref == tag+"^{}" || ref == head) {
			found[ref] = id
		}
	}
	commit, ok := found[tag]
	if !ok {
		return taggedRef{}, fmt.Errorf("not a tag: %s", name)
	}
	if p, ok := found[tag+"^{}"]; ok {
		commit = p
	}
	_, branch := found[head]
	return taggedRef{resolved{Commit: commit, Kind: "tag", FetchRef: tag}, branch}, nil
}

// errUnavailable is a commit that the remote does not give.
type errUnavailable struct {
	commit string
	err    error
}

func (e *errUnavailable) Error() string {
	return fmt.Sprintf("commit %s is not available: %v", e.commit, e.err)
}

// fetchedTree is a fetched commit, published in the cache.
type fetchedTree struct {
	Dir      string // the lock field dir, relative to <state>/plugins
	Digest   string
	Manifest *pluginManifest
}

// fetchCommit fetches commit at url into a temporary bare repository in tmp and publishes its tree
// for owner/repo (section 5, "Fetch", steps 1 to 7). The entry is the whole tree; subdir, when
// set, names the plugin directory in it. fetchRef is the exact ref to fetch (a branch or tag ref),
// or the commit ID itself. byID tells that fetchRef is a commit ID: a failed fetch is then an
// unavailable commit. Nothing is published when a check fails.
func fetchCommit(ctx context.Context, git gitRunner, stateDir, tmp, url, owner, repo, subdir, fetchRef, commit string, byID bool) (*fetchedTree, error) {
	gitDir := filepath.Join(tmp, "git")
	if _, err := git(ctx, tmp, nil, "init", "-q", "--bare", gitDir); err != nil {
		return nil, err
	}
	if _, err := git(ctx, gitDir, nil, "fetch", "-q", "--depth", "1", "--", url, fetchRef); err != nil {
		if byID {
			return nil, &errUnavailable{commit, err}
		}
		return nil, err
	}
	got, err := git(ctx, gitDir, nil, "rev-parse", "--verify", "--quiet", "FETCH_HEAD^{commit}")
	if err != nil {
		if t, terr := git(ctx, gitDir, nil, "cat-file", "-t", "FETCH_HEAD"); terr == nil {
			return nil, fmt.Errorf("%s is a %s, not a commit", commit, strings.TrimSpace(string(t)))
		}
		return nil, err
	}
	if strings.TrimSpace(string(got)) != commit {
		return nil, fmt.Errorf("ref %s moved during the fetch; run the command again", fetchRef)
	}
	if t, err := git(ctx, gitDir, nil, "cat-file", "-t", commit); err != nil || strings.TrimSpace(string(t)) != "commit" {
		return nil, fmt.Errorf("%s is not a commit", commit)
	}
	tree, err := lsTree(ctx, git, gitDir, commit)
	if err != nil {
		return nil, err
	}
	// With a subdir, the plugin directory must be a directory of the tree with a lazyrex.json (step
	// 6). ls-tree lists files only, so the manifest path proves both.
	if _, ok := tree[subdir+"/lazyrex.json"]; subdir != "" && !ok {
		return nil, fmt.Errorf("%s is not a plugin directory at commit %s", subdir, commit)
	}
	stage, err := newStage(stateDir, owner, repo, commit)
	if err != nil {
		return nil, err
	}
	files, err := archiveInto(ctx, git, gitDir, commit, tree, stage.Dir)
	if err != nil {
		// An extraction that still runs owns the stage: it stays (unused disk state; no lock names it).
		if running := (*outputRunningError)(nil); !errors.As(err, &running) {
			stage.discard()
		}
		return nil, err
	}
	m, _, err := readPluginManifest(filepath.Join(stage.Dir, filepath.FromSlash(subdir)))
	if err != nil {
		stage.discard()
		// The stage is removed, so the text names the commit in place of the stage path.
		return nil, errors.New(strings.ReplaceAll(err.Error(), stage.Dir, "the tree of "+commit))
	}
	if err := ctx.Err(); err != nil { // a cancel after the extraction publishes nothing
		stage.discard()
		return nil, err
	}
	rel, err := stage.publish()
	if err != nil {
		return nil, err
	}
	return &fetchedTree{Dir: rel, Digest: digestOf(files), Manifest: m}, nil
}

// treeFile is one file of a commit's tree, from ls-tree.
type treeFile struct {
	Exec bool
	OID  string // the blob ID
}

// lsTree lists the files of commit (section 5, step 4). Mode 100644 is a file and 100755 an
// executable file. A symlink (120000), a submodule (160000) and any other mode are refused.
func lsTree(ctx context.Context, git gitRunner, gitDir, commit string) (map[string]treeFile, error) {
	out, err := git(ctx, gitDir, nil, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	files := map[string]treeFile{}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, p, ok := strings.Cut(string(rec), "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("ls-tree: cannot read %q", rec)
		}
		switch f[0] {
		case "100644", "100755":
		case "120000":
			return nil, fmt.Errorf("plugin trees with symlinks are not supported: %s", p)
		case "160000":
			return nil, fmt.Errorf("submodules are not supported: %s", p)
		default:
			return nil, fmt.Errorf("mode %s is not supported: %s", f[0], p)
		}
		if err := checkTreePath(p); err != nil {
			return nil, err
		}
		files[p] = treeFile{Exec: f[0] == "100755", OID: f[2]}
	}
	return files, nil
}

// checkTreePath refuses a path that is absolute, not clean, or has an empty, "." or ".."
// component.
func checkTreePath(p string) error {
	bad := p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) || path.Clean(p) != p
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			bad = true
		}
	}
	if bad {
		return fmt.Errorf("the tree has a path that is not allowed: %q", p)
	}
	return nil
}

// outputRunningError is a runner's return while its stream callback still runs (the fetch child's
// bound ended first). The callback owns what it writes until it returns: the caller reads none of
// its results and leaves its files alone.
type outputRunningError struct{}

func (*outputRunningError) Error() string {
	return "the archive reader did not end within the stop bound"
}

// archiveRead is a test hook: it wraps the archive stream before extractTar reads it. nil in
// production.
var archiveRead func(io.Reader) io.Reader

// archiveInto runs git archive and extracts the archive into dir (section 5, step 5). The runner
// streams the archive into extractTar. files is read only after the runner returns without an
// error: then the extraction has ended (a runner that returns while it still runs returns an
// error).
func archiveInto(ctx context.Context, git gitRunner, gitDir, commit string, tree map[string]treeFile, dir string) ([]digestFile, error) {
	var files []digestFile
	_, err := git(ctx, gitDir, func(r io.Reader) error {
		if archiveRead != nil {
			r = archiveRead(r)
		}
		var xerr error
		files, xerr = extractTar(bufio.NewReader(r), commit, tree, dir)
		return xerr
	}, "archive", "--format=tar", commit)
	if err != nil {
		return nil, err
	}
	return files, nil
}

// extractTar extracts a tar stream of commit into dir. Its entries must be the tree exactly: each
// file of tree once, with its blob ID, and no other file. Directory entries are accepted only as
// parents of tree files, and one pax global header (git archive writes the commit ID in it) only
// first. The mode comes from the tree, not from the archive. It gives the digest lines.
func extractTar(r io.Reader, commit string, tree map[string]treeFile, dir string) ([]digestFile, error) {
	parents := map[string]bool{}
	for p := range tree {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			parents[d] = true
		}
	}
	mismatch := func(p, why string) error {
		return fmt.Errorf("the archive of %s does not match its tree: %s: %s", commit, p, why)
	}
	seen := map[string]bool{}
	var files []digestFile
	tr := tar.NewReader(r)
	for i := 0; ; i++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the archive of %s: %w", commit, err)
		}
		switch h.Typeflag {
		case tar.TypeXGlobalHeader:
			if i != 0 {
				return nil, mismatch(h.Name, "a second global header")
			}
			continue
		case tar.TypeDir:
			p := strings.TrimSuffix(h.Name, "/")
			if checkTreePath(p) != nil || !parents[p] || seen[p+"/"] {
				return nil, mismatch(h.Name, "an unexpected directory")
			}
			seen[p+"/"] = true
			continue
		case tar.TypeReg:
		default:
			return nil, mismatch(h.Name, fmt.Sprintf("entry type %q is not allowed", h.Typeflag))
		}
		p := h.Name
		if checkTreePath(p) != nil {
			return nil, mismatch(p, "the path is not allowed")
		}
		tf, ok := tree[p]
		if !ok {
			return nil, mismatch(p, "the file is not in the tree")
		}
		if seen[p] {
			return nil, mismatch(p, "the file appears twice")
		}
		seen[p] = true
		df, err := writeTreeFile(filepath.Join(dir, filepath.FromSlash(p)), tr, h.Size, tf.Exec)
		if err != nil {
			return nil, err
		}
		if df.oid != tf.OID {
			return nil, mismatch(p, "the content differs from the blob")
		}
		df.Path = p
		files = append(files, df.digestFile)
	}
	for p := range tree {
		if !seen[p] {
			return nil, mismatch(p, "the file is not in the archive")
		}
	}
	return files, nil
}

// writtenFile is a written file: its digest line and its git blob ID.
type writtenFile struct {
	digestFile
	oid string
}

// writeTreeFile writes one file of a stage: 0700 for an executable file, else 0600 (publish sets
// the published modes from these). It hashes the content for the digest and for the blob ID.
func writeTreeFile(p string, r io.Reader, size int64, exec bool) (writtenFile, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return writtenFile{}, err
	}
	mode := os.FileMode(0o600)
	if exec {
		mode = 0o700
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return writtenFile{}, err
	}
	defer f.Close()
	sum, blob := sha256.New(), sha1.New()
	fmt.Fprintf(blob, "blob %d\x00", size)
	n, err := io.Copy(io.MultiWriter(f, sum, blob), r)
	if err != nil {
		return writtenFile{}, err
	}
	if err := f.Close(); err != nil {
		return writtenFile{}, err
	}
	w := writtenFile{digestFile: digestFile{Exec: exec, Size: n}, oid: hex.EncodeToString(blob.Sum(nil))}
	copy(w.Sum[:], sum.Sum(nil))
	return w, nil
}
