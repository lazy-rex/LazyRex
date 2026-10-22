package main

// The admission of the plugin manager's add, update and restart ops (step 4 plan r2, with the hub
// lead's rulings lead-b8-step4, lead-b8-step4-blk and lead-b8-step4-final). The phases that run
// them are in pluginops.go.

import (
	"encoding/json"
	"path"
	"strings"
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
	h.entriesMu.Lock()
	others := append([]*configEntry{}, h.entries...)
	h.entriesMu.Unlock()
	for _, x := range append(list, others...) {
		if sameRepo(x, e) {
			return nil, refuse("conflict", "%s is the same repository as %s (%s); update that entry instead", in.Source, opEntryKey(x), x.Source)
		}
	}
	m := fileEntry(e)
	b, _ := json.Marshal(opAddInput{Source: m.Source, ID: m.ID, Args: m.Args, WatchRoots: m.WatchRoots})
	op.Entry, op.Input, op.Base, op.EntryIdentity = "managed:"+in.ID, string(b), "absent", entryIdentity(e)
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
