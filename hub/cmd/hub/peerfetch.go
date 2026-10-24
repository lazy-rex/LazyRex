package main

// Plugin source over the peer link (user decision 1; lead-plugsrc, lead-plugsrc3). A managed GitHub
// entry with "fetch": "peer" takes the files of its pin from the hub's plugins_from peer, not from
// GitHub. The source hub serves only a pin that its lock has and whose cache entry verifies:
// hub.peer.plugin_tree gives the file list in pages, and hub.peer.plugin_bytes gives the
// concatenation of the files in parts. Each page and part is sized by the source's reply limit.
// The receiver checks the whole list against the pin digest before it fetches a byte, checks each
// file as it writes it, and publishes the stage only when its digest is the pin digest.
//
// Limit (D): the receiver proves that the files are the files of the peer's pin, not that they are
// the files of the GitHub commit: the pin comes from the same peer.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	peerFetchFiles = 10000    // the most files of a tree that a peer fetch takes
	peerFetchBytes = 16 << 20 // the most bytes of a tree that a peer fetch takes
	peerPartMax    = 512 << 10
)

// peerTreeFile is one line of hub.peer.plugin_tree: a digestOf line.
type peerTreeFile struct {
	Path   string `json:"path"`
	Exec   bool   `json:"exec"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type peerTreePage struct {
	Files []peerTreeFile `json:"files"`
	From  int            `json:"from"`
	Next  *int           `json:"next"`
	Count int            `json:"count"`
	Size  int64          `json:"size"`
}

type peerBytesPart struct {
	Data   string `json:"data"`
	Offset int64  `json:"offset"`
	End    int64  `json:"end"`
}

// peerServeBudget is the most bytes of one plugin_tree or plugin_bytes result, encoded: 512 KiB,
// or less if the hub's Rex connection takes smaller reply frames.
func (h *Hub) peerServeBudget() int {
	return min(peerPartMax, h.replyLimit(h.rex)-peerReplyWrap)
}

// servedTree is the verified cache entry of the pin that a plugin_tree or plugin_bytes call names:
// the entry root and its files in digest order. The lock must have the source at the commit with
// the digest, and the entry's digest must be the digest. Each call verifies again: a call never
// reads a list that an earlier call made.
func (h *Hub) servedTree(args map[string]any) (string, []digestFile, error) {
	peer, _ := args["hub"].(string)
	if h.peerLink(peer) == nil {
		return "", nil, refuse("unknown_hub", "%q is not a configured peer", peer)
	}
	source, _ := args["source"].(string)
	commit, _ := args["commit"].(string)
	digest, _ := args["digest"].(string)
	if source == "" || !commitID.MatchString(commit) || !lockDigest.MatchString(digest) {
		return "", nil, bad("invalid: give source, commit (40 hex digits) and digest (sha256:<64 hex digits>)")
	}
	notCached := refuse("not_cached", "%s at %s is not in this hub's verified cache", source, commit)
	e := &configEntry{Source: source}
	if e.checkSource("") != nil || !e.github {
		return "", nil, notCached
	}
	lock, err := readLock(lockPath(h.cfg.path, h.cfg.Lock))
	if err != nil {
		return "", nil, notCached
	}
	le := lock.Plugins[source]
	if le == nil || le.Commit != commit || le.Digest != digest {
		return "", nil, notCached
	}
	// The digest check of the entry is lockedDir's, on the list that the call serves.
	root := filepath.Join(h.cfg.State, "plugins", filepath.FromSlash(le.Dir))
	files, err := cacheFiles(root)
	if err != nil || digestOf(files) != digest {
		return "", nil, notCached
	}
	return root, files, nil
}

// peerPluginTreeServe is hub.peer.plugin_tree {hub, source, commit, digest, from}: the files of
// the pin's verified cache entry from index from, as many whole lines as fit the budget (at least
// one), with next (null after the last page), count and size.
func (h *Hub) peerPluginTreeServe(args map[string]any) (any, error) {
	_, files, err := h.servedTree(args)
	if err != nil {
		return nil, err
	}
	from, ok := intArg(args["from"])
	if !ok || from < 0 || from > len(files) {
		return nil, bad("invalid: from must be a file index from 0 to %d", len(files))
	}
	var size int64
	for _, f := range files {
		size += f.Size
	}
	page := peerTreePage{Files: []peerTreeFile{}, From: from, Count: len(files), Size: size}
	head, _ := json.Marshal(page)
	used, budget := len(head)+len(`,"next":0000000000`), h.peerServeBudget()
	i := from
	for ; i < len(files); i++ {
		line := peerTreeFile{Path: files[i].Path, Exec: files[i].Exec, Size: files[i].Size, SHA256: hex.EncodeToString(files[i].Sum[:])}
		b, _ := json.Marshal(line)
		if i > from && used+len(b)+1 > budget {
			break
		}
		used += len(b) + 1
		page.Files = append(page.Files, line)
	}
	if i < len(files) {
		page.Next = &i
	}
	log.Printf("peer %s: plugin_tree %s %s from %d: %d files", args["hub"], args["source"], args["commit"], from, len(page.Files))
	return page, nil
}

// peerPluginBytesServe is hub.peer.plugin_bytes {hub, source, commit, digest, offset}: the bytes
// [offset, end) of the concatenation of the files in digest order, base64, at most the budget
// encoded. end = the tree size marks the last part.
func (h *Hub) peerPluginBytesServe(args map[string]any) (any, error) {
	root, files, err := h.servedTree(args)
	if err != nil {
		return nil, err
	}
	var size int64
	for _, f := range files {
		size += f.Size
	}
	off, ok := intArg(args["offset"])
	offset := int64(off)
	if !ok || offset < 0 || offset > size {
		return nil, bad("invalid: offset must be a byte offset from 0 to %d", size)
	}
	raw := (h.peerServeBudget() - len(`{"data":"","offset":0000000000,"end":0000000000}`)) / 4 * 3
	end := min(size, offset+int64(raw))
	buf := make([]byte, 0, end-offset)
	var at int64 // the offset of file f in the concatenation
	for _, f := range files {
		if at >= end {
			break
		}
		lo, hi := max(offset, at), min(end, at+f.Size)
		if lo < hi {
			b, err := readRange(filepath.Join(root, filepath.FromSlash(f.Path)), lo-at, hi-lo)
			if err != nil {
				return nil, refuse("not_cached", "%s at %s is not in this hub's verified cache: %v", args["source"], args["commit"], err)
			}
			buf = append(buf, b...)
		}
		at += f.Size
	}
	log.Printf("peer %s: plugin_bytes %s %s offset %d: %d bytes", args["hub"], args["source"], args["commit"], offset, len(buf))
	return peerBytesPart{Data: base64.StdEncoding.EncodeToString(buf), Offset: offset, End: end}, nil
}

// readRange reads n bytes at off of the file path.
func readRange(path string, off, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

// peerFetchCall is a peerCall in the fetching phase: an ambiguous label (the peer's hub restarted)
// is tried again every 2 s until the phase's bound.
func (h *Hub) peerFetchCall(ctx context.Context, l *peerLink, action string, args map[string]any) ([]byte, error) {
	for {
		out, err := h.peerCall(ctx, l.cfg, action, args)
		if err == nil || !peerAmbiguous(err) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(peerPollUp):
		}
	}
}

// peerPinTarget is the target of an install of a peer entry with no lock entry: the plugins_from
// peer's pin of the exact source (hub.peer.plugins), fetched by its commit ID with its digest.
func (h *Hub) peerPinTarget(ctx context.Context, source string) (opResolved, error) {
	l := h.pluginsFrom()
	if l == nil {
		return opResolved{}, errors.New("fetch peer needs a plugins_from peer")
	}
	out, err := h.peerFetchCall(ctx, l, "hub.peer.plugins", map[string]any{"hub": h.cfg.HubName})
	if err != nil {
		return opResolved{}, err
	}
	var r struct {
		Pins json.RawMessage `json:"pins"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return opResolved{}, fmt.Errorf("hub.peer.plugins on %s: the reply is not JSON: %v", l.cfg.Name, err)
	}
	doc, err := decodePinDoc(r.Pins)
	if err != nil {
		return opResolved{}, fmt.Errorf("hub.peer.plugins on %s: the pin document: %v", l.cfg.Name, err)
	}
	for _, p := range doc.Entries {
		if p.Source != source {
			continue
		}
		raw, _ := json.Marshal(p)
		s, err := peerTarget(raw, source)
		if err != nil {
			return opResolved{}, err
		}
		var t opResolved
		err = json.Unmarshal([]byte(s), &t)
		return t, err
	}
	return opResolved{}, fmt.Errorf("the peer %s has no pin for %s", l.cfg.Name, source)
}

// peerFetchStats are the counts of one peer fetch, for the op's result and the lab.
type peerFetchStats struct {
	peer        string
	tree, parts int
	size        int64
	elapsed     time.Duration
}

// peerFetch fetches the tree of the target t from the plugins_from peer into a new cache entry and
// gives its complete lock entry. Nothing is published when a check fails.
func (h *Hub) peerFetch(ctx context.Context, t opResolved, e *configEntry) (*lockEntry, *peerFetchStats, error) {
	l := h.pluginsFrom()
	if l == nil {
		return nil, nil, errors.New("fetch peer needs a plugins_from peer")
	}
	if t.Digest == "" {
		return nil, nil, errors.New("fetch peer needs the peer's pin digest; the plugin sync updates this entry")
	}
	st := &peerFetchStats{peer: l.cfg.Name}
	t0 := time.Now()
	call := map[string]any{"hub": h.cfg.HubName, "source": t.Source, "commit": t.Commit, "digest": t.Digest}
	files, size, err := h.peerFetchTree(ctx, l, call, t.Digest, st)
	if err != nil {
		return nil, nil, err
	}
	r := e.githubRef()
	stage, err := newStage(h.cfg.State, r.Owner, r.Repo, t.Commit)
	if err != nil {
		return nil, nil, err
	}
	if err := h.peerFetchBytes(ctx, l, call, stage.Dir, files, size, st); err != nil {
		stage.discard()
		return nil, nil, err
	}
	if d, err := dirDigest(stage.Dir); err != nil || d != t.Digest {
		stage.discard()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("the written tree is %s, not the peer's %s", d, t.Digest)
	}
	m, _, err := readPluginManifest(filepath.Join(stage.Dir, filepath.FromSlash(r.Subdir)))
	if err != nil {
		stage.discard()
		return nil, nil, errors.New(strings.ReplaceAll(err.Error(), stage.Dir, "the tree of "+t.Commit))
	}
	if err := ctx.Err(); err != nil {
		stage.discard()
		return nil, nil, err
	}
	rel, err := stage.publish()
	if err != nil {
		return nil, nil, err
	}
	st.size, st.elapsed = size, time.Since(t0)
	return &lockEntry{lockPin: lockPin{Name: m.Name, Owner: r.Owner, Repo: r.Repo, Subdir: r.Subdir, Ref: r.Ref, RefKind: t.RefKind,
		Tag: t.Tag, Commit: t.Commit, Digest: t.Digest}, Dir: rel}, st, nil
}

// peerFetchTree reads every page of the tree list and checks the whole list: the pages agree, the
// bounds, the paths, the digest order, and digestOf(list) = digest.
func (h *Hub) peerFetchTree(ctx context.Context, l *peerLink, call map[string]any, digest string, st *peerFetchStats) ([]digestFile, int64, error) {
	var files []digestFile
	count, size := -1, int64(0)
	for from := 0; ; {
		args := map[string]any{"from": from}
		for k, v := range call {
			args[k] = v
		}
		out, err := h.peerFetchCall(ctx, l, "hub.peer.plugin_tree", args)
		if err != nil {
			return nil, 0, err
		}
		st.tree++
		var p peerTreePage
		if err := decodeStrict(out, &p); err != nil {
			return nil, 0, fmt.Errorf("plugin_tree from %d: %v", from, err)
		}
		switch {
		case p.From != from:
			return nil, 0, fmt.Errorf("plugin_tree: the page from %d answers from %d", from, p.From)
		case count >= 0 && (p.Count != count || p.Size != size):
			return nil, 0, fmt.Errorf("plugin_tree: the page from %d has count %d and size %d, not %d and %d", from, p.Count, p.Size, count, size)
		case p.Count < 0 || p.Count > peerFetchFiles:
			return nil, 0, fmt.Errorf("plugin_tree: %d files; a peer fetch takes at most %d", p.Count, peerFetchFiles)
		case p.Size < 0 || p.Size > peerFetchBytes:
			return nil, 0, fmt.Errorf("plugin_tree: %d bytes; a peer fetch takes at most %d", p.Size, peerFetchBytes)
		}
		count, size = p.Count, p.Size
		for _, f := range p.Files {
			sum, err := hex.DecodeString(f.SHA256)
			if err != nil || len(sum) != sha256.Size || f.Size < 0 {
				return nil, 0, fmt.Errorf("plugin_tree: the line of %q is not valid", f.Path)
			}
			df := digestFile{Path: f.Path, Exec: f.Exec, Size: f.Size}
			copy(df.Sum[:], sum)
			files = append(files, df)
		}
		if len(files) > count {
			return nil, 0, fmt.Errorf("plugin_tree: more than %d lines", count)
		}
		if p.Next == nil {
			break
		}
		if len(p.Files) == 0 || *p.Next != from+len(p.Files) {
			return nil, 0, fmt.Errorf("plugin_tree: the page from %d has %d lines and next %d", from, len(p.Files), *p.Next)
		}
		from = *p.Next
	}
	if len(files) != count {
		return nil, 0, fmt.Errorf("plugin_tree: %d lines, not count %d", len(files), count)
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	if total != size {
		return nil, 0, fmt.Errorf("plugin_tree: the lines have %d bytes, not size %d", total, size)
	}
	if err := checkTreePaths(files); err != nil {
		return nil, 0, fmt.Errorf("plugin_tree: %v", err)
	}
	if d := digestOf(files); d != digest {
		return nil, 0, fmt.Errorf("plugin_tree: the list is %s, not the pin's %s", d, digest)
	}
	return files, size, nil
}

// checkTreePaths checks the paths of a tree list: in digest order with no duplicate, relative and
// slash separated with no empty, "." or ".." part, and no path under another file.
func checkTreePaths(files []digestFile) error {
	set := map[string]bool{}
	for i, f := range files {
		if i > 0 && f.Path <= files[i-1].Path {
			return fmt.Errorf("%q is not after %q (duplicate or out of order)", f.Path, files[i-1].Path)
		}
		if f.Path == "" || strings.ContainsRune(f.Path, 0) {
			return fmt.Errorf("path %q is not valid", f.Path)
		}
		for _, part := range strings.Split(f.Path, "/") {
			if part == "" || part == "." || part == ".." {
				return fmt.Errorf("path %q is not valid", f.Path)
			}
		}
		set[f.Path] = true
	}
	for _, f := range files {
		for d := f.Path; ; {
			i := strings.LastIndexByte(d, '/')
			if i < 0 {
				break
			}
			d = d[:i]
			if set[d] {
				return fmt.Errorf("path %q is under the file %q", f.Path, d)
			}
		}
	}
	return nil
}

// peerFetchBytes reads the parts from offset 0 to size and writes the files into dir. Each file is
// 0600, or 0700 for an executable line (the stage contract); its size and sha256 must be its line's.
func (h *Hub) peerFetchBytes(ctx context.Context, l *peerLink, call map[string]any, dir string, files []digestFile, size int64, st *peerFetchStats) error {
	w := &treeWriter{dir: dir, files: files}
	defer w.close()
	if err := w.next(); err != nil {
		return err
	}
	for offset := int64(0); offset < size; {
		args := map[string]any{"offset": offset}
		for k, v := range call {
			args[k] = v
		}
		out, err := h.peerFetchCall(ctx, l, "hub.peer.plugin_bytes", args)
		if err != nil {
			return err
		}
		st.parts++
		var p peerBytesPart
		if err := decodeStrict(out, &p); err != nil {
			return fmt.Errorf("plugin_bytes at %d: %v", offset, err)
		}
		data, err := base64.StdEncoding.DecodeString(p.Data)
		switch {
		case err != nil:
			return fmt.Errorf("plugin_bytes at %d: data: %v", offset, err)
		case p.Offset != offset || p.End <= offset || p.End > size || int64(len(data)) != p.End-offset:
			return fmt.Errorf("plugin_bytes at %d: the part is offset %d, end %d with %d bytes", offset, p.Offset, p.End, len(data))
		}
		if err := w.write(data); err != nil {
			return err
		}
		offset = p.End
	}
	if w.i != len(files) {
		return fmt.Errorf("plugin_bytes: the bytes ended at file %d of %d", w.i, len(files))
	}
	return nil
}

// treeWriter writes the concatenation of files into dir, one file after the other.
type treeWriter struct {
	dir     string
	files   []digestFile
	i       int // the file that is open, or len(files)
	f       *os.File
	written int64
	sum     hash.Hash
}

// close closes the open file, if any (after a failure; the caller discards the stage).
func (w *treeWriter) close() {
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
}

// next ends the files that are complete (also files of size 0) and opens the next one.
func (w *treeWriter) next() error {
	for w.i < len(w.files) {
		cur := w.files[w.i]
		if w.f == nil {
			p := filepath.Join(w.dir, filepath.FromSlash(cur.Path))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if cur.Exec {
				mode = 0o700
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			w.f, w.written, w.sum = f, 0, sha256.New()
		}
		if w.written < cur.Size {
			return nil
		}
		err := w.f.Close()
		w.f = nil
		if err != nil {
			return err
		}
		if [sha256.Size]byte(w.sum.Sum(nil)) != cur.Sum {
			return fmt.Errorf("plugin_bytes: %s does not have the sha256 of its line", cur.Path)
		}
		w.i++
	}
	return nil
}

// write writes data across the files in order.
func (w *treeWriter) write(data []byte) error {
	for len(data) > 0 {
		if w.i >= len(w.files) {
			return errors.New("plugin_bytes: more bytes than the files")
		}
		n := min(int64(len(data)), w.files[w.i].Size-w.written)
		if _, err := w.f.Write(data[:n]); err != nil {
			return err
		}
		w.sum.Write(data[:n])
		w.written += n
		data = data[n:]
		if err := w.next(); err != nil {
			return err
		}
	}
	return nil
}

func (s *peerFetchStats) String() string {
	return fmt.Sprintf("from peer %s: %d tree calls, %d byte calls, %d bytes in %.1fs", s.peer, s.tree, s.parts, s.size, s.elapsed.Seconds())
}
