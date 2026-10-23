package main

// The admission of the plugin manager's add, update and restart ops (step 4 plan r2, with the hub
// lead's rulings lead-b8-step4, lead-b8-step4-blk and lead-b8-step4-final). The phases that run
// them are in pluginops.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// opAddInput is the input of an add. The admission stores it with the final id, so a replay
// never derives the id again.
type opAddInput struct {
	Source     string            `json:"source"`
	ID         string            `json:"id"`
	Args       map[string]string `json:"args"`
	WatchRoots []string          `json:"watch_roots"`
}

// opUpdateInput is the input of an update: "latest", or a tag name.
type opUpdateInput struct {
	To string `json:"to"`
}

// opRestartTarget is the stored target of a restart: the run that runs at admission, and its
// instance (the replace's expect_run and expect_instance).
type opRestartTarget struct {
	Source   string `json:"source"`
	Commit   string `json:"commit"`
	Digest   string `json:"digest"`
	Instance string `json:"instance"`
}

func (t *opRestartTarget) run() *expectRun { return &expectRun{t.Source, t.Commit, t.Digest} }

// updateTarget reports whether to is "latest" or a tag name. A branch and a commit are not
// targets of an update: a full ref (refs/...) or 40 hex is refused here, and the resolve reads
// refs/tags/<to> only, so a branch name with no tag is refused there.
func updateTarget(to string) bool {
	if to == "latest" {
		return true
	}
	if to == "" || len(to) > 200 || strings.HasPrefix(to, "refs/") || commitID.MatchString(to) || strings.ContainsAny(to, " \t\n@^~:?*[\\") {
		return false
	}
	return true
}

// defaultAddID is the id of an add without one: the last path element of the source (the subdir,
// else the repository), lowercased.
func defaultAddID(e *configEntry) string {
	last := e.repo
	if e.subdir != "" {
		last = path.Base(e.subdir)
	}
	return strings.ToLower(last)
}

// sameRepo reports whether two GitHub entries have the same lock key: the same repository (owner
// and repository compare without case, as GitHub does) and the same subdir. The ref does not
// count: the same repository at another ref is the same plugin.
func sameRepo(a, b *configEntry) bool {
	return a.github && b.github && strings.EqualFold(a.owner, b.owner) && strings.EqualFold(a.repo, b.repo) && a.subdir == b.subdir
}

// addAdmission checks an add and builds its op: a GitHub source only, an id (the default when
// none is given), and no entry with that id or that repository and subdir. The manifest name is
// known only after the fetch; a name that collides is conflict at locking (the graph check).
func (h *Hub) addAdmission(op *pluginOp, req opRequest) (*pluginOp, error) {
	if req.Entry != "" {
		return nil, bad("invalid: add has no entry; give input {source, id?, args?, watch_roots?}")
	}
	var in opAddInput
	if err := decodeManifest("input", req.Input, &in); err != nil {
		return nil, bad("invalid: %v", err)
	}
	if !strings.HasPrefix(in.Source, "github:") {
		return nil, bad("invalid: add needs a GitHub source github:<owner>/<repo>[/<subdir>]@<ref>; add a local directory by hand or with the plugin command")
	}
	// An add of an offer (the Plugins panel's confirm): the target is the complete offer that the
	// user saw. It must be a current offer of the plugins_from peer, else nothing starts. Its pin
	// is the stored target: a later change of the offer does not change what this op starts.
	var offer *peerOffer
	if len(req.Target) > 0 {
		var o peerOffer
		if err := decodeStrict(req.Target, &o); err != nil {
			return nil, bad("invalid: target: %v", err)
		}
		if o.ID == "" || in.Source != o.Source || in.ID != o.ID {
			return nil, bad("invalid: the add of an offer gives the offer's source and id as input")
		}
		if !slices.Contains(h.peerOffered(), o) {
			return nil, refuse("candidate_changed", "the offered code changed since it was shown: confirm the current offer")
		}
		offer = &o
	}
	e := &configEntry{Source: in.Source, Args: in.Args, WatchRoots: in.WatchRoots}
	if err := e.checkSource(""); err != nil {
		return nil, bad("invalid: %v", err)
	}
	if err := e.checkFields(); err != nil {
		return nil, bad("invalid: %v", err)
	}
	if in.ID == "" {
		in.ID = defaultAddID(e)
		if !managedID.MatchString(in.ID) {
			return nil, bad("invalid: the default id %q does not match [a-z0-9_-]{1,64}; give an id", in.ID)
		}
	} else if !managedID.MatchString(in.ID) {
		return nil, bad("invalid: id %q must match [a-z0-9_-]{1,64}", in.ID)
	}
	e.managed = in.ID
	_, list, err := readManaged(h.cfg.managedPath)
	if err != nil {
		return nil, refuse("refused", "managed plugin file %s: %v", h.cfg.managedPath, err)
	}
	if managedByID(list, in.ID) != nil {
		return nil, refuse("conflict", "managed:%s exists; give another id", in.ID)
	}
	// list is this call's own parse; h.entries are shared, and a reload updates them in place
	// under entriesMu, so the check and its text read them under that lock.
	same := func(x *configEntry) error {
		if sameRepo(x, e) {
			return refuse("conflict", "%s is the same repository as %s (%s); update that entry instead", in.Source, opEntryKey(x), x.Source)
		}
		return nil
	}
	for _, x := range list {
		if err := same(x); err != nil {
			return nil, err
		}
	}
	h.entriesMu.Lock()
	for _, x := range h.entries {
		if err := same(x); err != nil {
			h.entriesMu.Unlock()
			return nil, err
		}
	}
	h.entriesMu.Unlock()
	m := fileEntry(e)
	b, _ := json.Marshal(opAddInput{Source: m.Source, ID: m.ID, Args: m.Args, WatchRoots: m.WatchRoots})
	op.Entry, op.Input, op.Base, op.EntryIdentity = "managed:"+in.ID, string(b), "absent", entryIdentity(e)
	if offer != nil {
		pin, _ := json.Marshal(offer.Pin)
		if op.Target, err = peerTarget(pin, m.Source); err != nil {
			return nil, err
		}
	}
	if op.LockBase, err = h.lockSnapshot([]string{m.Source}); err != nil {
		return nil, err
	}
	return op, nil
}

// restartTarget is the run and the instance of a running plugin. A plugin that does not run is
// refused: a restart replaces a running instance; use start.
func (h *Hub) restartTarget(name string) (*opRestartTarget, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pl := h.plugins[name]
	if pl == nil || pl.ClientID == "" || !alive(pl.ChildPID, pl.ChildStart) || pl.InstanceID == "" {
		return nil, refuse("refused", "plugin %s is not running; use start", name)
	}
	return &opRestartTarget{Source: pl.run.Source, Commit: pl.run.Commit, Digest: pl.run.Digest, Instance: pl.InstanceID}, nil
}

// opResolveLimit bounds the resolving phase: every git command of it (step 4 plan, item 3a).
const opResolveLimit = 30 * time.Second

// resolveLimit is opResolveLimit; a test shortens it.
var resolveLimit = opResolveLimit

// opResolved is the stored target of an add or an update: the entry's source after the op and the
// commit that the resolve gave. The first stored target is the decision: a replay never resolves
// again (docs/hub.md, "a replay uses only stored values").
type opResolved struct {
	Source   string `json:"source"`
	RefKind  string `json:"ref_kind"`
	Tag      string `json:"tag"` // the release tag of a latest source, else ""
	Commit   string `json:"commit"`
	FetchRef string `json:"fetch_ref"`
	// A peer target (peersync.go): the peer's digest, which the fetch must give, and a fetch by the
	// commit ID. Absent on a resolved target.
	Digest string `json:"digest,omitempty"`
	ByID   bool   `json:"by_id,omitempty"`
}

// opNewSource is the source of an update: the entry's source up to its last "@", then the new ref.
// A tag that a branch also has is spelled refs/tags/<name>, so a later CLI sync reads it as the tag.
func opNewSource(source, to string, branch bool) string {
	ref := to
	if branch {
		ref = "refs/tags/" + to
	}
	return source[:strings.LastIndex(source, "@")] + "@" + ref
}

// opResolveTarget resolves the target of an add or an update at the remote. It writes nothing.
func (h *Hub) opResolveTarget(ctx context.Context, op *pluginOp) (opResolved, error) {
	source, to := "", ""
	switch op.Kind {
	case "add":
		var in opAddInput
		if err := json.Unmarshal([]byte(op.Input), &in); err != nil {
			return opResolved{}, err
		}
		source = in.Source
	case "update":
		var base managedFileEntry
		var in opUpdateInput
		if err := json.Unmarshal([]byte(op.Base), &base); err != nil {
			return opResolved{}, err
		}
		if err := json.Unmarshal([]byte(op.Input), &in); err != nil {
			return opResolved{}, err
		}
		source, to = base.Source, in.To
		if to == "latest" {
			source = opNewSource(source, "latest", false)
		}
	default:
		return opResolved{}, fmt.Errorf("%s has no resolve", op.Kind)
	}
	e := &configEntry{Source: source}
	if err := e.checkSource(""); err != nil {
		return opResolved{}, err
	}
	r := e.githubRef()
	tmpRoot := filepath.Join(h.cfg.State, "plugins", ".tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return opResolved{}, err
	}
	o := h.opCmd(op, "resolving")
	url, git := o.url(r), o.git
	return withTmp(tmpRoot, func(tmp string) (opResolved, error) {
		switch {
		case to != "" && to != "latest":
			t, err := resolveTag(ctx, git, tmp, url, to)
			if err != nil {
				return opResolved{}, err
			}
			return opResolved{Source: opNewSource(source, to, t.Branch), RefKind: "tag", Commit: t.Commit, FetchRef: t.FetchRef}, nil
		case r.Ref == "latest":
			rel, err := resolveLatest(ctx, git, tmp, url)
			if err != nil {
				return opResolved{}, err
			}
			return opResolved{Source: source, RefKind: "latest", Tag: rel.Tag, Commit: rel.Commit, FetchRef: rel.FetchRef}, nil
		}
		if err := checkRef(ctx, git, tmp, r.Ref); err != nil {
			return opResolved{}, err
		}
		res, err := resolveRef(ctx, git, tmp, url, r.Ref)
		if err != nil {
			return opResolved{}, err
		}
		return opResolved{Source: source, RefKind: res.Kind, Commit: res.Commit, FetchRef: res.FetchRef}, nil
	})
}

// opResolve is the resolving phase: it resolves the target and stores it. A stored target is
// never resolved again. A stop of the hub during the resolve stores nothing, so the replay
// resolves again; a remote that changed then gives the new answer. A failed resolve wrote
// nothing, so the op ends failed.
func (h *Hub) opResolve(ctx context.Context, op *pluginOp) error {
	if op.Target == "" {
		rctx, cancel := context.WithTimeout(ctx, resolveLimit)
		resolve := h.pops.resolve
		if resolve == nil {
			resolve = h.opResolveTarget
		}
		t, err := resolve(rctx, op)
		timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil && timedOut:
			return h.setOpPhase(ctx, op, "failed", "", fmt.Sprintf("resolve %s: no answer within %v; nothing was written", op.Entry, resolveLimit))
		case err != nil:
			return h.setOpPhase(ctx, op, "failed", "", fmt.Sprintf("resolve %s: %v; nothing was written", op.Entry, err))
		}
		if h.opFault("plugin-op-after-resolve") {
			return errOpCrash
		}
		b, _ := json.Marshal(t)
		if _, err := h.db.ExecContext(ctx, `UPDATE plugin_ops SET target = ?, updated = ? WHERE op_id = ? AND target = ''`, string(b), time.Now().UnixMilli(), op.ID); err != nil {
			return err
		}
		op.Target = string(b)
	}
	var t opResolved
	json.Unmarshal([]byte(op.Target), &t)
	return h.setOpPhase(ctx, op, op.nextPhase("resolving"), fmt.Sprintf("resolved %s to %s %s", op.Entry, t.Source, t.Commit), "")
}
